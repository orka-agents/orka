# 27. Substrate data-only cold suspension and resume

Date: 2026-08-22

## Status

Accepted. Implements the data-only subset of issue #409 on the class-backed
binding from ADR 0026 and the Substrate-backed RuntimePool contract from ADR
0025. Full process-memory restore stays fail-closed and is gated separately
(issue #423); Agent Sandbox cold resume is issue #422; bounded retention is
issue #424.

## Context

ADR 0025 prohibits suspending a live Substrate actor because the provider's
default `Full` snapshot scope checkpoints supervisor process memory — live
pool, capability, and provider-proxy credentials — into snapshot storage.
That prohibition is a consequence of the snapshot policy, not of suspension
itself: Substrate's `Data` scope checkpoints only snapshot-capable
`DurableDir` volumes and its `onResume.fromData: ColdBoot` restores them into
a freshly booted workload with no preserved memory.

Session-reused ACP workspaces need exactly that: repository data that survives
while compute stops, with the logical RuntimeSession restored from Orka's
canonical state and credentials re-issued after boot.

## Decision

### Operator-owned policy

Suspension is permitted per class profile: `RuntimeWorkspaceProfile.spec.
substrate.suspend.mode: DataOnly` is the only admitted mode. The policy is
frozen through the class profile hash, surfaces on the pool as the immutable
`RuntimePool.spec.executionWorkspace.substrate.suspendMode`, and the ACP
provider adapter advertises the generic `suspend` feature only for the
substrate backend. A Task's effective `Suspend` detach action is admitted only
for a session-reused, substrate-backed, DataOnly class; everything else stays
fail-closed, including every legacy provider-shaped request.

### Derived template rendering

A suspend-capable pool's derived ActorTemplate never relies on provider
snapshot defaults. The controller renders, inside the fenced template revision:

- a reserved controller-owned `orka-workspace` `DurableDir` volume (a base
  template defining that name is rejected),
- the single container mount at `/durable/orka-workspace` plus
  `ORKA_ACP_DURABLE_WORKSPACE_DIR`, and
- an explicit `snapshotsConfig` of `onPause: Data`, `onCommit: Data`,
  `onResume.fromData: ColdBoot`.

The supervisor, when that variable is set, hosts each logical session's
repository workspace at `ws-<RuntimeSession UID>` under the durable mount and
keeps the session root, home, temporary files, XDG state, and every
credential on ephemeral storage. One deliberate exception is durable: the
non-secret session identity allocator state (the UID/GID high-water mark,
its configured range, and its lock) lives under
`<durable root>/.session-identity`, because a cold-booted supervisor with a
fresh allocator would otherwise hand a continuation the same UID/GID a
pre-suspension session already used. Data snapshots MUST include this
directory; the supervisor refuses startup when committed checkpoints exist
on the volume without it. Committed durable
content carries a marker recording the repository identity and revision.
Continuity is judged on the stable session-level repository identity (GitHub
identities compare case-insensitively): a cold resume reuses committed
content when the identities match, even when a verified publication has
legitimately advanced the revision. The delta baseline is NOT re-captured
from the preserved tree - it is reconstructed by materializing the
controller-verified repository baseline, so unpublished pre-suspension edits
appear in the next publication instead of silently vanishing. Anything
uncommitted is wiped. Sessions on a resumed workspace lineage carry an
authenticated `expectDurableResume` assertion: creation fails closed when no
committed checkpoint exists, and a committed checkpoint bound to a different
repository identity is never wiped under that assertion (the provider
restored a wrong or stale snapshot). Credentials are never written under the
durable root.

### Suspension

The workspace detach action drives `ExecutionWorkspace.spec.desiredState:
Suspended`; the ACP workspace adapter records the suspension intent on the
linked pool and scales it to zero. The pool backend then runs the same
authenticated drain barriers as scale-to-zero (probe, drain request, observed
quiescence, persisted `Quiescent`), re-proves the deployed template's exact
data-only snapshot policy at the suspension boundary, persists the consensual
checkpoint record, discards the boot record — a supervisor lifetime remains
exactly one boot — and only then calls the provider suspension. Once the actor
settles `Suspended`, the worker-Pod fences clear and the pool reports Stopped.

A provider-initiated suspension of a booted actor still recycles fail-closed,
consent record or not. On data-only pools a snapshot record alone is expected
history and no longer recycles a running actor; on every other pool it remains
proof of foreign suspension.

### Cold resume

A continuation Task returns the workspace to `Ready`; the adapter lifts the
pool intent. The backend rotates the consumed one-time bootstrap material
(exactly as for a replacement actor), refreshes the derived template in place
with the rotated public nonce and verification key — the suspended actor has
no live process to observe the transition, and recycling it would destroy the
data snapshot that is the point of the suspension — re-fences the template,
and resumes the actor from its data snapshot (`ResumeActor` without
boot-from-scratch). The workload cold-boots under the refreshed template,
re-proves worker placement with a fresh Pod fence, completes a new signed
credential bootstrap, and the consensual checkpoint record retires so a
replacement actor can never resume from stale data. The logical RuntimeSession
continues from Orka's canonical transcripts with a new boot ID and
RuntimeInstanceID.

### Cleanup and retention

Deleting a suspended workspace tears the pool down through the existing
staged, credential-safe teardown; `DeleteActor` removes the actor and its
snapshots with the durable volume. The idle-pool reaper never retires a
suspended workspace. A class `maxLifetime` IS enforced today: the settled
suspension self-schedules its deadline and, once the frozen lifetime elapses,
the workspace fails terminally and its linked pool (with the checkpoint) is
deleted — a suspended workspace therefore persists until explicit deletion
only when its class sets no maximum lifetime. Idle-timeout retention and
suspension quotas beyond that remain issue #424.

## Consequences

- Session continuity across detach is real for substrate classes: repository
  data survives suspension while process memory and credentials never do.
- The one-live-writer rule is preserved: one pool, one actor, one consensual
  checkpoint record consumed by exactly one resume.
- Template drift while suspended still fails closed (recycle destroys the
  checkpoint rather than resuming under unverified infrastructure).
- Until #424, suspended workspaces without a class `maxLifetime` persist until
  explicitly deleted.
- Live Task-level conformance for suspend/resume is tracked by #425; the
  provider-side contract here is covered by focused controller and supervisor
  tests.
