# Memory backends: operation, recovery, and rollout

Remote namespace memory is controlled by `core.orka.ai/v1alpha1 MemoryBackend/default` and the strict `orka.oms.v0alpha1` profile. The feature is default-off and uses an explicit two-gate rollout:

- `--memory-backend-enabled` enables CRD reconciliation, staging, and dispatch support.
- `--memory-remote-activation-enabled` permits the explicit `Active` cutover only
  in a source-gated activation release. The foundation artifact rejects this
  flag and advertises feature epoch 1; the later activation artifact advertises
  epoch 2.
- `--memory-cluster-id` must be a stable, non-secret identity shared by every replica.

The supported SQLite control-plane topology is one controller replica with a
`Recreate` deployment strategy. Activation requires both a live activation-epoch
heartbeat and durable history proving that the lower foundation epoch ran first;
deploying an activation artifact directly cannot satisfy the cutover barrier.

A `Staged` object validates without changing SQLite authority. Once activation commits, deleting or breaking the Kubernetes object does **not** reactivate SQLite. Reads and mutations fail closed until recovery, decommission, force-orphan, or an explicit clean `restore-legacy` action completes.

## Admission boundaries

The installation includes fail-closed `ValidatingAdmissionPolicy` bindings for two controller-owned security boundaries:

- `memory.orka.ai/backend-protection` may be removed only by an identity authorized to update the `memorybackends/finalizers` subresource. Ordinary `MemoryBackend` editors cannot bypass decommission or force-orphan by patching metadata.
- `orka.ai/task` Job/Pod provenance may be established or changed only by controllers authorized to update the owning Task or Job status. Worker authentication verifies the exact controller owner-reference API version, UID, controller bit, and matching Job/Pod task labels.

Do not remove these policies or grant their status/finalizer permissions to untrusted namespace writers. They are part of the memory authorization boundary, not optional hardening.

## Secret binding

The bearer Secret must be in the backend namespace and include:

```yaml
metadata:
  labels:
    memory.orka.ai/client-auth: "true"
  annotations:
    memory.orka.ai/backend-uid: <MemoryBackend UID>
    memory.orka.ai/endpoint: https://canonical.example
    memory.orka.ai/store-name: <spec.store.name>
    memory.orka.ai/namespace-uid: <Namespace UID>
    memory.orka.ai/tenant-id: <controller-derived tenant ID>
data:
  token: <base64 bearer token>
```

Never copy token values into `MemoryBackend`, Tasks, status, logs, audit, or operation resources. Secret UID/resourceVersion, not token content, are recorded in the durable binding. Rotation invalidates readiness until uncached revalidation succeeds.

## Activation checklist

1. Apply the exact target CRD first and wait for `Established`.
2. Roll out the foundation release. The artifact must reject activation even if
   the runtime flag or Helm value is forced.
3. Verify the single serving/dispatching replica has a live foundation feature-epoch heartbeat.
4. Capture a matched controller SQLite/PVC, adapter state, Kubernetes object, and provider backup set.
5. Create `MemoryBackend/default` as `Staged` and wait for fresh store, capability, ownership, endpoint, TLS, and Secret validation.
6. Record the matched pre-activation receipt with
   `orka memory backend checkpoint --manifest-digest sha256:<digest> --reason ... --yes`.
7. Enable the activation gate only after mixed-version and restore rehearsal.
8. Run `orka memory backend activate --reason ... --yes`.

Activation archives the namespace's legacy rows and installs SQLite triggers that reject legacy writes. The archive is not merged with remote content.

## Capacity defaults and hard caps

| Resource | Default | Hard cap |
|---|---:|---:|
| Content | 64 KiB | 256 KiB |
| Tags | 32 | 64 |
| Operation payload | 96 KiB | 512 KiB |
| Unresolved operations/namespace | 1,000 | 10,000 |
| Public page | 100 | 200 |
| Request timeout | 15 s | 60 s |
| Operation maximum age | 7 days | 30 days |

Admission fails before payload allocation when a durable quota is exhausted. Delete and local disable remain the safety path. Monitor queue age, dead letters, materialization latency, validation expiry, identity conflicts, divergence/loss, database size/integrity, and incomplete retrievals.

## Operations

- `ReadOnly`: blocks new create/update/proposal-apply, drains accepted work, then advances the routing fence. Delete and disable remain available.
- `Disabled`: advances the local routing fence, stops dispatch, and suppresses reads/recall. It never selects SQLite.
- `Decommissioning`: rejects new work and waits for unresolved operations to converge.
- `force-orphan`: establishes the local egress barrier, orphans unresolved work, preserves the anti-resurrection fence, and leaves the namespace removed/fail-closed.
- `restore-legacy`: is allowed only from a clean decommissioned binding. Always run `--dry-run` first; it reports archived rows and blockers.

Every consequential operation requires a reason and immutable audit intent. Manual operation abandonment requires both provider proof that the operation was never applied and a durable fence preventing later application.

## Matched backup set

A recoverable set contains:

- an atomic SQLite/PVC snapshot, including WAL/SHM or a quiesced checkpoint;
- `MemoryBackend`, Namespace UID, and referenced Secret identity metadata (Secret values only in encrypted Kubernetes backup);
- provider and adapter checkpoint identities and the stable store UUID;
- cluster ID, authority/routing epochs, and maximum committed operation sequence;
- a checksummed manifest tying the components together.

Successful payloads must not be purged until a verified adapter/provider checkpoint covers their sequence and the recovery window. The initial target is RPO <= 15 minutes and RTO <= 60 minutes.

After the checkpoint is recorded and its recovery window has elapsed, operators
can reclaim bounded local retention capacity explicitly, for example:

```bash
orka memory backend purge \
  --checkpoint-id mcheckpoint-... \
  --before 2026-08-01T08:00:00Z \
  --payloads --expired-idempotency \
  --reason "reclaim checkpoint-covered memory retention" \
  --yes
```

The server resolves the active binding identity and refuses any purge that is
not covered by the exact verified checkpoint watermark and retention gates.

## Restore procedure

1. Restore the matched SQLite, adapter, Kubernetes, and provider set.
2. Start Orka with memory activation disabled.
3. Keep the binding in `Recovering`; reads, recall, admission, and dispatch stay disabled.
4. Verify namespace/backend identity, authority/routing epochs, store UUID, canonical IDs, generations, versions, digests, tombstones, trust, and proposal state.
5. Mark missing/corrupt content `lost` or `diverged`; it remains suppressed.
6. Resume only after reconciliation proves the matched state.

A same-UUID stale snapshot is not trusted. A replacement UUID requires explicit rebind/adoption. Empty or rewound local state must never silently adopt an existing provider store.

## Rollback

Before activation, schema-compatible rollback is allowed. After activation, do not roll back to a binary that can serve legacy SQLite for the namespace. Fence admission/dispatch and use the matched-snapshot recovery procedure. CR deletion is not rollback.
