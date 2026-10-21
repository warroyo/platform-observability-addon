#!/usr/bin/env bash
# Probe the VCF Operations ingest path the aggregator uses, without a cluster:
# get a token with one cluster's OAuth client, then send OTLP metrics and logs
# labelled as one or more cluster IDs. Tells us whether a client is tied to
# its own cluster_id.
#
#   scripts/probe-ingest.sh <client-cluster> <secret-file> <cluster-id>...
#
#   scripts/probe-ingest.sh lab1-vks-01 ~/.config/vcf-ops/lab1-vks-01.secret \
#     lab1-vks-01 probe-vks-02
#
# <secret-file> holds the client secret, as issued by onboard.sh (POST
# configurations) or step 5 of the onboarding dialog.
# Every cluster ID given gets real-looking data in VCF Operations, so expect a
# probe-vks-02 entry under Platform Services if the ingest accepts it.
#
# Streams for PROBE_MINUTES (default 10), one batch every PROBE_INTERVAL
# seconds (default 30), so the dashboard has a series to show, not one point.
#
# Environment: VCF_OPS_URL, VCF_OPS_USERNAME, VCF_OPS_PASSWORD as in onboard.sh.
set -euo pipefail

[ $# -ge 3 ] || { echo "usage: $0 <client-cluster> <secret-file> <cluster-id>..." >&2; exit 2; }
client_cluster=$1; secret_file=$2; shift 2
ops_url=${VCF_OPS_URL:?VCF_OPS_URL is required}; ops_url=${ops_url%/}
: "${VCF_OPS_PASSWORD:?VCF_OPS_PASSWORD is required}"
client_secret=$(tr -d '\r\n' <"$secret_file")
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT

# Endpoints, CAs and client ID, the same way onboard.sh gets them.
ops_token=$(curl -sSk -X POST "$ops_url/suite-api/api/auth/token/acquire" \
  -H 'Content-Type: application/json' -H 'Accept: application/json' \
  -d "$(jq -n --arg u "${VCF_OPS_USERNAME:-admin}" --arg p "$VCF_OPS_PASSWORD" '{username: $u, password: $p}')" \
  | jq -r .token)
curl -sSk -G "$ops_url/suite-api/internal/extension/vodap/app0/configurations" \
  --data-urlencode "clusterName=$client_cluster" \
  -H "Authorization: OpsToken $ops_token" -H 'Accept: application/json' \
  -H 'X-vRealizeOps-API-use-unsupported: true' | jq '.[0]' >"$work/config.json"
jq -r .vmspInstanceCertificate "$work/config.json" >"$work/gateway-ca.crt"
jq -r .vmspPlatformCertificate "$work/config.json" >"$work/oauth-ca.crt"
client_id=$(jq -r .saClientId "$work/config.json")
token_url="https://$(jq -r .vmspPlatformFqdn "$work/config.json")/api/v1/identity/token"
ingest="https://$(jq -r .vmspInstanceFqdn "$work/config.json")/otel-ingest-service"

# Token, as the collector's oauth2client gets it. Try credentials in the body
# first, then basic auth, since the token server may accept only one.
get_token() {
  curl -sS --cacert "$work/oauth-ca.crt" -o "$work/token.json" -w '%{http_code}' "$@" "$token_url"
}
fetch_token() {
  local code style=body
  code=$(get_token -d grant_type=client_credentials \
    --data-urlencode "client_id=$client_id" --data-urlencode "client_secret=$client_secret")
  if [ "$code" != 200 ]; then
    code=$(get_token -u "$client_id:$client_secret" -d grant_type=client_credentials)
    style=basic
  fi
  [ "$code" = 200 ] || { echo "token: HTTP $code ($style auth): $(head -c 300 "$work/token.json")" >&2; return 1; }
  access_token=$(jq -r .access_token "$work/token.json")
  token_style=$style
}
fetch_token
echo "token: HTTP 200 ($token_style auth), response keys: $(jq -c 'keys' "$work/token.json")"
# The claims say what the token is scoped to; print them without the signature.
jq -R 'split(".")[1] | gsub("-";"+") | gsub("_";"/") | . + ("=" * ((4 - length % 4) % 4)) | @base64d | fromjson' \
  <<<"$access_token" 2>/dev/null | jq '{sub, aud, scope, client_id, azp, exp}' || true

send() {
  local cluster=$1 now=$2 resource metrics logs body signal code
  resource=$(jq -n --arg c "$cluster" '{attributes: [
    {key: "cluster_id",         value: {stringValue: $c}},
    {key: "k8s.cluster.name",   value: {stringValue: $c}},
    {key: "k8s.node.name",      value: {stringValue: "\($c)-probe-node"}},
    {key: "k8s.namespace.name", value: {stringValue: "default"}},
    {key: "k8s.pod.name",       value: {stringValue: "probe"}}]}')
  # A wandering CPU value, so the chart shows a line rather than a flat one.
  metrics=$(jq -n --argjson r "$resource" --arg t "$now" --argjson cpu "0.$((RANDOM % 90 + 10))" '{resourceMetrics: [{resource: $r,
    scopeMetrics: [{scope: {name: "probe"}, metrics: [
      {name: "k8s.node.cpu.usage", gauge: {dataPoints: [{timeUnixNano: $t, asDouble: $cpu}]}},
      {name: "k8s.pod.phase",      gauge: {dataPoints: [{timeUnixNano: $t, asInt: "2"}]}}]}]}]}')
  logs=$(jq -n --argjson r "$resource" --arg t "$now" '{resourceLogs: [{resource: $r,
    scopeLogs: [{scope: {name: "probe"}, logRecords: [
      {timeUnixNano: $t, severityText: "INFO", severityNumber: 9, body: {stringValue: "vcf-ops ingest probe"}}]}]}]}')
  for signal in metrics logs; do
    body=${!signal}
    code=$(curl -sS --cacert "$work/gateway-ca.crt" -o "$work/resp" -w '%{http_code}' \
      -H "Authorization: Bearer $access_token" -H 'Content-Type: application/json' \
      -d "$body" "$ingest/v1/$signal")
    printf '%s cluster_id=%-16s %-7s HTTP %s %s\n' "$(date +%T)" "$cluster" "$signal" "$code" "$(head -c 200 "$work/resp")"
  done
}

end=$(( $(date +%s) + ${PROBE_MINUTES:-10} * 60 ))
while :; do
  for cluster in "$@"; do send "$cluster" "$(( $(date +%s) * 1000000000 ))"; done
  [ "$(date +%s)" -lt "$end" ] || break
  sleep "${PROBE_INTERVAL:-30}"
  fetch_token
done
