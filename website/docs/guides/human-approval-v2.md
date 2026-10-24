---
description: "Hold a consequential tool call for a person's decision, then run exactly the action they approved."
---

# Human approval

Some actions should not happen unattended: creating a work order, deploying, spending money,
writing to someone's account. Orka can hold such a call until a person approves or declines
it, then run exactly the stored action — never a regenerated one — and return the real result
to the agent.

Approvals appear in three places:

| Where | What gets approved | Set up |
| --- | --- | --- |
| Native autonomous coordinators (`type: ai`) | Custom Tools listed in `coordination.approvalRequiredTools`, and requests the agent makes with `request_approval`. The Task parks until a decision arrives. | [Autonomous tasks](autonomous-tasks.md#human-approvals) |
| External runtimes, such as AgentKit and Foundry agents | Orka-executed tools in the runtime's `approvalRequiredTools`. The tool call stays open while it waits. | This page |
| Linked accounts | Every write tool that acts through a person's [connector](connectors.md) | Automatic |
| Built-in runtimes (Codex, Claude, Copilot, OpenCode) | The runtime's own requests to run a tool, for a Task with `agentRuntime.permissionDecisions: Requester`. The prompt waits while the request is held. | [Permission requests](#permission-requests-from-built-in-runtimes) |

People decide in the Task's **Approvals** tab in the dashboard, with `orka task approve` and
`orka task decline`, or through the API. Most of this page covers the external-runtime
path; the last section covers built-in runtimes' permission requests.

## Approvals for external runtimes

Orka can let an agent look up inventory automatically and require a person to approve a
proposed work order. The controller saves the exact action before showing it for review.
The tool has not run at that point. Approval lets Orka execute that stored action and return
its real result to the waiting agent.

This applies to tools executed by Orka for qualified AgentKit and Foundry
`orka.harness.v2` runtimes. Runtime-native shell and file permissions are a
separate capability; see
[Permission requests from built-in runtimes](#permission-requests-from-built-in-runtimes).

:::tip[Video demo]
Watch [Human approval before an agent takes action](https://www.youtube.com/watch?v=KgzcvW1WznE).
:::

## Configure the policy

An external `AgentRuntime` declares an explicit, sorted MCP policy and a
runtime profile whose policy digests match it:

```yaml
spec:
  contractVersion: orka.harness.v2
  capabilities:
    mcpPolicy:
      allowedTools: [create-work-order, read-inventory]
      disallowedTools: []
      allowBash: false
      approvalRequiredTools: [create-work-order]
```

The registered adapter must advertise `supportsBrokeredToolApprovals` and
pass conformance. Task `agentRuntime.allowedTools` must exactly match the
registered policy. Per-Task settings cannot override that frozen profile.
Brokered approval support is off by default. After qualifying the exact
adapter and hosted configuration, set the supervisor's
`ORKA_ACP_BROKERED_TOOL_APPROVAL_PROFILE_DIGEST` to its complete runtime-profile
digest. A mismatched opt-in fails startup. Requalify after changing that profile.
Use the profile renderer and manifests in
[`examples/human-approval-v2`](https://github.com/orka-agents/orka/tree/main/examples/human-approval-v2)
for complete configuration, including immutable adapter and image digests.

The example tools run against a disposable HTTP counter. One lookup returns
inventory without review. One simulated action increments the counter only
after approval. It does not create equipment orders or make purchases.

## Review the action

Open the Task's Approvals tab. It shows the proposed operation, a sanitized
input preview, and the review deadline. A `call-tool` wrapper also names its
inner operation when that name is present in the inputs. Sensitive values are
redacted and oversized previews are bounded. The original executable inputs
remain private; the preview is never used as execution input.

The same records and decisions are available through the existing API:

```http
GET /api/v1/tasks/:id/approvals?namespace=<ns>

POST /api/v1/tasks/:id/approvals/:approvalID/decision?namespace=<ns>
Content-Type: application/json

{"decision":"approve","reason":"Reviewed the asset and quantity"}
```

The list response names the Task it was filtered against (`taskUID`), its
phase at that same read (`taskPhase`), and whether it is being deleted
(`taskDeleting`), so a poller such as `orka task approvals --watch` can tell
whether a pending request can still be decided without a second lookup by
name. Decisions are refused for finished and deleting Tasks.

Use `decline` to reject the action. Reviewers need `update` on
`core.orka.ai/tasks/approvals` and `patch` on the parent Task. Gateway-owned
Tasks also enforce their existing operate permission. See the
[authorization reference](../reference/api-authorization.md#route-permissions).
The first terminal decision wins. While the bound Task run remains active,
repeating that decision returns its saved state. A competing decision receives
a conflict. Once the run settles or its binding becomes stale, even an identical
decision retry receives HTTP 409.

The panel separates the review decision from execution. An approved action
can still fail, become stale before starting, or have an unknown outcome.
History records the reviewer and decision, then `not_started`, `running`,
`succeeded`, `failed`, or `unknown` as execution evidence becomes available.
When the tool sent nothing or the remote system refused the request, the
outcome is `failed` and its reason says why, for example the HTTP status and
message GitHub refused a missing permission with; nothing changed. An unknown
outcome means the action may have run. Check the target system before
choosing whether to make a new request. The controller logs the redacted
cause of both.

## Waiting and cancellation

Review has a maximum of ten minutes. Approved execution has a maximum of
four minutes. Approval-required HTTP Tool timeouts must be shorter than
four minutes. The MCP call allows fifteen minutes to cover both phases and
transport overhead. The Task deadline can shorten either phase and the
runtime must keep renewing its normal prompt lease. Configure a Task timeout
that leaves room for initial agent work as well as the review.

Decline, expiry, and cancellation prevent an unstarted action from running.
Before execution, Orka rechecks the Task identity and active attempt, runtime
and controller fences, tool definition, and frozen policy. A late decision
cannot revive an ended run. Waiting holds no global execution lock, so other
Tasks in separate RuntimeSessions can progress when capacity is available.

If the agent receives the cancellation result and finishes before prompt
cancellation reaches it, Orka retains that completed result. The approval
stays cancelled and the action does not run.

Direct AgentKit receives the longer MCP timeout from the supervisor. Hosted
AgentKit also needs persistent continuation state with a TTL of 1800 seconds
and a pinned Foundry agent version whose idle timeout is at least 1800 seconds.
See the [adapter contract](../development/agent-runtime-adapter-contract.md#brokered-tool-approvals)
for exact settings and final error codes.

## Recovery and evidence

Orka stores the original request in an immutable Secret owned by the Task.
It binds the review to the Task UID, attempt, prompt, tool call and operation,
runtime identity, arguments, tool descriptor, and policy. Events expose safe
metadata and fingerprints. They do not contain the executable request,
downstream credentials, or raw tool results.

An interrupted broker connection preserves a pending review. Exact redelivery
can resume it only while the original authority remains valid. A new
controller epoch or supervisor boot invalidates the old run. Those cases keep
their evidence and do not replay the prompt or transfer approval to a new run.
A pending review whose runtime has disappeared becomes non-actionable when
the Task ends or its review deadline expires.

Execution reserves a durable external-effect record before review and claims
it once after approval. A committed result can be returned again without
calling the tool. After a controller restart, Orka repairs missing approval
execution events from those durable records. A committed result restores its
outcome; a started action without a committed result becomes `unknown`.
Orka never takes over an expired execution lease to try the action again.
Task ownership removes the private request Secret when the Task is deleted;
retain the Task and its durable stores while investigating an unknown outcome.

Enroll external Kubernetes runtimes with `deployment.kubernetesRecovery` before
submitting Tasks if cleanup must survive supervisor loss. Enrollment binds the
exact Deployment, Pod, container, and authentication identity. Use a stable
runtime instance ID and a newly generated boot ID at each supervisor start.
The supported topology has one AgentKit supervisor, or a Foundry supervisor
and broker. Init and ephemeral containers invalidate recovery enrollment.

For Foundry recovery, first upgrade the controller and qualify the broker, then
set the supervisor's `ORKA_ACP_FOUNDRY_RECOVERY_PROFILE_DIGEST` to the exact
runtime profile digest. Recovery is default-off and independent of approval
qualification. It records the broker's durable ledger identity before
admission. After supervisor loss, Orka requires proof that the original
container terminated and that the same broker permanently sealed and retired
every remote session under that boot. Keep its ledger intact through recovery.
Missing historical enrollment cannot be recreated after the original compute
is gone. Cleanup remains blocked when that evidence is absent.

Crash validation checks normal Task deletion after those receipts are recorded,
including product finalizer release, private request reclamation, and eventual
release of retained boot credentials when the registration is deleted.

The example acceptance runner uses the real Task and approval APIs. It checks
the counter before approval, after repeated decisions, after decline and
cancellation, and after expiry. Run it for both a direct AgentKit registration
and an AgentKit agent hosted behind the Foundry bridge. Local deterministic
model or hosted-process runs prove those transports; Azure-hosted acceptance
also requires the deployed Foundry gateway and the configured hosted version.

## Permission requests from built-in runtimes

Built-in runtimes ask Orka before they run some tools, and by default Orka answers each
request from the Task's frozen tool policy. In that default, a runtime runs the shell commands
and file changes the policy allows without asking. To have a person decide instead, set:

```yaml
spec:
  agentRuntime:
    permissionDecisions: Requester
```

- Orka then launches the runtime so that it asks before every shell command and every file
  write or edit the policy allows, including read-only commands. Tools outside the policy
  stay removed or denied, exactly as in the default. Only the built-in runtimes can be
  launched this way; a Task that sets `Requester` for an external AgentRuntime is rejected
  before it runs.
- Orka names each request from the runtime's structured description of the call, such as
  its kind and arguments, never from its display title. A request it cannot name is refused.
- Orka still refuses whatever the policy refuses. A request the policy would allow is held
  for the Task's requester instead: the approvals list shows it with `kind: permission`,
  the tool name, the runtime's description of the call and the offered `options`, and the
  Task reports `WaitingForApproval`.
- Only the Task's requester may decide it: the person whose verified OIDC or context-token
  sign-in created the Task, recorded in `spec.requestedBy`. Other reviewers, including
  ServiceAccounts with `update` on `tasks/approvals`, receive HTTP 403. A Task without a
  verified requester has every such request refused.
- `approve` answers the runtime's allow-once option and `decline` its reject-once option.
  Orka never chooses an "always" option, so every later call asks again.
- A held request expires ten seconds before the runtime's own permission timeout, or at the
  Task deadline if that is sooner, and is then refused. The runtime waits five minutes by
  default; set the controller's `--acp-permission-timeout` (between 1m and 1h) to give people
  longer. Runtime Pods take the new value when their RuntimePool is replaced.
- When the prompt ends or is cancelled first, the request is recorded as `cancelled`. A
  request held when the controller restarts cannot be decided afterwards, because the run
  it belonged to ended.
- The setting is part of each Task, not of the runtime profile, so it never moves a Task to
  another RuntimePool. A Session's runtime session, however, is launched in one mode: when a
  turn's setting differs from the previous turn's, Orka retires the runtime session and
  starts a new one from the Session transcript and its verified branch, as it does when the
  tool policy changes. That turn starts slower and the agent keeps only what the transcript
  carries. Orka never reuses a runtime session launched in the other mode.
- How a declined request ends depends on the runtime. Claude Code tells the model and
  continues. Copilot and OpenCode end the turn; OpenCode also declines its other pending
  requests. Codex ends the turn as cancelled. Codex runs commands without a terminal in this
  mode, so input written to an approved command cannot run commands nobody approved, and
  Claude Code is limited to Orka's native tools.
- A brokered tool that already requires an approval at the broker is not asked about again
  at the runtime's prompt.
