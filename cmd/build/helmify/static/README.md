# Orka Helm chart

This chart is generated from `cmd/build/helmify`; edit the generator inputs and
run `make manifests` rather than editing generated chart copies directly. It
packages all twenty canonical Orka CRDs under `crds/`.

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
Helm release name. Run only one Orka controller release per namespace. If a
cluster has multiple releases, every release (including the first) must use a
cluster-unique release name or `fullnameOverride`, a separate controller
namespace, and a distinct, non-empty `controller.watchNamespace`. Do not mix a
cluster-wide watcher with namespace-scoped releases: gateway admission policies
would overlap. All releases share the same cluster-scoped CRDs.

## Remote memory control plane

`controller.memoryBackend.enabled` is disabled by default. Enabling it requires a
stable `controller.memoryBackend.clusterId` and durable controller storage. On an
upgrade, apply the exact target `MemoryBackend` CRD first; Helm does not upgrade
files under `crds/`. The controller stays disabled until the CRD schema marker is
observed unless `crdsReadyOverride=true` is used after independent verification.

`controller.memoryBackend.activationEnabled` is the separate second-release
cutover gate. The foundation chart and controller artifacts reject activation
even when this value or the matching runtime environment variable is forced.
The foundation controller advertises feature epoch 1; only a later source-gated
activation artifact may advertise epoch 2 and accept the activation value.
While the durable authority remains SQLite, enabling MemoryBackend support also
requires `controller.replicas: 1`. The controller Deployment uses `Recreate`, so
the foundation replica stops before the activation artifact starts; activation
also requires durable evidence that a lower feature epoch was previously
observed and that every live heartbeat supports the activation epoch.
Creating `MemoryBackend/default` in `Staged` validates without changing SQLite
authority. Activation, decommission, force-orphan, and restore-legacy remain
explicit audited API/CLI actions. Dispatcher concurrency, sustained rate, and
burst controls are configured under `controller.memoryBackend.dispatcher*`; the
defaults bound both global and per-namespace work.

The chart always installs fail-closed admission policies that reserve Orka task
Job/Pod provenance for the owning controllers and require
`memorybackends/finalizers` update authorization before the backend protection
finalizer can be removed. Do not disable these policies or grant their protected
status/finalizer permissions to untrusted namespace writers.

Helm uses release-scoped pre-install/pre-upgrade RBAC and provenance policy hooks
before rolling the controller Deployment, then installs the retained
steady-state policies. This closes the Helm kind-ordering gap while allowing the
old controller and Kubernetes Job controller to create legacy-format work during
the upgrade. Helm removes the release-scoped preflight RBAC after each successful
hook run. If a later hook aborts the release, pre-delete hooks replace any
leftover grants with inert, subject-free/rule-free tombstones during uninstall;
Helm versions may then remove those tombstones after the cleanup hook completes.
Intentionally retained steady-state controls remain in place. The raw installer
likewise places the task provenance policies and bindings after their RBAC
grants but before either Deployment.

The chart-created controller store PVC is annotated `helm.sh/resource-policy:
keep`, and `store.persistence.existingClaim` may select an operator-managed PVC.
The MemoryBackend finalizer policy and binding are also retained on uninstall so
surviving `MemoryBackend` objects cannot have their lifecycle barrier stripped.
Delete retained resources manually only after every backend is safely
decommissioned or force-orphaned and a matched recovery set is verified.

Example foundation values:

```yaml
controller:
  memoryBackend:
    enabled: true
    activationEnabled: false
    clusterId: production-cluster-a
    crdsReadyOverride: true # only after separately applying/verifying target CRDs
store:
  persistence:
    enabled: true
```

## Out-of-tree OMS adapters

Orka ships the provider-neutral `orka.oms.v0alpha1` protocol and conformance
harness, but provider adapters are maintained and deployed independently. The
KD6 adapter, its Helm chart, image lifecycle, and live provider release gate
live in [`orka-agents/orka-oms-kd6-adapter`](https://github.com/orka-agents/orka-oms-kd6-adapter).

The Orka chart never creates an adapter or a `MemoryBackend`. Install the chosen
adapter separately, expose it through a public HTTPS endpoint, bind an
operator-managed client-auth Secret, and stage/activate the backend explicitly.

## Upgrade

Helm installs files from `crds/` only during installation. It does not create or
update them during `helm upgrade`, including when upgrading from an older Orka
chart that installed no CRDs.

Apply the exact CRD specs from the target chart before upgrading the
controller. The first apply creates missing CRDs and transfers ownership of
present fields; the guarded JSON Patch then replaces each `spec` so fields
removed by the target version do not remain from an older Helm manager:

```bash
set -euo pipefail

TARGET_CHART=/absolute/path/to/orka-<version>.tgz
TARGET_CONTEXT=replace-with-context
TARGET_CRDS="$(mktemp)"
trap 'rm -f "$TARGET_CRDS"' EXIT

helm show crds "$TARGET_CHART" > "$TARGET_CRDS"
kubectl --context "$TARGET_CONTEXT" apply \
  --server-side \
  --force-conflicts \
  --field-manager=orka-crd-lifecycle \
  -f "$TARGET_CRDS"

kubectl --context "$TARGET_CONTEXT" create --dry-run=client -f "$TARGET_CRDS" -o json | \
  jq -c '{name: .metadata.name, spec: .spec}' | \
  while IFS= read -r target; do
    name="$(jq -er '.name' <<< "$target")"
    spec="$(jq -ec '.spec' <<< "$target")"
    resource_version="$(kubectl --context "$TARGET_CONTEXT" get crd "$name" -o jsonpath='{.metadata.resourceVersion}')"
    patch="$(jq -cn --arg rv "$resource_version" --argjson spec "$spec" \
      '[{"op":"test","path":"/metadata/resourceVersion","value":$rv},{"op":"replace","path":"/spec","value":$spec}]')"
    kubectl --context "$TARGET_CONTEXT" patch crd "$name" --type=json -p "$patch"
    kubectl --context "$TARGET_CONTEXT" wait --for=condition=Established --timeout=60s "crd/$name"
  done

helm upgrade orka "$TARGET_CHART" \
  --namespace orka-system \
  --kube-context "$TARGET_CONTEXT" \
  --wait
```

A matching Orka source checkout provides the same guarded flow as
`scripts/apply-helm-crds.sh "$TARGET_CHART" "$TARGET_CONTEXT"`. Do not run
competing CRD apply workflows for the same cluster.

If another system owns the CRDs, perform the CRD-first step through that system,
wait for all twenty CRDs to become `Established`, and then upgrade Orka.

If a previous release was uninstalled, update its retained CRDs first and install
the replacement release with `--skip-crds`.

## Uninstall and deletion

`helm uninstall` removes release resources but retains Orka's CRDs and custom
resources. This is Helm's standard `crds/` behavior and is not controlled by a
chart value.

Deleting a CRD also deletes every custom resource stored under that kind. Delete
Orka CRDs only as an explicit cluster-wide data-destruction operation after the
resources have been removed or backed up.
