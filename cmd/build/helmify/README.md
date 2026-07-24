# Orka Helm chart generator

This directory is derived from Gatekeeper's `cmd/build/helmify` flow at
`c9b67657102032a460a28e7f3b9c88ec0c193453` and adapted for Orka.

`make manifests` performs the same staged generation pattern used by Gatekeeper:

1. `controller-gen` refreshes the canonical CRDs under `config/crd/bases`.
2. Kustomize renders `config/default`.
3. This generator copies the static chart inputs and writes every rendered CRD
   under `manifest_staging/charts/orka/crds`.
4. Kustomize also writes the next-release installer to
   `manifest_staging/deploy/orka.yaml`.

Only CRDs are generated from the Kustomize stream in this adaptation. Orka's
existing non-CRD Helm templates remain static inputs under `static/templates`;
full manifest-to-template conversion would require Orka-specific Helm
substitutions and is intentionally outside this change.

Release preparation promotes the reviewed `manifest_staging/deploy` and
`manifest_staging/charts` trees into the root `deploy` and `charts` snapshots.

The upstream Apache-2.0 license and notice are retained in this directory.
