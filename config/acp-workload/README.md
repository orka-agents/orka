# ACP workload base

This Kustomize base deploys the Orka controller, worker RBAC, ACP runtime broker
proxies, and the clean-room workspace publisher without installing CRDs.

Before applying it directly, create the required Secrets in `orka-system`:

- `acp-artifact-capability` with `capability-secret`
- `agent-execution-snapshot-key` with `snapshot-key` containing exactly 32 raw
  bytes or their base64 encoding
- `workspace-publisher-auth` with `controller-token` and `operation-capability-secret`
- `provider-auth-proxy` with `token`
- `scm-egress-proxy-auth` with `token`

Direct base users must also create exactly one `AgentExecutionControl` named
`cluster` in `orka-system` before starting the controller. The canonical
production overlay owns that singleton with v1 disabled and v2 enabled.

Use `config/acp-production` or `make deploy` for the supported digest-pinned
production flow. That path creates missing Secrets without printing their values,
renders immutable controller, runtime, and publisher image references, and applies
the namespace, generated runtime-image ConfigMap, and execution-control singleton
before dependent workloads.
