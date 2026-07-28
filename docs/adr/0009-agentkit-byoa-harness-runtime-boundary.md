# ADR 0009: Register bring-your-own agent runtimes through AgentRuntime

Date: 2026-06-26

## Status

Accepted for the first bring-your-own agent runtime implementation slice.

## Context

Orka routes `type: agent` tasks through `AgentRuntime`, an Orka-facing runtime contract backed by `orka.harness.v1`. The remote execution backend may be a generic self-hosted HTTP runtime, AgentKit Serve, Azure AI Foundry hosted agents, or a future backend. Orka should not parse backend authoring formats or accept arbitrary per-Agent images and commands.

Backend-specific protocol skins are adapter-owned and live outside the Orka source tree. Orka keeps only the provider-neutral protocol, public Go aliases, conformance checks, reference fixtures, and facade samples. Orka needs a stable Kubernetes-native boundary: the `AgentRuntime` registry, readiness/conformance, runtime routing, task provenance, capability tiers, and brokered-governance hooks.

## Decision

Orka consumes only the `orka.harness.v1` endpoint for bring-your-own agent runtimes.

For the first milestone, Orka supports `AgentRuntime.spec.deployment.mode: external-endpoint`. The runtime is pre-deployed outside the `AgentRuntime` controller, and Orka validates the endpoint before any `Task` may route to it. Managed adapter Deployments are explicitly deferred until the protocol seam is proven.

`AgentRuntime` is **namespaced in this first slice**. Issue #160 describes the long-term cluster-scoped, admin-owned registry, but the external-endpoint milestone intentionally mirrors the existing namespaced `Provider` governance pattern so namespace owners can validate the protocol seam without introducing cluster-wide admission or runtime allowlists in the same PR. Admin governance is still required for Secret use: harness bearer-token Secrets must explicitly opt in with `orka.ai/agent-runtime-auth: "true"`, may scope themselves to a single runtime with `orka.ai/agent-runtime-name`, and must bind themselves to the intended endpoint with `orka.ai/agent-runtime-endpoint`. A future follow-up can add a cluster-scoped registry or namespace allowlist once managed/dedicated-image runtimes and the BYO trust tier are designed.

Agents select remote execution backends with `spec.runtime.runtimeRef`. Built-in CLI runtimes continue to use `spec.runtime.type: codex|claude|copilot` and the shared harness-wrapper endpoint. An Agent must set exactly one of `type` or `runtimeRef`.

Tool execution has capability tiers:

- **Observed mode:** the remote execution backend may use its own internal tools; Orka records lifecycle, output, and terminal result from harness frames, but cannot govern backend-internal side effects.
- **Brokered mode:** brokered governance is the target model. Remote backends request Orka Tool execution, and Orka owns authorization, approval, idempotency-key injection, credential resolution, execution/brokering, events, lineage, and audit. Remote backends must not receive production Orka Tool credentials.

Fibey remains one acceptance scenario, but the demo should show the same Orka API and approval UX across generic HTTP, AgentKit Serve, and Foundry-backed `AgentRuntime` facades. Foundry and AgentKit are backend options, not the architecture.

## Consequences

- Orka has a first-class `AgentRuntime` registry and readiness condition without exposing arbitrary Agent image/command fields.
- `AgentRuntime` status carries sanitized observed capabilities and `observedGeneration`, which lets task routing fail closed on non-ready or stale runtime definitions.
- Harness bearer token Secrets must opt in with `orka.ai/agent-runtime-auth: "true"`, may scope themselves to one runtime with `orka.ai/agent-runtime-name`, and must bind to the intended endpoint with `orka.ai/agent-runtime-endpoint`, preventing AgentRuntime authors from using the controller as a generic Secret exfiltration path.
- Built-in CLI runtime behavior remains backward compatible.
- External runtimes can prove the cross-repo contract without placing provider-specific implementations in the Orka repository or making Orka own Deployment, ServiceAccount, NetworkPolicy, image policy, or secret-delivery hardening.
- Brokered Orka Tool execution is opt-in by capability and policy; observed mode must not be marketed as full brokered governance.

## External adapter references

- Microsoft Foundry Hosted Agents (Responses API): [`orka-agents/agent-runtime-foundry`](https://github.com/orka-agents/agent-runtime-foundry)
- Microsoft Foundry Agent Service classic (Threads/Runs API): [`orka-agents/agent-runtime-foundry-classic`](https://github.com/orka-agents/agent-runtime-foundry-classic)

## References

- `docs/development/harness-protocol-mvp.md`
- `docs/development/harness-conformance.md`
- `internal/harness/`
- Planned issue direction: bring-your-own agent runtimes as governed `AgentRuntime` entries rather than free-form per-Agent images.
