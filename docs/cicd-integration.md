# CI/CD Integration Plan

## Problem

`delegate_task` hardcodes `Type: TaskTypeAI` on child tasks. A coordinator agent cannot delegate to `type: agent` tasks (Claude Code, Copilot CLI). This means agents that can natively create branches, write code, commit, push, and open PRs via `gh` are unreachable from the coordination system.

Additionally, the Tool CRD HTTP executor sends all parameters as JSON body — it cannot do URL path interpolation (e.g., `/repos/{owner}/{repo}/pulls/{number}/merge`). This blocks lightweight GitHub/GitLab API calls from Tool CRDs.

Fixing these two issues — plus shipping sample YAML — gives Mercan a full CI/CD workflow using existing primitives, with no new tools or agent roles needed.

## Key Insight

Mercan doesn't need built-in PR lifecycle tools. The `type: agent` tasks run Claude Code or Copilot CLI, which natively know how to create branches, write code, commit, push, and run `gh pr create`. These are full coding agents with filesystem access, bash, and git — they do PR creation **better than any custom tool we'd build**.

What's needed is:
1. Let `delegate_task` create agent tasks (not just AI tasks)
2. Let Tool CRDs make REST API calls with URL path interpolation (for GitHub API)
3. Ship sample Tool CRDs for GitHub CI status checks and PR merges
4. Ship a sample GitHub Actions workflow that triggers Mercan tasks on CI failure

## Architecture

```
Coordinator (type: ai, coordination enabled)
  │
  │  delegate_task(agent: "claude-coder", prompt: "Fix auth, create PR",
  │                workspace: {gitRepo: "...", branch: "main"})
  │  → detects claude-coder has Runtime set
  │  → creates type: agent task (not type: ai)
  │  → passes workspace config
  │
  ▼
Claude Code Agent (type: agent, Claude CLI runtime)
  ├─ Clones repo into /workspace
  ├─ Creates feature branch
  ├─ Writes code (Claude Code's native capability)
  ├─ Runs tests locally
  ├─ Commits, pushes, runs `gh pr create`
  └─ Writes result: {"pr_url": "...", "pr_number": 42, "branch": "fix-auth"}
  │
  ▼
Coordinator gets result back via wait_for_tasks
  │
  │  Uses github-check-ci Tool CRD (HTTP GET)
  │  → Returns CI status
  │
  ▼
Coordinator decides:
  ├─ CI passed → github-merge-pr Tool CRD → merge
  ├─ CI failed → delegate_task to agent to fix failures
  └─ Needs review → wait or notify user
```

## Changes

### Change 1: Fix `delegate_task` to auto-detect task type

**File:** `internal/tools/delegate_task.go`

**Current behavior:** Always creates `Type: TaskTypeAI` child tasks.

**New behavior:** Look up the target Agent CRD. If it has `Spec.Runtime` set, create a `Type: TaskTypeAgent` task instead. Accept optional `workspace` and `agentRuntime` fields in the tool arguments so the coordinator can pass git repo configuration to the child.

**Args schema change:**

```json
{
  "type": "object",
  "properties": {
    "agent": {
      "type": "string",
      "description": "Name of the agent to delegate to"
    },
    "prompt": {
      "type": "string",
      "description": "The task prompt for the agent"
    },
    "namespace": {
      "type": "string",
      "description": "Namespace (defaults to current)"
    },
    "priority": {
      "type": "integer",
      "description": "Priority 0-1000 (defaults to parent priority)"
    },
    "workspace": {
      "type": "object",
      "description": "Git workspace configuration (for agent tasks)",
      "properties": {
        "gitRepo": {
          "type": "string",
          "description": "Git repository URL to clone"
        },
        "branch": {
          "type": "string",
          "description": "Branch to checkout"
        },
        "ref": {
          "type": "string",
          "description": "Specific git ref (commit SHA, tag)"
        }
      }
    },
    "maxTurns": {
      "type": "integer",
      "description": "Max agent loop iterations (for agent tasks)"
    },
    "allowBash": {
      "type": "boolean",
      "description": "Allow bash commands (for agent tasks)"
    }
  },
  "required": ["agent", "prompt"]
}
```

**Logic change in `Execute()`:**

```go
// After resolving namespace, before building child task:

// Look up the target Agent to determine task type
targetAgent := &corev1alpha1.Agent{}
if err := t.k8sClient.Get(ctx, types.NamespacedName{
    Name: delegateArgs.Agent, Namespace: ns,
}, targetAgent); err != nil {
    return "", fmt.Errorf("failed to get agent %q: %w", delegateArgs.Agent, err)
}

// Auto-detect task type based on agent configuration
taskType := corev1alpha1.TaskTypeAI
if targetAgent.Spec.Runtime != nil {
    taskType = corev1alpha1.TaskTypeAgent
}

// Build child task spec
childTask.Spec.Type = taskType

// Add agent runtime config for agent tasks
if taskType == corev1alpha1.TaskTypeAgent {
    childTask.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{}

    if delegateArgs.Workspace != nil {
        childTask.Spec.AgentRuntime.Workspace = &corev1alpha1.WorkspaceConfig{
            GitRepo: delegateArgs.Workspace.GitRepo,
            Branch:  delegateArgs.Workspace.Branch,
            Ref:     delegateArgs.Workspace.Ref,
        }
    }

    if delegateArgs.MaxTurns != nil {
        childTask.Spec.AgentRuntime.MaxTurns = delegateArgs.MaxTurns
    }

    if delegateArgs.AllowBash != nil {
        childTask.Spec.AgentRuntime.AllowBash = delegateArgs.AllowBash
    }
}
```

**Struct changes:**

```go
type DelegateTaskArgs struct {
    Agent     string           `json:"agent"`
    Prompt    string           `json:"prompt"`
    Namespace string           `json:"namespace,omitempty"`
    Priority  *int32           `json:"priority,omitempty"`
    Workspace *WorkspaceArgs   `json:"workspace,omitempty"`
    MaxTurns  *int32           `json:"maxTurns,omitempty"`
    AllowBash *bool            `json:"allowBash,omitempty"`
}

type WorkspaceArgs struct {
    GitRepo string `json:"gitRepo,omitempty"`
    Branch  string `json:"branch,omitempty"`
    Ref     string `json:"ref,omitempty"`
}
```

**Estimated diff:** ~60 lines changed in `delegate_task.go`, ~30 lines in `delegate_task_test.go`.

### Change 2: Add URL path interpolation to Tool CRD executor

**File:** `internal/worker/tool_executor.go`

**Problem:** The GitHub API is REST-style: `GET /repos/{owner}/{repo}/commits/{ref}/check-runs`. The current executor sends all parameters as JSON body to a fixed URL. There's no way to interpolate parameters into the URL path.

**Solution:** Before building the request, scan the URL for `{{key}}` placeholders and replace them with values from the parsed arguments. Remove interpolated keys from the body so they aren't sent twice.

**Implementation:**

```go
// In Execute(), after parsing params and before building request body:

// Interpolate URL path parameters
url := tool.Spec.HTTP.URL
interpolatedKeys := map[string]bool{}
for key, val := range params {
    placeholder := "{{" + key + "}}"
    if strings.Contains(url, placeholder) {
        url = strings.ReplaceAll(url, placeholder, fmt.Sprintf("%v", val))
        interpolatedKeys[key] = true
    }
}

// Remove interpolated keys from body params
for key := range interpolatedKeys {
    delete(params, key)
}

// Build request body from remaining params
body, err := json.Marshal(params)
```

For GET requests with no remaining params, send an empty body (or no body).

**Estimated diff:** ~20 lines in `tool_executor.go`, ~40 lines in `tool_executor_test.go`.

### Change 3: Sample GitHub Tool CRDs

**Files:** New YAML files in `config/samples/` and `examples/github-cicd/`

#### `github-check-ci` Tool

```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Tool
metadata:
  name: github-check-ci
spec:
  description: |
    Check CI/check-run status on a GitHub pull request or commit.
    Returns the combined status of all check runs.
  parameters:
    type: object
    properties:
      owner:
        type: string
        description: "Repository owner (org or user)"
      repo:
        type: string
        description: "Repository name"
      ref:
        type: string
        description: "Commit SHA, branch name, or tag"
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
```

#### `github-merge-pr` Tool

```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Tool
metadata:
  name: github-merge-pr
spec:
  description: |
    Merge a pull request on GitHub. Only works if the PR has passing CI
    and required approvals. Supports merge, squash, and rebase strategies.
  parameters:
    type: object
    properties:
      owner:
        type: string
        description: "Repository owner"
      repo:
        type: string
        description: "Repository name"
      pull_number:
        type: integer
        description: "Pull request number"
      merge_method:
        type: string
        description: "Merge strategy"
        enum: [merge, squash, rebase]
        default: squash
      commit_title:
        type: string
        description: "Custom merge commit title (optional)"
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
```

#### `github-get-pr` Tool

```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Tool
metadata:
  name: github-get-pr
spec:
  description: |
    Get details about a GitHub pull request, including its status,
    review state, and mergeable status.
  parameters:
    type: object
    properties:
      owner:
        type: string
        description: "Repository owner"
      repo:
        type: string
        description: "Repository name"
      pull_number:
        type: integer
        description: "Pull request number"
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

#### GitHub token Secret

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: github-token
type: Opaque
stringData:
  token: "ghp_YOUR_TOKEN_HERE"  # needs repo, workflow, pull_request scopes
```

### Change 4: Sample coordinator and agent configuration

**File:** `examples/github-cicd/agents.yaml`

```yaml
# Claude Code agent that writes code and creates PRs
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
    name: claude-coder-secrets  # ANTHROPIC_API_KEY + GITHUB_TOKEN
---
# Coordinator agent that orchestrates the full CI/CD flow
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
      You are a CI/CD coordinator. You manage the full lifecycle of code changes:

      1. Use delegate_task to assign coding work to the claude-coder agent.
         Include workspace.gitRepo and workspace.branch so the agent clones the right repo.
      2. Use wait_for_tasks to get the result (which should include the PR number).
      3. Use github-check-ci to poll CI status on the PR's head commit.
         Poll every 30 seconds until checks complete (allow up to 10 minutes).
      4. If CI passes: use github-merge-pr to merge the PR.
      5. If CI fails: delegate_task to claude-coder to fix the failures,
         referencing the same branch.
      6. Report the final outcome.

      Always include the full owner/repo when calling GitHub tools.
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

**File:** `examples/github-cicd/task.yaml`

```yaml
# Trigger the full CI/CD flow
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

### Change 5: CI failure webhook example

**File:** `examples/github-cicd/github-actions-webhook.yaml`

This is a GitHub Actions workflow snippet users add to their repo:

```yaml
# .github/workflows/ci-feedback.yml
# Call Mercan API when CI fails to trigger auto-fix agent
name: CI Feedback to Mercan
on:
  workflow_run:
    workflows: ["CI"]  # name of your CI workflow
    types: [completed]

jobs:
  trigger-fix:
    if: ${{ github.event.workflow_run.conclusion == 'failure' }}
    runs-on: ubuntu-latest
    steps:
      - name: Get failure logs
        id: logs
        run: |
          # Fetch the failed run logs (simplified)
          echo "run_id=${{ github.event.workflow_run.id }}" >> $GITHUB_OUTPUT
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
              "prompt": "CI failed on branch ${{ steps.logs.outputs.branch }} (commit ${{ steps.logs.outputs.sha }}). Check the CI failures, fix the code, and push to the same branch.",
              "agentRuntime": {
                "workspace": {
                  "gitRepo": "https://github.com/${{ steps.logs.outputs.repo }}.git",
                  "branch": "${{ steps.logs.outputs.branch }}"
                }
              },
              "timeout": "15m",
              "priority": 900
            }'
```

## File Summary

| File | Action | ~LOC | Description |
|---|---|---|---|
| `internal/tools/delegate_task.go` | Modify | ~60 | Auto-detect agent type, add workspace/runtime args |
| `internal/tools/delegate_task_test.go` | Modify | ~80 | Tests for agent-type delegation |
| `internal/worker/tool_executor.go` | Modify | ~20 | URL path interpolation `{{key}}` |
| `internal/worker/tool_executor_test.go` | Modify | ~40 | Tests for URL interpolation |
| `examples/github-cicd/kustomization.yaml` | Create | ~10 | Kustomize manifest |
| `examples/github-cicd/agents.yaml` | Create | ~50 | Coordinator + claude-coder agents |
| `examples/github-cicd/tools.yaml` | Create | ~70 | GitHub Tool CRDs (check-ci, merge-pr, get-pr) |
| `examples/github-cicd/secret.yaml` | Create | ~10 | GitHub token secret template |
| `examples/github-cicd/task.yaml` | Create | ~15 | Example task to trigger CI/CD flow |
| `examples/github-cicd/github-actions-webhook.yaml` | Create | ~40 | GitHub Actions CI failure → Mercan task |
| **Total** | | **~395** | |

## Testing

1. **Unit tests for `delegate_task`**: Verify agent type auto-detection using fake K8s client. Create an Agent with `Runtime` set, delegate to it, assert child task has `Type: TaskTypeAgent` and `AgentRuntime.Workspace` populated.

2. **Unit tests for URL interpolation**: Verify `{{owner}}/{{repo}}` replacement in Tool executor. Test that interpolated keys are removed from the JSON body. Test partial interpolation (some keys in URL, some in body). Test no placeholders (existing behavior unchanged).

3. **Integration test (optional)**: Full flow with envtest — create coordinator task, verify child agent task is created with correct type and workspace config.

## Why Not Build Custom PR Tools

| | Build PR tools | Fix delegate_task + Tool CRDs |
|---|---|---|
| Lines of code | ~500+ (5 new tool files) | ~80 (modify 2 existing files) |
| Worker image changes | Need git, gh installed in AI worker | None (agent workers already have git/gh) |
| Git provider support | GitHub-only or build for each | Tool CRDs work with any REST API |
| Maintenance burden | Own the git/PR logic | Claude Code / Copilot own it |
| Code change quality | Limited custom tool | Full coding agent, battle-tested |
| CI polling logic | Custom implementation | LLM naturally loops with check-ci tool |

## Comparison with Multiclaude

| Capability | Multiclaude | Mercan (after this plan) |
|---|---|---|
| Agent creates branch + PR | Worker agent (fixed role) | Claude Code agent (flexible) |
| CI pass → auto-merge | Merge-queue agent (hardcoded) | Coordinator + merge-pr Tool CRD |
| CI fail → auto-fix | Not built-in | GitHub Actions webhook → new task |
| Human review flow | PR-shepherd agent (fixed role) | Coordinator prompt variation |
| Code review | Reviewer agent (fixed role) | Coordinator + delegate_task |
| Git provider | GitHub only | Any (swap Tool CRDs) |
| Behavior customization | Edit markdown templates | Edit coordinator system prompt |
| Deployment | Local tmux | K8s Jobs with isolation + RBAC |

Mercan's advantage: the workflow logic lives in the coordinator's **prompt**, not in code. The same coordinator can be reconfigured by changing its system prompt — no code changes, no redeployment. Multiclaude's fixed roles require code changes to modify behavior.
