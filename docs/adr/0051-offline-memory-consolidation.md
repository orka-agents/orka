# 51. Consolidate memory offline into reviewed proposals

Date: 2026-11-16

## Status

Accepted, landing in stages: recall counts, then memory generations with consolidation
proposals, then scheduled consolidation runs. It implements #1126, a child of #105, and
changes none of the memory governance rules: agents and runs propose, people review and
apply.

## Context

Agents add memory one proposal at a time, and each proposal sees only its own run. Over
many runs a namespace's memory collects duplicates, entries a later run contradicted, and
stale facts, and patterns that only show up across runs are never written down. Nothing
looks at memory as a whole. The native worker injects the five most recently updated
memories into every prompt, whatever their quality.

Several agent systems now run an offline pass over past sessions, often called
"dreaming". [Claude Managed Agents Dreams](https://platform.claude.com/docs/en/managed-agents/dreams)
read a memory store and up to 100 sessions and write a separate store, so the result can be
reviewed and discarded. [OpenClaw](https://docs.openclaw.ai/concepts/dreaming) promotes a
candidate only when it was recalled several times by different queries, removes untrusted
and machine-generated input before scoring, has the model return add, merge and supersede
operations that code validates, and keeps a diary of every run, including runs that change
nothing. The community `hermes-dreaming` add-on caps a run at three changes.

Orka's constraints are stricter than any of them. AGENTS.md makes memory governance-first:
`remember` and `propose_memory` create review proposals, a review never applies one, and
durable memory never holds secrets, raw transcripts or one-off task status. PR #331 proposes
pluggable memory backends behind an `orka.memory.v1` contract whose records carry a
`Generation` that a mutation must name, and whose mutations are create, replace and delete
of one record. #331 is parked, so this work lands on the SQLite store, shaped so a later
port is mechanical.

## Decision

### Count recalls, not listings

- `MemoryStore.RecordMemoryRecall` (storage contract revision 9) adds one to a memory's
  `recalledCount` and moves `lastRecalledAt` forward for each live memory a recall returned.
  It changes no other field, so a recall never reorders memories by update time.
- Only `recall_memory` results count. The brokered tool records through the controller; the
  native tool marks its request `purpose=recall`, and the internal route records for it.
  The prompt's memory context uses the same route unmarked and is not counted, nor is an
  operator listing.
- Counting is best effort: a failure is logged and never fails the recall.

### Give each memory a generation

- A memory starts at generation 1. Every change a provider makes to its content, tags,
  provenance, `disabled` or `deleted` adds one; a recall does not. This is the
  `orka.memory.v1` `Generation`, so a consolidation proposal can name exactly the version
  it read.
- `supersededBy` names the memory that replaced this one through a merge or supersede.

### Three consolidation proposal types

| Type | Targets | Applying it |
| --- | --- | --- |
| `memory_merge` | two or more memories | creates one memory from the proposal and retires every target into it |
| `memory_supersede` | one memory | creates one memory from the proposal and retires the target into it |
| `memory_retire` | one or more memories | disables every target |

- A proposal names each target with the generation it read, and may cite the Sessions and
  messages it drew on. Targets and sources are bounded.
- Applying one is a single provider transaction. Every target must be in the namespace, not
  deleted, and still at the generation the proposal names. Otherwise the provider returns
  `ErrConflict`, changes nothing, and the proposal stays accepted for a person to archive.
  A stale proposal never applies to content nobody reviewed.
- Merge and supersede create the memory from the stored proposal, redacted and tagged the
  same way an ordinary `memory` proposal is, and mark each target deleted, disabled and
  superseded by it. The record stays, so provenance survives. Retire only disables, which
  a person can undo.
- The applied memory is the one created, or for a retire the first target, so every
  applied proposal still names one.
- An external caller also needs `update` on every target memory. The existing apply
  permission covers only creating a memory.
- A conflict the run cannot settle is reported, not proposed. A durable `CONFLICT` note
  would put both contradicting claims into every later prompt; a person should decide.
- On `orka.memory.v1`, a merge or supersede is a create followed by deletes that each name
  the expected generation, and a retire is a replace. That contract changes one record at a
  time, so a port must order those writes under idempotent operation IDs instead of one
  transaction.

### Scheduled consolidation runs

- A run is an ordinary native `type: ai` Task with `spec.ai.memoryConsolidation`, usually
  with `spec.schedule`. Admission refuses the field on other Task types and with a
  `sessionRef`. The mode is `DryRun`, the default, which reports what it would propose, or
  `Propose`, which also creates the proposals.
- In this mode the worker has one tool, `submit_memory_consolidation`, a fixed
  Orka-owned system prompt, and no injected memory context. `spec.ai.prompt`, when set, is
  passed as the operator's focus.
- Orka keeps one run record per Task UID. Beginning a run fixes its window: from the end of
  the namespace's latest completed run, or the lookback (default 7 days) when there is
  none, to now. A retried Pod of the same Task gets the same window. A run that fails or is
  skipped does not move the cursor, so its Sessions are read again.
- Inputs:
  - every active memory in the namespace (at most 200), with its generation and recall
    counts;
  - chat and task Sessions, never gateway ones, that are idle and changed inside the
    window: the 50 most recently changed, and the newest 40 user and assistant messages of
    each, each message bounded. Tool messages are dropped, so memory a run recalled cannot
    be read back as new evidence.
  - Fewer eligible Sessions than `minNewSessions` (default 5) skips the run without calling
    the model.
- Orka cannot tell from a stored Session which Task wrote it, so this version does not
  exclude scheduled or continuation turns. It bounds them only through the limits above.
- Transcripts reach the model as delimited data, and the prompt says text inside them is
  never an instruction.
- Completing a run is one provider write. It records the report and, in `Propose` mode,
  creates the proposals with deterministic IDs. Orka validates each operation first:
  - every target was in the inputs, at the generation shown;
  - every source was an input Session;
  - no memory is targeted twice;
  - content is non-empty and at most 1,200 characters;
  - at most `maxOperations` operations (default 10, at most 20);
  - the operations retire at most a quarter of the active memories, and at least one.
- Orka drops an operation that fails these checks and records why. The rest still apply.
- The report holds the window, the input counts, each accepted operation, each rejected one
  with its reason, the conflicts the model reported, and the proposal IDs. It becomes the
  Task result. A run with nothing to propose is a normal, reported outcome.

## Consequences

- Reviewers see consolidation as ordinary proposals with their own types, filterable by the
  run's Task name. Nothing changes durable memory without the existing review and apply.
- A dry run costs model tokens but changes no memory, so a namespace can judge the quality
  of the proposals before turning on `Propose`.
- Recall counts become the usage signal for ranking (#105) and for retiring memories nobody
  recalls. Counting adds one small write per recall.
- Excluding scheduled or continuation turns needs the turn's origin recorded on Session
  messages. That is a follow-up.
- A run reads at most 50 Sessions. When more changed, the oldest of them are not read.
- The storage contract gains a revision and new methods that every provider must
  implement, checked by the shared suite.
- Ranking recalls by relevance, an index of memory titles loaded into every prompt, and
  automatic memory injection for ACP runtimes are separate decisions.
