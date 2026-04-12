# GitHub CI/CD Integration

This maintained example shows how to use Orka's multi-agent coordination to automate a GitHub PR workflow:

1. An AI coordinator delegates code changes to a Claude Code runtime agent.
2. The runtime agent pushes a feature branch.
3. The coordinator opens a PR with Orka's built-in `create_pull_request` tool.
4. The coordinator waits for CI and merges with `auto_merge_pull_request`.
5. If CI fails, the coordinator loops back with fix feedback on the same branch.

## How It Works

1. A **coordinator agent** (AI type with coordination enabled) receives a task
2. It delegates work to a **Claude Code agent** via `delegate_task`, including workspace details for clone, push, and PR creation
3. It uses built-in GitHub coordination tools to create the PR and auto-merge when checks pass
4. On CI failure, it can delegate a follow-up fix using `prior_task`

## Files

| File | Description |
|------|-------------|
| `agents.yaml` | Coordinator and Claude Code agent definitions |
| `secret.yaml` | Example `git-credentials` Secret for clone/push/PR auth |
| `task.yaml` | Sample task to trigger the workflow |
| `github-actions-webhook.yaml` | Optional GitHub Actions workflow that triggers a branch-fix agent task on CI failure |

## Setup

```bash
# Update `spec.providerRef.name` in agents.yaml to match your Provider CRD.
# Edit secret.yaml and replace the token value.
kubectl apply -f examples/github-cicd/secret.yaml

# Create Claude runtime credentials if you do not already have them
kubectl create secret generic claude-credentials \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-your-key

# Deploy the example
kubectl apply -k examples/github-cicd

# Submit a task
kubectl apply -f examples/github-cicd/task.yaml
```

Before running the task, edit `task.yaml` and replace the placeholder repository details:

- `gitRepo`
- `branch`
- `gitSecretRef`
- `pushBranch`

## Optional GitHub Actions Integration

The `github-actions-webhook.yaml` file is a workflow you can copy into `.github/workflows/` in a repository. When a CI job fails, it creates a direct `type: agent` task for `claude-coder` to investigate and push a fix to the same branch.

Configure these repository secrets:

- `ORKA_API_URL`
- `ORKA_TOKEN`

And make sure the Orka namespace already has a `git-credentials` Secret that matches the name used in the workflow payload.
