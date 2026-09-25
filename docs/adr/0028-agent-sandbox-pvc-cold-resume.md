# 28. Agent Sandbox PVC-backed cold suspension and resume

Date: 2026-08-22

## Status

Accepted. Implements issue #422 on the class binding from ADR 0026, mirroring
the data-only suspension contract ADR 0027 established for Substrate. Bounded
retention stays issue #424.

## Context

Agent Sandbox supports a native cold suspension: setting the owned Sandbox's
`operatingMode: Suspended` terminates its Pod while the Sandbox and its owned
PVCs persist, and `Running` creates a fresh Pod. This preserves PVC data —
never process memory or root-filesystem changes — which is exactly the
data-only scope the ACP contract permits. The blueprint-level
`volumeClaimTemplates` and claim-level PVC injection provide the durable
volume; a claim that requests its own PVC deliberately cold-starts instead of
adopting a warm-pool Pod.

## Decision

### Operator-owned policy

`RuntimeWorkspaceProfile.spec.agentSandbox.suspend` permits PVC-backed
suspension with a frozen durable volume shape (`storageClassName`,
`accessModes` defaulting to ReadWriteOnce, and a required `capacity`). The
policy freezes through the class profile hash into the execution snapshot
(including the volume shape, which is part of the binding digest) and surfaces
on the pool as the immutable
`RuntimePool.spec.executionWorkspace.agentSandbox` binding. Suspend stays
admitted only for session-reused DataOnly classes; the frozen binding
re-validates the volume shape offline.

### Rendering

Suspend-capable pools render the derived SandboxTemplate with the reserved
`orka-workspace` mount, `ORKA_ACP_DURABLE_WORKSPACE_DIR` (the same supervisor
durable-workspace mode ADR 0027 introduced), and
`volumeClaimTemplatesPolicy: Allowed`; the pool's SandboxClaim then requests
exactly the frozen durable PVC template, and the claim fence recycles any
claim whose volume shape drifts. The provider injects the per-sandbox PVC
volume into the Pod, so materialization attestation strips exactly that
reserved-name PVC volume when the template does not declare it — every other
unexpected volume still fails the attestation.

### Suspension

The workspace adapter's shared suspension intent scales the pool to zero; the
sandbox backend runs the same authenticated drain barriers as scale-to-zero,
resolves the exact adopted Sandbox from the claim (rejecting anything not
controller-owned by that exact claim), persists a consent record naming the
Sandbox's name and UID, and patches only that Sandbox to
`operatingMode: Suspended`. The pool reports Stopped once the upstream
`Suspended` condition is true and no runtime Pod remains; the claim, Sandbox,
and PVC all persist.

### Cold resume

Lifting the intent rotates the consumed one-time bootstrap material and
refreshes the derived SandboxTemplate in place (a rollout would delete the
claim and its PVC). Because the provider builds the replacement Pod from the
Sandbox spec rather than the template, resume also refreshes the exact
suspended Sandbox's blueprint with the rotated material before returning it to
`Running`. A fresh Pod UID, materialization attestation, bootstrap-instance
binding, and a new signed credential bootstrap gate admission exactly as for a
new claim; the consent record retires once the resumed Pod is observed. A
missing or replaced Sandbox at resume recycles the claim fail-closed instead
of adopting an impostor.

### Cleanup

Deleting the workspace tears down the claim through the existing finalizer;
the provider cascades the Sandbox and its PVC, satisfying the all-Delete
deletion policies that remain the only admitted ones until #424.

## Consequences

- Both supported backends now offer data-only cold suspension under one
  class vocabulary, provider advertisement, and adapter state machine.
- The warm-pool capacity tradeoff is explicit: a suspendable sandbox class
  always cold-starts.
- Sandbox suspension terminates the Pod without checkpointing anything, so a
  provider- or operator-initiated suspension is a liveness event, not a
  credential exposure; the existing instance fences catch replacement Pods.
- Live Task-level suspend/resume conformance remains #425.
