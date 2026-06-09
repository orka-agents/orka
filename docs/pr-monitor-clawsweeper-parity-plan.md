# PR Monitor to ClawSweeper Parity Plan

This document describes how to evolve Orka's current PR monitor into an
Orka-native maintainer automation system with capabilities comparable to
ClawSweeper.

The intent is not to copy ClawSweeper's GitHub Actions and state-repository
architecture directly. Orka already has a Kubernetes controller, CRDs, task
reconciliation, agent runtimes, SQLite stores, REST APIs, and an embedded UI.
The target architecture should use those primitives while matching the useful
behavioral guarantees: durable state, exact-head review, command routing,
bounded repair, deterministic GitHub mutation gates, auditability, and operator
visibility.

## Current Orka PR Monitor

The current PR monitor is created by the `create_pr_monitor` tool. It creates a
scheduled `type: AI` Task with a prompt that tells an agent to:

1. List open pull requests for a configured GitHub repository.
2. Skip draft pull requests.
3. Check whether Orka already reviewed the current PR head SHA.
4. Check pull request CI status.
5. Fetch the PR diff.
6. Review correctness, tests, security, and maintainability.
7. Post one GitHub review that includes a hidden head-SHA marker.

The monitor uses these tools:

- `list_pull_requests`
- `check_pr_review_marker`
- `check_pull_request_ci`
- `review_pull_request`
- `post_review_comment`

Recent branch work has added important repository-scope hardening around
GitHub tool calls. Explicit `repo_url` values must match the current task's
workspace or transaction repository scope before a tool can act on that
repository.

The current monitor is useful, but it is still prompt-orchestrated. The agent
decides which PRs to process during a scheduled task. Orka does not yet keep a
durable per-PR review record, command status, repair budget, merge gate, or
state-machine history.

## ClawSweeper Capability Summary

ClawSweeper's capabilities can be grouped into these lanes:

1. **Scheduled and exact-event review**
   - Review open issues and pull requests on a cadence.
   - Review one exact issue or PR from a GitHub event.
   - Keep background review work separate from priority maintainer-requested
     work.

2. **Durable generated state**
   - Write one durable report per issue, PR, or commit.
   - Preserve the GitHub snapshot, decision, evidence, proposed comment, and
     runtime metadata.
   - Publish dashboard, audit, repair, and activity state.

3. **Single mutable public comments**
   - Maintain one marker-backed public review comment per issue or PR.
   - Edit that comment in place instead of posting repeated comments.
   - Use hidden markers for verdicts, actions, head SHAs, and status.

4. **Maintainer command routing**
   - Accept commands such as review, re-review, status, explain, fix CI,
     address review, rebase, autofix, automerge, approve, stop, and autoclose.
   - Verify maintainer permissions before write actions.
   - Route commands through durable jobs.

5. **Bounded repair and autofix**
   - Repair opted-in PRs through a bounded Codex review/fix loop.
   - Re-review exact heads after repair.
   - Stop after configured per-PR and per-head repair budgets.

6. **Automerge**
   - Merge only after exact-head review, required checks, mergeability, policy
     gates, and explicit maintainer opt-in.
   - Keep a mutable status surface that records review, repair, re-review, and
     merge progress.

7. **Issue implementation**
   - For narrow, reproducible, high-confidence bug issues, open guarded
     implementation PRs.
   - Keep broad, product, security, and ambiguous items in human review.

8. **Commit review**
   - Manually review selected code-bearing commits.
   - Store one report per SHA.
   - Optionally route narrow commit findings into the repair lane.

9. **Deterministic safety boundary**
   - Let the model decide review and repair content.
   - Keep authorization, repo boundaries, stale-head checks, budgets, labels,
     comments, pushes, closes, and merges in deterministic code.

## Parity Goal

Orka reaches practical parity when a configured repository can:

- Review PRs on schedule and on exact GitHub events.
- Avoid duplicate and stale-head reviews.
- Keep one durable public status/review surface per PR.
- Store typed review and repair state in Orka.
- Accept maintainer commands.
- Repair opted-in PRs with bounded loops.
- Re-review exact heads after repair.
- Merge only through deterministic gates.
- Expose all monitor, review, repair, and audit state through API and UI.
- Block unsafe repo, branch, permission, stale-state, security, and validation
  cases without relying on model judgment.

Issue review, issue implementation, and commit review should follow after PR
review, repair, and automerge are stable.

## Design Principles

1. **Orka owns orchestration**
   - Controllers and stores select work, enforce limits, and decide state
     transitions.
   - Agents receive focused tasks with explicit input and output contracts.

2. **Agents produce content, not authority**
   - Agents may review diffs, identify findings, repair code, and summarize
     evidence.
   - Agents must not decide final authorization, mutation eligibility, or merge
     authority.

3. **Every GitHub mutation is preflighted**
   - Re-fetch live GitHub state immediately before comments, labels, pushes,
     branch updates, closes, or merges.
   - Compare live state with the intended repository, PR number, head SHA,
     labels, permissions, and policy.

4. **State is typed and durable**
   - Hidden GitHub markers are useful for idempotency and public continuity.
   - SQLite state is the primary Orka source of truth.

5. **Exact-head semantics are mandatory**
   - A review, repair, or merge decision applies only to the PR head SHA it was
     made for.
   - If a PR moves, stale decisions become historical evidence only.

6. **Repair is bounded**
   - Every automated loop has explicit per-PR, per-head, per-run, and elapsed
     time limits.

7. **Security and credentials stay fail-closed**
   - Keep GitHub tokens in Kubernetes Secrets.
   - Do not expose write credentials to model review prompts.
   - Prefer deterministic executors for GitHub writes.

## Proposed Orka Architecture

### New CRD: RepositoryMonitor

Add a new CRD rather than overloading `RepositoryScan`. `RepositoryScan` is
security-posture focused. `RepositoryMonitor` should own maintainer automation
for issues, pull requests, and commits.

Example:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: RepositoryMonitor
metadata:
  name: openclaw-openclaw
  namespace: default
spec:
  provider: github
  repoURL: https://github.com/openclaw/openclaw
  branch: main
  gitSecretRef:
    name: github-app-token
  schedule: "*/15 * * * *"
  timeZone: UTC
  suspend: false

  targets:
    pullRequests:
      enabled: true
      includeDrafts: false
      maxPerRun: 20
    issues:
      enabled: false
    commits:
      enabled: false

  agents:
    reviewer:
      name: pr-reviewer
    repairer:
      name: pr-repairer
    implementer:
      name: issue-implementer

  review:
    event: COMMENT
    requireGreenCI: true
    staleReviewTTL: 168h
    exactEventEnabled: true

  repair:
    enabled: true
    requireMaintainerOptIn: true
    maxRepairsPerPR: 10
    maxRepairsPerHead: 2
    maxValidationRetries: 2
    maxReviewFixRetries: 2

  automerge:
    enabled: false
    requireMaintainerOptIn: true
    requireGlobalMergeGate: true
    allowedMergeMethods:
      - squash

  policy:
    protectedLabels:
      - orka:human-review
      - security
    optInLabels:
      autofix: orka:autofix
      automerge: orka:automerge
    pauseLabels:
      - orka:human-review
    advisoryLabels:
      enabled: true
    allowedRepositoryPermissions:
      - admin
      - maintain
      - write

  validation:
    mode: changed
    commands:
      - make test
```

Status fields:

- `phase`
- `lastRunID`
- `lastRunTime`
- `lastSuccessfulRunTime`
- `observedGeneration`
- `openPullRequests`
- `pendingReviews`
- `activeRepairs`
- `blockedItems`
- `mergeReadyItems`
- `conditions[]`

### Store Model

Add store interfaces and SQLite implementation for:

#### repository_monitors

Stores normalized monitor metadata and generation snapshots.

Important fields:

- `namespace`
- `name`
- `uid`
- `repo_url`
- `owner`
- `repository`
- `branch`
- `generation`
- `created_at`
- `updated_at`

#### monitor_runs

One row per scheduled, manual, or exact-event run.

Important fields:

- `id`
- `monitor_namespace`
- `monitor_name`
- `trigger`: `schedule`, `manual`, `pull_request_event`, `issue_comment`,
  `repository_dispatch`
- `target_kind`
- `target_number`
- `target_sha`
- `phase`
- `started_at`
- `completed_at`
- `selected_count`
- `created_task_count`
- `skipped_count`
- `error`

#### monitor_items

One current row per issue, PR, or commit target.

Important fields:

- `monitor_namespace`
- `monitor_name`
- `kind`: `pull_request`, `issue`, `commit`
- `number`
- `sha`
- `title`
- `author`
- `state`
- `labels_json`
- `base_branch`
- `head_branch`
- `head_sha`
- `base_sha`
- `draft`
- `mergeable_state`
- `ci_state`
- `last_review_id`
- `last_reviewed_head_sha`
- `last_verdict`
- `repair_state`
- `automerge_state`
- `status_comment_id`
- `status_comment_url`
- `updated_at`
- `last_seen_at`

#### review_records

Immutable review results.

Important fields:

- `id`
- `monitor_namespace`
- `monitor_name`
- `kind`
- `number`
- `head_sha`
- `task_name`
- `task_namespace`
- `verdict`
- `confidence`
- `repairable`
- `security_status`
- `findings_json`
- `summary`
- `suggested_comment`
- `rendered_comment`
- `marker`
- `github_review_id`
- `github_comment_id`
- `github_comment_url`
- `created_at`

#### command_events

Maintainer command intake and routing.

Important fields:

- `id`
- `monitor_namespace`
- `monitor_name`
- `repo`
- `kind`
- `number`
- `comment_id`
- `comment_url`
- `author`
- `author_association`
- `permission`
- `command`
- `intent`
- `head_sha`
- `status`
- `status_comment_id`
- `created_repair_job_id`
- `created_at`
- `processed_at`
- `error`

#### repair_jobs

Durable repair and automerge state.

Important fields:

- `id`
- `monitor_namespace`
- `monitor_name`
- `repo`
- `pr_number`
- `intent`: `fix_ci`, `address_review`, `rebase`, `autofix_pr`,
  `automerge_pr`
- `source`: `maintainer_command`, `trusted_review`, `manual`
- `head_sha`
- `base_sha`
- `phase`
- `repair_count_pr`
- `repair_count_head`
- `validation_attempts`
- `review_fix_attempts`
- `task_name`
- `branch`
- `pushed_sha`
- `last_error`
- `created_at`
- `updated_at`
- `completed_at`

#### monitor_events

Append-only audit log.

Important fields:

- `id`
- `monitor_namespace`
- `monitor_name`
- `run_id`
- `item_kind`
- `item_number`
- `item_sha`
- `event_type`
- `actor`
- `summary`
- `metadata_json`
- `created_at`

## Controller Components

### RepositoryMonitorReconciler

Responsibilities:

- Watch `RepositoryMonitor` resources.
- Validate repository config.
- Schedule runs from cron.
- Update status summary from store state.
- Enqueue manual and exact-event runs.
- Respect `spec.suspend`.

### MonitorRunScheduler

Responsibilities:

- Create `monitor_runs`.
- Enforce concurrency per monitor.
- Apply priority between exact events and background runs.
- Select eligible PRs deterministically.
- Create review Tasks.
- Record skip reasons.

Eligibility checks for PR review:

- PR is open.
- PR is not draft unless configured.
- PR repository matches monitor repo.
- PR head SHA is known.
- PR is not already reviewed for the exact head SHA.
- CI state is acceptable or policy allows review before green CI.
- Protected labels do not block review.
- Per-run and concurrency limits allow another task.

### ReviewResultIngestor

Responsibilities:

- Watch completed review Tasks.
- Parse and validate typed review JSON.
- Reject malformed output without posting model-controlled comments.
- Persist immutable `review_records`.
- Render deterministic public comments.
- Update or create the single Orka status/review comment.
- Emit verdict and action markers.
- Create repair jobs only when policy and markers allow.

### CommandRouter

Responsibilities:

- Handle GitHub `issue_comment`, `pull_request_review_comment`, and
  `pull_request_review` events.
- Parse accepted command aliases.
- Verify HMAC signature.
- Verify repository allowlist.
- Verify commenter permissions for write actions.
- Deduplicate by comment ID plus command marker.
- Update one command/status comment.
- Create or resume repair jobs.

Initial command set:

- `status`
- `review`
- `re-review`
- `stop`

Second command set:

- `fix ci`
- `address review`
- `rebase`
- `autofix`

Final command set:

- `automerge`
- `approve`
- `explain`
- `autoclose`

### RepairJobController

Responsibilities:

- Drive repair job state transitions.
- Create focused agent-runtime Tasks for repair.
- Run validation.
- Push only after deterministic preflight.
- Trigger exact-head re-review.
- Stop on budget exhaustion or explicit maintainer stop.

### AutomergeController

Responsibilities:

- Poll or receive exact-head review completion.
- Check GitHub required checks.
- Check mergeability.
- Check security and human-review state.
- Check global merge gate.
- Merge only when every gate passes.
- Record a complete audit event.

## Task Contracts

### Review Task Input

The review task should receive a single PR, not a repo-wide instruction.

Input:

- repo URL
- PR number
- base branch and SHA
- head branch and SHA
- diff
- changed files
- CI summary
- labels
- relevant prior review state
- repository policy
- validation expectations

The task should not need write credentials.

### Review Task Output

Require JSON with this shape:

```json
{
  "schemaVersion": "orka.prReview.v1",
  "repo": "owner/repo",
  "prNumber": 123,
  "headSHA": "abc123",
  "verdict": "needs_changes",
  "confidence": "high",
  "repairable": true,
  "summary": "Short maintainer-facing summary.",
  "findings": [
    {
      "priority": "P1",
      "confidence": "high",
      "file": "path/to/file.go",
      "line": 42,
      "title": "Finding title",
      "body": "Why this is a bug.",
      "recommendation": "What should change."
    }
  ],
  "security": {
    "status": "clear",
    "notes": ""
  },
  "tests": {
    "status": "not_run",
    "evidence": ""
  },
  "suggestedComment": "Draft prose. Orka may render or ignore this."
}
```

Allowed `verdict` values:

- `passed`
- `needs_changes`
- `needs_human`
- `security_sensitive`
- `skipped`

### Repair Task Input

Input:

- repo URL
- PR number
- exact head SHA
- repair intent
- findings to address
- failed check names and URLs, when applicable
- validation commands
- branch write mode
- constraints on files, tools, and allowed mutation

The repair task may use an agent runtime workspace. It should not directly
merge or close a PR.

### Repair Task Output

Require structured result with:

- intended PR number
- base SHA
- original head SHA
- final head SHA
- files changed
- summary
- validation commands run
- validation results
- unresolved issues
- patch or pushed branch metadata

## GitHub Comment and Marker Contract

Use deterministic rendering for comments. The model can suggest text, but Orka
must own final formatting and hidden markers.

Review identity marker:

```html
<!-- orka-monitor-review repo=owner/repo pr=123 -->
```

Verdict marker:

```html
<!-- orka-verdict:needs-changes repo=owner/repo pr=123 head_sha=abc123 confidence=high -->
```

Repair action marker:

```html
<!-- orka-action:fix-required repo=owner/repo pr=123 head_sha=abc123 finding=review-feedback -->
```

Security marker:

```html
<!-- orka-security:security-sensitive repo=owner/repo pr=123 head_sha=abc123 confidence=high -->
```

Command status marker:

```html
<!-- orka-command-status repo=owner/repo pr=123 command_id=456 intent=autofix -->
```

Rules:

- One current review/status comment per PR per monitor.
- One current command/status comment per command intent and head SHA.
- Automation never depends on visible prose.
- Hidden markers must include repository, PR number, and head SHA when the
  decision is head-specific.
- Stale markers are historical only.
- Markers should be signed if they are consumed as trust signals from GitHub.

## Maintainer Command Semantics

### Read-only commands

`status`

- Show current review, repair, automerge, and blocked state.
- Allowed for maintainers.
- Optionally allowed for PR authors.

`review` and `re-review`

- Create an exact-head review run.
- Do not repair or merge.
- Allowed for maintainers.
- Optionally allowed for PR authors on their own PRs.

`explain`

- Summarize why Orka is blocked or why a decision was made.
- Does not mutate labels, branches, PR state, or reviews.

### Control commands

`stop`

- Add the monitor pause label.
- Cancel or mark queued repair jobs stopped.
- Make stale autofix/automerge command comments ineligible to continue.

`approve`

- Clear a human-review pause only if policy allows.
- Does not bypass exact-head review, checks, mergeability, or merge gate.

### Repair commands

`fix ci`

- Create a repair job for terminal failing checks.
- Include failed check names and URLs in the repair prompt.

`address review`

- Create a repair job from unresolved Orka findings or configured review-bot
  feedback.

`rebase`

- Create a base-sync repair job.
- Prefer deterministic fast path for clean rebase.
- Fall back to agent repair only for conflicts.

`autofix`

- Add opt-in label.
- Create or resume a bounded repair loop.
- Never merge.

`automerge`

- Add opt-in label.
- Create or resume a bounded repair and merge loop.
- Merge only after deterministic automerge gates pass.

## State Machines

### Review State

```text
unseen
  -> queued
  -> running
  -> ingesting
  -> passed
  -> needs_changes
  -> needs_human
  -> security_sensitive
  -> skipped
  -> stale
  -> failed
```

Transitions:

- `queued -> running`: review Task created.
- `running -> ingesting`: task completed.
- `ingesting -> passed|needs_changes|needs_human|security_sensitive|skipped`:
  typed result validated.
- Any terminal state -> `stale`: live PR head SHA changed.
- Any active state -> `failed`: task or ingest failure.

### Repair State

```text
queued
  -> preflight
  -> running_agent
  -> validating
  -> push_preflight
  -> pushing
  -> re_review_queued
  -> waiting_review
  -> passed
  -> blocked
  -> stopped
  -> failed
```

Transitions:

- `preflight` re-fetches live PR state and checks policy.
- `running_agent` creates an agent-runtime Task.
- `validating` runs deterministic validation.
- `push_preflight` re-checks branch, head, repo, labels, and permissions.
- `pushing` applies the mutation.
- `re_review_queued` creates a new exact-head review.
- `waiting_review` waits for review result.
- `passed` means repair no longer needed.
- `blocked` means human action is required or budgets are exhausted.

### Automerge State

```text
not_requested
  -> requested
  -> reviewing
  -> repairing
  -> waiting_checks
  -> merge_preflight
  -> merging
  -> merged
  -> blocked
  -> stopped
```

Merge preflight must verify:

- Maintainer opted in.
- Global merge gate is enabled.
- PR is open and not draft.
- Live head SHA matches the passing review.
- Required checks pass.
- Mergeability is clean.
- No security-sensitive state blocks merge.
- No human-review or stop label blocks merge.
- Repair budgets are not exhausted.
- Repository policy allows the merge method.

## Deterministic GitHub Mutation Gates

Every write operation must call a shared preflight function.

Inputs:

- monitor spec
- intended operation
- target repo
- target PR or issue number
- expected head SHA, when applicable
- command event or review record source
- token identity

Checks:

1. Repository matches monitor scope.
2. Repository matches task transaction/workspace scope.
3. GitHub token is present and comes from the configured Secret.
4. Actor is authorized for the requested operation.
5. PR or issue is still open unless the operation explicitly handles closed
   items.
6. Live head SHA matches the expected SHA for head-specific operations.
7. Required labels are present.
8. Blocking labels are absent.
9. Branch is writable for push operations.
10. Security-sensitive state is handled according to policy.
11. Validation state is sufficient for push or merge.
12. CI/check state is sufficient for merge.
13. The operation is idempotent or has not already been applied.

Operations:

- post or update review/status comment
- add or remove label
- push branch
- create replacement PR
- close PR or issue
- merge PR

## Security Requirements

- Keep GitHub credentials in Kubernetes Secrets.
- Do not print, store, or return tokens in task results.
- Do not pass write tokens into review-only model tasks.
- Prefer read-only GitHub context for review tasks.
- Use write credentials only in deterministic executors.
- Redact tokens from logs, artifacts, task results, and audit metadata.
- Validate webhook signatures before reading or acting on payloads.
- Fail closed when repository scope cannot be established.
- Treat fork PRs and workflow-file changes as high-risk push cases.
- Treat security-sensitive findings as human-only unless explicitly opted in
  and policy allows repair.
- Keep ServiceAccount, OIDC, and context-token authorization checks aligned
  with repository monitor operations.

## API Plan

Add REST endpoints:

```text
POST   /api/v1/monitors/repositories
GET    /api/v1/monitors/repositories
GET    /api/v1/monitors/repositories/:name
PUT    /api/v1/monitors/repositories/:name
DELETE /api/v1/monitors/repositories/:name

POST   /api/v1/monitors/repositories/:name/runs
GET    /api/v1/monitors/repositories/:name/runs
GET    /api/v1/monitors/repositories/:name/items
GET    /api/v1/monitors/repositories/:name/items/:kind/:number

GET    /api/v1/monitors/reviews/:id
GET    /api/v1/monitors/repairs
GET    /api/v1/monitors/repairs/:id
POST   /api/v1/monitors/repairs/:id/stop
POST   /api/v1/monitors/repairs/:id/retry

POST   /api/v1/monitors/items/:kind/:number/review
POST   /api/v1/monitors/items/:kind/:number/autofix
POST   /api/v1/monitors/items/:kind/:number/automerge
POST   /api/v1/monitors/items/:kind/:number/stop

GET    /api/v1/monitors/events
```

Context-token scopes:

- `orka:monitors:read`
- `orka:monitors:write`
- `orka:monitors:operate`

## UI Plan

Add dashboard sections:

1. **Repository Monitors**
   - Monitor list.
   - Phase, repo, schedule, last run, pending reviews, active repairs,
     blocked items.

2. **Monitor Detail**
   - Configuration summary.
   - Manual run button.
   - Recent runs.
   - PR and issue queues.
   - Error and condition panels.

3. **PR Queue**
   - PR number, title, author, labels, head SHA, CI, review state, repair
     state, automerge state.
   - Filters for pending, needs changes, blocked, merge-ready, stale, failed.

4. **Review Detail**
   - Verdict.
   - Findings table.
   - Rendered public comment preview.
   - GitHub comment/review links.
   - Raw typed result.

5. **Repair Job Detail**
   - Intent and source command.
   - State timeline.
   - Tasks created.
   - Validation commands and results.
   - Push/review/merge gate status.
   - Stop and retry controls.

6. **Audit Events**
   - Append-only event feed.
   - Filter by monitor, PR, run, repair job, and operation.

## Compatibility and Migration

Keep the existing `create_pr_monitor` tool initially.

Migration path:

1. `create_pr_monitor` continues to create scheduled AI Tasks.
2. Add `create_repository_monitor` as the new tool for durable monitors.
3. Add an optional compatibility mode where `create_pr_monitor` creates a
   `RepositoryMonitor` when the feature gate is enabled.
4. Deprecate prompt-orchestrated scheduled PR monitors after parity features
   exist.

Backward compatibility requirements:

- Existing scheduled monitor Tasks keep working.
- Existing review markers are recognized.
- New marker parser accepts old `orka:pr-review` markers.
- Repo URL scope checks remain enforced.

## Implementation Phases

### Phase 0: Design Stabilization

Deliverables:

- Finalize `RepositoryMonitor` API shape.
- Decide feature gate names.
- Decide whether exact-event PR review shares the existing GitHub webhook
  endpoint or uses a monitor-specific route.
- Decide marker format and signing key source.
- Decide initial validation command behavior.

Acceptance criteria:

- API reviewed.
- Store schema reviewed.
- Security boundary documented.
- Existing PR monitor behavior unchanged.

### Phase 1: Durable Monitor API and Store

Deliverables:

- Add `RepositoryMonitor` CRD types.
- Generate CRDs and deepcopy code.
- Add SQLite store interfaces and migrations.
- Add API CRUD endpoints.
- Add basic UI list/detail skeleton.
- Add unit tests for defaults, validation, and store operations.

Acceptance criteria:

- A monitor can be created, listed, fetched, updated, and deleted.
- Status reflects basic monitor metadata.
- No GitHub calls are made yet.

### Phase 2: Deterministic PR Inventory and Run Selection

Deliverables:

- Implement GitHub PR inventory client using configured Secret.
- Add `monitor_runs` creation for manual and scheduled runs.
- Select eligible PRs deterministically.
- Store skip reasons.
- Add exact-event run creation for PR webhook events.

Acceptance criteria:

- A manual monitor run lists PRs and records monitor items.
- Draft, already-reviewed, blocked-label, and over-limit PRs are skipped with
  explicit reasons.
- No model task is required to decide which PRs need review.

### Phase 3: Focused Review Tasks and Typed Ingest

Deliverables:

- Create one review Task per selected PR.
- Define review input payload.
- Define JSON schema for review output.
- Parse and validate review output.
- Persist immutable review records.
- Mark malformed reviews as failed without posting model-owned comments.

Acceptance criteria:

- Review Tasks are focused on one PR.
- Review result is stored as typed data.
- Stale head SHA results are rejected or marked stale.

### Phase 4: Deterministic Comment Renderer and Markers

Deliverables:

- Add marker writer/parser package.
- Render public comments from typed review state.
- Create or update one Orka-owned PR comment.
- Preserve old marker compatibility.
- Add marker signing and previous-key support.

Acceptance criteria:

- Re-running review edits the same Orka comment.
- Automation does not depend on visible prose.
- A PR head SHA can be recognized as reviewed from SQLite and marker state.

### Phase 5: Maintainer Command Router

Deliverables:

- Extend webhook handling for issue comments and PR comments.
- Parse commands.
- Verify maintainer permissions.
- Deduplicate command events.
- Implement `status`, `review`, `re-review`, and `stop`.
- Render command/status comments.

Acceptance criteria:

- Unauthorized write commands are ignored or rejected according to policy.
- Maintainer `re-review` creates an exact-head review run.
- Maintainer `stop` blocks further repair/automerge activity.
- Command status is idempotent.

### Phase 6: Repair Job Model and Non-Merge Repair

Deliverables:

- Add `repair_jobs` store.
- Implement repair state machine.
- Support `fix ci`, `address review`, and `rebase`.
- Add repair Task input/output contracts.
- Run validation before push.
- Re-review exact head after push.

Acceptance criteria:

- Maintainer can opt into repair.
- Repair is bounded by per-PR and per-head limits.
- Pushes are preflighted and audited.
- Repairs trigger exact-head re-review.
- Failed validation blocks push unless policy explicitly permits artifact-only
  output.

### Phase 7: Autofix Loop

Deliverables:

- Add `autofix` command and opt-in label handling.
- Create or resume durable autofix jobs.
- Loop review -> repair -> validation -> push -> re-review.
- Stop on pass, blocked, stopped, stale head, or budget exhaustion.

Acceptance criteria:

- Autofix never merges.
- Each loop iteration is visible in API/UI.
- Per-head and per-PR budgets are enforced.
- Human stop takes effect immediately.

### Phase 8: Automerge

Deliverables:

- Add `automerge` and `approve` command handling.
- Add global merge gate flag.
- Add merge preflight function.
- Add merge operation with full audit.
- Add automerge status timeline.

Acceptance criteria:

- Automerge requires maintainer opt-in.
- Draft PRs do not merge.
- Stale reviews do not merge.
- Failing checks do not merge.
- Security-sensitive or human-review states do not merge.
- Merge only happens after exact-head passing review and deterministic gates.

### Phase 9: UI Completion and Operations

Deliverables:

- Complete monitor detail and queue pages.
- Add repair and automerge timelines.
- Add audit event view.
- Add metrics for monitor runs, reviews, repairs, skips, failures, and merges.
- Add operational docs.

Acceptance criteria:

- Operators can understand why an item is pending, skipped, blocked, repaired,
  or merged without reading controller logs.
- All automated GitHub writes have audit events.

### Phase 10: Issue Review and Implementation PRs

Deliverables:

- Add issue inventory and review state.
- Add issue review schema.
- Add advisory label sync.
- Add guarded issue implementation PR lane.
- Keep implementation PR lane non-merging by default.

Acceptance criteria:

- Orka can review issues and provide maintainer-facing status.
- Orka can open implementation PRs only for narrow, reproducible, policy-allowed
  issues.
- Ambiguous, product, security, and broad issues stay human-only.

### Phase 11: Commit Review

Deliverables:

- Add manual commit review endpoint.
- Add code-bearing commit classifier.
- Add per-SHA review records.
- Add optional finding-to-repair intake.

Acceptance criteria:

- Maintainers can request commit review for a SHA or range.
- Reports are stored per commit.
- Repair intake only accepts narrow, non-security, still-relevant findings.

## Testing Plan

### Unit Tests

- CRD defaulting and validation.
- Store migrations and CRUD.
- Monitor run selection.
- PR eligibility and skip reasons.
- Marker rendering, parsing, signing, and rotation.
- Command parsing.
- Permission decision logic.
- Mutation preflight logic.
- State-machine transitions.
- Review JSON validation.
- Repair output validation.

### Integration Tests

- Manual monitor run with fake GitHub server.
- Scheduled run creates review Tasks.
- Completed review Task updates store and comment.
- Re-review on same head edits existing comment.
- New head SHA marks previous review stale.
- Maintainer command creates exact review.
- Stop command blocks queued repair.
- Repair job validates and pushes in fake git remote.
- Automerge blocks on failing gates.

### E2E Tests

- Kind deployment with fake GitHub API.
- Webhook HMAC validation.
- Exact PR event review.
- Comment command routing.
- Autofix loop with one repair.
- Automerge happy path.
- Automerge blocked cases:
  - stale head
  - draft PR
  - failing CI
  - missing global merge gate
  - protected label
  - unauthorized commenter
  - repo scope mismatch

### Security Tests

- Token redaction in logs, task results, audit metadata, and errors.
- Repo URL mismatch rejected for all GitHub tools.
- Fork branch write restrictions.
- Workflow file mutation restrictions.
- Security-sensitive review blocks repair and merge.
- Context-token constraints enforce namespace, repo, branch, model, and tools.

## Metrics

Add Prometheus metrics:

- `orka_repository_monitor_runs_total`
- `orka_repository_monitor_run_duration_seconds`
- `orka_repository_monitor_items_selected_total`
- `orka_repository_monitor_items_skipped_total`
- `orka_repository_monitor_reviews_total`
- `orka_repository_monitor_review_duration_seconds`
- `orka_repository_monitor_repairs_total`
- `orka_repository_monitor_repair_duration_seconds`
- `orka_repository_monitor_mutations_total`
- `orka_repository_monitor_mutation_failures_total`
- `orka_repository_monitor_automerge_attempts_total`
- `orka_repository_monitor_automerge_success_total`
- `orka_repository_monitor_automerge_blocked_total`

Useful labels:

- `namespace`
- `monitor`
- `repo`
- `trigger`
- `phase`
- `verdict`
- `skip_reason`
- `intent`
- `operation`

## Rollout Plan

1. Ship CRD and API behind a feature gate.
2. Enable monitor inventory in a test namespace with fake GitHub.
3. Enable review-only mode on one low-risk repository.
4. Enable exact-event review.
5. Enable command router for read-only commands.
6. Enable repair commands with artifact-only output.
7. Enable branch push for same-repo PRs.
8. Enable branch push for safe fork PRs, if policy allows.
9. Enable autofix.
10. Enable automerge only with global merge gate disabled.
11. Enable global merge gate for one repository after audit review.

Rollback:

- `spec.suspend: true` stops scheduled work.
- Feature gate disables monitor controllers.
- Webhook command router can be disabled independently.
- Global merge gate defaults off.
- Existing scheduled PR monitor Tasks continue to work.

## Open Questions

1. Should `RepositoryMonitor` be namespace-scoped like `RepositoryScan`?
2. Should monitor state live in the existing SQLite store or a dedicated
   database file?
3. Should exact-event review use the existing `/webhooks/github` endpoint or a
   monitor-specific endpoint?
4. Should Orka use GitHub App installation tokens as the preferred credential
   model?
5. Should comments be GitHub reviews, issue comments, or both?
6. Which labels should be built in versus configured per monitor?
7. Which validation commands should be inferred from repository files versus
   configured explicitly?
8. Should automerge support only squash initially?
9. How should Orka handle unresolved third-party review-bot comments?
10. Should issue implementation PRs require an explicit maintainer command, or
    can they be triggered from high-confidence review findings?

## Initial Milestone Breakdown

### Milestone 1: Review-only parity foundation

Scope:

- `RepositoryMonitor` CRD.
- Durable monitor runs and items.
- Deterministic PR selection.
- Focused review Tasks.
- Typed review results.
- Single mutable review comments.

This milestone replaces prompt-orchestrated repo-wide PR review with durable,
deterministic review orchestration.

### Milestone 2: Maintainer command parity

Scope:

- Comment command webhook.
- Permission checks.
- `status`, `review`, `re-review`, and `stop`.
- Command status comments.
- Audit events.

This milestone gives maintainers direct control without enabling code mutation.

### Milestone 3: Repair parity

Scope:

- Repair jobs.
- `fix ci`, `address review`, `rebase`, and `autofix`.
- Validation gates.
- Push preflight.
- Exact-head re-review.
- Repair budgets.

This milestone makes Orka able to repair opted-in PRs while keeping final merge
manual.

### Milestone 4: Automerge parity

Scope:

- `automerge` and `approve`.
- Merge preflight.
- Global merge gate.
- Status timeline.
- Merge audit.

This milestone should be enabled only after repair behavior is stable.

### Milestone 5: Expanded ClawSweeper lanes

Scope:

- Issue review.
- Advisory labels.
- Guarded issue implementation PRs.
- Manual commit review.

This milestone expands beyond PR monitor parity into the rest of ClawSweeper's
maintenance surface.

## Definition of Done

The PR monitor parity work is complete when:

- A `RepositoryMonitor` can run on schedule and from exact PR events.
- The controller, not the model, selects PRs for review.
- Every review result is typed, stored, and tied to an exact head SHA.
- Orka maintains one mutable public review/status surface per PR.
- Maintainer commands are authenticated, authorized, idempotent, and audited.
- Opted-in PRs can be repaired through bounded loops.
- Every push is validated and preflighted.
- Every repair triggers exact-head re-review.
- Automerge is possible only through deterministic gates.
- UI and API expose enough state to explain every pending, skipped, blocked,
  repaired, and merged item.
- All GitHub writes are covered by tests for stale head, repo mismatch,
  unauthorized actor, protected label, security-sensitive state, and failed
  validation.
