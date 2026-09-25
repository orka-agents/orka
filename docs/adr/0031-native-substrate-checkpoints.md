# 31. Use native Substrate Tags for cold checkpoints

Date: 2026-09-06

## Status

Accepted. Supersedes the fork-specific DataOnly ActorSnapshot and atomic resume
requirements in ADRs 0027 and 0030 for the native cold path. The full-memory
restore gate in ADR 0030 stays closed.

## Context

The official provider uses native Actor, ActorTemplate, Worker, Atespace, and
Tag resources. Tags copy suspended Actor snapshots and survive source deletion.
They require the original immutable template UID when creating a new Actor.
Suspended UpdateActor supports UID/version CAS and compatible template changes.
SuspendActor, ResumeActor, and DeleteActor have no caller identity preconditions.
A client must not manufacture provider fences from preflight reads.

## Decision

Pin the provider and untouched protocol together. Use authenticated TLS with
rotating client identity. Keep Orka ownership, immutable creation intents,
selected Tags, and recovery failures in resourceVersion-protected controller
ConfigMaps. That CAS serializes Orka's journal; it is not native operation CAS.
Provider control operators and the router are trusted, and must not mutate
Orka-owned resources concurrently. Upstream authorization is not implemented.

Before provider operations, persist a pool marker requiring its lifecycle
journal. Missing journals close admission and block cleanup, including when a
legacy pool has lifecycle evidence but no native journal. Release the marker
only after provider cleanup, before deleting the journal during finalization.

Use Data/Data/ColdBoot templates and a fresh Actor for continuation. Drain the
supervisor and exact single-Actor worker before snapshot capture. Verify Tag
source UID/version, template UID, and Data scope. Delete and observe the exact
worker Pod before deleting the source Actor and reporting suspension. A snapshot
alone is insufficient termination evidence. Full process memory is never restored.

Create a restore Actor using the Tag's original template, then CAS-update the
suspended Actor to a compatible newly compiled template. Rotate credentials,
verify the process-local sealed bootstrap challenge and actual Actor identity,
and authenticate a new supervisor boot before admission. Preserve uncertain
operations and the last checkpoint; never replay possibly accepted work.

Public ExecutionWorkspaceCheckpoint resources retain data independently of a
source workspace. Require source `use` authorization for export and checkpoint
`use` authorization for consumption. Persist the selected digest before pinning
it; acquire references before releasing a source, and close acquisition with a
persistent Deleting marker before native garbage collection. Restore into a
fresh workspace under the exact namespace, class/provider revisions, runtime,
and durable layout. Explicit recovery accepts the last verified checkpoint
without changing the source Task's outcome.

## Consequences

Session identity survives compute deletion. Checkpoint forks share immutable
source data references and have independent mutable runtimes. Unused template
revisions are collected without removing revisions referenced by journals or
checkpoints. No private runtime credentials enter native templates or durable
workspace volumes.

This does not add atomic lifecycle fences or RBAC to Substrate. Native worker
Pods and their autoscaling remain operator-managed. Existing legacy suspended
pools are not converted by inferring proof from mutable native observations.
Native TLS tests and an unmodified provider conformance suite replace the
former provider-patch tests.
