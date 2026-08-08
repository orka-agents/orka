# Orka Helm chart

This chart is generated from `cmd/build/helmify`; edit the generator inputs and
run `make manifests` rather than editing generated chart copies directly. It
packages all 26 canonical Orka CRDs under `crds/`.

## Fresh install

A normal install creates the CRDs before the templated release resources:

```bash
kubectl create namespace orka-system
kubectl label namespace orka-system orka.ai/controller-mode=harness-v2

helm install orka charts/orka \
  --namespace orka-system \
  --set controller.mode=harness-v2 \
  --set controller.watchNamespace=orka-system \
  --wait
```

The provider proxy is disabled by default. Before enabling it for a
`harness-v2` release, install Vekil in `vekil-system`; the chart then installs
the exact cross-namespace ingress policy there. The chart-managed provider
proxy itself always runs in the Helm release namespace. Leave
`controller.acpRuntime.providerProxyNamespace` empty or set it to that release
namespace. The only supported upstream is
`http://vekil.vekil-system.svc:1337` (an optional trailing slash is normalized);
alternate hosts, namespaces, and ports are rejected because the chart does not
create matching NetworkPolicies.

`service.port` is the controller Service port used by controller and Publisher Service URLs. `controller.apiPort` is only the controller container listener and Service target port.

### Coordinated authentication Secret rotation

The Publisher and SCM egress proxy read their authentication material at process startup. Rotate each Secret and its non-secret rollout marker in the same Helm upgrade:

- When rotating `publisher.auth.existingSecret` (or the chart-managed publisher auth values), bump `publisher.auth.rolloutNonce`. The marker is added only to the controller and Publisher Pod templates so both restart onto the same credential generation.
- When rotating `scmEgressProxy.auth.existingSecret` (or the chart-managed SCM proxy token), bump `scmEgressProxy.auth.rolloutNonce`. The marker is added only to the Publisher and SCM proxy Pod templates.

The nonce is a revision label, not a credential. Never put Secret content in it. A coordinated upgrade may briefly fail closed while Pods roll, but it avoids an indefinite split generation.

The harness-v1 wrapper likewise keeps execution authority and transport
material separate. `harnessV1.auth.existingSecret` contains only the bearer
token and is immutable while v1 work exists. `harnessV1.tls.existingSecret`
contains `tls.crt`, `tls.key`, and `ca.crt`. A TLS Secret name change is a
wrapper Pod-template change and automatically uses the existing drained
rollover. For same-name certificate renewal, update the TLS Secret and bump
`harnessV1.tls.rolloutNonce` in the Helm upgrade; the hook drains the live
wrapper before both wrapper and controller restart. Keep the updated `ca.crt`
able to verify the certificate currently being served during that drain, or
rotate to a versioned TLS Secret so the hook can mount the prior CA.

CRDs are cluster-scoped and shared by every Orka release. Use `--skip-crds`
only when a designated platform or GitOps workflow already manages compatible
Orka CRDs for the cluster.

## Static harness mode

Every release selects exactly one controller mode: `harness-v1` or
`harness-v2`. `dual`, `auto`, and `harness-v1-drain` are rejected. Each release
also requires a distinct, non-empty `controller.watchNamespace` labeled with
the matching mode:

```bash
kubectl create namespace orka-v2-system
kubectl label namespace orka-v2-system orka.ai/controller-mode=harness-v2

helm install orka-v2 charts/orka \
  --namespace orka-v2-system \
  --set controller.mode=harness-v2 \
  --set controller.watchNamespace=orka-v2-system
```

The mode is an installation identity, not an upgrade toggle. Never change a
release from v1 to v2 in place or reuse its PVC, SQLite store, ledger, Session,
or Task identities under the other mode.

A v1 and v2 release may share a cluster only when their release/watch
namespaces, Services, ServiceAccounts/RBAC, Leases, stores, Secrets, and
data-plane resources are disjoint. The chart intentionally requires
`controller.watchNamespace` to equal the Helm release namespace. The v2
release must also have its own runtime namespace. Install the shared compatible
CRDs and common admission resources through one designated owner; install the
second release with `--skip-crds`.

Controller Services, worker ServiceAccounts, and worker RBAC are scoped to the
Helm release name. Run only one Orka controller release per namespace. If a
cluster has multiple releases, every release (including the first) must use a
cluster-unique release name or `fullnameOverride`, a separate controller
namespace, and a distinct, non-empty `controller.watchNamespace`. Cluster-wide
watchers are rejected. All releases share the same cluster-scoped CRDs, and
cluster-scoped gateway/workspace ownership belongs only to the v2 release.

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
wait for all 26 CRDs to become `Established`, and then upgrade Orka.

If a previous release was uninstalled, update its retained CRDs first and install
the replacement release with `--skip-crds`.

## Uninstall and deletion

`helm uninstall` removes release resources but retains Orka's CRDs and custom
resources. This is Helm's standard `crds/` behavior and is not controlled by a
chart value.

Deleting a CRD also deletes every custom resource stored under that kind. Delete
Orka CRDs only as an explicit cluster-wide data-destruction operation after the
resources have been removed or backed up.
