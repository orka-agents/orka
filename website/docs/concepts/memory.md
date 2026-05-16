---
slug: /memory
---

# Memory

Orka provides a namespace-scoped memory layer for AI worker tasks. It is designed for shared Kubernetes environments where memory should be useful, auditable, and safe by default.

The current model has three related concepts:

| Concept | Purpose | Persistence |
|---------|---------|-------------|
| **Durable memory** | Reviewed project facts, decisions, conventions, or reusable context that can help future tasks | Stored in SQLite as namespace-scoped `memories` records |
| **Memory proposal** | A worker- or user-submitted suggestion for memory, policy, workflow, or skill changes | Stored in SQLite as `memory_proposals` for review |
| **Transcript search** | Compact search over prior session messages | Stored in SQLite session transcript tables |

Memory proposals are **review-only**. Reviewing a proposal as accepted or rejected records the decision but does not automatically create or update durable memory. To create durable memory today, use the durable memory API directly.

## Worker behavior

When an AI worker can reach the controller internal API, it loads reviewed durable memories before invoking the model. The durable memory section is appended to the system prompt as background project context, separate from the current session transcript.

When coordination is enabled on an Agent, Orka auto-injects the memory tool family along with the coordination tools:

| Tool | Purpose |
|------|---------|
| `recall_memory` | Query durable namespace-scoped memories by text, tags, provenance, and limit |
| `remember` | Submit a durable memory proposal for review |
| `propose_memory` | Submit a memory-adjacent governance proposal such as memory, policy, workflow, or skill content |
| `search_transcript` | Search prior session transcripts and return compact snippets |

`remember` and `propose_memory` intentionally submit proposals instead of mutating durable memory directly. This keeps shared memory reviewable and prevents live model output from silently changing future task context.

### Memory context bounds

Durable memory injection is bounded to keep prompts stable:

| Setting | Default | Purpose |
|---------|---------|---------|
| `ORKA_MEMORY_CONTEXT_LIMIT` | `5` | Maximum memories to inject into the worker prompt |
| `ORKA_MEMORY_CONTEXT_MAX_CHARS` | `6000` | Maximum durable-memory prompt section size |

The worker also truncates individual memory entries before prompt injection. Memory infrastructure is best-effort: failure to load memory should not prevent task execution.

## Durable memory API

All public endpoints require a ServiceAccount bearer token and are under `/api/v1`.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/memories` | GET | List durable memories |
| `/api/v1/memories` | POST | Create durable memory |
| `/api/v1/memories/:id` | GET | Get durable memory |
| `/api/v1/memories/:id` | PUT | Update durable memory |
| `/api/v1/memories/:id` | DELETE | Soft-delete durable memory |
| `/api/v1/memories/:id/disable` | POST | Disable memory for normal recall |
| `/api/v1/memories/:id/enable` | POST | Re-enable memory for normal recall |

Supported list query parameters:

| Parameter | Description |
|-----------|-------------|
| `namespace` | Namespace to query |
| `query` or `q` | Text search over memory content and tags |
| `sessionName` | Filter by session provenance |
| `agentName` | Filter by agent provenance |
| `taskName` | Filter by task provenance |
| `parentTask` | Filter by parent task provenance |
| `source` | Filter by source such as `task`, `session`, `user`, `system`, or `e2e` |
| `tags` | Comma-separated tags |
| `ids` | Comma-separated memory IDs |
| `includeDisabled` | Include disabled memories when `true` |
| `includeDeleted` | Include soft-deleted memories when `true` |
| `limit` | Maximum rows to return |

Example:

```bash
TOKEN=$(kubectl create token orka-client -n orka-system)

curl -sS http://localhost:8080/api/v1/memories \
  -H "Authorization: Bearer $TOKEN" \
  -G \
  --data-urlencode namespace=orka-system \
  --data-urlencode query="release checklist"
```

Create durable memory:

```bash
curl -sS -X POST http://localhost:8080/api/v1/memories \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "namespace": "orka-system",
    "source": "user",
    "content": "Release tasks should run make lint-fix and make test before merging.",
    "tags": ["release", "testing"]
  }'
```

## Memory proposal API

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/memory-proposals` | GET | List memory proposals |
| `/api/v1/memory-proposals` | POST | Create a memory proposal |
| `/api/v1/memory-proposals/:id` | GET | Get a memory proposal |
| `/api/v1/memory-proposals/:id/review` | POST | Record a review decision |
| `/api/v1/memory-proposals/:id/archive` | POST | Archive a proposal without applying it |

Supported list query parameters:

| Parameter | Description |
|-----------|-------------|
| `namespace` | Namespace to query |
| `taskName` | Filter by task provenance |
| `agentName` | Filter by agent provenance |
| `type` | Filter by proposal type, for example `memory`, `skill`, `policy`, or `workflow` |
| `status` | Filter by status such as `pending`, `accepted`, `rejected`, or `archived` |
| `query` or `q` | Text search over title, description, content, and skill name |
| `limit` | Maximum rows to return |

Create a proposal:

```bash
curl -sS -X POST http://localhost:8080/api/v1/memory-proposals \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "namespace": "orka-system",
    "taskName": "release-review",
    "agentName": "release-agent",
    "type": "memory",
    "title": "Release validation command",
    "description": "Reusable release procedure discovered during task execution.",
    "content": "Run make lint-fix and make test before merging release PRs."
  }'
```

Review a proposal:

```bash
curl -sS -X POST "http://localhost:8080/api/v1/memory-proposals/mprop-example/review?namespace=orka-system" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "status": "accepted",
    "reviewer": "platform-team",
    "reviewNote": "Valid reusable release procedure."
  }'
```

Review returns `204 No Content`. It records governance state only; it does not apply the proposal as durable memory.

## Safety model

- Store durable memories only for reusable project facts, decisions, conventions, or procedures.
- Do not store secrets, credentials, tokens, raw transcripts, one-off task status, or sensitive personal data.
- Memory and proposal persistence passes content through sensitive-text redaction.
- Disabled and deleted memories are excluded from normal recall by default.
- Memory writes are namespace-scoped and authenticated through the same ServiceAccount-token API model as other Orka APIs.

## Validation

The live Copilot proxy E2E suite validates the current memory path with a real model-backed worker:

- durable memory can be pre-seeded through the API
- a live worker receives memory tools through `ORKA_AI_TOOLS`
- the worker executes `recall_memory`, `remember`, `propose_memory`, and `search_transcript`
- durable recall does not create duplicate durable memory
- proposed memory remains a proposal and is not written as durable memory
- proposal review persists accepted/rejected state without applying the proposal

Deterministic unit and integration tests cover store behavior, API handlers, tool registration, prompt composition, and worker/tool plumbing.

## Current limitations

- Accepted proposals are not automatically converted to durable memories.
- There is no explicit proposal apply endpoint yet.
- Durable memory search currently uses store-level filters and substring matching rather than semantic ranking.
- Transcript search returns snippets, not model-generated summaries.
- External memory backends are not part of the default implementation.
