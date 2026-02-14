# Autonomous Task Execution

Autonomous mode enables long-running, self-driving development loops. A coordinator agent can autonomously decompose a high-level goal into sub-tasks, implement them, test, iterate, and continue working until the goal is complete.

## Overview

When a task's agent has `coordination.autonomous: true`, the controller runs a loop:

1. Creates a Job for the coordinator agent
2. The agent reads the current plan, delegates sub-tasks, and updates the plan
3. When the Job completes, the controller checks termination conditions
4. If not complete, it creates a new Job (next iteration) with the updated plan
5. Repeats until: max iterations reached, goal marked complete, or user cancels

## Configuration

Enable autonomous mode on an Agent's coordination config:

```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Agent
metadata:
  name: autonomous-coordinator
spec:
  providerRef:
    name: my-provider
  model:
    name: claude-sonnet-4-20250514
  coordination:
    enabled: true
    autonomous: true
    maxIterations: 20    # 0 = unlimited
    maxDepth: 3
    maxConcurrentChildren: 5
    allowedAgents:
      - name: coder
      - name: reviewer
```

Then create a task:

```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Task
metadata:
  name: build-feature
spec:
  type: ai
  agentRef:
    name: autonomous-coordinator
  prompt: "Implement a REST API with user authentication, CRUD operations, and tests"
```

## How It Works

### Controller Loop

The controller manages the autonomous loop at the Kubernetes level:

- Each iteration runs as a separate Job (resilient to pod crashes)
- Plan state is persisted in SQLite between iterations
- The task's `status.iteration` tracks the current iteration number
- Termination conditions are checked after each Job completes

### Plan State

The LLM manages its own plan using the `update_plan` tool:

```json
{
  "summary": "Completed auth module, starting CRUD endpoints",
  "progress_pct": 35,
  "goal_complete": false,
  "plan_document": "# Goal\nBuild REST API...\n\n# Completed\n- [x] Auth module\n..."
}
```

Plan state includes:
- **summary**: Human-readable progress description
- **progress_pct**: Estimated completion percentage (0-100)
- **goal_complete**: Whether the goal has been achieved
- **plan_document**: Freeform markdown plan managed by the LLM

### Termination Conditions

The autonomous loop stops when any of these conditions are met:

1. **Goal complete**: The LLM calls `update_plan` with `goal_complete: true`
2. **Max iterations**: The configured `maxIterations` limit is reached
3. **User cancel**: The task's `suspend` field is set to `true`
4. **Timeout**: The per-iteration timeout is exceeded (fails the task)

## Monitoring

### Task Status

The task status shows the current iteration:

```yaml
status:
  phase: Running
  iteration: 5
  message: "autonomous iteration 5"
```

### Plan API

View the current plan state:

```bash
# Via API
curl -H "Authorization: Bearer $TOKEN" \
  $CONTROLLER_URL/api/v1/tasks/build-feature/plan
```

Response:
```json
{
  "TaskName": "build-feature",
  "Namespace": "default",
  "Iteration": 5,
  "Summary": "Completed auth and CRUD, working on tests",
  "ProgressPct": 70,
  "GoalComplete": false,
  "PlanDocument": "# Goal\n..."
}
```

### Pausing and Resuming

Suspend an autonomous task:

```bash
kubectl patch task build-feature --type=merge -p '{"spec":{"suspend":true}}'
```

The current iteration will complete, then the task will stop.

## Environment Variables

These environment variables are injected into autonomous worker pods:

| Variable | Description |
|----------|-------------|
| `MERCAN_AUTONOMOUS_MODE` | Set to `true` for autonomous tasks |
| `MERCAN_AUTONOMOUS_ITERATION` | Current iteration number (0-based) |
| `MERCAN_AUTONOMOUS_MAX_ITERATIONS` | Max iterations limit (if configured) |

## Tools

### update_plan

Available to autonomous coordinators for managing plan state:

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `summary` | string | Yes | Brief progress summary |
| `progress_pct` | integer | No | Progress percentage (0-100) |
| `goal_complete` | boolean | No | Whether the goal is complete |
| `plan_document` | string | Yes | Full markdown plan document |

## Limitations

- Plan state is stored per-task in SQLite (not shared between tasks)
- Each iteration starts with a fresh LLM context (only plan state carries over)
- Code artifacts must be persisted via git (using workspace/pushBranch)
- Per-iteration timeout applies, not total duration timeout
