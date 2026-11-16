# Orka Helm chart generator

This directory is derived from Gatekeeper's `cmd/build/helmify` flow at
`c9b67657102032a460a28e7f3b9c88ec0c193453` and adapted for Orka.

`make manifests` performs the staged generation flow:

1. `controller-gen` refreshes the canonical CRDs under `config/crd/bases`.
2. Kustomize renders `config/acp-production` as the next-release raw installer.
3. The Helmify Kustomize input renders `config/default`; this generator copies
   the static chart inputs and writes every rendered CRD under
   `manifest_staging/charts/orka/crds`.
4. The raw installer is written to `manifest_staging/deploy/orka.yaml` with
   fail-closed digest placeholders and without CRDs.

Only CRDs are generated from the Kustomize stream in this adaptation. Orka's
existing non-CRD Helm templates remain static inputs under `static/templates`;
full manifest-to-template conversion would require Orka-specific Helm
substitutions and is intentionally outside this change.

Source chart inputs identify development builds as `0.0.0-dev` and select the
latest validated `main` images under that rolling tag. The Development Images
workflow builds the complete release image inventory by digest, then publishes
commit-specific tags and advances development aliases only after validating the
whole set. Its digest-pinned `values.json` artifact selects one exact build.
Override images for feature branches or older checkouts; the rolling tag need
not match those sources. Retagging images does not roll out existing Pods.

`make release-manifest NEWVERSION=vX.Y.Z` stamps the application version, chart
metadata, and all nine release images before generation. Release preparation
then promotes the reviewed `manifest_staging/deploy` and `manifest_staging/charts`
trees into the root `deploy` and `charts` snapshots. Generation never guesses a
version from mutable registry tags.

The upstream Apache-2.0 license and notice are retained in this directory.
