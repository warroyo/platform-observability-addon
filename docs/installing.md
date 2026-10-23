# Installing

How to install, verify and uninstall the onboarder Supervisor Service. The
examples used onboarder 0.3.2, chart 0.3.1 and VKS 3.8, tested on 2026-10-23.

## Install from the vCenter UI

1. Go to Workload Management > Services > Add and upload
   `platform-observability-onboarder.yml`.
2. Install the service on the Supervisor with its values. `ops_url` and
   `ops_password` are required. See [Configuration](configuration.md).

## Install through the vCenter API

The REST calls do the same as the UI. Base64 here means the file content
encoded on one line, for example `base64 -w0 file`.

1. Log in. `POST https://<vcenter>/api/session` with basic auth returns a
   session ID. Send it as the header `vmware-api-session-id` on every call.

2. Register the service in the catalog.
   `POST /api/vcenter/namespace-management/supervisor-services` answers 201:

   ```json
   {"spec_type":"CARVEL","carvel_spec":{"version_spec":{"content":"<base64 of platform-observability-onboarder.yml>"}}}
   ```

   To add a version to a service that is already registered, use
   `POST /api/vcenter/namespace-management/supervisor-services/<service>/versions`:

   ```json
   {"carvel_spec":{"content":"<base64>"}}
   ```

3. Install it on the Supervisor.
   `POST /api/vcenter/namespace-management/clusters/<supervisor cluster id>/supervisor-services`
   answers 204. The cluster ID looks like `domain-c9`.

   ```json
   {"supervisor_service":"platform-observability-onboarder.field.vmware.com","version":"0.3.2","yaml_service_config":"<base64 of the values YAML>"}
   ```

   The values need at least `ops_url` and `ops_password`, plus
   `ops_insecure: true` for a self-signed lab Ops. Quote the password as a YAML
   string.

   ```yaml
   ops_url: https://ops.example.com
   ops_password: "..."
   ops_insecure: true
   ```

4. Poll
   `GET /api/vcenter/namespace-management/clusters/<id>/supervisor-services/<service>`
   until `config_status` is `CONFIGURED` and `current_version` matches. This
   took 63 s. `CONFIGURED` tracks the PackageInstall, so check that the
   Deployment rolled.

### Upgrade

`PUT` the same install path with `{"version": ..., "yaml_service_config": ...}`.

`onboarder/test/install-supervisor-service.sh` does register, install and
upgrade, and keeps the installed values (`VALUES_PATCH`). See
[Development](development.md).

## Verifying an install

Label a cluster, then check these in order. Expected timing from the label:
the Secret in seconds, the ClusterAddon Ready in about 40 s, the collectors
Running in about 45 s.

1. On the Supervisor, in the cluster's namespace:

   ```sh
   kubectl -n <ns> get secret platform-observability-<cluster>
   kubectl -n <ns> get clusteraddon <cluster>-platform-observability
   ```

2. Get the guest kubeconfig from the Secret `<cluster>-kubeconfig`, key
   `value`, in the cluster's namespace:

   ```sh
   kubectl -n <ns> get secret <cluster>-kubeconfig -o jsonpath='{.data.value}' | base64 -d > guest.kubeconfig
   ```

3. In the guest, the collectors run: one agent per node plus 2 aggregators.

   ```sh
   kubectl --kubeconfig guest.kubeconfig -n vcf-monitoring get pods
   ```

4. In the guest, the HelmRelease in `vmware-system-helm` is Ready and shows the
   chart as `platform-observability@<version>+<digest>`.

   ```sh
   kubectl --kubeconfig guest.kubeconfig -n vmware-system-helm get helmrelease
   ```

5. The aggregator logs have none of `401`, `unauthorized`, `Exporting failed`
   or `dropping`.

6. Ops lists the cluster. See
   [the example in VCF Operations API](vcf-operations-api.md#example-list-onboarded-clusters).

For problems, see the [FAQ](faq.md).

## Uninstall

Do the steps in this order.

1. Remove the label `addons.kubernetes.vmware.com/platform-observability` from
   every cluster, and wait until the finalizer `platform-observability/offboard`
   is gone. This offboards each cluster: the onboarder deletes its Secret and
   runs `DELETE configurations/<cluster>` in Ops, which also deletes the VSP
   client. The AddonInstall stops matching, so the collectors and
   `vcf-monitoring` are removed from the guest.

   Do not uninstall the service first. The finalizer would stay on the
   Clusters with nothing to remove it, and that blocks deleting them.

2. Uninstall the service from the Supervisor.
   `DELETE /api/vcenter/namespace-management/clusters/<id>/supervisor-services/<service>`
   answers 204. Poll the GET until it answers 404. The service namespace, the
   AddonRepository, the AddonRepositoryInstall and the AddonInstall go with it,
   and the manager removes the Addon, AddonRelease and ACD.

3. Remove the service from the catalog.
   1. Deactivate it:
      `PATCH /api/vcenter/namespace-management/supervisor-services/<service>?action=deactivate`
      answers 204. A `POST ...?action=deactivate` answers 404. A PATCH with the
      body `{"state":"DEACTIVATED"}` answers 204 but changes nothing.
   2. `DELETE .../supervisor-services/<service>/versions/<version>` for each
      version. Each answers 204.
   3. `DELETE .../supervisor-services/<service>` answers 204. On an active
      service it answers 400 "because it is active".

Known leftover: the manager's Addon
`helm-repo-svc-platform-observability-onboarder-<id>` stays in
`vmware-system-vks-public` after the service namespace is deleted. The manager
owns it, so leave it. On reinstall, the service namespace got the same name and
reused it.
