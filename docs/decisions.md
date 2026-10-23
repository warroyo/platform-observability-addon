# Decisions

## Helm addon instead of a Carvel package

Date: 2026-10-21.

Context: the collectors are the upstream `opentelemetry-collector` helm chart,
installed twice, and the addon has to be installable on VKS clusters by label.

Decision: ship the addon as a helm chart in a helm AddonRepository, published
as a chart (first to GitHub Pages, since [moved to OCI](#chart-on-oci-in-github-packages-not-a-github-pages-helm-repository)),
instead of a Carvel package repository.

Alternatives rejected: a Carvel package repository. A Carvel (imgpkg)
repository was the only way to ship a hand-written ACD before VKS 3.8, but it
means packaging, a bundle per release, and an AddonRepository that is frozen
once installed. The collectors are already helm charts, and a helm repository
is simpler to release. VKS 3.8 lets a helm chart carry its own
ACD, which removed the one reason to use Carvel.

## Credentials in a Secret read by a custom ACD, not in an AddonConfig

Date: 2026-10-23 (chart 0.2.0, VKS 3.8).

Context: on 2026-10-21 a helm AddonRepository got a generated ACD (`helmValues`
and `helmOptions`, no `templateInputResources`), so per-cluster values, the
secret included, could only come in through the AddonConfig, in plain text,
readable by anyone with edit in the namespace. VKS 3.8 reads an ACD from the
`addons.kubernetes.vmware.com/addon-config-definition` annotation in
`Chart.yaml`.

Decision: ship our own ACD. It reads the Secret
`platform-observability-<cluster>` as an optional `templateInputResource` and
renders the values from it, with `enabled: false` while the Secret does not
exist. The onboarder writes only the Secret. The telegraf addon uses the same
pattern: a Supervisor-side component writes per-cluster Secrets that the ACD
reads. Tested on VKS 3.8.0 with `dev1-cluster` and `goad-test-01a`.

Alternatives rejected: keep writing credentials into the AddonConfig. The
onboarder and Argo CD would share ownership of one object (server-side apply
splits ownership inside `spec.values`, so the `vcfOps` keys had to stay out of
Git), a prune in Argo CD would remove the credentials and force a new secret,
and the secret sat in an object tenants can edit.

## A restricted Ops account, and no VSP credential in automation

Date: 2026-10-22.

Context: `POST configurations` needs some Ops role. No built-in role works, not
even Administrator.

Decision: a custom role with only `extensions.management.usage`, and a local
user `platform-observability-onboard` that holds only that role
(`scripts/ops-account.sh`). With that alone, `GET`, `POST` and `DELETE
configurations/<cluster>` work. The onboarder holds no VSP credential. The
`lastSecretRotationDate` comparison is dropped from the automation. The
onboarder issues a new secret only when its stored credentials are missing, or
when someone annotates the Cluster. `onboard.sh --check` stays as a manual check
for a person, as `admin@vsp.local`.

Alternatives rejected: the Ops `admin` user (too much privilege), and an
automated drift check with a VSP admin credential.

## One AddonInstall with crossNamespaceSelection Allowed

Date: 2026-10-22.

Context: an AddonInstall per namespace means the onboarder has to create one in
every namespace with a labelled cluster. Telegraf ships one:
`vcfops-telegraf-addoninstall` in `vmware-system-vks-public` with
`crossNamespaceSelection: Allowed`, selecting
`addons.kubernetes.vmware.com/automated-monitoring=enabled`.

Decision: the onboarder service ships one AddonInstall with
`crossNamespaceSelection: Allowed`. The cluster label is the only switch per
cluster.

Alternatives rejected: one AddonInstall per namespace.

## Finalizer-based offboarding

Date: 2026-10-22.

Context: removing a cluster from Ops needs a `DELETE configurations/<cluster>`
call, which also deletes the VSP client. It has to happen before the cluster is
gone, including when only the label is removed.

Decision: offboarding runs behind the finalizer `platform-observability/offboard`
on the Cluster. The onboarder deletes the Secret, then calls the `DELETE`, then
removes the finalizer. The annotation `platform-observability/skip-offboard`
skips the Ops call when Ops is gone.

Alternatives rejected: an interim CronJob around `onboard.sh`. It works on a
schedule and can't clean up before a delete. A controller reacts to labels at
once and supports the finalizer.

## No timed rotation

Date: 2026-10-21.

Context: helm-controller's HelmRelease for the addon has `interval: 10m` and
doesn't watch the values Secret. A new secret reaches the aggregators up to 10
minutes after it is issued, and issuing it revokes the old one at once.

Decision: never rotate on a timer. The onboarder issues a new secret only when
the cluster has none, or when someone asks with the rotate annotation. A
scheduled rotation would cost up to 10 minutes of data per cluster each time.

Alternatives rejected: scheduled rotation. Shortening the interval has no
option that stays out of the guest today.

## Remove legacy AddonConfig credentials support in 0.3.0

Date: 2026-10-23.

Context: chart 0.2.0 and the onboarder 0.2.0 supported both the Secret and
credentials in an AddonConfig (`legacy_addon_config`, for clusters still on
0.1.0). The upgrade from 0.1.0 to 0.2.0 was run on 2026-10-23 with no new
secret issued.

Decision: chart 0.3.0 and the matching onboarder version read credentials from
the Secret only. They no longer support credentials in an AddonConfig.

Alternatives rejected: keeping the legacy mode. Every cluster had moved, and
the mode kept the OAuth secret readable and editable in AddonConfigs, kept
AddonConfig RBAC and code paths in the onboarder, and made the chart accept two
sources of credentials. Dropping it leaves one source, the Secret.

## Chart on OCI in GitHub Packages, not a GitHub Pages helm repository

Date: 2026-10-23.

Context: the chart was published to GitHub Pages with chart-releaser, and the
AddonRepository fetched it over `https://`. VKS 3.8 helm AddonRepositories
also accept `oci://`, and the builtin VKS 3.8 helm repository uses one. The
onboarder image and bundle already lived on ghcr.io.

Decision: publish the chart with `helm push` to
`oci://ghcr.io/warroyo/platform-observability-addon/charts`, and register the
AddonRepository with that URL. The package is public, so pulls are anonymous.
GitHub Pages is no longer used. The release workflow skips a version that is
already published, and creates a GitHub release
`platform-observability-<version>` for the README badge.

Alternatives rejected: GitHub Pages with chart-releaser. It needs a `gh-pages`
branch and an `index.yaml` that the release job rewrites, and the chart lived
apart from the onboarder image and bundle on ghcr.io. With OCI, all three
artifacts are packages of this repo, released by one token (`GITHUB_TOKEN`).

Moving an installed registration needs new object names. See
[Upgrading](upgrading.md#moving-the-chart-repository).
