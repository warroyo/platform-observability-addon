# Development

## Onboarder

Run these in `onboarder/`.

```sh
make test                      # unit tests: a fake Ops server and a fake client with server-side apply
OPS_URL=https://ops.example.com OPS_PASSWORD=... \
  go run . --leader-elect=false --kubeconfig ~/.kube/config
make release VERSION=0.1.0     # CI does this for a tag onboarder-v0.1.0
```

Running it locally uses your own kubeconfig identity, not the service's
ClusterRole, so it does not test the RBAC in `config/deploy.yml`.

## Make targets

| Target | Does |
|---|---|
| `make test` | `go vet` and `go test -race`. All CI runs. |
| `make build VERSION=<v>` | Builds the image. |
| `make push VERSION=<v>` | Pushes the image. |
| `make release VERSION=<v>` | Packages an image already pushed for `VERSION` into `platform-observability-onboarder.yml`. The image is pinned by digest. |
| `make dev-release` | Builds, pushes and packages `DEV_VERSION`. |
| `make install-supervisor-service` | Registers and installs or upgrades the service on a Supervisor. |
| `make verify-supervisor` | Asserts the installed service against the live Supervisor. |

## Pre-release gate

Run by hand before a tag, with `.env` filled in (see `onboarder/.env.example`).
None of it runs in CI: it pushes to the registry and needs a vCenter, a
Supervisor and VCF Operations.

```sh
make dev-release DEV_VERSION=0.1.0-rc1
make install-supervisor-service
make verify-supervisor
```

A dev release is an ordinary release under a pre-release version, so it
exercises the real build, digest pinning and packaging. `verify-supervisor`
checks the digest actually in the registry for this version, which catches a
stale install. vCenter reporting CONFIGURED does not mean the new pod is
running.

The Supervisor's RBAC refuses even a vSphere administrator ClusterRoles, so the
service lands only through vCenter's service-install API.

## Releases

- Onboarder: a tag `onboarder-v<version>` makes `.github/workflows/onboarder.yaml`
  build and push the image and the bundle, and attach
  `platform-observability-onboarder.yml` to a GitHub release.
- Chart: a push to `main` that bumps the version in
  `chart/platform-observability/Chart.yaml` makes
  `.github/workflows/release.yaml` publish it to GitHub Pages with
  chart-releaser. That also creates a GitHub release tagged
  `platform-observability-<version>`. A version already released is skipped,
  never overwritten.

## Embedding the ACD

`scripts/embed-acd.sh` writes `chart/addon-config-definition.yaml` into
`Chart.yaml` (gzip, then base64) and sets `metadata.name` to
`platform-observability.<version>`. Run it again after editing the ACD or
bumping the chart version. `scripts/embed-acd.sh --check` fails if `Chart.yaml`
is out of date. The release workflow runs it.
