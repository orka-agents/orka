---
description: "The orka:implement workflow from issue planning through pull request readiness."
---

# Issue-to-PR automation

RepositoryMonitor runs a durable issue-to-PR loop from `orka:implement` or the equivalent API/CLI/UI command. Planning, implementation, PR review, and bounded repair continue automatically when policy permits them.

## Flow

1. A maintainer labels an issue with `orka:implement` (or runs `orka monitor issue implement <monitor> <number>`).
2. Orka verifies the webhook signature and current GitHub actor permission, then records a durable `command_event`.
3. A `work_action` is queued with the monitor generation, target snapshot digest, dedupe key, and command ID.
4. Orka inventories the issue and computes a content digest that excludes Orka-authored labels/comments.
5. With `spec.issueWorkflow.implementation.requirePlan: true`, the default, Orka queues a read-only planning task if there is no current ready plan. Triage and research remain available through ad hoc API/CLI commands.
6. A current `ready` plan continues the original implement command automatically. A `blocked` or `needs_human` result stops progression and records the reason.
7. Implementation runs in a writable sanitized workspace on a controller-selected push branch. The agent leaves changes for the separate Workspace Publisher, which freezes and validates the delta, publishes it, and verifies the remote.
8. The controller accepts only a matching `VerifiedExact` delivery receipt, then creates or reuses the PR and records GitHub mutation audit rows.
9. PR review and bounded repair continue on exact heads until the PR reaches `merge_ready` or a clear blocked state.

Managed workflows with `review.publish.enabled: true` publish the commit status `orka/<namespace>/<monitor-name>/ready`. Require this status alongside your CI and approving-review rules. GitHub's per-PR auto-merge setting owns merging. Orka never enables it or calls the merge endpoint; when it is disabled, the PR stays open and ready.

`spec.suspend` pauses background monitor runs; queued and manual runs can still finish. Use `orka:pause` to block a specific issue or PR and its readiness. Pause labels rely on observed GitHub state; use an explicit `stop` command for a durable halt independent of label propagation.

Before disabling readiness publication, deleting a monitor, or changing its repository or tracked branch, remove or replace its required status in the affected GitHub repository's branch protection. GitHub commit statuses persist after the monitor is removed or repointed, and Orka does not currently revoke them as part of these configuration changes.

## Safety model

- Issue and PR text is untrusted input.
- Read-only agents never receive GitHub mutation credentials or direct Git credentials. Claude and OpenCode roles receive only scoped read tools with Bash denied. The controller supports Codex for pull-request review and read-only issue triage, research, and planning. Codex runs inside the RuntimeSession boundary with controller-rejected elevation requests and read-intent delta classification. The monitor API currently accepts Codex only for the reviewer role. Copilot and external `runtimeRef` runtimes are rejected for this hardened mode.
- Implementation agents receive only runtime model credentials and a pre-cloned writable workspace, never Git push credentials. Codex and Claude are supported; Copilot is rejected because its runtime credential can mutate GitHub.
- Code-changing tasks must produce a validated patch artifact before any branch push.
- GitHub writes are controller-owned and recorded in `github_mutation_records`.
- Stop commands cancel queued workflow actions and active monitor Tasks, and prevent post-task mutation from stale task results.
- Repair commands execute only when `spec.repair.enabled` is true and remain bounded by `maxRepairsPerPR` and `maxRepairsPerHead` when configured.
- Plans and implementation are bound to issue content digests; human edits make downstream artifacts stale.
- `orka:pause` blocks further workflow actions and readiness. A running bounded Task may finish, including publication. Removing the label queues fresh reconciliation; unresolved plans still block implementation.
- Policy labels follow current GitHub repository state. `triggers.github.labels.requireActorPermission` authorizes command and webhook intake; it does not override GitHub permissions for editing policy labels. A rejected pause-label webhook does not queue a run, but later inventory still observes the repository's labels.

Pause-label intake and workflow reconciliation can update the same item concurrently. A stale write can overwrite the stored label or workflow progress. Coordinating these writes atomically remains a follow-up; the current workflow does not guarantee pause enforcement across this race.

With automatic repair enabled and readiness publication disabled, full workflow polls fetch details for every open PR before applying `maxPerRun`. That limit bounds started work, not GitHub reads. Fair, bounded refresh and caching remain a follow-up for repositories with many open PRs.

## CLI quick reference

```bash
orka monitor issue plan orka-main 123
orka monitor issue implement orka-main 123
orka monitor issue implementation get orka-main 123
orka monitor mutations list orka-main --kind issue --number 123
orka monitor pr review orka-main 456 --target-sha '<head-sha>'
orka monitor pr fix orka-main 456 --target-sha '<head-sha>'
orka monitor pr ready readiness orka-main 456
orka monitor work-actions list orka-main --kind issue --number 123
```

## Debugging blocked work

Use the workflow timeline first:

```bash
orka monitor work-actions list orka-main --status blocked
orka monitor doctor orka-main
```

Blocked records include a low-cardinality reason such as `stale_command_snapshot`, `security_sensitive`, `patch_path_denied`, `validation_failed`, or `stopped_by_command`. The dashboard shows the same blocked reason alongside implementation jobs and GitHub mutation records.


## Implementation budgets and path policy

`spec.issueWorkflow.implementation` bounds code-changing work and publication:

- `maxActive` caps active issue implementation jobs per monitor (default `2`).
- `maxAttemptsPerIssue` caps implementation attempts for one issue (default `2`).
- `maxChangedFiles` caps files in an `orka.patch.v1` artifact (default `12`).
- `allowedPaths` optionally restricts patch files to monitor-owned glob/prefix allowlists such as `api/**`, `internal/**`, or `docs/**`.

Denied paths and secret scanning are always enforced before allow-list checks.


## Rate-limit and retry states

Monitor runs classify transient infrastructure failures into low-cardinality states that are written to monitor events and metrics:

- `github_rate_limited` for GitHub primary/secondary rate-limit responses.
- `llm_rate_limited` for model-provider throttling surfaced through workflow errors.
- `cluster_capacity_blocked` for Kubernetes capacity/quota pressure.
- `retry_scheduled` for retryable transient failures.

Use `orka monitor events <monitor> --event-type run_failed` or the dashboard audit/timeline panels to see the state attached to failed runs.

Controller-created PRs start a targeted inventory run. Transient failures have bounded retries. If that run fails terminally before creating a PR inventory item, correcting credentials alone does not restart discovery when no schedule or PR event queues another inventory run. After correcting the cause, create a fresh targeted run without a head SHA so inventory fetches the current PR:

```bash
orka monitor run orka-main --namespace orka --target-kind pull_request --target-number 456
```

Automatic rediscovery after credential correction remains a follow-up. Direct PR review commands require an existing inventory item and its current head SHA.


## Fake-GitHub validation

Run the integrated, secret-free validation suite locally with:

```bash
make repository-monitor-fake-e2e
```

For the broader local validation bundle that also checks generated CLI docs, example manifests, website docs, and workflow syntax, run:

```bash
make repository-monitor-validate
```

The suite covers durable command intake, replay/coalescing, guard-label blocking, issue implementation to PR, stop/resume late-task safety, and PR review/repair/readiness against fake GitHub servers. The `Repository Monitor Smoke` GitHub Actions workflow runs the same fake-GitHub E2E script on relevant PRs.

Patch previews are available through `orka monitor issue patch preview <monitor> <issue-number>` or `GET /api/v1/monitors/implementation-jobs/{id}/patch-preview`; the endpoint returns safe `orka.patch.v1` metadata instead of blindly streaming arbitrary task output.


## Completion audit helper

Run the local validation bundle and report the remaining live validation with:

```bash
make repository-monitor-completion-audit
```

The audit exits non-zero if local validation fails. Live model execution, publication, and GitHub rule enforcement require separate validation against an explicitly selected repository.
