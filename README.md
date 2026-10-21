# platform-observability addon

Onboards a VKS cluster to VCF Operations Platform Observability (Extensions >
Platform Observability > Platform Services) as a VKS helm addon, not through
the "Onboard VKS Cluster" dialog.

The dialog hands out a list of commands: two CA certificates, an OAuth
client, a random token, and two helm installs of the OpenTelemetry collector
with values files to download. All of that is per cluster and copied by hand.
Here it is one chart, one Supervisor-side `AddonConfig` per cluster, and a
script that gets that cluster's values from the VCF Operations API.

## What is in it

- `chart/platform-observability`: an umbrella chart. It creates the
  `vcf-monitoring` namespace (Pod Security `privileged`), the four secrets the
  dialog creates, and both collector releases. The collectors are the
  upstream `opentelemetry-collector` chart 0.158.1, twice, as `aggregator`
  and `agent`, with the VCF Operations values files. The only changes to
  those values are marked `changed:`. The aggregator reads the cluster ID and
  the two VCF Operations endpoints from a ConfigMap, so the values are the
  same for every cluster.
- `install/addonrepository.yaml`: the AddonRepository and
  AddonRepositoryInstall that register the chart as the addon
  `platform-observability`. A Supervisor administrator applies them once.
- `scripts/onboard.sh`: prints a cluster's `AddonConfig`, built from the VCF
  Operations configurations API.
- `scripts/probe-ingest.sh`: sends fake OTLP metrics and logs with one
  cluster's OAuth client, to test the ingest path without a cluster.
- `examples/addoninstall.yaml`: attaches the addon to labelled clusters in a
  Supervisor namespace.

The chart is published to GitHub Pages by `.github/workflows/release.yaml`.

## Use it

1. Register the addon, once per Supervisor:

   ```sh
   kubectl apply -f install/addonrepository.yaml
   ```

2. Attach it in the cluster's Supervisor namespace (edit the namespace first):

   ```sh
   kubectl apply -f examples/addoninstall.yaml
   ```

3. For each cluster, write its AddonConfig and label the cluster:

   ```sh
   VCF_OPS_URL=https://ops.example.com VCF_OPS_PASSWORD=... \
     scripts/onboard.sh lab1-vks-01 my-namespace | kubectl apply -f -
   kubectl -n my-namespace label cluster lab1-vks-01 \
     addons.kubernetes.vmware.com/platform-observability=enabled \
     addon.addons.kubernetes.vmware.com/helm-controller=managed
   ```

The second label installs the `helm-controller` addon, which helm addons need.

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

## The secret rotates whenever it is fetched

A cluster keeps the same OAuth client (`svc-vodap-app0-<cluster>` in the VSP
identity service), but there is no way to read its secret, only to issue a
new one, and a new one revokes the old one at once. Opening the onboarding
dialog for a cluster issues a new one, and that cluster's aggregators then
get 401s until they have it.

So `onboard.sh` records on the AddonConfig when the secret it carries was
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

- The AddonConfig holds the OAuth client secret in plain text, in the
  cluster's Supervisor namespace.
- Only the aggregators roll on a new OAuth secret. Nothing restarts when the
  CA certificates or endpoints change; restart the collectors then.
- The agent-to-aggregator token is generated on first install and kept
  across upgrades (helm `lookup`). Set `internalToken` to choose it.
