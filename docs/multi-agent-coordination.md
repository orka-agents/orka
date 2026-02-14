# Multi-Agent Coordination

Enable coordinator agents to dynamically delegate subtasks to specialist agents at runtime. The LLM decides what to delegate, to whom, and how to synthesize results. The controller enforces guardrails (allowed agents, max depth, max concurrency). Each child task is a real Kubernetes Job with full isolation.

## Workflow

```
User creates Task (agentRef: coordinator-agent, prompt: "Refactor auth")
  │
  ▼
Controller: Pending → resolves Agent → coordination.enabled=true → creates Job
  │
  ▼
AI Worker pod starts, LLM receives system prompt + delegate_task/wait_for_tasks tools
  │
  ▼
LLM calls delegate_task(agent: "backend-dev", prompt: "Refactor API layer")
LLM calls delegate_task(agent: "frontend-dev", prompt: "Update UI auth")
  │
  ▼
Worker creates child Tasks via K8s API with:
  - ownerRef → parent Task
  - label: mercan.ai/parent-task = <parent>
  - label: mercan.ai/delegated-agent = <agent>
  - annotation: mercan.ai/coordination-depth = <parent_depth + 1>
  │
  ▼
Controller reconciles child Tasks:
  - Validates agent is in parent's allowedAgents
  - Validates depth < maxDepth
  - Validates active children < maxConcurrentChildren
  - Creates Jobs for children → children run in parallel
  - Updates parent's status.childTasks[]
  │
  ▼
LLM calls wait_for_tasks(tasks: ["child-1", "child-2"])
  - Worker polls child Task status via K8s API
  - Blocks until all reach terminal phase
  - Reads results from SQLite (via controller's internal API), returns aggregated results
  │
  ▼
LLM synthesizes final answer from child results → writes result via HTTP POST to controller
  │
  ▼
Controller: parent Job succeeded → parent Task → Succeeded
  - ChildTaskStatus fully populated
  - Owner references cascade-delete children when parent is deleted
```

## Components

### Coordination Tools

Located in `internal/tools/`. Coordination tools include `delegate_task`, `wait_for_tasks`, PR tools (`create_pull_request`, `merge_pull_request`, `auto_merge_pull_request`, `review_pull_request`, `post_review_comment`), and agent management tools (`create_agent`, `delete_agent`). All are registered via `RegisterCoordinationTools(k8sClient)` in `internal/tools/registry.go` when `MERCAN_COORDINATION_ENABLED=true`.

#### `delegate_task` Tool

LLM-visible parameter schema:
```json
{
  "type": "object",
  "properties": {
    "agent":       {"type": "string", "description": "Name of the agent to delegate to"},
    "prompt":      {"type": "string", "description": "The task prompt for the agent"},
    "namespace":   {"type": "string", "description": "Namespace (defaults to current)"},
    "priority":    {"type": "integer", "description": "Priority 0-1000 (defaults to parent priority)"},
    "auto_retry":  {"type": "boolean", "description": "Enable automatic re-creation if task fails"},
    "max_retries": {"type": "integer", "description": "Max auto-retry attempts (default: 2)"}
  },
  "required": ["agent", "prompt"]
}
```

Implementation (`internal/tools/delegate_task.go`):
1. Reads `MERCAN_TASK_NAME`, `MERCAN_TASK_NAMESPACE`, `MERCAN_COORDINATION_DEPTH` from env
2. Reads `MERCAN_COORDINATION_ALLOWED_AGENTS` and validates the target agent is allowed
3. Checks depth + 1 does not exceed `MERCAN_COORDINATION_MAX_DEPTH`
4. Creates child Task via K8s API with:
   - `GenerateName: {parentName}-child-`
   - `Labels: mercan.ai/parent-task, mercan.ai/coordinator, mercan.ai/delegated-agent`
   - `Annotations: mercan.ai/coordination-depth`
   - `OwnerReferences` pointing to parent Task
   - `Spec.Type: ai`, `Spec.AgentRef.Name: <agent>`, `Spec.Prompt: <prompt>`
5. If `auto_retry: true`, stores retry config as annotations: `mercan.ai/auto-retry`, `mercan.ai/max-retries`, `mercan.ai/retry-count`, `mercan.ai/original-prompt`
6. Returns `{"taskName": "<name>", "status": "created"}` to LLM

#### `wait_for_tasks` Tool

LLM-visible parameter schema:
```json
{
  "type": "object",
  "properties": {
    "tasks":   {"type": "array", "items": {"type": "string"}, "description": "Child task names to wait for"},
    "timeout": {"type": "string", "description": "Max wait duration, e.g. '5m' (default: '10m')"}
  },
  "required": ["tasks"]
}
```

Implementation (`internal/tools/wait_for_tasks.go`):
1. Parses timeout (default 10m)
2. Poll loop (5s interval):
   - Gets each child Task by name via K8s API
   - Checks if all are in terminal phase (Succeeded/Failed)
   - **Auto-retry**: If a failed task has `mercan.ai/auto-retry=true` and retry count < max retries, automatically creates a new child task with the error context prepended to the original prompt, and continues polling the retry task
   - If timeout exceeded, returns partial results with timeout flag
   - Respects context cancellation
3. For each completed child, reads result via the controller's result API
4. Returns aggregated JSON:
   ```json
   {
     "completed": true,
     "results": [
       {"task": "name", "agent": "agent", "phase": "Succeeded", "result": "..."},
       {"task": "name", "agent": "agent", "phase": "Failed", "result": "error: ..."}
     ]
   }
   ```

### Controller Enforcement

Located in `internal/controller/task_controller.go`.

#### `handlePending` — Coordination Validation

After agent resolution, before Job creation, the controller validates coordination constraints for child tasks (identified by `mercan.ai/coordination-depth` annotation):

```go
if depthStr := task.Annotations["mercan.ai/coordination-depth"]; depthStr != "" {
    parentName := task.Labels["mercan.ai/parent-task"]
    depthInt, _ := strconv.Atoi(depthStr)

    // Look up parent task to find its agent's coordination config
    parentTask := &corev1alpha1.Task{}
    r.Get(ctx, types.NamespacedName{Name: parentName, Namespace: task.Namespace}, parentTask)

    parentAgent := &corev1alpha1.Agent{}
    r.Get(ctx, types.NamespacedName{Name: parentTask.Spec.AgentRef.Name, Namespace: task.Namespace}, parentAgent)

    coord := parentAgent.Spec.Coordination
    if coord == nil || !coord.Enabled {
        return r.failTask(ctx, task, "parent agent does not have coordination enabled")
    }

    // 1. Enforce maxDepth
    if int32(depthInt) > coord.MaxDepth {
        return r.failTask(ctx, task, fmt.Sprintf("coordination depth %d exceeds max %d", depthInt, coord.MaxDepth))
    }

    // 2. Enforce allowedAgents
    allowed := false
    for _, a := range coord.AllowedAgents {
        if a.Name == task.Spec.AgentRef.Name {
            allowed = true
            break
        }
    }
    if !allowed {
        return r.failTask(ctx, task, fmt.Sprintf("agent %q not in parent's allowedAgents", task.Spec.AgentRef.Name))
    }

    // 3. Enforce maxConcurrentChildren (requeue if at limit)
    var siblings corev1alpha1.TaskList
    r.List(ctx, &siblings, client.InNamespace(task.Namespace),
        client.MatchingLabels{"mercan.ai/parent-task": parentName})
    active := 0
    for _, s := range siblings.Items {
        if s.Status.Phase == corev1alpha1.TaskPhasePending || s.Status.Phase == corev1alpha1.TaskPhaseRunning {
            active++
        }
    }
    if int32(active) >= coord.MaxConcurrentChildren {
        return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
    }
}
```

#### `handleRunning` — ChildTaskStatus Population

For coordinator tasks (those without a `mercan.ai/parent-task` label), the controller lists child tasks and populates `status.childTasks[]` with name, agent, phase, and truncated result (max 500 chars):

```go
if _, hasChildren := task.Labels["mercan.ai/parent-task"]; !hasChildren {
    // This task might be a coordinator — check for children
    var children corev1alpha1.TaskList
    if err := r.List(ctx, &children, client.InNamespace(task.Namespace),
        client.MatchingLabels{"mercan.ai/parent-task": task.Name}); err == nil && len(children.Items) > 0 {
        task.Status.ChildTasks = make([]corev1alpha1.ChildTaskStatus, 0, len(children.Items))
        for _, child := range children.Items {
            cs := corev1alpha1.ChildTaskStatus{
                Name:  child.Name,
                Phase: child.Status.Phase,
            }
            if child.Spec.AgentRef != nil {
                cs.Agent = child.Spec.AgentRef.Name
            }
            // Include truncated result summary from SQLite store
            if child.Status.Phase == corev1alpha1.TaskPhaseSucceeded {
                data, err := r.resultStore.GetResult(ctx, task.Namespace, child.Name)
                if err == nil {
                    result := string(data)
                    if len(result) > 500 {
                        result = result[:500] + "..."
                    }
                    cs.Result = result
                }
            }
            task.Status.ChildTasks = append(task.Status.ChildTasks, cs)
        }
    }
}
```

### Job Builder — Coordination Config

Located in `internal/controller/job_builder.go`. In `addAIEnvVars`, when an agent has coordination enabled, the following env vars are injected:

```go
if agent != nil && agent.Spec.Coordination != nil && agent.Spec.Coordination.Enabled {
    envVars = append(envVars,
        corev1.EnvVar{Name: "MERCAN_COORDINATION_ENABLED", Value: "true"},
        corev1.EnvVar{Name: "MERCAN_COORDINATION_MAX_DEPTH",
            Value: fmt.Sprintf("%d", agent.Spec.Coordination.MaxDepth)},
        corev1.EnvVar{Name: "MERCAN_COORDINATION_MAX_CHILDREN",
            Value: fmt.Sprintf("%d", agent.Spec.Coordination.MaxConcurrentChildren)},
    )

    var agentNames []string
    for _, a := range agent.Spec.Coordination.AllowedAgents {
        agentNames = append(agentNames, a.Name)
    }
    envVars = append(envVars,
        corev1.EnvVar{Name: "MERCAN_COORDINATION_ALLOWED_AGENTS",
            Value: strings.Join(agentNames, ",")},
    )

    // Current depth (0 for top-level coordinator)
    depth := "0"
    if d, ok := task.Annotations["mercan.ai/coordination-depth"]; ok {
        depth = d
    }
    envVars = append(envVars,
        corev1.EnvVar{Name: "MERCAN_COORDINATION_DEPTH", Value: depth},
    )
}
```

The `delegate_task` and `wait_for_tasks` tools are automatically injected into the tools list when coordination is enabled.

### AI Worker Wiring

Located in `workers/ai/main.go`. When `MERCAN_COORDINATION_ENABLED=true`:

- Coordination tools are registered into `DefaultRegistry` via `tools.RegisterCoordinationTools(k8sClient)`
- `maxIterations` is increased from 10 to 50 for coordinator agents

## RBAC

The worker ServiceAccount (`mercan-worker`) ClusterRole in `config/rbac/worker_role.yaml` includes:

```yaml
- apiGroups: ["core.mercan.ai"]
  resources: ["tasks"]
  verbs: ["get", "list", "watch", "create"]
- apiGroups: ["core.mercan.ai"]
  resources: ["agents"]
  verbs: ["get", "create", "update", "delete"]
```

## Example YAML

### Coordinator Agent
```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Agent
metadata:
  name: project-coordinator
spec:
  providerRef:
    name: anthropic-provider
  model:
    name: claude-sonnet-4-20250514
  systemPrompt:
    inline: |
      You are a project coordinator. Your job is to break complex tasks into
      subtasks and delegate them to specialist agents using the delegate_task
      tool. Once all subtasks complete, use wait_for_tasks to collect results
      and synthesize a final summary.

      Available specialist agents and their capabilities will be provided.
      Delegate appropriately based on the task requirements.
  coordination:
    enabled: true
    maxConcurrentChildren: 3
    maxDepth: 2
    allowedAgents:
      - name: backend-dev
      - name: frontend-dev
      - name: test-writer
  secretRef:
    name: llm-credentials
```

### Specialist Agents
```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Agent
metadata:
  name: backend-dev
spec:
  providerRef:
    name: anthropic-provider
  model:
    name: claude-sonnet-4-20250514
  systemPrompt:
    inline: "You are a backend developer specializing in Go and Kubernetes."
  tools:
    - name: code_exec
    - name: file_read
  secretRef:
    name: llm-credentials
---
apiVersion: core.mercan.ai/v1alpha1
kind: Agent
metadata:
  name: frontend-dev
spec:
  providerRef:
    name: anthropic-provider
  model:
    name: claude-sonnet-4-20250514
  systemPrompt:
    inline: "You are a frontend developer specializing in React and TypeScript."
  tools:
    - name: code_exec
    - name: file_read
  secretRef:
    name: llm-credentials
```

### Coordination Task
```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Task
metadata:
  name: refactor-auth
spec:
  type: ai
  agentRef:
    name: project-coordinator
  prompt: |
    Refactor the authentication module for better security and testability.
    Break this into backend API changes and frontend UI updates.
    Ensure all changes are tested.
  timeout: 30m
  priority: 800
```

### Expected Task Status (while running)
```yaml
status:
  phase: Running
  startTime: "2026-02-06T10:00:00Z"
  jobName: refactor-auth-job-abc12345
  childTasks:
    - name: refactor-auth-child-xyz1
      agent: backend-dev
      phase: Running
    - name: refactor-auth-child-xyz2
      agent: frontend-dev
      phase: Succeeded
      result: "Updated 12 components to use new auth context..."
```

## File Summary

| File | Description |
|---|---|
| `internal/tools/delegate_task.go` | delegate_task tool implementation |
| `internal/tools/delegate_task_test.go` | Unit tests with fake K8s client |
| `internal/tools/wait_for_tasks.go` | wait_for_tasks tool implementation |
| `internal/tools/wait_for_tasks_test.go` | Unit tests with fake K8s client |
| `internal/tools/create_pull_request.go` | create_pull_request tool implementation |
| `internal/tools/create_pull_request_test.go` | Unit tests with fake K8s client |
| `internal/tools/merge_pull_request.go` | merge_pull_request tool implementation |
| `internal/tools/merge_pull_request_test.go` | Unit tests with fake K8s client |
| `internal/tools/auto_merge_pull_request.go` | auto_merge_pull_request tool implementation |
| `internal/tools/auto_merge_pull_request_test.go` | Unit tests with fake K8s client |
| `internal/tools/review_pull_request.go` | review_pull_request tool implementation |
| `internal/tools/review_pull_request_test.go` | Unit tests with fake K8s client |
| `internal/tools/post_review_comment.go` | post_review_comment tool implementation |
| `internal/tools/post_review_comment_test.go` | Unit tests with fake K8s client |
| `internal/tools/create_agent.go` | create_agent tool implementation |
| `internal/tools/create_agent_test.go` | Unit tests with fake K8s client |
| `internal/tools/delete_agent.go` | delete_agent tool implementation |
| `internal/tools/delete_agent_test.go` | Unit tests with fake K8s client |
| `internal/tools/registry.go` | `RegisterCoordinationTools()` function |
| `internal/controller/task_controller.go` | Coordination validation + ChildTaskStatus population |
| `internal/controller/task_controller_test.go` | Tests for coordination enforcement |
| `internal/controller/job_builder.go` | Coordination env vars for worker pods |
| `internal/controller/job_builder_test.go` | Tests for coordination env vars |
| `workers/ai/main.go` | Register coordination tools, increase maxIterations |
| `config/rbac/worker_role.yaml` | Task create + ConfigMap list permissions |

## Testing

1. **Unit tests** for `delegate_task` and `wait_for_tasks` tools using `sigs.k8s.io/controller-runtime/pkg/client/fake`
2. **Controller tests** for coordination validation (depth, allowedAgents, concurrency) using envtest
3. **Job builder tests** verifying coordination env vars are set correctly

## Self-Healing Coordination

When `auto_retry` is enabled on a delegated task, `wait_for_tasks` automatically re-creates failed child tasks with the error context prepended to the original prompt.

### How It Works

1. Coordinator calls `delegate_task` with `auto_retry: true` (and optional `max_retries`, default 2)
2. `delegate_task` stores retry config as annotations on the child task:
   - `mercan.ai/auto-retry: "true"`
   - `mercan.ai/max-retries: "2"`
   - `mercan.ai/retry-count: "0"`
   - `mercan.ai/original-prompt: "<original prompt>"`
3. If the child task fails, `wait_for_tasks` detects the failure and:
   - Checks `retry-count < max-retries`
   - Creates a new child task with the error message prepended:
     ```
     PREVIOUS ATTEMPT FAILED (attempt 1 of 2):
     <error message>

     Please retry the original task, avoiding the previous error:
     <original prompt>
     ```
   - Sets `mercan.ai/retried-from` annotation on the retry task
   - Increments `retry-count`
   - Continues polling the retry task
4. The original failed task result includes `failureDetails` with message, retryCount, and maxRetries
5. If retries are exhausted, the task is reported as failed with full failure details

### Example

```json
{
  "agent": "coder",
  "prompt": "Implement the feature",
  "auto_retry": true,
  "max_retries": 3
}
```

### Result Format

When a task is retried, its result includes:
```json
{
  "task": "parent-task-child-abc",
  "phase": "Retried",
  "retried": true,
  "retryTaskName": "parent-task-child-abc-retry-xyz",
  "failureDetails": {
    "message": "out of memory",
    "retryCount": 0,
    "maxRetries": 3
  }
}
```

## Iterative Code Review Workflows

Mercan supports iterative multi-agent workflows where a coordinator orchestrates coding, review, and feedback loops until code is approved.

### Flow

```
COORDINATOR (AI worker)
  │
  ├── delegate_task(agent="coder", prompt="Implement feature",
  │                 workspace={gitRepo: "upstream/repo",
  │                            pushBranch: "feature/my-change",
  │                            gitSecretRef: "git-credentials"})
  │     Coder: clones repo → writes code → auto-pushes to branch
  │            → result includes cumulative diff + pushBranch
  │
  ├── wait_for_tasks → gets {summary, files, pushBranch} (diff stripped)
  │
  ├── delegate_task(agent="reviewer", prompt="Review changes",
  │                 prior_task="coder-task-xyz")
  │     Reviewer: clones repo → applies coder's diff → reviews
  │               → result: {verdict: "CHANGES_NEEDED", feedback: "Add tests"}
  │
  ├── wait_for_tasks → gets {verdict, feedback}
  │
  ├── delegate_task(agent="coder", prompt="Fix: add tests",
  │                 prior_task="coder-task-xyz", feedback="Add tests")
  │     Coder iter 2: clones repo → applies iter 1 diff → fixes → pushes
  │
  ├── (review loop repeats until APPROVED or max iterations)
  │
  ├── create_pull_request(task_name="coder-task-xyz",
  │                       head_branch="feature/my-change",
  │                       base_branch="main", title="feat: my change")
  │     → creates PR via GitHub API using git credentials from task
  │
  ├── review_pull_request(task_name="coder-task-xyz", pr_number=42)
  │     → fetches PR diff and file changes for analysis
  │
  ├── post_review_comment(task_name="coder-task-xyz", pr_number=42,
  │                       body="LGTM", event="APPROVE")
  │     → posts review with verdict and optional line comments
  │
  ├── merge_pull_request(task_name="coder-task-xyz", pr_number=42)
  │     → verifies CI checks pass, then merges the PR (instant, fails if CI not green)
  │
  ├── auto_merge_pull_request(task_name="coder-task-xyz", pr_number=42)
  │     → polls CI checks every 30s and auto-merges when green (blocks up to timeout)
  │
  └── COMPLETE with merged PR
```

### delegate_task Iteration Parameters

| Parameter | Type | Description |
|-----------|------|-------------|
| `prior_task` | string | Name of a previously completed task whose diff to apply before starting |
| `feedback` | string | Review feedback prepended to the prompt as `FEEDBACK FROM REVIEW: ...` |
| `workspace.pushBranch` | string | Remote branch to auto-push changes to after the agent completes |

When `prior_task` is set:
- `PriorTaskRef` is set on the child Task, triggering diff application in the worker
- Workspace config is copied from the prior task if not explicitly provided
- Iteration labels are tracked: `mercan.ai/iteration` (incremented) and `mercan.ai/iteration-group` (shared ID)

### Structured Result Format

Workers with git workspaces produce structured results:

```json
{
  "version": 1,
  "summary": "Implemented JWT authentication middleware",
  "baseSHA": "abc123def456",
  "diff": "diff --git a/auth.go ...",
  "verdict": "APPROVED",
  "feedback": "Looks good, minor nit on line 42",
  "files": ["auth.go", "middleware.go"],
  "pushBranch": "feature/jwt-auth",
  "metadata": {}
}
```

- `diff` is always **cumulative** (`git diff --binary --full-index` from clean checkout)
- `verdict`: `APPROVED` or `CHANGES_NEEDED`
- `wait_for_tasks` **strips the diff** from results returned to the coordinator to prevent context bloat
- Plain-text results remain backward compatible

### Iteration Labels

| Label | Description |
|-------|-------------|
| `mercan.ai/iteration` | Iteration number (1, 2, 3...) |
| `mercan.ai/iteration-group` | Shared ID grouping all iterations of the same logical task |

### Recommended Coordinator System Prompt

```
You are a coordinator agent. Follow this protocol:

1. DELEGATE implementation to the coder agent.
2. WAIT for the coder's result.
3. DELEGATE review to the reviewer agent (pass prior_task = coder's task name).
4. WAIT for the reviewer's verdict.
5. IF verdict == "CHANGES_NEEDED" AND iteration < 3:
   DELEGATE fix to the coder agent with prior_task + feedback.
   Go to step 2.
6. IF verdict == "APPROVED":
   Report final result.
7. Report final result.
```

### Environment Variables

| Variable | Set When | Description |
|----------|----------|-------------|
| `MERCAN_PRIOR_TASK` | Task has PriorTaskRef | Name of the prior task |
| `MERCAN_PRIOR_TASK_NAMESPACE` | Task has PriorTaskRef | Namespace of the prior task |
| `MERCAN_FORK_REPO` | Workspace has ForkRepo | Fork repository URL |
| `MERCAN_PR_BASE_BRANCH` | Workspace has PRBaseBranch | PR target branch |
| `MERCAN_PUSH_BRANCH` | Workspace has PushBranch | Branch to auto-push changes to |

### create_pull_request Tool

Creates a GitHub pull request from a branch that was pushed by a completed agent task.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `task_name` | string | yes | Name of the completed child task (used to look up workspace config and git credentials) |
| `head_branch` | string | yes | Branch containing the changes (the `pushBranch` from the coder task) |
| `base_branch` | string | yes | Target branch to merge into (e.g. `main`) |
| `title` | string | yes | Pull request title |
| `body` | string | no | Pull request body in Markdown |

The tool reads the git credentials from the child task's `gitSecretRef` secret (looks for `token` or `password` key) and calls the GitHub REST API. The coordinator must have RBAC access to read Secrets.

### review_pull_request Tool

Fetches a GitHub pull request's diff, file changes, and metadata for code review. Returns the full unified diff, individual file patches with change statistics, and PR metadata (title, body, author, branches).

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `task_name` | string | yes | Name of the child task whose workspace config has the repo and git credentials |
| `pr_number` | integer | yes | GitHub pull request number to review |

Returns:
```json
{
  "pr_title": "feat: add auth middleware",
  "pr_body": "Adds JWT-based authentication...",
  "pr_author": "dev-bot",
  "base_branch": "main",
  "head_branch": "feature/jwt-auth",
  "diff": "diff --git a/auth.go ...",
  "files": [
    {"filename": "auth.go", "status": "added", "additions": 42, "deletions": 0, "patch": "..."}
  ],
  "status": "fetched"
}
```

Usage: Call `review_pull_request` after `create_pull_request` to fetch the PR diff, then analyze the changes and submit feedback via `post_review_comment`.

### post_review_comment Tool

Posts a review on a GitHub pull request with a verdict (APPROVE, REQUEST_CHANGES, or COMMENT) and optional line-level comments.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `task_name` | string | yes | Name of the child task whose workspace config has the repo and git credentials |
| `pr_number` | integer | yes | GitHub pull request number |
| `body` | string | yes | Top-level review body text |
| `event` | string | yes | Review verdict: `APPROVE`, `REQUEST_CHANGES`, or `COMMENT` |
| `comments` | array | no | Line-level review comments (each with `path`, `line`, `body`) |

Each item in `comments`:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `path` | string | yes | File path relative to repo root |
| `line` | integer | yes | Line number in the diff (new file line number) |
| `body` | string | yes | Comment text |

Returns:
```json
{
  "review_id": 12345,
  "status": "submitted",
  "html_url": "https://github.com/owner/repo/pull/1#pullrequestreview-12345"
}
```

### merge_pull_request Tool

Merges a GitHub pull request after verifying all CI checks have passed. Supports merge, squash, and rebase merge methods.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `task_name` | string | yes | Name of the child task whose workspace config has the repo and git credentials |
| `pr_number` | integer | yes | GitHub pull request number to merge |
| `merge_method` | string | no | Merge method: `merge`, `squash`, or `rebase` (defaults to `squash`) |
| `commit_title` | string | no | Custom merge commit title |
| `commit_message` | string | no | Custom merge commit message |

Returns:
```json
{
  "sha": "abc123def456",
  "merged": true,
  "checks_passed": true,
  "message": "Pull request merged successfully"
}
```

If CI checks have not passed, returns `{"merged": false, "checks_passed": false, "message": "CI checks have not all passed: ..."}` without failing.

### auto_merge_pull_request Tool

Polls GitHub CI checks and automatically merges a pull request when all checks pass. Unlike `merge_pull_request` which checks CI once and fails if not green, this tool blocks with a configurable timeout (default 30 minutes), polling every 30 seconds until CI passes, fails, or the PR is closed/merged externally. Resilient to transient GitHub API errors (429/5xx).

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `task_name` | string | yes | Name of the child task whose workspace config has the repo and git credentials |
| `pr_number` | integer | yes | GitHub pull request number to merge |
| `merge_method` | string | no | Merge method: `merge`, `squash`, or `rebase` (defaults to `squash`) |
| `commit_title` | string | no | Custom merge commit title |
| `commit_message` | string | no | Custom merge commit message |
| `timeout` | string | no | Maximum wait duration, e.g. `30m`, `1h` (defaults to `30m`) |

Returns:
```json
{
  "merged": true,
  "checks_passed": true,
  "sha": "abc123def456",
  "message": "Pull request merged successfully",
  "outcome": "merged"
}
```

Possible `outcome` values:
- `"merged"` — CI passed and PR was merged successfully
- `"ci_failed"` — A CI check completed with failure; returns immediately with `checks_details`
- `"closed"` — PR was closed externally during polling
- `"already_merged"` — PR was already merged when checked
- `"timeout"` — Timeout exceeded while CI checks were still pending

### Recommended Coordinator System Prompt

```
You are a coordinator agent. Follow this protocol:

1. DELEGATE implementation to the coder agent with pushBranch set.
2. WAIT for the coder's result.
3. DELEGATE review to the reviewer agent (pass prior_task = coder's task name).
4. WAIT for the reviewer's verdict.
5. IF verdict == "CHANGES_NEEDED" AND iteration < 3:
   DELEGATE fix to the coder agent with prior_task + feedback.
   Go to step 2.
6. IF verdict == "APPROVED":
   Call create_pull_request with the coder's task name and pushBranch.
   Call review_pull_request to fetch the PR diff for final review.
   Call post_review_comment to approve the PR.
   Call merge_pull_request to merge the PR after CI passes.
   Or call auto_merge_pull_request to wait for CI and merge automatically.
7. Report final result with the PR URL.
```

## Autonomous Mode

For long-running goals that require multiple planning and execution cycles, enable autonomous mode on the coordinator agent. See [Autonomous Task Execution](autonomous-tasks.md) for details.

When `coordination.autonomous: true` is set, the controller runs the coordinator in a loop — each iteration gets the accumulated plan state, delegates sub-tasks, and updates the plan. The loop continues until the goal is complete, max iterations are reached, or the task is suspended.

- **TaskGroup CRD** — Declarative DAG-based workflows (Layer 2)
- **Agent-to-Agent Messaging** — Real-time inter-agent communication (Layer 3)
- **Cost tracking** — Per-task/child token usage aggregation
- **UI support** — Tree view of coordinator → child tasks in Web UI
- **Recursive delegation** — Child agents that are themselves coordinators (works with maxDepth, but needs testing)
