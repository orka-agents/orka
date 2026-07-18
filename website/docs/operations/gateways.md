# Operating generic gateways

Generic gateways are enabled by default. They use the controller SQLite database for normalized ingress events, Session transcript provenance, and outbound delivery state.

## Default service levels and bounds

The documented local reference target is p95 durable ingress acknowledgement below 500 ms for a 1,000-event burst with 100 active Sessions. This is an admission SLO, not an end-to-end model response SLO.

Defaults:

| Setting | Default |
| --- | --- |
| Pending events per Session | 100 |
| Retained operational event records per Gateway | 1,000 |
| Retained rejected audit records per Gateway | 250 |
| Event/delivery expiry | 24h |
| Delivery timeout | 15s |
| Delivery attempts | 10 |
| Terminal retention | 720h (30 days) |
| Claim lease | 1m |
| Default persistent volume request | 1Gi |

Controller flags use the `--gateway-*` prefix. Helm values are under `controller.gateway`. Gateway dispatch, delivery, and retention maintenance run only on the elected controller leader; SQLite claims preserve crash recovery while leader election makes namespace task-capacity checks replica-safe.
Gateway ingress is enabled by default, so the Helm chart also enables the SQLite PVC by default. Helm rejects `controller.gateway.enabled=true` with `store.persistence.enabled=false` unless `controller.gateway.allowEphemeralStore=true` is explicitly set for disposable development; acknowledged events on an ephemeral store are not durable across Pod replacement.
For offline GitOps rendering, apply the bundled Task/Gateway CRDs separately and set `controller.gateway.crdsReadyOverride: true`; live Helm installs normally discover the CRDs with `lookup`.

## Readiness

A Gateway is Ready only when:

- its GatewayClass is accepted;
- exactly one safe HTTPS endpoint or TLS-authenticated selector-backed same-namespace Service resolves;
- separate inbound/outbound Secrets exist, opt in, and bind to the Gateway;
- the outbound Secret binds to the exact resolved HTTPS endpoint;
- authenticated health and capability probes succeed;
- observed capabilities satisfy the GatewayClass requirements.

Agent runtime warmth does not affect Gateway readiness. Accepted events remain durable while downstream execution is temporarily unavailable.

## Task and Session access

Gateway-created Tasks remain ordinary Orka Task objects for controller execution, but their CRs contain no external message text; the prompt is loaded from the bounded, task-owned Session transcript. Public Task list/get/log/result/event/trace/fork surfaces require both the ordinary Task permission and gateway-read authorization for the owning Gateway. Destructive Task actions and approval decisions additionally require gateway-operate authorization. The default Helm and Kustomize installs also create a fail-closed `ValidatingAdmissionPolicy` that permits direct Kubernetes create/update/delete of gateway-owned Tasks only from the owning Orka controller or trusted worker ServiceAccounts. Namespace-isolated Helm releases scope the policy by the immutable Gateway namespace encoded in `requestedBy.issuer`, so multiple releases do not deny one another and coordinated workers can create inherited child Tasks. Canonical gateway Sessions are hidden from generic Session, Session-event, transcript-search, and chat-loading surfaces; gateway event/delivery APIs are the supported operator view.

## Secret rotation

Create or update Secret data without changing the configured key. Kubernetes Secret watch events trigger reconciliation; status records only the observed resourceVersion. Never put token values in Gateway metadata, status, logs, Tasks, or support bundles.

When an endpoint changes, update the outbound Secret's `gateway.orka.ai/adapter-endpoint` annotation to the exact new resolved HTTPS endpoint. For `serviceRef`, the adapter certificate must be trusted by the controller and valid for `<service>.<namespace>.svc`. The Gateway remains not ready until the binding matches and the authenticated probe succeeds.

## Dead letters and recovery

Inspect event and delivery state in the dashboard or with:

```bash
orka gateway events list --state DeadLettered,Expired
orka gateway deliveries list --state DeadLettered,Failed,Expired
orka gateway deliveries retry <delivery-id>
```

Manual retry preserves the stable delivery/idempotency ID, resets the bounded attempt window, extends expiry by 24 hours, and increments `manualRetryCount`. Expired ingress events are not automatically replayed because their original sender/context authorization may no longer be valid.

## Backup and restore

Gateway Sessions, normalized events, delivery rows, and event-to-Task correlation are in the controller SQLite database. Gateway CRDs, referenced Secrets, and Task CRs are Kubernetes objects and are **not** in that file. A disaster-recovery backup therefore needs both the SQLite volume and the corresponding cluster objects; keep credentials in the cluster Secret backup, never in the SQLite archive.

SQLite runs in WAL mode. Do not copy only `orka.db` while the controller is writing because committed records may still be in `orka.db-wal`. Use one of these consistency-safe approaches:

- take an atomic CSI `VolumeSnapshot` of the whole persistent volume, including the database, WAL, and shared-memory files; quiescing writes first is still preferred;
- use SQLite's online backup API against the live database; or
- pause adapter ingress, stop every controller replica that can write the store, checkpoint/truncate the WAL from a maintenance process, and then copy the main database file.

For a release or disaster-recovery backup:

1. Record the Orka image/chart version, Gateway CRD manifests, controller gateway flags, and adapter versions/capabilities.
2. Pause external ingress and wait for in-flight API requests to finish. Record queued, sending, retry-scheduled, and dead-letter counts.
3. Create a consistent SQLite/PVC snapshot and a cluster backup containing GatewayClasses, Gateways, GatewayBindings, Tasks, referenced Agents, and referenced Secrets.
4. Verify the SQLite copy with `PRAGMA integrity_check` in an isolated location and keep the backup immutable.
5. Resume ingress only after the snapshot and cluster-object backup both succeed.

To restore, stop gateway writers, restore the SQLite files and matching Kubernetes objects, then start one controller replica first. Confirm CRDs are Established, migrations complete, Gateways return Ready, terminal deliveries remain terminal, and queued events/due deliveries become claimable before restoring normal replica count and ingress. Claims that were active at backup time become eligible only after their recorded lease expires.

Deterministic Task and delivery IDs prevent restored work from receiving new identities. They cannot prove whether a provider accepted a request immediately before the snapshot, so a conforming adapter must deduplicate any replay by the original delivery/idempotency ID. Never "repair" a restore by deleting or regenerating delivery IDs.

## Upgrade compatibility and version skew

The adapter wire contract is exact-versioned. The current controller accepts only `orka.gateway.v1`; `adapterVersion` is informational and capabilities are readiness inputs, not a version-negotiation mechanism. Unknown JSON fields are rejected. There is no implied N-1 or N+1 adapter compatibility: a controller and adapter may be rolled independently only while both continue to speak exactly `orka.gateway.v1` and the adapter still advertises every capability required by its GatewayClass.

Use this rollout order:

1. Take the consistency backup above and export the currently installed Gateway CRDs.
2. Apply the target release's CRDs first and wait for each CRD to become Established. Do not remove the currently served/storage version during a rolling upgrade.
3. Roll the controller and verify store migration, API health, Gateway readiness, queue depth, and dead-letter rate.
4. Run the gateway conformance CLI against each target adapter build, then roll adapters one at a time.
5. Confirm observed adapter name/version/capabilities and perform one idempotent test delivery before completing the rollout.

If a future release introduces another wire version, controller and adapter release notes must define an explicit dual-version overlap. Do not infer compatibility from similar payloads or from the adapter's product version.

## SQLite schema migration and rollback

The controller runs idempotent SQLite migrations during database open, before serving gateway work. Migration success is a startup gate. A forward migration does not imply that an older controller can safely use the resulting database; some repository migrations may rebuild tables or indexes even when the gateway tables themselves are unchanged.

Before upgrading, rehearse startup against a copy of production data and compare Session/event/delivery counts, task references, terminal states, and `PRAGMA integrity_check` before and after migration. Keep the pre-upgrade snapshot until the new release has processed queued events and deliveries successfully.

A binary-only rollback is acceptable only when the release notes explicitly state that no incompatible SQLite or CRD migration occurred. Otherwise:

1. Pause ingress and stop all controller writers.
2. Restore the pre-upgrade SQLite/PVC snapshot.
3. Restore the matching prior Gateway CRDs and Kubernetes objects without deleting stable object UIDs out from under retained ledger rows.
4. Deploy the prior controller and adapter versions.
5. Validate readiness and ledger counts before resuming ingress.

Do not run an older controller against a forward-migrated production database merely because it starts. Do not use a down migration on the live database unless that exact path is shipped and documented by the release.

## Cleanup

The maintenance loop marks pending work expired at its deadline, releases Session reservations, removes terminal deliveries older than retention, and compacts eligible terminal events into small deduplication tombstones before deleting their full ledger rows. It then prunes the corresponding gateway transcript messages, reclaims empty gateway Sessions, and deletes orphaned gateway Tasks so their controller finalizers can remove results, artifacts, plans, and execution history. Tombstones expire after one additional retention window, so duplicate suppression remains bounded without retaining full message text forever. Increase retention when audit requirements exceed 30 days and size the persistent volume for the retained event, delivery, Session, Task-result, and tombstone windows.
