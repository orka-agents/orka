---
slug: /multi-agent-coordination
description: "Let a coordinator agent delegate work to specialist agents, with depth, allowlist, and concurrency limits the controller enforces."
---

# Multi-agent coordination

A coordinator agent splits a job into pieces and hands each piece to a specialist agent. The
model decides what to delegate and how to combine the results. The controller decides what is
allowed: which Agents the coordinator may delegate to, how deep delegation may go, and how many
children run at once. Every delegation is an ordinary child Task, so you can watch, cancel, and
audit it like any other Task.

The coordinator is a native `type: ai` Agent. Its children can be native AI Agents, or
coding-agent Agents (Codex, Claude Code, Copilot CLI, OpenCode) that run on RuntimePools.

:::tip[Runnable examples]
[`code-review/`](https://github.com/orka-agents/orka/tree/main/examples/code-review) runs one
code-and-review pass and opens a pull request on approval.
[`self-bootstrapping/`](https://github.com/orka-agents/orka/tree/main/examples/self-bootstrapping)
creates the specialists it needs before delegating to them.
:::

## How delegation works

```text
Task "refactor-auth" (coordinator Agent)              depth 0
├── delegate_task(agent: "backend-dev", ...)  → child Task, depth 1
├── delegate_task(agent: "coder", ...)        → child Task, depth 1
│   └── delegate_task(...)                    → grandchild Task, depth 2
└── wait_for_tasks([...])                     → collected results
```

1. You create a Task whose Agent has `coordination.enabled: true`.
2. Its AI worker gives the model the [coordination tools](../reference/built-in-tools.md#coordination-tools).
3. Each `delegate_task` call creates a child Task owned by the parent. The child is
   `type: agent` when its Agent has `spec.runtime`, and `type: ai` otherwise.
4. The controller checks the child against the [guardrails](#guardrails) before it runs, and
   records every child's progress in the parent's `status.childTasks`.
5. `wait_for_tasks` blocks until the children finish and returns their results.
6. The coordinator writes the final answer and the parent Task completes. Deleting the parent
   deletes its children.

## Set up a coordinator

Create the specialists first. One native AI Agent and one coding agent:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: backend-dev
  namespace: orka-system
spec:
  providerRef:
    name: anthropic
  systemPrompt:
    inline: "You are a backend developer specializing in Go and Kubernetes."
  tools:
    - name: code_exec
    - name: file_read
---
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: coder
  namespace: orka-system
spec:
  model:
    name: gpt-6-astra
  runtime:
    type: codex
```

Then the coordinator. `allowedAgents` is the list of Agents it may delegate to:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: project-coordinator
  namespace: orka-system
spec:
  providerRef:
    name: anthropic
  systemPrompt:
    inline: |
      You are a project coordinator. Break the request into subtasks, delegate each
      one to the right specialist with delegate_task, wait for the results with
      wait_for_tasks, and write a final summary.
  coordination:
    enabled: true
    maxDepth: 2
    maxConcurrentChildren: 3
    allowedAgents:
      - name: backend-dev
      - name: coder
```

Start the work with an ordinary Task:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: refactor-auth
  namespace: orka-system
spec:
  type: ai
  agentRef:
    name: project-coordinator
  prompt: |
    Refactor the authentication module for better security and testability.
    Split the work into API changes and tests.
  timeout: 30m
```

The coding agent needs the [provider proxy](../operations/provider-proxy.md), like any
coding-agent Task. For a long-running goal that needs several rounds of planning, turn on
[autonomous mode](autonomous-tasks.md).

## Watch the children

```bash
orka task children refactor-auth
kubectl -n orka-system get tasks -l orka.ai/parent-task=refactor-auth
```

While it runs, the parent's status lists each child with its Agent, phase, and the first 500
characters of its result:

```yaml
status:
  phase: Running
  childTasks:
    - name: refactor-auth-child-xyz1
      agent: backend-dev
      phase: Running
    - name: refactor-auth-child-xyz2
      agent: coder
      phase: Succeeded
      result: "Updated 12 files to use the new auth context..."
```

Children carry the labels `orka.ai/parent-task` and `orka.ai/delegated-agent`, so you can
select them with `kubectl -l`.

## Guardrails

The controller checks every child against its parent's Agent before the child runs:

| Field | Default | What happens |
| --- | --- | --- |
| `allowedAgents` | `[]` | A child for an Agent not on the list fails. With an empty list, the coordinator can delegate only to Agents it created itself with `create_agent`. |
| `maxDepth` | `3` | A child deeper than this fails. The top-level coordinator is depth 0. |
| `maxConcurrentChildren` | `5` | A child beyond this limit waits, and starts when a sibling finishes. It does not fail. |

The AI worker also bounds each run. A native AI Task gets up to 10 rounds of model calls and
tool calls; a coordinator gets 50, and an autonomous coordinator 100 per iteration.

The full set of coordination fields is in the [Agent reference](../reference/resources.md#agent).

## Tools the coordinator gets

| Purpose | Tools |
| --- | --- |
| Delegate and collect | `delegate_task`, `wait_for_tasks`, `create_container_task`, `cancel_task` |
| Talk between siblings | `send_message`, `check_messages` |
| Remember | `recall_memory`, `remember`, `propose_memory`, `search_transcript` |
| Work with GitHub | `create_pull_request`, `review_pull_request`, `post_review_comment`, `check_pull_request_ci`, `merge_pull_request`, and more |
| Create specialists | `create_agent`, `delete_agent` |
| Plan (autonomous mode) | `update_plan` |

Parameters and return values for each are in [Built-in tools](../reference/built-in-tools.md).

## Let children talk to each other

Children of the same coordinator can coordinate directly with `send_message` and
`check_messages`, for example to share a finding or avoid duplicated work. Messages go through
the controller, never Pod to Pod, and only siblings can read them. Send to `"*"` to broadcast to
every sibling. Messages are deleted with the Tasks that own them.

## Retry failed children

`wait_for_tasks` never retries anything. Delegating with `auto_retry: true` only adds
structured failure details, so the coordinator can decide:

```json
{
  "agent": "coder",
  "prompt": "Implement the feature",
  "auto_retry": true,
  "max_retries": 3
}
```

A failed child then reports:

```json
{
  "task": "parent-task-child-abc",
  "phase": "Failed",
  "failureDetails": {
    "message": "out of memory",
    "retryCount": 0,
    "maxRetries": 3
  }
}
```

To retry, the coordinator calls `delegate_task` again, changing the prompt if it wants to, and
tracks its own attempt budget. `max_retries` defaults to 2 and is information for that
decision, not a limit Orka enforces.

## Delegating with transaction tokens

When the coordinator's Task carries [transaction](../concepts/transaction-tokens.md) metadata,
its children inherit the same transaction identity, so parent and child work can be audited
by one transaction ID. If the worker can exchange tokens, `delegate_task` and
`create_container_task` also request a narrower child token before creating the child. The
worker needs:

```bash
ORKA_CONTEXT_TOKEN_TTS_ENDPOINT=https://transaction-token-tts.transaction-token-system.svc/oauth/token
ORKA_CONTEXT_TOKEN_SUBJECT_TOKEN_FILE=/var/run/orka/transaction-token/token
ORKA_CONTEXT_TOKEN_CHILD_SCOPE=orka:agents:run
```

The child scope must already be in the parent's scopes; asking for more fails before the child
exists. The child token is stored in a short-lived Secret owned by the child Task and mounted
into the child's worker.

## Code, review, and publish loops

A common pattern is a loop between a coding agent and a reviewer until the change is approved,
then a pull request. Coding agents never hold Git credentials: the coder edits a clean checkout,
and Orka's publisher pushes the validated change to `pushBranch` and records the result in the
child's `status.delivery`. The reviewer reads that published branch. The pull request tools
then act with the coder Task's `forgeCredentialRef`, or with the requester's
[linked account](connectors.md); the push credential is never used for the GitHub API.

A delegated child can create a branch but cannot update one that already exists: updating a
branch needs `workspace.expectedRemoteSHA`, which `delegate_task` does not expose. So each
revision publishes to a new branch, starting from the previous revision's branch, and the
coordinator stops once the pull request exists. Repairs to an open pull request belong to a
[repository monitor](repository-monitors.md#pr-repair-and-readiness) or to a direct repair Task
that sets `expectedRemoteSHA`; the
[`github-cicd` example](https://github.com/orka-agents/orka/tree/main/examples/github-cicd)
shows both.

A coordinator prompt for this loop:

```text
You are a coordinator agent. Follow this protocol:

1. DELEGATE implementation to the coder agent with workspace.intent = write,
   gitRepo, publicationGitRepo, a new pushBranch (for example feature-x-1),
   and four distinct Secrets: readCredentialRef (clone),
   publicationReadCredentialRef (verify), publicationCredentialRef (push), and
   forgeCredentialRef (pull request API). Tell the coder to edit only.
2. WAIT for the coder's result and require a verified delivery receipt.
3. DELEGATE review to the reviewer agent with workspace.intent = read on the
   PUBLISHED branch: publicationGitRepo (or gitRepo for same-repository
   publication) at branch = that revision's pushBranch.
4. WAIT for the reviewer's verdict.
5. IF verdict == "CHANGES_NEEDED" AND revision < 3:
   DELEGATE the fix to the coder agent again (workspace.intent = write) with
   branch = the previous pushBranch, a NEW pushBranch (feature-x-2, ...), and
   the reviewer feedback in the prompt. Go to step 2.
6. IF verdict == "APPROVED":
   Call create_pull_request with the last coder task's name and pushBranch,
   then call check_pull_request_ci.
7. Report the PR URL, the review result, and the CI status, and stop. Do not
   delegate further writes to the PR branch.
```

Require a terminal verified delivery receipt before creating a pull request. A model saying
it changed or pushed files is not evidence. [Write and publication
Tasks](../concepts/agent-runtimes.md#write-and-publication-task) describe the receipt.

For a pull request flow that runs without a coordinator — on every PR, or from an issue
label — use [repository monitors](repository-monitors.md) and
[issue-to-PR automation](issue-to-pr-automation.md) instead.
