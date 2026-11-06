---
description: "Let an ACP agent end its turn while it waits for an approval, child tasks, or a backoff, release its runtime, and resume automatically."
---

# Parking agent turns

An agent often spends minutes or hours waiting: for a person to sign off, for the child
tasks it delegated to finish, or for a rate limit to clear. A waiting turn still holds a
RuntimePool slot, a resident RuntimeSession, and possibly a workspace. With `park_until`, the
agent ends its turn instead. Orka releases that runtime capacity, waits for the event, and
then starts a new turn in the same Session.

Parking is **data-only**. Orka never snapshots or restores process memory. The new turn
starts from the Session transcript, with fresh runtime credentials, a fresh execution
snapshot, and the same checks as any other Session continuation. Running processes, shells,
and unsaved in-memory state from the parked turn are gone, and the agent is told so.

## Enable it

`park_until` is a brokered built-in tool for the built-in ACP runtimes (`codex`, `claude`,
`copilot`, `opencode`). Like every brokered tool, it is offered only when the Agent or Task
allows it:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: release-captain
spec:
  runtime:
    type: claude
    defaultAllowedTools: [Read, Grep, Glob, delegate_task, wait_for_tasks, park_until]
```

A Task can park only when all of these hold:

| Requirement | Why |
| --- | --- |
| `spec.sessionRef` is set and `append` is true | The continuation resumes from the Session transcript, so this turn must be recorded there. |
| The Task is not a gateway conversation turn (`promptIncluded`/`throughMessageID`) | The gateway starts the next turn itself. |
| No `spec.transaction` | A continuation cannot inherit a transaction token. Orka fails closed rather than re-issue one. |
| No `spec.schedule` or `spec.priorTaskRef` | Neither can be inherited safely by a later turn. |
| The chain has fewer than 32 continuations | Bounds runaway park loops. |

## What the agent calls

```json
{"kind": "approval", "reason": "The release notes are ready. May I publish v2.4.0?"}
{"kind": "tasks", "reason": "Waiting for the reviewers", "tasks": ["release-captain-child-x7k2p"]}
{"kind": "timer", "reason": "GitHub secondary rate limit", "duration": "15m"}
```

An optional `timeout` (for example `"2h"`) can shorten the wait. The Task deadline always
applies. After the call, the agent should end its turn with a short status message.

| Kind | Resumes when | The new turn is told |
| --- | --- | --- |
| `approval` | A person approves or declines the sign-off request in the Task's **Approvals** tab, with `orka task approve` / `orka task decline`, or through the API. | The decision, who made it, and their note. |
| `tasks` | Every named child Task has finished, following a child's own continuation chain if it parked too. | Each child's final phase. The agent then calls `wait_for_tasks` to read results. |
| `timer` | The duration has elapsed. | That the backoff ended. |

A parked `approval` is a **sign-off checkpoint**. It does not approve any specific tool
call. Tools listed in `approvalRequiredTools` still need their own approval in the turn that
runs them. Those approvals still hold a live tool call for up to ten minutes; see
[Human approval](human-approval-v2.md).

## The lifecycle

`status.wait` on the parked Task records the state, and the `Waiting` condition mirrors it.

```text
Requested ──turn succeeded──▶ Waiting ──event──▶ Resuming ──▶ Resumed (continuation Task created)
    │                            │
    │ turn failed / retried      ├─ cancel-wait annotation or Task deletion ─▶ Cancelled
    ▼                            └─ Task deadline leaves < 1 minute ─────────▶ Expired
Abandoned
```

- **Requested.** The turn called `park_until`. Nothing changes until that exact attempt and
  prompt end. If the turn fails, is cancelled, or is retried, the wait becomes `Abandoned`
  and nothing resumes.
- **Waiting.** The turn settled `Succeeded`, so no prompt, tool call, or publication is still
  in flight. Its runtime capacity has been released:
  - a read-only Session's resident RuntimeSession is retired, which frees its pool slot;
  - write-intent and task-scoped sessions are retired at the end of every turn anyway;
  - a workspace-backed Session follows its frozen `onDetach` policy. With
    `onDetach: Suspend`, the workspace data is cold-suspended and its pool scales to zero.
- **Resuming → Resumed.** Orka reserves a deterministic continuation name in status, then
  creates one continuation Task. It copies the parked Task's agent, runtime, workspace,
  requester, and Session. It changes three things: a controller-authored prompt describing
  the event, `sessionRef.create: false`, and `timeout` set to the time remaining before the
  original deadline. The parked prompt is never sent again.
- When `timeout` expires but the Task deadline still leaves time, the wait resumes anyway and
  tells the agent it timed out. An undecided sign-off is marked expired.

The parked Task keeps the `Succeeded` phase of its own turn. Tools and people that need the
overall result should follow the chain. `wait_for_tasks` does this automatically: a child
whose turn parked is reported as unfinished until its newest continuation finishes, and the
result describes that continuation (`continuationTask`, `waitState`).

## Cancel a wait

```bash
# Stop waiting but keep the Task and its history:
kubectl -n team annotate task release-captain orka.ai/cancel-wait="no longer needed"

# Or delete the Task. Its continuation, if one was already created, is deleted too:
kubectl -n team delete task release-captain
```

A cancelled or expired sign-off cannot be decided later. A late decision receives HTTP 409.

## Waiting for capacity

Separately from parking, a Task that cannot start because runtime capacity is exhausted
reports it with `status.execution.reason: WaitingForCapacity` and a `WaitingForCapacity`
condition:

| Condition reason | Cause |
| --- | --- |
| `RuntimePoolAtCapacity` | The RuntimePool is full or not admitting, or the runtime rate-limited prompt admission. |
| `WorkspaceCapacityUnavailable` | The execution-workspace pool or provider has no admission capacity. |
| `CapacityAvailable` | (False) Capacity is no longer the blocking reason. |

The Task stays `Pending`, is retried automatically, and is cancelled with `TaskTimeout` if its
deadline passes first. A Substrate actor pool that receives `ResourceExhausted` reports
phase `Pending` with a `WaitingForCapacity` reason instead of failing.

## Metrics

| Metric | Labels |
| --- | --- |
| `orka_task_wait_transitions_total` | `kind`, `state` |
| `orka_task_wait_duration_seconds` | `kind`, `state`: time from release to the final state |
| `orka_task_wait_runtime_releases_total` | `result`: resident RuntimeSessions retired on park |
| `orka_waiting_for_capacity_total` | `resource` (`task`, `substrate_actor_pool`), `reason` |

## Not yet supported

- Parking a pending `approvalRequiredTools` call. That approval is bound to the exact
  running call, attempt, and runtime fences, and approved execution must happen under that
  call's frozen policy, so it still waits live.
- Waking on pull request reviews or CI results. GitHub webhooks currently deliver only
  `issues` and `pull_request` events, and they go to repository monitors, not to Tasks.
  Until review and check events are routed to Tasks, park on a `timer` and check again with
  `check_pull_request_ci`.
- Gateway conversations, transaction-bound Tasks, and external `runtimeRef` agents.

The design and its safety boundaries are recorded in
[ADR 0041](https://github.com/orka-agents/orka/blob/main/docs/adr/0041-data-only-parked-turns.md).
