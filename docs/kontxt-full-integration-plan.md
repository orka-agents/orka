# kontxt Full Integration Plan for Orka

## Purpose

This document describes how to move Orka from **kontxt-compatible token verification** to a **full kontxt TxToken integration** across Orka's API, Kubernetes Tasks, workers, multi-agent delegation, audit trail, and live CI.

The current PR gives Orka the first important piece: it can consume upstream-compatible `kontxt` TxTokens at API ingress. Full integration means Orka should also preserve transaction identity, authorize operations from signed token context, issue or request narrower child tokens for delegated work, and make the same transaction traceable across the whole workflow.

## Upstream kontxt model to align with

Upstream kontxt describes TxTokens as a way to unlock three main capabilities:

1. **Request-level authorization across service boundaries**
   - The token carries signed request context (`tctx`) and requester context (`rctx`).
   - Downstream services can make authorization decisions using sealed context instead of trusting ad-hoc headers.

2. **Immutable delegation chains**
   - A service can replace an incoming token with a shorter-lived, narrower-scoped TxToken for the next hop.
   - Scope should only shrink through delegation.
   - The transaction remains bound to the original request.

3. **End-to-end audit correlation**
   - The `txn` claim identifies the transaction.
   - Every hop can preserve the same transaction ID so logs, traces, tasks, and downstream calls can be correlated.

Relevant upstream references:

- `https://github.com/aramase/kontxt#what-txtokens-unlock`
- `https://github.com/aramase/kontxt/blob/main/pkg/token/types.go`
- `https://github.com/aramase/kontxt/blob/main/sdk/tts/client.go`
- `https://github.com/aramase/kontxt/blob/main/sdk/verify/verifier.go`
- `https://github.com/aramase/kontxt/blob/main/pkg/tts/server.go`

## Current Orka state

Orka currently has a strong Phase 1 foundation:

- Accepts kontxt TxTokens from the `Txn-Token` header.
- Supports the upstream JWT type `txntoken+jwt`.
- Verifies RS256 JWT signatures via JWKS.
- Validates configured issuer and audience.
- Validates time claims through existing JWT verification flow.
- Requires core kontxt claims:
  - `iat`
  - `txn`
  - `scope`
  - `req_wl`
- Parses:
  - `txn`
  - `scope`
  - `req_wl`
  - `tctx`
  - `rctx`
- Stores the parsed token on request-local `UserInfo.ContextToken`.
- Maps `scope` values to `UserInfo.Roles`.
- Stamps verified OIDC/context-token identity into immutable `Task.spec.requestedBy` during REST Task creation.
- Rejects client-supplied `requestedBy` fields on the REST API.
- Has live CI coverage for valid kontxt Task creation and tampered-token rejection.

Current Orka capabilities that are adjacent to full kontxt integration:

- Native parent/child Task delegation.
- Coordinator agents that call `delegate_task` / `wait_for_tasks`.
- Delegation guardrails:
  - allowed agents
  - max depth
  - max concurrent children
  - owner references
  - parent/child labels and annotations
- Kubernetes Jobs and Pods per Task.
- Structured logs, result storage, and Prometheus metrics.
- Kubernetes-native CRDs that can be annotated/labeled for transaction correlation.

## Current gaps

Orka is not yet fully integrated with kontxt because:

1. **Transaction metadata is request-local only**
   - `txn`, `req_wl`, `tctx`, and `rctx` are parsed but not persisted as first-class Task/Job/Pod metadata.
   - Operators cannot reliably correlate Orka Tasks, Jobs, Pods, logs, and downstream calls by `txn`.

2. **`scope` and `tctx` are not yet authorization inputs**
   - Orka authenticates the TxToken and records identity.
   - Orka does not yet enforce operation-level permissions from token `scope`.
   - Orka does not yet constrain Task fields from signed `tctx` values such as namespace, agent, repo, branch, model, or allowed tools.

3. **Delegated Tasks do not receive narrowed TxTokens**
   - Orka's `delegate_task` tool creates child Kubernetes Tasks directly.
   - Child Tasks inherit Orka coordination structure, but not a cryptographically narrowed kontxt token.
   - The delegation chain is Kubernetes-native, not TxToken-native.

4. **Workers do not propagate TxTokens to downstream services**
   - Worker outbound HTTP/tool calls do not automatically attach `Txn-Token`.
   - There is no generic token exchange/replacement before tool calls.

5. **Direct Kubernetes CRD creation bypasses REST stamping**
   - The REST API rejects `requestedBy` tampering.
   - Users with Kubernetes RBAC can still create `Task` CRs directly.
   - CRD immutability prevents later changes, but does not prove the initial provenance was Orka-stamped.

6. **No live TTS-based E2E yet**
   - Current live CI generates an upstream-compatible token and validates Orka's verifier path.
   - It does not deploy real kontxt TTS, perform token exchange, propagate tokens through child work, or test scope narrowing.

## Definition of full integration

Orka should be considered fully integrated when this statement is true:

> A request enters Orka with a kontxt TxToken; Orka validates and authorizes it using issuer, audience, signature, time claims, `scope`, and `tctx`; Orka persists safe transaction metadata; Orka propagates the same `txn` through parent/child Tasks, Jobs, Pods, logs, and worker context; Orka obtains narrower replacement TxTokens for child agents and downstream tool calls; and downstream services can independently verify why they were called by validating the propagated TxToken.

## Target architecture

```text
External caller / GitHub Actions / user workflow
  │
  │  OIDC token, access token, or existing TxToken
  ▼
kontxt TTS
  │
  │  TxToken: txn=A, scope=orka:tasks:create orka:agents:delegate, tctx={...}
  ▼
Orka REST API
  │
  ├─ validate TxToken signature, issuer, audience, type, time, required claims
  ├─ authorize API action from scope + tctx
  ├─ stamp immutable requester identity
  ├─ persist safe transaction metadata
  ▼
Task CR
  │
  ├─ spec.requestedBy: verified subject/issuer/roles
  ├─ spec.transaction: txn, req_wl, scope, context digest, selected allowlisted context
  ├─ labels/annotations: safe transaction correlation values
  ▼
Controller
  │
  ├─ copies transaction metadata to Job/Pod
  ├─ enforces coordination guardrails
  ▼
Worker Pod
  │
  ├─ receives transaction metadata
  ├─ requests narrower TxToken from kontxt TTS for each child delegation or tool call
  ├─ attaches Txn-Token to outbound calls
  ▼
Child Task / downstream service
  │
  ├─ validates child TxToken independently
  ├─ sees same txn=A and narrower scope
  └─ logs/audits same transaction ID
```

## Workstream 1: Persist safe transaction metadata

### Goal

Make kontxt transaction identity durable and queryable across Orka resources without storing raw tokens or sensitive context.

### Proposed API model

Add an immutable transaction/provenance block to `TaskSpec` or a controller-owned equivalent location.

Possible shape:

```yaml
spec:
  requestedBy:
    subject: spiffe://example.test/ns/default/sa/client
    issuer: https://kontxt-tts.example.test
    username: spiffe://example.test/ns/default/sa/client
    roles:
      - orka:tasks:create
      - orka:agents:delegate
  transaction:
    profile: kontxt
    id: txn-abc123
    issuer: https://kontxt-tts.example.test
    audience:
      - orka
    subject: spiffe://example.test/ns/default/sa/client
    requestingWorkload: spiffe://example.test/ns/default/sa/client
    scope: "orka:tasks:create orka:agents:delegate"
    scopes:
      - orka:tasks:create
      - orka:agents:delegate
    contextDigest: sha256:...
    requesterContextDigest: sha256:...
    context:
      purpose: code-review
      namespace: default
      repo: github.com/sozercan/orka
      branch: kontxt
      agent: security-reviewer
```

### Storage rules

- Never store raw TxTokens.
- Never log raw TxTokens.
- Treat `tctx` and `rctx` as potentially sensitive.
- Persist full `tctx` / `rctx` only if explicitly configured.
- Prefer storing:
  - `txn`
  - `req_wl`
  - scope string and parsed scopes
  - token profile
  - issuer/audience/subject
  - digest of full `tctx`
  - digest of full `rctx`
  - configured allowlist of safe `tctx` fields
- Keep transaction metadata immutable once set.

### Resource propagation

Propagate safe transaction fields to:

- `Task.metadata.labels`
- `Task.metadata.annotations`
- `Task.spec.transaction` or equivalent
- `Job.metadata.labels`
- `Job.metadata.annotations`
- `PodTemplate.metadata.labels`
- `PodTemplate.metadata.annotations`
- worker environment variables
- structured controller logs
- structured worker logs

Suggested labels:

```yaml
orka.ai/transaction-id: <safe-label-value-or-hash>
orka.ai/auth-profile: kontxt
orka.ai/requesting-workload-hash: <hash>
```

Suggested annotations:

```yaml
orka.ai/transaction-id: txn-abc123
orka.ai/requesting-workload: spiffe://example.test/ns/default/sa/client
orka.ai/transaction-context-digest: sha256:...
orka.ai/requester-context-digest: sha256:...
orka.ai/transaction-scope: "orka:tasks:create orka:agents:delegate"
```

Use labels only for values that fit Kubernetes label constraints and are safe for indexing. Use annotations for richer values.

### Acceptance criteria

- A Task created with a valid kontxt TxToken has immutable transaction metadata.
- The associated Job and Pod carry the same transaction ID or transaction hash.
- Controller and worker logs include `transactionID` where available.
- No raw token appears in Task, Job, Pod, logs, events, or test output.
- Unit tests cover transaction metadata stamping and immutability.
- Live CI verifies `txn` appears on Task/Job/Pod metadata.

## Workstream 2: Authorize Orka API actions from `scope` and `tctx`

### Goal

Move from TxToken authentication to TxToken authorization.

### Scope model

Define Orka scopes with a stable vocabulary. Suggested initial scopes:

| Scope | Meaning |
|---|---|
| `orka:tasks:create` | Create Tasks through the REST API |
| `orka:tasks:get` | Read a Task |
| `orka:tasks:list` | List Tasks |
| `orka:tasks:cancel` | Cancel Tasks |
| `orka:tasks:delete` | Delete Tasks |
| `orka:agents:run` | Run agent-backed Tasks |
| `orka:agents:delegate` | Create child delegated Tasks |
| `orka:agents:create` | Create Agent CRs through Orka tools/API |
| `orka:agents:delete` | Delete Agent CRs through Orka tools/API |
| `orka:tools:use` | Use configured tools |
| `orka:tools:http` | Use external HTTP tools |
| `orka:memory:read` | Read memory/session context |
| `orka:memory:write` | Create memory proposals or writes |
| `orka:workspace:read` | Use a read-only workspace |
| `orka:workspace:write` | Push workspace changes |
| `orka:providers:use` | Use configured model providers |
| `orka:admin` | Administrative override, if intentionally enabled |

Avoid using broad scopes in examples unless needed. Prefer least privilege.

### API enforcement points

Add authorization checks at REST handlers and internal tool APIs:

- `POST /api/v1/tasks`
- `GET /api/v1/tasks`
- `GET /api/v1/tasks/:name`
- `DELETE /api/v1/tasks/:name`
- cancel endpoints
- chat/orchestrator task creation
- OpenAI-compatible task/session creation if it creates Orka Tasks
- Anthropic-compatible task/session creation if it creates Orka Tasks
- memory handlers
- artifact handlers if they expose task data
- internal tool endpoints where external auth applies

### `tctx` constraints

Use signed `tctx` to constrain request-specific values. Example:

```json
{
  "purpose": "code-review",
  "namespace": "default",
  "taskType": "agent",
  "agent": "security-reviewer",
  "repo": "github.com/sozercan/orka",
  "branch": "kontxt",
  "maxDepth": 2,
  "allowedTools": ["file_read", "web_search"],
  "allowedProviders": ["openai", "anthropic"],
  "allowedModels": ["gpt-5.4", "claude-sonnet"],
  "allowWorkspaceWrite": false
}
```

Reject a request when the body violates `tctx`, for example:

- request namespace differs from `tctx.namespace`
- requested task type differs from `tctx.taskType`
- requested agent differs from `tctx.agent` or is not in `tctx.allowedAgents`
- workspace repo differs from `tctx.repo`
- branch/ref differs from `tctx.branch` / `tctx.ref`
- requested tools are not a subset of `tctx.allowedTools`
- provider/model is not allowed
- delegation depth exceeds `tctx.maxDepth`
- write-capable workspace is requested when `allowWorkspaceWrite=false`

### Policy configuration

Add configurable policy instead of hardcoding all semantics.

Possible config:

```yaml
contextTokenAuthorization:
  enabled: true
  defaultDecision: deny
  scopeAliases:
    read: ["orka:tasks:get", "orka:tasks:list"]
    write: ["orka:tasks:create"]
  tctxAllowlist:
    - purpose
    - namespace
    - taskType
    - agent
    - allowedAgents
    - repo
    - branch
    - ref
    - maxDepth
    - allowedTools
    - allowedProviders
    - allowedModels
  actions:
    createTask:
      requiredScopes: ["orka:tasks:create"]
      constraints:
        namespace: tctx.namespace
        taskType: tctx.taskType
        agent: tctx.agent
        repo: tctx.repo
```

### Acceptance criteria

- Valid token with required scope can perform the operation.
- Valid token without required scope receives `403`.
- Valid token with mismatched `tctx` receives `403`.
- ServiceAccount and OIDC behavior remains backward-compatible unless kontxt authorization is explicitly required.
- Authorization failures log safe fields only: action, issuer, subject, txn, missing scope, violated field.
- Tests cover allow, missing scope, mismatched namespace, mismatched agent, mismatched repo, and mixed OIDC/context-token setups.

## Workstream 3: Integrate kontxt TTS for token exchange

### Goal

Allow Orka to obtain TxTokens from kontxt TTS instead of only verifying caller-supplied TxTokens.

### Use cases

1. **External exchange before Orka**
   - Caller exchanges OIDC/access token at TTS.
   - Caller sends TxToken to Orka.
   - Orka only verifies.

2. **Orka-assisted exchange**
   - Caller sends OIDC/GitHub OIDC token to Orka.
   - Orka exchanges token with TTS.
   - Orka validates and uses returned TxToken internally.

3. **Delegation replacement**
   - Parent Task has TxToken or persisted transaction context.
   - Orka asks TTS for a child TxToken with narrower scope and updated context.
   - Child Task/worker receives the child token or a reference to retrieve it.

4. **Outbound tool replacement**
   - Worker asks TTS for a narrow token for a specific outbound call.
   - Worker sends `Txn-Token` to downstream service.

### TTS client configuration

Add optional controller/worker config:

```bash
ORKA_CONTEXT_TOKEN_TTS_URL=https://kontxt-tts.kontxt-system.svc
ORKA_CONTEXT_TOKEN_TTS_AUDIENCE=orka
ORKA_CONTEXT_TOKEN_TTS_TIMEOUT=5s
ORKA_CONTEXT_TOKEN_TTS_TOKEN_SOURCE=serviceAccount|incoming|none
ORKA_CONTEXT_TOKEN_TTS_DEFAULT_CHILD_TTL=5m
ORKA_CONTEXT_TOKEN_TTS_DEFAULT_TOOL_TTL=2m
```

Possible CLI flags:

```bash
--context-token-tts-url
--context-token-tts-audience
--context-token-tts-timeout
--context-token-tts-token-source
--context-token-child-token-ttl
--context-token-tool-token-ttl
```

### Token source choices

| Source | Description | Tradeoff |
|---|---|---|
| `incoming` | Use incoming caller token as subject token for exchange | Best continuity, requires safely handling incoming token only in memory |
| `serviceAccount` | Use Orka controller/worker ServiceAccount identity | Simpler operationally, weaker end-user continuity unless `rctx` records original identity |
| `none` | Disable Orka-initiated exchanges | Verification-only mode |

### Token handling rules

- Do not persist raw returned TxTokens in Task spec/status.
- Prefer short-lived in-memory use.
- If a child worker needs a token after scheduling, pass it via Kubernetes Secret with owner reference and short TTL cleanup, or have the worker perform exchange at startup.
- Redact tokens in logs and events.
- Hash tokens only if needed for cache keys.

### Acceptance criteria

- Orka can exchange an OIDC/access token for a TxToken when configured.
- Orka can replace a parent TxToken with a narrower child TxToken.
- Exchange errors fail closed when kontxt authorization is required.
- Token exchange requests include correct `tctx` and `rctx` mapping.
- Tests use a fake TTS server and verify request fields.
- Live CI can optionally deploy real TTS and perform exchange.

## Workstream 4: Propagate TxTokens into workers and outbound calls

### Goal

Make worker-originated calls carry verifiable transaction context.

### Worker environment

Pass safe transaction metadata to workers:

```bash
ORKA_CONTEXT_TOKEN_PROFILE=kontxt
ORKA_TRANSACTION_ID=txn-abc123
ORKA_TRANSACTION_SUBJECT=spiffe://example.test/ns/default/sa/client
ORKA_TRANSACTION_REQUESTING_WORKLOAD=spiffe://example.test/ns/default/sa/client
ORKA_TRANSACTION_SCOPE="orka:agents:run"
ORKA_TRANSACTION_CONTEXT_DIGEST=sha256:...
ORKA_CONTEXT_TOKEN_TTS_URL=https://kontxt-tts.kontxt-system.svc
```

Do not pass raw parent TxToken via environment unless explicitly accepted as a risk. Prefer worker-side TTS exchange using ServiceAccount identity plus persisted transaction metadata, or a mounted Secret with tight owner reference and cleanup.

### Propagation library

Add a small internal library for workers:

```go
type TransactionContext struct {
    ID string
    Subject string
    RequestingWorkload string
    Scope []string
    Context map[string]any
}

type TxTokenProvider interface {
    TokenFor(ctx context.Context, operation OperationContext) (string, error)
}
```

The provider can be:

- no-op when TxToken propagation is disabled
- static for tests
- TTS-backed for production

### Outbound call integration points

Attach `Txn-Token` for:

- Orka REST/internal API calls from workers
- external HTTP tools
- custom Tool CRD calls
- MCP/tool-server calls when HTTP transport is used
- GitHub helper calls if a gateway/downstream service expects TxTokens
- child Task creation if moved through Orka API

### Operation-specific `tctx`

Each outbound token should include operation context, for example:

```json
{
  "parentTask": "parent-abc",
  "childTask": "child-def",
  "tool": "create_pull_request",
  "operation": "github.pr.create",
  "repo": "github.com/sozercan/orka",
  "branch": "kontxt",
  "purpose": "autonomous-dev-workflow"
}
```

### Acceptance criteria

- Worker can request or receive a TxToken for an outbound operation.
- HTTP tools attach `Txn-Token` when configured.
- Downstream mock service can verify the propagated token.
- Scope is narrowed per operation.
- Tokens are not printed by tool logs, errors, or debug output.

## Workstream 5: Make Orka delegation TxToken-native

### Goal

Make Orka's multi-agent delegation preserve kontxt transaction semantics cryptographically.

### Current delegation model

Current Orka delegation already creates child Tasks with:

- owner reference to parent Task
- `orka.ai/parent-task` label
- `orka.ai/delegated-agent` label
- coordination depth annotation
- controller validation against allowed agents, max depth, and concurrency

This remains valuable and should stay.

### Desired TxToken-native model

For every delegated child Task:

1. Preserve the same `txn`.
2. Preserve or reference the initiating subject.
3. Add parent/child Task identifiers to child `tctx`.
4. Narrow scope to the minimum needed by the child.
5. Encode delegation depth and target agent in `tctx`.
6. Ensure child token expiry is short.
7. Ensure downstream services can verify child context independently.

Example child token context:

```json
{
  "txn": "txn-abc123",
  "parentTask": "coordinator-1",
  "childTask": "coordinator-1-child-xyz",
  "delegationDepth": 1,
  "agent": "backend-dev",
  "purpose": "implement-auth-change",
  "repo": "github.com/sozercan/orka",
  "branch": "kontxt",
  "allowedTools": ["file_read", "file_write", "code_exec"],
  "allowedScopes": ["orka:workspace:write", "orka:tools:use"]
}
```

### Scope narrowing rules

- Child scopes must be a subset of parent scopes, or must be derivable by a configured scope mapping.
- Child may never gain broader workspace/model/tool access than parent.
- If parent lacks `orka:agents:delegate`, `delegate_task` must fail.
- If parent `tctx.allowedAgents` exists, child agent must be in that list.
- If parent `tctx.maxDepth` exists, Orka's depth must not exceed it.

### Implementation options

#### Option A: `delegate_task` calls Orka REST API

Instead of creating child Tasks directly with Kubernetes client, worker calls Orka REST API with a TxToken.

Pros:

- Reuses API auth/stamping/authorization.
- Same path for external and internal Task creation.
- Easier to ensure provenance consistency.

Cons:

- Requires worker to know Orka API endpoint and have token exchange/propagation.
- More network dependency.

#### Option B: `delegate_task` keeps Kubernetes client but requests child token

The tool creates child Task directly and attaches transaction metadata after obtaining child TxToken/context from TTS.

Pros:

- Less disruptive to existing architecture.
- Maintains current in-cluster controller-runtime flow.

Cons:

- Duplicates API stamping/authorization logic.
- Harder to prevent drift between REST and direct-Kubernetes paths.

#### Recommended direction

Use **Option A** for long-term integrity. Keep Option B only as a compatibility path while migrating.

### Acceptance criteria

- A parent Task with TxToken can delegate a child Task that carries the same transaction ID.
- Child token has narrower scope than parent.
- Child Task metadata records parent transaction and child transaction context digest.
- Delegation fails if requested child scope exceeds parent scope.
- Delegation fails if `tctx.allowedAgents` excludes the requested agent.
- Delegation fails if `tctx.maxDepth` would be exceeded.
- Live E2E proves parent and child are linked by `txn` and child cannot broaden privileges.

## Workstream 6: Admission and direct Kubernetes hardening

### Goal

Prevent direct Kubernetes API writes from spoofing Orka-managed provenance.

### Problem

The REST API can reject `requestedBy` tampering and stamp verified metadata, but a Kubernetes user with `create tasks` permission could create a Task directly. The CRD can make `requestedBy` immutable after creation, but cannot prove the creator was authenticated by Orka's REST API.

### Options

#### Option 1: Validating admission webhook

Reject Task creates/updates that set Orka-managed provenance fields unless the request comes from the Orka API/controller ServiceAccount.

Managed fields:

- `spec.requestedBy`
- `spec.transaction`
- transaction labels/annotations

Pros:

- Strongest Kubernetes-native enforcement.
- Protects all clients, not only REST clients.

Cons:

- Adds webhook deployment/availability considerations.

#### Option 2: Move verified provenance to status

Keep request-owned fields in spec, but write verified provenance only to `status` from the controller/API server.

Pros:

- Status is naturally controller-owned.
- Avoids users setting verified provenance in spec.

Cons:

- Status updates happen after create.
- Less useful for scheduling-time policy unless controller blocks unverified Tasks early.

#### Option 3: RBAC guidance only

Document that direct Task creation should be restricted and clients should use Orka API.

Pros:

- Simple.

Cons:

- Not full integrity.

### Recommended direction

Use **Option 1 + selected status fields**:

- Admission webhook protects provenance fields.
- Status can expose controller-observed verification state.
- RBAC docs recommend external clients use Orka API instead of direct CRD writes.

### Acceptance criteria

- Direct create with client-supplied `spec.requestedBy` is rejected unless caller is Orka's trusted ServiceAccount.
- Direct create with client-supplied `spec.transaction` is rejected unless caller is trusted.
- Direct update of provenance fields is rejected.
- REST API path still works.
- Controller-owned status updates still work.

## Workstream 7: Observability, audit, and redaction

### Goal

Make TxToken-backed workflows debuggable and auditable without leaking secrets.

### Structured log fields

Add safe fields to API/controller/worker logs:

```text
authType=contextToken
contextTokenProfile=kontxt
transactionID=txn-abc123
requestingWorkload=spiffe://...
subject=spiffe://...
issuer=https://...
task=...
namespace=...
parentTask=...
childTask=...
delegationDepth=...
action=createTask
```

Do not log:

- raw TxToken
- authorization header
- full unreviewed `tctx`
- full unreviewed `rctx`
- secrets or provider credentials

### Metrics

Potential metrics:

- `orka_context_token_auth_total{profile,result}`
- `orka_context_token_authorization_total{action,result,reason}`
- `orka_context_token_tts_exchange_total{result,reason}`
- `orka_context_token_tts_exchange_duration_seconds`
- `orka_transaction_tasks_total{profile}`

Avoid high-cardinality labels like raw `txn`, subject, repo, or task name. Use logs/annotations for those.

### Audit views

Add CLI/UI support later:

```bash
orka tasks list --transaction txn-abc123
orka tasks get <task> --show-transaction
orka audit trace txn-abc123
```

UI could show:

- transaction ID
- requester
- requesting workload
- parent/child Task tree
- scopes used
- authorization decisions
- downstream tool calls if recorded

### Acceptance criteria

- Operators can trace a transaction from Orka API request to Task to Job to Pod to worker logs.
- Metrics show validation/authorization/TTS exchange health.
- Redaction tests cover TxToken headers and token-looking values.

## Workstream 8: Configuration and deployment docs

### Goal

Make the integration deployable and understandable.

### Orka configuration examples

Verification-only mode:

```bash
ORKA_CONTEXT_TOKEN_PROFILE=kontxt
ORKA_CONTEXT_TOKEN_ISSUER=https://kontxt-tts.example.test
ORKA_CONTEXT_TOKEN_AUDIENCE=orka
ORKA_CONTEXT_TOKEN_HEADERS=Txn-Token
```

Explicit JWKS:

```bash
ORKA_CONTEXT_TOKEN_JWKS_URL=https://kontxt-tts.example.test/.well-known/jwks.json
```

Authorization mode:

```bash
ORKA_CONTEXT_TOKEN_AUTHZ_ENABLED=true
ORKA_CONTEXT_TOKEN_AUTHZ_DEFAULT_DECISION=deny
ORKA_CONTEXT_TOKEN_TCTX_ALLOWLIST=purpose,namespace,agent,allowedAgents,repo,branch,ref,maxDepth,allowedTools,allowedProviders,allowedModels
```

TTS exchange mode:

```bash
ORKA_CONTEXT_TOKEN_TTS_URL=https://kontxt-tts.kontxt-system.svc
ORKA_CONTEXT_TOKEN_TTS_TIMEOUT=5s
ORKA_CONTEXT_TOKEN_TTS_TOKEN_SOURCE=serviceAccount
ORKA_CONTEXT_TOKEN_CHILD_TOKEN_TTL=5m
ORKA_CONTEXT_TOKEN_TOOL_TOKEN_TTL=2m
```

### Documentation to add

- `docs/security.md`
  - authn vs authz modes
  - token redaction rules
  - direct Kubernetes CRD caveats
- `docs/kontxt.md`
  - deployment guide
  - Orka config
  - TTS integration
  - example scopes and `tctx`
- `docs/multi-agent-coordination.md`
  - TxToken-aware delegation chain
  - child token narrowing
  - transaction audit trace
- `README.md`
  - concise capability statement

### Capability statement for README

Avoid overclaiming until all phases land.

Current safe wording:

> Orka can validate upstream-compatible kontxt TxTokens at API ingress and stamp verified requester identity onto Tasks.

After full integration:

> Orka can participate in kontxt transaction-token workflows end-to-end: validating TxTokens at ingress, authorizing Tasks from signed context, preserving transaction IDs across Jobs and delegated agents, and propagating narrower TxTokens to downstream tools and services.

## Workstream 9: Testing and CI

### Unit tests

Add tests for:

- transaction metadata extraction
- safe allowlisted `tctx` persistence
- context digest stability
- no raw token persistence
- scope parser and matcher
- scope aliases
- missing required scope -> `403`
- mismatched `tctx.namespace` -> `403`
- mismatched `tctx.agent` -> `403`
- mismatched `tctx.repo` -> `403`
- child scope subset checks
- TTS exchange request construction
- TTS exchange failure behavior
- worker TxToken provider
- outbound HTTP tool header injection
- admission webhook allow/reject behavior

### Integration tests

Add fake TTS and fake downstream verifier tests:

1. Issue parent token.
2. Create parent Task.
3. Verify transaction metadata.
4. Delegate child Task.
5. Fake TTS returns child token.
6. Verify child metadata preserves `txn` and narrows scope.
7. Worker calls fake downstream service with `Txn-Token`.
8. Fake downstream verifies token and returns success.
9. Broadening attempt fails.

### Live CI expansion

Current live CI should remain as fast verification. Add optional deeper jobs:

#### Live kontxt verifier E2E

Already covered by current PR:

- generate upstream-compatible TxToken
- serve JWKS at `/.well-known/jwks.json`
- create Orka Task with `Txn-Token`
- reject tampered token

#### Live kontxt TTS E2E

New job:

1. Deploy kind cluster.
2. Deploy kontxt TTS.
3. Configure Orka with TTS/JWKS.
4. Exchange GitHub OIDC or test subject token for TxToken.
5. Create parent Task.
6. Verify transaction metadata on Task/Job/Pod.
7. Delegate child Task.
8. Verify child preserves `txn` and has narrower scope.
9. Call mock downstream verifier with child TxToken.
10. Verify mock downstream accepted same transaction ID.
11. Attempt scope broadening and expect failure.
12. Attempt `tctx` mismatch and expect failure.
13. Verify no JWTs or request tokens appear in logs.

### Acceptance criteria

- Existing ServiceAccount/OIDC tests continue passing.
- Verification-only mode remains backward-compatible.
- Authorization mode has deterministic `403` failures.
- TTS outage behavior is explicit and tested.
- Live CI proves both verifier-only and TTS-backed paths.

## Workstream 10: Rollout and compatibility

### Feature gates

Use feature flags to avoid breaking existing users:

| Feature | Default | Rationale |
|---|---:|---|
| context-token verification | off unless configured | existing behavior preserved |
| transaction metadata persistence | on when context-token auth configured | safe and useful |
| context-token authorization | off initially | avoid breaking existing tokens/scopes |
| TTS exchange | off | requires external service |
| worker TxToken propagation | off | requires downstream support |
| admission webhook enforcement | opt-in initially | deployment complexity |

### Migration path

1. Enable verification-only mode.
2. Observe validated context-token requests and transaction metadata.
3. Define scope vocabulary and TTS policies.
4. Enable authorization in warn/audit mode.
5. Fix callers with missing scopes or mismatched context.
6. Switch authorization to enforce mode.
7. Enable TTS-backed child/outbound tokens for selected namespaces.
8. Enable admission enforcement once REST/API path is the standard entry point.

### Audit/warn mode

Before enforcing `scope` and `tctx`, support an audit mode:

```bash
ORKA_CONTEXT_TOKEN_AUTHZ_MODE=audit
```

Behavior:

- Evaluate policy.
- Log would-deny decisions.
- Allow request.
- Expose metrics.

Then switch to:

```bash
ORKA_CONTEXT_TOKEN_AUTHZ_MODE=enforce
```

## Security requirements

- Never commit, log, or print raw TxTokens or credentials.
- Never store raw TxTokens in Task spec/status, annotations, labels, ConfigMaps, or logs.
- Redact `Txn-Token` everywhere request/response data may be printed.
- Treat `tctx` and `rctx` as sensitive by default.
- Persist only allowlisted context fields plus digests.
- Ensure child scopes cannot exceed parent scopes.
- Ensure token exchange fails closed when authorization requires it.
- Keep token TTLs short for child/tool tokens.
- Avoid high-cardinality Prometheus labels.
- Protect admission webhook availability; fail closed for provenance spoofing if enabled.
- Document RBAC expectations for direct Task CRD access.

## Risks and mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| Overclaiming kontxt support | Users expect full delegation/audit semantics before implemented | Use precise docs: verifier-compatible vs full integration |
| Persisting sensitive `tctx` | Leaks request details | Persist digests and allowlisted fields only |
| Token leakage in logs | Credential exposure | Redaction tests and centralized log sanitization |
| Scope vocabulary drift | TTS policies and Orka checks disagree | Define stable Orka scope registry and docs |
| TTS outage breaks tasks | Availability issue | Explicit fail-open/fail-closed config; default fail-closed only when authz requires TTS |
| High-cardinality metrics | Prometheus issues | Do not label metrics by transaction/user/task |
| Direct CRD bypass | Spoofed provenance | Admission webhook and RBAC guidance |
| Child token broader than parent | Privilege escalation | Subset checks plus tests |
| Existing OIDC/SA auth breakage | Regression | Feature gates and compatibility tests |

## Open questions

1. Should transaction metadata live in `spec`, `status`, annotations, or split across them?
2. Which `tctx` fields are safe and useful enough to persist by default?
3. What exact Orka scope vocabulary should be stable for v1?
4. Should `read` / `write` compatibility scopes remain accepted, or should Orka require namespaced scopes like `orka:tasks:create`?
5. Should `delegate_task` migrate fully to Orka REST API, or keep direct Kubernetes creation with duplicated authorization?
6. How should workers obtain child/outbound tokens: mounted short-lived Secret, worker-side TTS exchange, or API-mediated exchange?
7. Should Orka support audit mode for authorization before enforce mode?
8. Should admission enforcement be installed by default or optional?
9. What downstream services/tools will actually verify propagated TxTokens in the first end-to-end demo?
10. Should transaction IDs be exposed in UI by default, or hidden behind an audit/details view?

## Proposed PR sequence

### PR 1: Current verifier integration

Status: in progress / mostly complete.

Scope:

- kontxt TxToken validation.
- `Txn-Token` support.
- immutable `requestedBy` stamping.
- upstream token compatibility test.
- live verifier E2E.

### PR 2: Transaction metadata persistence

Scope:

- Add Task transaction metadata type.
- Stamp `txn`, `req_wl`, scope, digests, allowlisted context.
- Propagate to Job/Pod metadata.
- Add logs with `transactionID`.
- Add tests.

### PR 3: Scope and `tctx` authorization

Scope:

- Define Orka scope vocabulary.
- Add policy config.
- Enforce create/read/cancel/delete as initial actions.
- Enforce namespace/agent/repo constraints for Task creation.
- Add audit mode if desired.
- Add tests.

### PR 4: TTS client integration

Scope:

- Add TTS client config.
- Add fake TTS tests.
- Support exchange/replacement library.
- Do not yet wire every worker tool.

### PR 5: TxToken-aware delegation

Scope:

- Child token request/replacement.
- Same `txn`, narrowed child scope.
- Child transaction metadata.
- Delegation policy checks from parent `scope` / `tctx`.
- Decide whether `delegate_task` calls Orka REST API.

### PR 6: Worker outbound propagation

Scope:

- Worker TxToken provider.
- Attach `Txn-Token` for configured HTTP/tool calls.
- Operation-specific `tctx`.
- Downstream mock verifier tests.

### PR 7: Admission hardening

Scope:

- Validating webhook for Orka-managed provenance fields.
- Trusted ServiceAccount exceptions.
- Docs and tests.

### PR 8: Full live TTS E2E and docs

Scope:

- Deploy real kontxt TTS in kind.
- Exchange token.
- Create parent Task.
- Delegate child Task.
- Verify same `txn` and narrower scope.
- Verify downstream mock receives valid TxToken.
- Docs for deployment and operations.

## Final acceptance checklist

Orka is fully integrated when all of these pass:

- [ ] Orka validates upstream-compatible kontxt TxTokens at ingress.
- [ ] Orka rejects invalid issuer, audience, type, signature, expiry, missing `kid`, missing `txn`, missing `scope`, and missing `req_wl`.
- [ ] Orka authorizes API actions from `scope`.
- [ ] Orka constrains Task creation from signed `tctx`.
- [ ] Orka persists safe transaction metadata without raw tokens.
- [ ] Task, Job, Pod, controller logs, and worker logs can be correlated by transaction ID.
- [ ] Orka obtains or supports narrowed child TxTokens through TTS.
- [ ] Child Tasks preserve the same transaction ID and have no broader scope than parent.
- [ ] Worker outbound calls can propagate operation-specific TxTokens.
- [ ] Downstream mock services can verify propagated TxTokens independently.
- [ ] Direct Kubernetes Task creation cannot spoof Orka-managed provenance fields when admission is enabled.
- [ ] Live CI covers verifier-only and TTS-backed delegation paths.
- [ ] Documentation clearly distinguishes verifier compatibility from full end-to-end integration.
