# Multi-Model Code Review

This example sets up a multi-model code review workflow where:

1. A **coordinator** manages the full issue-to-PR workflow
2. A **coder** (`default` agent) implements fixes
3. Three **reviewers** (Opus, GPT, Gemini) review in parallel
4. If any reviewer requests changes, the coder fixes and reviewers re-review
5. The coordinator approves only when all reviewers approve

## Setup

```bash
# Prerequisites: copilot-token secret must exist
kubectl get secret copilot-token

# Apply all agents
kubectl apply -f examples/multi-model-review/

# Or individually:
kubectl apply -f examples/multi-model-review/reviewer-opus.yaml
kubectl apply -f examples/multi-model-review/reviewer-gpt.yaml
kubectl apply -f examples/multi-model-review/reviewer-gemini.yaml
kubectl apply -f examples/multi-model-review/dev-coordinator.yaml
```

## Usage

### Via Chat
Just tell the chat to pick up an issue — it will automatically use the `dev-coordinator`:

> Pick up https://github.com/org/repo/issues/42

### Via Task CRD
```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: fix-issue-42
spec:
  type: ai
  agentRef:
    name: dev-coordinator
  prompt: "Fix issue #42 on https://github.com/org/repo"
  agentRuntime:
    workspace:
      gitRepo: "https://github.com/org/repo.git"
      branch: "main"
      gitSecretRef:
        name: copilot-token
  timeout: 1h
```

## Configuring Review Models

Add or remove reviewer agents to change which models review your PRs. Just ensure:

1. The reviewer Agent CRD exists with the desired model
2. The `dev-coordinator`'s `allowedAgents` list includes the reviewer name
3. The coordinator's system prompt references the reviewer name

Example: adding a new reviewer:
```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: reviewer-deepseek
spec:
  runtime:
    type: copilot
    defaultMaxTurns: 30
  model:
    name: deepseek-r1
  systemPrompt:
    inline: |
      You are a code reviewer. [same prompt as other reviewers]
  secretRef:
    name: copilot-token
```

Then update the coordinator:
```bash
kubectl patch agent dev-coordinator --type=json -p='[
  {"op": "add", "path": "/spec/coordination/allowedAgents/-", "value": {"name": "reviewer-deepseek"}}
]'
```

And update the coordinator's system prompt to include `reviewer-deepseek` in the parallel review list.
