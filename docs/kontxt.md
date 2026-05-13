# Kontxt TxToken integration

Orka can participate in `kontxt` transaction-token workflows without storing raw transaction tokens. The integration is intentionally staged so existing Kubernetes ServiceAccount and OIDC callers continue to work unless context-token authentication or authorization is explicitly configured.

## Current capability status

| Capability | Status |
|---|---|
| Ingress TxToken verification | Enabled when `--context-token-profile=kontxt`, issuer, and audience are configured. Tokens are read from `Txn-Token` by default. |
| Verified requester stamping | REST-created Tasks record verified identity in immutable `spec.requestedBy`. |
| Safe transaction metadata | REST-created context-token Tasks record immutable `spec.transaction` plus safe transaction labels/annotations; Jobs, Pods, and worker environment receive the same safe metadata. |
| Context-token authorization | Optional `off`, `audit`, or `enforce` mode. Scopes authorize API, chat, provider, tool, memory, session, skill, and security-scan operations. Selected signed `tctx` values constrain task, provider, model, and tool requests. |
| Delegation and outbound TxTokens | Worker-side delegation and HTTP Tool calls can exchange or propagate TxTokens when worker environment and token files are configured. Child token Secrets are owner-scoped to the parent Task and mounted into child workers. |
| Direct Kubernetes hardening | Optional Task provenance admission webhook rejects untrusted spoofing of Orka-managed provenance fields. |
| Observability | Prometheus metrics cover context-token authentication, authorization decisions, and TTS exchange health. Logs use safe transaction fields and must not include raw tokens. |

Full end-to-end TTS-backed live CI is still a separate rollout step; do not treat verifier-only deployment as proof that every downstream service validates propagated TxTokens.

## Ingress verification

Enable the built-in `kontxt` profile with issuer and audience:

```bash
ORKA_CONTEXT_TOKEN_PROFILE=kontxt
ORKA_CONTEXT_TOKEN_ISSUER=https://kontxt-tts.example.test
ORKA_CONTEXT_TOKEN_AUDIENCE=orka-api
# Optional for kontxt; defaults to <issuer>/.well-known/jwks.json.
ORKA_CONTEXT_TOKEN_JWKS_URL=https://kontxt-tts.example.test/.well-known/jwks.json
```

By default Orka reads raw TxTokens from `Txn-Token`:

```bash
curl -H "Txn-Token: $TXN_TOKEN" https://orka.example.test/api/v1/tasks
```

`Authorization: Bearer` remains reserved for ServiceAccount and OIDC bearer tokens unless context-token bearer support is explicitly opted in:

```bash
ORKA_CONTEXT_TOKEN_HEADERS=Txn-Token,Authorization:Bearer
```

When bearer support is enabled, Orka only treats bearer JWTs with JOSE header `typ: txntoken+jwt` as context tokens; other bearer tokens continue through OIDC or Kubernetes TokenReview authentication.

## Authorization modes

Context-token authorization is disabled by default. Enable it gradually:

```bash
# Evaluate and log safe would-deny decisions, but allow requests.
ORKA_CONTEXT_TOKEN_AUTHZ_MODE=audit

# Reject context-token callers that lack scope or violate signed context.
ORKA_CONTEXT_TOKEN_AUTHZ_MODE=enforce
```

Authorization only applies to authenticated context-token callers. ServiceAccount and OIDC behavior remains backward-compatible unless a request is authenticated as a context token.

### Default scopes

| Operation class | Default scope |
|---|---|
| Task create | `orka:tasks:create` |
| Task read/get-related data | `orka:tasks:get` |
| Task list | `orka:tasks:list` |
| Task delete | `orka:tasks:delete` |
| Tool read | `orka:tools:read` |
| Orka-managed tool use | `orka:tools:use` |
| Chat/OpenAI/Anthropic provider use and model listing | `orka:providers:use` |
| Agent read/write | `orka:agents:read` / `orka:agents:write` |
| Memory read/write | `orka:memory:read` / `orka:memory:write` |
| Session read/write-delete | `orka:sessions:read` / `orka:sessions:write` |
| Security scan read/write | `orka:security:read` / `orka:security:write` |
| Skill read/write | `orka:skills:read` / `orka:skills:write` |

Each scope list is configurable with the corresponding `ORKA_CONTEXT_TOKEN_*_SCOPES` environment variable or `--context-token-*-scopes` flag. Use comma-separated values when allowing aliases.

### Signed `tctx` constraints

Orka also uses selected signed transaction context values for request-specific checks. Example token context:

```json
{
  "namespace": "default",
  "taskType": "agent",
  "agent": "security-reviewer",
  "allowedAgents": ["security-reviewer", "patcher"],
  "repo": "https://github.com/sozercan/orka",
  "branch": "kontxt",
  "allowedTools": ["file_read", "web_search"],
  "allowedProviders": ["openai"],
  "allowedModels": ["gpt-5.4"]
}
```

In `enforce` mode Orka rejects context-token requests whose body or resolved route violates these constraints, for example a mismatched namespace, agent, repository, provider, model, or tool set. In `audit` mode the same policy is evaluated and logged with safe fields, but the request is allowed.

## Safe transaction metadata

When a REST request creates a Task with a valid context token, Orka stamps:

- immutable `spec.requestedBy` with verified subject/issuer/roles;
- immutable `spec.transaction` with safe transaction metadata;
- labels such as `orka.ai/transaction-id` and `orka.ai/auth-profile`;
- annotations such as `orka.ai/transaction-id`, `orka.ai/transaction-scope`, `orka.ai/transaction-context-digest`, and `orka.ai/requester-context-digest`.

The controller propagates safe metadata to Jobs, Pod templates, and worker environment variables including:

```bash
ORKA_TRANSACTION_ID
ORKA_TRANSACTION_PROFILE
ORKA_TRANSACTION_ISSUER
ORKA_TRANSACTION_SUBJECT
ORKA_TRANSACTION_REQUESTING_WORKLOAD
ORKA_TRANSACTION_SCOPE
ORKA_TRANSACTION_SCOPES
ORKA_TRANSACTION_CONTEXT_DIGEST
ORKA_TRANSACTION_REQUESTER_CONTEXT_DIGEST
```

Raw TxTokens are not stored in Task specs/status, labels, annotations, logs, or metrics. Full `tctx` and `rctx` are treated as sensitive; Orka stores digests and selected allowlisted string context fields only.

## TTS exchange, delegation, and outbound propagation

Orka has two token-flow patterns:

1. **Propagate an existing child token**: if a Task annotation references an Orka-owned transaction-token Secret, the controller mounts it into the worker and sets both `ORKA_TRANSACTION_TOKEN_FILE` and `ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_FILE` to the token path. HTTP Tool calls attach that token as `Txn-Token`.
2. **Exchange a narrower token through TTS**: when worker environment includes a TTS URL, subject-token file, and requested child/outbound scope, worker-side delegation tools and HTTP Tool calls call kontxt TTS for a replacement token before creating child Tasks or calling downstream services.

Worker-side exchange environment:

```bash
ORKA_CONTEXT_TOKEN_TTS_URL=https://kontxt-tts.kontxt-system.svc
ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_FILE=/var/run/orka/transaction-token/token
# Optional; defaults to urn:ietf:params:oauth:token-type:txn_token.
ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_TYPE=urn:ietf:params:oauth:token-type:txn_token
# Required for delegate_task/create_container_task exchanges.
ORKA_CONTEXT_TOKEN_CHILD_SCOPE=orka:agents:run
# Optional for HTTP Tool exchanges; falls back to ORKA_TRANSACTION_SCOPE.
ORKA_CONTEXT_TOKEN_OUTBOUND_SCOPE=orka:tools:use
```

Delegation scope is fail-closed: a requested child scope must be a subset of the parent Task transaction scopes. A successful child-token exchange stores the returned raw TxToken only in an owner-referenced Kubernetes Secret so the child worker can mount it; the Task stores only the Secret reference annotation and safe transaction metadata.

Controller/API TTS flags are available for deployments that configure Orka-side exchange plumbing. When a child Task mounts an Orka-owned transaction-token Secret, the controller propagates the TTS URL and worker exchange scope settings into that child worker so deeper delegation can request narrower replacement tokens:

```bash
ORKA_CONTEXT_TOKEN_TTS_URL=https://kontxt-tts.kontxt-system.svc
ORKA_CONTEXT_TOKEN_TTS_AUDIENCE=orka
ORKA_CONTEXT_TOKEN_TTS_TIMEOUT=5s
ORKA_CONTEXT_TOKEN_TTS_TOKEN_SOURCE=serviceAccount
ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_TYPE=urn:ietf:params:oauth:token-type:txn_token
ORKA_CONTEXT_TOKEN_CHILD_SCOPE=orka:agents:run
ORKA_CONTEXT_TOKEN_OUTBOUND_SCOPE=orka:tools:use
ORKA_CONTEXT_TOKEN_CHILD_TOKEN_TTL=5m
ORKA_CONTEXT_TOKEN_TOOL_TOKEN_TTL=2m
```

These settings do not by themselves grant downstream access; workers still need an appropriate subject token file, which Orka provides automatically only for Tasks with an owner-referenced transaction-token Secret.

## Direct Kubernetes provenance hardening

The REST API rejects client-supplied `spec.requestedBy` and `spec.transaction` and stamps verified provenance itself. To prevent direct Kubernetes API users from spoofing the same fields, enable the optional validating admission webhook:

```bash
ORKA_TASK_PROVENANCE_ADMISSION_ENABLED=true
```

The webhook rejects untrusted creates or updates that set or modify:

- `spec.requestedBy`
- `spec.transaction`
- `orka.ai/transaction-*` labels and annotations
- `orka.ai/context-token-profile`
- `orka.ai/transaction-token-secret`

Trusted controller/API ServiceAccounts and the worker ServiceAccount are configurable with `ORKA_TASK_PROVENANCE_ADMISSION_TRUSTED_USERS` and `ORKA_TASK_PROVENANCE_ADMISSION_TRUSTED_SERVICE_ACCOUNTS`.

## Observability

Context-token metrics use low-cardinality labels only:

| Metric | Labels | Meaning |
|---|---|---|
| `orka_context_token_auth_total` | `profile`, `result` | Context-token authentication successes/failures. |
| `orka_context_token_authorization_total` | `action`, `result`, `reason` | Authorization decisions such as allowed, denied, or audit-mode would-deny. |
| `orka_context_token_tts_exchange_total` | `result`, `reason` | TTS exchange attempts. |
| `orka_context_token_tts_exchange_duration_seconds` | `result`, `reason` | TTS exchange latency. |

Use logs and safe annotations for per-transaction tracing. The CLI can filter Task summaries by transaction ID:

```bash
orka task list --transaction txn-abc123
orka task get my-task --show-transaction
orka audit trace txn-abc123
```

Do not use raw transaction IDs, subjects, repositories, task names, or token values as metric labels.

## Redaction rules

Never commit, log, print, or persist raw TxTokens. Orka redaction covers `Txn-Token` and `Authorization` header values plus token-looking strings in worker/tool output. Keep downstream tools and custom scripts under the same rule: log transaction IDs and digests, not tokens.

## Example least-privilege Task token

A token that can create one agent Task in `default`, bound to a repository/branch and a narrow tool set, would use a scope and context like:

```json
{
  "scope": "orka:tasks:create orka:tools:use",
  "tctx": {
    "namespace": "default",
    "taskType": "agent",
    "agent": "coder",
    "repo": "https://github.com/sozercan/orka",
    "branch": "kontxt",
    "allowedTools": ["file_read", "file_write"]
  }
}
```

Start in `audit` mode to observe missing scopes and context mismatches, then switch to `enforce` once callers consistently issue least-privilege TxTokens.
