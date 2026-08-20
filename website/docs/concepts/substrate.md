---
slug: /substrate
---

# Agent Substrate Workspaces

Agent Substrate is an externally installed and operated execution-workspace
provider. Orka can host a built-in agent Task's ACP RuntimeSession inside a
gVisor-isolated Substrate Actor through a **workspace-provider-backed
RuntimePool** (Phase 2 of the seam established for
[Agent Sandbox](agent-sandbox.md)). The integration is disabled by default and
fails closed; there is never a fallback to the removed worker-based path or to
the agent-sandbox backend.

`Task.spec.workspace` remains the only repository surface:

```yaml
spec:
  type: agent
  workspace:
    intent: write
    gitRepo: https://github.com/example/project.git
    readCredentialRef:
      name: project-read
    publicationGitRepo: https://github.com/example/project.git
    publicationCredentialRef:
      name: project-publish
    pushBranch: orka/example-change
```

The separate Workspace/Publisher performs clone, deterministic commit
preparation, exact-ref push, independent verification, and optional PR
reconciliation. The Actor never receives publication credentials.

`Task.spec.execution.workspace` requests the Substrate backend. Unlike
agent-sandbox, `templateRef` is **required**: it names the operator-owned
*infrastructure* ActorTemplate whose placement fields (workerPoolRef, runsc
build, snapshot location) seed the controller-rendered runtime template.

```yaml
spec:
  type: agent
  execution:
    workspace:
      enabled: true
      provider: substrate
      templateRef:
        namespace: ate-demo
        name: orka-codex-infra
```

## Execution model

```text
Task
  -> workspace binding (provider, policies, session key, infrastructure
     template) frozen into the immutable execution snapshot
  -> dedicated single-session RuntimePool (acp-ws-<runtime>-<hash>)
  -> derived, controller-owned ActorTemplate: operator infrastructure fields
     (including the provider-required snapshotsConfig) + the immutable ACP
     runtime container with fence env literals and NO credential material —
     only the public per-pool bootstrap nonce
  -> one Actor, booted from scratch exactly once (ResumeActor boot=true)
  -> post-boot credential bootstrap: the supervisor boots credential-free into
     an awaiting phase; the controller seeds the pool tokens over the router
     with a one-time, nonce-gated, idempotent PUT (a payload conflict recycles
     the exact instance)
  -> authenticated exact-instance fence probe through the router
     (Host: <actorID>.<actorDNSSuffix>) selects the ActiveInstance
  -> ephemeral RuntimeSession, fenced prompts, workspace validation,
     optional clean-room Workspace/Publisher transaction — all unchanged
```

Orka remains authoritative for Task attempt state, `OutcomeUnknown`
classification, epochs/fences/request digests, prompt leases, permissions,
cancellation, canonical transcripts, workspace deltas, publication, and
delivery receipts. Substrate supplies physical placement and gVisor isolation.

## Suspension is prohibited

gVisor suspension checkpoints Actor **process memory** into provider snapshot
storage. A running supervisor holds the pool controller token, the capability
secret, and the live provider-proxy bearer in memory, so suspending a booted
ACP actor would write live credentials into a snapshot — which the
execution-workspace contract forbids. Therefore:

- a live supervisor is never suspended; because the provider deletes only
  suspended actors, teardown first destroys the workload's memory by deleting
  its single-workload worker Pod, then settles the memoryless actor (a
  suspension with nothing left to checkpoint) and deletes it;
- the derived template copies the operator's `snapshotsConfig` (the provider
  requires it and golden-snapshots every template by booting and checkpointing
  one instance) — this is safe only because the rendered container is
  credential-free: the golden build captures a waiting, unseeded supervisor
  plus a public nonce, and credentials are seeded only into the pool's real,
  booted actor;
- a provider-initiated suspension or snapshot of a booted actor fails closed:
  admission closes, the actor is recycled, and the replacement boots fresh;
- **operators must disable provider-side idle suspension for ACP actor
  templates**;
- Task options that imply warm workspaces (`boot`, `poolRef`, `snapshot`,
  `hibernation`, `onDetach`, `cleanupPolicy: retain`) are rejected before any
  workspace or RuntimePool demand exists.

Warm suspend/resume becomes viable only with provider-side credential-safe
sessions (for example short-lived certificates via `SessionIdentity.MintCert`,
which the provider API defines but does not support yet). Until then, "resume"
is recreation: a fresh actor and boot, with logical session continuity through
the existing RuntimeSession generation-increment recreation path. See
`docs/adr/0025-substrate-backed-runtime-pools.md` for the full analysis.

## Enablement and operator requirements

Dispatch requires `--substrate-enabled` (with valid `--substrate-api-*`,
`--substrate-router-url`, and `--substrate-actor-dns-suffix` configuration)
plus `--acp-workspace-dispatch-enabled`. Either missing fails closed with the
reason projected to `Task.status.executionWorkspace`.

- The infrastructure ActorTemplate must exist in the referenced namespace; its
  containers are never executed by ACP pools.
- The controller needs an operator-provided RoleBinding in the template
  namespace granting `ate.dev` ActorTemplate CRUD (the derived template is
  created there). No Secret access is needed: pool credentials stay in the
  controller's runtime namespace and are seeded post-boot over the nonce-gated
  credential bootstrap endpoint.
- The controller also needs Pod `get`/`list`/`delete` in the provider worker
  namespace for the credential-safe teardown of live actors.
- The Substrate control plane and router share the cluster with Orka;
  cross-cluster topologies are unsupported and fail closed.
- The Actor's egress must reach the Orka controller API and the provider
  proxy (Vekil).
- Pool finalization deletes the actor through the control API before the pool
  is released; an unreachable control plane blocks pool deletion rather than
  leaking a credentialed workload.

Task status stays provider-neutral: provider, phase, reason, and policies.
Route hosts, worker names, snapshot URIs, and every other provider-assigned
identifier never enter public Task status. The controller-derived actor ID
appears only inside `status.execution.runtimeInstanceID` — Orka's own
exact-instance fencing identity, deterministically derived from the pool name
that status already carries, exactly as Pod-backed pools expose
`pod:<uid>.<bootID>`.
