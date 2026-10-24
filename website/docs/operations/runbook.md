---
description: "What to do when an Orka installation looks stuck, and why each remedy works."
---

# Operations runbook

This page is for the person keeping an Orka installation healthy. It explains
how the moving parts behave during failures, upgrades, and everyday operation —
and what to do when something looks stuck. Each section starts with the concept,
because the right remedy depends on understanding what Orka is protecting.

If you only have a symptom, start with the
[quick reference](#quick-reference) at the bottom.

## One active controller, by design

Run Orka with one controller replica. Its reconciliation and storage model has
two relevant constraints:

- **Leader election.** Controller replicas coordinate through a Kubernetes
  Lease, and only the leader reconciles.
- **A local state store.** The controller keeps durable state (execution
  snapshots, monitor inventory, scan history) on a PersistentVolumeClaim that
  most storage classes provision as `ReadWriteOnce`. That permits read-write
  mounts on one node, including multiple Pods on that node. The Deployment uses
  `Recreate` to stop the old Pod before starting its replacement during an upgrade.

:::danger[Do not scale the controller above one replica]
`ReadWriteOnce` is not a single-Pod lock. A second replica on the same node can open
the same SQLite file; a replica on another node may remain `Pending` with a `Multi-Attach`
warning. Keep one replica. Leader election coordinates reconciliation but does not make
the local store safe for multiple controller processes.
:::

What this means in practice:

- **Failover is node replacement, not a hot standby.** If the controller's node
  dies, expect a short outage (a few minutes) while Kubernetes reschedules the
  Pod and reattaches the volume. In-flight work is not lost: running agent
  prompts settle from durable records once the controller returns (see below).
- **A brief API gap during upgrades is normal.** `Recreate` stops the old Pod
  before starting the new one.

## What happens to running work during a restart or deploy

Agent tasks execute inside runtime Pods (RuntimePools, below), not inside the
controller — so a controller restart does not kill running prompts. The
guarantee Orka maintains is **a prompt that was accepted by a runtime is never
sent twice**. Re-sending could repeat side effects (a duplicated commit, a
double-posted comment), so Orka prefers honesty over silent retries:

- If the controller restarts while a prompt is running, it re-attaches and the
  task continues or settles from the durable attempt record. A task status
  message such as *"settled from the durable attempt record after the live
  settlement was interrupted"* is this mechanism working as intended — not a
  bug.
- If a runtime Pod is replaced mid-prompt, the attempt fails with an explicit
  message rather than replaying the prompt. Retry policy (or the user) decides
  what happens next.

Give `make deploy`-driven rollouts time to drain: pools finish accepted work
before replacement runtime Pods take over.

## RuntimePool recovery

A **RuntimePool** is a controller-owned pool of runtime Pods for one agent
profile (runtime type + image digest + configuration). Pools scale to zero when
idle and admit new sessions only while their status shows `Serving` and
`Accepting`.

Pools are cattle: the controller recreates any pool a task needs. That makes
deletion the safe, general remedy when a pool wedges.

| Symptom | What it means | Remedy |
|---|---|---|
| Pool status shows `RolloutTimedOut` or `RolloutFailed` and never recovers | The pool latched a failed rollout (for example, an image that could not be pulled, or an interrupted deploy) and will not retry on its own | Fix the underlying cause if there is one (image digest, registry access), then **delete the RuntimePool**; the controller recreates it on demand |
| A deleted or superseded pool keeps coming back with an old image digest | A queued or running Task still references the old profile, and the controller faithfully rebuilds the pool that task asked for | Find and delete (or cancel) the stale Task first, then delete the pool |
| Tasks queue forever with no runtime Pod appearing | The pool exists but is not `Serving`/`Accepting` | `kubectl describe` the pool for its condition message, then apply the row above that matches |

Two habits prevent most pool trouble:

1. **Deploy with pinned digests** (the `make deploy IMG=…@sha256:…` form).
   Tag-based images make "which pool is current" ambiguous.
2. **After a deploy, check for leftovers**: pools whose digest does not match
   the new deployment are candidates for cleanup once their tasks finish.

## Session cleanup evidence

Session deletion removes transcript state after authoritative cleanup completes.
SQLite retains one terminal-projection receipt per finalized turn so Task
finalizer retries, archived recovery, and restored Tasks can verify the original
outcome. These receipts contain execution identity, terminal status, and
diagnostics; they exclude the user prompt and terminal content.

There is currently no automatic garbage collection or total row-count bound for
these receipts. Monitor SQLite volume usage for workloads that frequently create
and delete Sessions. The normal terminal payload limit bounds each input, not the
total retained history. Deleting receipts during PromptAttempt reclamation would
break supported cleanup retries. Safe reclamation needs an explicit restoration
retention policy and proof that no original or restored Task can still consume
the receipt. Keep Session cleanup completion tombstones as well; they prevent
reuse of deleted Session identities.

## Storage recovery holds

Orka records, in the `ControllerEpoch` object's status, the controller epoch and
the latest write watermark each storage dataset acknowledged. About once a
minute the controller advances each dataset's watermark and records the new
value. At every takeover, and whenever a dataset reports that its database
restarted or switched, it compares the dataset with that record.

If a dataset holds less than it acknowledged, it lost acknowledged writes or
was restored from an older copy. A restored copy keeps its dataset identity, so
the identity alone never lets work resume. Orka then places a storage recovery
hold on the whole installation. It also holds when a storage contract is served
by a different dataset than before, and when a dataset whose durability class is
`async-replicated` restarts or fails over. While held:

- every storage write and control-store change is refused as temporarily
  unavailable, so API calls that write storage answer 503; no Task Job is
  created, no agent prompt is dispatched and no Task reports success;
- reads and status reporting keep working;
- the hold is recorded in the `ControllerEpoch` status, so a controller
  restart keeps it.

Inspect it with:

```bash
kubectl -n <control-namespace> get controllerepochs
kubectl -n <control-namespace> get controllerepoch <name> -o jsonpath='{.status.storageRecovery}'
```

`status.storageRecovery` names the hold's `id`, its `reason`
(`EpochRegressed`, `WatermarkRegressed`, `DatasetChanged` or `UnsafeSwitch`),
the contract and dataset, and the acknowledged and stored values. The
`StorageRecoveryRequired` condition is `True` while the hold is in place, and
the `orka_storage_recovery_hold` metric is 1 with the hold's reason.

Do not delete the hold from the status. Recover or accept the storage state
first: restore the newest copy, or decide that the writes it lacks are gone.
Then release that exact hold by its ID:

```bash
kubectl -n <control-namespace> annotate controllerepoch <name> \
  storage.orka.ai/recovery-acknowledged=<hold-id> --overwrite
```

The controller then advances each dataset again, records what each one holds
now as acknowledged, clears the hold, and resumes work. An annotation that
names any other hold releases nothing.

## Security scan operations

Repository security scanning runs **one scan at a time per repository** — a
new run is refused while another is `pending` or `running`. Two consequences:

- A `409 already running` from `orka security scan run` immediately after a
  scan finishes is a short finalization window, not a wedged scan. Retry after
  a few seconds.
- **Never delete a scan's worker or retry Tasks while the scan is active.**
  The scan run is waiting on those exact task results; deleting one fails the
  run with *"terminal task result was not found"*. If a retry task is truly
  wedged, let the scan fail (or cancel it) first, then clean up.

## Quick reference

| Symptom | First check | Section |
|---|---|---|
| Controller Pod `Pending` with `Multi-Attach error` | `kubectl get deploy` replica count — scale back to 1 | [One active controller](#one-active-controller-by-design) |
| Task message mentions "settled from the durable attempt record" | Nothing — informational; check the task's final phase | [Running work during a restart](#what-happens-to-running-work-during-a-restart-or-deploy) |
| Pool stuck `RolloutTimedOut` / `RolloutFailed` | Pool conditions, then delete the pool | [RuntimePool recovery](#runtimepool-recovery) |
| Old-digest pool resurrects after deletion | Queued Tasks referencing the old profile | [RuntimePool recovery](#runtimepool-recovery) |
| `409 already running` starting a scan | Wait a few seconds and retry | [Security scan operations](#security-scan-operations) |
| Scan failed with "terminal task result was not found" | Was one of its Tasks deleted mid-run? | [Security scan operations](#security-scan-operations) |
| Writes answer 503 and no Task starts; `StorageRecoveryRequired=True` on the `ControllerEpoch` | `status.storageRecovery` on the `ControllerEpoch` | [Storage recovery holds](#storage-recovery-holds) |
