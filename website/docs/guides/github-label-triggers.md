---
slug: /github-label-triggers
---

# GitHub Label Triggers

Orka can create an agent-runtime Task when a GitHub issue or pull request receives a label such as `agent:implement`, `agent:update-branch`, `agent:review`, or `agent:to-issues`.

## Webhook endpoint

Configure a GitHub repository webhook:

- **Payload URL:** `https://<orka-api-host>/webhooks/github`
- **Content type:** `application/json`
- **Secret:** a shared secret stored outside git, provided to the controller as `ORKA_GITHUB_WEBHOOK_SECRET`
- **Events:** `Issues` and `Pull requests`

Orka verifies `X-Hub-Signature-256` before reading the payload. Requests without a valid HMAC signature are rejected and do not create Tasks.

## Controller configuration

Set these environment variables on the controller Deployment:

| Variable | Required | Description |
| --- | --- | --- |
| `ORKA_GITHUB_WEBHOOK_SECRET` | yes | Shared webhook secret used for HMAC verification. Use a Kubernetes Secret. |
| `ORKA_GITHUB_LABEL_TRIGGER_AGENT` | yes | Default runtime Agent CR used for created `type: agent` Tasks. |
| `ORKA_GITHUB_LABEL_TRIGGER_NAMESPACE` | no | Namespace for created Tasks. Defaults to the controller watch namespace, then `default`. |
| `ORKA_GITHUB_LABEL_TRIGGER_GIT_SECRET` | no | Secret name mounted into eligible agent workspaces as `/secrets/git` for clone and GitHub API auth when it is safe for the target repository. `agent:review` can use it for private same-repository PR clones but never receives a push branch. When Orka uses this secret, it must exist in the label-trigger namespace and contain a non-empty `token`, `password`, or `GITHUB_TOKEN` key. |
| `ORKA_GITHUB_LABEL_TRIGGER_PREFIX` | no | Label prefix. Defaults to `agent:`. |
| `ORKA_GITHUB_LABEL_TRIGGER_TIMEOUT` | no | Task timeout. Defaults to `30m`. |
| `ORKA_GITHUB_LABEL_TRIGGER_MAX_TURNS` | no | Agent max turns. Defaults to `100`. |
| `ORKA_GITHUB_LABEL_AGENT_<ACTION>` | no | Action-specific Agent override, for example `ORKA_GITHUB_LABEL_AGENT_REVIEW=review-agent`. Hyphens become underscores. |

Helm values expose the same settings under `github.webhook` and `github.labelTrigger`.

## Behavior

When GitHub sends a `labeled` event and the label starts with the configured prefix, Orka creates an idempotent Task named from the action, target number, and delivery ID.

Default action prompts:

- `agent:implement` - implement the issue or PR request and run tests. When a valid safe git secret is configured, Orka commits and pushes final changes to a generated `orka/implement-...` branch (or the PR head branch for same-repository PRs). For fork PRs or deployments without a git secret, Orka captures the final workspace diff in the task result and does not push automatically.
- `agent:update-branch` - for pull requests only; update the PR head branch from the base branch. Orka pushes back only when a valid safe git secret is configured for the PR head repository.
- `agent:review` - for pull requests only; review without changing code.
- `agent:to-issues` - break the request into independently implementable GitHub issues, creating them when credentials/tools permit or returning drafts.
- Other `agent:<action>` labels create a generic action task with a scoped prompt.

GitHub delivery IDs make retries safe: if the same delivery is received again, Orka returns `202 Accepted` with the existing task name instead of creating a duplicate.

## Repository Monitor Events

The same `/webhooks/github` endpoint can queue exact-head `RepositoryMonitor` runs from pull request events. This path does not require an `agent:*` label. A monitor is eligible when `spec.review.exactEventEnabled: true`, pull request monitoring is enabled, the webhook repository matches `spec.repoURL`, the PR base branch matches the monitor branch, and the monitor is not suspended.

For `opened`, `reopened`, `synchronize`, `ready_for_review`, `labeled`, and `unlabeled` pull request events, Orka queues a monitor run for the exact PR head SHA and records an audit event. If the controller has a watch namespace, only monitors in that namespace are considered; otherwise monitors across all namespaces are eligible. Duplicate deliveries or already-queued runs for the same PR head are accepted without creating duplicate work. If a previous exact-event run for the same delivery failed before the queued audit event was recorded, a webhook retry can requeue that failed run.

## CI Coverage

`.github/workflows/live-github-label-trigger-e2e.yml` is a manual GitHub Actions workflow for the label trigger path. It runs focused Go tests for webhook and PR monitor tooling, then builds the controller image, deploys Orka into a fresh Kind cluster, creates a synthetic runtime Agent, and sends signed webhook payloads to `/webhooks/github`.

The workflow is model-free and secret-free. It generates the webhook secret during the run and uses a synthetic `agent:implement` issue label payload for the configured `target_repo_url` and `target_number` inputs. The script verifies that invalid signatures return `401`, a valid label event creates one scoped agent Task, and a repeated GitHub delivery returns `202` with the original task name.

Run the same validation locally with:

```bash
GITHUB_LABEL_TRIGGER_TARGET_REPO_URL=https://github.com/sozercan/orka \
GITHUB_LABEL_TRIGGER_TARGET_NUMBER=1 \
bash scripts/live-github-label-trigger-e2e.sh
```

## Minimal Helm configuration

```yaml
github:
  webhook:
    secretName: github-webhook-secret
    secretKey: secret
  labelTrigger:
    agent: codex-agent
    namespace: default
    gitSecret: git-credentials
    agents:
      review: review-agent
```

Create the referenced Secret outside git:

```bash
kubectl create secret generic github-webhook-secret \
  --from-literal=secret='<webhook-secret>'
```
