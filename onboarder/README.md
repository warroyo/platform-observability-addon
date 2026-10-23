# platform-observability-onboarder

[![onboarder](https://github.com/warroyo/platform-observability-addon/actions/workflows/onboarder.yaml/badge.svg)](https://github.com/warroyo/platform-observability-addon/actions/workflows/onboarder.yaml)

## Overview

A Supervisor Service that onboards every labelled VKS cluster to VCF Operations
Platform Observability, without anyone running `scripts/onboard.sh`. Label a
cluster `addons.kubernetes.vmware.com/platform-observability=enabled`, and the
onboarder gets the cluster's OAuth client from VCF Operations and writes it into
the Secret `platform-observability-<cluster>` in the cluster's Supervisor
namespace. It logs in to Ops with the restricted account from
`scripts/ops-account.sh` and needs no VSP credential. It requires VKS 3.8 or
later.

## How it works

- It watches Clusters with the label, across namespaces.
- For a cluster with no secret, it sends `POST configurations` to Ops, writes
  the Secret and adds the finalizer `platform-observability/offboard`.
- Its AddonInstall attaches the addon, and the chart's own
  AddonConfigDefinition reads the Secret. The onboarder never touches
  AddonConfigs.
- When the label is removed or the Cluster is deleted, it deletes the Secret and
  the cluster's client in Ops, then removes the finalizer.
- It issues a new secret only for a cluster that has none, or when the Cluster
  has the annotation `platform-observability/rotate`. A resync makes no Ops call.

See [Architecture](../docs/architecture.md) for the behaviour per Cluster state,
the Secret format, RBAC and scale.

## Quick start

1. Create the Ops account: `scripts/ops-account.sh create`.
2. In vCenter, Workload Management > Services > Add, upload
   `platform-observability-onboarder.yml` from a release (tag
   `onboarder-v<version>`), then install it on the Supervisor with `ops_url` and
   `ops_password` set. The service registers the addon itself. If the addon is
   already registered with `install/addonrepository.yaml`, delete that
   registration first or set `addon_repository: false`.
3. Label a cluster `addons.kubernetes.vmware.com/platform-observability=enabled`,
   plus `addon.addons.kubernetes.vmware.com/helm-controller=managed` if it does
   not have helm-controller.

All values are in [Configuration](../docs/configuration.md). To move to a newer
chart, see [Upgrading](../docs/upgrading.md).

## Repo structure

```text
main.go, reconciler.go, ops.go   the controller and the Ops client
*_test.go                        unit tests (a fake Ops server and a fake client)
config/                          service values and deploy manifests
test/                            install and verify scripts for a live Supervisor
Makefile                         test, build, release and dev targets
```

Docs: [development](../docs/development.md), [FAQ](../docs/faq.md),
[decisions](../docs/decisions.md), [VCF Operations API](../docs/vcf-operations-api.md).
