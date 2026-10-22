#!/usr/bin/env bash
# Create the restricted VCF Operations account that onboard.sh logs in with,
# so onboarding does not need the Ops admin password.
#
#   scripts/ops-account.sh create    make or update the role, make the user
#   scripts/ops-account.sh delete    remove the user, then the role
#
# The account needs one privilege, extensions.management.usage (Extensions >
# Use), and nothing else: with it alone GET and POST configurations both
# answer 200. No built-in role works, not even Administrator: Administrator
# has every privilege except extensions.management.usage,
# extensions.management.manage and administration.logInsight_viewpage, so
# its users get 403 from the configurations API. Only the local admin user,
# which is outside roles, gets through without the custom role.
# extensions.management.manage alone also gets 403.
#
# What the account can do: read every onboarded cluster's configuration
# (certificates and endpoints, no secrets; GET ignores clusterName), and issue
# a new OAuth secret for any cluster, which revokes the old one and breaks
# that cluster's collectors until they have the new one. Keep the password
# as carefully as the cluster secrets.
#
# A user created through the API starts disabled, and PUT users/<id>/enable
# answers 405, so create sends the whole user back with enabled: true.
#
# The password is generated on the first create and written to
# OPS_ACCOUNT_PASSWORD_FILE (mode 600). It is never printed. Then:
#
#   VCF_OPS_USERNAME=platform-observability-onboard \
#   VCF_OPS_PASSWORD=$(cat ~/.config/vcf-ops/onboard.password) \
#     scripts/onboard.sh lab1-vks-01 my-namespace | kubectl apply -f -
#
# Environment:
#   VCF_OPS_URL                  required, e.g. https://ops.example.com
#   VCF_OPS_ADMIN_USERNAME       default admin
#   VCF_OPS_ADMIN_PASSWORD       required
#   OPS_ACCOUNT_USERNAME         default platform-observability-onboard
#   OPS_ACCOUNT_ROLE             default platform-observability-onboard
#   OPS_ACCOUNT_PASSWORD_FILE    default ~/.config/vcf-ops/onboard.password
#   VCF_OPS_INSECURE             set to 1 to skip TLS verification
set -euo pipefail

ops_url=${VCF_OPS_URL:?VCF_OPS_URL is required}; ops_url=${ops_url%/}
: "${VCF_OPS_ADMIN_PASSWORD:?VCF_OPS_ADMIN_PASSWORD is required}"
username=${OPS_ACCOUNT_USERNAME:-platform-observability-onboard}
role=${OPS_ACCOUNT_ROLE:-platform-observability-onboard}
password_file=${OPS_ACCOUNT_PASSWORD_FILE:-$HOME/.config/vcf-ops/onboard.password}
curl_opts=(-sS -m 30)
[ "${VCF_OPS_INSECURE:-0}" = 1 ] && curl_opts+=(-k)

token=$(curl "${curl_opts[@]}" -X POST "$ops_url/suite-api/api/auth/token/acquire" \
  -H 'Content-Type: application/json' -H 'Accept: application/json' \
  -d "$(jq -n --arg u "${VCF_OPS_ADMIN_USERNAME:-admin}" --arg p "$VCF_OPS_ADMIN_PASSWORD" \
        '{username: $u, password: $p}')" | jq -r '.token // empty')
[ -n "$token" ] || { echo "VCF Operations login failed" >&2; exit 1; }

# api <method> <path> [body]: prints the body, then the status on its own line.
api() {
  local args=(-X "$1" -H "Authorization: OpsToken $token" -H 'Accept: application/json' -w '\n%{http_code}')
  [ $# -gt 2 ] && args+=(-H 'Content-Type: application/json' -d "$3")
  curl "${curl_opts[@]}" "${args[@]}" "$ops_url/suite-api/api/auth$2"
}
body() { sed '$d' <<<"$1"; }
status() { tail -n1 <<<"$1"; }
user_id() {
  body "$(api GET "/users?username=$username")" \
    | jq -r --arg u "$username" '[.users[]? | select(.username == $u) | .id][0] // empty'
}

case ${1:-} in
  create)
    role_body=$(jq -n --arg n "$role" '{name: $n, displayName: $n,
      description: "VCF Operations Platform Observability onboarding (scripts/onboard.sh)",
      "privilege-keys": ["extensions.management.usage"]}')
    if [ "$(status "$(api GET "/roles/$role")")" = 200 ]; then
      out=$(api PUT /roles "$role_body"); echo "role $role: updated (HTTP $(status "$out"))"
    else
      out=$(api POST /roles "$role_body"); echo "role $role: created (HTTP $(status "$out"))"
    fi
    case $(status "$out") in 2*) ;; *) body "$out" >&2; exit 1 ;; esac

    id=$(user_id)
    if [ -z "$id" ]; then
      [ -s "$password_file" ] || {
        mkdir -p "$(dirname "$password_file")"
        # Ops wants upper, lower, digit and special characters.
        (umask 077; printf '%sAa1!' "$(openssl rand -base64 24 | tr -d '/+=\n')" > "$password_file")
      }
      out=$(api POST /users "$(jq -n --arg u "$username" --rawfile p "$password_file" --arg r "$role" \
        '{username: $u, password: $p, firstName: "Platform Observability", lastName: "onboarding",
          "role-permissions": [{roleName: $r, allowAllObjects: true}]}')")
      case $(status "$out") in 2*) ;; *) body "$out" | jq -c 'del(.password)?' >&2; exit 1 ;; esac
      id=$(user_id)
      echo "user $username: created, password in $password_file"
    else
      echo "user $username: exists"
    fi

    # Enabled, and holding only the role.
    user=$(body "$(api GET "/users/$id")" | jq -c --arg r "$role" \
      'del(.links) | .enabled = true | .roleNames = [$r] | ."role-permissions" = [{roleName: $r, allowAllObjects: true}]')
    out=$(api PUT /users "$user")
    case $(status "$out") in 2*) echo "user $username: enabled, role $role" ;; *) body "$out" >&2; exit 1 ;; esac
    ;;
  delete)
    id=$(user_id)
    if [ -n "$id" ]; then echo "user $username: deleted (HTTP $(status "$(api DELETE "/users/$id")"))"
    else echo "user $username: not found"; fi
    echo "role $role: deleted (HTTP $(status "$(api DELETE "/roles/$role")"))"
    ;;
  *)
    echo "usage: $0 create|delete" >&2; exit 2 ;;
esac
