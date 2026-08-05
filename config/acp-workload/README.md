# ACP workload base

This Kustomize base deploys the Orka controller, worker RBAC, ACP runtime broker
proxies, and the clean-room workspace publisher without installing CRDs.

Before applying it directly, create the required Secrets in `orka-system`:

- `acp-artifact-capability` with `capability-secret`
- `workspace-publisher-auth` with `controller-token` and `operation-capability-secret`
- `provider-auth-proxy` with `token`
- `scm-egress-proxy-auth` with `token`

Use `config/acp-production` or `make deploy` for the supported digest-pinned
production flow. That path creates missing Secrets without printing their values,
renders immutable controller, runtime, and publisher image references, and applies
the namespace and generated runtime-image ConfigMap before dependent workloads.
