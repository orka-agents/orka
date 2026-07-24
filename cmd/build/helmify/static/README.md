# Orka Helm chart

This chart is generated from `cmd/build/helmify`; edit the generator inputs and
run `make manifests` rather than editing generated chart copies directly. It
packages all twelve canonical Orka CRDs under `crds/`.

## Fresh install

A normal install creates the CRDs before the templated release resources:

```bash
helm install orka charts/orka \
  --namespace orka-system \
  --create-namespace \
  --wait
```

CRDs are cluster-scoped and shared by every Orka release. Use `--skip-crds`
only when a designated platform or GitOps workflow already manages compatible
Orka CRDs for the cluster.

Controller Services, worker ServiceAccounts, and worker RBAC are scoped to the
Helm release name. Multiple Orka releases can therefore share a namespace while
continuing to use the same cluster-scoped CRDs.

## Upgrade

Helm installs files from `crds/` only during installation. It does not create or
update them during `helm upgrade`, including when upgrading from an older Orka
chart that installed no CRDs.

Apply the CRDs from the exact target chart before upgrading the controller:

```bash
set -euo pipefail

TARGET_CHART=/absolute/path/to/orka-<version>.tgz
TARGET_CONTEXT=replace-with-context

helm show crds "$TARGET_CHART" | \
  kubectl --context "$TARGET_CONTEXT" apply --server-side --force-conflicts -f -

helm upgrade orka "$TARGET_CHART" \
  --namespace orka-system \
  --kube-context "$TARGET_CONTEXT" \
  --wait
```

`--force-conflicts` transfers managed-field ownership from Helm to the designated
CRD lifecycle owner. Do not run competing CRD apply workflows for the same
cluster.

If another system owns the CRDs, perform the CRD-first step through that system,
wait for all twelve CRDs to become `Established`, and then upgrade Orka.

If a previous release was uninstalled, update its retained CRDs first and install
the replacement release with `--skip-crds`.

## Uninstall and deletion

`helm uninstall` removes release resources but retains Orka's CRDs and custom
resources. This is Helm's standard `crds/` behavior and is not controlled by a
chart value.

Deleting a CRD also deletes every custom resource stored under that kind. Delete
Orka CRDs only as an explicit cluster-wide data-destruction operation after the
resources have been removed or backed up.
