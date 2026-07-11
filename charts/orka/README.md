# Orka Helm chart

## Fresh install

A normal fresh install creates the nine Orka CustomResourceDefinitions from the
chart's `crds/` directory before the namespaced release resources:

```bash
TARGET_CONTEXT="replace-with-context"

helm install orka orka/orka \
  --version 0.1.0 \
  --namespace orka-system \
  --create-namespace \
  --kube-context "$TARGET_CONTEXT"
```

Use `--skip-crds` only when another cluster-level process owns the Orka CRDs.

## Upgrade an existing release

Helm does not install or upgrade files in `crds/` during `helm upgrade`. This
also means that upgrading a release originally installed from a chart without
CRDs will not create them. Migrate the CRDs from the **exact packaged target
chart** and wait for all nine to become `Established` before upgrading the
release.

The migration helper requires Bash, Helm, `kubectl`, `jq`, `tar`, `base64`,
`gzip`, and either `sha256sum` or `shasum`. It supports Helm's standard
`secret` storage driver (the default) and the `configmap` driver when
`HELM_DRIVER=configmap` is set. It:

1. validates the explicit Kubernetes context, existing Orka release, local
   chart archive, and exact nine-CRD set. Helm release storage is read through
   `kubectl`, `HELM_KUBE*` endpoint or credential overrides are rejected, and
   the latest release must be `deployed` or `failed` so no pending Helm operation
   can race the migration;
2. snapshots all existing Orka CRDs;
3. runs every exact patch or create with `--dry-run=server` before changing any
   CRD;
4. replaces each live CRD spec with the server-normalized target spec, waits for
   `Established`, and verifies the result; and
5. leaves any partial CRD mutations in place and preserves recovery artifacts
   plus a unique migration marker. Automatic schema rollback is intentionally
   avoided because it can invalidate custom resources or alter API field
   ownership; reconcile and rerun the helper before upgrading Helm.

Set explicit targets first; never rely on the ambient Kubernetes context or an
unpinned remote chart version:

```bash
TARGET_CONTEXT="replace-with-context"
RELEASE=orka
NAMESPACE=orka-system
CHART_REF=orka/orka
TARGET_VERSION=0.1.0
```

### Source-checkout workflow

Package the source chart once and use that same immutable local archive for the
CRD migration and `helm upgrade`. The fail-fast subshell prevents the release
upgrade when any check, migration, wait, or verification fails:

```bash
(
  set -euo pipefail

  WORK_DIR="$(mktemp -d)"
  KEEP_WORK_DIR=false
  cleanup_work_dir() {
    status=$?
    trap - EXIT
    if [[ "$KEEP_WORK_DIR" == true ]]; then
      echo "Target chart and work files preserved at $WORK_DIR" >&2
    else
      rm -rf "$WORK_DIR"
    fi
    exit "$status"
  }
  trap cleanup_work_dir EXIT

  helm status "$RELEASE" \
    --namespace "$NAMESPACE" \
    --kube-context "$TARGET_CONTEXT"

  helm package charts/orka --destination "$WORK_DIR"
  TARGET_CHARTS=("$WORK_DIR"/orka-*.tgz)
  test "${#TARGET_CHARTS[@]}" -eq 1
  TARGET_CHART="${TARGET_CHARTS[0]}"
  test -f "$TARGET_CHART"

  KEEP_WORK_DIR=true
  scripts/helm-chart.sh upgrade-crds \
    --chart "$TARGET_CHART" \
    --kube-context "$TARGET_CONTEXT" \
    --release "$RELEASE" \
    --namespace "$NAMESPACE"

  helm upgrade "$RELEASE" "$TARGET_CHART" \
    --namespace "$NAMESPACE" \
    --kube-context "$TARGET_CONTEXT" \
    --wait
  KEEP_WORK_DIR=false
)
```

### Validated portable workflow

Without a full source checkout, obtain `scripts/helm-crd-upgrade.sh` separately
from a trusted Orka source release or your platform team's vendored tooling and
verify its provenance according to your supply-chain policy. **Treat the chart
archive as data only; never execute code extracted from it.** Then pull the
pinned chart once and use the same local archive for migration and upgrade:

```bash
(
  set -euo pipefail

  MIGRATOR="${MIGRATOR:?path to trusted helm-crd-upgrade.sh}"
  test -f "$MIGRATOR"

  WORK_DIR="$(mktemp -d)"
  KEEP_WORK_DIR=false
  cleanup_work_dir() {
    status=$?
    trap - EXIT
    if [[ "$KEEP_WORK_DIR" == true ]]; then
      echo "Target chart and work files preserved at $WORK_DIR" >&2
    else
      rm -rf "$WORK_DIR"
    fi
    exit "$status"
  }
  trap cleanup_work_dir EXIT

  helm status "$RELEASE" \
    --namespace "$NAMESPACE" \
    --kube-context "$TARGET_CONTEXT"

  helm pull "$CHART_REF" \
    --version "$TARGET_VERSION" \
    --destination "$WORK_DIR"
  TARGET_CHARTS=("$WORK_DIR"/orka-*.tgz)
  test "${#TARGET_CHARTS[@]}" -eq 1
  TARGET_CHART="${TARGET_CHARTS[0]}"
  test -f "$TARGET_CHART"

  KEEP_WORK_DIR=true
  bash "$MIGRATOR" \
    --chart "$TARGET_CHART" \
    --kube-context "$TARGET_CONTEXT" \
    --release "$RELEASE" \
    --namespace "$NAMESPACE"

  helm upgrade "$RELEASE" "$TARGET_CHART" \
    --namespace "$NAMESPACE" \
    --kube-context "$TARGET_CONTEXT" \
    --wait
  KEEP_WORK_DIR=false
)
```

The helper prints the archive's SHA-256 digest before confirmation. Always use
the same explicit `--kube-context` and archive path for migration and upgrade.
CRDs are cluster-scoped and shared by every Orka release in the cluster, so
coordinate the migration across release owners.

### Replacement install with retained CRDs

If the previous release was uninstalled but any Orka CRDs remain, migrate them
before installing the replacement. The explicit missing-release mode refuses a
cluster with neither a release nor existing Orka CRDs. Use trusted repository
code or the separately verified `MIGRATOR`; the chart remains data only:

```bash
(
  set -euo pipefail

  TARGET_CONTEXT="${TARGET_CONTEXT:?set TARGET_CONTEXT}"
  RELEASE="${RELEASE:?set RELEASE}"
  NAMESPACE="${NAMESPACE:?set NAMESPACE}"
  CHART_REF="${CHART_REF:?set CHART_REF}"
  TARGET_VERSION="${TARGET_VERSION:?set TARGET_VERSION}"

  MIGRATOR="${MIGRATOR:-scripts/helm-crd-upgrade.sh}"
  test -f "$MIGRATOR"

  WORK_DIR="$(mktemp -d)"
  KEEP_WORK_DIR=false
  cleanup_work_dir() {
    status=$?
    trap - EXIT
    if [[ "$KEEP_WORK_DIR" == true ]]; then
      echo "Target chart and work files preserved at $WORK_DIR" >&2
    else
      rm -rf "$WORK_DIR"
    fi
    exit "$status"
  }
  trap cleanup_work_dir EXIT

  helm pull "$CHART_REF" \
    --version "$TARGET_VERSION" \
    --destination "$WORK_DIR"
  TARGET_CHARTS=("$WORK_DIR"/orka-*.tgz)
  test "${#TARGET_CHARTS[@]}" -eq 1
  TARGET_CHART="${TARGET_CHARTS[0]}"
  test -f "$TARGET_CHART"

  KEEP_WORK_DIR=true
  bash "$MIGRATOR" \
    --chart "$TARGET_CHART" \
    --kube-context "$TARGET_CONTEXT" \
    --release "$RELEASE" \
    --namespace "$NAMESPACE" \
    --allow-missing-release

  helm install "$RELEASE" "$TARGET_CHART" \
    --skip-crds \
    --namespace "$NAMESPACE" \
    --create-namespace \
    --kube-context "$TARGET_CONTEXT" \
    --wait
  KEEP_WORK_DIR=false
)
```

## Retention semantics

Helm retains resources from `crds/` when a release is uninstalled. This protects
existing Orka custom resources and allows a replacement release to reuse the
same API definitions. Manually deleting an Orka CRD also deletes all custom
resources of that kind, so remove CRDs only as an intentional cluster-wide data
destruction step after every Orka release has been removed.
