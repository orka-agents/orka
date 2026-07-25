# ADR 0013: Stage the generic gateway resource rollout

Date: 2026-07-16

## Status

Accepted for the first vertical slice.

## Context

The complete design includes transport routing, policy, discovery, semantic binding, execution, and delivery. Publishing every proposed resource before a working vertical slice would freeze overlapping responsibilities and provider assumptions into `v1alpha1`.

## Decision

The first public API contains only:

- cluster-scoped `GatewayClass`, the administrator-owned adapter profile;
- namespaced `Gateway`, one HTTPS endpoint or TLS-authenticated same-namespace Service plus separate inbound/outbound auth;
- namespaced `GatewayBinding`, exact normalized context/sender matching to one Agent.

Orka core reconciles all three through `orka.gateway.v1`. Real provider adapters remain out of tree. `GatewayRoute`, standalone `GatewayPolicy`, and `GatewayContext` remain deferred until the first slice proves that transport routing, governance, and discovery need separate resources. Existing provider-specific webhooks and `Task.spec.webhookURL` are unchanged.

## Consequences

`GatewayBinding` is the sole V1 semantic target-selection resource. Unknown contexts and senders fail closed. Deferred resources cannot route directly to an `AgentRuntime`; execution runtime selection remains owned by the bound Agent and Task.
