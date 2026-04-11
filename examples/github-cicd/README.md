# GitHub CI/CD Integration

This example shows how to use Orka's multi-agent coordination to automate a CI/CD workflow: a coordinator agent delegates coding tasks to Claude Code agents, which create PRs, and the coordinator monitors CI and merges when checks pass.

## How It Works

1. A **coordinator agent** (AI type with coordination enabled) receives a task
2. It delegates work to a **Claude Code agent** via `delegate_task`, which creates a branch, writes code, and opens a PR
3. The coordinator uses `auto_merge_pull_request` to wait for CI checks and merge automatically
4. On failure, the coordinator can delegate a fix and retry

## Files

| File | Description |
|------|-------------|
| `agents.yaml` | Coordinator and Claude Code agent definitions |
| `tools.yaml` | GitHub API Tool CRDs (CI status check, PR merge, PR details) using [URL path interpolation](../../docs/configuration.md#url-path-interpolation) |
| `secret.yaml` | GitHub token Secret (replace with your token) |
| `task.yaml` | Sample task to trigger the workflow |
| `github-actions-webhook.yaml` | GitHub Actions workflow that creates an Orka task on CI failure |

## Setup

```bash
# Create the GitHub token secret (edit secret.yaml first)
kubectl apply -f secret.yaml

# Deploy agents and tools
kubectl apply -k .

# Submit a task
kubectl apply -f task.yaml
```

## GitHub Actions Integration

The `github-actions-webhook.yaml` file is a GitHub Actions workflow you add to your repository (`.github/workflows/`). When a CI job fails, it sends a webhook to Orka to create a task that investigates and fixes the failure. Configure the `ORKA_ENDPOINT` and `ORKA_TOKEN` secrets in your GitHub repository settings.
