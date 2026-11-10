# Storage ownership and bootstrap

This inventory describes the production wiring in `cmd/main.go` for
[#512](https://github.com/orka-agents/orka/issues/512). Application consumers
reach storage through one storage client over the public provider contract
described below. SQLite remains the only application database and artifact
store, with one active controller; there is no network adapter or backend
selection.

## Dataset metadata

`store_dataset` contains exactly one canonical, nonzero UUID, an origin (`new` or
`adopted`), a bootstrap-completion flag and the dataset's installation binding:
the installation that owns it (recorded by `BindDataset`, or by the first
controller epoch advance until the installation binds it itself), the record
UID and storage switch generation that bound it, the snapshot key fingerprint,
whether the installation has detached it, and when the binding last changed.
`PRAGMA user_version = 4` identifies this metadata layout. Opening a database
still validates every table and index definition: a version marker alone cannot
establish compatibility.

Fresh schema creation, identity creation and version recording commit together.
Exact supported historical layouts are backed up and upgraded under the existing
process lock and one `BEGIN IMMEDIATE` transaction. The 167-to-168 transition
adds the metadata table, the 168-to-169 transition adds operation receipts and
the epoch advance columns, the 169-to-170 transition adds the binding columns,
the 170-to-171 transition makes a `retry_pending` work action own its dedupe
key, and the upgrade records an adopted identity. A store at an earlier schema
version upgrades through numbered version steps under the same lock, backup and
transaction: a version-1 store (the 159-object layout) gains the 168-to-169,
169-to-170 and 170-to-171 objects, a version-2 store gains the 169-to-170
columns and the 170-to-171 index, a version-3 store gains the 170-to-171 index,
and all keep their identity row, records and bootstrap state unchanged. Before
the 170-to-171 index is rebuilt, a `retry_pending` work action that shares its
dedupe key with a queued, leased or running action, or with a newer
`retry_pending` one, is cancelled with reason `dedupe_superseded`. An installation
a version-2 store's epoch advance recorded stays its owner, at switch
generation 0 with no record. Missing or malformed
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
every connector custody family before recording bootstrap completion and
exposing the store to consumers. The storage client performs it (see "Key
custody and sealing"). Failure closes SQLite and releases the lock. An interrupted fresh
start can resume with the same identity; an API-committed Secret key is reused
after a lost response or an update conflict. Clearing ordinary records does not
reopen permission to mint a key after completion. Existing key-rotation rules
remain, including authentication of protected retained ciphertext and the
existing expiry handling for connector consents.

This is dataset-local history. The store-side binding records which
installation owns the dataset, but Orka does not yet keep the matching
Kubernetes installation record or check the two against each other at
startup: replacing the whole database also replaces its history. A UUID does
not detect rollback or establish that a replacement empty database belongs to
an installation. Backup/restore and the installation record require additional
protocols.

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
| `AgentExecutionSnapshotStore`, `AgentExecutionSnapshotLifecycleStore` | Immutable snapshots keyed by Task UID/digest, sealed by the storage client, with metadata and retention operations; state. | Task admission/dispatch, internal APIs and reference-aware retention. Kubernetes Task bindings are separate. |
| `SessionLineageStore` | Payload projection of Kubernetes-authoritative Session lineage; state. | Session continuity and dispatch. Projection failure may block dispatch but never changes Kubernetes lineage authority. |
| `NativeSessionStore`, `NativeSessionImportStore` | Private native session records sealed by the storage client, import/capture receipts, finalization receipts and deleted-name tombstones; state. | ACP continuity/finalization and migration APIs. Native state is not a public Task artifact; reads validate canonical Session UID and transcript boundary. |
| `ConnectorCredentialStore`, `ConnectorConsentStore` | Credentials, consents and parked completions sealed by the storage client, grants, retired custody, versions, sequences and disconnect tombstones; state. | Connector APIs, credential source and linked-account tool execution. Kubernetes Connection resources carry authority/metadata; secret custody stays in the database and is authenticated by the snapshot key. |
| `SessionTurnPersistenceStore` | Turn records, transcript additions and deferred terminal outbox projections; state. | Kubernetes composite control store. `CommitSessionTurnFinalization` groups these records (and applicable sealed native state) atomically; projection activates after Kubernetes finalization. |
| `OutboxPersistenceStore` | Projection payloads, claims and completion receipts; state. | Kubernetes composite control store and projection reconciler. Database persistence does not choose controller ownership. |
| `SessionCleanupPersistenceStore`, `SessionTurnCleanupReceiptStore` | Cleanup identity, durable intents, completion tombstones and retained turn receipts; state. | Kubernetes composite and cleanup recovery. These close crash windows across Kubernetes deletion and transcript deletion. |
| `GatewayEventStore`, `GatewayDeliveryStore` | Ingress deduplication/claims, canonical transcript projection, deliveries, budgets, retries and event tombstones; state. | Gateway service, adapters, task reconciliation and conversation reply tools. Existing grouped admission/projection and delivery idempotency must survive a future adapter. |
| `GatewayTaskCleanupReceiptStore`, `GatewayTaskCleanupReceiptMaintenanceStore`, `GatewaySessionCleanupCandidateStore` | Compaction ownership evidence and retained transcript candidates; state. | Gateway retention/recovery. Live uncached Kubernetes absence checks precede receipt deletion. |
| `MemoryStore`, `MemoryProposalStore` | Durable namespace memories and governance proposals/review/application history; state. | Memory APIs and worker tools. Proposal review does not apply a memory. Redaction and authorization remain Orka responsibilities. |
| `PlanStore`, `ExecutionEventStore`, `DeduplicatingExecutionEventStore`, `AtomicExecutionEventPlanStore` | Plan state, ordered event streams, sequence projections and deduplication; state. | Task reconciliation, runtime event capture, SSE/API and worker tools. Plan-aware append groups event and plan update. Existing event retention is preserved. |
| `MessageStore` | Inter-agent messages and read state; state. | Worker APIs, task message tools and Task cleanup. A consuming read is a write that marks exactly the messages it returns, and its replay returns the same messages (see "Messages, counters and chat turns"). |
| `SecurityStore` | Scan runs, ingestion receipts, slices, threat models, findings, dropped findings and patch proposals; state. | RepositoryScan reconciler and security APIs. Ingestion groups result application, run changes and receipt. Kubernetes RepositoryScan/Task identity remains authoritative. |
| `RepositoryMonitorStore`, `tools.RepositoryValidationBindingStore` | Monitor snapshots/runs/items, actions, review/publication records, commands, implementation/repair jobs, GitHub mutation records and monitor events; state. | RepositoryMonitor reconciler, APIs and repository validation tools. The narrow binding interface uses monitor-event persistence; it is not a separate database. |
| `UsageStore` | Immutable work ownership, Task observations, PR links/outcomes and retention watermark; state. | Usage reconciler, monitor workflows, event ingestion and usage API. Observation retries are idempotent; usage retention is independent of transcript cleanup. |
| `TaskJobAuthorityStore`, `TaskDataFenceStore` | Job revocations and Task/namespace cleanup generations; state, also fencing colocated artifact writes. | Worker API and brokered tool access, Task finalization and cleanup. Authorization is checked against live Kubernetes identity with no store transaction open; each fenced data call then checks the cleanup generation and authorized Jobs in its own transaction. |
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
  controller epoch mirror, outbox persistence, Task Job revocations and task
  data fences, execution events (including grouped appends), gateway records,
  security records (including scan Task ingestion), repository monitors,
  usage, memories with their proposals (creation, review, archiving and
  application), SessionTurn records with their finalization, and sealed
  execution snapshots, native session records and connector custody (see
  "Key custody and sealing").
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
  `state.v1` and `artifacts.v1`. A fenced `SaveArtifact` checks the cleanup
  generation and Job revocations in the artifact provider's own transaction,
  against the generations of the store the fence was resolved from. Orka
  resolves every fence from `state.v1`, so while it does, the client keeps both
  contracts on one store. Binding artifacts to a separate provider also needs
  artifact-side cleanup generations and Job revocations.
- **No callbacks or held transactions.** Provider methods take no Go
  functions, and no provider transaction stays open while Orka authorizes,
  validates against Kubernetes or computes. Where Orka must check something
  between a read and a write, the contract splits the operation into a
  resolve step and a conditional command, described in "Conditional commands"
  below and, for the sealed families, in "Key custody and sealing".
- **Errors.** A provider reports each refusal with its sentinel error:
  `ErrNotFound`, `ErrConflict` (including a reused identifier or a duplicate
  dedupe key), `ErrFenced`, `ErrPreconditionFailed` (a conditional command's
  target or version changed), `ErrValidation` (including an invalid cursor),
  `ErrTooLarge`, `ErrCapacity`, `ErrNotReady`, or a family error such as
  `ErrDuplicateMismatch`, `ErrTaskDataCleanupChanged`, `ErrTaskJobRevoked` or
  `ErrGatewaySessionCleanupPending` (gateway
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
  failures; no commit outcome can be lost in transit. A provider reached over
  a network returns `ErrUnauthenticated` when it rejects the caller's
  credentials (missing, expired, invalid or not permitted for the dataset);
  retrying with the same credentials fails again, so Orka reloads them first.
  `ErrIncompatible` means the provider cannot serve the contract revision a
  call needs or the dataset's schema; retrying cannot succeed until the
  provider or the dataset is replaced. Neither commits anything.
- **Write envelope.** `WriteEnvelope{DatasetID, Fence, OperationID,
  RequestDigest, IssuedAt, Sequence}` travels in the context
  (`WithWriteEnvelope`, `WriteEnvelopeFrom`). The storage client attaches it
  to every write, and a provider refuses a write without one with
  `ErrValidation`. The checks it carries, operation
  receipts and forward-only controller ownership are described in "Write
  envelopes, operation receipts and controller ownership" below. Only the
  outbox family takes a `ControllerEpochFence` today, as an explicit parameter
  that stays authoritative for it and must equal the envelope fence; the
  envelope reaches every other write family without another change to each of
  the contract's roughly 200 context-taking methods. Operation-specific
  preconditions, such as an expected version or message count, stay explicit
  parameters.
- **Provider description.** `Describe(ctx)` on both providers returns
  `ProviderInfo`: the contract version and parts served, the dataset ID and
  its origin (`new`, `adopted` or `migrated`), the provider's schema version,
  optional capabilities beyond the contract parts (none in v1), size limits
  and the durability class. It is there for startup validation (step 4),
  installation binding (step 5) and migration; nothing in Orka calls it yet.
  It also reports the contract revision the provider implements, the
  dataset's state, an `Incarnation` that changes when the serving database
  may have restarted or switched and stays the same otherwise, the provider
  clock when `Describe` ran (`ObservedAt`, in UTC), how far the dataset is
  through its first start (`Bootstrap`: whether it completed, and whether the
  dataset holds no application record, which a provider may report as false
  when it cannot prove it cheaply), and optionally the dataset's size
  (`Usage`). The operation receipt retention and result limit stay zero until
  providers keep receipts. `Describe` is read-only, may take up to about a
  second, and answers for a dataset Orka has not yet bound, bootstrapped or
  advanced to a controller epoch, so it carries no envelope and is never
  fenced. It returns `ErrUnavailable` when the dataset cannot be reached,
  `ErrUnauthenticated` when credentials are rejected, and `ErrIncompatible`
  when the provider cannot report on the dataset at all, for example because
  it does not recognize the dataset's schema version. The embedded
  SQLite store reports the current revision, `ready` (it upgrades its schema when it
  opens, under the process lock), an incarnation generated each time it
  opens, its bootstrap flag and a scan of its tables, and the size of its
  file plus the write-ahead log. A schema version it does not support returns
  `ErrIncompatible`.
- **Contract revisions and schema verdicts.** `ContractRevision` counts the
  additions to `orka.storage.v1` that every provider must implement; a
  provider reports the highest revision whose obligations it meets, and
  revisions are additive. A change that adds such an obligation increments
  `storagev1.ContractRevision`, so startup validation can refuse an older
  provider with one precise reason instead of failing at the first missing
  method; an optional family is announced as a capability instead. Revision
  2 requires operation receipts and controller epoch advancement on both
  provider contracts. Revision 3 added dataset binding (see "Installation
  binding"). Revision 4 requires a valid write envelope on every write and
  `ErrValidation` when it is missing; revision 3 still required accepting
  writes without one during the transition. Revision 5 adds
  `MarkWorkActionRetryPending`, which parks an active work action for retry
  and leaves a finished one unchanged in one step, and defines the work-action
  statuses and which of them are active (`WorkActionStatus*`,
  `IsWorkActionActive`). Revision 6 split the inbox read into
  `ListUnreadMessages` and `ConsumeMessages`, made `SendMessage` write the
  assigned ID and creation time back, and made every Session lease
  decision, and the claim check in
  `FreezeGatewayEventTaskRuntimeAllowedTools`, use the time the request
  carries (see "Messages, counters and chat turns" and "Lease clocks").
  The revision Orka requires for each part belongs to Orka, not to the
  contract.
  Orka never interprets a provider's `SchemaVersion` beyond equality: a
  provider gives its own verdict on its schema. One that can still read the
  identity of a dataset it cannot serve reports the state `incompatible`;
  such a dataset answers `Describe` and returns `ErrIncompatible` from every
  other call, committing nothing, until an operator replaces the provider or
  the dataset. One that cannot read the identity answers `Describe` with
  `ErrIncompatible`. Orka treats both as the same verdict.
- **Health checks.** `HealthCheck` reaches the dataset, not only the provider
  process, takes no write lock and is cheap: Orka probes every few seconds
  with a short deadline, so it should finish in tens of milliseconds. It
  works before binding and before the controller epoch is advanced. It
  returns nil, `ErrUnavailable` (unreachable), `ErrNotReady` (`migrating` or
  `importing`), `ErrUnauthenticated` or `ErrIncompatible`. SQLite, whose
  database the controller process holds open, runs `SELECT 1` on that
  connection.
- **Call classes.** The method catalog gives every method a call class, which
  a storage client uses to choose its deadline, retries and concurrency limit
  and a network transport uses to carry bulk transfers on their own
  connection: `control` for `HealthCheck`, `Describe` and the controller epoch
  and operation resolution methods, which must get through when everything
  else is saturated; `background` for retention, receipt pruning and recovery
  scans; `bulk` for results, prompt result receipts, artifacts, sealed
  execution snapshots and native session records (including the turn
  finalization that carries one), which move whole bodies of up to several
  megabytes; and `interactive` for everything
  else. A fifth class, `stream`, is never assigned by the catalog: a poller,
  such as an event stream, chooses it for its reads so the number of open
  streams cannot use up the interactive limit.
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
  test over every record type. `ExecutionEvent.Usage` is also tagged
  `json:"-"`: it carries the usage observation Orka derived from the event
  (see "Orka policy above the client").
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

### Write envelopes, operation receipts and controller ownership

These rules make retried writes safe and keep a replaced controller from
writing (#512 step 3). The types are in `pkg/storage/v1`: `WriteEnvelope`,
`RequestDigest`, the method catalog (`LookupMethod`, `Methods`),
`OperationReceiptStore`, `ControllerEpochAdvancer`, the errors
`ErrDatasetMismatch`, `ErrOperationExpired`, `ErrOperationAborted` and
`ErrClockSkew`, and the new `ProviderInfo` fields. Both providers implement
`OperationReceiptStore` and `ControllerEpochAdvancer`, and check every write
when it commits. SQLite does, and the shared suite's
write catalog (`storagetest`) runs the dataset, receipt, age, fence and
resolution checks for every write method, the sealed families, Task data
fences and conditional commands included.
The storage client attaches an envelope to every write (see "Envelopes in the
storage client" below), and a provider refuses a write without one with
`ErrValidation`. Production SQLite does; SQLite's package tests may still
write without one.

- **Method catalog.** Every provider method is a read, a write or an
  administrative method. Reads carry no envelope and need no stored receipt.
  Every write carries an envelope and leaves an operation receipt.
  `AdvanceControllerEpoch`, `ResolveOperation`, `PruneOperationReceipts`, the
  dataset binding writes (`BindDataset`, `DetachDatasetBinding`,
  `UpdateBindingKeyFingerprint`, `CompleteDatasetBootstrap`) and, until it is
  replaced, `SyncControllerEpochMirror` are administrative: they carry no
  envelope and are not checked against the current fence.
  Reading an inbox is two methods, the plain read `ListUnreadMessages` and
  the consuming write `ConsumeMessages`, so each is classified once.
  `IsLocked` counts as a write, because SQLite clears an expired Task lock
  when it finds one. A contract
  test fails when a provider interface method is missing from the catalog, so
  no new method can ship unclassified. A write may also name its natural key:
  request values, such as a dedupe key or a cleanup operation ID, by which the
  provider recognizes a repeat, so running it again as a new operation
  returns the stored outcome or fails, but never repeats its effect.
- **Envelope.** `WriteEnvelope` names the dataset as `Describe` reports it,
  the issuing controller's fence for that dataset, an operation ID of at most
  256 bytes, the request digest, the controller time the operation was
  created (`IssuedAt`) and its issue order within the fence (`Sequence`).
  Every retry of an operation reuses the whole envelope. `Validate` checks the
  fields; a write without a valid envelope returns `ErrValidation`. A method
  that already takes an explicit `ControllerEpochFence` keeps it
  authoritative, and it must equal the envelope fence.
- **Checks at commit.** In the transaction that commits a write, a provider
  checks, in this order:
  1. the dataset, or `ErrDatasetMismatch`;
  2. the operation receipt. A committed receipt with the same method and
     digest returns the original outcome without the later checks, so a
     retry reads the original result even under a stale controller fence and
     changes nothing. A Task data fence carried in the context is still
     checked before the outcome is returned, so a replay after the cleanup or
     Job revocation that fence guards returns `ErrTaskDataCleanupChanged` or
     `ErrTaskJobRevoked` instead. A different method or digest returns
     `ErrDuplicateMismatch`, and an aborted operation returns
     `ErrOperationAborted`;
  3. the operation's age: `IssuedAt` more than `MaxOperationAge` (15 minutes)
     before the provider clock returns `ErrOperationExpired`, and more than
     `MaxClockSkew` (30 seconds) after it returns `ErrClockSkew`;
  4. the dataset state: `migrating` or `importing` returns `ErrNotReady`
     (an `incompatible` dataset refuses every call with `ErrIncompatible`),
     and a dataset its installation has detached returns
     `ErrDatasetDetached`, which matches `ErrNotReady`;
  5. the fence, which must equal the dataset's stored controller epoch, or
     `ErrFenced`. The provider reads it under a lock that conflicts with an
     epoch advance;
  6. the method's own preconditions.

  It then writes the change and its receipt together. A refusal commits
  nothing and records no receipt, so a later attempt is evaluated again.
- **Request digests.** `RequestDigest(method, args...)` hashes the method and
  the arguments after the context. It walks Go values, not JSON, so fields
  tagged `json:"-"` are covered outside dynamic values. It stays stable
  across additive changes and
  across a process boundary, so a network server can recompute it from the
  request it decoded: zero struct fields are left out, so a field added later
  and left unset does not change the digest; a non-nil pointer counts even
  when it points to a zero value; maps are sorted; times are UTC seconds and
  nanoseconds; and dynamic values (`any`, non-empty maps of `any` and
  `json.RawMessage`) are canonical JSON with sorted keys and every number
  kept exactly as written, so distinct numbers, such as
  `1152921504606846976` and `1152921504606847000`, never share a digest. A
  JSON null counts as absent; an empty JSON array or object is a value. A
  server that recomputes the digest must decode dynamic values losslessly,
  with `UseNumber` or as `json.RawMessage` bytes, as it must anyway to store
  them unchanged; a copy decoded into float64 is only guaranteed to match a
  typed value's floating-point numbers and integers up to 2^53, and where it
  rewrote a number it fails the digest check rather than matching another
  request.
- **Receipts.** A receipt commits with its change. A replay returns the
  original outcome and changes nothing; a provider either keeps the typed
  result with the receipt, up to `MaxReceiptResultBytes` (1 MiB) in its own
  encoding, or reads it again from the records the write made, so the replay
  carries the original result, including any identifiers the write assigned.
  Outputs the records do not hold, such as a created flag or a count, are
  always kept with the receipt. A result read again reflects records that
  changed or were deleted since, and a method whose replay can differ that
  way says so. `ResolveOperation` reports only the
  state; a caller reads a committed result by retrying with the same
  envelope. A successful write that changed nothing is a
  committed outcome and gets a receipt, so a delayed duplicate cannot act
  later, for example by releasing a lock the same Task acquired again. A
  receipt never holds plaintext protected data. A receipt may hold a sealed
  result, such as the connector credential, consent or completion a custody
  write returns; deleting, shredding or tombstoning that Connection's custody,
  or deleting its consents, clears those results in the same transaction, and
  a replay of such a write then returns `ErrConflict` without running it
  again. A provider keeps receipts for
  at least `MinReceiptRetention` (1 hour) on its own clock, which is longer
  than `MaxOperationAge` plus `MaxClockSkew`, so every retry it can still
  accept finds its receipt. `PruneOperationReceipts` removes older receipts in
  bounded batches, and `Describe` reports the retention and the result limit.
  SQLite measures receipt ages on the host's wall clock, which time
  synchronization can step back, so it keeps receipts for
  `MinReceiptRetention` plus 15 minutes and assumes cumulative clock regression
  between pruning and every later resolution never exceeds that allowance,
  including repeated steps, process restarts and host failovers. The allowance
  does not reset at each step or restart. Beyond it, `ResolveOperation`
  could report an operation whose receipt was already pruned as aborted.
- **Unknown outcomes.** When a write's outcome is unknown (`ErrOutcomeUnknown`,
  or a transport failure after the request was sent), the storage client
  retries the same envelope until its deadline and then calls
  `ResolveOperation` with the operation's `IssuedAt`. That returns the
  committed outcome, or durably records the operation as aborted so it can
  never commit later. Any controller may resolve an operation, including one
  whose fence is stale, because resolution can only prevent a change. A
  missing receipt proves nothing committed only while any receipt the
  operation could have left is still kept, so for an operation older than
  `MinReceiptRetention` minus `MaxClockSkew`, `ResolveOperation`
  records nothing and returns `ErrOperationExpired`: the outcome stays unknown
  for good, and the affected work is held for recovery. The bound uses the
  contract's minimum retention, so raising a provider's retention never
  turns a receipt pruned earlier into a false abort. Retention and operation
  ages use a durable non-regressing provider clock, or retention is extended
  by a bound on cumulative clock regression between pruning and every later
  resolution, including repeated backward steps, restarts and failovers.
  A bound on the largest individual step does not suffice. While an outcome
  cannot be settled, the client refuses new writes to that dataset with
  `ErrNotReady` and keeps trying to resolve it; an outcome found to be
  permanently unknown moves to a recovery hold on the affected work and stops
  blocking the dataset. A refusal of a retry settles only that attempt: the
  earlier attempt whose outcome was unknown may have committed and had its
  receipt pruned since, or may still commit, for example after a migration
  finishes. The operation is settled only by a replay, by
  `ErrOperationAborted` or by a `ResolveOperation` result. For one operation
  ID, a provider makes the receipt check and commit of each attempt and an
  abort recorded by `ResolveOperation` atomic with respect to one another,
  for example by claiming the operation ID first, so an operation never both
  commits and aborts. `ErrOutcomeUnknown` is
  neither success nor failure: an API handler answers 409 without a retry hint
  and tells the caller not to repeat the request, while a reconciler requeues
  using its durable operation identity. Neither reports success. An operation
  ID names one operation instance and is never derived from a durable
  identity, so an aborted instance does not block a later one. A durable
  operation run again after a restart is a new operation. Its natural key, or
  for a write such as a delete the state it changes, makes that safe; a write
  with neither, such as a send or a counter update, must not be run again
  unless the caller can observe whether the first one committed.
- **Messages, counters and chat turns.** Receipts make these retries safe: a
  replayed `SendMessage` writes back the assigned ID and creation time
  without a second message, a replayed counter update adds nothing, and a
  replayed chat-turn reservation or commit returns its original outcome.
  `ListUnreadMessages` returns the oldest unread messages of a Task's inbox,
  ordered by ID, and changes nothing. `ConsumeMessages` returns them and
  marks exactly those messages read, by ID, so a message the selection did
  not return is never marked at any isolation level; its replay returns the
  same messages, without any that Task cleanup deleted in between. Both
  return at most `MaxMessageBatch` (500) messages per call, so a longer
  backlog is read over several calls. Orka's own message store keeps one
  `GetMessages(markRead)`, which the client maps onto the two. SQLite keeps
  the consumed IDs with the receipt and reads the messages again on replay,
  which is why a provider never reuses a message ID.
- **Client-assigned identities and times.** Before computing a write's
  digest, the storage client gives a transcript message without an ID a
  UUIDv7 (in `AppendMessages`, `AppendMessagesWithLock` and
  `CommitSessionTurn`), a memory or memory proposal without an ID one in the
  `mem-` or `mprop-` form, and a dropped finding without an ID a `drop_`
  hash, and fills their zero creation times, and a message's, with the
  operation's `IssuedAt`. Every retry therefore sends the same request, and
  no provider clock or ID generator decides what Orka stores. A transcript
  message that already has an ID is the caller's natural key and keeps a zero
  timestamp, because a provider matches a repeat of it without comparing a
  zero timestamp. Finding IDs are already derived from fingerprints. A
  provider still fills a value that a caller bypassing the client leaves
  empty.
- **Artifact and result identity.** Artifact writes and deletes are bound to
  the Task UID. A delete leaves a tombstone for that UID, which refuses a
  delayed save for the deleted Task. A replacement carries the envelope's
  epoch and sequence, so an older write cannot overwrite a newer one or reach
  a later Task with the same name. Results follow the same rules and gain a
  `MaxResultBytes` limit equal to the API's 10 MiB. Files stored before UIDs
  were recorded belong to the current holder of the Task name until its first
  delete.
- **Controller ownership.** Kubernetes decides which controller may act. Each
  dataset holds its own controller epoch, which `AdvanceControllerEpoch`
  moves to the exact record the Kubernetes compare-and-swap produced. The
  request also carries the installation identity (`InstallationID`); Orka
  passes the UID of the installation's ControllerEpoch object. The advance
  only moves forward. A lower epoch or another installation returns
  `ErrFenced`, a repeat of the stored epoch with the same holder and request
  digest succeeds, any other record at the same epoch returns `ErrConflict`,
  and forward jumps are allowed. A detached dataset returns
  `ErrDatasetDetached`. A refused advance commits nothing. A dataset that
  belongs to no installation records the installation of its first successful
  advance, including a repeat of the stored epoch; that installation owns the
  dataset at switch generation 0 until it binds the dataset itself (see
  "Installation binding"). The advance
  waits for fenced writes in progress, and once it returns, no write with an
  older fence commits, however long it was delayed. At takeover Orka
  acquires the epoch in Kubernetes. The full protocol, including acknowledgement
  tracking, for each distinct dataset it is bound to is:
  1. reads the last durable acknowledgement and advances the dataset with
     `AcknowledgedEpoch` set to that
     acknowledgement. The provider checks its stored epoch against this floor
     atomically with the advance, before changing any epoch or installation.
     An absent epoch counts as zero. A lower stored epoch returns `ErrNotReady`
     without changing anything, and Orka stops for recovery. Every later
     takeover supplies the same acknowledgement until a new one is recorded,
     so a controller crash cannot erase the rollback evidence; checking only
     `Previous` after an advance would lose that evidence on the next advance;
  2. records the new acknowledgement in Kubernetes, in the ControllerEpoch
     status until an installation record exists;
  3. opens that dataset's fence for writes.

  The controller epoch manager currently advances each
  dataset with the exact record the compare-and-swap produced and the
  ControllerEpoch UID, and refuses to open a dataset whose advance is
  refused, then opens that dataset's fence. It leaves `AcknowledgedEpoch` zero:
  persisting and reading dataset acknowledgements, supplying the floor, and
  recovery holds remain future work.

  Each dataset's fence opens independently, and a write waits only for the
  fence of the contract that serves it (`MethodInfo.Contract`), so an
  artifacts outage at takeover does not stop state writes. This is safe
  because no transaction spans the two contracts and each store checks its
  own fence at commit. Until a store's own advance, the replaced controller's
  delayed writes can still commit there, but the successor does not write
  that store until then, so each store has one writing controller at a time.
  `CurrentFence`, which Kubernetes mutations and the explicit-fence store
  methods use, opens with the state dataset's fence. Kubernetes-only
  mutations keep using the Kubernetes fence.
- **Single writer and schema migration.** A store refuses writes under any
  fence but its current one, and a dataset bound to one installation refuses
  another installation's advance. A schema change moves the dataset to
  `migrating` under the same lock as an epoch advance, so writes in progress
  finish first and later new writes return `ErrNotReady` until the dataset is
  `ready` again; a replay of a committed operation is still served. A crash
  during migration leaves the dataset `migrating`. While the dataset is not
  ready, `ResolveOperation` returns an outcome it already holds and otherwise
  returns `ErrNotReady`, recording nothing. A provider that imports a dataset
  either carries its operation receipts and aborted records with it or gives
  the imported dataset a new dataset ID, so a resolution can never report a
  committed operation as aborted. `Describe` reports the state. SQLite keeps
  its process lock as well.
- **Lease clocks.** A provider decides a lease only by comparing times passed
  in the same request with stored times that came from requests. It never
  compares a caller's time with its own clock. Leases are therefore decided on
  the controller clock, and the commit-time fence makes an early reclaim after
  a takeover harmless: a new controller whose clock is ahead may reclaim a
  lease early, but the old holder can no longer commit, and one whose clock
  is behind resumes the work up to twice `MaxClockSkew` later. A request's
  times are fixed for the operation and covered by its digest, so a retry is
  judged at the original request time, which owner identity, not expiry,
  makes harmless. Gateway claims, outbox claims and gateway message
  admission carry the request time and lease they need, and
  `FreezeGatewayEventTaskRuntimeAllowedTools` refuses a zero request time
  with `ErrValidation` rather than judging the claim on its own clock. Chat
  turns and Session locks carry them too: `AcquireChatTurnRequest` and
  `CommitSessionTurnRequest` have `Now` (and the reservation's `ExpiresAt`);
  `AppendMessagesWithLock`, `UpdateTokenCountsWithLock`,
  `ReleaseLockWithTokenUsage` and `EnsureSessionSoulWithLock` take a
  `SessionOwnerWrite` with the lock owner and `Now`; and `AcquireLock`,
  `IsLocked` and `DeleteSession`, which reclaim or respect an expired chat
  turn or transient lock, take `now`. A zero request time returns
  `ErrValidation`. `PrepareSessionCleanup` and `CompleteSessionCleanup` judge
  the locks at the intent's `PreparedAt`. Session mutation leases and prompt
  attempts are Kubernetes resources and keep the controller clock there. The
  shared suite decides every lease at request times months away from the
  wall clock, so a provider that consults its own clock fails it.
- **Provider clock and skew.** The provider clock is used only for operation
  age and the future-skew check, receipt and tombstone retention,
  `ObservedAt`, and informational times the provider assigns when a caller
  leaves them zero, plus connector completion expiry checked inside
  `CommitSealedConnectorCompletion` at commit time. That expiry is a
  redemption deadline, not a controller-owned lease: both the request time
  and the provider's current time must precede it, so a stalled commit
  cannot redeem an expired completion. In SQLite the provider clock is the
  controller's, so the two never differ. For a separate store, the storage
  client estimates the offset from `ObservedAt` as
  `ObservedAt - (sent + received) / 2`, uncertain by half the round trip,
  when the leader starts and every minute after
  (`ClockMonitor`). While the offset exceeds `MaxClockSkew` (30 seconds) even
  at the edge of that uncertainty, it refuses new writes to that dataset with
  `ErrNotReady` and logs it, and it resumes them when a later check finds the
  clocks back within bounds; it never restarts the controller over skew. A
  provider that does not report `ObservedAt` is not checked. A provider also
  refuses an `IssuedAt` more than `MaxClockSkew` in its future with
  `ErrClockSkew`, which catches a fast controller clock without the check.
  Which clock owns each time:

  | Time | Clock |
  | --- | --- |
  | Chat turn reservations, Session locks, gateway and outbox claims, gateway message expiry | Controller, in the request |
  | Session mutation leases and prompt attempts | Controller, in Kubernetes |
  | Operation age, future skew, receipt and tombstone retention, `ObservedAt` | Provider |
  | Connector completion expiry, at commit | Both: the request time and the provider's current time must precede it |
  | Creation times of messages, transcript messages, memories, proposals and dropped findings; execution event times | Controller, filled by the storage client |
  | Other record times a caller leaves zero, such as artifact, result, monitor and security times | Provider, informational only |
- **Durability and lost writes.** A nil error means the change and its
  receipt survive what the provider's `DurabilityClass` promises: an
  automatic switch loses no acknowledged write under `synchronous-replicated`;
  `local-fsync` survives a host crash; `local-buffered`, the SQLite default,
  may lose the latest writes in a host crash; and `async-replicated` may lose
  acknowledged writes in a switch. `Describe` reports an `Incarnation` that
  changes when the database may have restarted or switched. After a change,
  Orka validates the dataset again and checks that the store still holds the
  current epoch. After a potentially lossy switch, an `async-replicated`
  provider itself refuses new writes and missing-receipt resolution with
  `ErrNotReady`, recording no abort, from the moment the new incarnation can
  serve requests until an operator explicitly acknowledges that incarnation
  after recovering or holding the affected work. The provider enforces this
  gate atomically with write commits and missing-receipt checks; an Orka
  `Describe` check, even immediately before resolution, cannot close the race.
  A retained receipt may still return its known outcome. Acknowledgement
  permits new work but does not prove that a pre-switch operation aborted:
  a missing pre-switch outcome remains unknown unless independently recovered,
  even after the gate opens. Orka stops affected work until recovery and never
  repeats it as a new operation merely because the switch was acknowledged.
- **Detection and recovery holds.** Orka detects missing or conflicting
  records at three levels. At the dataset level, a store's epoch is below its
  Kubernetes acknowledgement, or a periodic write watermark is below the value
  Kubernetes recorded, which also catches a restore from an older copy with
  the same identity. At the record level, a Kubernetes record requires a
  store record that is missing or different, such as a settling prompt
  attempt without its result receipt. At the operation level, an outcome
  cannot be settled. A dataset-level signal places a recovery hold on the
  whole installation; a record-level signal holds the affected Task or Session
  with a `StorageRecoveryRequired` condition. Held work makes no side effects,
  is never retried as a new operation and never reports success. An operator
  releases a hold explicitly after recovery.
- **Store halves of the Kubernetes control protocols.** Session turn
  creation, finalization and projection activation, outbox enqueue, claim and
  completion, Session cleanup preparation and completion, prompt result
  receipts and gateway Task cleanup receipts are store writes inside protocols
  that Kubernetes drives. Each commits only under the current fence, so a
  delayed step from a replaced controller is refused. A step whose response
  was lost is retried safely through its operation receipt. A step run again
  by a successor controller as a new operation is safe only where the
  catalog names a natural key for it or the step is idempotent by the state
  it changes, as an outbox claim or a receipt deletion is; Session cleanup
  preparation has no natural key, because after completion it records a new
  intent. Session turn creation and finalization are recognized by the turn
  key, and projection activation is idempotent by the state it changes. SQLite
  also checks the fence each Session turn step carries against the dataset's
  controller epoch when it commits.
- **Execution event dedupe keys.** A repeated dedupe key returns the stored
  event only for a request with the same Task, Session and tool call;
  anything else returns `ErrDuplicateMismatch`, so a reused key is not
  dropped silently. A field left empty on either side matches. The key is
  scoped to the stream, so the stream needs no comparison. The type, content and summary may differ: competing outcomes
  of one step, such as a completion and a failure, share one key so the first
  durable outcome wins, and an event derived again after a restart may be
  redacted differently.

### Installation binding

Each dataset records the one installation that owns it, written once, so a
second installation can never claim a dataset another one bound and a dataset
the installation has left refuses writes (#512 step 5). The types are in
`pkg/storage/v1`: `DatasetBindingStore`, `DatasetBinding`, `BindMode`, the
request types, `BindingConflictError` and `ErrDatasetDetached`. Both providers
implement it, `Describe` reports the binding, and the shared suite checks it.
Orka calls only `CompleteDatasetBootstrap` so far, at SQLite startup, and does
not bind datasets yet: the Kubernetes installation record that names each
dataset, the create-once startup protocol and the checks on every restart come
later.

- **State.** A dataset is unbound, bound or detached. `BindDataset` records the
  installation ID, the UID of the installation record that bound it, the
  storage switch generation and the snapshot key fingerprint (an equality-only
  identifier Orka derives from the key; never key material). The first
  controller epoch advance on an unbound dataset records its installation too,
  without a record, at switch generation 0, so a dataset an installation
  already advanced stays that installation's and is bound by it later.
- **Create once.** Moving a dataset from unbound to bound is atomic and
  linearizable with every other call on it. A repeat with the same
  installation, record UID, generation and key fingerprint returns the stored
  binding, whatever the mode. A dataset that belongs to another installation
  returns a `BindingConflictError`, which matches `ErrConflict` and carries the
  stored binding, so the caller can tell another installation, another record
  and a detached dataset apart without another read.
- **Modes.** `fresh` needs a new dataset (origin `new`) holding no record of
  any kind, including control records such as a controller epoch, whose
  bootstrap has not completed; a dataset an advance has reached is never
  fresh. `embedded` is accepted only by an embedded
  provider that opened the dataset under its process lock, in any bootstrap
  state; others return `ErrValidation`. `migrated` needs an imported dataset
  whose import finished. Those three need the dataset unclaimed: unbound, or
  recorded only by the same installation's advance. `adopt` also accepts a
  dataset the same installation detached at a lower generation, and `rebind`
  moves a dataset the installation already owns (bound, detached or recorded
  by its advance) to a higher generation. The generation never moves backward for a dataset.
- **Detach.** `DetachDatasetBinding` records that the installation left the
  dataset at a higher generation, keeping its installation and key
  fingerprint. A detached dataset refuses every write that carries an
  envelope, and the controller epoch advance, with `ErrDatasetDetached`.
  Reads, replays of committed operations, `ResolveOperation` and receipt
  pruning still work, because none of them can make a new change, and only
  `adopt` or `rebind` binds it again.
- **Key fingerprint.** `UpdateBindingKeyFingerprint` is a compare-and-swap on
  a bound dataset's fingerprint, for a key rotation Orka has already proved by
  authenticating the dataset's sealed records under the new key.
- **Bootstrap.** `CompleteDatasetBootstrap` records that the dataset finished
  its first start, and `Describe` reports it in `Bootstrap.Complete`. It
  never reverts, so a completed dataset is never bound `fresh` and never
  receives a newly minted key. SQLite startup calls it with the dataset ID
  once the storage client has activated the snapshot key.
- **Authority.** The binding writes are administrative and `GetDatasetBinding`
  is a read: no envelope and no fence. `BindDataset` establishes identity
  before any fence exists. A delayed detach cannot undo a later change,
  because the switch generation only increases. The key fingerprint changes
  by compare-and-swap on the fingerprint itself, so a delayed key update is
  refused only while the binding does not hold that update's expected
  fingerprint again; a caller therefore never returns a binding to a
  fingerprint it held before under the same record and generation, and
  rotates to a new key or rebinds at a higher generation instead. A later
  layer replaces the fingerprint comparison with a key revision before any
  caller rotates keys. Binding a
  dataset records its installation as the advance does, so the advance then
  accepts only that installation and `SyncControllerEpochMirror` is refused.
- **SQLite.** The binding lives in the `store_dataset` row (schema version 3 and later).
  Every binding method runs in one transaction that reserves the writer, and a
  successful call checkpoints the WAL before it returns, as dataset identity
  does. A checkpoint that fails after the commit returns `ErrOutcomeUnknown`,
  and repeating the identical request synchronizes again. A row whose binding columns contradict one another is refused when the
  store opens, never repaired.

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
  and most record times; the storage client fills the creation times of
  messages, transcript messages, memories, memory proposals and dropped
  findings. The provider still assigns some informational times: artifact
  creation times, Task result timestamps, a plan's creation and update times
  (`SavePlan` ignores the caller's), and in SQLite the repository monitor,
  security and usage times it fills when a caller leaves them zero or always
  sets, such as in `UpsertRepositoryMonitor` and `CancelWorkActions`. None of
  them decides a lease (see "Lease clocks" above); moving them to the client
  follows the same pattern.
- **Bounded lists.** `ListSessions`, `ListSessionCleanupIntents` and
  `ListActiveScanRuns` return
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
- **Change feed.** While one controller is the only writer, the storage
  client knows every committed change, so pollers can be woken in process and
  a poll budget can bound what they read. Standby controllers or writers
  outside Orka would need a contract-level change feed instead. Its reserved
  shape is a read, `ListChanges(ctx, ChangesRequest{AfterCursor, Families,
  Limit})`, that returns the keys changed after an opaque, provider-issued
  cursor in commit order, under the same visibility rule as
  provider-assigned sequences. It is not part of the contract yet.

`cmd/main.go` builds one `internal/storageclient.Client` over the SQLite store,
activates the snapshot key on it, and passes it to every application consumer.
Outside the client, it uses the SQLite store directly only for its process lock,
bootstrap history, configuration and manager lifecycle. The client composes
the state and artifact providers. Every application family is part of the
provider contract, so no family remains embedded and `storageclient.Embedded`
has no methods.

State and artifact records live in the same SQLite database, and fenced
artifact writes rely on the cleanup generations held with state, so the
client refuses a configuration that would place them on different stores.
That sharing is an embedded-store behavior, not a provider
obligation; see "No cross-contract transactions" above.

`pkg/storage/v1/storagetest` is the reusable behavioral suite. `RunState` and
`RunArtifacts` exercise each provider family through the public interfaces.
The suite attaches a write envelope to every write it sends without one,
naming the dataset `Describe` reports and the epoch the dataset holds, so a
provider that refuses writes without an envelope passes it; the cases that
test envelopes send their own. The SQLite provider runs it directly, refusing
writes without an envelope as in production, and the storage client runs it
over SQLite with `ProviderAttachesEnvelopes`, so the client attaches the
envelopes.

`internal/storageclient/storagefault` injects storage faults for Orka's own
tests. It wraps a state provider, an artifact provider or the embedded store
(`NewEmbeddedClient` builds a complete storage client over one), so a test
can take state or artifact storage down and bring it back separately, fail
chosen methods or kinds with any error (such as `ErrUnauthenticated` or
`ErrNotReady`), delay calls, hold them until released, let a write commit
and then lose its response (`ErrOutcomeUnknown`), or change what `Describe`
reports, such as a new incarnation or another dataset ID. It counts every
call it receives and every call that reached the provider. On the embedded
store, `Describe` and `HealthCheck` are state calls; a binding check for
artifacts wraps the same store as an artifact provider, which shares the
faults. Its wrappers are generated from the provider interfaces, and tests
fail when they are out of date or miss a catalog method the embedded store
serves.

SQLite also implements the duplicate SQL control-record methods described in
"Kubernetes control authority and retained SQL compatibility" below. They are
not part of the provider contract or the client. A client test fails if any
other SQLite method is missing from the client, so a consumer's type assertion
cannot silently lose a capability.

### Orka policy above the client

The storage client applies Orka's redaction, sanitization and usage
attribution before a provider sees the data, so a provider, including the
embedded SQLite store, stores only content Orka has already made safe to
persist:

- **Execution events.** The client redacts and bounds each event's summary,
  text and JSON content and stamps the creation time when the caller left it
  unset. A provider only requires structured content to be valid JSON, and
  stores content that is only whitespace as no content.
- **Execution-event metrics.** The client records the execution-event store
  metrics for every provider, with the names and labels the SQLite store
  used: appends (`orka_execution_events_appended_total`,
  `orka_execution_event_append_failures_total` and
  `orka_execution_event_append_duration_seconds`), store reads
  (`orka_execution_event_list_requests_total` and
  `orka_execution_event_list_duration_seconds` with `scope` `task_store` or
  `session_store`), and redaction and truncation once for each newly
  appended event. An event the client refuses before calling the provider
  counts as an append failure, as does a provider error, and a deduplicated
  replay counts as an appended event. An invalid deduplication key or a
  missing plan is rejected before the provider is called and is not counted,
  as SQLite never counted it. A grouped append (`AppendExecutionEventsIfAbsent`)
  counts each of its events as one append attempt with the group's outcome and
  duration, because the provider commits the group in one transaction or not
  at all: a refusal by the client or the provider counts every event of the
  group as failed. A provider records none of these
  metrics, so a provider in another process still reports them in Orka's
  registry and nothing is counted twice. The append duration runs from the
  start of the client call, so it includes Orka's sanitization and, for a
  remote provider, network time. The embedded SQLite store still records
  `orka_store_db_size_bytes` from its database file.
- **Usage from execution events.** The client derives the usage observation
  that a worker usage report or the controller's ACP journal carries and
  sends it as `ExecutionEvent.Usage`, unless Orka code attached an
  observation itself. The provider records it in the event's
  transaction, as `RecordUsage` would, and records nothing when a
  deduplication key returns an existing event. A worker report keeps the
  time it was first received (`KeepFirstObservedAt`). Attribution travels in
  the context (`store.WithUsageAttribution`): the authenticated Task for a
  worker report and the frozen profile model for the journal. It never
  reaches a provider. Invalid journal accounting is dropped without failing
  the append; an invalid worker report fails it, as before.
- **Usage names.** The client redacts and bounds provider and model names in
  `RecordUsage` and in derived observations.
- **Token attribution.** A provider returns the observations of every counter
  a Task observed (`ListTaskCounterObservations`), in accounting order. The
  client applies the usage report's attribution rules for `TaskTokenUsage`
  and for the prompt attempt a Session turn finalizes, and passes that
  attempt's totals in `CommitSessionTurnFinalizationRequest`. The client reads
  the observations just before the finalization commits rather than inside
  its transaction, so usage recorded for the attempt in that interval is not
  added to the Session's counters, as usage recorded after finalization never
  was.
- **Threat models.** The client sanitizes threat model content before
  `SaveThreatModel` and `SaveThreatModelIfVersion`, including threat models a
  scan ingestion records for its commit.
- **Gateway text.** The client redacts, trims and bounds the text Orka and
  adapters attach to gateway records: an event's state message, sender
  display name and metadata (also when it looks up a duplicate), expiry, retry
  and dead-letter reasons, a delivery's last error, provider message ID and
  metadata, and the metadata of a terminal transcript message. It also redacts
  the text of a message enqueued for a Task (`EnqueueGatewayMessage`), and
  refuses text over the interim text limit or not valid UTF-8 instead of
  truncating it, as the Gateway service does. Gateway
  identifiers that the protocol already bounds are stored as received, now
  including the copies in a gateway user message's transcript metadata.
- **Memory.** The client redacts memory content, every free-text proposal
  field, review fields and the applier. Proposal application builds the new
  memory from the stored proposal in Orka and applies the redaction rule
  again, so a proposal stored under an older rule cannot become a memory
  unredacted; the provider receives the redacted memory.

Two tests in `internal/storageclient` send secrets through these paths over a
recording SQLite provider and fail if the provider receives any of them:
`TestProviderReceivesOnlySanitizedGatewayText` covers the gateway writes and
`TestProviderReceivesOnlyRedactedContent` covers the others.
`TestClientRecordsTheStoreMetricsSQLiteRecorded` runs a fixed append and
list workload through the client and requires the series and counts that
the SQLite store recorded for it before the client took over.

### Key custody and sealing

The storage client owns the 32-byte snapshot key. Startup loads the key,
calls `Client.ActivateSnapshotKey`, and only then records bootstrap completion
and hands the client to consumers. Activation pages through every retained
sealed snapshot and native session record
(`ListSealedAgentExecutionSnapshots`, `ListSealedNativeSessions`) and
authenticates each one with the candidate key (native records one at a time,
since each can hold a large bundle). It then pages through connector custody
(`ListSealedConnectorCredentials`, `ListSealedRetiredConnectorCredentials`,
`ListSealedConnectorConsents`, `ListSealedConnectorCompletions`) and
authenticates the data keys of current and retired custody, the verifiers of
unexpired pending consents and the payloads of parked completions, so linked
accounts stay usable and revocable. An expired consent holds no token and does
not block a key. A key that cannot open retained data is rejected and not
installed, so one dataset never holds records sealed under different keys.
Until activation succeeds, every sealed operation fails closed with
`ErrSnapshotKeyNotActive`.

The sealing formats live in `internal/storageclient` and are the formats the
embedded store wrote before, so existing datasets open unchanged:

- An execution snapshot body is sealed with AES-256-GCM under a fresh random
  nonce, with associated data
  `orka.agent-execution-snapshot\x00<task UID>\x00<digest>\x00<schema version>`.
  On read the client also checks the body against the digest in its key.
- A native session record is sealed as its whole JSON encoding, with
  associated data
  `orka.native-session.v1\x00<namespace>\x00<Session name>\x00<owner UID>\x00<record digest>`,
  where the record digest is the canonical digest of that encoding. The
  provider stores the owner UID, record and bundle digests, source operation,
  runtime generation and transcript boundary in plain fields so it can enforce
  ownership, boundaries and monotonic history without opening the record. The
  client checks each of those fields against the plaintext when it opens a
  record.
- Connector custody (current and retired) is sealed as JSON under a fresh
  32-byte data key per record, with associated data
  `orka.connector-credential\x00<Connection UID>\x00<namespace>\x00<name>\x00<subject digest>\x00<provider>`.
  The data key is sealed under the snapshot key with associated data
  `orka.connector-data-key\x00<Connection UID>`. The provider stores the grant
  sequence beside the sealed bytes; the client checks it against the sealed
  grant.
- A pending consent seals its PKCE verifier with associated data binding
  every plain field: `orka.connector-consent`, nonce, Connection UID,
  namespace, name, subject digest, provider, mode, authority and revocation
  digests, space-joined scopes, expiry in Unix nanoseconds and consent
  sequence.
- A parked completion seals its tokens, the consent authority and the
  committed marker as JSON, with associated data binding every plain field:
  `orka.connector-completion`, nonce, Connection UID, subject digest,
  namespace, name, provider, mode, expiry in Unix nanoseconds and consent
  sequence.

Providers see only sealed records and the conditional commands of
`state.v1`: `CreateSealedAgentExecutionSnapshot` returns `ErrConflict` for an
existing key and the client then compares the stored snapshot, so a repeated
persist succeeds and different content returns `ErrDuplicateMismatch`.
`SaveSealedNativeSession` and `StageSealedNativeSessionImport` carry the
request digest the client computed from the plaintext.

A turn finalization that carries native state is read, decided and sealed in
the client, then committed conditionally. The embedded store used to append
the terminal messages with IDs it generated and seal the native record against
the boundary it derived, which needs the key inside the transaction. Now the
client reads the turn, the Session's transcript boundary and its stored native
record (`ReadSessionTurnFinalizationBasis`), assigns the IDs of the messages
the finalization appends, and seals the record against the boundary after
them; a carried checkpoint is opened and compared with the stored one first.
`CommitSealedSessionTurnFinalization` commits only if the transcript still has
the boundary the client read and, for a carried checkpoint, the stored record
is the one it opened; otherwise it returns `ErrConflict` and changes nothing.
A finalization without native state appends at the current end of the
transcript, as before. When the turn is already finalized, the client sends no
native state and the provider only compares the recorded finalization.

Connector writes are read, decided and sealed in the client, then committed
conditionally, as turn finalization is. The embedded store used to decide
several of them inside its transaction from plaintext: it opened a parked
completion to read its sealed committed marker, opened the custody a commit
or refresh replaced to compare tokens before retiring it, and assigned grant
and consent sequences that it then sealed into the record. Now the client
reads the Connection's custody (`ReadConnectorCustody`: tombstone, current
and retired custody, parked completions and the grant and consent counters
from one snapshot), opens what it needs, decides and seals, and sends a
command that names what it read:

- `PutSealedConnectorCredential` and `CommitSealedConnectorCompletion` carry
  the next grant sequence, and `CreateSealedConnectorConsent` the next
  consent sequence; another grant or consent since the read returns
  `ErrConflict`.
- `ReplaceSealedConnectorCredential` and `CommitSealedConnectorCompletion`
  carry the custody version the client opened, and say whether that custody
  is retired and until when. The provider copies the stored sealed bytes into
  retirement; it never compares tokens.
- `CreateSealedConnectorCompletion` and `CommitSealedConnectorCompletion`
  carry every parked completion of the Connection as read, each with the
  `SealedValueDigest` of its payload, and the uncommitted ones the client
  chose to replace or drop. The provider commits only if the Connection's
  completions are still exactly those.

A refused command changes nothing. For a grant, a consent or a completion the
client reads again, up to eight times, before it returns `ErrConflict`; a
replacement returns `ErrConflict` at once, because its caller chose the
version it replaces. The provider still enforces the rules
it can decide from plain fields: tombstones, consent order against the
committed consent, expiry at the request's time, pruning of retired material
with nothing left to revoke, and the bounds on retained consent and refresh
grants. Its commit refusals come in a fixed order (`ErrConflict`,
`ErrNotFound` for an expired completion, `ErrConnectorConsentSuperseded`,
`ErrConnectorRetiredLimit`, `ErrConnectorCustodyTombstoned`), so callers keep
the errors they branched on before. SQLite still truncates its write-ahead
log after deleting custody, so a deleted record's sealed bytes do not outlive
it in the log. The truncation runs after the write's operation commits, on a
replay too, and also removes the sealed results the removal cleared from
operation receipts.

SQLite's duplicate SQL control implementation, which tests use in place of
the Kubernetes control store, finalizes turns itself and seals native state through the client's
`store.NativeSessionFinalizationSealer`; production never installs it.

`TestProviderReceivesOnlySealedSnapshotsAndNativeSessions` records every
provider call while snapshots, imports, captures and turn finalizations with
captured and carried native state go through the client, and fails if the
provider receives a snapshot body, a native bundle in raw or base64 form, or
the key. `TestProviderReceivesOnlySealedConnectorCustody` does the same for
every connector path, including key activation, and fails if the provider
receives a token, a PKCE verifier, a per-record data key or the snapshot key
in raw, hex or base64 form. `TestSealedFormatsMatchStoredRecords` seals
records with an independent implementation of the stored formats and reads
them through the client, and the reverse; `TestSealedConnectorFormatsMatchStoredRecords`
does the same for every connector format, and
`TestConnectorCustodySealedByEarlierReleaseOpensUnchanged` opens connector
custody that the embedded store sealed before this change
(`internal/store/sqlite/testdata/connector-custody.sql`). The suite's
`sealed-snapshots`, `sealed-native-sessions`, `session-turn-records` and
`sealed-connector-*` families check that a provider stores opaque bytes
exactly and decides each conditional commit from the plain fields and the
expectations Orka sends.

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

Loading rejects more than one YAML document, field names in another case,
unknown and duplicate fields, a binding that names no adapter,
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

### Envelopes in the storage client

Below the policy layer and the sealing step, so a digest covers exactly what
the provider receives, the client runs every write through generated
decorators of the two providers (`internal/storageclient/envelope_gen.go`, written by
`go generate ./internal/storageclient` from the provider interfaces and the
method catalog; `make generate` runs it and a test fails if it is stale).
Reads and administrative methods reach the provider unchanged.

- **Fence.** A write first waits for the fence of the dataset that serves its
  contract. The controller epoch manager is the fence source: it opens a
  contract's fence once that contract's dataset has advanced to the acquired
  epoch. A write waits up to ten seconds for a fence source to be bound and
  for that fence to open, and is then refused with `ErrNotReady`.
- **Envelope.** The client creates one operation per write: the dataset ID
  and fence the fence source gives, a new UUIDv7 operation ID, the digest of
  the arguments as they are when the write is issued, the issue time and the
  next sequence. A method that takes its own fence, such as the outbox
  family, carries that fence in its envelope, so a caller holding a replaced
  fence is still refused with `ErrFenced`. A context that already carries an
  envelope belongs to a caller that owns the operation, such as the shared
  suite, and the write runs once with it. That caller owns the request too:
  the client sends it unchanged and assigns no identity or time, so a caller
  that supplies a write envelope must stamp identity and time before
  computing its digest, and the client refuses a request that lacks them
  with `ErrValidation`. The same holds for what the client would redact,
  bound or otherwise prepare (memory and proposal text, gateway text and
  metadata, threat models, usage names and execution events): a caller that
  supplies a write envelope must send an already redacted and truncated
  request, the client refuses one it would change, and one it would not
  change is sent as it is. No production caller supplies its own envelope.
- **Retries.** After `ErrUnavailable` (nothing committed) or
  `ErrOutcomeUnknown` (it may have), the client retries the same envelope with
  the same arguments, with backoff, for at most thirty seconds after that
  first failure, and never once the operation is `MaxOperationAge` old, less
  `MaxClockSkew`. Every retry runs under that bound, so a provider call that
  stalls cannot hold the write past it; a retry the bound ends in flight may
  have committed and goes to resolution. When the call's deadline comes
  first, it ends the retries instead. A retry it ends returns the context
  error without resolution, as any call its caller abandons does, so its
  outcome is unknown to the caller, unless an earlier attempt's outcome was
  already unknown: the client then resolves the operation, as above. The
  first attempt runs under the call's deadline alone, so a long write is not
  cut short. A replay of a committed operation returns its original result.
  `ErrClockSkew` is returned as `ErrNotReady`.
- **Resolution.** When retrying cannot settle an unknown outcome, or a later
  attempt is refused, the client calls `ResolveOperation` once. A committed
  operation is attempted once more to read its result as a replay. Any
  refusal of that replay, including a moved Task data fence or a sealed
  connector result cleared when its custody was removed, leaves the result
  unreadable and the write returns `ErrOutcomeUnknown`. Task data access
  preserves that uncertainty even if a handler translates the error, and
  never substitutes a cleanup refusal that would trigger reauthorization
  and a new operation. The API returns 409 without a retry hint. An aborted one
  returns `ErrOperationAborted`, joined with the refusal when a
  refusal ended the attempts, or with `ErrUnavailable` when only transient
  failures did, because nothing committed and running it again is safe. An
  error that leaves the outcome unknown matches only `ErrOutcomeUnknown`. An
  operation the client cannot settle, because the store is
  unreachable, returns `ErrOutcomeUnknown` and joins the dataset's unresolved
  set: new writes to that dataset are refused with `ErrNotReady` while the
  client keeps resolving the set in the background. An operation that
  `ResolveOperation` finds too old to settle leaves the set with its outcome
  permanently unknown; holding the affected work for recovery comes with the
  recovery holds.
- **Sealed writes and callback protocols.** The client seals a record, or
  computes a conditional command, once per write, and every retry resends the
  same request with the same envelope. A seal uses a fresh nonce, so a
  resealed retry would have another digest and be refused with
  `ErrDuplicateMismatch`. A retry the client makes after its own conflict,
  such as a connector custody commit that lost a race, seals again and is a
  new operation.
- **Identities and clocks.** Above the decorators, the client fills the IDs
  and creation times described in "Client-assigned identities and times",
  and the write's envelope is issued at that same time. While the last clock
  check found a dataset's clock more than `MaxClockSkew` away (see "Provider
  clock and skew"), new writes to it are refused with `ErrNotReady`.
- **Callers.** `store.IsTemporaryStorageError` matches `ErrNotReady`,
  `ErrUnavailable` and `ErrOutcomeUnknown`. The API answers `ErrNotReady` and
  `ErrUnavailable` with 503 and a `Retry-After` hint, in its shared error
  handler, the gateway ingress,
  gateway store and internal task-data paths, the worker message and
  execution event routes, and the memory routes. `ErrOutcomeUnknown` answers
  409 without `Retry-After`: HTTP retries do not preserve the storage operation
  ID, so resubmitting may consume a new message batch or create another
  provider-assigned record. The response tells callers not to retry this
  request; HTTP result recovery requires a future request-level idempotency
  protocol. Task result collection
  requeues the Task instead of completing it without its result, repository
  monitor implementations retry an artifact the store has not accepted
  instead of blocking, and gateway admission no longer reads a fenced write
  as a Session ownership conflict.
  The SQLite in-process store never returns `ErrUnavailable` or
  `ErrOutcomeUnknown`; those paths matter once a provider runs out of process.

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

## Conditional commands

Provider methods take no Go callbacks. Each operation that used to run an
Orka callback inside a database transaction is now a protocol with the same
three steps: the provider resolves the target without returning its content,
Orka authorizes or computes with no provider transaction open, and a
conditional command applies only while the target and its version still
match. A changed target or version returns `ErrPreconditionFailed` (which
also matches `ErrConflict`) with nothing applied or disclosed. The storage
client runs Orka's side, so callers keep their interfaces. The shared suite
covers each protocol, including competing callers. The sealed families' read,
decide and commit steps (turn finalization with native state and connector
custody, described in "Key custody and sealing") follow the same shape but
report a changed target with `ErrConflict`.

| Operation | Protocol |
| --- | --- |
| Memory proposal application (`ApplyMemoryProposal`, `AuthorizeExistingMemory`) | `ResolveMemoryProposalApplication` returns the memory ID the application would return (the applied memory, or one already created from the proposal) and the proposal status and applied ID it read, but no memory content. Orka authorizes that exact memory for an external caller; a new memory is covered by the caller's create permission, and Orka builds and redacts it from the stored proposal. `CommitMemoryProposalApplication` applies only if the proposal still resolves to the same target. Competing applications create one memory: the losers' retries resolve, authorize and return the winner's memory. |
| Task data access (`WithAuthorizedTaskDataAccess`, `WithTaskDataAccess`, which replace the task-data transaction methods) | `ResolveTaskDataFence` returns the namespace or Task cleanup generation. A Task is registered (`RegisterTaskDataFence`) only after a first successful authorization, and the registration is released when the access it was created for fails. Orka authorizes against live Kubernetes state, then sends the fence, with the authorized Job identities, in the context of each data call. Trusted controller access (`WithTaskDataAccess`) registers a Task only for the duration of the access, so it leaves nothing behind for a Task that may already be finalized. Every fence-checking method listed on `TaskDataFenceStore` refuses a moved generation with `ErrTaskDataCleanupChanged` or a revoked Job with `ErrTaskJobRevoked` in the same transaction as its own reads and writes. The calls of one access are separate operations: each either completes before an invalidating cleanup, which then removes its effect, or is refused, and callers retry with fresh authorization. A definitely uncommitted refusal that a caller turned into its own error is still reported as the fence error. An uncertain write or an unreadable committed result remains `ErrOutcomeUnknown`, even after handler error translation or a joined cleanup error, and is never automatically repeated under fresh authorization. A parent's request to cancel a child first commits a deduplicated `TaskCancellationRequested` execution event, bound to both Task UIDs, under the parent Job and cleanup fence. That commit orders cancellation against revocation: revocation before the intent refuses the action, while an intent committed first may finish publishing its Kubernetes annotation after revocation. Publication retains the child's UID and resourceVersion preconditions and never holds a provider transaction open. Evidence that must not be recorded in part, such as a recovered approval decision and its execution outcome, uses one grouped append (`AppendExecutionEventsIfAbsent`). |
| Scan Task ingestion (`SecurityStore.ApplyScanTaskIngestion`) | Orka reads the receipt and the run, validates the live RepositoryScan identity before applying and again before committing, and runs the application logic against a store that records its reads, with the results it saw, and its writes. A read of records an earlier recorded write may have changed is answered by `PreviewScanTaskIngestion`, which runs the steps so far with the provider's own write semantics and keeps nothing. `CommitScanTaskIngestion` applies the writes, the run update and the receipt in one transaction only while the stored run equals the run Orka read and every recorded read still returns what Orka saw, compared field by field with an empty list equal to an absent one and times compared as instants (`ScanRunsEqual`, `ScanIngestionReadsEqual`); a step's provider-assigned times come from the commit's time, so a preview and its commit agree. A changed run or read returns `ErrPreconditionFailed` with nothing changed, and the reconciler repeats the ingestion from fresh state. |

Two callback-shaped interfaces stay in Orka because they are not storage
provider operations:

| Operation | Why it stays in Orka |
| --- | --- |
| `ControllerEpochMutationStore.WithControllerEpochMutation` | A Kubernetes Lease interlock around a short Kubernetes mutation, implemented by the Kubernetes control store. It is not a cross-resource transaction; UID/resourceVersion checks and ambiguous-outcome handling remain necessary. |
| `SessionRuntimeCleanupFunc` / `kube.WithSessionRuntimeCleanup` | The local dispatcher retires the exact resident runtime after the durable cleanup intent fences new work, before authority deletion. It runs outside the epoch mutation lock, and mutations and short receipt writes revalidate their fences. |

### SQLite-only behavior at the seam

The contract and the storage client contain no SQL. Some SQLite behavior is
not a contract obligation, and a remote backend may differ:

- The embedded store sets `PRAGMA secure_delete=ON`, so deleted content is
  overwritten in the database file. The contract does not require it: a
  remote backend may keep deleted bytes in its storage, logs or backups until
  its own retention removes them.
- Monitor and security list cursors are SQLite offsets; see "Rules reserved
  for later steps".
- `UpdateScanRun` decides whether a newer run of the same RepositoryScan was
  admitted by `rowid` order. The contract does not yet define that admission
  order for other backends.

Typed turn finalization, deferred outbox activation, gateway projection,
deduplicated event/plan append, native-session finalization and cleanup intents
already express grouped workflows. Future work should preserve these guarantees
before introducing new protocol names, lost-response receipts, transport or
backend selection. Encryption/sealing and memory protections must remain owned
by Orka above an eventual external adapter; the storage client already owns
snapshot, native session and connector custody sealing, and their stored
formats are unchanged.
