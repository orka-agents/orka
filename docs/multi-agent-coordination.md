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

Located in `internal/tools/delegate_task.go` and `internal/tools/wait_for_tasks.go`. Registered via `RegisterCoordinationTools(k8sClient)` in `internal/tools/registry.go` when `MERCAN_COORDINATION_ENABLED=true`.

#### `delegate_task` Tool

LLM-visible parameter schema:
```json
{
  "type": "object",
  "properties": {
    "agent":     {"type": "string", "description": "Name of the agent to delegate to"},
    "prompt":    {"type": "string", "description": "The task prompt for the agent"},
    "namespace": {"type": "string", "description": "Namespace (defaults to current)"},
    "priority":  {"type": "integer", "description": "Priority 0-1000 (defaults to parent priority)"}
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
5. Returns `{"taskName": "<name>", "status": "created"}` to LLM

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

## Future Extensions (Not in Scope)

- **TaskGroup CRD** — Declarative DAG-based workflows (Layer 2)
- **Agent-to-Agent Messaging** — Real-time inter-agent communication (Layer 3)
- **Cost tracking** — Per-task/child token usage aggregation
- **UI support** — Tree view of coordinator → child tasks in Web UI
- **Recursive delegation** — Child agents that are themselves coordinators (works with maxDepth, but needs testing)
