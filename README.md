# platform-observability addon

Onboards a VKS cluster to VCF Operations Platform Observability (Extensions >
Platform Observability > Platform Services) as a VKS helm addon, not through
the "Onboard VKS Cluster" dialog.

The dialog hands out a list of commands: two CA certificates, an OAuth
client, a random token, and two helm installs of the OpenTelemetry collector
with values files to download. All of that is per cluster and copied by hand.
Here it is one chart and a Supervisor Service, the onboarder, that gets each
labelled cluster's values from the VCF Operations API and keeps them in a
Secret in the cluster's Supervisor namespace. A script does the same for one
cluster by hand.

## What is in it

- `chart/platform-observability`: an umbrella chart. It creates the
  `vcf-monitoring` namespace (Pod Security `privileged`), the four secrets the
  dialog creates, and both collector releases. The collectors are the
  upstream `opentelemetry-collector` chart 0.158.1, twice, as `aggregator`
  and `agent`, with the VCF Operations values files. The only changes to
  those values are marked `changed:`. The aggregator reads the cluster ID and
  the two VCF Operations endpoints from a ConfigMap, so the values are the
  same for every cluster.
- `chart/addon-config-definition.yaml`: the source of the addon's
  AddonConfigDefinition (ACD). Since chart 0.2.0 the chart ships it as the
  `addons.kubernetes.vmware.com/addon-config-definition` annotation in
  `Chart.yaml`, which VKS 3.8 and later read. Before 3.8, a helm
  AddonRepository got a generated ACD with only `helmValues` and `helmOptions`.
  This one reads the Secret `platform-observability-<cluster>` as an optional
  template input and renders the credentials into the chart values.
- `scripts/embed-acd.sh`: writes that file into `Chart.yaml` (gzip, then
  base64) and sets `metadata.name` to `platform-observability.<version>`.
  `scripts/embed-acd.sh --check` runs in the release workflow. Run the script
  again after editing the ACD or bumping the chart version.
- `install/addonrepository.yaml`: the AddonRepository and
  AddonRepositoryInstall that register the chart as the addon
  `platform-observability`. A Supervisor administrator applies them once.
- `scripts/onboard.sh`: prints a cluster's credentials Secret, built from the VCF
  Operations configurations API. It is the manual path for one cluster.
- `onboarder/`: a Supervisor Service controller that onboards every labelled
  cluster without running `onboard.sh` by hand. It writes the credentials
  Secret and, by default, registers the addon too. See
  [onboarder/README.md](onboarder/README.md).
- `scripts/ops-account.sh`: creates the restricted VCF Operations role and
  user that the onboarder and `onboard.sh` log in with, so onboarding does not need the Ops
  admin password.
- `scripts/probe-ingest.sh`: sends fake OTLP metrics and logs with one
  cluster's OAuth client, to test the ingest path without a cluster.
- `examples/addoninstall.yaml`: attaches the addon to labelled clusters in a
  Supervisor namespace.

The chart is published to GitHub Pages by `.github/workflows/release.yaml`.

## Requirements

The custom ACD needs VKS 3.8 or later. On 3.8 a helm AddonRepositoryInstall in
a Supervisor Service namespace also propagates to guests, which it did not
before. That is what lets the onboarder service register the addon itself.

## Use it

1. Register the addon, once per Supervisor. The onboarder service does this
   itself (value `addon_repository`, default `true`), so skip this step if you
   install it. To do it by hand:

   ```sh
   kubectl apply -f install/addonrepository.yaml
   ```

2. Create the onboarding account in VCF Operations, once per Ops instance.
   This is the only step that needs the Ops admin password:

   ```sh
   VCF_OPS_URL=https://ops.example.com VCF_OPS_ADMIN_PASSWORD=... \
     scripts/ops-account.sh create
   ```

   It writes the new user's password to `~/.config/vcf-ops/onboard.password`.
   See [The onboarding account](#the-onboarding-account).

3. Install the onboarder service on the Supervisor with that account. See
   [onboarder/README.md](onboarder/README.md#install). By default it also ships
   an AddonInstall that attaches the addon to labelled clusters in every
   namespace.

4. Label each cluster:

   ```sh
   kubectl -n my-namespace label cluster lab1-vks-01 \
     addons.kubernetes.vmware.com/platform-observability=enabled \
     addon.addons.kubernetes.vmware.com/helm-controller=managed
   ```

The onboarder writes the Secret `platform-observability-lab1-vks-01` in
`my-namespace`, and the chart reads it through the ACD. The second label
installs the `helm-controller` addon, which helm addons need.

To move to a newer chart, see
[Upgrading the chart](onboarder/README.md#upgrading-the-chart).

### Without the onboarder

For a single cluster you can skip the onboarder. Register the addon (step 1),
create the account (step 2), then attach the addon in the cluster's Supervisor
namespace (edit the namespace first):

```sh
kubectl apply -f examples/addoninstall.yaml
```

Write the cluster's credentials Secret and label the cluster:

```sh
VCF_OPS_URL=https://ops.example.com \
VCF_OPS_USERNAME=platform-observability-onboard \
VCF_OPS_PASSWORD=$(cat ~/.config/vcf-ops/onboard.password) \
  scripts/onboard.sh lab1-vks-01 my-namespace | kubectl apply -f -
kubectl -n my-namespace label cluster lab1-vks-01 \
  addons.kubernetes.vmware.com/platform-observability=enabled \
  addon.addons.kubernetes.vmware.com/helm-controller=managed
```

`onboard.sh` prints the Secret `platform-observability-<cluster>`. The 0.3.0 ACD
reads the credentials from that Secret only. The onboarder can take the Secret
over later without issuing a new secret.

## The onboarding account

`onboard.sh` needs one VCF Operations privilege, `extensions.management.usage`
(Extensions > Use). With only that, `GET` and `POST configurations` both
answer 200. `scripts/ops-account.sh create` makes a custom role
`platform-observability-onboard` with that one privilege, and a local user of
the same name that holds only that role, on all objects. It is safe to run
again: it updates the role and leaves the password alone.

No built-in role works, not even Administrator. Administrator has every
privilege except `extensions.management.usage`,
`extensions.management.manage` and `administration.logInsight_viewpage`, so
its users get 403 from the configurations API. Only the local `admin` user
gets through without the custom role. `extensions.management.manage` alone
also gets 403.

What the account can do:

- Read every onboarded cluster's configuration: certificates, endpoints and
  OAuth client IDs, no secrets. `GET` ignores `clusterName` and returns all
  of them.
- Issue a new OAuth secret for any cluster, onboarded or not. That revokes the
  old secret, and the cluster's aggregators get 401s until they have the new
  one. Keep this password as carefully as the cluster secrets.

Two API details `ops-account.sh` handles: a user created through
`POST /suite-api/api/auth/users` starts disabled, and
`PUT /users/<id>/enable` answers 405, so it sends the whole user back with
`PUT /suite-api/api/auth/users` and `enabled: true`.

`--check` mode does not use this account. It reads VSP as `admin@vsp.local`.

`scripts/ops-account.sh delete` removes the user and the role.

## The VCF Operations API

The dialog calls
`/vcf-operations/rest/ops/internal/extension/vodap/app0/configurations?clusterName=<cluster>`,
which wants a browser session. The same API is at
`/suite-api/internal/extension/vodap/app0/configurations` and takes an
`OpsToken` from `/suite-api/api/auth/token/acquire` with the
`X-vRealizeOps-API-use-unsupported: true` header. It is an internal API and
can change in another release.

The method decides what it does.

`GET` returns the cluster's configuration and changes nothing:

| Field | Used for |
|---|---|
| `vmspInstanceFqdn` | `https://<fqdn>/otel-ingest-service`, where metrics and logs go |
| `vmspInstanceCertificate` | `otel-gateway-tls-ca`, PEM |
| `vmspPlatformFqdn` | `https://<fqdn>/api/v1/identity/token`, the OAuth token URL |
| `vmspPlatformCertificate` | `otel-gateway-oauth-tls-ca`, PEM |
| `saClientId` | `otel-gateway-oauth-creds` client ID |

`POST` (empty body) is what the dialog sends when a cluster name is entered.
It creates the cluster's OAuth client if needed, issues the client a new
secret, and returns the dialog's `values-aggregator.yaml`, `values-agent.yaml`
and `commands`. Step 5 of `commands` holds the new secret. `onboard.sh` sends
the POST, takes the secret from step 5, and uses the GET for the rest.

`DELETE /suite-api/internal/extension/vodap/app0/configurations/<cluster>`
offboards a cluster: it removes the cluster from the list and deletes its
OAuth client (`svc-vodap-app0-<cluster>`) in VSP, so the cluster's collectors
stop getting tokens. The onboarding account can do it; it needs no more than
`extensions.management.usage`. `DELETE configurations?clusterName=<cluster>`
answers 405.

A VSP client left behind without an Ops entry, for example one made directly
in VSP, is deleted in VSP with
`DELETE /api/v1/identity/service-accounts?componentType=vcf-obs-data-platform&componentId=<id>&clientId=<url-encoded saClientId>`
as `admin@vsp.local`. A client ID in the path answers 307, and `clientName`
answers 400.

## The secret rotates whenever it is fetched

A cluster keeps the same OAuth client (`svc-vodap-app0-<cluster>` in the VSP
identity service), but there is no way to read its secret, only to issue a
new one, and a new one revokes the old one at once. Opening the onboarding
dialog for a cluster issues a new one, and that cluster's aggregators then
get 401s until they have it.

**Do not open the "Onboard VKS Cluster" dialog for a cluster this addon
manages.** Entering the cluster's name there is the same `POST
configurations` that `onboard.sh` sends: it revokes the cluster's
current secret at once, and nothing tells the addon. Rotate a cluster's secret
with the `platform-observability/rotate` annotation (see the onboarder) or
`onboard.sh` instead, and offboard it with `DELETE
configurations/<cluster>`, not from the dialog. If the dialog was opened
anyway, the sign is 401s in the aggregator logs; annotate the cluster to
rotate, or run `onboard.sh` for that cluster and apply its output.

`--check` below is for a person looking into a cluster. It logs in to VSP as
`admin@vsp.local`, so automation should not depend on it: the onboarding
account alone onboards, rotates and offboards.

`onboard.sh` records on the Secret when the secret it carries was
issued (`platform-observability/secret-rotated-at`, from VSP's
`lastSecretRotationDate`), and `onboard.sh <cluster> <ns> --check` compares
that with VSP without touching the secret. It exits 3 when the secret was
rotated elsewhere. Then run `onboard.sh` again, which issues a new secret,
and apply its output:

```sh
scripts/onboard.sh "$c" "$ns" --check || scripts/onboard.sh "$c" "$ns" | kubectl apply -f -
```

It also
puts a hash of the secret on the aggregator pods, so a new secret rolls them.

## Limits

- The OAuth client secret is in the Secret `platform-observability-<cluster>`
  in the cluster's Supervisor namespace, readable by anyone who can read
  Secrets there. It is in no other object.
- A new secret reaches the aggregators only on helm-controller's HelmRelease
  interval (10 minutes). ACD output templates carry no metadata, so the values
  Secret can't get Flux's watch label.
- Only the aggregators roll on a new OAuth secret. Nothing restarts when the
  CA certificates or endpoints change; restart the collectors then.
- The agent-to-aggregator token is generated on first install and kept
  across upgrades (helm `lookup`). Set `internalToken` to choose it.
