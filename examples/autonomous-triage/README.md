# Autonomous GitHub Triage

This example deploys an autonomous triage workflow that scans a GitHub repository for open issues and pull requests, then delegates coding and review work to specialized agents.

## What It Does

1. **Triage coordinator** scans the target repo for open issues and PRs
2. For unassigned issues, it delegates a **coder** agent to implement a fix and open a PR
3. For open PRs, it delegates a **reviewer** agent to provide a thorough code review
4. The coordinator tracks progress and repeats until `maxIterations` is reached

## Prerequisites

- Kubernetes cluster with Orka installed
- Anthropic API key
- GitHub personal access token (with `repo` scope)

## Setup

### 1. Create the Anthropic API key secret

```bash
kubectl create secret generic anthropic-api-key \
  --from-literal=api-key=$ANTHROPIC_API_KEY
```

### 2. Create the GitHub credentials secret

```bash
kubectl create secret generic github-credentials \
  --from-literal=token=$GITHUB_TOKEN
```

### 3. Deploy the example

```bash
kubectl apply -f examples/autonomous-triage/
```

This creates the Provider, three Agents (coordinator, coder, reviewer), and the Task.

## Monitoring

Watch the task status:

```bash
kubectl get tasks triage-kubeairunway -w
```

View coordinator logs:

```bash
kubectl logs -l orka.ai/task=triage-kubeairunway -f
```

Check delegated child tasks:

```bash
kubectl get tasks -l orka.ai/parent=triage-kubeairunway
```

## Configuration

| Field | Default | Description |
|-------|---------|-------------|
| `coordination.maxIterations` | `10` | Number of autonomous loop iterations |
| `coordination.maxConcurrentChildren` | `3` | Max parallel delegated tasks |
| `coordination.maxDepth` | `2` | Max delegation nesting depth |
| `spec.timeout` | `2h` | Overall task timeout |

### Recurring triage

Add a `schedule` field to the Task spec to run triage on a cron schedule:

```yaml
spec:
  schedule: "0 9 * * 1-5"  # weekdays at 9 AM
```
