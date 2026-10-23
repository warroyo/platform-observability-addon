# VCF Operations API

## The API

Onboarding uses `/suite-api/internal/extension/vodap/app0/configurations`, which
takes an `OpsToken` from `/suite-api/api/auth/token/acquire` with the
`X-vRealizeOps-API-use-unsupported: true` header. It is an internal API and can
change in another release.

The method decides what it does.

`GET` returns the cluster's configuration and changes nothing. It ignores
`clusterName` and returns every onboarded cluster.

| Field | Used for |
|---|---|
| `vmspInstanceFqdn` | `https://<fqdn>/otel-ingest-service`, where metrics and logs go |
| `vmspInstanceCertificate` | `otel-gateway-tls-ca`, PEM |
| `vmspPlatformFqdn` | `https://<fqdn>/api/v1/identity/token`, the OAuth token URL |
| `vmspPlatformCertificate` | `otel-gateway-oauth-tls-ca`, PEM |
| `saClientId` | `otel-gateway-oauth-creds` client ID |

`POST` (empty body, `?clusterName=<cluster>`) creates the cluster's OAuth client
in the VSP identity service if needed (with the `app0` role), issues the client
a new secret, and returns install `commands`. Step 5 of `commands` holds the new
secret. The onboarder and `onboard.sh` send the POST, take the secret from
step 5, and use the GET for the rest.

`DELETE /suite-api/internal/extension/vodap/app0/configurations/<cluster>`
offboards a cluster: it removes the cluster from the list and deletes its OAuth
client (`svc-vodap-app0-<cluster>`) in VSP, so the cluster's collectors stop
getting tokens. The onboarding account can do it; it needs no more than
`extensions.management.usage`. `DELETE configurations?clusterName=<cluster>`
answers 405.

A VSP client left behind without an Ops entry, for example one made directly in
VSP, is deleted in VSP with
`DELETE /api/v1/identity/service-accounts?componentType=vcf-obs-data-platform&componentId=<id>&clientId=<url-encoded saClientId>`
as `admin@vsp.local`. A client ID in the path answers 307, and `clientName`
answers 400.

Other facts about the API:

- Ops keys clients and `cluster_id` on the bare cluster name:
  `svc-vodap-app0-<cluster>`.
- The `app0` role is not scoped to a cluster (`resources: null`), so a token can
  send data labelled as any cluster. One client per cluster is an Ops
  convention, not something the auth path enforces.
- Creating clients directly in the VSP identity service does not work for this:
  an admin of the `vsp` component can create a client and get its secret, but
  the `roles` in the request are dropped, and the ingest answers `403
  insufficient role privilege`. Clients have to come from Ops.

## The onboarding account

`onboard.sh` and the onboarder need one VCF Operations privilege,
`extensions.management.usage` (Extensions > Use). With only that, `GET` and
`POST configurations` both answer 200. `scripts/ops-account.sh create` makes a
custom role `platform-observability-onboard` with that one privilege, and a
local user of the same name that holds only that role, on all objects. It is
safe to run again: it updates the role and leaves the password alone.

```sh
VCF_OPS_URL=https://ops.example.com VCF_OPS_ADMIN_PASSWORD=... \
  scripts/ops-account.sh create
```

It writes the new user's password to `~/.config/vcf-ops/onboard.password`. This
is the only step that needs the Ops admin password.
`scripts/ops-account.sh delete` removes the user and the role.

No built-in role works, not even Administrator. Administrator has every
privilege except `extensions.management.usage`, `extensions.management.manage`
and `administration.logInsight_viewpage`, so its users get 403 from the
configurations API. Only the local `admin` user gets through without the custom
role. `extensions.management.manage` alone also gets 403.

What the account can do:

- Read every onboarded cluster's configuration: certificates, endpoints and
  OAuth client IDs, no secrets. `GET` ignores `clusterName` and returns all of
  them.
- Issue a new OAuth secret for any cluster, onboarded or not. That revokes the
  old secret, and the cluster's aggregators get 401s until they have the new
  one. Keep this password as carefully as the cluster secrets.

Two API details `ops-account.sh` handles: a user created through
`POST /suite-api/api/auth/users` starts disabled, and
`PUT /users/<id>/enable` answers 405, so it sends the whole user back with
`PUT /suite-api/api/auth/users` and `enabled: true`.

## Example: list onboarded clusters

This gets a token and lists the onboarded clusters. The password goes in
through a file and stdin, so it is not on a command line. Use `-k` only for a
self-signed lab Ops.

```sh
ops=https://ops.example.com
tok=$(jq -n --arg u platform-observability-onboard --rawfile p ~/.config/vcf-ops/onboard.password \
  '{username: $u, password: ($p | rtrimstr("\n"))}' \
  | curl -sk -X POST "$ops/suite-api/api/auth/token/acquire" \
      -H 'Content-Type: application/json' -H 'Accept: application/json' -d @- | jq -r .token)
curl -sk "$ops/suite-api/internal/extension/vodap/app0/configurations" \
  -H "Authorization: OpsToken $tok" -H 'Accept: application/json' \
  -H 'X-vRealizeOps-API-use-unsupported: true' | jq -r '.[].clusterName'
```

## The secret rotates whenever it is fetched

A cluster keeps the same OAuth client (`svc-vodap-app0-<cluster>` in the VSP
identity service), but there is no way to read its secret, only to issue a new
one, and a new one revokes the old one at once. The cluster's aggregators then
get 401s until they have it.

So the secret is issued once, stored, and only re-issued deliberately. Rotate
with the `platform-observability/rotate` annotation on the Cluster. See
[Architecture](architecture.md#what-the-onboarder-does-per-cluster-state).

## Checking for drift with --check

`--check` is for a person looking into a cluster. It logs in to VSP as
`admin@vsp.local`, so automation does not depend on it: the onboarding account
alone onboards, rotates and offboards. It does not use the onboarding account.

VSP's `lastSecretRotationDate` (`GET /api/v1/identity/service-accounts?componentType=vcf-obs-data-platform`)
shows when a client's secret was last issued, without touching it.
`onboard.sh` records on the Secret when the secret it carries was issued
(`platform-observability/secret-rotated-at`, from `lastSecretRotationDate`), and
`onboard.sh <cluster> <ns> --check` compares that with VSP without touching the
secret. It exits 3 when the secret was rotated elsewhere. Then run `onboard.sh`
again, which issues a new secret, and apply its output:

```sh
scripts/onboard.sh "$c" "$ns" --check || scripts/onboard.sh "$c" "$ns" | kubectl apply -f -
```

`onboard.sh` also puts a hash of the secret on the aggregator pods, so a new
secret rolls them.
