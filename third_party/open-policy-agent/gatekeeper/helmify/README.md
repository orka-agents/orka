# Gatekeeper-derived Helm chart generator

This directory contains an Orka-specific fork of Gatekeeper's in-repository
`cmd/build/helmify` chart generator.

- Upstream repository: `https://github.com/open-policy-agent/gatekeeper`
- Upstream revision: `c9b67657102032a460a28e7f3b9c88ec0c193453`
- Imported: 2026-07-23
- Upstream path: `cmd/build/helmify`

The fork retains Gatekeeper's generation model: Kustomize renders canonical
manifests, a small Go program classifies generated objects and combines them
with static Helm-only inputs, and the complete chart is written under
`manifest_staging/charts/orka` for review and testing before release
promotion.

## Orka modifications

- output and static-input paths target Orka
- only CRDs are currently emitted from the Kustomize stream; Orka's existing
  non-CRD templates remain static inputs until their render parity is proven
- canonical CRDs are synchronized byte-for-byte from `config/crd/bases` after
  generation, rather than accepting Kustomize's YAML reserialization
- generation is deterministic and rejects unresolved substitution sentinels,
  duplicate output names, and unsupported generated object kinds

The generated trees under `manifest_staging/`, `charts/`, and `deploy/` must
not be edited directly. Edit this directory or the canonical `config/`
manifests and run `make manifests`.

The Gatekeeper-derived generator source retains the Apache License 2.0
contained in `LICENSE`, with relevant upstream attribution in `NOTICE`.
Orka-authored chart inputs under `static/` and other Orka-specific additions
remain covered by Orka's root MIT license unless a file states otherwise.
