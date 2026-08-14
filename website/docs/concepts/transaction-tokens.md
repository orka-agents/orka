# Transaction Tokens

Orka implements a strict, vendor-neutral OAuth Transaction Token profile for request governance. Transaction tokens are not downstream resource credentials.

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

The token must be an RS256 JWT with `typ: txntoken+jwt`, a matching issuer/audience, valid time claims, and non-empty `sub`, `exp`, `iat`, `txn`, `scope`, and `req_wl` claims. `Txn-Token` is the default raw header. `Authorization: Bearer` is opt-in so ServiceAccount and OIDC bearer authentication can coexist.

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

## Exact TTS endpoint

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

With context-token authorization in `enforce` mode, direct transactional AI and agent Task creation requires this endpoint with `tokenSource: incoming`. Orka rejects the request before creating a Task when TTS is disabled or uses `serviceAccount`, because only the incoming caller token can be exchanged after Kubernetes assigns the Task name and UID.

For those direct Tasks, Orka creates two Task-owned Secrets. The Task annotation names only the workload Secret, which contains the current task-bound token plus non-sensitive rotation metadata and mounts only the token key into Job workloads. A separately randomized renewal-authority Secret is not referenced from the Task or workload Secret and is discovered only by the controller through Task-UID labels; workers cannot enumerate Secrets. The controller uses the persisted caller or ServiceAccount credential only for the initial exchange; every later refresh replaces the current task-bound Txn-Token before it expires. Long-running transactional Tasks therefore require a TTS that supports Txn-Token replacement and permits replacements to extend the token-expiration chain for the supported Task duration. Orka resolves the latest workload Secret value for `runtimeRef` broker calls and deletes both Secrets when the Task terminates. Raw renewal authority is never written to Task spec, status, events, or logs.

Because Kubernetes Secret-volume updates are asynchronous, directly created AI Tasks require a configured TTS child TTL of at least five minutes, and Orka rejects exchanged Job tokens with less than four minutes of remaining lifetime. RuntimeRef Agent tasks read the latest token directly through the controller and do not use the projected-volume minimum.

Transaction-token TTS calls use RFC 8693, request `urn:ietf:params:oauth:token-type:txn_token`, and require a matching `issued_token_type` plus `token_type=N_A`.

## Provider integrations

Provider installation and live tests are out of tree. See the [Kontxt integration repository](https://github.com/orka-agents/orka-integration-kontxt) and the [agentgateway integration repository](https://github.com/orka-agents/orka-integration-agentgateway).
