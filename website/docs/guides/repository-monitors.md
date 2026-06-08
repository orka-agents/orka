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
- ingest typed JSON review results from completed review tasks
- store monitor runs, PR items, review records, and audit events durably
- show monitor status, recent runs, and the PR queue in the dashboard under **Monitors**

The review task is bound to the exact PR head SHA. It runs as a `type: agent` task, uses a Claude runtime Agent, checks out the PR head in a read-only workspace, and is instructed to return only the structured review result. It does not post GitHub comments, push commits, merge, close, or mutate labels.

## Current Limits

The first implementation is intentionally narrow:

- GitHub is the only supported provider.
- Pull requests are the only supported target type.
- `spec.targets.issues.enabled` and `spec.targets.commits.enabled` are rejected.
- `spec.targets.pullRequests.enabled` must be true or omitted.
- `spec.review.requireGreenCI` is rejected until CI state collection is available.
- Repair, automerge, maintainer command routing, and public review comment updates are represented in the API/store shape but are not active workflows in this slice.
- The reviewer Agent must use `runtime.type: claude` for read-only repository monitor reviews.

## Prerequisites

Create a Claude runtime Agent in the same namespace as the monitor, or set `spec.agents.reviewer.namespace` explicitly.

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
    defaultAllowBash: true
    defaultAllowedTools:
      - Read
      - Grep
      - Glob
      - Bash
  systemPrompt:
    inline: |
      Review the exact pull request head for correctness, tests, security, and maintainability.
      Return concise, structured findings and do not mutate GitHub.
```

For private repositories or higher GitHub rate limits, create a Secret in the monitor namespace. The controller accepts a token from `token`, `password`, or `GITHUB_TOKEN`.

```bash
kubectl create secret generic repo-monitor-github \
  --namespace default \
  --from-literal=token='<github-token>'
```

The same Secret is mounted into review workspaces for same-repository PR heads. Fork PR heads are checked out from the fork URL without the monitored repository credential.

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

If a review task fails, is cancelled, returns malformed JSON, or returns a stale head SHA, the controller records a rejected review result and leaves an audit event explaining why.

## API And Authorization

Repository monitor endpoints live under `/api/v1/monitors/*` and require normal Orka API authentication. When context-token authorization is enabled, monitor reads require `orka:monitors:read`, monitor CRUD requires `orka:monitors:write`, and manual run creation requires `orka:monitors:operate`.

Context-token `tctx` constraints can also restrict monitor access by namespace, repository URL, branch, reviewer Agent, or allowed Agent set.

See [API Reference](../reference/api-reference.md#repository-monitors) for endpoint details.

## Prompt-Orchestrated PR Monitor Tool

`create_pr_monitor` remains the compatibility path for prompt-orchestrated scheduled PR monitors. It creates a scheduled `type: ai` Task with `spec.workspace.gitRepo` set to the requested GitHub repository, injects the PR review loop tools, and instructs the monitor to call `list_pull_requests`, `check_pr_review_marker`, `check_pull_request_ci`, `review_pull_request`, and `post_review_comment` with the same `repo_url`.

The tool requires an AI Agent with coordination enabled and autonomous coordination disabled. The created Task uses a narrow explicit tool set instead of the full coordination tool set, and it requires a Git credential Secret either through `gitSecretRef` or one of the supported default Secret names in the target namespace.

GitHub tools that accept explicit `repo_url` values are scoped to the current Task. If a tool call provides a different repository than the Task workspace or signed transaction context permits, Orka rejects the call before resolving credentials or calling GitHub.

## Related Workflows

- [GitHub Label Triggers](github-label-triggers.md) create one-off agent tasks from labels such as `agent:review` or `agent:implement`.
- [Repository Security Scanning](repository-security-scanning.md) scans repository history for security findings and supports patch proposal workflows.
- `create_pr_monitor` remains available for prompt-orchestrated scheduled PR monitor tasks, but it does not provide the durable per-PR run, item, review, and event records described here.
