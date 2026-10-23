# Upgrading

## Upgrading the chart

1. Add the new version to `addon_versions`, and keep the old one while clusters
   still use it.
2. Set `addon_release` to the new release, e.g. `platform-observability.0.3.1`.
   A ClusterAddon does not move to a newer AddonRelease by itself, even with no
   `releaseFilter`: it stays on the release it was installed with and reports
   `UpgradePending` False.

The clusters' Secrets are unchanged, so no secret rotates.

`onboarder/test/install-supervisor-service.sh` keeps the installed values on an
upgrade. Change one with `VALUES_PATCH`, a yq expression:

```sh
VALUES_PATCH='.addon_release = "platform-observability.0.3.1"' make install-supervisor-service
```

Run it from `onboarder/`. See [Development](development.md).

## Upgrading the service

Services installed before 0.2.0 can't be upgraded until the service carries the
kapp rebase rule in `onboarder/config/deploy.yml`. The addon manager adds its
own labels and annotations to the AddonRepositoryInstall, kapp removed them on
upgrade, and the webhook refused that: `only addon manager service account is
allowed to modify VKS-managed labels/annotations`.

## Moving the chart repository

On 2026-10-23 the chart moved from GitHub Pages to
`oci://ghcr.io/warroyo/platform-observability-addon/charts`. An installed
AddonRepository's URL can't change, so the service registers new objects,
`platform-observability-oci-repo` and `platform-observability-oci-repo-install`.
They replace `platform-observability-repo` and `-repo-install`.

- The `package-offerings` repositoryVersion is 2.0.0, not 1.0.0. The
  AddonRepositoryInstall webhook refuses a registration whose offerings equal an
  existing one's, as a duplicate.
- On the upgrade, kapp deleted the old AddonRepository before its
  AddonRepositoryInstall. The webhook refused with `AddonRepository is in use by
  an AddonRepositoryInstall, delete is not allowed`, and the service went to
  ERROR.
- Recovery: in the service namespace, delete the old AddonRepositoryInstall,
  then the old AddonRepository. kapp then reconciled.
- The new pair carries kapp `change-group` and `change-rule` annotations, so a
  future rename deletes the install first. kapp reads them from the objects it
  deletes, so they only help pairs created with them.

During the swap the clusters stayed Ready and moved to 0.3.1 from the OCI
repository. The HelmRelease shows `platform-observability@0.3.1+<digest>`,
because helm-controller appends the OCI digest. The credentials Secrets were
unchanged, the aggregators did not restart, and there were no errors.

The upgrade command:

```sh
VALUES_PATCH='.addon_chart_repository = "oci://ghcr.io/warroyo/platform-observability-addon/charts" | .addon_versions = ["0.3.1"] | .addon_release = "platform-observability.0.3.1"' make install-supervisor-service
```

If you registered the addon by hand, apply the new `install/addonrepository.yaml`
and delete the old pair, the install first.

## History

The move from chart 0.1.0 to 0.2.0, which took the credentials from the
AddonConfig to the Secret, was done on 2026-10-23. Chart 0.3.0 and the matching
onboarder version no longer support credentials in an AddonConfig. See
[Decisions](decisions.md).
