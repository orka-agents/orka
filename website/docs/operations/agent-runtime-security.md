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
