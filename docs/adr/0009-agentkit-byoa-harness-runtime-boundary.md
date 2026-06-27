# ADR 0009: Register AgentKit-built custom agents through the Orka harness contract

Date: 2026-06-26

## Status

Accepted for the first BYOA runtime implementation slice.

## Context

Orka already routes `type: agent` tasks through the `orka.harness.v1` HTTP+SSE contract and the shared CLI harness wrapper. The next custom-agent milestone needs AgentKit-built agents to run under Orka governance without making Orka parse AgentKit authoring formats or accepting arbitrary per-Agent images and commands.

The same custom-agent image may also expose standalone or Foundry-facing protocol skins, but those surfaces are owned by AgentKit. Orka needs a stable Kubernetes-native boundary: an admin-governed runtime registry, readiness/conformance, runtime routing, task provenance, and later trust-tier hardening.

## Decision

Orka consumes only the `orka.harness.v1` endpoint for custom/BYO agent runtimes.

For the first milestone, Orka supports `AgentRuntime.spec.deployment.mode: external-endpoint`. The runtime is pre-deployed outside the `AgentRuntime` controller, and Orka validates the endpoint before any `Task` may route to it. Managed custom runtime Deployments are explicitly deferred until the protocol seam is proven.

`AgentRuntime` is **namespaced in this first slice**. Issue #160 describes the long-term cluster-scoped, admin-owned registry, but the external-endpoint milestone intentionally mirrors the existing namespaced `Provider` governance pattern so namespace owners can validate the protocol seam without introducing cluster-wide admission or runtime allowlists in the same PR. Admin governance is still required for Secret use: harness bearer-token Secrets must explicitly opt in with `orka.ai/agent-runtime-auth: "true"` and may scope themselves to a single runtime with `orka.ai/agent-runtime-name`. A future follow-up can add a cluster-scoped registry or namespace allowlist once managed/dedicated-image runtimes and the BYO trust tier are designed. This ADR records the namespaced choice as deliberate, not accidental drift from #160's end state.

Agents select custom runtimes with `spec.runtime.runtimeRef`. Built-in CLI runtimes continue to use `spec.runtime.type: codex|claude|copilot` and the shared harness-wrapper endpoint. An Agent must set exactly one of `type` or `runtimeRef`.

Tool execution is split into two phases:

- **Observed mode first:** the custom runtime owns its internal tools; Orka records lifecycle, output, and terminal result from harness frames.
- **Brokered mode later:** a custom runtime may request Orka Tool CRDs through harness frames, and Orka enforces approval, policy, exact argument digest matching, and idempotency-key injection before side effects.

Fibey remains the acceptance scenario because it demonstrates value beyond provider routing: approval state, durable provenance, and exactly-once side-effect execution. The first Fibey custom-runtime demo uses an external/pre-deployed AgentKit-compatible harness endpoint; the same-image standalone/Foundry claim remains AgentKit acceptance, not Orka acceptance.

## Consequences

- Orka has a first-class `AgentRuntime` registry and readiness condition without exposing arbitrary Agent image/command fields.
- `AgentRuntime` status carries sanitized observed capabilities and `observedGeneration`, which lets task routing fail closed on non-ready or stale runtime definitions.
- Harness bearer token Secrets must opt in with `orka.ai/agent-runtime-auth: "true"` and may scope themselves to one runtime with `orka.ai/agent-runtime-name`, preventing AgentRuntime authors from using the controller as a generic Secret exfiltration path.
- Built-in CLI runtime behavior remains backward compatible.
- External custom runtimes can prove the cross-repo contract before Orka owns Deployment, ServiceAccount, NetworkPolicy, image policy, or secret-delivery hardening.
- Brokered Orka Tool execution and BYO trust-tier controls remain future work and must be opt-in by capability and policy.

## References

- `docs/development/harness-protocol-mvp.md`
- `docs/development/harness-conformance.md`
- `internal/harness/`
- Planned issue direction: custom/BYO agent runtimes as governed `AgentRuntime` entries rather than free-form per-Agent images.
