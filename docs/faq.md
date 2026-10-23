# FAQ

## Why doesn't a ClusterAddon upgrade by itself?

A ClusterAddon does not move to a newer AddonRelease on its own, even with no
`releaseFilter`. It stays on the release it was installed with, with
`UpgradePending=False`. Pin `addon_release`. See [Upgrading](upgrading.md).

## Why does a new secret take up to 10 minutes to reach the collectors?

helm-controller's HelmRelease for the addon has `interval: 10m`, and it does not
watch the values Secret it reads (`valuesFrom`). A new secret reaches the guest
Secret at once, but the release is only upgraded on the next interval. Tested on
a VKS 3.7 Supervisor on 2026-10-21: the guest values Secret updated within 3
seconds, and the aggregators rolled onto the new secret about 9 minutes later.
ACD output templates carry no metadata, so the values Secret can't get Flux's
`reconcile.fluxcd.io/watch: Enabled` label, and `helmOptions` exposes only
`targetNamespace`.

Issuing a secret revokes the old one at once, so the aggregators send with a
revoked secret until then. The exporters retry for 65 s and then drop data, so
the gap is lost data. This is why the onboarder never rotates on a timer. See
[Decisions](decisions.md#no-timed-rotation).

## What if someone onboards the cluster from the VCF Operations UI?

Don't onboard a managed cluster from the VCF Operations UI; it issues a new
secret. Recover with the rotate annotation: annotate the Cluster with
`platform-observability/rotate`. The sign is 401s in the aggregator logs.

## What happens with duplicate cluster names?

Ops keys clusters by name alone. Two clusters with the same name, in different
namespaces or on different Supervisors, would share a client and rotate each
other's secret. The onboarder doesn't onboard the second cluster in the same
Supervisor: it does nothing and sends a `DuplicateClusterName` Warning event.
Across Supervisors, nothing checks. See
[Architecture](architecture.md#what-the-onboarder-does-per-cluster-state).

## How do I delete a cluster when Ops is gone?

Offboarding runs behind the finalizer `platform-observability/offboard` and
calls Ops. If Ops is gone for good, the finalizer blocks deleting the cluster.
Annotate the Cluster `platform-observability/skip-offboard`. The onboarder then
offboards without calling Ops.

## Who can read the OAuth secret?

The OAuth client secret is in the Secret `platform-observability-<cluster>` in
the cluster's Supervisor namespace, readable by anyone who can read Secrets
there. It is in no other object.

## What restarts when something changes?

Only the aggregators roll on a new OAuth secret. Nothing restarts when the CA
certificates or endpoints change; restart the collectors then.

## What is the agent-to-aggregator token?

It is generated on first install and kept across upgrades (helm `lookup`). Set
`internalToken` to choose it.

## Why does the onboarder release fail with `permission_denied: write_package`?

The image or bundle package was first created by a push with a personal token,
so the workflow has no access to it. In the package's settings, go to "Manage
Actions access", add this repository, and give it the Write role. See
[Development](development.md#package-access-for-actions).

## Why does the service go to ERROR with `AddonRepository is in use by an AddonRepositoryInstall`?

The service renamed or replaced its AddonRepository. kapp deleted the
AddonRepository before its AddonRepositoryInstall, and the webhook refuses
that: `delete is not allowed`. In the service namespace, delete the old
AddonRepositoryInstall, then the old AddonRepository. kapp then reconciles. See
[Upgrading](upgrading.md#moving-the-chart-repository).

## Why did Ops answer 500 when a cluster was offboarded?

VCF Operations answers `DELETE configurations/<cluster>` with HTTP 500
("Downstream service returned error") for a cluster it doesn't list. Seen on
2026-10-23: the clusters' Ops entries and VSP clients were already gone, and
every retry got 500, so the finalizer never came off. Since onboarder 0.3.2, a
failed DELETE is checked against a fresh `GET configurations`, and a cluster
Ops no longer lists counts as offboarded.
