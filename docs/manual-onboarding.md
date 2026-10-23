# Manual onboarding

For a single cluster you can skip the onboarder and use `scripts/onboard.sh`.

1. Register the addon, once per Supervisor:

   ```sh
   kubectl apply -f install/addonrepository.yaml
   ```

2. Create the onboarding account, once per Ops instance. See
   [VCF Operations API](vcf-operations-api.md#the-onboarding-account).

3. Attach the addon in the cluster's Supervisor namespace (edit the namespace
   in the file first):

   ```sh
   kubectl apply -f examples/addoninstall.yaml
   ```

4. Write the cluster's credentials Secret and label the cluster:

   ```sh
   VCF_OPS_URL=https://ops.example.com \
   VCF_OPS_USERNAME=platform-observability-onboard \
   VCF_OPS_PASSWORD=$(cat ~/.config/vcf-ops/onboard.password) \
     scripts/onboard.sh lab1-vks-01 my-namespace | kubectl apply -f -
   kubectl -n my-namespace label cluster lab1-vks-01 \
     addons.kubernetes.vmware.com/platform-observability=enabled \
     addon.addons.kubernetes.vmware.com/helm-controller=managed
   ```

`onboard.sh` prints the Secret `platform-observability-<cluster>`. The 0.3.0 ACD
reads the credentials from that Secret only. The onboarder can take the Secret
over later without issuing a new secret.

Running `onboard.sh` again issues a new secret and revokes the old one. To check
whether the secret was rotated elsewhere, use `--check`. See
[VCF Operations API](vcf-operations-api.md#checking-for-drift-with---check).
