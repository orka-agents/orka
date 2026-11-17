# ADR 0052: RepositoryMonitor human review feedback

Date: 2026-11-16

## Status

Proposed. Implements the review-feedback half of
[#357](https://github.com/orka-agents/orka/issues/357), behind the opt-in
`spec.repair.humanFeedback`. Builds on the normalized CI evidence that fix-CI
repairs already carry and leaves
[ADR 0038](0038-repository-monitor-repair-validation.md) unchanged. #357's
"authorized resolution" requirement (replying to and resolving addressed
threads) is deferred. In this release Orka never replies to, resolves, or
dismisses a human review thread or review.

## Context

A RepositoryMonitor repair sees Orka's own findings and, for a fix-CI repair,
the failing checks' output. It does not see what people said about the pull
request. A maintainer who leaves "please handle the nil case" on line 40 has to
fix it themselves or repeat it in a label command, and a repair that runs anyway
cannot knowingly address it.

GitHub exposes that feedback in two shapes. Review threads carry a path, a line
in the current diff (or only an original line once the code moved), a resolved
flag, and comments. Reviews carry a state (`CHANGES_REQUESTED`, `APPROVED`,
`COMMENTED`, `DISMISSED`) and a body. Anyone who can read a public repository
can comment, and the pull request author can comment on their own pull request.
GitHub App bots also comment, and the collaborator permission API answers 404
for them.

Three things make this harder than CI evidence. The text is written by people,
some of whom have no permission on the repository, so it is a prompt-injection
surface. The feedback also has to start work, not only inform it, so who wrote
it matters. And Orka cannot tell when a person considers a comment addressed,
because people resolve threads, not Orka.

## Decision

### Read feedback by GraphQL, bounded and bound to the head

The controller reads review threads and reviews with GitHub GraphQL. Each
thread keeps its ID, resolved and outdated flags, path, line, original line,
and diff side. It also keeps its root comment and its 20 newest comments, each
with `fullDatabaseId`, author login and type, body, URL, creation and last-edit
times, minimized flag, and the review it belongs to. Each review keeps its
`fullDatabaseId`, state, body, author, commit, and times. Reads are bounded
(four pages of 50 threads, four pages of 50 reviews, 20 permission lookups, 20
seconds; permission lookups are also cached and bounded per monitor poll), and a read that stops early sets an explicit `partial` flag with a
reason. Reviews are read newest page first (`reviews(last:, before:)`), so a
read cut at its page limit holds the newest reviews: every reviewer it holds
has their true latest verdict, and a reviewer whose latest review is older than
the pages read is absent rather than shown with an earlier verdict. Rate limits, including GraphQL's HTTP 200 `RATE_LIMITED` error, are
classified with the same helper as CI evidence.

The result is a snapshot bound to the repository, pull request, and head SHA,
with a stable ID per entry (`review_thread:<node ID>`,
`review_comment:<id>`, `review:<id>`) and a SHA-256 digest of its bytes. Every
page reports the pull request's head. If the head differs from the one being
repaired, or changes between pages, the snapshot is marked `stale` and holds no
entries.

Text runs through the CI evidence sanitizer: ANSI and control characters are
stripped, credentials are redacted per line and across wrapped lines, and the
text is bounded (4 KiB and 80 lines per comment, 8 KiB per thread, 32 KiB in
total). HTML comments are removed first. GitHub does not render them, so they
can hide instructions from the people reading the thread. Every comment is
removed, even inside code, and an unclosed `<!--` removes the rest of the
text: deciding what GitHub hides needs a full Markdown parser, and dropping
some visible text is the safe failure. An outdated thread
keeps only its original line, so it is never shown at a current line. Deleted
comments are gone from the API. Minimized comments, and comments that are
empty once hidden text is removed, are left out. Edits keep the comment's ID
and record `lastEditedAt`.

### Separate rules for evidence and for starting a repair

**Evidence** is what the repairer may read. It includes unresolved threads and
each reviewer's current `CHANGES_REQUESTED` review. A later approval or the
dismissal of that review withdraws it, and a comment-only review does not.
Outside reviewers, the pull request author, deleted accounts, and authors whose
permission lookup failed are all evidence, each marked with the permission
GitHub reported (`admin` through `read`, `none`, `unknown`). Resolved threads,
Orka's own comments and reviews, and bots outside the allowlist are not. Orka's
own comments are recognized by the review IDs in Orka's stored publish records,
not by the hidden marker alone, so a copied marker hides nothing. The records
of every monitor in the namespace count, so a review another monitor of the
same repository published is never read as human feedback; records never cross
namespaces.

**Authority to start a repair** is narrower. The author's collaborator
permission must meet `minPermission` (default:
`spec.triggers.github.labels.requireActorPermission`), and the permission must
also be in `spec.policy.allowedRepositoryPermissions` when that is set. A failed
or skipped lookup never authorizes, though the comment stays evidence. The
bounded lookups go first to authors whose feedback could start a repair:
commenters on unresolved, current threads that no Orka finding opened, and
reviewers with a current `CHANGES_REQUESTED` review that has a body, newest
activity first. Any lookups left go to other authors in GitHub's order, so
evidence-only commenters cannot use up the lookups an authorized reviewer
needs. The pull
request author never starts a repair of their own pull request, so a repair
cannot loop on itself. Replies on a thread that one of Orka's findings opened
are context for that finding and start nothing of their own. An outdated thread
is open but not current, so it starts nothing. A partial or stale snapshot
starts nothing at all.

### Bots

A bot's comments are evidence, and can start a repair, only when its login is
in `botLogins`. Logins are matched case-insensitively, with or without the
`[bot]` suffix. Because the collaborator API cannot describe a GitHub App bot,
the allowlist is the bot's authority, and its permission is recorded as `bot`.
A bot comment also needs a path and a current line. Bot review bodies and
outdated or file-level bot comments are never evidence, which keeps
summary-style bot output out of the prompt.

### Trigger, prompt, and trust domains

For pull requests in full repair scope, authorized current feedback schedules
an ordinary `fix` repair once the feedback has been quiet for two minutes after
its newest comment or edit. Each repair takes up to `maxCommentsPerRepair`
targets (default 10, at most 50). The same snapshot also goes, as evidence
without targets, into any findings `fix` repair and any explicit repair
command.

The repair input `orka.prRepair.input.v1` gets a separate `humanFeedback` field
with its digest and the target IDs. The prompt frames it as untrusted reviewer
statements, a trust domain separate from CI output, Orka's findings, and Orka's
instructions. It tells the repairer to change only what is within this pull
request's scope, and to ignore requests about credentials, secrets, or CI and
workflow files, as well as any instructions embedded in the text. It also
tells the repairer to weigh all reviewers' feedback together and not to pick a
side between conflicting reviewers. On a conflict it makes no change and says
so, which leaves the decision to a person.

### An immutable attempt ledger

`ActionRecord` cannot count attempts: it has no atomic counter, and updates
overwrite its result fields. A new store table, `monitor_feedback_attempts`,
holds one immutable row per attempt. Each row records the monitor, repository,
pull request, feedback ID, and attempt number, plus the head SHA, the snapshot
digest that admitted it, the command event ID, and the repair job ID. A unique
key on the first five fields and a contiguity check make attempt `n` admissible
only once, and only after attempt `n-1`.

The rows are written in the same transaction as the repair's command event.
The command's dedupe key names the budget slot it takes: the pull request, its
head, and the repair counts the budget check saw. Admission is therefore atomic
with the existing repair admission. Two reconciles that pick different targets
cannot both schedule a repair, or both fit into one remaining slot. Replaying
the same command for the same monitor and pull request is idempotent. The
command's budget slot ignores the monitor generation, because repair budgets
outlive spec changes. Editing a comment keeps its ID, so the edit does not
reset its lifetime budget of `maxAttemptsPerComment` (default 2). A new head
re-triggers feedback only while attempts remain and the thread is still
unresolved and current.

### One outcome per feedback repair

Earlier designs inferred whether an attempt had run from the command's status,
the repair job's phase, whether the repair Task still existed, and the work
action's status and metadata. Each was written at a different moment, and the
attempt charge, the readiness projection, and the repair budget each inferred
differently, which repeatedly disagreed on edge cases. Instead, each feedback
repair has one mutable outcome record beside its immutable attempts, in the
store table `monitor_feedback_outcomes`, keyed by the command event:

- `reserved`, written with the command and its attempts at admission, lists
  the admitted feedback IDs. All of them are charged, so a queued repair holds
  its attempts.
- `started` is written only once the repair Task exists, with the
  `humanFeedbackTargets` its prompt froze and the Task's name. From then on
  only those targets are charged; admitted feedback that was resolved before
  the Task was created is released.
- One terminal state follows and never changes: `withdrawn` (only from
  `reserved`: the feedback no longer needed a repair, or needs a person),
  `failed` (from either), `no_change` or `succeeded` (only from `started`).

The charge is a function of the record alone: `reserved` charges every
admitted ID, anything else charges its targets, which are empty when no Task
ever existed. An attempt without an outcome is charged, so the budget fails
closed. Each transition is written where it becomes known, idempotently and
under the controller's write fence: the handoff after creating or adopting
the Task, the withdrawal and refusal paths before it, and repair ingestion,
which records the Task's end before the job so a retried ingestion records the
same end. A command that ends any other way, such as a cancellation or
exhausted retries, ends its outcome as it is settled. A restart in any state
resumes from the record. A `started` repair whose Task disappeared before its
result was recorded fails with `repair_task_missing`, keeps its charge, and is
not run again; repair ingestion settles it once the Task has been missing for
five minutes, longer than a lagging cache. An outcome that already ended
records that end on the job, the action, and the item again. Until the
outcome starts, the repair job is marked `repair_task_create_pending`, and a
Task create that failed without the API server refusing it, such as a
timeout, keeps that mark. A `reserved` repair whose job is so marked and
whose Task is missing may have run, so it fails with
`repair_task_outcome_unknown` and keeps every admitted charge rather than
running again. A create the API server refused outright stays retryable and
charges nothing.

The readiness projection skips a `withdrawn` repair, so a withdrawal leaves
the head as if no repair had been requested. The pull request's repair budget
counts every feedback repair past `reserved`, including one withdrawn before
its Task existed. A feedback repair whose Task verifiably changed nothing,
because its feedback was already addressed, ends `no_change`: it keeps its
charge, is not a failed repair, and leaves the head and its review alone;
open-feedback readiness decides whether a person is needed.

An unreleased build of this change wrote schema version 8 without the outcome
table. Opening such a store repairs its layout in place at version 8 and
recovers one outcome per admitted command: an unfinished command stays
`reserved`, a finished one whose repair job settled from its Task charges
every admitted ID, and any other finished one charges nothing.

### One read per handoff and one completeness verdict

A queued feedback repair reads feedback once per handoff. The same snapshot
rechecks the repair before its Task exists and becomes the Task's input, so no
change can fall between a recheck and the input. A retried handoff whose Task
already exists adopts the Task's frozen input and reads nothing.

Every decision about whether a snapshot is complete goes through one verdict:

- `complete`: every piece of feedback that could start a repair is shown,
  with its author's authority known. Only a complete snapshot admits a repair
  or hands one off.
- `incomplete_retry`: a transient failure: a failed or throttled GitHub read
  or permission lookup, the poll's permission lookup budget, or a read for
  another head. Readiness waits and a queued repair is deferred.
- `needs_person`: a permanent limit: more feedback than the page limits hold,
  a single thread whose response exceeds GitHub's limit even when read alone
  (larger thread pages are read again at half the size, so they are not a
  limit),
  an unresolved thread whose omitted replies may hide feedback, a thread or
  review left out of the 32 KiB budget that holds or may hide feedback that
  could start a repair, or more reviewers than one snapshot may look up.
  Readiness asks for a person and a queued repair is withdrawn with
  `review_feedback_needs_person`.

Collaborator permissions are cached for one monitor poll, shared by every
pull request, readiness, and repair label checks, and bounded to 60 lookups
per poll in addition to the 20 per snapshot. A failed lookup is cached for the
poll too, so a throttled account is not asked again until the next poll.

### Budgets and maxRounds

Feedback repairs are ordinary repair jobs, so they count against
`maxRepairsPerPR` and `maxRepairsPerHead`. `spec.review.maxRounds` bounds Orka's
own review loop: it raises a review to `needs_human` after that many unpassed
heads. **It does not stop human-feedback repairs.** A person asking for a change
is not Orka's review loop failing to converge, and the attempt and repair budgets
still bound the work. This is open for maintainer review.

### Readiness

When `humanFeedback` is enabled, a head that would otherwise be ready follows
the snapshot's verdict first: `incomplete_retry` keeps it `pending`, and
`needs_person` makes it a non-success status saying a person must review the
feedback. A complete snapshot stays `pending` ("Addressing N reviewer
comments") while authorized current feedback has attempts left. It becomes a
non-success status saying a person is needed once that feedback's attempts are
exhausted, or when authorized feedback is open but automation will not act on
it, such as an outdated thread or a repair label added by an account that may
not start repairs. Readiness never resolves or replies to threads.

### Why there is no thread resolution in v1

Resolving a thread claims a person's concern is settled. Orka cannot verify
that claim. A pushed change, a passing check, or a thread that turned outdated
shows that code moved, not that the reviewer agrees. Replies and resolutions
also need an idempotent GitHub mutation ledger, exact-head and validation
checks, and a policy for who may close what. #357 asks for all of that, and it
is deferred. Until then people resolve threads, and readiness reflects what
they left open.

### Relation to ADR 0038

A feedback repair is a repair Task like any other. When the monitor has
`spec.validation.image` and `repairCommand`, the repair's patch is validated
before it is published, exactly as ADR 0038 describes. A feedback repair cannot
bypass that or the exact-head compare-and-swap publication. Fork pull requests
and the maintainer-edits rule also apply unchanged.

## Consequences

- Reviewer comments can drive repairs, but only from authorized authors and
  only within the attempt and repair budgets.
- Each feedback repair handoff costs one feedback read (two GraphQL
  connections) and up to 20 permission lookups, cached across the poll.
  Readiness makes the same reads only for a head that would otherwise report
  success.
- A comment the repairer addressed still counts against its budget until a
  person resolves the thread or the change makes it outdated. With the default
  of two attempts, a satisfied but unresolved comment can get one extra no-op
  repair before readiness asks for a person.
- The schema gains the attempt ledger and outcome tables and moves to schema
  version 8. The state provider contract gains the outcome methods in revision 9.
- Reply and resolve actions, webhook-driven latency for review events, and the
  issue's resolved-thread evidence remain future work.
