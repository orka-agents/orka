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

A new memory that no existing one covers stays an ordinary `memory` proposal, as `remember`
creates; a run that proposes one cites the Sessions it drew on.

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
  time, so a port cannot keep the all-or-nothing guarantee. It must check every target's
  generation before its first write, then write under idempotent operation IDs and record
  the application as in progress. When a later write conflicts, because a target changed
  after the check, the writes that succeeded stay: the new memory exists and some targets
  are retired. The port records the proposal as partly applied, naming what changed, and
  leaves the rest for a person, who can retire the new memory or the remaining targets. It
  never retries a conflicting write against the newer content.

### Scheduled consolidation runs

- A run is an ordinary native `type: ai` Task with `spec.ai.memoryConsolidation`, usually
  with `spec.schedule`. Admission refuses the field on other Task types and with a
  `sessionRef`. The mode is `DryRun`, the default, which reports what it would propose, or
  `Propose`, which also creates the proposals.
- A run reads the transcripts of every user's Sessions in its namespace, which no ordinary
  Task can: the internal transcript routes limit a Task to its own Session and its
  coordination tree. So a `ValidatingAdmissionPolicy` checks the live caller that creates
  such a Task, or changes any of its spec: it needs `list` and `get` on `sessions` in the
  namespace (reading a transcript needs `get`), and in `Propose` mode also `create` on
  `memoryproposals`. Checking every spec change matters because the prompt steers the run
  and the Agent or provider decides where the transcripts are sent. The field is immutable.
  A scheduled run is created by the controller from a parent that passed this check. The
  internal run routes accept only the authenticated worker of an active Task that carries
  the field.
- Memory and its proposals are namespace-wide. A namespace that enables consolidation
  accepts that what one person's Session taught can reach everyone's prompts, after review.
- In this mode the worker has one tool, `submit_memory_consolidation`, a fixed
  Orka-owned system prompt, and no injected memory context. `spec.ai.prompt`, when set, is
  passed as the operator's focus.
- Orka keeps one run record per Task UID. Beginning a run fixes its window, which ends now.
  It starts at its mode's cursor: the window end of the latest completed run in that mode,
  or, before any completed, the window start of the mode's first run, so skipped and failed
  runs read their Sessions again however long they wait. A cursor is followed back at most
  720 hours. With no run at all, the window starts the lookback (default 7 days) before
  now. `DryRun` and `Propose` keep separate cursors, so dry runs never use up the Sessions
  a proposing run has yet to read. A retried Pod of the same Task gets the same window.
- At most one run reads a namespace's memory at a time. Beginning a run while another in the
  namespace, of either mode, is running returns a conflict, so two runs never propose over
  the same memories at once. A run whose Task died stops blocking after two hours.
- Inputs:
  - every active memory in the namespace (at most 200), with its generation and recall
    counts, each cut to 2,000 characters;
  - chat and task Sessions, never gateway ones, that changed inside the window: the 50 most
    recently changed, and the newest 40 user and assistant messages of each, each cut to
    2,000 characters. A Session with a turn still running is read too: its transcript holds
    only finished turns, and releasing its lock does not change it again, so skipping it
    could miss it for good. Tool messages are dropped, so memory a run recalled cannot be
    read back as new evidence.
  - at most 60,000 characters of memories and, separately, 60,000 characters of
    transcripts, so memories never crowd out the evidence; the report says when either was
    cut. That stays inside a model's context even at one token per character. The run makes
    one model call and never drops input to fit: a model that cannot take it fails the run,
    which leaves its Sessions to the next.
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
  - the operations remove at most `max(1, ⌊active memories ÷ 4⌋)` memories. A merge of k
    memories removes k−1, a supersede none and a retire k, so merging two duplicates is
    always possible, and a run that only supersedes or proposes nothing is within it.
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
- A run reads at most 50 Sessions and 60,000 characters of transcripts. When more changed,
  the oldest of them are not read.
- The storage contract gains a revision and new methods that every provider must
  implement, checked by the shared suite.
- Ranking recalls by relevance, an index of memory titles loaded into every prompt, and
  automatic memory injection for ACP runtimes are separate decisions.
