# ACP harness-v2 workload base

This Kustomize base deploys one static `harness-v2` Orka controller, worker
RBAC, ACP runtime broker proxies, and the clean-room workspace publisher
without installing CRDs. It does not contain the harness v1 wrapper or a
cross-protocol fallback.

Before applying it directly, create the required Secrets in `orka-system`:

- `acp-artifact-capability` with `capability-secret`
- `agent-execution-snapshot-key` with `snapshot-key` containing exactly 32 raw
  bytes or their base64 encoding
- `workspace-publisher-auth` with `controller-token` and `operation-capability-secret`
- `provider-auth-proxy` with `token`
- `scm-egress-proxy-auth` with `token`

The base claims `orka-system` with `orka.ai/controller-mode: harness-v2` and
starts the controller with `--controller-mode=harness-v2` and
`--watch-namespace=orka-system`. An overlay that chooses a different namespace
must patch the namespace claim and watch argument together before the first
installation. Never retarget an existing installation or change its mode
claim in place. Use a dedicated controller namespace, ServiceAccount, Lease,
store, and runtime namespace for this installation.

Use `config/acp-production` or `make deploy` for the supported digest-pinned
production flow. That path creates missing Secrets without printing their values,
renders immutable controller, runtime, and publisher image references, and
applies the namespace and generated runtime-image ConfigMap before dependent
workloads.

CRDs are shared across every Orka release on a cluster. Apply the compatible
CRD bundle through one designated owner; this workload base must not compete
with another release for CRD or cluster-scoped admission ownership.
