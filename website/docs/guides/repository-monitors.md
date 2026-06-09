---
slug: /repository-monitors
---

# Repository Monitors

Repository monitors are durable, Kubernetes-native PR review automation for GitHub repositories. A `RepositoryMonitor` stores the repository scope, review agent, schedule, and safety policy in a CRD. The controller records runs, PR inventory, review results, and audit events in the SQLite store, then exposes that state through the REST API and embedded dashboard.

This is the durable successor path for prompt-orchestrated PR monitor tasks created by the `create_pr_monitor` tool. The current implementation supports GitHub pull request inventory and read-only review task creation.

## What It Does

A repository monitor can:

- list open pull requests for one GitHub repository and base branch
- skip drafts unless explicitly configured to include them
- skip PRs blocked by configured protected or pause labels
- skip PR heads that already have a fresh review result
- queue one read-only review task per selected PR head
- refetch one pull request for targeted manual and webhook runs before queueing review work
- queue exact-head monitor runs from GitHub pull request webhook events
- ingest typed JSON review results from completed review tasks
- store monitor runs, PR items, review records, and audit events durably
- show monitor status, recent runs, and the PR queue in the dashboard under **Monitors**

The review task is bound to the exact PR head SHA. It runs as a `type: agent` task, uses a Claude runtime Agent, checks out the PR head in a read-only workspace, writes generated PR context under `/workspace/.git/orka/`, and is instructed to return only the structured review result. It does not post GitHub comments, push commits, merge, close, or mutate labels.

## Current Limits

The first implementation is intentionally narrow:

- GitHub is the only supported provider.
- Pull requests are the only supported target type.
- `spec.targets.issues.enabled` and `spec.targets.commits.enabled` are rejected.
- `spec.targets.pullRequests.enabled` must be true or omitted.
- `spec.review.requireGreenCI` is rejected until CI state collection is available.
- GitHub webhook-driven exact runs are opt-in with `spec.review.exactEventEnabled`.
- Repair, automerge, maintainer command routing, and public review comment updates are represented in the API/store shape but are not active workflows in this slice.
- The reviewer Agent must use `runtime.type: claude` and reference a Secret in the monitor namespace with `ANTHROPIC_API_KEY` or `ANTHROPIC_FOUNDRY_API_KEY`.

## CI Coverage

Repository monitor backend coverage has a focused GitHub Actions workflow at `.github/workflows/repository-monitor-smoke.yml`. It runs on pull requests and pushes that touch the workflow, Go API/controller/store code, CRD/config paths, worker code, or Go dependency files.

The smoke workflow creates the UI embed stub and runs targeted Go tests for monitor store CRUD, API handlers, GitHub pull request event handling, targeted single-PR inventory runs, controller queue and review flow, blocked status counts, read-only review task job construction, stdout result forwarding, `create_pr_monitor` repository URL and credential validation, and PR review marker tooling. Worker-level PR review diff context generation is covered by the normal Go test workflow. UI monitor pages are covered by the normal frontend test workflow rather than this smoke workflow.

## Prerequisites

Create Claude runtime credentials in the monitor namespace:

```bash
kubectl create secret generic claude-runtime-credentials \
  --namespace default \
  --from-literal=ANTHROPIC_API_KEY='<anthropic-api-key>'
```

Then create a Claude runtime Agent in the same namespace as the monitor, or set `spec.agents.reviewer.namespace` explicitly. Orka validates that the Agent references a Secret in the monitor namespace and that the Secret contains a supported Claude auth key.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: repo-reviewer
  namespace: default
spec:
  secretRef:
    name: claude-runtime-credentials
  runtime:
    type: claude
    defaultMaxTurns: 50
    defaultAllowedTools:
      - Read
      - Grep
      - Glob
      - LS
  systemPrompt:
    inline: |
      Review the exact pull request head for correctness, tests, security, and maintainability.
      Return concise, structured findings and do not mutate GitHub.
```

For private repositories or higher GitHub rate limits, create a Git Secret in the monitor namespace. This is separate from the reviewer Agent's Claude credential Secret. When a monitor is created or updated through the API, Orka validates that the referenced Git Secret exists and contains a non-empty `token`, `password`, or `GITHUB_TOKEN` key.

```bash
kubectl create secret generic repo-monitor-github \
  --namespace default \
  --from-literal=token='<github-token>'
```

The same Secret is mounted into review workspaces for same-repository PR heads. Fork PR heads are checked out from the fork URL without the monitored repository credential.

## Review Workspace Context

Before the Claude reviewer starts, the worker fetches the PR base branch and writes generated read-only context files:

- `/workspace/.git/orka/pr-review.md` - base/head summary and diff stats
- `/workspace/.git/orka/pr-review.files` - changed file list
- `/workspace/.git/orka/pr-review.diff` - unified diff from the base branch to the PR head

The generated files are added to the workspace's git exclude file so they are not captured as task changes. Read-only review tasks receive only scoped file-reading tools for `/workspace/**` and selected Claude runtime environment variables from the reviewer Secret.

## Create a Monitor

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: RepositoryMonitor
metadata:
  name: example-app
  namespace: default
spec:
  provider: github
  repoURL: https://github.com/example/app
  branch: main
  gitSecretRef:
    name: repo-monitor-github
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
    mode: changed
    commands:
      - make test
```

Apply it with:

```bash
kubectl apply -f repository-monitor.yaml
```

The controller normalizes `provider`, `owner`, `repository`, `branch`, pull request enablement, `maxPerRun`, `review.event`, and validation mode when omitted.

## Run Manually

Scheduled runs are queued from `spec.schedule` when the monitor is not suspended. You can also trigger a manual run through the API:

```bash
curl -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  "https://<orka-api-host>/api/v1/monitors/repositories/example-app/runs?namespace=default" \
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

## Run From GitHub Events

Repository monitors can also receive exact pull request events through the same signed GitHub webhook endpoint used by label triggers. Configure the repository webhook for `Pull requests` events and set `spec.review.exactEventEnabled: true` on the monitor.

When `/webhooks/github` receives an `opened`, `reopened`, `synchronize`, `ready_for_review`, `labeled`, or `unlabeled` pull request event, Orka matches monitors by repository and base branch. If the controller has a watch namespace, only monitors in that namespace are considered; otherwise monitors across all namespaces are eligible. A matching monitor queues a run with `targetKind: pull_request`, the PR number, and the exact head SHA from the webhook payload. Replayed deliveries and already-queued runs for the same PR head are accepted without creating duplicate monitor work. If a previous exact-event run for the same delivery failed before the queued audit event was recorded, a webhook retry can requeue that failed run.

Exact event runs are still read-only review runs. They are stored with trigger `pull_request_event`, create an `exact_event_run_queued` audit event, and wait behind any active or queued monitor run. When the run executes, Orka refetches the current pull request by number and skips review work if the PR is no longer open, moved to another base branch, or no longer matches the event head SHA.

## Inspect State

Use `kubectl` for CRD-level status:

```bash
kubectl get repositorymonitors -n default
kubectl describe repositorymonitor example-app -n default
```

Use the API or dashboard for durable run and item state:

```bash
curl -H "Authorization: Bearer $TOKEN" \
  "https://<orka-api-host>/api/v1/monitors/repositories?namespace=default"

curl -H "Authorization: Bearer $TOKEN" \
  "https://<orka-api-host>/api/v1/monitors/repositories/example-app/runs?namespace=default"

curl -H "Authorization: Bearer $TOKEN" \
  "https://<orka-api-host>/api/v1/monitors/repositories/example-app/items?namespace=default&kind=pull_request"

curl -H "Authorization: Bearer $TOKEN" \
  "https://<orka-api-host>/api/v1/monitors/events?namespace=default&name=example-app"
```

The embedded dashboard shows the same state under **Monitors**:

- monitor list with phase, schedule, repository, and summary counts
- detail page with open PR count, pending reviews, blocked items, merge-ready count, recent runs, and PR queue
- manual **Run** action for an immediate monitor run

## Review Results

Review tasks must return a JSON object with schema version `orka.prReview.v1`. The controller validates the repository, PR number, and exact head SHA before accepting the result. Accepted results are stored as immutable review records and copied onto the current monitor item.

Valid review verdicts are:

- `passed`
- `needs_changes`
- `needs_human`
- `security_sensitive`
- `skipped`

For status summaries, open PRs with `needs_changes`, `needs_human`, `security_sensitive`, stale, failed, or skipped review state count as blocked items. Open PRs with queued review work count as pending reviews.

If a review task fails, is cancelled, returns malformed JSON, or returns a stale head SHA, the controller records a rejected review result and leaves an audit event explaining why.

## API And Authorization

Repository monitor endpoints live under `/api/v1/monitors/*` and require normal Orka API authentication. When context-token authorization is enabled, monitor reads require `orka:monitors:read`, monitor CRUD requires `orka:monitors:write`, and manual run creation requires `orka:monitors:operate`.

Context-token `tctx` constraints can also restrict monitor access by namespace, repository URL, branch, reviewer Agent, or allowed Agent set.

See [API Reference](../reference/api-reference.md#repository-monitors) for endpoint details.

## Prompt-Orchestrated PR Monitor Tool

`create_pr_monitor` remains the compatibility path for prompt-orchestrated scheduled PR monitors. It creates a scheduled `type: ai` Task with `spec.workspace.gitRepo` set to the requested GitHub repository, injects the PR review loop tools, and instructs the monitor to call `list_pull_requests`, `check_pr_review_marker`, `check_pull_request_ci`, `review_pull_request`, and `post_review_comment` with the same `repo_url`.

`repo_url` must be a credential-free GitHub repository root URL, for example `https://github.com/owner/repo`, `https://github.com/owner/repo.git`, or `git@github.com:owner/repo.git`. Do not pass a pull request, issue, branch, file, commit, query-string, fragment, or token-bearing URL. Orka rejects non-root repository URLs before it creates the monitor Task, which prevents prompts or copied browser URLs from widening the monitor's repository scope.

The tool requires an AI Agent with coordination enabled and autonomous coordination disabled. The created Task uses a narrow explicit tool set instead of the full coordination tool set, and it requires a Git credential Secret either through `gitSecretRef` or one of the supported default Secret names in the target namespace: `git-credentials`, `github-credentials`, `copilot-token`, `github-token`, or `git-token`. Orka validates the selected Secret before creating the monitor Task; it must contain a non-empty `token`, `password`, or `GITHUB_TOKEN` key.

The scheduled monitor prompt tells the worker to pass the same `repo_url` to every PR review loop tool. Those GitHub tools are scoped to the current Task: when task context is available, the requested repository must match the Task workspace repository or signed transaction repository context. If it does not match, Orka rejects the tool call before resolving credentials or calling GitHub. This means a monitor created for `owner/repo` cannot use its Task credential to list, review, or comment on another repository by changing tool arguments.

### PR Review Markers

`check_pr_review_marker` returns the exact hidden marker that the monitor should include in the GitHub review body:

```html
<!-- orka:pr-review repo=owner/repo pr=123 head_sha=abc123 sig=... -->
```

The marker binds the review to one repository, pull request number, and head SHA. Future monitor runs skip that PR head only when they find a matching marker in a GitHub pull request review.

Markers are stable across GitHub token rotation. They are not signed with the live GitHub token by default. To make marker verification independent of the review author, provide a stable worker environment secret named `ORKA_PR_REVIEW_MARKER_SECRET` to the monitor Task. During rotation, keep the old value in comma-separated `ORKA_PR_REVIEW_MARKER_PREVIOUS_SECRETS` until reviews signed with it have aged out.

For compatibility, Orka also recognizes legacy markers and markers signed before a dedicated marker secret was configured, but only from a trusted reviewer account. Set `ORKA_PR_REVIEW_MARKER_TRUSTED_AUTHOR` to that GitHub login, or omit it to let Orka resolve the authenticated GitHub user for the Task's Git credential. Do not store marker signing secrets in the repository; use Kubernetes Secrets or another secret injection path for Task environment.

## Related Workflows

- [GitHub Label Triggers](github-label-triggers.md) create one-off agent tasks from labels such as `agent:review` or `agent:implement`.
- [Repository Security Scanning](repository-security-scanning.md) scans repository history for security findings and supports patch proposal workflows.
- `create_pr_monitor` remains available for prompt-orchestrated scheduled PR monitor tasks, but it does not provide the durable per-PR run, item, review, and event records described here.
