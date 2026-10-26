---
slug: /repository-monitors
description: "Watching a repository and having agents triage, review, and repair on a schedule."
---

# Repository monitors

A `RepositoryMonitor` puts a GitHub repository under agent care. It reviews pull requests as
they arrive and posts the findings back, and it can also plan and implement issues, repair
pull requests when review or CI asks, and publish a readiness check that GitHub's auto-merge
can wait on. Every run, item, review, and GitHub write is recorded durably and shown in the
API, the CLI, and the dashboard's **Monitors** page.

Reviews run as read-only coding-agent Tasks bound to the exact pull request head. GitHub
writes — review comments, branch pushes, pull requests, labels, and statuses — are made by
Orka's controller and publisher and recorded in an audit log, never by an agent. GitHub's own auto-merge owns merging; Orka publishes
readiness and never merges.

:::tip[Runnable examples]
[`repository-monitor-pr-review-repair`](https://github.com/orka-agents/orka/tree/main/examples/repository-monitor-pr-review-repair)
reviews and repairs pull requests;
[`repository-monitor-issue-plan-only`](https://github.com/orka-agents/orka/tree/main/examples/repository-monitor-issue-plan-only)
triages and plans issues without writing code. To go from an issue label to a merge-ready
pull request, see [Issue-to-PR automation](issue-to-pr-automation.md).
:::

## What it does

A repository monitor can:

- list open pull requests for one GitHub repository and base branch
- inventory open GitHub issues, excluding pull-request-shaped issues
- skip drafts unless explicitly configured to include them
- skip PRs blocked by configured protected or pause labels
- skip PR heads that already have a fresh review result
- queue one read-only review task per selected PR head
- refetch one pull request for targeted manual and webhook runs before queueing review work
- queue exact-head monitor runs from GitHub pull request webhook events
- ingest typed JSON review results from completed review tasks
- store monitor runs, issue/PR items, command events, workflow actions, action records, implementation jobs, mutation records, review records, and audit events durably
- show monitor status, recent runs, workflow timeline, blocked reasons, implementation jobs, mutation audit, issues, and the PR queue in the dashboard under **Monitors**

The review Task is bound to the exact PR head SHA. It runs as a `type: agent` Task with `workspace.intent: read`, receives no Git credential, and must leave the verified tree unchanged. If `spec.review.publish.enabled` is true, the controller later revalidates the PR state and may publish a deterministic neutral `COMMENT` review with `spec.forgeCredentialRef`.

## Prerequisites

Create a built-in runtime Agent in the same namespace as the monitor, or set `spec.agents.reviewer.namespace` explicitly. Built-in runtime Agents must not set `spec.secretRef`; provider authentication is supplied by the controller-managed runtime proxy.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: repo-reviewer
  namespace: orka-system
spec:
  runtime:
    type: claude
    contractVersion: orka.harness.v2
    defaultMaxTurns: 50
    defaultAllowedTools:
      - Read
      - Grep
      - Glob
  model:
    name: claude-opus-5
  systemPrompt:
    inline: |
      Review the exact pull request head for correctness, tests, security, and maintainability.
      Return concise, structured findings and do not mutate GitHub.
```

For private repositories or higher GitHub rate limits, create a source-read Secret in the monitor namespace. When a monitor is created or updated through the API, Orka validates that each configured credential Secret exists and contains a non-empty `token`, `password`, or `GITHUB_TOKEN` key.

```bash
kubectl create secret generic repo-monitor-source-read \
  --namespace orka-system \
  --from-literal=token='<read-only-github-token>'
```

For review, triage, research, and planning Tasks, the controller maps `RepositoryMonitor.spec.readCredentialRef` to top-level `workspace.readCredentialRef`. `spec.gitSecretRef` is accepted only as a compatibility fallback for this source-read role. The clean-room source boundary resolves it for same-repository private PR heads; the ACP runtime never receives the Secret. Fork PR heads use the eligible source repository URL and remain read-only.

Implementation and repair are write workflows and require four explicit, pairwise-distinct Secret references:

| RepositoryMonitor field | Task workspace role | Required capability |
| --- | --- | --- |
| `readCredentialRef` | `workspace.readCredentialRef` | Source clone/read only |
| `publicationReadCredentialRef` | `workspace.publicationReadCredentialRef` | Target preflight and independent verification only |
| `publicationCredentialRef` | `workspace.publicationCredentialRef` | Exact compare-and-swap branch push |
| `forgeCredentialRef` | `workspace.forgeCredentialRef` | Controller-owned GitHub API operations and PR reconciliation |

`gitSecretRef` never supplies a write, publication, or forge role. A write Task is not created when any explicit role is missing or when two roles reference the same Secret. Credential values are resolved only by the controller/Workspace Publisher brokers and never appear in ACP process environment, prompts, Task status, or delivery receipts.

## Create a monitor

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: RepositoryMonitor
metadata:
  name: example-app
  namespace: orka-system
spec:
  provider: github
  repoURL: https://github.com/example/app
  branch: main
  readCredentialRef:
    name: repo-monitor-source-read
  schedule: "*/30 * * * *"
  timeZone: "UTC"
  targets:
    pullRequests:
      enabled: true
      includeDrafts: false
      maxPerRun: 10
  agents:
    reviewer:
      name: repo-reviewer
  review:
    event: COMMENT
    staleReviewTTL: 24h
    exactEventEnabled: true
  policy:
    protectedLabels:
      - security-sensitive
    pauseLabels:
      - orka:pause
  validation:
    image: ghcr.io/example/app-validation@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
```

Apply it with:

```bash
kubectl apply -f repository-monitor.yaml
```

The controller normalizes `provider`, `owner`, `repository`, `branch`, pull request enablement, `maxPerRun`, and `review.event` when omitted. `review.publish.enabled` defaults to `false`; when enabled, V1 rejects publish events other than `COMMENT` and same-head policies other than `skip`.

If an existing monitor still uses `validation.mode` and `validation.commands`, migrate it before upgrading the controller. Apply the new CRD first, replace the legacy fields with a digest-pinned `validation.image`, and remove the old fields. The compatibility schema preserves a non-empty legacy command list long enough for the controller to report `LegacyValidationCommandsUnsupported`; it will not run the monitor with validation silently disabled.

## Run manually

Scheduled runs are queued from `spec.schedule` when the monitor is not suspended. You can also trigger a manual run through the API:

```bash
curl -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  "https://<orka-api-host>/api/v1/monitors/repositories/example-app/runs?namespace=orka-system" \
  -d '{}'
```

To target one pull request, include `targetKind` and `targetNumber`:

```json
{
  "targetKind": "pull_request",
  "targetNumber": 123
}
```

`targetSHA` can also be supplied to require an exact head SHA match.

When `targetNumber` is set, the controller fetches that pull request directly from GitHub before applying the monitor's open-state, base-branch, draft, label, stale-review, and optional `targetSHA` checks. Targeted runs do not retire missing or out-of-scope items from the repository-wide inventory, so monitor status counts continue to summarize the stored PR queue rather than only the targeted PR.

## Run from GitHub events

Repository monitors can also receive exact pull request events through the same signed GitHub webhook endpoint used by label triggers. Configure the repository webhook for `Pull requests` events and set `spec.review.exactEventEnabled: true` on the monitor.

When `/webhooks/github` receives an `opened`, `reopened`, `synchronize`, `ready_for_review`, `labeled`, or `unlabeled` pull request event, Orka matches monitors by repository and base branch. If the controller has a watch namespace, only monitors in that namespace are considered; otherwise monitors across all namespaces are eligible. A matching monitor queues a run with `targetKind: pull_request`, the PR number, and the exact head SHA from the webhook payload. Replayed deliveries and already-queued runs for the same PR head are accepted without creating duplicate monitor work. If a previous exact-event run for the same delivery failed before the queued audit event was recorded, a webhook retry can requeue that failed run.

Exact event runs are still read-only review runs. They are stored with trigger `pull_request_event`, create an `exact_event_run_queued` audit event, and wait behind any active or queued monitor run. When the run executes, Orka refetches the current pull request by number and skips review work if the PR is no longer open, moved to another base branch, or no longer matches the event head SHA.

## Inspect state

Use `kubectl` for CRD-level status:

```bash
kubectl get repositorymonitors -n orka-system
kubectl describe repositorymonitor example-app -n orka-system
```

Use the API or dashboard for durable run and item state:

```bash
curl -H "Authorization: Bearer $TOKEN" \
  "https://<orka-api-host>/api/v1/monitors/repositories?namespace=orka-system"

curl -H "Authorization: Bearer $TOKEN" \
  "https://<orka-api-host>/api/v1/monitors/repositories/example-app/runs?namespace=orka-system"

curl -H "Authorization: Bearer $TOKEN" \
  "https://<orka-api-host>/api/v1/monitors/repositories/example-app/items?namespace=orka-system&kind=pull_request"

curl -H "Authorization: Bearer $TOKEN" \
  "https://<orka-api-host>/api/v1/monitors/events?namespace=orka-system&name=example-app"
```

The embedded dashboard shows the same state under **Monitors**:

- monitor list with phase, schedule, repository, and summary counts
- detail page with open PR count, pending reviews, blocked items, merge-ready count, recent runs, and PR queue
- manual **Run** action for an immediate monitor run

## Review workspace context

The review Task is pinned to the exact PR head SHA with `workspace.intent: read`. Before creating it, the controller fetches the pull request identity, lists the pull request's changed files with their patches, and refetches the pull request to ensure the base, head, and head repository did not change during context assembly. The drift check runs even when the file listing failed, so a race fails closed instead of queueing a stale review; a GitHub read failure does not fail the run and instead marks the context `contextUnavailable`.

The controller embeds a bounded `orka.prReview.context.v1` payload in the prompt: at most 700 KiB encoded context, patch excerpts (at most 64 KiB encoded each) for the first 100 changed files, and path-only identity entries (`patchOmitted: "capped"`) for up to 2,000 changed files, with bounded paths/short metadata fields. Identities take precedence over patches inside the byte budget: patches are dropped first, and `truncated.files` is set only when the complete change set could not be represented — a missing identity, a compare listing shorter than the pull request's `changed_files` total (GitHub caps the compare file array at 300 entries), or an omitted patch that hides content the checkout cannot show (a removed file, a renamed file's previous contents, or deleted lines in any changed file). Because the checkout carries no Git metadata, the prompt then requires a non-`passed` verdict (`needs_human` or stricter). Patches and paths pass through the shared credential redaction before they are persisted in the Task prompt. The prompt treats titles, labels, paths, patches, and repository content as untrusted data and tells the reviewer to inspect the verified checkout whenever GitHub context is incomplete.

If a Task with the predictable review name already exists, the controller adopts it only when its spec is byte-identical to the freshly rendered review (including the diff context) or carries only the controller-shaped `contextUnavailable` envelope; any other pre-existing Task fails the run.

The reviewer must not mutate the verified tree. Any unexpected workspace change fails read validation. GitHub review publishing, when enabled, happens later through the controller's deterministic publisher path; the ACP child has no GitHub mutation credential.

If `spec.validation.image` is configured, the review Task also receives the brokered `run_validation` tool. The reviewer inspects the repository and supplies one shell command. Orka creates one container Task with the configured image, a 45-minute timeout, and the same exact-head read-only workspace. The checkout init container finishes first. Orka then installs a deny-all NetworkPolicy before releasing the validation command. The reviewer cannot change the image, checkout, credentials, publication settings, or network policy. It can wait only for its own child Task through `wait_for_tasks` before returning the review result.

The image is repository-specific, not language-specific. It must contain `/bin/sh` plus every compiler, linter, package manager, dependency, or infrastructure CLI that validation may need. The command cannot download modules, packages, providers, or contact cloud APIs. A Go repository that uses `golangci-lint` should point at a Go-based image with dependencies and `golangci-lint` installed. Repositories that run offline Terraform or Azure CLI checks should include those tools and required local data. Maintainers can build and publish that image once, then reuse it in the repository's monitor configuration.

Validation fails closed when configured. A reviewer that does not call the tool, a failed command, a non-terminal child Task, or a child Task whose image or checkout no longer matches the review cannot produce a `passed` or merge-ready result. Orka stores the image, command digest, status, and safe evidence in the durable review record. The validation binding retains the same SHA-256 command digest. Validation stdout and stderr are suppressed so repository or fixture secrets cannot enter Pod logs or result storage. A failure before workload execution is recorded as `unavailable`, and the same head remains eligible for a later review. No validation Task is required when `spec.validation.image` is empty; the review records validation as `not_run`.

## Review results

Review tasks must return a JSON object with schema version `orka.prReview.v1`. The controller validates the repository, PR number, and exact head SHA before accepting the result. When validation is configured, the controller also verifies the child validation Task independently of the reviewer's reported test status. Accepted results are stored as immutable review records and copied onto the current monitor item.

Valid review verdicts are:

- `passed`
- `needs_changes`
- `needs_human`
- `security_sensitive`
- `skipped`

For status summaries, open PRs with `needs_changes`, `needs_human`, `security_sensitive`, stale, failed, or skipped review state count as blocked items. Open PRs with queued review work count as pending reviews.

If a review task fails, is cancelled, returns malformed JSON, or returns a stale head SHA, the controller records a rejected review result and leaves an audit event explaining why. If GitHub publishing is enabled, the controller performs publish-time safety checks immediately after ingestion: it refetches the PR, requires the PR to remain open on the monitor base branch and exact reviewed head SHA, rejects draft or protected-label PRs, skips duplicate same-head publications using Orka publish records and hidden GitHub markers, neutralizes mentions in rendered text, and never posts `security_sensitive` results unless explicitly configured.

If GitHub returns HTTP 401 during publication, the controller records `github_authentication_failed` and retries after the existing five-minute publish cooldown. Each attempt reloads `spec.forgeCredentialRef`, so refreshing an expired GitHub App installation token lets the controller publish the completed review without running the reviewer again. Every retry repeats the PR safety and duplicate checks. Orka does not refresh the token itself; an invalid or revoked credential continues to fail until replaced. Non-rate-limited HTTP 403 and HTTP 404 responses remain terminal permission failures. Older attempts already recorded as `github_permission_denied` are not automatically reclassified.

## Issues

Repository monitors can also inventory open GitHub issues when `spec.targets.issues.enabled: true`. Issue inventory excludes GitHub issues that represent pull requests, stores the item as `monitor_items.kind = issue`, and records an issue content digest over human-controlled inputs: issue number, title, body, and non-`orka:*` / non-`orka-state:*` labels. Orka-authored command labels and state labels therefore do not stale issue plans or future issue workflow artifacts.

A monitor can be run against one exact issue without retiring unrelated inventory:

```bash
orka monitor run orka-main --target-kind issue --target-number 123 --namespace orka-system
orka monitor issues list orka-main --namespace orka-system
```

Label commands — `orka:implement` to start the workflow and `orka:pause` to hold an item —
are covered in [Issue-to-PR automation](issue-to-pr-automation.md#start-from-a-github-label).
Guard labels from `spec.policy.protectedLabels` and `spec.policy.pauseLabels` record blocked
commands and do not queue work.

Inspect command intake with:

```bash
orka monitor commands list orka-main --namespace orka-system
orka monitor commands get '<command-id>' --namespace orka-system
```

### Triage, research, planning, and implementation

`orka:implement` runs planning when a required ready plan is missing, then continues to implementation if policy permits it. Triage and research are ad hoc API/CLI operations:

- `triage` API/CLI command creates a read-only issue triage task and stores an `issue_triage` action record.
- `research` API/CLI command creates a read-only issue research task and stores an `issue_research` action record.
- `plan` API/CLI command creates a read-only planning task and stores an `issue_plan` action record. Unresolved plans block implementation with a reason.
- `orka:implement` creates an implementation task only when policy permits it. By default, implementation requires a ready plan; otherwise Orka queues planning first.

Issue action tasks are bound to the issue snapshot digest. Result payloads with mismatched issue numbers or stale digests are recorded as stale/failed action records instead of advancing workflow state.

Implementation Tasks use `workspace.intent: write`, a controller-selected push branch (`spec.issueWorkflow.implementation.branchPrefix`, default `orka/issue`), and the four explicit credential roles above. The ACP child edits only the sanitized workspace and receives no Git or forge credential. After prompt completion, the separate Workspace Publisher freezes and validates the delta, performs the exact publication, independently verifies the remote, and records a non-secret `status.delivery` receipt.

The controller advances the workflow only from a `VerifiedExact` delivery whose publication repository, branch, expected commit, verified remote SHA, prior remote SHA, and artifact digest all match the Task contract. Missing, conflicting, superseded, cancelled, credential-blocked, or otherwise unverifiable delivery is recorded as a blocked implementation. After verified publication, the controller uses `forgeCredentialRef` to create or reuse the pull request with a deterministic Orka-rendered body.

Inspect action records with:

```bash
orka monitor actions list orka-main --namespace orka-system --kind issue --number 123
orka monitor actions get '<action-id>' --namespace orka-system
```

## PR repair and readiness

The controller starts bounded repairs automatically when enabled. Ad hoc PR commands can request the same operations:

- `review` API/CLI command queues an exact-head review run.
- `fix` API/CLI command queues a repair task on the current same-repository PR head branch.
- `fix_ci` API/CLI command queues a CI repair task using the same repair path.
- `update_branch` API/CLI command queues a base-update repair task and allows empty push-branch updates.

Repair Tasks are exact-head write Tasks: `workspace.ref` and `workspace.expectedRemoteSHA` bind the selected PR head, and publication targets that same branch. A repair succeeds only with a matching `VerifiedExact` delivery receipt. An `update-branch` no-change result is accepted only after the controller independently verifies that the exact PR head contains the requested base revision. Successful repairs clear stale review state so the next exact-head review can recompute readiness. By default, a PR with a passed exact-head review and no active repair is surfaced as merge-ready; GitHub owns the merge decision.

## GitHub merge readiness

Managed workflows with `review.publish.enabled: true` publish the commit status `orka/<namespace>/<monitor-name>/ready` on the exact PR head. Require this status context together with your normal CI and approving-review rules. A GitHub App is not required.

The controller uses `forgeCredentialRef` for both status writes and readiness reads, including PR inventory and check runs. For private repositories, classic personal access tokens and OAuth tokens need the [`repo` scope](https://docs.github.com/en/enterprise-cloud@latest/apps/oauth-apps/building-oauth-apps/scopes-for-oauth-apps). `repo:status` alone cannot read private PRs or check runs. Fine-grained credentials need Commit statuses write, [Pull requests read](https://docs.github.com/en/rest/pulls/pulls#list-pull-requests), and [Checks read](https://docs.github.com/en/rest/checks/runs#list-check-runs-for-a-git-reference); publishing reviews also requires Pull requests write.

Reserve this context for Orka. Orka excludes only status IDs persisted in its mutation audit when evaluating CI, including peer monitors, so readiness cannot wait on itself. Check-run IDs are never excluded.

The status stays pending or failed while review, validation, or repair is incomplete. Statuses are commit-scoped: when multiple open PRs on the monitored base branch share a head SHA, all must be ready before that commit receives success. A paused or unreviewed peer blocks the shared status. A new head requires new evidence. Repair defaults to at most five attempts per PR and two per head; explicit lower limits are honored. Workflow polling observes CI and completed Tasks without requiring another command label.

GitHub merges only when its per-PR auto-merge setting is enabled and all required conditions pass. When disabled, the PR remains open and merge-ready. Orka never calls the merge endpoint or changes the auto-merge setting. There is no sensitive label or additional merge-approval classifier.

## API and authorization

Repository monitor endpoints live under `/api/v1/monitors/*` and require normal Orka API authentication. When context-token authorization is enabled, monitor reads require `orka:monitors:read`, monitor CRUD requires `orka:monitors:write`, and manual run creation requires `orka:monitors:operate`.

Context-token `tctx` constraints can also restrict monitor access by namespace, repository URL, branch, reviewer Agent, or allowed Agent set.

See [API Reference](../reference/api-reference.md#repository-monitors) for endpoint details.

## Limits

- GitHub is the only supported provider.
- Pull requests and issues are supported targets; commit monitoring is rejected.
- Reviewer Agents must use a built-in `claude`, `codex`, or `opencode` runtime and omit
  `spec.secretRef`; provider credentials come from the controller-managed runtime proxy and
  never enter the Task spec. Codex reviewers are confined by the RuntimeSession boundary:
  the controller rejects elevation requests, the supervisor mediates file writes, and any
  turn that modifies the workspace fails read-intent validation.
- Review publishing supports only neutral `COMMENT` reviews, with `sameHeadPolicy: skip`.

## Prompt-orchestrated monitors

Before RepositoryMonitor existed, the chat tool `create_pr_monitor` created a scheduled AI
Task that reviewed pull requests. It still works, but it keeps none of the durable per-PR
runs, items, review records, publish records, or audit events described on this page. Its
parameters, repository scoping, and review markers are documented under
[`create_pr_monitor`](../reference/built-in-tools.md#create_pr_monitor).
