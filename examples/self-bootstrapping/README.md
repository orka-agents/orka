# Self-bootstrapping coordinator

This example demonstrates Orka's self-bootstrapping agent pattern, where a coordinator
agent dynamically creates specialist agents and delegates work to them. The
deliverable is a reviewed implementation plan. For repository changes, use
[code-review](../code-review).

## How It Works

1. A coordinator agent is configured with coordination enabled
2. When given a task, it analyzes the requirements and creates specialist agents
3. Specialists execute their assigned work in parallel
4. The coordinator reviews results and iterates if needed
5. Deleting the parent Task removes its owned specialist Agents and child Tasks

## Usage

Update `spec.providerRef.name` in `coordinator-agent.yaml` to match the Provider CRD in your cluster before applying it.

### Via YAML
```bash
kubectl -n orka-system apply -f examples/self-bootstrapping/coordinator-agent.yaml
task_name="$(kubectl -n orka-system create -f examples/self-bootstrapping/coordinator-task.yaml -o jsonpath='{.metadata.name}')"
kubectl -n orka-system get task "$task_name" -w
```

### Via Chat (One-Shot)
In the Orka chat, simply ask:
> "Create a coordinator to produce a reviewed implementation plan for a TODO REST API in Go"

The chat can bootstrap the coordinator and let it create the specialists it needs.

### Via CLI
```bash
orka -n orka-system task create --agent coordinator "Plan a TODO REST API in Go with CRUD endpoints and tests"
```

## Key Concepts

- **Auto-cleanup**: Specialist agents have owner references to the coordinator task,
  so they are garbage collected when the task is deleted
- **Provider inheritance**: Specialists inherit the coordinator's LLM provider config
- **Depth limits**: Coordination depth is enforced (default: 3 levels) to prevent infinite loops
- **Concurrency limits**: Maximum concurrent child tasks is configurable (default: 5)
