---
slug: /memory
description: "The namespace-scoped memory layer: durable memories, proposals, and why writing one is a review request."
---

# Memory

Memory lets agents carry knowledge from one Task to the next: project facts, decisions,
conventions, and procedures. In a shared cluster, memory that any model output could change
would be a liability, so Orka makes writing a memory a review request. Agents propose; a
person reviews and applies; only then do future Tasks see it. Memory is scoped to a namespace
and available to native `type: ai` Tasks.

The current model has three related concepts:

| Concept | Purpose | Persistence |
|---------|---------|-------------|
| **Durable memory** | Reviewed project facts, decisions, conventions, or reusable context that can help future tasks | Stored in SQLite as namespace-scoped `memories` records |
| **Memory proposal** | A worker- or user-submitted suggestion for memory, policy, workflow, or skill changes | Stored in SQLite as `memory_proposals` for review |
| **Transcript search** | Compact search over prior session messages | Stored in SQLite session transcript tables |

Memory proposal review is **non-applying**. Reviewing a proposal as accepted or rejected records the decision only. Accepted proposals with `type: "memory"` or a [consolidation type](#consolidation-proposals) can then be applied explicitly. A `memory` apply creates (or idempotently returns) durable memory linked back to the proposal; a consolidation apply changes the memories it targets.

Both steps record who took them: `reviewer` on the review and `appliedBy` on the apply. The server takes these names from the signed-in identity of the request. A client cannot set them: a request body with a non-empty `reviewer` or `appliedBy` is rejected with `400 Bad Request`, and a request without an identity is refused.

:::tip[Video demo]
Watch [Review and publish an agent's memory](https://www.youtube.com/watch?v=iCqyVfWBpyg).
:::

## Memory proposal lifecycle

Memory proposals follow an explicit review/apply lifecycle:

- New proposals start with `status: "pending"`.
- Review decisions only apply to pending proposals and record either `status: "accepted"` or `status: "rejected"`; review does not create durable memory.
- Accepted proposals with `type: "memory"`, or one of the [consolidation types](#consolidation-proposals), require an explicit apply request before they change durable memory.
- Rejected or archived proposals cannot be applied.

Accepted memory proposals can suggest durable memory tags by including a `Tags:` line in the proposal `description`. The apply step reads tags from the first `Tags:` line only, normalizes them, and ignores later `Tags:` lines. Example:

```text
Reusable release procedure discovered during task execution.
Tags: release, testing
```

## Consolidation proposals

Over many Tasks, memory collects duplicates, entries that later work contradicted, and stale facts. Three proposal types consolidate existing memories. Like every proposal, each is reviewed and applied by a person; nothing changes until it is applied.

| Type | Targets | Applying it |
|------|---------|-------------|
| `memory_merge` | two or more memories | creates one memory from the proposal's `content` and retires every target into it |
| `memory_supersede` | one memory | creates one memory from the proposal's `content` and retires the target into it |
| `memory_retire` | one or more memories | disables every target |

A consolidation proposal names each memory it changes in `targets`, with the `generation` of the memory it read. Every memory has a `generation`: `1` when it is created, and one more each time its content, tags, provenance, `disabled` or `deleted` state changes. A recall does not change it. Any proposal may also cite the Sessions it drew on in `sources`, each with a `sessionName` and an optional `messageId`. A proposal names at most 20 targets and 20 sources.

```json
{
  "namespace": "orka-system",
  "type": "memory_merge",
  "title": "One release checklist",
  "content": "Release PRs run make lint-fix and make test, then the e2e suite.",
  "targets": [
    {"memoryId": "mem-release-1", "generation": 1},
    {"memoryId": "mem-release-2", "generation": 3}
  ],
  "sources": [{"sessionName": "release-review"}]
}
```

Applying a consolidation proposal changes its targets in one transaction, and only while every target is still at the generation the proposal names and is not deleted. If any target changed, the apply returns `409 Conflict` and changes nothing: the proposal was written against content nobody reviewed since. It stays `accepted`, so archive it. Applying one also requires `update` permission on every target memory, besides the usual permission to apply proposals and create memories; see [API authorization](../reference/api-authorization.md). Consolidation proposals are reviewed and applied only through `/api/v1`: the worker-facing internal routes check only the namespace, so they refuse to review or apply one with `403 Forbidden`.

A merge or supersede marks each target deleted and disabled, and sets its `supersededBy` to the new memory, so the replaced record and its provenance stay readable: list with `includeDeleted=true&includeDisabled=true`, since deleted memories are disabled too. A retire only disables its targets, which you can enable again. The apply response is the memory created, or for a retire the first target.

## Worker behavior

When an AI worker can reach the controller internal API, it loads reviewed durable memories before invoking the model. The durable memory section is appended to the system prompt as background project context, separate from the current session transcript.

In this warning release, every native `type: ai` Task receives the memory tools,
regardless of Agent `spec.tools`, Task `spec.ai.tools`, or `coordination.enabled`.
They cannot yet be disabled per Agent, including with `enabled: false` or the
`orka.ai/disable-coordination-tool-injection` Task annotation.

When a model calls a memory tool that is neither explicitly selected nor covered
by coordination, the worker logs a deprecation warning. **The next release will
stop adding these tools unless the Agent enables coordination or the Agent/Task
explicitly selects them.** In that release, the disable-injection annotation will
also suppress automatic memory tools; explicit selections will remain available.
See the [migration and release notes](../operations/upgrading.md#memory-tool-opt-in)
and [tool parameters](../reference/built-in-tools.md#memory-tools).

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

### Recall counts

Each memory records how often agents recalled it: `recalledCount` and `lastRecalledAt`. Only a recall counts. When an agent's `recall_memory` call returns memories, each memory it returned gains one count, and its `lastRecalledAt` becomes the time of the call. Only a recall by the verified worker of an active Task, or through the controller's tool broker, is counted. Memories loaded into a prompt's durable memory section, and memories an operator lists through the API or CLI, are not counted. Neither is a recall whose result is larger than 1 MiB, the most an agent's tool call receives.

Counting a recall does not update the memory: `updatedAt`, and so the order in which memories are listed and loaded into prompts, stay as they were. Counting is best effort: it runs in the background without blocking the recall's response, so it never delays or fails the recall, and when many counts are already running a new one is dropped. Deleted memories are not counted, and a new memory always starts with none, whatever its create request names.

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
| `source` | Filter by source such as `task`, `session`, `user`, `system`, `memory_proposal`, or `e2e` |
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
| `/api/v1/memory-proposals/:id/review` | POST | Record a review decision without applying it |
| `/api/v1/memory-proposals/:id/apply` | POST | Apply an accepted `memory` or consolidation proposal to durable memory |
| `/api/v1/memory-proposals/:id/archive` | POST | Archive a proposal without applying it |

Supported list query parameters:

| Parameter | Description |
|-----------|-------------|
| `namespace` | Namespace to query |
| `taskName` | Filter by task provenance |
| `agentName` | Filter by agent provenance |
| `type` | Filter by proposal type, for example `memory`, `memory_merge`, `memory_supersede`, `memory_retire`, `skill`, `policy`, or `workflow` |
| `status` | Filter by status such as `pending`, `accepted`, `rejected`, `applied`, or `archived` |
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
    "description": "Reusable release procedure discovered during task execution.\nTags: release, testing",
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
    "reviewNote": "Valid reusable release procedure."
  }'
```

Review returns the stored review record: `id`, `namespace`, `status`, `reviewer`, `reviewNote`, and `reviewedAt`. `reviewer` is the identity that made the request. Review records governance state only; it does not apply the proposal as durable memory.

Apply an accepted memory proposal:

```bash
curl -sS -X POST "http://localhost:8080/api/v1/memory-proposals/mprop-example/apply?namespace=orka-system" \
  -H "Authorization: Bearer $TOKEN"
```

Apply returns the durable memory as JSON. The created memory has `source: "memory_proposal"` and `sourceProposalId` set to the proposal ID, and the proposal is updated to `status: "applied"` with `appliedMemoryId`, `appliedBy` (the identity that made the request), and `appliedAt`. Repeating the apply request is idempotent and returns the same durable memory. Only accepted proposals with `type: "memory"` or a consolidation type can be applied; rejected and archived proposals cannot be applied. A consolidation apply returns the memory a merge or supersede created, or a retire's first target, and returns `409 Conflict` when a target changed since the proposal was written; see [Consolidation proposals](#consolidation-proposals).

## Safety model

- Store durable memories only for reusable project facts, decisions, conventions, or procedures.
- Do not store secrets, credentials, tokens, raw transcripts, one-off task status, or sensitive personal data.
- Memory and proposal persistence passes content through sensitive-text redaction.
- Disabled and deleted memories are excluded from normal recall by default.
- Memory writes are namespace-scoped and authenticated through the same ServiceAccount-token API model as other Orka APIs.
- Applying accepted memory proposals preserves proposal-to-memory linkage for auditability.

## Current limitations

- Accepted proposals are not automatically converted to durable memories; they require the explicit apply endpoint.
- Only accepted proposals with `type: "memory"` or a consolidation type can be applied to durable memory.
- Durable memory search currently uses store-level filters and substring matching rather than semantic ranking.
- Transcript search returns snippets, not model-generated summaries.
- External memory backends are not part of the default implementation.
