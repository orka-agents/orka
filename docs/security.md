# Security

## Worker Pods

All worker pods run with a hardened security context:

- Non-root user (uid 1000)
- Read-only root filesystem
- All Linux capabilities dropped
- Seccomp profile: RuntimeDefault
- Optional `RuntimeClass` routing via `spec.execution.runtimeClassName`
- Optional runtime-aware placement controls via `spec.execution.nodeSelector`, `tolerations`, and `affinity`

### Runtime Isolation

By default, worker pods use the cluster's standard container runtime. To opt into a stronger isolation boundary, set `spec.execution.runtimeClassName` on an Agent or Task.

- `gvisor` is the first recommended profile for Linux `kind` and similar containerd-based clusters
- `kata-qemu` is supported through the same API and is better suited to `minikube` or production clusters with virtualization-capable nodes
- Only worker Jobs need the alternate runtime; the controller can stay on the default runtime
- Runtime-specific node pools should usually be combined with `nodeSelector`, `tolerations`, or `affinity` so pods land on compatible nodes

### Writable Paths

Only three paths are writable (mounted as EmptyDir volumes):

| Path | Purpose |
|------|---------|
| `/tmp` | Container runtime temporary files |
| `/home/worker` | CLI config/cache (Claude, Copilot, Codex) |
| `/workspace` | Git clone and working directory |

All other filesystem paths are read-only. Workers that write outside these paths will fail.

### Agent Runtime Security Notes

- **Copilot agent worker**: Auto-approves **all** permission requests from the Copilot SDK without filtering. The agent can execute shell commands, file operations, and network requests in autonomous mode with no safeguards beyond the read-only rootfs
- **Claude agent worker**: Sets `HOME=/home/worker` unconditionally, bypassing default HOME permissions
- **Git credentials**: `GIT_TOKEN` and `GITHUB_TOKEN` are set as environment variables, visible to all child processes. If the prompt is user-controlled, the LLM agent could potentially access these via `os.Environ()`
- **Prior task diffs**: Applied via `git apply --check` (dry-run) then `git apply`, but without verifying patch author or integrity. A compromised prior task could inject arbitrary changes

## Controller

The controller runs with:

- Non-root user (uid 65532)
- Read-only root filesystem
- Seccomp profile: RuntimeDefault

## Authentication

- All API endpoints require authentication with a Kubernetes ServiceAccount bearer token or, when configured, an OIDC JWT or generic context token
- ServiceAccount token validation uses the Kubernetes TokenReview API
- OIDC token validation checks issuer, audience, time claims, and RS256 signatures using either the configured JWKS URL or issuer metadata discovery
- Context-token validation supports the built-in `kontxt` profile. `kontxt` tokens are RS256-signed JWTs with `typ: txntoken+jwt`, matching issuer and audience, valid time claims, a non-empty subject, and required `iat`, `txn`, `scope`, and `req_wl` claims
- `kontxt` tokens are read from the raw `Txn-Token` header by default. `Authorization: Bearer` support is opt-in with `--context-token-headers=Txn-Token,Authorization:Bearer` (or `ORKA_CONTEXT_TOKEN_HEADERS`); when enabled, only bearer JWTs with `typ: txntoken+jwt` are handled as context tokens so normal OIDC and ServiceAccount bearer tokens can coexist
- Optional context-token authorization can run in `off`, `audit`, or `enforce` mode with `--context-token-authz-mode` (or `ORKA_CONTEXT_TOKEN_AUTHZ_MODE`). In enforce mode, Task creation requires a configured scope (default `orka:tasks:create`) and honors signed `tctx` constraints for namespace, task type, agent, workspace repo/branch/ref, and allowed tools; Task read/list/delete endpoints require their configured scopes (defaults: `orka:tasks:get`, `orka:tasks:list`, `orka:tasks:delete`), Tool read endpoints require `orka:tools:read`, Orka-managed chat/proxy tool execution requires `orka:tools:use`, chat/OpenAI/Anthropic model-provider endpoints require `orka:providers:use` and honor signed `tctx` constraints for namespace, provider/allowedProviders, model/allowedModels, and allowedTools, Agent read/write endpoints require `orka:agents:read` / `orka:agents:write`, memory endpoints require `orka:memory:read` / `orka:memory:write`, session read/delete endpoints require `orka:sessions:read` / `orka:sessions:write`, security scan read/list/get endpoints require `orka:security:read`, security scan create/update/delete and mutation endpoints require `orka:security:write`, and Skill read/write endpoints require `orka:skills:read` / `orka:skills:write`
- Optional kontxt TTS exchange configuration is available with `--context-token-tts-url` / `ORKA_CONTEXT_TOKEN_TTS_URL` plus token-source and TTL settings. Orka keeps this disabled unless an endpoint is configured and does not store exchanged raw TxTokens in Task resources
- Delegation tools can exchange a mounted subject token for a child-scope TxToken when `ORKA_CONTEXT_TOKEN_TTS_URL`, `ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_FILE`, and `ORKA_CONTEXT_TOKEN_CHILD_SCOPE` are configured; the requested child scope must be a subset of the parent transaction scopes, and the child token is stored in an ephemeral Secret that is referenced by Task annotation, adopted by the child Task after creation, and mounted into the child worker as a token file
- Worker HTTP Tool CRD calls can propagate a TxToken from a mounted file using `ORKA_TRANSACTION_TOKEN_FILE`; when `ORKA_CONTEXT_TOKEN_TTS_URL` and `ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_FILE` are configured, workers exchange the subject token for an operation-scoped outbound TxToken before calling the tool. Token values are read at request time and are not stored in Task spec/status; downstream services must still verify incoming TxTokens and enforce their own policy
- When agent sandbox workspaces are enabled, the outer worker stages mounted transaction/context-subject token files into sandbox-local `0600` files, rewrites `ORKA_TRANSACTION_TOKEN_FILE` / `ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_FILE` for the inner worker, and scrubs the staged files after execution or before retaining a workspace. Raw TxTokens are still not written to Task spec/status/logs.
- See [Kontxt TxToken integration](kontxt.md) for deployment examples, scope vocabulary, transaction metadata, TTS exchange, admission hardening, metrics, and rollout guidance
- OIDC- and context-token-authenticated Task creation stamps the verified identity into immutable `spec.requestedBy`; context-token-authenticated Task creation also stamps immutable `spec.transaction` metadata for audit correlation. Client-supplied `requestedBy` and `transaction` fields are rejected
- Optional Task provenance admission (`--task-provenance-admission-enabled`) rejects untrusted direct Kubernetes Task creates/updates that set or modify Orka-managed provenance fields, including `spec.requestedBy`, `spec.transaction`, and transaction metadata labels/annotations. The opt-in manifest defaults to `failurePolicy: Ignore`; switch it to `Fail` only after webhook TLS, CA bundle injection, and availability are configured
- The `orka` CLI extracts tokens from kubeconfig for browser-based login
- **Token caching**: Validated ServiceAccount tokens are cached for 60 seconds using SHA256 hashes to avoid repeated TokenReview API calls. Token revocation has up to 60s propagation delay. The cache is in-memory only — not persistent across pod restarts

## Secret Management

- LLM API keys and tool credentials are stored in Kubernetes Secrets
- Secrets are referenced via `secretRef` in CRD specs and mounted as read-only volumes
- Secrets are never logged, stored in task specs, or exposed in API responses

## Namespace Scoping

- Controller can be scoped to a single namespace with the `--watch-namespace` flag
- Chat endpoint blocks operations in `kube-system` and `kube-public` namespaces
- The embedded UI is served over the same port as the API (no separate attack surface)

## Multi-Tenancy

Orka supports soft multi-tenancy using Kubernetes namespaces as tenant boundaries.

### Namespace Isolation

Enable `--enforce-namespace-isolation` to restrict users to their ServiceAccount's namespace:

- API requests are rejected (403) if the target namespace differs from the caller's SA namespace
- Tasks cannot reference Agents or Providers in other namespaces (cross-namespace `agentRef.namespace` and `providerRef.namespace` are rejected)
- Workers cannot submit results to namespaces other than their own
- All access denials are logged with caller identity and IP address

### Per-Namespace Task Limits

Use `--max-tasks-per-namespace` to cap the number of active (Pending/Running) tasks per namespace. Tasks exceeding the limit are requeued with backoff. Set to `0` (default) for unlimited.

### Recommended Production Configuration

For multi-tenant deployments, enable both isolation and limits:

```
--enforce-namespace-isolation=true
--max-tasks-per-namespace=50
--watch-namespace=""
```

### Data Isolation

All stored data (results, sessions, plans) is keyed by namespace in SQLite. Queries always filter by namespace. The controller, API server, and workers all enforce namespace boundaries at their respective layers.

### Audit Logging

Security-relevant events are logged at the API layer:

- Authentication failures (missing/invalid tokens)
- Namespace access denials (isolation violations, watch-namespace mismatches)
- Cross-namespace worker access attempts
