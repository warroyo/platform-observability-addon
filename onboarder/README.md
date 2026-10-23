# platform-observability-onboarder

A Supervisor Service that does what `scripts/onboard.sh` does, for every
cluster, without anyone running it. Label a VKS cluster
`addons.kubernetes.vmware.com/platform-observability=enabled`, and the onboarder
gets the cluster's OAuth client from VCF Operations and writes it into the
Secret `platform-observability-<cluster>` in the cluster's Supervisor
namespace. The service's AddonInstall then attaches the addon, and chart 0.2.0
reads the Secret through its own AddonConfigDefinition. That needs VKS 3.8 or
later. Clusters on chart 0.1.0 need [legacy mode](#legacy-mode-and-the-shared-addonconfig).

It logs in to Ops with the restricted account from `scripts/ops-account.sh`
(one privilege, `extensions.management.usage`). It needs no VSP credential.

## What it does

| The Cluster | What the onboarder does |
|---|---|
| Labelled, and neither the Secret nor the AddonConfig has a secret | `POST configurations`: creates the cluster's client in Ops and issues it a secret. Writes the Secret, and adds the finalizer `platform-observability/offboard`. |
| Labelled, no Secret, and the AddonConfig has `vcfOps` credentials (from chart 0.1.0, an older onboarder or `onboard.sh`) | Adopts them: copies them into the Secret with no `POST`, so nothing rotates. Then removes them from the AddonConfig, unless legacy mode is on. |
| Labelled, and the Secret already has a secret | Writes nothing on later resyncs. |
| Annotated `platform-observability/rotate` (any value, empty included) | Issues a new secret and writes it to the Secret, together with the annotation `platform-observability/rotated-for: <value>` on the Secret. Then removes the Cluster's annotation, only if it still holds that value: a newer request set meanwhile is kept and answered next. If removing it fails, the retry sees the marker and only removes the annotation. Once the annotation is gone the marker is dropped, so the same value can ask again later. Don't edit the marker: without it, an unfinished request is answered a second time. |
| Its credentials have a `clientSecret` but no `clientId` | Nothing, and an `IncompleteCredentials` Warning event. It won't replace a secret it wasn't asked to; annotate the cluster to rotate. |
| Label removed, or the Cluster deleted | Deletes the Secret, removes any credentials from the AddonConfig, then `DELETE configurations/<cluster>` (which also deletes the VSP client), then removes the finalizer. In that order, a cluster labelled again halfway through finds no secret and onboards again, instead of adopting one Ops has deleted. |
| Annotated `platform-observability/skip-offboard` | Offboards without calling Ops, for when Ops is gone for good and the finalizer would otherwise block deleting the cluster. |
| Same name as a cluster in another namespace that holds the name (carries the finalizer, labelled or still offboarding) | Nothing: no `POST`, no adoption, no finalizer, and a `DuplicateClusterName` Warning event. Ops keys clusters by name alone, so onboarding the second would rotate the first one's secret, and offboarding either would delete the client both use. |

It reports on the Cluster with events: `Onboarded`, `SecretRotated`,
`Offboarded`, `DuplicateClusterName`, `IncompleteCredentials`.

Offboarding removes `vcfOps` and the secret hash from the AddonConfig even when
another field manager also owns them (an AddonConfig first written by
`onboard.sh`), and leaves every other key alone.

## The credentials Secret

The Secret `platform-observability-<cluster>` is the source of truth. It has
these keys:

| Key | Holds |
|---|---|
| `clientId` | the cluster's OAuth client ID |
| `clientSecret` | the OAuth client secret |
| `instanceFqdn` | the ingest endpoint host |
| `platformFqdn` | the identity endpoint host |
| `instanceCA` | the ingest endpoint CA, PEM |
| `platformCA` | the identity endpoint CA, PEM |

It carries the label `app.kubernetes.io/managed-by:
platform-observability-onboarder` and the annotation
`platform-observability/secret-hash`. During a rotation it also carries
`platform-observability/rotated-for`. The onboarder writes it with server-side
apply, field manager `platform-observability-onboarder`, forced.

The rotate marker lives on the Secret now. Chart 0.1.0 and the onboarder before
this change kept it on the AddonConfig.

Who can read the OAuth secret: anyone who can read Secrets in the namespace,
not anyone who can read AddonConfigs. Tenants can no longer change it by
editing an AddonConfig.

The ClusterRole grants `secrets` `get`, `create`, `patch` and `delete`, and no
`list` or `watch`. The Supervisor refuses a cluster-wide Secret list or watch
to a third-party service account. So the onboarder reads Secrets by name with
live reads and never caches or watches them. A deleted credentials Secret is
noticed at the next resync (`sync_period`, 10 minutes by default). The
onboarder then onboards the cluster again, which issues a new secret.

## Legacy mode and the shared AddonConfig

Without legacy mode the onboarder writes nothing to the AddonConfig. It only
removes the credentials it finds there. Argo CD can own AddonConfigs as usual,
with a normal sync and no `ServerSideApply` or `ServerSideDiff` requirement.
Non-secret values such as `aggregator.replicaCount` pass through the 0.2.0
AddonConfigDefinition to the chart.

Legacy mode is the flag `--legacy-addon-config` and the service value
`legacy_addon_config` (default `false`). When it is on, the onboarder also
writes the old AddonConfig keys, for clusters still on chart 0.1.0. When it is
off, it removes the credentials from the AddonConfig and leaves other managers'
keys alone.

In legacy mode the onboarder writes with server-side apply, field manager
`platform-observability-onboarder`, and owns only these AddonConfig keys:

```
spec.values.helmValues.clusterName
spec.values.helmValues.vcfOps.*
spec.values.helmValues.aggregator.podAnnotations["platform-observability/secret-hash"]
```

Anything else in the same AddonConfig can be owned by Argo CD or anyone else.
To manage the rest from Argo CD in legacy mode:

- Give the Application `ServerSideApply=true` and `ServerSideDiff=true`, so the
  onboarder's keys do not show as drift.
- Keep the onboarder's keys out of Git. Argo CD's server-side sync forces
  conflicts, so a `vcfOps` key in Git would take it over.
- Let the onboarder create the AddonConfig. If Argo CD creates it and later
  prunes it, the onboarder's keys go with it. The Secret stays, so the
  onboarder writes the keys again from it.

In legacy mode the secret is in plain text in the AddonConfig, readable by
anyone who can read AddonConfigs in the namespace.

## Upgrade from chart 0.1.0

This path moves clusters to chart 0.2.0 without rotating any secret. The
end-to-end upgrade on a live Supervisor has not been run yet, so it is untested
there.

1. Upgrade the onboarder service with `legacy_addon_config: true` and
   `addon_versions` including `0.2.0`. It writes each cluster's Secret from the
   AddonConfig values it already has, with no `POST`, and keeps the AddonConfig
   keys.
2. Move clusters to 0.2.0. They move on their own if the AddonInstall has no
   `releaseFilter`. Otherwise change `addon_release`. The 0.2.0
   AddonConfigDefinition also reads legacy AddonConfig values, so the order of
   steps 1 and 2 does not matter.
3. Set `legacy_addon_config: false`. The onboarder removes the credentials from
   the AddonConfigs.

## Never issue a secret it doesn't have to

Issuing a secret revokes the old one at once, and the collectors only pick a
new one up on helm-controller's next interval (up to 10 minutes). So:

- Every decision that leads to a `POST` is made from a live read of the
  Cluster, the Secret and the AddonConfig, never from the cache. A stale cache would
  otherwise `POST` again right after onboarding.
- A resync makes no Ops call.
- Reconciles of the same cluster name never run at the same time.
- It reads the endpoints before the `POST`, so an Ops that can't serve them
  fails before a secret is issued. The exception is an Ops with no cluster
  onboarded yet, which lists endpoints only after the first `POST`.
- A secret issued by a `POST` is kept in memory, keyed by the Cluster's UID,
  until a live read shows it written. Every reconcile settles it first, before
  anything else, so a failed write, a write that succeeded but reported an
  error, or a rotate request removed in between never leads to a second
  `POST`, and a stale entry is never reused.
- What it can't cover: a crash between the `POST` and the write. The cluster's
  old secret is already revoked, and the new one is lost with the process. The
  next reconcile finds no secret and onboards again, so the cluster recovers
  with one extra secret issued.

## Scale

- At most `max_concurrent` reconciles at once (default 4), which bounds the
  `POST`s when many clusters are labelled at once.
- The endpoints and CA chains are the same for every cluster, so one
  `GET configurations` (which lists every cluster) serves all of them for
  `endpoints-ttl` (10 minutes). Concurrent misses wait for one refresh, and a
  failed refresh is shared for 5 seconds, so a failing Ops gets one `GET` per
  window. The secret and client ID come from each `POST`.
- The duplicate-name check lists Clusters with a `metadata.name` field
  selector, and only for a cluster that doesn't hold the name yet.
- Clusters are cached without their managed fields; AddonConfigs are watched
  as metadata only. Secrets are never cached or watched.

## Install

1. Create the Ops account: `scripts/ops-account.sh create`.
2. The service registers the addon itself (an AddonRepository and its
   AddonRepositoryInstall). On VKS 3.8 a helm AddonRepositoryInstall in a
   Supervisor Service namespace also propagates to guests, which it did not
   before, and that is what lets the service do this. Where it is already registered with
   `install/addonrepository.yaml`, either delete that registration first or
   install with `addon_repository: false`: two would both create the same
   Addon.
3. In vCenter, Workload Management > Services > Add, upload
   `platform-observability-onboarder.yml` from a release (tag
   `onboarder-v<version>`), then install it on the Supervisor with these values:

| Value | Default | |
|---|---|---|
| `ops_url` | | VCF Operations, e.g. `https://ops.example.com`. Required. |
| `ops_username` | `platform-observability-onboard` | |
| `ops_password` | | Required. |
| `ops_ca` | | PEM CA chain for the Ops certificate, if not from a public CA. |
| `ops_insecure` | `false` | Skip TLS verification. Labs only. |
| `addon_name` | `platform-observability` | |
| `sync_period` | `10m` | |
| `max_concurrent` | `4` | |
| `legacy_addon_config` | `false` | Also write the credentials into the AddonConfig, for clusters still on chart 0.1.0. See [Upgrade from chart 0.1.0](#upgrade-from-chart-010). |
| `addon_install` | `true` | Ship one AddonInstall with `crossNamespaceSelection: Allowed`. |
| `addon_release` | | Pin the AddonInstall to one AddonRelease, e.g. `platform-observability.0.2.0`. |
| `addon_repository` | `true` | Register the addon's helm repository. |
| `addon_chart_repository` | `https://warroyo.github.io/platform-observability-addon` | |
| `addon_versions` | `["0.1.0", "0.2.0"]` | Chart versions to offer, one AddonRelease each. The only repository setting an upgrade can change. |

4. Label a cluster `addons.kubernetes.vmware.com/platform-observability=enabled`,
   plus `addon.addons.kubernetes.vmware.com/helm-controller=managed` if it does
   not have helm-controller.

## Develop

```sh
make test                      # unit tests: a fake Ops server and a fake client with server-side apply
OPS_URL=https://ops.example.com OPS_PASSWORD=... \
  go run . --leader-elect=false --kubeconfig ~/.kube/config
make release VERSION=0.1.0     # CI does this for a tag onboarder-v0.1.0
```

Running it locally uses your own kubeconfig identity, not the service's
ClusterRole, so it does not test the RBAC in `config/deploy.yml`.
