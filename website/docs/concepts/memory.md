---
slug: /memory
---

# Memory

Orka provides namespace-scoped durable memory for AI worker tasks. The design is governance-first: workers may propose reusable knowledge, but durable shared memory remains reviewable, auditable, bounded, and subject to an explicit content authority.

## Concepts and authority

| Concept | Purpose | Authority and persistence |
|---------|---------|---------------------------|
| **Durable memory** | Project facts, decisions, conventions, or reusable context for future tasks | SQLite content for namespaces that have never activated a remote backend; after cutover, the OMS provider is content-authoritative while Orka retains governance and audit state |
| **Memory proposal** | Worker- or user-submitted suggestion for memory, policy, workflow, or skill changes | Orka's durable governance store; review does not apply the proposal |
| **Memory operation** | Durable create, replace, delete, or proposal-apply work against a remote backend | Orka's operation and idempotency ledgers, with provider receipts and retry state |
| **Transcript search** | Compact search over prior session messages | SQLite session transcript tables |

A namespace has exactly one effective memory content authority:

- **Legacy authority:** if a remote backend has never been activated, existing SQLite memory behavior is preserved.
- **Staged backend:** `MemoryBackend/default` may validate endpoint, Secret, store, identity, and capabilities without changing SQLite authority.
- **Remote authority:** activation performs a durable cutover. The remote OMS store becomes authoritative for content; Orka remains authoritative for canonical IDs, trust, provenance, tags, generations, tombstones, operations, and audit state.
- **No automatic fallback:** after activation, backend deletion, invalid configuration, identity mismatch, or an outage fails closed. SQLite does not silently become authoritative again.
- **Explicit restore:** legacy SQLite can resurface only through the audited `restore-legacy` workflow after a clean decommission. `force-orphan` leaves the namespace in fail-closed `Removed` state.

### Backend lifecycle matrix

`spec.lifecycleState` is requested intent. `status.effectiveLifecycleState` reports the durable state after validation, drain, routing-fence, or recovery barriers complete.

| Effective state | Content authority | Reads / recall | New remote mutations |
|-----------------|-------------------|----------------|----------------------|
| `Staged` | SQLite | Available from legacy SQLite | Legacy behavior only; no remote cutover |
| `Validating` | Previous authority | Fail closed for a pending remote cutover | Blocked until validation and binding commit |
| `Active` | Remote OMS backend | Available when binding validation is fresh | Create, update, delete, and proposal apply allowed |
| `Draining` | Remote OMS backend | Depends on the requested transition | New work blocked while admitted operations settle |
| `ReadOnly` | Remote OMS backend | Available | Create, update, and proposal apply blocked; safety controls remain available |
| `Disabled` | Remote OMS backend remains selected | Suppressed | Blocked and routing-fenced |
| `Recovering`, `IdentityMismatch`, `IdentityConflict`, `Diverged` | Remote binding retained | Fail closed or locally suppressed | Blocked until audited recovery succeeds |
| `Decommissioning` | Remote binding retained during convergence | Fail closed | Blocked |
| `Decommissioned` | No active remote service | Unavailable until an explicit next action | Blocked; clean `restore-legacy` may be previewed/applied |
| `Removed` | Remote ownership orphaned and fenced locally | Unavailable | Blocked; SQLite is not reactivated |

## Trust and governance

Every memory has server-owned trust state:

| Trust | Meaning | Normal recall/search eligibility |
|-------|---------|----------------------------------|
| `untrusted` | Directly created or not yet promoted | Excluded by default |
| `reviewed` | Materialized from an accepted memory proposal with proposal provenance | Included |
| `trusted` | Explicitly promoted by an authorized operator | Included |

Passive recall and `recall_memory` request only `reviewed` and `trusted` records. Disabled, deleted, tombstoned, diverged, wrong-binding, and otherwise ineligible records are suppressed locally even if a provider returns them.

Memory proposal review is **non-applying**:

1. New proposals start as `pending`.
2. Review records `accepted` or `rejected`; it does not create memory.
3. An accepted proposal with `type: "memory"` requires a separate apply request.
4. Apply may complete immediately or return a durable remote memory operation.
5. Rejected or archived proposals cannot be applied.

Accepted proposals can suggest tags through the first `Tags:` line in `description`:

```text
Reusable release procedure discovered during task execution.
Tags: release, testing
```

`remember` and `propose_memory` always create proposals. Model output never mutates durable shared memory directly.

## Worker behavior

When controller context is present, the AI worker registers and auto-enables the memory tool family independently of coordination mode:

| Tool | Purpose |
|------|---------|
| `recall_memory` | Strict keyword search over eligible namespace memories by text, tags, provenance, and limit |
| `remember` | Submit a durable memory proposal for review |
| `propose_memory` | Submit a memory-adjacent governance proposal such as memory, policy, workflow, or skill content |
| `search_transcript` | Search prior session transcripts and return compact snippets |

### Remote-search authorization

Remote search is outbound data egress. `recall_memory` forwards the task's mounted transaction token from `ORKA_TRANSACTION_TOKEN_FILE` in the `Txn-Token` header. The controller must validate the task-scoped `orka:memory:search:remote` scope, memory-read authorization, and any configured approval policy before provider egress. If the token is configured but unreadable, the tool fails closed instead of silently downgrading to ServiceAccount-only authorization.

Legacy SQLite search does not perform provider egress, but the same tool response remains a normalized JSON array of Memory objects.

### Synthetic lower-trust passive context

Before the first model request, the worker may load a bounded, deterministic top-N set of `reviewed` and `trusted` memories. The content is **not** concatenated into the system or user prompt. It is inserted as a synthetic assistant tool call plus tool result named `orka_passive_memory`:

- the provider receives a matching tool declaration so OpenAI- and Anthropic-compatible APIs can validate the historical tool exchange;
- the declaration is a context carrier only and is excluded from executable tool authorization;
- the actual system policy independently states that passive memory is lower-trust, untrusted data;
- embedded instructions cannot authorize tools, approvals, secret access, or external transmission;
- if the model attempts to call the synthetic tool, the worker rejects it as not enabled.

Passive memory is best effort. Failure to fetch it does not prevent the task from running.

### Memory context bounds

| Setting | Default | Purpose |
|---------|---------|---------|
| `ORKA_MEMORY_CONTEXT_LIMIT` | `5` | Maximum memories selected for passive context |
| `ORKA_MEMORY_CONTEXT_MAX_CHARS` | `6000` | Maximum formatted passive-memory content before tool-result encoding |

The worker also truncates individual entries and caps the encoded synthetic tool result.

## Public memory API

Public routes are under `/api/v1` and use the server's configured authentication. Context-token callers need the corresponding memory scopes; backend configuration and lifecycle operations additionally require a TokenReview-authenticated Kubernetes identity and `memorybackends` RBAC.

### Memories and search

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/memories` | GET | Deterministically list memories; legacy `query`/`q` compatibility may invoke keyword search under remote authority |
| `/api/v1/memories` | POST | Create memory |
| `/api/v1/memories/search` | POST | Explicit bounded `keyword`, `semantic`, `hybrid`, or `auto` search |
| `/api/v1/memories/:id` | GET | Get one memory; disabled content requires `includeDisabled=true` and memory-operate scope |
| `/api/v1/memories/:id` | PUT | Replace mutable memory fields |
| `/api/v1/memories/:id` | DELETE | Delete memory or install a remote tombstone |
| `/api/v1/memories/:id/disable` | POST | Disable normal recall locally |
| `/api/v1/memories/:id/enable` | POST | Re-enable normal recall locally |
| `/api/v1/memories/:id/trust` | POST | Audited trust transition |

Common list filters include `namespace`, `query`/`q`, `sessionName`, `agentName`, `taskName`, `parentTask`, `source`, `tags`, `ids`, `trust`, `includeDisabled`, `includeDeleted`, `cursor`, and `limit`.

Exact GET keeps disabled content suppressed unless an authorized operator explicitly sets `includeDisabled=true`; context-token callers need both memory-read and memory-operate scopes for that inspection.

Explicit search accepts `query`, `tags`, `ids`, provenance filters, `trust`, `limit`, `cursor`, `mode`, `allowIncomplete`, `includeDisabled`, and `includeDeleted`. A successful response contains:

```json
{
  "items": [{"memory": {"id": "mem-example", "content": "..."}, "score": 0.91}],
  "actualMode": "keyword",
  "cursor": "opaque-next-page-cursor",
  "exhausted": false,
  "complete": true
}
```

`semantic` and `hybrid` fail with `422 MEMORY_SEARCH_MODE_UNSUPPORTED` when the active backend lacks the capability. Only `auto` may downgrade. Strict mode is the default; callers must opt into partial results with `allowIncomplete: true`.

### Mutation status and idempotency

Remote create, update, delete, and proposal apply require an `Idempotency-Key` of at most 256 characters. Reuse the same key only for an exact retry of the same logical request. Reusing it with different input returns `409 MEMORY_IDEMPOTENCY_KEY_REUSE`.

| Outcome | Status | Body and headers |
|---------|--------|------------------|
| Legacy create | `201 Created` | Materialized Memory JSON |
| Legacy update | `200 OK` | Materialized Memory JSON |
| Legacy delete | `204 No Content` | Empty body |
| Remote mutation acknowledged immediately | `201`, `200`, or `204` matching the operation | Materialized Memory JSON or empty delete response |
| Remote mutation queued, retrying, or acknowledgement-ambiguous | `202 Accepted` | MemoryOperation JSON plus `Location` and `Retry-After` |
| Backend/capacity temporarily unavailable | `429` or `503` | Structured reason and, when applicable, `Retry-After` |
| Missing remote idempotency key | `428 Precondition Required` | `MEMORY_IDEMPOTENCY_KEY_REQUIRED` |

Polling `Location` returns the durable operation. Non-terminal states include `queued`, `leased`, `dispatching`, and `ambiguous`; terminal states include `succeeded`, `dead_lettered`, `abandoned`, `superseded`, and `orphaned`.

Example remote-safe create:

```bash
TOKEN=$(kubectl create token orka-client -n orka-system)

curl -i -X POST http://localhost:8080/api/v1/memories \
  -H "Authorization: Bearer $TOKEN" \
  -H "Idempotency-Key: memory-create-release-checklist-v1" \
  -H "Content-Type: application/json" \
  -d '{
    "namespace": "orka-system",
    "source": "user",
    "content": "Release tasks should run make lint-fix and make test before merging.",
    "tags": ["release", "testing"]
  }'
```

### Operations

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/memory-operations` | GET | List durable operations by namespace, state, cursor, and limit |
| `/api/v1/memory-operations/:id` | GET | Get one operation; active work includes `Retry-After` |
| `/api/v1/memory-operations/:id/retry` | POST | Audited manual retry |
| `/api/v1/memory-operations/:id/abandon` | POST | Submit audited abandonment evidence; safe completion requires verified non-application and fencing |

### Backends

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/memory-backends` | GET / POST | List or create fixed-name `MemoryBackend/default` |
| `/api/v1/memory-backends/default` | GET / PUT / DELETE | Inspect, update, or delete the namespace backend |
| `/api/v1/memory-backends/default/status` | GET | Read bounded effective status |
| `/api/v1/memory-backends/default/activate` | POST | Request validated durable cutover |
| `/api/v1/memory-backends/default/decommission` | POST | Start or preview clean decommission |
| `/api/v1/memory-backends/default/force-orphan` | POST | Fence locally and orphan unresolved remote state |
| `/api/v1/memory-backends/default/restore-legacy` | POST | Preview or explicitly restore archived SQLite authority after clean decommission |
| `/api/v1/memory-backends/default/checkpoints` | POST | Record a matched activation receipt or verified runtime checkpoint |
| `/api/v1/memory-backends/default/purge` | POST | Purge explicitly selected, checkpoint-covered local retention state |

Administrative writes require an audit `reason`; destructive/lifecycle CLI commands also require explicit confirmation.

### Proposals

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/memory-proposals` | GET / POST | List or create proposals |
| `/api/v1/memory-proposals/:id` | GET | Get a proposal |
| `/api/v1/memory-proposals/:id/review` | POST | Record an accepted/rejected decision without applying |
| `/api/v1/memory-proposals/:id/apply` | POST | Apply an accepted memory proposal; may return Memory JSON or `202 MemoryOperation` |
| `/api/v1/memory-proposals/:id/archive` | POST | Archive without applying |

Review and archive return `204 No Content`. For context-token callers, review requires memory-operate; apply requires both memory-write and memory-operate. Proposal apply participates in the same remote idempotency and operation behavior as other mutations.

## Internal worker routes

Workers use namespace-in-path routes under `/internal/v1`:

- `GET|POST /internal/v1/memories/:namespace`
- `POST /internal/v1/memories/:namespace/search`
- `GET|PUT|DELETE /internal/v1/memories/:namespace/:id`
- `POST /internal/v1/memories/:namespace/:id/disable|enable`
- `GET /internal/v1/memory-operations/:namespace[/:id]`
- `GET|POST /internal/v1/memory-proposals/:namespace`
- `GET /internal/v1/memory-proposals/:namespace/:id`
- `POST /internal/v1/memory-proposals/:namespace/:id/review|apply|archive`

Internal routes are not a weaker authorization path. They always validate the Kubernetes workload and namespace. When context-token authorization is enabled, task transaction context, required memory scopes, and remote-search approval are also validated server-side.

## CLI

The CLI exposes:

- `orka memory create|update|delete|proposal apply --idempotency-key ... [--wait --wait-timeout 5m]`
- `orka memory operation list|get|retry|abandon`
- `orka memory backend list|get|status|create|update|delete|activate|decommission|force-orphan|restore-legacy`

When omitted, the CLI generates a mutation idempotency key and prints it on request or wait failure so the same request can be retried safely. `--wait` follows `Location` and honors bounded `Retry-After` until terminal completion or timeout. See the generated [CLI Command Reference](../reference/cli-commands.md) for every flag.
