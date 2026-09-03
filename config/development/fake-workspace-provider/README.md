# Fake workspace provider development package

This Kustomize package installs the two `fake.workspace.orka.ai` CRDs and their
RBAC. They support the in-memory fake workspace adapter used for local
development and are intentionally excluded from `config/default` and the Orka
Helm chart.

Apply the package before enabling the fake adapter in an otherwise normal Orka
deployment:

```bash
bin/kustomize build \
  --load-restrictor LoadRestrictionsNone \
  config/development/fake-workspace-provider | kubectl apply -f -
```

Run `make kustomize` first if `bin/kustomize` is not present. The relaxed local
load restriction lets this package reuse the generated CRDs and RBAC in their
canonical directories instead of maintaining copies.

The production ACP Helm chart does not expose these development-only gates.
When running the controller directly or from a dedicated development overlay,
enable both required controller flags:

```text
--enable-workspace-provider-api=true
--enable-fake-workspace-provider=true
```

The equivalent environment variables are
`ORKA_ENABLE_WORKSPACE_PROVIDER_API=true` and
`ORKA_ENABLE_FAKE_WORKSPACE_PROVIDER=true`.

The package is not a standalone Orka installation. Removing either fake CRD
also deletes all custom resources stored under that kind.
