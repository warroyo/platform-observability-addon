# Upgrading

## Upgrading the chart

1. Add the new version to `addon_versions`, and keep the old one while clusters
   still use it.
2. Set `addon_release` to the new release, e.g. `platform-observability.0.3.0`.
   A ClusterAddon does not move to a newer AddonRelease by itself, even with no
   `releaseFilter`: it stays on the release it was installed with and reports
   `UpgradePending` False.

The clusters' Secrets are unchanged, so no secret rotates.

`onboarder/test/install-supervisor-service.sh` keeps the installed values on an
upgrade. Change one with `VALUES_PATCH`, a yq expression:

```sh
VALUES_PATCH='.addon_release = "platform-observability.0.3.0"' make install-supervisor-service
```

Run it from `onboarder/`. See [Development](development.md).

## Upgrading the service

Services installed before 0.2.0 can't be upgraded until the service carries the
kapp rebase rule in `onboarder/config/deploy.yml`. The addon manager adds its
own labels and annotations to the AddonRepositoryInstall, kapp removed them on
upgrade, and the webhook refused that: `only addon manager service account is
allowed to modify VKS-managed labels/annotations`.

## History

The move from chart 0.1.0 to 0.2.0, which took the credentials from the
AddonConfig to the Secret, was done on 2026-10-23. Chart 0.3.0 and the matching
onboarder version no longer support credentials in an AddonConfig. See
[Decisions](decisions.md).
