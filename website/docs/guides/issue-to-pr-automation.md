# Issue-to-PR Automation

RepositoryMonitor can run a durable maintainer-controlled issue-to-PR loop from `orka:*` labels or equivalent API/CLI/UI commands.

## Flow

1. A maintainer labels an issue with `orka:implement` (or runs `orka monitor issue implement <monitor> <number>`).
2. Orka verifies the webhook signature and current GitHub actor permission, then records a durable `command_event`.
3. A `work_action` is queued with the monitor generation, target snapshot digest, dedupe key, and command ID.
4. Orka inventories the issue and computes a content digest that excludes Orka-authored labels/comments.
5. Triage, research, and planning run as read-only agent tasks when missing or stale.
6. If policy requires approval, the workflow stops until `orka:approve-plan` or the equivalent CLI/API/UI command.
7. Implementation runs as a patch-only task. The agent returns an `orka.issueImplementation.v1` result and a diff.
8. The controller validates and stores an `orka.patch.v1` artifact, then creates a separate mutation task with a configured push branch.
9. The mutation task applies/pushes the branch; the controller creates or reuses the PR and records GitHub mutation audit rows.
10. PR review and repair continue on exact heads until the PR reaches `merge_ready` or a clear blocked state.

## Safety model

- Issue and PR text is untrusted input.
- Read-only agents never receive GitHub mutation credentials.
- Code-changing tasks must produce a validated patch artifact before any branch push.
- GitHub writes are controller-owned and recorded in `github_mutation_records`.
- Stop commands cancel queued workflow actions and prevent post-task mutation from stale task results.
- Plans and implementation are bound to issue content digests; human edits make downstream artifacts stale.

## CLI quick reference

```bash
orka monitor issue plan orka-main 123
orka monitor issue approve-plan orka-main 123
orka monitor issue implement orka-main 123
orka monitor issue implementation get orka-main 123
orka monitor mutations list orka-main --kind issue --number 123
orka monitor pr review orka-main 456
orka monitor pr fix orka-main 456
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

`spec.issueWorkflow.implementation` bounds code-changing work before a mutation task can push a branch:

- `maxActive` caps active issue implementation/mutation jobs per monitor (default `2`).
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


## Fake-GitHub validation

Run the integrated, secret-free validation suite locally with:

```bash
make repository-monitor-fake-e2e
```

For the broader local validation bundle that also checks generated CLI docs, example manifests, website docs, and workflow syntax, run:

```bash
make repository-monitor-validate
```

The suite covers durable command intake, replay/coalescing, guard-label blocking, issue implementation to PR, stop/resume late-task safety, PR review/repair/readiness, and optional automerge against fake GitHub servers. The `Repository Monitor Smoke` GitHub Actions workflow runs the same fake-GitHub E2E script on relevant PRs.

Patch previews are available through `orka monitor issue patch preview <monitor> <issue-number>` or `GET /api/v1/monitors/implementation-jobs/{id}/patch-preview`; the endpoint returns safe `orka.patch.v1` metadata instead of blindly streaming arbitrary task output.


## Live GitHub/kind preflight

The optional live/manual E2E requires Docker, kind, kubectl, the local Orka images, and a target GitHub repository/issue. Check local prerequisites without changing the cluster with:

```bash
make repository-monitor-live-preflight
```

If Docker is not running, the preflight exits before creating or modifying a kind cluster.


## Completion audit helper

Run the local validation bundle plus the live preflight with:

```bash
make repository-monitor-completion-audit
```

The audit exits non-zero when the live preflight is blocked (for example, Docker is not running), but still prints which RepositoryMonitor requirements are covered by the local fake-GitHub validation bundle.
