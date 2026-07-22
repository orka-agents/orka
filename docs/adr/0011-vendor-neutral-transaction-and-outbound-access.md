# ADR 0011: Separate transaction governance from outbound resource access

- Status: Accepted
- Date: 2026-07-14

## Context

Orka previously coupled its strict transaction-token behavior to one provider SDK and used the same provider-specific TTS configuration for transaction delegation and outbound Tool calls. That made transaction governance, resource credentials, and network routing look like one concern even though they have different trust boundaries and response semantics.

## Decision

Orka core owns three separate semantic classes.

1. **Transaction governance** authenticates, authorizes, delegates, and audits one strict OAuth Transaction Token profile.
2. **Resource credentials** are obtained through a reusable OAuth exchange module and are accepted only as Bearer credentials for downstream resources.
3. **Gateway routing** sends the original Tool request through an explicitly trusted Kubernetes Service without turning the gateway or its resource credential into transaction authority.

Provider installation, provider CRDs, ext-authz configuration, and provider-specific E2E assets live in separately versioned integration repositories.

## Transaction-token contract

The canonical profile name is `transaction-token`. No compatibility alias is accepted.

- Default header: `Txn-Token`
- Required JOSE `typ`: `txntoken+jwt`
- Required claims: `sub`, `exp`, `iat`, `txn`, `scope`, `req_wl`
- Requested token type: `urn:ietf:params:oauth:token-type:txn_token`
- TTS response: matching `issued_token_type` and `token_type=N_A`
- Configuration: `--context-token-tts-endpoint`, `ORKA_CONTEXT_TOKEN_TTS_ENDPOINT`, and `controller.contextToken.tts.endpoint`

The endpoint value is exact. Orka never appends a provider-specific path. The removed URL/base-URL names are not aliases.

Orka retains signature, issuer, audience, time, required-claim, operation-scope, signed `tctx`, child-scope subset, immutable Task provenance, owner-referenced child-token Secret, outbound propagation, safe metadata, metrics, and redaction behavior. Transaction tokens and resource credentials remain distinct even when both are present on one gateway-bound request.

## `OutboundAccessPolicy` contract

`OutboundAccessPolicy` is namespaced and referenced by `Tool.spec.http.outboundAccessPolicyRef`. Tool and policy must share a namespace. Exactly one adapter is required:

```yaml
spec:
  direct: {}
  # or
  gateway: {}
```

### Direct adapter

Direct mode supports RFC 8693 token exchange and RFC 7523 JWT bearer grants.

- Subject sources: `TransactionToken`, `ServiceAccount`, or same-namespace `SecretRef`
- Optional RFC 8693 actor token
- Arbitrary string token-type URNs
- Repeated audiences/resources, scopes, requested token type, expected issued-token type for RFC 8693, and static additional form parameters
- Client authentication: none, client-secret basic/post, or `private_key_jwt`
- Exact HTTPS URL or Kubernetes Service token endpoint
- Configurable output header/prefix, default `Authorization: Bearer `
- Mandatory Bearer response validation
- Digest-keyed bounded in-memory cache with singleflight and subject-expiry capping

Direct mode rejects reserved OAuth additional parameters, `Txn-Token` as the output header, `authSecretRef` coexistence, cross-namespace Secrets, empty credentials, missing/non-Bearer results, and mismatched issued-token types. Transaction-token requested scopes must be a subset of the parent transaction scopes.

### Gateway adapter

Gateway mode selects an exact Kubernetes Service namespace/name/port, scheme, and optional TLS server name/CA Secret. Orka dials that Service while preserving the original Tool authority, path, query, method, body, protocol headers, idempotency key, transaction token, and explicit Tool authorization. Same-namespace Services are allowed automatically. Cross-namespace Services require an exact entry in the appropriate gateway allowlist; wildcards are forbidden.

The original Tool URL still passes SSRF validation. Only the explicitly trusted gateway dial target bypasses public-host validation. Gateway redirects are not followed. Because Orka dials only the trusted Service, that gateway is the enforcement boundary that binds the preserved authority to an operator-configured upstream; it must not act as an unrestricted DNS forward proxy. Orka's execution-time hostname check is defense in depth against stale or newly mutated Tool definitions, not a replacement for gateway route policy.

### Trusted Service configuration

Cross-namespace allowlists use exact `namespace/name:port` entries:

- `--outbound-access-trusted-gateway-services`
- `ORKA_OUTBOUND_ACCESS_TRUSTED_GATEWAY_SERVICES`
- `controller.outboundAccess.trustedGatewayServices`
- `--outbound-access-trusted-token-endpoint-services`
- `ORKA_OUTBOUND_ACCESS_TRUSTED_TOKEN_ENDPOINT_SERVICES`
- `controller.outboundAccess.trustedTokenEndpointServices`

Gateway and token-endpoint trust are separate. No wildcard syntax is accepted.

## Status, failure, and observability

`OutboundAccessPolicy.status` contains only `observedGeneration`, `Accepted`, and `ResolvedRefs`. Messages identify safe validation categories without credential values or credential-bearing URLs. Secret, Service, ServiceAccount, policy generation, and Tool changes trigger reconciliation. Execution revalidates critical references rather than trusting status alone.

OAuth exchanges fail closed on cancellation, timeout, malformed/oversized JSON, OAuth errors, non-2xx responses, empty tokens, response-type mismatch, and TLS/reference errors. Low-cardinality metrics use adapter, grant class, result, reason, and duration. Cache keys and metrics never contain raw tokens.

Raw transaction tokens, subject assertions, actor tokens, ServiceAccount tokens, client secrets, signing keys, exchanged credentials, MCP session IDs, gateway error bodies, and credential-bearing URLs must not appear in status, events, logs, metrics, or Tool errors/results.

## Integration repositories

- `orka-agents/orka-integration-kontxt`
- `orka-agents/orka-integration-agentgateway`

They own pinned provider installations, compatibility matrices, version-specific overlays, kind CI, release workflows, provider examples, and live integration tests.

## Non-goals

- Provider, webhook, and general AgentRuntime credential injection
- A central credential broker
- Upstream agentgateway changes
- Wildcard cross-namespace trust
- Ephemeral incoming user tokens as V1 direct subject sources
- Long-lived Entra OBO lifecycle management
- Sending downstream production credentials to remote AgentRuntime adapters

## Consequences

This is a breaking release. Deployments must migrate the profile name and exact TTS endpoint, and use the integration repositories for provider-specific installation. Rollback requires the previous Orka release and its former provider-specific configuration.
