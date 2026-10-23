#!/usr/bin/env bash
# Writes chart/addon-config-definition.yaml into the chart's Chart.yaml as the
# addons.kubernetes.vmware.com/addon-config-definition annotation (gzip, then
# base64), with metadata.name set to platform-observability.<chart version>.
# VKS 3.8 and later read the AddonConfigDefinition from there.
#
#   scripts/embed-acd.sh           update Chart.yaml
#   scripts/embed-acd.sh --check   fail if Chart.yaml is out of date (CI)
#
# gzip -n leaves out the name and mtime, so the same input always encodes the
# same, and --check can compare.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
chart=$root/chart/platform-observability/Chart.yaml
source=$root/chart/addon-config-definition.yaml
key=addons.kubernetes.vmware.com/addon-config-definition

name=$(yq '.name' "$chart")
version=$(yq '.version' "$chart")
encoded=$(yq ".metadata.name = \"$name.$version\" | ... comments=\"\"" "$source" | gzip -n -9 | base64 -w0)

if [[ ${1:-} == --check ]]; then
  current=$(yq ".annotations.\"$key\" // \"\"" "$chart")
  if [[ $current != "$encoded" ]]; then
    echo "$chart: the $key annotation is out of date; run scripts/embed-acd.sh" >&2
    exit 1
  fi
  exit 0
fi

# Plain text, not yq -i, which would reflow the folded description. The
# annotations block is the last thing in Chart.yaml, and holds only this key.
sed -i '/^annotations:$/,$d' "$chart"
printf 'annotations:\n  %s: %s\n' "$key" "$encoded" >> "$chart"
echo "embedded $name.$version into $chart"
