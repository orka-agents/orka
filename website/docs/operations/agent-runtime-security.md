# AgentRuntime security operations

AgentRuntime integrations preserve one invariant: remote agents may ask, but Orka decides and executes.

## Trust boundaries

| Zone | Trust | Notes |
| --- | --- | --- |
| Orka control plane | trusted for policy, approvals, idempotency, audit | owns Tool execution and credentials |
| AgentRuntime adapter | least-privileged protocol adapter | should not store downstream Tool secrets |
| Remote execution backend | untrusted/semi-trusted workload runtime | may be prompt-injected or compromised |
| Downstream tools | protected systems | reached only through Orka Tool execution |

## Required controls

- Keep credentials out of `AgentRuntime.spec.deployment.endpoint`.
- Keep the default `spec.deployment.transportSecurity: tls` for `https://`
  endpoints. Use `insecure-cluster-local-http` only for a selector-backed,
  non-`ExternalName` Service in the same namespace as the `AgentRuntime`.
- Bind runtime bearer Secrets with `orka.ai/agent-runtime-auth=true`, optional `orka.ai/agent-runtime-name`, and required `orka.ai/agent-runtime-endpoint`.
- Use namespace-local facades unless and until a tenant runtime catalog is intentionally designed.
- Expose tools with explicit `Task.spec.agentRuntime.allowedTools`; brokered mode does not imply access to all Tools.
- Classify remote-exposed Tools with `spec.brokeredToolClass`.
- Use write-class Tools for consequential actions so approval and exact-argument digest checks run before execution.
- Treat `WaitingForApproval=True` as an intentional parked state, not a failure.
- Investigate `tool_execution_outcome_unknown` before retrying a write; Orka failed closed to avoid duplicate side effects.

## Production hardening checklist

- Prefer HTTPS, mTLS, signed turn tokens, or private networking for external adapters.
- Rotate runtime bearer Secrets and ensure the `AgentRuntime` observes the new Secret resourceVersion before use.
- Ensure downstream tools honor `Idempotency-Key` for write requests.
- Keep large artifacts in artifact storage and return safe references rather than huge summaries.
- Redact auth headers, tokens, TxTokens, and raw transcripts from adapter logs.

Orka rejects AgentRuntime redirects. For insecure cluster-local HTTP, it also
disables environment-configured HTTP proxies. `/v1/health` and
`/v1/capabilities` must remain available without bearer authentication; Orka
uses bearer auth only for turn and turn-resource endpoints.

## Migration from implicit HTTP

Upgrade the `agentruntimes.core.orka.ai` and `tasks.core.orka.ai` CRDs before
rolling out a controller that writes the new fields. Helm does not update
already-installed CRDs from a chart's `crds/` directory. For Helm-managed
installations, run the repository's trusted CRD migration helper against the
same packaged chart archive that will be upgraded:

```bash
scripts/helm-chart.sh upgrade-crds \
  --chart ./orka-<version>.tgz \
  --kube-context <context> \
  --release <release> \
  --namespace <namespace> \
  --yes
```

Do not deploy the new controller until `agentruntimes.core.orka.ai` accepts
`transportSecurity` and `tasks.core.orka.ai` accepts `transportSecurity`,
`backendPodName`, `backendPodUID`, and `backendAddress`; otherwise the API
server can reject the AgentRuntime spec field or prune the frozen Task transport
and backend identity needed for safe turn recovery.

Existing manifests that use `http://` without `transportSecurity` now resolve to
the `tls` default and are rejected. Migrate only namespace-local Service
facades by adding:

```yaml
spec:
  deployment:
    endpoint: http://runtime.<namespace>.svc.cluster.local:8080
    transportSecurity: insecure-cluster-local-http
```

The referenced Service must exist, have a non-empty selector, not be an
`ExternalName`, and be in the `AgentRuntime` namespace. The endpoint host must
use the unambiguous `service.namespace.svc.<cluster-domain>` FQDN; Orka derives
the cluster domain from the controller pod's ordered Kubernetes search domains
and fails closed if it cannot identify that tuple. Prefer moving external or
cross-namespace endpoints to HTTPS instead. Transport mode is frozen in Task
status; an older in-flight HTTP task with no frozen mode is treated as `tls`
and must be retried or recreated after the `AgentRuntime` manifest is migrated.
