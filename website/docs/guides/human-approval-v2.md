---
description: "Review an exact AgentKit or Foundry tool call before Orka executes it."
---

# Human approval for v2 tools

Orka can let an agent look up inventory automatically and require a person to
approve a proposed work order. The controller saves the exact action before
showing it for review. The tool has not run at that point. Approval lets Orka
execute that stored action and return its real result to the waiting agent.

This applies to tools executed by Orka for qualified AgentKit and Foundry
`orka.harness.v2` runtimes. Runtime-native shell and file permissions are a
separate capability. Existing `type: ai` autonomous approvals keep their
worker parking behavior; v2 approvals keep the original tool call open.

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
An unknown outcome means the action may have run. Check the target system
before choosing whether to make a new request.

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
