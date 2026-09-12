# Orka Helm chart

This chart is generated from `cmd/build/helmify`; edit the generator inputs and
run `make manifests` rather than editing generated chart copies directly. It
packages all 26 production Orka CRDs under `crds/`. The development-only
`fake.workspace.orka.ai` CRDs are available separately from a matching source
checkout at `config/development/fake-workspace-provider`.

## Fresh install

A normal `harness-v2` install requires Vekil to be running in `vekil-system`,
immutable controller and Publisher image digests, and two operator-managed
Secrets. Prepare:

- a snapshot key file containing exactly 32 random bytes;
- a webhook serving certificate and private key whose certificate is valid for
  `orka-webhook.orka-system.svc`, plus its PEM CA certificate; and
- `CONTROLLER_DIGEST` and `PUBLISHER_DIGEST` values in
  `sha256:<64 lowercase hexadecimal characters>` form.

The following creates the namespace and required Secrets without putting key
material in Helm values or command-line arguments, then installs the CRDs and
release resources. Replace the file paths and digest placeholders first:

```bash
set -euo pipefail

: "${SNAPSHOT_KEY_FILE:?set SNAPSHOT_KEY_FILE to the 32-byte key file}"
: "${WEBHOOK_CERT_FILE:?set WEBHOOK_CERT_FILE to the serving certificate}"
: "${WEBHOOK_PRIVATE_KEY_FILE:?set WEBHOOK_PRIVATE_KEY_FILE to the private key}"
: "${WEBHOOK_CA_FILE:?set WEBHOOK_CA_FILE to the CA certificate}"
: "${CONTROLLER_DIGEST:?set CONTROLLER_DIGEST to sha256:<64 lowercase hex>}"
: "${PUBLISHER_DIGEST:?set PUBLISHER_DIGEST to sha256:<64 lowercase hex>}"

kubectl create -f - <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: orka-system
  labels:
    orka.ai/controller-mode: harness-v2
EOF
kubectl -n orka-system create secret generic agent-execution-snapshot-key \
  --from-file=snapshot-key="${SNAPSHOT_KEY_FILE}"
kubectl -n orka-system create secret generic orka-webhook-tls \
  --type=kubernetes.io/tls \
  --from-file=tls.crt="${WEBHOOK_CERT_FILE}" \
  --from-file=tls.key="${WEBHOOK_PRIVATE_KEY_FILE}" \
  --from-file=ca.crt="${WEBHOOK_CA_FILE}"

WEBHOOK_CA_BUNDLE="$(kubectl -n orka-system get secret orka-webhook-tls \
  -o jsonpath='{.data.ca\.crt}')"

helm install orka manifest_staging/charts/orka \
  --namespace orka-system \
  --set controller.mode=harness-v2 \
  --set controller.watchNamespace=orka-system \
  --set-string controller.image.digest="${CONTROLLER_DIGEST}" \
  --set-string publisher.image.digest="${PUBLISHER_DIGEST}" \
  --set-string controller.agentExecutionSnapshot.existingSecret=agent-execution-snapshot-key \
  --set-string controller.agentExecutionSnapshot.key=snapshot-key \
  --set-string webhooks.tls.existingSecret=orka-webhook-tls \
  --set-string webhooks.caBundle="${WEBHOOK_CA_BUNDLE}" \
  --set providerProxy.enabled=true \
  --wait
```

The chart only supports `harness-v2`. The protocol remains a fixed installation
identity, and any other controller mode or removed wrapper setting is rejected.

The chart installs the exact cross-namespace ingress policy for Vekil. The
chart-managed provider proxy itself always runs in the Helm release namespace. Leave
`controller.acpRuntime.providerProxyNamespace` empty or set it to that release
namespace. The only supported upstream is
`http://vekil.vekil-system.svc:1337` (an optional trailing slash is normalized);
alternate hosts, namespaces, and ports are rejected because the chart does not
create matching NetworkPolicies.

`service.port` is the controller Service port used by controller and Publisher Service URLs. `controller.apiPort` is only the controller container listener and Service target port.

### SCM proxy NetworkPolicy portability boundary

The SCM proxy NetworkPolicy excludes RFC 1918 and reserved address ranges, but
Kubernetes does not define whether Service destination NAT runs before or
after `ipBlock` evaluation. Some CNI and cloud combinations can therefore
reach the `kubernetes.default` transport through its ClusterIP despite those
exclusions. This does not grant API authorization: the SCM proxy Pod and
ServiceAccount do not mount a service-account token, service links are disabled,
Orka grants that identity no API RBAC, and the proxy refuses to start if the
conventional Kubernetes service-account token path exists. The proxy itself
accepts only exact configured SCM hostnames and rejects non-public DNS answers
and connected peers. Clusters requiring TCP-level API denial must add and
validate a CNI- or cloud-native pre-DNAT or Service-aware egress control;
standard NetworkPolicy cannot guarantee this portably.

### Coordinated authentication Secret rotation

The Publisher and SCM egress proxy read their authentication material at process startup. Rotate each Secret and its non-secret rollout marker in the same Helm upgrade:

- When rotating `publisher.auth.existingSecret` (or the chart-managed publisher auth values), bump `publisher.auth.rolloutNonce`. The marker is added only to the controller and Publisher Pod templates so both restart onto the same credential generation.
- When rotating `scmEgressProxy.auth.existingSecret` (or the chart-managed SCM proxy token), bump `scmEgressProxy.auth.rolloutNonce`. The marker is added only to the Publisher and SCM proxy Pod templates.

The nonce is a revision label, not a credential. Never put Secret content in it. A coordinated upgrade may briefly fail closed while Pods roll, but it avoids an indefinite split generation.

CRDs are cluster-scoped and shared by every Orka release. Use `--skip-crds`
only when a designated platform or GitOps workflow already manages compatible
Orka CRDs for the cluster.

## Installation ownership

Every release runs `harness-v2` and requires a distinct, non-empty
`controller.watchNamespace` labeled `orka.ai/controller-mode: harness-v2`.
The watch namespace must equal the Helm release namespace. The runtime
namespace must differ from the release namespace and belong to that release.

Controller Services, worker ServiceAccounts, and worker RBAC are scoped to the
Helm release name. Run only one Orka controller release per namespace. If a
cluster has multiple releases, every release (including the first) must use a
cluster-unique release name or `fullnameOverride`, a separate controller
namespace, and a distinct, non-empty `controller.watchNamespace`. Cluster-wide
watchers are rejected. All releases share the same cluster-scoped CRDs, and
shared cluster admission resources require one designated owner.

## Upgrade

An in-place controller upgrade preserves the existing `harness-v2` namespace
claim and the live controller's protocol, watch namespace, chart fullname,
runtime namespace, and snapshot encryption Secret identity. A deleted
controller can be recreated under that retained namespace claim. Unsupported
or unclassified installations require a new release and namespace.

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
wait for all 26 production CRDs to become `Established`, and then upgrade Orka.

If a previous release was uninstalled, update its retained CRDs first and install
the replacement release with `--skip-crds`.

## Uninstall and deletion

`helm uninstall` removes release resources but retains Orka's CRDs and custom
resources. This is Helm's standard `crds/` behavior and is not controlled by a
chart value.

Deleting a CRD also deletes every custom resource stored under that kind. Delete
Orka CRDs only as an explicit cluster-wide data-destruction operation after the
resources have been removed or backed up.
