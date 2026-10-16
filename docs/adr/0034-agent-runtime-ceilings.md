# ADR 0034: Agent runtime ceilings bound what Tasks may request

Date: 2026-10-16

## Status

Accepted.

## Context

An Agent's `runtime.defaultAllowedTools`, `defaultAllowBash`, and
`defaultMaxTurns` are defaults. A Task's `spec.agentRuntime` replaces them
outright, so the Agent cannot say what it may never do: whoever creates the
Task decides. External `runtimeRef` Agents already have a fixed policy, because
the operator registers an exact MCP allowlist on the AgentRuntime, but built-in
runtimes (Codex, Claude, Copilot, OpenCode) have nothing equivalent.

An agent that is governed and permissioned separately from its callers needs
an upper bound owned by the Agent. Changing the defaults into limits would
break every existing Task that widens them, so the bound has to be opt-in.

## Decision

Add an optional `Agent.spec.runtime.ceiling` with `allowedTools`, `allowBash`,
and `maxTurns`. An omitted ceiling, or an omitted field within it, leaves that
dimension unbounded, so existing Agents and Tasks behave exactly as before.

The ceiling is a validation gate, not a new way of computing policy. Before a
Task is bound, the Task controller resolves the policy the Task would run with,
using the same resolution and normalization the runtime uses, and fails the
Task when that policy exceeds the ceiling. Effective-policy computation, the
execution snapshot, and harness v1 dispatch are unchanged. A Task that passes
the gate therefore runs with a policy inside the ceiling, and no consumer of
the frozen snapshot can disagree with the check. A Task is judged against the
Agent generation it binds to, which the binding already freezes.

The alternative was to make an omitted Task policy fall back to the ceiling
instead of the defaults. That would change effective-policy computation in
several places, including the snapshot that harness v1 replays, and any path
that missed the change would silently run with the old, wider default. A
mismatch between defaults and ceiling is surfaced instead: the Task fails with
the reason, and an Agent whose explicit defaults exceed its ceiling reports
`Ready=False`.

Orka-injected coordination tools for delegated children (`send_message`,
`check_messages`) are not counted, because they are platform plumbing rather
than a capability the caller requested. The implicit `reply_in_conversation`
grant for gateway Tasks does reach outside the Task, so Orka keeps it only when
the projected policy still fits the ceiling. For `runtimeRef` Agents only
`allowedTools` applies; the registered profile already fixes Bash and turns.
The exception is a harness v1 registration, which takes `maxTurns` from the
Task or Agent, so it is also held to the turn ceiling once planning resolves it.

## Consequences

- Owners can publish an Agent whose capabilities do not depend on who creates
  its Tasks. This is the first piece of treating an Agent as a separately
  permissioned principal.
- An Agent with a tool ceiling needs explicit `defaultAllowedTools` inside it,
  or every Task must supply an allowlist; otherwise Tasks resolve to the full
  native tool set and fail.
- The ceiling binds callers of the Task API, not editors of the Agent. Anyone
  with `update` on the Agent can change the ceiling, so Agent RBAC is the
  boundary for the ceiling itself.
- Delegation targets, connector writes, and native `ai` Agents are not covered
  yet. They can extend the same `ceiling` object when needed.
