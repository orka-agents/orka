---
description: "Orka's vendor-neutral OAuth Transaction Token profile for governing who asked for a piece of work."
---

# Transaction tokens

A transaction token (TxToken) is a short-lived, signed record of who asked for a piece of
work and what they allowed it to do. When a request reaches Orka with one, Orka can
authorize each operation against the token's scopes, stamp the verified requester onto the
Tasks it creates, and hand narrower tokens to delegated agents and outbound tool calls. The
same transaction ID then ties together everything the request caused.

Orka implements a strict, vendor-neutral OAuth Transaction Token profile. Transaction tokens
govern requests; they are not credentials for downstream resources. To reach a downstream
API, a Tool uses an [outbound access policy](outbound-access.md).

## Profile

Configure the canonical profile only:

```yaml
controller:
  contextToken:
    profile: transaction-token
    issuer: https://transactions.example.test
    audience: orka
    headers: Txn-Token
    authzMode: enforce
```

The token must be an RS256 JWT with `typ: txntoken+jwt`, a matching issuer/audience, valid time claims, and non-empty `sub`, `exp`, `iat`, `txn`, `scope`, and `req_wl` claims. `Txn-Token` is the default raw header. `Authorization: Bearer` is opt-in (`--context-token-headers=Txn-Token,Authorization:Bearer`), and even then only bearer JWTs with `typ: txntoken+jwt` are treated as transaction tokens, so ServiceAccount and OIDC bearer authentication keep working.

`authzMode` is `off`, `audit`, or `enforce`. In `enforce` mode each operation requires its
scope, and Orka also honors signed `tctx` constraints — for example namespace, task type,
agent, workspace repository/branch/ref, and allowed tools on Task creation, and namespace,
provider, model, and allowed tools on chat and compatibility-API calls. The controller flags
are listed in [Configuration](../reference/configuration.md#external-api-context-token-authentication).

## Governance retained by Orka

- operation-scope authorization and signed `tctx` constraints
- immutable `spec.requestedBy` and `spec.transaction` provenance
- safe transaction labels, annotations, and context digests
- child scope subset validation
- owner-referenced Secrets for delegated child tokens
- request-time propagation and fail-closed replacement
- credential redaction from status, events, logs, metrics, errors, and Tool results

## Default scopes

When authorization is enabled, Orka defaults to operation-specific scopes such as `orka:tasks:create`, `orka:tasks:get`, `orka:tasks:list`, `orka:tasks:delete`, `orka:tools:read`, `orka:tools:use`, `orka:providers:use`, `orka:agents:read`, `orka:agents:write`, `orka:memory:read`, `orka:memory:write`, `orka:sessions:read`, `orka:sessions:write`, `orka:security:read`, `orka:security:write`, `orka:monitors:read`, `orka:monitors:write`, `orka:monitors:operate`, `orka:skills:read`, and `orka:skills:write`. Using credential-bearing Secrets or minting ServiceAccount tokens for outbound access requires `orka:secrets:credentials:read`. Every default can be replaced with its `--context-token-*-scopes` flag or matching environment/Helm value.

## Restricted tool grants

A `tctx.allowedTools` constraint must cover every tool a Task will expose, including the
tools Orka adds automatically: memory tools on every `type: ai` Task, the coordination set
for coordinator Agents, `request_approval` for autonomous coordinators, and the messaging
tools for delegated children. [Built-in tools](../reference/built-in-tools.md#who-gets-which-tools)
lists exactly which tools each Task receives. Task admission and delegated child validation
both compute this effective tool set, and a token that omits a required tool is rejected.

- The annotation `orka.ai/disable-coordination-tool-injection: "true"` suppresses implicit
  coordination, approval, and child messaging tools. Memory tools stay, and explicitly
  selected tools still need grants.
- Agent Tasks using `runtimeRef` receive no implicit coordination or child messaging tools;
  the registered runtime tool policy is authoritative.
- Child grants must stay subsets of the parent grant.
- An empty `allowedTools: []` is a valid, explicit deny-all. Preserve the empty array when
  issuing or forwarding tokens, because omitting the claim removes the restriction. A
  deny-all grant rejects any Task with a nonempty effective tool set, including `type: ai`
  Tasks with their memory tools.
- Malformed authorization constraints fail token validation.

Verify representative Tasks are accepted with the intended grant and denied when a required
tool is omitted.

## Exact TTS endpoint

A Token Transaction Service (TTS) exchanges a token for a narrower one. Orka uses it to give
delegated child Tasks and outbound Tool calls their own scoped tokens.

```yaml
controller:
  contextToken:
    tts:
      endpoint: https://transactions.example.test/oauth/token
      audience: orka
      tokenSource: incoming
      childScope: orka:agents:run
      outboundScope: orka:tools:http
```

Equivalent flags and environment variables are `--context-token-tts-endpoint` and `ORKA_CONTEXT_TOKEN_TTS_ENDPOINT`. The value is the exact OAuth endpoint; Orka does not append a path.

Transaction-token TTS calls use RFC 8693, request `urn:ietf:params:oauth:token-type:txn_token`, and require a matching `issued_token_type` plus `token_type=N_A`.

Delegated children receive their token in a short-lived Secret they own; see
[delegating with transaction tokens](../guides/multi-agent-coordination.md#delegating-with-transaction-tokens).
Raw tokens never enter Task specs, status, or logs.

## Migrate from the Kontxt profile

The in-tree Kontxt profile and deployment assets have been removed. To migrate:

1. Change `--context-token-profile=kontxt` to `--context-token-profile=transaction-token` (or the matching environment/Helm value).
2. Replace the former TTS base URL with the exact endpoint:
   - `--context-token-tts-endpoint`
   - `ORKA_CONTEXT_TOKEN_TTS_ENDPOINT`
   - `controller.contextToken.tts.endpoint`
3. Move provider installation, signing/JWKS, and live E2E configuration to [`orka-agents/orka-integration-kontxt`](https://github.com/orka-agents/orka-integration-kontxt).
4. Use [`orka-agents/orka-integration-agentgateway`](https://github.com/orka-agents/orka-integration-agentgateway) for gateway routing and downstream OAuth exchange examples.
5. Remove the old URL flag/environment/value; there is no compatibility alias.
6. Update issuer policies for [restricted tool grants](#restricted-tool-grants) so they cover the effective tool set, then reissue affected tokens or narrow the Task configuration.

Rollback requires deploying the previous Orka release and restoring its old Kontxt configuration. Do not run the previous controller with the new exact-endpoint-only values.

## Provider integrations

Provider installation and live tests are out of tree. See the [Kontxt integration repository](https://github.com/orka-agents/orka-integration-kontxt) and the [agentgateway integration repository](https://github.com/orka-agents/orka-integration-agentgateway).
