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
| `ORKA_GITHUB_LABEL_TRIGGER_GIT_SECRET` | no | Secret name mounted into agent workspaces as `/secrets/git` for clone, push, and GitHub API auth. Automatic push branches are only configured when this secret is set and safe for the target repository. |
| `ORKA_GITHUB_LABEL_TRIGGER_PREFIX` | no | Label prefix. Defaults to `agent:`. |
| `ORKA_GITHUB_LABEL_TRIGGER_TIMEOUT` | no | Task timeout. Defaults to `30m`. |
| `ORKA_GITHUB_LABEL_TRIGGER_MAX_TURNS` | no | Agent max turns. Defaults to `100`. |
| `ORKA_GITHUB_LABEL_AGENT_<ACTION>` | no | Action-specific Agent override, for example `ORKA_GITHUB_LABEL_AGENT_REVIEW=review-agent`. Hyphens become underscores. |

Helm values expose the same settings under `github.webhook` and `github.labelTrigger`.

## Behavior

When GitHub sends a `labeled` event and the label starts with the configured prefix, Orka creates an idempotent Task named from the action, target number, and delivery ID.

Default action prompts:

- `agent:implement` - implement the issue or PR request and run tests. When a safe git secret is configured, Orka commits and pushes final changes to a generated `orka/implement-...` branch (or the PR head branch for same-repository PRs). For fork PRs or deployments without a git secret, Orka captures the final workspace diff in the task result and does not push automatically.
- `agent:update-branch` - for pull requests only; update the PR head branch from the base branch. Orka pushes back only when a safe git secret is configured for the PR head repository.
- `agent:review` - for pull requests only; review without changing code.
- `agent:to-issues` - break the request into independently implementable GitHub issues, creating them when credentials/tools permit or returning drafts.
- Other `agent:<action>` labels create a generic action task with a scoped prompt.

GitHub delivery IDs make retries safe: if the same delivery is received again, Orka returns `202 Accepted` with the existing task name instead of creating a duplicate.

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
