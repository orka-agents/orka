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

## Consolidation runs

A consolidation run is a scheduled native `type: ai` Task that reads the namespace's current memories and its recently changed Sessions, and finds [consolidation proposals](#consolidation-proposals) and new memories worth adding. It never changes memory itself. In `DryRun` mode, the default, it only reports what it found. In `Propose` mode it also creates a pending proposal for each finding, for a person to review and apply as usual. Start with `DryRun`, read a few reports, and switch to `Propose` once the findings are worth reviewing.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: memory-consolidation
  namespace: orka-system
spec:
  type: ai
  schedule: "0 3 * * *"
  agentRef:
    name: memory-curator
  ai:
    prompt: Focus on release and testing conventions.
    memoryConsolidation:
      mode: DryRun          # or Propose
      minNewSessions: 5     # skip the run when fewer Sessions changed
      maxOperations: 10     # 1 to 20
      lookback: 168h        # how far back the first run reads; at most 720h
```

`spec.ai.prompt` steers what the run focuses on. The run uses a fixed Orka system prompt, no tools, and no injected memory, whatever the Agent or Task configure. The field requires an `ai` Task without `sessionRef`, and cannot be added, removed or changed after the Task is created.

### Who can create a run

A run reads the transcripts of everyone's Sessions in the namespace, which no ordinary Task can. Creating a Task with `memoryConsolidation`, or changing the spec of one, therefore requires `list` and `get` on `sessions` and `list` on `memories` in the namespace, since the run sends memories to the model and its report quotes them, and in `Propose` mode also `create` on `memoryproposals`. A `ValidatingAdmissionPolicy` checks the person making the change, so nobody without those permissions can edit a run's prompt, Agent or provider to steer it or send the transcripts elsewhere. The Orka API, which creates Tasks with its own identity, checks the same permissions for the person calling it, and a transaction token also needs the session-read and memory-read scopes, and in `Propose` mode the memory-write scope. The scheduled runs the controller creates from a Task are covered by the check on that Task.

A run sends transcripts to the model of its Agent or Provider. Whoever can edit that Agent or Provider can change where they go, for every Task that uses it, so restrict who can edit the Agent and Provider a consolidation run uses as you restrict who can read Sessions.

Memory and proposals are namespace-wide. Enabling consolidation means what one person's Session taught can reach everyone's prompts, after review.

### What a run reads

Each run reads the Sessions that changed since the last completed run of its mode in the namespace, or within `lookback` for the first one:

- chat and task Sessions only, never gateway conversations;
- the 50 most recently changed of them, however long ago they were created, and the newest 40 user and assistant messages of each, found among its newest 200 records and each cut to 2,000 characters. Tool messages are dropped, so memory an agent recalled cannot come back as new evidence;
- every active memory, up to 200, with its `generation` and [recall counts](#recall-counts), each cut to 2,000 characters and at most 10 tags of 40 characters;
- at most 60,000 characters of memories and, separately, 60,000 characters of transcripts, so memories never crowd out what the run learns from. Within a Session the limit keeps the newest turns, which the model still reads in order. A report whose input reached any limit says `inputTruncated`: these character budgets, 200 memories (a namespace with more has its least recently updated memories left out), 50 changed Sessions, or a Session's 40 messages. Sessions left out are read again only if they change again. A model that still cannot take the input fails the run, which leaves its Sessions for the next one.

`DryRun` and `Propose` keep separate places, so dry runs never use up the Sessions a proposing run has yet to read. When fewer than `minNewSessions` Sessions changed, the run is skipped without calling the model. A skipped or failed run does not move its mode's place: the next run reads the same Sessions again, from where the first run started, up to 720 hours back. A run's window ends 15.5 minutes before the run begins, the longest a stamped Session write can still take to commit, so a change still being written is read by the next run rather than missed. After a gap longer than 720 hours, such as a suspended schedule, the window starts 720 hours back, and the report's `windowCutFrom` says where its mode had last stopped.

A run records its input when it begins: a retried Pod reads the same input, and its result is checked against exactly what the model saw. The recorded input is deleted when the run finishes, or when its Task fails, is cancelled or is deleted first, and no other copy of it is kept. A run whose Task ended without finishing it is marked abandoned, and the next run in its mode reads its Sessions again. Once a run's Task is deleted, its stored report is cleared too, and Orka keeps only the few finished runs that mark where each mode's next run starts, so a recurring run's history does not grow the store. Only the worker of the run's own active Task can begin or complete it, and once its Job is revoked it can do neither, even before Kubernetes shows the revocation.

Only one run at a time reads a namespace's memory. A run that begins while another is running fails with a conflict. A run that read its inputs while another run in its mode completed reads them again from where that run ended, so the two never read the same Sessions. A run whose Task died stops blocking after two hours; the run that takes over marks it abandoned, so it can neither resume nor create proposals.

Orka cannot yet tell which Task wrote a Session, so turns of scheduled Tasks and parked continuations are read like any other.

### What a run keeps

The model's result is untrusted. A result without an `operations` list, or from a response the model did not finish, is not accepted: the worker asks once more, and otherwise the run fails, so its Sessions are read again. A run that finds nothing to change submits an empty list. Orka then checks every operation against what the run read and drops one that fails, recording why:

- every target is a memory the run read, at the generation it read, still active at that generation, and no two operations change the same memory;
- every source is a Session the run read, and an `add` cites at least one;
- new memory content is non-empty and at most 1,200 characters;
- at most `maxOperations` operations;
- the run removes at most `max(1, ⌊memories read ÷ 4⌋)` memories. A merge of k memories removes k−1, a supersede removes none, and a retire of k removes k, so a run can always merge two duplicates.

Contradictions the model could not settle are reported as conflicts, not proposed.

### The report

A run's report holds its mode and status (`completed` or `skipped`), the window it read, how many memories and Sessions it read, each kept operation, each dropped one with its reason, the conflicts, the IDs of the proposals it created, and a short summary.

The Task result is only the report's structure, marked `structural`: each operation's type, targets and sources, each dropped operation's index and reason, the memories each conflict names, the proposal IDs and the counts. It has no titles, content, reasons the model wrote, tags, conflict notes or summary, since anyone who can read the Task can read its result, and that text comes from everyone's Sessions and from memories. The full report is at `GET /api/v1/tasks/<run>/memory-consolidation-report`, which also requires `list` and `get` on `sessions` and `list` on `memories` (for a transaction token, the session-read and memory-read scopes as well as Task read), while the Task exists. A dropped operation's reason never repeats what the model named, only memories the run read.

A report that would be larger than 256 KiB is marked `abridged`, dropping, in order, the dropped operations, the conflicts, the summary and the kept operations' text until it fits. The proposals keep their full text. A run that finds nothing to change is a normal, reported outcome. Proposals a `Propose` run creates are attributed to the run's Task, so `orka memory proposal list --task-name <run>` lists them.

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

Only applying a proposal links a memory to it, so a create request with a `sourceProposalId` is rejected with `400 Bad Request`.

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
