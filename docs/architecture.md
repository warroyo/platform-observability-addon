# Architecture

## Components

- `chart/platform-observability`: an umbrella chart. It creates the
  `vcf-monitoring` namespace (Pod Security `privileged`), the CA, OAuth and
  internal token secrets, and both collector releases. The collectors are the
  upstream `opentelemetry-collector` chart 0.158.1, twice, as `aggregator` and
  `agent`, with the VCF Operations values files. The only changes to those
  values are marked `changed:`. The aggregator reads the cluster ID and the two
  VCF Operations endpoints from a ConfigMap, so the values are the same for
  every cluster.
- `chart/addon-config-definition.yaml`: the source of the addon's
  AddonConfigDefinition (ACD). Since chart 0.2.0 the chart ships it as the
  `addons.kubernetes.vmware.com/addon-config-definition` annotation in
  `Chart.yaml`, which VKS 3.8 and later read. Before 3.8, a helm
  AddonRepository got a generated ACD with only `helmValues` and `helmOptions`.
  This ACD reads the Secret `platform-observability-<cluster>` as an optional
  template input and renders the credentials into the chart values. Without the
  Secret, the values get `enabled: false` and an empty `vcfOps`, and the chart
  installs nothing.
- `install/addonrepository.yaml`: the AddonRepository and
  AddonRepositoryInstall that register the chart as the addon
  `platform-observability`. A Supervisor administrator applies them once. The
  onboarder service does the same itself.
- `onboarder/`: a Supervisor Service controller that onboards every labelled
  cluster. It writes the credentials Secret and, by default, registers the
  addon and ships one AddonInstall.
- `scripts/onboard.sh`: prints a cluster's credentials Secret, built from the
  VCF Operations configurations API. It is the manual path for one cluster. See
  [Manual onboarding](manual-onboarding.md).
- `scripts/ops-account.sh`: creates the restricted VCF Operations role and user
  that the onboarder and `onboard.sh` log in with. See
  [VCF Operations API](vcf-operations-api.md#the-onboarding-account).
- `scripts/probe-ingest.sh`: sends fake OTLP metrics and logs with one
  cluster's OAuth client, to test the ingest path without a cluster.
- `examples/addoninstall.yaml`: attaches the addon to labelled clusters in a
  Supervisor namespace.

The chart is published as an OCI artifact on GitHub Packages by
`.github/workflows/release.yaml`:
`oci://ghcr.io/warroyo/platform-observability-addon/charts/platform-observability`.
The AddonRepository points at `oci://ghcr.io/warroyo/platform-observability-addon/charts`,
and the addon manager appends the chart name. The onboarder service registers
it as `platform-observability-oci-repo` and
`platform-observability-oci-repo-install`, in its own namespace.

The custom ACD needs VKS 3.8 or later. On 3.8 a helm AddonRepositoryInstall in
a Supervisor Service namespace also propagates to guests, which it did not
before. That is what lets the onboarder service register the addon itself.

## The credentials Secret

The Secret `platform-observability-<cluster>`, in the cluster's Supervisor
namespace, is the source of truth. It has these keys:

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

Who can read the OAuth secret: anyone who can read Secrets in the namespace.
The secret is in no other object.

## RBAC

The ClusterRole grants `secrets` `get`, `create`, `patch` and `delete`, and no
`list` or `watch`. The Supervisor refuses a cluster-wide Secret list or watch to
a third-party service account. So the onboarder reads Secrets by name with live
reads and never caches or watches them. A deleted credentials Secret is noticed
at the next resync (`sync_period`, 10 minutes by default). The onboarder then
onboards the cluster again, which issues a new secret.

Live Secret writes into tenant namespaces work (tested on VKS 3.8.0).

## Ownership

The onboarder never reads, writes or watches an AddonConfig, and its ClusterRole
has no rule for them. Argo CD, or anyone else, can own AddonConfigs with a
normal sync. Leave out `clusterName` and `vcfOps`: the chart's
AddonConfigDefinition takes both from the Secret and overwrites what an
AddonConfig sets. Other helm values, such as `aggregator.replicaCount`, pass
through to the chart.

## What the onboarder does per Cluster state

| The Cluster | What the onboarder does |
|---|---|
| Labelled, and the Secret has no secret | `POST configurations`: creates the cluster's client in Ops and issues it a secret. Writes the Secret, and adds the finalizer `platform-observability/offboard`. |
| Labelled, and a Secret someone else wrote (for example `scripts/onboard.sh`) has a `clientSecret` and a `clientId` | Takes it over with server-side apply, and fills in the endpoints from Ops if they are missing. No `POST`, so nothing rotates. |
| Labelled, and the Secret already has a secret | Writes nothing on later resyncs. |
| Annotated `platform-observability/rotate` (any value, empty included) | Issues a new secret and writes it to the Secret, together with the annotation `platform-observability/rotated-for: <value>` on the Secret. Then removes the Cluster's annotation, only if it still holds that value: a newer request set meanwhile is kept and answered next. If removing it fails, the retry sees the marker and only removes the annotation. Once the annotation is gone the marker is dropped, so the same value can ask again later. Don't edit the marker: without it, an unfinished request is answered a second time. |
| Its credentials have a `clientSecret` but no `clientId` | Nothing, and an `IncompleteCredentials` Warning event. It won't replace a secret it wasn't asked to; annotate the cluster to rotate. |
| Label removed, or the Cluster deleted | Deletes the Secret, then `DELETE configurations/<cluster>` (which also deletes the VSP client), then removes the finalizer. In that order, a cluster labelled again halfway through finds no secret and onboards again, instead of adopting one Ops has deleted. |
| Annotated `platform-observability/skip-offboard` | Offboards without calling Ops, for when Ops is gone for good and the finalizer would otherwise block deleting the cluster. |
| Same name as a cluster in another namespace that holds the name (carries the finalizer, labelled or still offboarding) | Nothing: no `POST`, no adoption, no finalizer, and a `DuplicateClusterName` Warning event. Ops keys clusters by name alone, so onboarding the second would rotate the first one's secret, and offboarding either would delete the client both use. |

It reports on the Cluster with events: `Onboarded`, `SecretRotated`,
`Offboarded`, `DuplicateClusterName`, `IncompleteCredentials`.

## Never issue a secret it doesn't have to

Issuing a secret revokes the old one at once, and the collectors only pick a new
one up on helm-controller's next interval (up to 10 minutes). So:

- Every decision that leads to a `POST` is made from a live read of the Cluster
  and the Secret, never from the cache. A stale cache would otherwise `POST`
  again right after onboarding.
- A resync makes no Ops call.
- Reconciles of the same cluster name never run at the same time.
- It reads the endpoints before the `POST`, so an Ops that can't serve them
  fails before a secret is issued. The exception is an Ops with no cluster
  onboarded yet, which lists endpoints only after the first `POST`.
- A secret issued by a `POST` is kept in memory, keyed by the Cluster's UID,
  until a live read shows it written. Every reconcile settles it first, before
  anything else, so a failed write, a write that succeeded but reported an
  error, or a rotate request removed in between never leads to a second `POST`,
  and a stale entry is never reused.
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
- The duplicate-name check lists Clusters with a `metadata.name` field selector,
  and only for a cluster that doesn't hold the name yet.
- Clusters are cached without their managed fields. Secrets are never cached or
  watched.
