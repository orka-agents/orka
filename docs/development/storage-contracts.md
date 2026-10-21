# Storage ownership and bootstrap

This inventory describes the production wiring in `cmd/main.go`. It prepares the
boundary for [#512](https://github.com/orka-agents/orka/issues/512); it does not
introduce a provider API. SQLite remains the only application database and
artifact store, with one active controller.

## Dataset metadata

`store_dataset` contains exactly one canonical, nonzero UUID, an origin (`new` or
`adopted`), and a bootstrap-completion flag. `PRAGMA user_version = 1` identifies
this metadata layout. Opening a database still validates every table and index
definition: a version marker alone cannot establish compatibility.

Fresh schema creation, identity creation and version recording commit together.
Exact supported historical layouts are backed up and upgraded under the existing
process lock and one `BEGIN IMMEDIATE` transaction. The final 167-to-168 transition
adds the metadata table and records an adopted identity. Missing or malformed
metadata in an established versioned dataset is an error, never a repair request.
Startup synchronizes a full WAL checkpoint after schema/identity initialization
and after bootstrap completion, before key decisions or consumer access. A busy
or failed checkpoint fails startup; ordinary writes retain their existing
`synchronous=NORMAL` setting.
The frozen historical fingerprints, migration SQL and unsupported #700 layout
remain unchanged. See [the upgrade procedure](../../website/docs/operations/upgrading.md).

Production startup calls `openControllerSQLiteStore`, which holds the process
lock before opening/upgrading SQLite or loading the snapshot key. A missing
managed Secret key may be generated only when the durable origin is `new`,
bootstrap is unfinished, and every application table is empty. An adopted
database, completed bootstrap, or any retained application/artifact/control row
requires an existing key. Operator key-file mode always requires a supplied key.

Key activation authenticates retained execution snapshots, native sessions and
connector custody before recording bootstrap completion and exposing the store
to consumers. Failure closes SQLite and releases the lock. An interrupted fresh
start can resume with the same identity; an API-committed Secret key is reused
after a lost response or an update conflict. Clearing ordinary records does not
reopen permission to mint a key after completion. Existing key-rotation rules
remain, including authentication of protected retained ciphertext and the
existing expiry handling for connector consents.

This is dataset-local history. There is no independent installation binding in
this contribution: replacing the whole database also replaces its history. A
UUID does not detect rollback or establish that a replacement empty database
belongs to an installation. Backup/restore and that future binding require
additional protocols.

## Application persistence

The proposed `state.v1` ownership below means all database-owned application
state other than public artifacts. `artifacts.v1` owns artifact bytes and their
metadata together. These are planning boundaries, not enabled configuration or
versioned interfaces. The current SQLite database contains both. Kubernetes
authority remains separate from either boundary.

| Production interfaces | Records and ownership | Main consumers and constraints |
| --- | --- | --- |
| `ResultStore`, `PromptResultReceiptStore` | Results and immutable attempt-bound result receipts; state. | Task reconciler, ACP dispatch/recovery, worker API and result API. A result receipt precedes prompt settling. |
| `SessionStore`, `SessionTurnCommitter`, `FencedSessionWriteStore` | Sessions, transcript messages, token counters and Task/chat ownership locks; state. | Session manager, chat, compatibility APIs and transcript tools. Turn commit groups messages with usage; fenced writes require the exact owner. `GetSessionType` is also consumed through a narrow local reader. |
| `AgentExecutionSnapshotStore`, `AgentExecutionSnapshotLifecycleStore` | Encrypted immutable snapshots keyed by Task UID/digest, metadata and retention operations; state. | Task admission/dispatch, internal APIs and reference-aware retention. Kubernetes Task bindings are separate. |
| `SessionLineageStore` | Payload projection of Kubernetes-authoritative Session lineage; state. | Session continuity and dispatch. Projection failure may block dispatch but never changes Kubernetes lineage authority. |
| `NativeSessionStore`, `NativeSessionImportStore` | Encrypted private native session bytes, import/capture receipts, finalization receipts and deleted-name tombstones; state. | ACP continuity/finalization and migration APIs. Native state is not a public Task artifact; reads validate canonical Session UID and transcript boundary. |
| `ConnectorCredentialStore`, `ConnectorConsentStore` | Sealed credentials/consents, grants, retired custody, completion receipts, versions, sequences and disconnect tombstones; state. | Connector APIs, credential source and linked-account tool execution. Kubernetes Connection resources carry authority/metadata; secret custody stays in the database and is authenticated by the snapshot key. |
| `SessionTurnPersistenceStore` | Turn records, transcript additions and deferred terminal outbox projections; state. | Kubernetes composite control store. `CommitSessionTurnFinalization` groups these records (and applicable native capture) atomically; projection activates after Kubernetes finalization. |
| `OutboxPersistenceStore` | Projection payloads, claims and completion receipts; state. | Kubernetes composite control store and projection reconciler. Database persistence does not choose controller ownership. |
| `SessionCleanupPersistenceStore`, `SessionTurnCleanupReceiptStore` | Cleanup identity, durable intents, completion tombstones and retained turn receipts; state. | Kubernetes composite and cleanup recovery. These close crash windows across Kubernetes deletion and transcript deletion. |
| `GatewayEventStore`, `GatewayDeliveryStore` | Ingress deduplication/claims, canonical transcript projection, deliveries, budgets, retries and event tombstones; state. | Gateway service, adapters, task reconciliation and conversation reply tools. Existing grouped admission/projection and delivery idempotency must survive a future adapter. |
| `GatewayTaskCleanupReceiptStore`, `GatewayTaskCleanupReceiptMaintenanceStore`, `GatewaySessionCleanupCandidateStore` | Compaction ownership evidence and retained transcript candidates; state. | Gateway retention/recovery. Live uncached Kubernetes absence checks precede receipt deletion. |
| `MemoryStore`, `MemoryProposalStore` | Durable namespace memories and governance proposals/review/application history; state. | Memory APIs and worker tools. Proposal review does not apply a memory. Redaction and authorization remain Orka responsibilities. |
| `PlanStore`, `ExecutionEventStore`, `DeduplicatingExecutionEventStore`, `AtomicExecutionEventPlanStore` | Plan state, ordered event streams, sequence projections and deduplication; state. | Task reconciliation, runtime event capture, SSE/API and worker tools. Plan-aware append groups event and plan update. Existing event retention is preserved. |
| `MessageStore` | Inter-agent messages and read state; state. | Worker APIs, task message tools and Task cleanup. Consume-and-mark-read behavior matters for future retry semantics. |
| `SecurityStore` | Scan runs, ingestion receipts, slices, threat models, findings, dropped findings and patch proposals; state. | RepositoryScan reconciler and security APIs. Ingestion groups result application, run changes and receipt. Kubernetes RepositoryScan/Task identity remains authoritative. |
| `RepositoryMonitorStore`, `tools.RepositoryValidationBindingStore` | Monitor snapshots/runs/items, actions, review/publication records, commands, implementation/repair jobs, GitHub mutation records and monitor events; state. | RepositoryMonitor reconciler, APIs and repository validation tools. The narrow binding interface uses monitor-event persistence; it is not a separate database. |
| `UsageStore` | Immutable work ownership, Task observations, PR links/outcomes and retention watermark; state. | Usage reconciler, monitor workflows, event ingestion and usage API. Observation retries are idempotent; usage retention is independent of transcript cleanup. |
| `TaskJobAuthorityStore`, `TaskDataTransactionStore` | Job revocations and Task/namespace cleanup generations; state, coordinating current colocated artifact access too. | Worker API and brokered tool access, Task finalization and cleanup. Authorization is checked against live Kubernetes identity and revalidated against the stored cleanup generation before access. |
| `ControllerEpochMirror` | A subordinate copy of the Kubernetes-acquired epoch for payload transaction fences; state. | Controller epoch manager and SQLite turn/outbox persistence. A mirror cannot elect or advance controller ownership. |
| `ArtifactStore` | Public Task artifact bytes, filename, content type, size and timestamps in one record; artifacts. | Worker uploads/downloads, task APIs, scan workflows and cleanup. Metadata cannot be detached into state when selecting a future artifact backend. |
| `HealthChecker`, store lifecycle and retention configuration | Reachability, process lock, schema/bootstrap and scheduled retention. | Manager startup/shutdown and health checks. These are operational responsibilities rather than application record families. |

Narrow tool views (`MemoryReader`, `MemoryProposalWriter`, `TranscriptSearcher`,
`TaskMessageStore`) and API wrappers delegate to the corresponding production
families; they do not create new persistence owners.

ACP workspace transport is a separate existing filesystem surface under
`ORKA_ACP_ARTIFACT_ROOT` (default `/data/acp-artifacts`). The artifact API uses
`artifactcap.FileObjectStore` for content-addressed bytes/metadata and
`artifactcap.FileLedger` for operation replay. `CapabilityReservationRecorder`
and `IdentityRetirer` persist reservations/retirements used by the collector;
`cmd/main.go` wires these into runtime capabilities and Task cleanup. These are
not SQLite `ArtifactStore` rows or private native-session snapshots. A future
artifact contract must account for their ownership and receipt lifetime rather
than assuming every durable byte lives in the database.

Kubernetes workspaces/checkpoints and publication resources are also separate.
Keeping artifact bytes and metadata together does not make deletion across
those resources a database transaction.

## Kubernetes control authority and retained SQL compatibility

`store/kube.NewComposite` receives precisely `SessionTurnPersistenceStore`,
`OutboxPersistenceStore` and `SessionCleanupPersistenceStore` through
`kube.SQLitePersistence`. It implements
the production `DurableControlStore` using Kubernetes resources and Leases.

| Interface | Production owner |
| --- | --- |
| `ControllerEpochStore`, `ControllerEpochMutationStore` | ControllerEpoch resource, resource versions and epoch Lease interlock. |
| `PromptAttemptStore`, `PromptAttemptRetentionStore` | PromptAttempt resources and durable Task finalization evidence. Both `PreparePromptAttemptReclamation` and `ReclaimPromptAttempts` route through the Kubernetes control store. |
| `SessionControlStore` | RuntimeSessionControl resources and mutation Leases; delegates turn payload persistence to SQLite. |
| `BranchClaimStore`, `PublicationStore` | BranchClaim and Publication resources, with immutable identity and resource-version preconditions. |
| `ExternalEffectIdentityReader`, `ExternalEffectStore` | ExternalEffect resources and operation identity/receipts. |
| `OutboxProjectionStore` | Kubernetes epoch validation around SQLite `OutboxPersistenceStore`. |
| `SessionCleanupStore`, `SessionCleanupRecoveryStore`, `GatewaySessionCleanupStore` | Kubernetes/runtime deletion coordination, delegating durable intents and transcript cleanup to SQLite. |

The SQL tables `prompt_attempts`, `prompt_attempt_reclamations`,
`session_controls`, `branch_claims`, `publications` and `external_effects` remain
in the validated on-disk layout for compatibility and test fixtures. Their
duplicate SQL control-authority implementations are not production provider
requirements. `controller_epochs` also supplies the real subordinate mirror;
`session_turns` and `outbox_projections` contain real production persistence and
must not be discarded with the duplicate control implementation.

[#613](https://github.com/orka-agents/orka/issues/613) owns retiring or isolating
the test-only control implementation. [#620](https://github.com/orka-agents/orka/issues/620)
owns misleading snapshot reference counts: the current SQL count includes both
duplicate prompt attempts and real persisted SessionTurns. This inventory does
not redefine those counts. [PR #574](https://github.com/orka-agents/orka/pull/574)
removed harness v1 and its `harness_v1_attempts` and `runtime_sessions` tables.

## Callback and transaction inventory

These operations cannot become network RPCs by serializing their Go callbacks.
The eventual boundary needs explicit conditional commands and identity/version
preconditions, while local Kubernetes/runtime orchestration stays in Orka.

| Operation | Current behavior and boundary |
| --- | --- |
| `MemoryProposalApply.AuthorizeExistingMemory` | The external memory API supplies authorization for the exact existing memory chosen by `ApplyMemoryProposal`, including replay/source-proposal resolution. SQLite invokes it inside the application transaction. A future adapter must resolve identity/version, authorize in Orka, then commit against those preconditions without disclosing unauthorized content. |
| `WithTaskDataTransaction` | Runs a bounded database-only callback using a transaction-bearing context and a documented subset of store methods. No network work or nested transaction is permitted. Used for grouped approval/event/plan projection and worker access. |
| `WithAuthorizedTaskDataTransaction` | Captures/registers a cleanup generation, performs live Kubernetes authorization outside the database transaction, then checks the generation and runs the data callback atomically. Task-specific and namespace-shared access have distinct fences. A future state/artifact split must retain cleanup and Job revocation checks at both owners. |
| `SecurityStore.ApplyScanTaskIngestion` | The RepositoryScan reconciler supplies a live identity validator and application callback over a transaction-scoped `SecurityStore`. Validation runs before application and just before commit; callback writes, run changes and ingestion receipt commit together. A future command must bind the exact scan/run/Task identities and versions. |
| `ControllerEpochMutationStore.WithControllerEpochMutation` | Kubernetes Lease interlock around a short Kubernetes mutation. It is not a cross-resource transaction; UID/resourceVersion checks and ambiguous-outcome handling remain necessary. It belongs in Orka, not a database service. |
| `SessionRuntimeCleanupFunc` / `kube.WithSessionRuntimeCleanup` | Local dispatcher retires the exact resident runtime after the durable cleanup intent fences new work, before authority deletion. Runs outside the epoch mutation lock; mutations and short receipt writes revalidate their fences. Runtime cleanup is not a remote database callback. |

Typed turn finalization, deferred outbox activation, gateway projection,
deduplicated event/plan append, native-session finalization and cleanup intents
already express grouped workflows. Future work should preserve these guarantees
before introducing new protocol names, lost-response receipts, transport or
backend selection. Encryption/sealing and memory protections must remain owned
by Orka above an eventual external adapter; this contribution leaves the current
implementations and formats intact.
