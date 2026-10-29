# Storage ownership and bootstrap

This inventory describes the production wiring in `cmd/main.go` for
[#512](https://github.com/orka-agents/orka/issues/512). Application consumers
reach storage through one storage client over the public provider contract
described below. SQLite remains the only application database and artifact
store, with one active controller; there is no network adapter or backend
selection.

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

The `state.v1` ownership below means all database-owned application state
other than public artifacts. `artifacts.v1` owns artifact bytes and their
metadata together. Both are interfaces of the `orka.storage.v1` contract in
`pkg/storage/v1` (see "Storage client and provider contract"), which stays
unstable until a release advertises storage extension support. Backend
selection is not enabled. The current SQLite database contains both.
Kubernetes authority remains separate from either boundary.

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

## Storage client and provider contract

`pkg/storage/v1` is the public storage extension contract, version
`orka.storage.v1`. It defines the provider-facing records, errors, execution
event vocabulary and two provider interfaces:

- `StateProvider` serves `state.v1`: results and prompt result receipts, plans,
  messages, Sessions and transcripts (including turn commits, owner-fenced
  writes, soul pinning and lineage), Session cleanup persistence, the
  controller epoch mirror, outbox persistence, Task Job revocations, execution
  events, gateway records, security records, repository monitors and usage.
- `ArtifactProvider` serves `artifacts.v1`: artifact bytes and metadata.

`internal/store` keeps aliases for the moved names, so Orka callers keep their
existing types. The contract and its test suite do not import Orka's internal
packages.

### Provider obligations

These rules are part of the contract. The package documentation in
`pkg/storage/v1` states them for provider authors, and the shared suite checks
the observable parts.

- **Execution.** Each provider method is atomic, so a write commits all of its
  changes or none, and is linearizable with respect to every other call on the
  same dataset, as if calls ran one at a time. SQLite's single writer gives
  this for free, so a serial suite cannot
  tell a correct provider from one running at a weaker isolation level. A
  PostgreSQL provider at the default `READ COMMITTED` isolation could pass a
  serial suite and still let two dispatchers claim one gateway event, because
  claims, chat-turn reservations and terminal approvals read and then write.
  A method that returns several records, such as a Session with its
  transcript, reads them from one snapshot; SQLite does this for `GetSession`,
  `LoadTranscriptThrough`, `ReadSessionSoul`, `GetGatewayQueueStats`,
  `ListPatchProposals`, `GetLatestExecutionEventSeqs`,
  `ListSessionExecutionEvents` and `HasSessionCleanupFenceForUID`, the read
  methods found issuing more than one query. A write method that returns or
  compares the record it changed reads it in the same transaction as the
  change; SQLite does this for `RenewGatewayEventClaim`,
  `FreezeGatewayEventTaskRuntimeAllowedTools`, `RetryGatewayDelivery` and
  `SavePromptResultReceipt`. A write method that reads after a failed update
  to tell a missing record from a conflict also reads in the update's
  transaction; SQLite does this for `UpdateScanRun`,
  `RequestScanRunCancellation`, `CompleteScanRunCancellation`,
  `BindSessionCleanupIdentity` and `UpdatePatchProposal`. A write method that
  checks state before changing it makes the checks in the transaction of the
  change; SQLite's `AcquireLock` checks that the Session exists, its gateway
  ownership and its deletion fence in the transaction of its update, and
  `RecordUsage` compares a replayed observation in the transaction of its
  deduplicating insert. The suite's concurrency cases release eight callers at
  once against Session creation, chat-turn reservation, deduplicated event
  appends, terminal approvals and gateway claims, and require exactly one
  winner (or one claim per event). Eight more cases read while writes run, and
  every write waits for a read to start: one requires a Session's message
  count to match the messages returned, one requires stream heads read in
  several batches to come from one snapshot, one requires a Session events
  page and its latest sequence to come from one snapshot while streams are
  appended to and deleted, one requires a Session UID to stay fenced while its
  cleanup intent is replaced by a completion, one requires a successful
  gateway claim renewal to return the event still dispatching under its owner
  while claims are retried, one requires lock acquisition during deletion of
  the same Session to succeed or report it missing and deletion to fail once a
  lock was granted, one requires an identical usage observation replayed while
  retention removes it to be recorded, and one requires binding a cleanup
  identity to a Session created during the call to succeed or report it
  missing, never a conflict.
- **Durability.** A nil error means the change survives a provider restart and
  any failover the provider declares automatic. `Describe` reports a
  `DurabilityClass` so Orka can refuse automatic continuation after a failover
  that may lose acknowledged writes. The embedded SQLite store reports
  `local-buffered`: it runs in WAL mode with `synchronous=NORMAL`, so a commit
  survives a process restart, but the most recent commits can be lost if the
  host fails before the WAL is synced.
- **No cross-contract transactions.** No operation or transaction spans
  `state.v1` and `artifacts.v1`. Today the embedded store's task-data
  transaction also covers `SaveArtifact`; that is an embedded-family behavior,
  and the 2.3 redesign keeps the cleanup-generation and Job-revocation checks
  at the artifact provider on their own.
- **Errors.** A provider reports each refusal with its sentinel error:
  `ErrNotFound`, `ErrConflict` (including a reused identifier or a duplicate
  dedupe key), `ErrFenced`, `ErrValidation` (including an invalid cursor),
  `ErrTooLarge`, `ErrCapacity`, `ErrNotReady`, or a family error such as
  `ErrDuplicateMismatch` or `ErrGatewaySessionCleanupPending` (gateway
  admission deferred while the target Session is being cleaned up; the
  gateway answers 503 and the event is retried, unlike `ErrConflict`, which
  rejects it). A provider that could not run an operation returns
  `ErrUnavailable` only when nothing was committed; when a write may have
  committed, it returns `ErrOutcomeUnknown` and never success. `ErrFenced`
  marks a stale controller epoch or owner: the caller must stop rather than
  retry. Providers build it with `FencedErrorf`, which also matches
  `ErrConflict` so Orka callers written before `ErrFenced` existed keep their
  behavior. The embedded SQLite store reports controller epoch fence failures
  and owner-fenced Session writes this way; gateway claim and outbox lease
  owner mismatches still report `ErrConflict` or `ErrNotFound`. As an
  in-process store, SQLite returns its driver's errors for infrastructure
  failures; no commit outcome can be lost in transit.
- **Write envelope.** `WriteEnvelope{DatasetID, Fence, OperationID,
  RequestDigest}` is reserved and travels in the context
  (`WithWriteEnvelope`, `WriteEnvelopeFrom`). Orka does not attach it yet and
  providers must accept writes without one. Operation receipts (step 3) will
  check the dataset, the controller epoch fence and the operation identity at
  commit time. Only the outbox family takes a `ControllerEpochFence` today, as
  an explicit parameter that stays authoritative for it; the envelope reaches
  every other write family without another change to each of the contract's
  roughly 190 context-taking methods. Operation-specific preconditions, such
  as an expected version or message count, stay explicit parameters.
- **Provider description.** `Describe(ctx)` on both providers returns
  `ProviderInfo`: the contract version and parts served, the dataset ID and
  its origin (`new`, `adopted` or `migrated`), the provider's schema version,
  optional capabilities beyond the contract parts (none in v1), size limits
  and the durability class. It is there for startup validation (step 4),
  installation binding (step 5) and migration; nothing in Orka calls it yet.
- **Times.** A provider preserves every caller-supplied time exactly, to the
  nanosecond, and returns stored times in UTC. Connector consent and
  completion encryption bind `ExpiresAt.UTC().UnixNano()` into associated
  data, and usage pull-request observations key on a nanosecond time, so a
  provider that rounds to microseconds (PostgreSQL `timestamptz`, for example)
  must store the nanoseconds separately. The suite round-trips nanosecond
  times given in a non-UTC zone through Sessions, transcripts, gateway events,
  outbox projections, usage records, monitor runs, scan runs and the workflow
  families. SQLite opens its database with the driver's `_timezone=UTC`
  option, so every time column is written and read in UTC. Its queries compare
  times as text, which is correct only while every row uses one zone. The
  option does not rewrite stored rows: a database written by an earlier
  release on a host whose local zone was not UTC keeps those rows' offsets,
  and they can sort or pass a cutoff incorrectly against newer rows. Orka's
  container images run in UTC unless `TZ` is set, so this affects such
  databases only, such as local development files. Usage records are stored
  as JSON, so SQLite converts their times to UTC when it writes and reads them.
- **Records are not the wire format.** Records carry JSON tags for Orka's API.
  Persisted fields such as gateway claim ownership, lease expiry, the frozen
  Task tool policy, usage phase history, scan-run cancellation state and
  finding decision times are tagged `json:"-"`, so a provider that sends these
  structs as JSON loses them. The
  network protocol (step 6) defines its own wire records with a round-trip
  test over every record type. `ExecutionEvent.Internal` is not part of the
  contract and other providers must ignore it.
- **Artifacts.** Requests carry an `ArtifactOwner` (namespace, Task name and
  Task UID) so the identity can grow with a Job and cleanup generation without
  another signature change. The Task UID is reserved: Orka passes it where it
  has the Task, and providers may ignore it until artifact identity is
  enforced, when an old retry must not overwrite or delete a newer Task's
  artifacts. Every provider accepts artifacts up to `MaxArtifactBytes`
  (10 MiB) and rejects larger ones with `ErrTooLarge`. `ListArtifacts`
  returns filenames in byte order. `ArtifactMetadata.CreatedAt` and
  `Message.CreatedAt` are `time.Time`; the API's JSON output keeps the format
  it had when they were strings.

### Rules reserved for later steps

The contract states these now so later slices build on them; code follows in
the named step.

- **Cursors and order.** Cursors are opaque, provider-issued keyset tokens.
  Each list method states its sort key, pages have no duplicates or gaps under
  concurrent inserts, and an invalid cursor returns `ErrValidation`. SQLite's
  monitor and security cursors are still numeric offsets, and their sort keys
  live in SQL (one is `rowid`). Keyset paging comes with startup validation
  (step 4) or earlier if SQLite moves first.
- **Provider-assigned sequences.** `Message.ID`, transcript message order,
  `TranscriptSearchResult.MessageID`, `ExecutionEvent.Seq`,
  `SessionExecutionEvent.SessionSeq` and the accounting order of usage
  observations are assigned by the provider, monotonic within their scope,
  and become visible in commit order: a poller reading after a sequence must
  never later see a lower one commit. Database sequences alone do not give
  that, so a provider serializes allocation per stream. Migration preserves
  these values.
- **Clock ownership.** Callers supply lease times (`now` and lease durations)
  and most record times. The provider assigns some: artifact and message
  creation times, Task result timestamps, and in SQLite the repository monitor
  times it fills when a caller leaves them zero or always sets, such as in
  `UpsertRepositoryMonitor` and `CancelWorkActions`. Those two clocks diverge
  once the store is a separate service; operation receipts (step 3) decide
  which side owns each time.
- **Bounded lists.** `ListSessions`, `ListSessionCleanupIntents`,
  `ListActiveScanRuns` and `GetMessages` return
  every match. They gain limits and cursors in the same signature sweep as the
  write envelope.
- **`state.v1` is a relational contract.** Several methods push joins and
  reports into the provider: `LoadUsage` (an as-of multi-CTE report),
  `SearchTranscript` (case-insensitive substring search with history bounds),
  `CountImplementationJobs` with excluded statuses, the gateway
  `MissingDelivery` and `SessionHeadOnly` filters, and `ReasonContains`.
  `state.v1` targets a transactional database with secondary indexes; a
  key-value provider is not a goal. Moving these methods above the client onto
  primitive reads is needed only if that changes.
- **Migration export and import.** Behavioral methods cannot copy a dataset:
  they assign sequences, IDs and times, apply quotas and deduplication, and
  some records have no public writer. Migration (step 3 or 5) uses a reserved
  per-family raw record surface, `Export(family, cursor)` and
  `Import(family, records)` into an empty dataset, with the wire encoding
  above. Backup can reuse it.
- **Suite isolation.** A suite factory may return a fresh isolated dataset on
  a shared provider (a schema or key prefix) instead of a new provider per
  case, so the suite stays practical against a managed database.

`cmd/main.go` builds one `internal/storageclient.Client` over the SQLite store
and passes it to every application consumer. Outside the client, it uses the
SQLite store directly only for its process lock, bootstrap and key setup,
configuration and manager lifecycle. The client composes the state and artifact providers and serves the
remaining families through `storageclient.Embedded`, which only the SQLite
store implements:

| Embedded family | Why it is outside the provider contract |
| --- | --- |
| `AgentExecutionSnapshotStore`, `AgentExecutionSnapshotLifecycleStore` | Snapshot bodies arrive in plaintext; SQLite encrypts them. |
| `NativeSessionImportStore`, `SessionTurnPersistenceStore` | Native session bytes arrive in plaintext, including the native capture in turn finalization. |
| `ConnectorCredentialStore`, `ConnectorConsentStore` | Tokens arrive in plaintext; SQLite performs row-bound sealing. |
| `MemoryStore`, `MemoryProposalStore` | Redaction runs inside SQLite, and proposal application calls `AuthorizeExistingMemory`. |
| `TaskDataTransactionStore` transaction methods | Go callbacks run inside a database transaction. |
| `SecurityStore.ApplyScanTaskIngestion` | Go callbacks run inside a database transaction. |

Embedded families share transactions with state and artifact records (a
task-data transaction context also covers `SaveArtifact`, for example), so the
client refuses a configuration that would place them on different stores. That
sharing is an embedded-store behavior, not a provider obligation; see "No
cross-contract transactions" above.
The SQLite provider also still applies Orka's execution event payload
sanitization on append, and derives token usage observations from appended
model usage events using the controller-only `ExecutionEvent.Internal` keys
`usageTaskUID` and `harnessV2UsageModel`. That field is not serialized, and the
provider suite does not cover the derivation, so a provider that implements
only the documented contract would record no usage from execution events. Both
behaviors move above the client in follow-up work.

`pkg/storage/v1/storagetest` is the reusable behavioral suite. `RunState` and
`RunArtifacts` exercise each provider family through the public interfaces.
The SQLite provider runs it directly, and the storage client runs it over
SQLite.

SQLite also implements the duplicate SQL control-record methods described in
the next section. They are not part of the provider contract or the client. A
client test fails if any other SQLite method is missing from the client, so a
consumer's type assertion cannot silently lose a capability.

## Storage configuration

`internal/storageconfig` loads which backend serves each contract before
application storage opens. Both sources produce the same `Config`:

- **Default.** Without `--storage-config`, both contracts bind to embedded
  SQLite at `--store-path`. `--store-backend` accepts only `sqlite`.
- **File.** `--storage-config=<path>` names a YAML file with `bindings`
  (`state` and `artifacts`, each `sqlite` or an adapter name) and `adapters`.
  The Helm chart renders it from `storage.bindings` and `storage.adapters`
  into the `<release>-storage` ConfigMap when a contract names an adapter.
  `--store-backend` cannot be combined with a file, and `--store-path` is the
  database for contracts bound to `sqlite`.

```yaml
bindings:
  state: postgres
  artifacts: sqlite
adapters:
  postgres:
    serviceRef: {name: orka-storage-postgres, port: 8443}   # or url: https://...
    tls:
      serverName: orka-storage-postgres.orka-system.svc     # optional
      caSecretRef: {name: orka-storage-postgres-ca, key: ca.crt}   # optional
    clientAuth:
      bearerTokenSecretRef: {name: orka-storage-postgres-client, key: token}
```

Loading rejects unknown and duplicate fields, a binding that names no adapter,
an adapter bound to no contract, an adapter named `sqlite`, an adapter with
both or neither of `serviceRef` and `url`, a URL that is not `https://` or has
credentials, a query or a fragment, and an adapter without
`clientAuth.bearerTokenSecretRef`. The file has no namespace fields: Services
and Secrets resolve in the installation namespace.

The configuration digest is `sha256:` followed by the hex SHA-256 of
`orka.storage.configuration.v1`, a NUL byte and the compact JSON of the
bindings in contract order. It covers each binding's backend, SQLite path,
endpoint, TLS settings and Secret references, never Secret values, and does not
depend on the source: the default flags and a file binding both contracts to
the same SQLite path have the same digest. Startup logs the source, digest and
backends.

`Config.Resolve` reads the referenced Secrets with the manager's direct API
reader, so it does not wait for the cache. A configuration that uses only
SQLite reads nothing. A bearer token must have at least 32 bytes of visible
ASCII, and a CA bundle at least one PEM certificate.

Until the adapter transport exists, the controller serves both contracts only
from embedded SQLite. After the Secrets resolve, a contract bound to an
adapter stops startup with `AdapterTransportUnavailable`; nothing falls back to
SQLite. Every failure carries a stable reason for later status reporting:
`InvalidConfiguration`, `SecretNotFound`, `SecretKeyMissing`, `InvalidSecret`,
`SecretUnavailable` or `AdapterTransportUnavailable`.

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
