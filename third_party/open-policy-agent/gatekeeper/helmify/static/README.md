# Orka Helm chart

## Generated release chart

This chart is generated from the Gatekeeper-style inputs under
`third_party/open-policy-agent/gatekeeper/helmify/`; do not edit generated
copies under `manifest_staging/charts/orka` or `charts/orka` directly.
`make manifests` writes the committed next-release chart to
`manifest_staging/charts/orka`, and release preparation promotes that exact
chart into `charts/orka` before a matching `v*` tag publishes it.

The chart packages Orka's nine cluster-scoped CustomResourceDefinitions in
`crds/`:

- `agentruntimes.core.orka.ai`
- `agents.core.orka.ai`
- `providers.core.orka.ai`
- `repositorymonitors.core.orka.ai`
- `repositoryscans.core.orka.ai`
- `skills.core.orka.ai`
- `substrateactorpools.core.orka.ai`
- `tasks.core.orka.ai`
- `tools.core.orka.ai`

## Fresh install

A normal install creates the CRDs before the release's templated resources:

```bash
helm install orka charts/orka \
  --namespace orka-system \
  --create-namespace \
  --wait
```

Use `--skip-crds` only when a designated platform or GitOps workflow already
owns compatible Orka CRDs in the cluster:

```bash
helm install orka charts/orka \
  --namespace orka-system \
  --create-namespace \
  --skip-crds \
  --wait
```

## CRD ownership

CRDs are cluster-scoped and shared by every Orka release. Designate exactly one
platform workflow or release owner to manage their lifecycle. All other Orka
releases must use `--skip-crds` and coordinate controller upgrades with the CRD
owner. A CRD schema change affects every Orka release and custom resource in the
cluster.

## Upgrade

> **Required:** run the guarded CRD migration from the exact target chart
> before every `helm upgrade`.

Helm processes `crds/` during installation only. It does not create or update
those files during `helm upgrade`. This rule also applies to releases installed
from Orka chart `0.1.0` or any older chart that contained zero CRDs: an ordinary
upgrade to a fixed chart will still leave the CRDs missing.

Use the exact immutable `.tgz` and the `scripts/helm-crds.sh` helper from
its matching Orka source release. The helper requires `helm`, `kubectl`, `jq`,
and `tar`. It verifies that the package contains
the canonical CRDs, server-preflights all nine operations, replaces each live
CRD `spec` with UID/resource-version concurrency guards, waits for establishment,
and compares the server-normalized live specs with the target before returning.
It intentionally does not roll back a partial migration because restoring an old
schema can invalidate already accepted custom resources.

```bash
set -euo pipefail

TARGET_CONTEXT="replace-with-context"
TARGET_CHART="/absolute/path/to/orka-<version>.tgz"
MIGRATOR="/path/to/matching/orka-source/scripts/helm-crds.sh"

"$MIGRATOR" apply-package "$TARGET_CHART" \
  --kube-context "$TARGET_CONTEXT"

helm upgrade orka "$TARGET_CHART" \
  --namespace orka-system \
  --kube-context "$TARGET_CONTEXT" \
  --wait
```

If the helper reports a preflight, concurrency, or verification failure, do not
upgrade the controller. Inspect the reported CRD and rerun the same command after
resolving the conflict.

If another GitOps system owns the CRDs, perform the CRD-first step through that
system instead of the command above, wait for all nine CRDs to become
`Established`, and only then upgrade the Orka release.

### Replacement install with retained CRDs

If a previous release was uninstalled, its CRDs remain. Update those retained
CRDs from the target chart first, then install the replacement release with
`--skip-crds`:

```bash
helm install orka "$TARGET_CHART" \
  --namespace orka-system \
  --create-namespace \
  --kube-context "$TARGET_CONTEXT" \
  --skip-crds \
  --wait
```

## Uninstall and deletion

`helm uninstall` removes the release resources but retains the Orka CRDs and
custom resources. This is intentional data protection and is not configurable
through chart values.

Deleting a CRD directly also deletes every custom resource stored under that
kind. Only delete Orka CRDs as an explicit cluster-wide data-destruction action
after all Orka releases and required custom resources have been removed or
backed up.
