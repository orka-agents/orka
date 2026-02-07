# CI/CD Integration

Mercan can orchestrate full CI/CD workflows using multi-agent coordination. A coordinator agent manages the lifecycle—delegating coding tasks to a Claude Code agent, polling CI status via GitHub API tools, and merging PRs when checks pass.

## Prerequisites

- Mercan controller deployed with `--claude-worker-image` configured
- GitHub token with `repo`, `workflow`, and `pull_request` scopes
- Anthropic API key for the coordinator agent

## Setup

All sample manifests are in [`examples/github-cicd/`](../examples/github-cicd/).

### 1. Create Secrets

```bash
# GitHub token for API calls and git operations
kubectl create secret generic github-token \
  --from-literal=token=ghp_YOUR_TOKEN

# Claude API key for the coder agent
kubectl create secret generic claude-coder-secrets \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-... \
  --from-literal=GITHUB_TOKEN=ghp_YOUR_TOKEN

# LLM credentials for the coordinator (uses built-in AI worker)
kubectl create secret generic llm-credentials \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-...
```

### 2. Create GitHub Tool CRDs

These tools wrap the GitHub REST API for CI status checks and PR operations.

```yaml
# github-check-ci: Check CI/check-run status on a commit
apiVersion: core.mercan.ai/v1alpha1
kind: Tool
metadata:
  name: github-check-ci
spec:
  description: Check CI status on a GitHub commit or PR head.
  parameters:
    type: object
    properties:
      owner: { type: string, description: "Repository owner" }
      repo: { type: string, description: "Repository name" }
      ref: { type: string, description: "Commit SHA, branch, or tag" }
    required: [owner, repo, ref]
  http:
    url: "https://api.github.com/repos/{{owner}}/{{repo}}/commits/{{ref}}/check-runs"
    method: GET
    headers:
      Accept: "application/vnd.github+json"
      X-GitHub-Api-Version: "2022-11-28"
    authSecretRef:
      name: github-token
      key: token
    timeout: 15s
---
# github-merge-pr: Merge a pull request
apiVersion: core.mercan.ai/v1alpha1
kind: Tool
metadata:
  name: github-merge-pr
spec:
  description: Merge a PR (requires passing CI and approvals).
  parameters:
    type: object
    properties:
      owner: { type: string }
      repo: { type: string }
      pull_number: { type: integer }
      merge_method: { type: string, enum: [merge, squash, rebase], default: squash }
    required: [owner, repo, pull_number]
  http:
    url: "https://api.github.com/repos/{{owner}}/{{repo}}/pulls/{{pull_number}}/merge"
    method: PUT
    headers:
      Accept: "application/vnd.github+json"
      X-GitHub-Api-Version: "2022-11-28"
    authSecretRef:
      name: github-token
      key: token
    timeout: 15s
---
# github-get-pr: Get PR details
apiVersion: core.mercan.ai/v1alpha1
kind: Tool
metadata:
  name: github-get-pr
spec:
  description: Get PR status, reviews, and mergeable state.
  parameters:
    type: object
    properties:
      owner: { type: string }
      repo: { type: string }
      pull_number: { type: integer }
    required: [owner, repo, pull_number]
  http:
    url: "https://api.github.com/repos/{{owner}}/{{repo}}/pulls/{{pull_number}}"
    method: GET
    headers:
      Accept: "application/vnd.github+json"
      X-GitHub-Api-Version: "2022-11-28"
    authSecretRef:
      name: github-token
      key: token
    timeout: 15s
```

Apply with:

```bash
kubectl apply -f examples/github-cicd/tools.yaml
```

### 3. Create the Claude Coder Agent

This agent uses Claude Code CLI to write code, run tests, and create PRs.

```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Agent
metadata:
  name: claude-coder
spec:
  runtime:
    type: claude
    defaultMaxTurns: 100
    defaultAllowBash: true
  systemPrompt:
    inline: |
      You are a software developer. When given a task:
      1. Create a feature branch from main
      2. Implement the changes
      3. Run any existing tests
      4. Commit with a descriptive message
      5. Push and create a PR using `gh pr create`
      6. Report the PR number and URL in your final output
  secretRef:
    name: claude-coder-secrets
```

### 4. Create the CI/CD Coordinator Agent

This agent orchestrates the full flow using multi-agent coordination.

```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Agent
metadata:
  name: cicd-coordinator
spec:
  providerRef:
    name: anthropic-provider
  model:
    name: claude-sonnet-4-20250514
  systemPrompt:
    inline: |
      You are a CI/CD coordinator. Manage the full lifecycle:
      1. delegate_task to claude-coder with workspace.gitRepo and workspace.branch
      2. wait_for_tasks to get the PR number from the result
      3. github-check-ci to poll CI status (every 30s, up to 10 minutes)
      4. If CI passes: github-merge-pr to merge
      5. If CI fails: delegate_task to claude-coder to fix failures
      6. Report the final outcome
  tools:
    - name: delegate_task
    - name: wait_for_tasks
    - name: github-check-ci
    - name: github-merge-pr
    - name: github-get-pr
  coordination:
    enabled: true
    maxConcurrentChildren: 3
    maxDepth: 2
    allowedAgents:
      - name: claude-coder
  secretRef:
    name: llm-credentials
```

Apply both agents:

```bash
kubectl apply -f examples/github-cicd/agents.yaml
```

### 5. Trigger the CI/CD Flow

Create a Task that invokes the coordinator:

```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Task
metadata:
  name: implement-feature
spec:
  type: ai
  agentRef:
    name: cicd-coordinator
  prompt: |
    Implement unit tests for the authentication module in
    https://github.com/myorg/myrepo (branch: main).
    Create a PR, wait for CI, and merge if green.
  timeout: 45m
  priority: 800
```

```bash
kubectl apply -f examples/github-cicd/task.yaml
kubectl get task implement-feature -w
```

## GitHub Actions Webhook for Auto-Fix

To automatically trigger Mercan when CI fails, add this workflow to your repository:

**`.github/workflows/ci-feedback.yml`**:

```yaml
name: CI Feedback to Mercan
on:
  workflow_run:
    workflows: ["CI"]
    types: [completed]

jobs:
  trigger-fix:
    if: ${{ github.event.workflow_run.conclusion == 'failure' }}
    runs-on: ubuntu-latest
    steps:
      - name: Get failure context
        id: ctx
        run: |
          echo "branch=${{ github.event.workflow_run.head_branch }}" >> $GITHUB_OUTPUT
          echo "sha=${{ github.event.workflow_run.head_sha }}" >> $GITHUB_OUTPUT
          echo "repo=${{ github.repository }}" >> $GITHUB_OUTPUT

      - name: Trigger Mercan fix agent
        run: |
          curl -s -X POST "${{ secrets.MERCAN_API_URL }}/api/v1/tasks" \
            -H "Authorization: Bearer ${{ secrets.MERCAN_TOKEN }}" \
            -H "Content-Type: application/json" \
            -d '{
              "type": "agent",
              "agentRef": {"name": "claude-coder"},
              "prompt": "CI failed on branch ${{ steps.ctx.outputs.branch }} (commit ${{ steps.ctx.outputs.sha }}). Check the CI failures, fix the code, and push to the same branch.",
              "agentRuntime": {
                "workspace": {
                  "gitRepo": "https://github.com/${{ steps.ctx.outputs.repo }}.git",
                  "branch": "${{ steps.ctx.outputs.branch }}"
                }
              },
              "timeout": "15m",
              "priority": 900
            }'
```

**Required GitHub Secrets**:

| Secret | Description |
|--------|-------------|
| `MERCAN_API_URL` | Mercan API endpoint (e.g., `https://mercan.example.com`) |
| `MERCAN_TOKEN` | ServiceAccount token: `kubectl create token mercan-client` |

## Flow Diagram

```
User creates Task (agentRef: cicd-coordinator)
  │
  ▼
Coordinator delegates to claude-coder
  │ (workspace.gitRepo, workspace.branch)
  ▼
Claude Code CLI: branch → code → test → commit → push → PR
  │
  ▼
Coordinator: wait_for_tasks → gets PR number
  │
  ▼
Coordinator: github-check-ci (poll loop)
  │
  ├─ CI passes → github-merge-pr → done
  │
  └─ CI fails → delegate_task to claude-coder (fix) → loop
```

## Troubleshooting

| Issue | Solution |
|-------|----------|
| `github-check-ci` 404 | Verify `owner`, `repo`, and `ref` values |
| `github-merge-pr` fails | Check branch protection rules and required reviews |
| claude-coder stuck | Increase `defaultMaxTurns` or check API key |
| Coordinator timeout | Increase Task `timeout` for long CI pipelines |
| Webhook not triggering | Verify `MERCAN_API_URL` and `MERCAN_TOKEN` secrets |
