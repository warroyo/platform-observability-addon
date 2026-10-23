#!/usr/bin/env bash
# Onboard a VKS cluster to VCF Operations Platform Observability without the
# "Onboard VKS Cluster" dialog, and keep its OAuth secret in step afterwards.
#
#   scripts/onboard.sh <cluster> <supervisor-namespace>          print the credentials Secret
#   scripts/onboard.sh <cluster> <supervisor-namespace> --check  is it still current?
#
# Print mode asks VCF Operations for the cluster's configuration (CA
# certificates, endpoints, OAuth client ID), issues the cluster's OAuth
# client a new secret, and prints the Secret platform-observability-<cluster>
# that the addon's AddonConfigDefinition reads. It is the Secret the onboarder
# writes, so a cluster onboarded with this script can be handed to the
# onboarder later without a new secret:
#
#   VCF_OPS_URL=https://ops.example.com VCF_OPS_PASSWORD=... \
#     scripts/onboard.sh lab1-vks-01 my-namespace | kubectl apply -f -
#   kubectl -n my-namespace label cluster lab1-vks-01 \
#     addons.kubernetes.vmware.com/platform-observability=enabled
#
# The output holds the OAuth client secret. Do not write it to a file you keep.
#
# The two calls are the onboarding dialog's, under suite-api where an Ops API
# token works. GET configurations returns the config and touches nothing.
# POST configurations is what the dialog sends when a cluster name is entered:
# it creates the cluster's client if needed, issues it a new secret (revoking
# the old one at once), and returns the dialog's values files and commands,
# with the secret in step 5.
#
# Why --check exists: there is no way to read a client's secret, only to
# issue a new one. Opening the dialog for a cluster, or running print mode
# again, does exactly that, and the cluster's collectors then get 401s until
# they get the new secret. So the Secret records when the secret it
# carries was issued (annotation platform-observability/secret-rotated-at, from
# VSP's lastSecretRotationDate), and --check compares that with VSP without
# touching the secret. Exit 0: in step. Exit 3: rotated elsewhere, or no
# Secret yet; run print mode and apply its output. A reconcile loop is
#
#   scripts/onboard.sh "$c" "$ns" --check || scripts/onboard.sh "$c" "$ns" | kubectl apply -f -
#
# Print mode also puts a hash of the secret on the Secret, which the
# AddonConfigDefinition puts on the aggregator pods, so a new secret changes
# the helm values and rolls the aggregators onto it.
#
# Environment:
#   VCF_OPS_URL            required, e.g. https://ops.example.com
#   VCF_OPS_USERNAME       default admin; use platform-observability-onboard,
#                          the restricted user scripts/ops-account.sh creates
#   VCF_OPS_PASSWORD       required in print mode
#   VSP_URL                default https://<vmspPlatformFqdn>; required in
#                          --check mode, e.g. https://vsp.example.com
#   VSP_USERNAME           default admin@vsp.local
#   VSP_PASSWORD           default VCF_OPS_PASSWORD, or the file
#                          ~/.config/vcf-ops/vsp.password
#   VCF_OPS_INSECURE       set to 1 to skip TLS verification
set -euo pipefail

usage() { echo "usage: $0 <cluster-name> <supervisor-namespace> [--check]" >&2; exit 2; }
[ $# -eq 2 ] || [ $# -eq 3 ] || usage
cluster=$1
namespace=$2
mode=print
if [ $# -eq 3 ]; then [ "$3" = --check ] && mode=check || usage; fi

addon=platform-observability
secret_name="$addon-$cluster"
rotated_annotation=platform-observability/secret-rotated-at
component_type=vcf-obs-data-platform
client_name="svc-vodap-app0-$cluster"
curl_opts=(-sS --fail-with-body)
[ "${VCF_OPS_INSECURE:-0}" = 1 ] && curl_opts+=(-k)

# When VSP last issued a secret for this cluster's client. Read-only: it never
# touches the secret. HTTP/1.1, because VSP resets HTTP/2 streams on errors.
vsp_rotated_at() {
  local vsp=${1%/} password token
  password=${VSP_PASSWORD:-${VCF_OPS_PASSWORD:-$(cat ~/.config/vcf-ops/vsp.password 2>/dev/null || true)}}
  [ -n "$password" ] || { echo "set VSP_PASSWORD" >&2; exit 2; }
  token=$(curl "${curl_opts[@]}" --http1.1 "$vsp/api/v1/identity/token" -d grant_type=password \
    --data-urlencode "username=${VSP_USERNAME:-admin@vsp.local}" \
    --data-urlencode "password=$password" | jq -r .access_token)
  curl "${curl_opts[@]}" --http1.1 -H "Authorization: Bearer $token" \
    "$vsp/api/v1/identity/service-accounts?componentType=$component_type" \
    | jq -r --arg n "$client_name" '.[] | select(.clientName == $n) | .lastSecretRotationDate // empty'
}

if [ "$mode" = check ]; then
  vsp_date=$(vsp_rotated_at "${VSP_URL:?VSP_URL is required with --check}")
  [ -n "$vsp_date" ] || { echo "$cluster: no client $client_name in VSP; onboard it first" >&2; exit 3; }
  stored=$(kubectl -n "$namespace" get secret "$secret_name" \
    -o jsonpath="{.metadata.annotations.${rotated_annotation//./\\.}}" 2>/dev/null || true)
  if [ -z "$stored" ]; then
    echo "$cluster: no Secret $namespace/$secret_name, or it has no $rotated_annotation"
    exit 3
  elif [ "$stored" != "$vsp_date" ]; then
    echo "$cluster: secret rotated at $vsp_date, the Secret carries the one from $stored"
    exit 3
  fi
  echo "$cluster: in step (secret issued $vsp_date)"
  exit 0
fi

ops_url=${VCF_OPS_URL:?VCF_OPS_URL is required}
ops_url=${ops_url%/}
: "${VCF_OPS_PASSWORD:?VCF_OPS_PASSWORD is required}"

# The dialog's API, under suite-api, where an API token works. The dialog's
# own path (/vcf-operations/rest/...) wants a browser session.
token=$(curl "${curl_opts[@]}" -X POST "$ops_url/suite-api/api/auth/token/acquire" \
  -H 'Content-Type: application/json' -H 'Accept: application/json' \
  -d "$(jq -n --arg u "${VCF_OPS_USERNAME:-admin}" --arg p "$VCF_OPS_PASSWORD" \
        '{username: $u, password: $p}')" | jq -r .token)
configurations() {
  curl "${curl_opts[@]}" "$@" -G "$ops_url/suite-api/internal/extension/vodap/app0/configurations" \
    --data-urlencode "clusterName=$cluster" \
    -H "Authorization: OpsToken $token" -H 'Accept: application/json' \
    -H 'X-vRealizeOps-API-use-unsupported: true'
}

# POST first: for a new cluster it also creates the client the GET then names.
# The secret is in the dialog's step 5, a kubectl command:
#   --from-literal=client-secret="<secret>"
client_secret=$(configurations -X POST -H 'Content-Length: 0' \
  | jq -r '.commands[] | select(test("otel-gateway-oauth-creds"))
           | capture("client-secret=\"(?<s>[^\"]+)\"").s')
[ -n "$client_secret" ] || { echo "no client secret in the POST configurations response for $cluster" >&2; exit 1; }

# The GET ignores clusterName and lists every onboarded cluster.
config=$(configurations | jq -c --arg c "$cluster" '[.[] | select(.clusterName == $c)]')

if ! jq -e --arg c "$cluster" 'type == "array" and length == 1 and .[0].clusterName == $c' \
     >/dev/null <<<"$config"; then
  echo "unexpected configurations response for $cluster" >&2
  exit 1
fi

# Read the issue date after the POST: if another rotation lands in between, the
# next --check says so.
rotated_at=$(vsp_rotated_at "${VSP_URL:-https://$(jq -r '.[0].vmspPlatformFqdn' <<<"$config")}")
[ -n "$rotated_at" ] || { echo "no client $client_name in VSP" >&2; exit 1; }
secret_hash=$(printf %s "$client_secret" | sha256sum | cut -c1-16)

# The keys and the secret-hash annotation are the ones the onboarder writes
# and the AddonConfigDefinition reads (onboarder/reconciler.go).
jq -n --argjson c "$(jq '.[0]' <<<"$config")" \
  --arg ns "$namespace" --arg name "$secret_name" \
  --arg secret "$client_secret" --arg hash "$secret_hash" \
  --arg rotated_annotation "$rotated_annotation" --arg rotated_at "$rotated_at" '
{
  apiVersion: "v1",
  kind: "Secret",
  metadata: {
    name: $name,
    namespace: $ns,
    annotations: {
      "platform-observability/secret-hash": $hash,
      ($rotated_annotation): $rotated_at
    }
  },
  type: "Opaque",
  stringData: {
    clientId: $c.saClientId,
    clientSecret: $secret,
    instanceFqdn: $c.vmspInstanceFqdn,
    platformFqdn: $c.vmspPlatformFqdn,
    instanceCA: $c.vmspInstanceCertificate,
    platformCA: $c.vmspPlatformCertificate
  }
}'
