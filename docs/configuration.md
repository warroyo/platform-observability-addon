# Configuration

Values of the onboarder Supervisor Service. Set them when you install the
service in vCenter (Workload Management > Services).

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
| `addon_install` | `true` | Ship one AddonInstall with `crossNamespaceSelection: Allowed`. |
| `addon_release` | | Pin the AddonInstall to one AddonRelease, e.g. `platform-observability.0.3.1`. A ClusterAddon does not move to a newer release by itself, so set this to upgrade. |
| `addon_repository` | `true` | Register the addon's helm repository. |
| `addon_chart_repository` | `https://warroyo.github.io/platform-observability-addon` | |
| `addon_versions` | `["0.3.1"]` | Chart versions to offer, one AddonRelease each. The only repository setting an upgrade can change. |

The service registers the addon itself (an AddonRepository and its
AddonRepositoryInstall). If the addon is already registered with
`install/addonrepository.yaml`, either delete that registration first or install
with `addon_repository: false`: two would both create the same Addon.

To change a value on an installed service, see [Upgrading](upgrading.md).
