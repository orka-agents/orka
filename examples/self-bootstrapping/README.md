# Self-Bootstrapping Coordinator Example

This example demonstrates Mercan's self-bootstrapping agent pattern, where a coordinator
agent dynamically creates specialist agents and delegates work to them.

## How It Works

1. A coordinator agent is configured with coordination enabled
2. When given a task, it analyzes the requirements and creates specialist agents
3. Specialists execute their assigned work in parallel
4. The coordinator reviews results and iterates if needed
5. Specialist agents are automatically cleaned up when the coordinator task completes

## Usage

### Via YAML
```bash
kubectl apply -f coordinator-agent.yaml
kubectl apply -f coordinator-task.yaml
```

### Via Chat (One-Shot)
In the Mercan chat, simply ask:
> "Create a coordinator to build a TODO REST API in Go with tests"

The chat will use `create_agent` with `initialPrompt` to bootstrap everything in one step.

### Via CLI
```bash
mercan task create --agent coordinator "Build a TODO REST API in Go with CRUD endpoints and tests"
```

## Key Concepts

- **Auto-cleanup**: Specialist agents have owner references to the coordinator task,
  so they are garbage collected when the task is deleted
- **Provider inheritance**: Specialists inherit the coordinator's LLM provider config
- **Depth limits**: Coordination depth is enforced (default: 3 levels) to prevent infinite loops
- **Concurrency limits**: Maximum concurrent child tasks is configurable (default: 5)
