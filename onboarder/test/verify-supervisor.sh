#!/usr/bin/env bash
# Assert an installed onboarder against a live Supervisor and VCF Operations.
# What unit tests can't cover: the built image and its digest, the package,
# the service's own ClusterRole, leader election, and the real Ops API.
#
# Run by `make verify-supervisor` after `make install-supervisor-service`,
# with .env filled in (see .env.example). Checks, in order:
#
#   1. The Deployment runs EXPECT_IMAGE (the digest in the registry for
#      DEV_VERSION), is available, and has won leader election.
#   2. ADOPT_CLUSTER, already onboarded, is adopted: the onboarder's field
#      manager has applied its credentials Secret, the Cluster has the
#      finalizer, the secret is the one it had before, and no secret was issued.
#   3. Only with ONBOARD_CLUSTER: label it, wait for onboarding (credentials
#      Secret, finalizer, Onboarded event, the cluster listed in Ops), then
#      unlabel it and wait for offboarding (Secret deleted, finalizer gone,
#      the cluster gone from Ops). It leaves the cluster as it found it.
#   4. The onboarder logged no errors.
set -euo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/dotenv.sh"
load_dotenv

: "${EXPECT_IMAGE:?run through make verify-supervisor, which resolves EXPECT_IMAGE}"
: "${ADOPT_CLUSTER:?set ADOPT_CLUSTER=<namespace>/<cluster> in .env}"
: "${OPS_URL:?set OPS_URL in .env}"
ADDON=platform-observability
FM=platform-observability-onboarder
TIMEOUT=${WAIT_TIMEOUT:-300}

log()  { echo "[verify] $*"; }
pass() { echo "[verify] PASS: $*"; }
fail() { echo "[verify] FAIL: $*" >&2; [[ -n "${NS:-}" ]] && kubectl -n "$NS" logs deploy/$FM --tail=40 >&2 || true; exit 1; }

# wait_for <description> <command...>: retry until the command succeeds.
wait_for() {
  local what="$1"; shift
  local deadline=$((SECONDS + TIMEOUT))
  until "$@" >/dev/null 2>&1; do
    (( SECONDS < deadline )) || fail "timed out after ${TIMEOUT}s waiting for: $what"
    sleep 5
  done
}

# Ops, as the onboarding account: the list of onboarded cluster names.
ops_clusters() {
  local pw  # the file first, as in install-supervisor-service.sh
  if [[ -n "${OPS_PASSWORD_FILE:-}" ]]; then pw="$(cat "$OPS_PASSWORD_FILE")"; else pw="${OPS_PASSWORD:?set OPS_PASSWORD_FILE}"; fi
  local k=(); [[ "${OPS_INSECURE:-false}" == true ]] && k=(-k)
  local token
  token="$(jq -n --arg u "${OPS_USERNAME:-platform-observability-onboard}" --arg p "$pw" '{username:$u,password:$p}' \
    | curl -sS "${k[@]}" -X POST "$OPS_URL/suite-api/api/auth/token/acquire" \
        -H 'Content-Type: application/json' -H 'Accept: application/json' --data-binary @- | jq -r .token)"
  curl -sS "${k[@]}" "$OPS_URL/suite-api/internal/extension/vodap/app0/configurations" \
    -H "Authorization: OpsToken $token" -H 'Accept: application/json' \
    -H 'X-vRealizeOps-API-use-unsupported: true' | jq -r '.[].clusterName'
}

# The credentials Secret, platform-observability-<cluster>. secret_of returns
# clientSecret still base64-encoded: compared, never printed.
cred_get()  { kubectl -n "$1" get secret "$ADDON-$2" -o json --show-managed-fields 2>/dev/null; }
secret_of() { cred_get "$1" "$2" | jq -r '.data.clientSecret // empty'; }
has_fin()   { kubectl -n "$1" get cluster.cluster.x-k8s.io "$2" -o json | jq -e '.metadata.finalizers // [] | index("platform-observability/offboard")'; }
no_fin()    { ! has_fin "$1" "$2"; }
owned()     { cred_get "$1" "$2" | jq -e --arg fm "$FM" '[.metadata.managedFields[] | select(.manager == $fm and .operation == "Apply")] | length > 0'; }
in_ops()    { ops_clusters | grep -qx "$1"; }
not_in_ops(){ ! in_ops "$1"; }
event()     { kubectl -n "$1" get events --field-selector "involvedObject.name=$2,reason=$3" -o name | grep -q .; }

# --- 1. the Deployment ---------------------------------------------------
NS="$(kubectl get deployment -A --field-selector metadata.name=$FM -o jsonpath='{.items[0].metadata.namespace}' 2>/dev/null || true)"
[[ -n "$NS" ]] || fail "no $FM Deployment on the Supervisor"
IMAGE="$(kubectl -n "$NS" get deployment $FM -o jsonpath='{.spec.template.spec.containers[0].image}')"
[[ "$IMAGE" == "$EXPECT_IMAGE" ]] || fail "running $IMAGE, expected $EXPECT_IMAGE"
kubectl -n "$NS" rollout status deployment/$FM --timeout=120s >/dev/null || fail "Deployment not available"
pass "Deployment in $NS runs $EXPECT_IMAGE"
wait_for "leader election" bash -c "kubectl -n '$NS' logs deploy/$FM | grep -q 'Successfully acquired lease'"
pass "won leader election (the service's Role covers leases)"

# --- 2. adoption -----------------------------------------------------------
ANS="${ADOPT_CLUSTER%%/*}"; ACL="${ADOPT_CLUSTER##*/}"
before="$(secret_of "$ANS" "$ACL")"
[[ -n "$before" ]] || fail "$ADOPT_CLUSTER has no Secret $ADDON-$ACL; it must be onboarded already"
wait_for "$ADOPT_CLUSTER Secret applied by $FM" owned "$ANS" "$ACL"
wait_for "$ADOPT_CLUSTER finalizer" has_fin "$ANS" "$ACL"
[[ "$(secret_of "$ANS" "$ACL")" == "$before" ]] || fail "$ADOPT_CLUSTER secret changed: adoption issued a new one"
if event "$ANS" "$ACL" Onboarded || event "$ANS" "$ACL" SecretRotated; then
  fail "$ADOPT_CLUSTER got an Onboarded/SecretRotated event: a secret was issued during adoption"
fi
pass "$ADOPT_CLUSTER adopted: Secret applied by $FM, finalizer set, same secret, nothing issued"

# --- 3. onboard and offboard ---------------------------------------------
if [[ -n "${ONBOARD_CLUSTER:-}" ]]; then
  ONS="${ONBOARD_CLUSTER%%/*}"; OCL="${ONBOARD_CLUSTER##*/}"
  label="addons.kubernetes.vmware.com/$ADDON"
  [[ -z "$(kubectl -n "$ONS" get cluster.cluster.x-k8s.io "$OCL" -o jsonpath="{.metadata.labels.addons\.kubernetes\.vmware\.com/$ADDON}")" ]] \
    || fail "$ONBOARD_CLUSTER is already labelled; pick an unlabelled cluster"
  cleanup() {
    kubectl -n "$ONS" label cluster.cluster.x-k8s.io "$OCL" "$label-" >/dev/null 2>&1 || true
  }
  trap cleanup EXIT

  log "labelling $ONBOARD_CLUSTER"
  kubectl -n "$ONS" label cluster.cluster.x-k8s.io "$OCL" "$label=enabled" >/dev/null
  wait_for "$ONBOARD_CLUSTER secret" bash -c "[[ -n \"\$(kubectl -n '$ONS' get secret '$ADDON-$OCL' -o jsonpath='{.data.clientSecret}' 2>/dev/null)\" ]]"
  wait_for "$ONBOARD_CLUSTER finalizer" has_fin "$ONS" "$OCL"
  wait_for "$ONBOARD_CLUSTER Onboarded event" event "$ONS" "$OCL" Onboarded
  wait_for "$OCL listed in Ops" in_ops "$OCL"
  pass "$ONBOARD_CLUSTER onboarded: secret written, finalizer set, Onboarded event, listed in Ops"

  log "unlabelling $ONBOARD_CLUSTER"
  kubectl -n "$ONS" label cluster.cluster.x-k8s.io "$OCL" "$label-" >/dev/null
  wait_for "$ONBOARD_CLUSTER finalizer removed" no_fin "$ONS" "$OCL"
  ! cred_get "$ONS" "$OCL" >/dev/null || fail "$ONBOARD_CLUSTER still has Secret $ADDON-$OCL after offboarding"
  wait_for "$OCL gone from Ops" not_in_ops "$OCL"
  pass "$ONBOARD_CLUSTER offboarded: Secret deleted, finalizer removed, gone from Ops"
  trap - EXIT
fi

# --- 4. no errors --------------------------------------------------------
errors="$(kubectl -n "$NS" logs deploy/$FM | grep -c '"level":"error"' || true)"
[[ "$errors" == 0 ]] || fail "$errors error lines in the onboarder log"
pass "no errors in the onboarder log"
log "ALL CHECKS PASSED for $EXPECT_IMAGE"
