# 29. Bounded retention for ACP execution workspaces

Date: 2026-08-22

## Status

Accepted. Implements the ACP-scoped subset of issue #424 on the class binding
(ADR 0026) and the cold-suspension contracts (ADRs 0027/0028).

## Context

Suspended workspaces retain provider compute objects, durable volumes, and
snapshots. Without expiry and quota they leak: the idle-pool reaper
deliberately skips suspended workspaces, and the generic class lifecycle's
`idleTimeout` and `maxLifetime` were recorded but never enforced.

## Decision

- A dedicated retention reconciler enforces the frozen class lifecycle on
  every class-backed ACP workspace: `maxLifetime` is a hard upper bound that
  forces terminal deletion regardless of state — even attached, where the
  workspace finalizer still runs the authenticated drain and adapter teardown
  — and `idleTimeout` bounds unattached time from the recorded last-detach
  instant (or creation for a never-attached workspace). An idle suspended
  workspace is deleted (its retention is exhausted); an idle Ready workspace
  follows the class default: Suspend where the frozen policy permitted it,
  otherwise the Delete disposition.
- `RuntimeWorkspaceProfile.spec.retention.maxSuspendedWorkspaces` caps
  concurrently suspended workspaces per class and namespace. The cap freezes
  through the profile hash, the execution snapshot, the binding digest, and a
  workspace annotation. Admission rejects a Task whose prospective Suspend
  action would exceed the cap. Settlement and idle retention re-check the live
  count and leave the frozen Suspend action pending when a concurrent
  suspension exhausted it; a queued continuation can take a still-Ready
  workspace directly, and `maxLifetime` remains the independent hard bound.
  A suspend-capable class must configure this cap, `idleTimeout`, or
  `maxLifetime`; a class with no retention bound is rejected by readiness and
  Task binding.
- Retention actions are observable through Events on the workspace and the
  bounded `orka_acp_workspace_retention_actions_total{action,reason}` metric;
  no object names, class names, or session identifiers enter metric labels.
- Orphan cleanup stays UID-fenced: expiry deletions use UID preconditions,
  the workspace adapter refuses foreign same-name pools, and a stopped pool
  whose linked workspace vanished is reaped through the existing
  idle-reap fall-through.

## Consequences

- Suspended workspaces are bounded by class policy in three independent ways:
  count (quota), idle age, and absolute age.
- Deeper retention dispositions (retaining volumes or checkpoints past
  workspace deletion) remain rejected at admission until a reviewed
  retention-disposition design exists; the deletion policy stays all-Delete.
- Task-level conformance for retention behavior belongs to #425.
