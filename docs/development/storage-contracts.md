# Storage ownership and bootstrap

This inventory describes the production wiring in `cmd/main.go` for
[#512](https://github.com/orka-agents/orka/issues/512). Application consumers
reach storage through one storage client over the public provider contract
described below. SQLite remains the only application database and artifact
store, with one active controller; there is no network adapter or backend
selection.

## Dataset metadata

`store_dataset` contains exactly one canonical, nonzero UUID, an origin (`new` or
`adopted`), a bootstrap-completion flag and the installation the dataset is
bound to, empty until the first controller epoch advance.
`PRAGMA user_version = 2` identifies this metadata layout. Opening a database
still validates every table and index definition: a version marker alone cannot
establish compatibility.

Fresh schema creation, identity creation and version recording commit together.
Exact supported historical layouts are backed up and upgraded under the existing
process lock and one `BEGIN IMMEDIATE` transaction. The 167-to-168 transition
adds the metadata table, the 168-to-169 transition adds operation receipts and
the epoch advance columns, and the upgrade records an adopted identity. A store
at an earlier schema version upgrades through numbered version steps under the
same lock, backup and transaction: a version-1 store (the 159-object layout)
gains the 168-to-169 objects and keeps its identity row, records and bootstrap
state unchanged. Missing or malformed
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
  events, gateway records, security records, repository monitors, usage, and
  memories with their proposals (creation, review and archiving; applying a
  proposal is still an embedded family).
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
- **Durability.** A nil error means the change and its operation receipt,
  which commit together, survive what the provider's `DurabilityClass`
  promises: a provider restart, and any failover the provider declares
  automatic. `Describe` reports the class so Orka can refuse automatic
  continuation after a failover that may lose acknowledged writes (see
  "Durability, switch detection and recovery holds" below). The embedded
  SQLite store runs in WAL mode. By default it uses `synchronous=NORMAL` and
  reports `local-buffered`: a commit survives a process restart, but the most
  recent commits can be lost if the host fails before the WAL is synced. With
  `--store-sqlite-synchronous=full` (chart value `store.sqliteSynchronous`) it
  syncs every commit and reports `local-fsync`; `Describe` reads the mode the
  connection actually uses.
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
  and the durability class. It also reports the dataset's write state
  (`ready`, `migrating` or `importing`), an `Incarnation` that changes when
  the serving database may have restarted or switched and stays the same
  otherwise, the provider clock when `Describe` ran (`ObservedAt`), and the
  operation receipt retention and result limit. The controller epoch manager
  reads the dataset ID, state, incarnation and durability class at takeover
  and once a minute; startup validation (step 4), installation binding (step
  5) and migration build on it. SQLite reports `ready` (it upgrades its
  schema while it opens, under its process lock) and an incarnation that is
  new each time the store opens; `ObservedAt` stays empty until providers
  report it.
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
`OperationReceiptStore`, `ControllerEpochAdvancer`, `WriteWatermarkStore`,
the errors `ErrDatasetMismatch`, `ErrOperationExpired`, `ErrOperationAborted`
and `ErrClockSkew`, and the new `ProviderInfo` fields. Both providers implement
`OperationReceiptStore`, `ControllerEpochAdvancer` and `WriteWatermarkStore`,
and check every write when it commits. SQLite does, and the shared suite's
write catalog (`storagetest`) runs the dataset, receipt, age, fence and
resolution checks for every write method except `CompleteScanTaskIngestion`,
which joins the catalog when scan ingestion becomes a conditional command.
The storage client attaches an envelope to every write (see "Envelopes in the
storage client" below), and a provider refuses a write without one with
`ErrValidation`. Production SQLite does; SQLite's package tests may still
write without one.

- **Method catalog.** Every provider method is a read, a write or an
  administrative method. Reads carry no envelope and need no stored receipt.
  Every write carries an envelope and leaves an operation receipt.
  `AdvanceControllerEpoch`, `ResolveOperation`, `PruneOperationReceipts` and,
  until it is replaced, `SyncControllerEpochMirror` are administrative: they
  carry no envelope and are not checked against the current fence.
  `GetMessages` counts as a write because it can mark messages read, until it
  is split into a plain read and a consuming write, and so does `IsLocked`,
  because SQLite clears an expired Task lock when it finds one.
  `AdvanceWriteWatermark` is a write that both contract parts serve, so its
  catalog entry names no contract; it carries the envelope of the dataset
  that serves the provider it is called on. A contract
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
     retry reads the original result even under a stale fence and changes
     nothing. A different method or digest returns `ErrDuplicateMismatch`, and
     an aborted operation returns `ErrOperationAborted`;
  3. the operation's age: `IssuedAt` more than `MaxOperationAge` (15 minutes)
     before the provider clock returns `ErrOperationExpired`, and more than
     `MaxClockSkew` (30 seconds) after it returns `ErrClockSkew`;
  4. the dataset state: anything but `ready` returns `ErrNotReady`;
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
  receipt never holds plaintext protected data. A provider keeps receipts for
  at least `MinReceiptRetention` (1 hour) on its own clock, which is longer
  than `MaxOperationAge` plus `MaxClockSkew`, so every retry it can still
  accept finds its receipt. `PruneOperationReceipts` removes older receipts in
  bounded batches, and `Describe` reports the retention and the result limit.
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
  turns a receipt pruned earlier into a false abort. While an outcome
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
  neither success nor failure: an API handler answers 503 and a reconciler
  requeues, and neither repeats the effect nor reports success. An operation
  ID names one operation instance and is never derived from a durable
  identity, so an aborted instance does not block a later one. A durable
  operation run again after a restart is a new operation. Its natural key, or
  for a write such as a delete the state it changes, makes that safe; a write
  with neither, such as a send or a counter update, must not be run again
  unless the caller can observe whether the first one committed.
- **Messages, counters and chat turns.** Receipts make these retries safe: a
  replayed `SendMessage` returns the assigned ID without a second message, a
  replayed counter update adds nothing, a replayed chat-turn reservation or
  commit returns its original outcome, and a replayed consuming message read
  returns the same messages, which are marked read by ID. Consuming reads are
  bounded per call. Before computing the digest, the client assigns transcript
  message, memory and finding IDs and fills zero record times from
  `IssuedAt`, so every retry sends the same request.
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
  and forward jumps are allowed. A refused advance commits nothing. A dataset
  that holds an epoch but no installation binds the installation of its first
  successful advance, including a repeat of the stored epoch. The advance
  waits for fenced writes in progress, and once it returns, no write with an
  older fence commits, however long it was delayed. At takeover Orka
  acquires the epoch in Kubernetes and then, for each storage contract:
  1. stops for recovery if the contract is served by another dataset than the
     one that acknowledged it before, because that is a storage switch;
  2. reads the epoch and write watermark the dataset holds, once per distinct
     dataset;
  3. stops for recovery if it holds a lower epoch or watermark than any
     contract acknowledged on it, because the store lost acknowledged writes
     or was restored from an older copy. The dataset is advanced, to fence
     out the replaced controller, only after the hold that keeps this
     evidence is recorded;
  4. advances the dataset, once, with the exact record the compare-and-swap
     produced and the ControllerEpoch UID;
  5. records the acknowledgement in Kubernetes, in the ControllerEpoch status
     until an installation record exists;
  6. opens that contract's fence for writes.

  A dataset whose advance is refused stops the manager. "Durability, switch
  detection and recovery holds" below describes the record and the holds.

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
  its process lock as well. "Writer exclusivity and schema migration for
  adapters" below lists what a network provider does instead.
- **Lease clocks.** A provider decides a lease only by comparing times passed
  in the same request with stored times that came from requests. It never
  compares a caller's time with its own clock. Leases are therefore decided on
  the controller clock, and the commit-time fence makes an early reclaim after
  a takeover harmless. A request's times are fixed for the operation and
  covered by its digest, so a retry is judged at the original request time.
  The provider clock is used only for operation age and the clock-skew check,
  receipt and tombstone retention, `ObservedAt`, and informational times the
  provider assigns when a caller leaves them zero. Orka estimates the clock
  offset from `ObservedAt` and refuses writes while it exceeds
  `MaxClockSkew`. Gateway claims, outbox claims and gateway message admission
  already carry the request time and lease they need; chat turns, Session
  locks and `IsLocked` gain them.
- **Durability and lost writes.** A nil error means the change and its
  receipt survive what the provider's `DurabilityClass` promises, and a
  provider reports an `Incarnation` that changes when its database may have
  restarted or switched. Orka checks a dataset again after every change and
  holds the installation when the dataset lost acknowledged writes, or when
  an `async-replicated` dataset switched. See "Durability, switch detection
  and recovery holds" below.
- **Detection and recovery holds.** Orka detects missing or conflicting
  records at three levels. At the dataset level, a store's epoch is below its
  Kubernetes acknowledgement, or its write watermark is below the value
  Kubernetes recorded, which also catches a restore from an older copy with
  the same identity; such a signal places a recovery hold on the whole
  installation. At the operation level, an outcome cannot be settled, and the
  storage client refuses new writes to that dataset until it is. At the
  record level, a Kubernetes record requires a store record that is missing
  or different, such as a settling prompt attempt without its result receipt;
  such a signal holds only the affected Task, with a
  `StorageRecoveryRequired` condition (see "Record holds" below). Held work
  makes no side effects, is never retried as a new operation and never
  reports success. An operator releases a hold explicitly after recovery.
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
  intent. The Session turn steps are still an embedded family: SQLite checks
  the fence each of them carries against the dataset's controller epoch when
  it commits, and they gain operation receipts when they join `state.v1`.
  "Kubernetes control protocols" below lists each protocol step by step.
- **Execution event dedupe keys.** A repeated dedupe key returns the stored
  event only for a request with the same Task, Session and tool call;
  anything else returns `ErrDuplicateMismatch`, so a reused key is not
  dropped silently. A field left empty on either side matches. The key is
  scoped to the stream, so the stream needs no comparison. The type, content and summary may differ: competing outcomes
  of one step, such as a completion and a failure, share one key so the first
  durable outcome wins, and an event derived again after a restart may be
  redacted differently.

### Durability, switch detection and recovery holds

A nil error means the change and its operation receipt survive the failures
the provider's `DurabilityClass` promises. What Orka does after it sees the
serving database restart or switch depends on the class:

| Class | Provider or controller restart | Database restart | Host or disk loss | Automatic database switch | After a detected restart or switch, Orka |
| --- | --- | --- | --- | --- | --- |
| `synchronous-replicated` | Survives | Survives | Survives on the replica | Loses no acknowledged write | Checks the dataset again and continues |
| `local-fsync` | Survives | Survives | Survives a host crash, not a disk loss | None | Checks the dataset again and continues |
| `local-buffered` | Survives | Survives | A host crash may lose the latest acknowledged writes | None | Checks the dataset again and continues |
| `async-replicated` | Survives | Survives | May lose recent acknowledged writes | May lose acknowledged writes | Holds the installation until an operator acknowledges it |

**Settings behind each class.**

- SQLite: WAL with `synchronous=NORMAL`, the default, is `local-buffered`;
  `synchronous=FULL` is `local-fsync`. A sync on every commit can slow the
  single writer on network-attached volumes, so the default is unchanged.
- PostgreSQL: `synchronous-replicated` needs `fsync = on`,
  `full_page_writes = on`, `synchronous_commit = on` (or `remote_apply`) for
  every adapter transaction, which the adapter sets per transaction, and a
  synchronous standby (`synchronous_standby_names`, or a managed service's
  documented synchronous high availability). An adapter checks the settings
  it can see at startup (`SHOW synchronous_commit`, `fsync`,
  `full_page_writes`, `synchronous_standby_names`) and never declares
  `synchronous-replicated` when one of them fails. High availability its role
  cannot observe is operator-declared configuration.
- Azure Database for PostgreSQL flexible server with zone-redundant or
  same-zone high availability acknowledges a commit once the standby has
  persisted its log, which matches `synchronous_commit = on`, so it is
  `synchronous-replicated`. Its server admin is not a superuser and cannot see
  replication details, so the class follows the server's documented
  high-availability setting. A server without high availability (including
  Burstable) has no automatic switch and is `local-fsync`.
- CloudNativePG is `synchronous-replicated` with
  `.spec.postgresql.synchronous` and `dataDurability: required`, and
  `async-replicated` otherwise.
- Read replicas never serve `state.v1` reads. Point-in-time restore and
  geo-restore create a server with older content and the same dataset
  identity, so they are never an automatic path; the checks below hold the
  installation after one.
- Under synchronous replication, a commit can be visible to other sessions
  before the standby confirms it. An adapter therefore answers a replay or a
  `ResolveOperation` call from a transaction that writes, so its answer is
  never an outcome a failover could still lose.

**Write watermark.** `WriteWatermarkStore` gives every dataset a counter that
only fenced writes increase: `AdvanceWriteWatermark` adds one and returns the
new value, and `GetWriteWatermark` reads it. SQLite keeps it in the dataset's
identity row, so a copy of the file keeps the value it had when it was taken.

**What Kubernetes records.** The ControllerEpoch status holds, for each
storage contract, the dataset that serves it, the controller epoch that
dataset acknowledged and the highest write watermark it acknowledged
(`status.storageEpochs`). Orka writes an entry only after the dataset returned
success, never lowers an epoch or watermark for the same dataset, and refuses
every write to the record under a fence other than the epoch the object
holds. The status sync of a later epoch carries the record across. It moves
into the installation record once that exists.

**Checks.** At takeover, the epoch manager compares each dataset with the
record before it advances the dataset or opens the contract's fence (see
"Controller ownership" above). From then on, about once a minute, it
describes each dataset that has passed, without waiting for datasets that
are still unreachable. When the
dataset's incarnation changed, the manager closes the fences of the
contracts it serves until it has been checked again; once it is ready, the
manager checks that the dataset still holds this controller's epoch and at
least its acknowledged watermark, and holds the installation for an `async-replicated`
dataset whatever it holds. It then advances the watermark with a fenced
write through the storage client and records the new value once the dataset
returned it. A value that is not above the recorded one also means the
dataset lost writes. SQLite runs in the controller process, so its
incarnation changes only when the controller restarts, and a host crash
looks like a restart; the takeover checks against the recorded epoch and
watermark are what detect such a loss, and a restore from an older copy.

**Installation hold.** A check that fails closes every fence at once and
then records a hold in `status.storageRecovery`, with its ID, reason (`EpochRegressed`,
`WatermarkRegressed`, `DatasetChanged` or `UnsafeSwitch`), contract, dataset
and the acknowledged and stored values, and sets the `StorageRecoveryRequired`
condition. While it is recorded, every contract's write fence and
`CurrentFence` refuse with an error that matches `ErrNotReady` (and
`store.ErrStorageRecoveryRequired`), so the storage client refuses every
contract write, as well as memory proposal application and scan ingestion,
which wait for the state fence before their embedded transactions;
control-store changes stop, no Task Job is created and no Task reports
success; reads continue. The other embedded families (connector custody,
agent execution snapshots, native session imports and Session turn records)
do not wait for a fence yet and are still written while held. Task Jobs also
wait while any contract's fence is closed, so a restarted controller starts no
work before every dataset passed its takeover checks, and `CurrentFence` waits
whenever the state fence is closed. The `orka_storage_recovery_hold` metric is 1 with the reason.
A restarted controller reads the hold and stays held: it checks and opens
nothing, and only advances each dataset that still serves the contract it
acknowledged, so a replaced controller cannot write to it. A check after a
restart or switch that cannot read the dataset is not a pass: the fences of
the contracts the dataset serves stay closed until a later check finishes. An operator releases the hold,
after the recovery procedure, by setting the
`storage.orka.ai/recovery-acknowledged` annotation of the ControllerEpoch to
the hold's ID. The controller then advances every dataset, records what each
holds now as acknowledged and clears the hold in the same update; each
contract's fence opens again once its dataset passes the takeover checks
against the new record. An annotation naming another hold releases
nothing. A dataset that passed its own checks opens its fence without
waiting for the others, so another dataset's hold can close a fence that was
already open.

**Record holds.** A Kubernetes record that requires a store record the
store no longer holds, or holds with other content, holds only the affected
object. Recovery does this for a settling prompt attempt whose result
receipt is missing or conflicts (P5 in "Kubernetes control protocols"
below): the Task gets `StorageRecoveryRequired=True` with reason
`MissingRecord` or `ConflictingRecord`, and the message names the record,
its key, the digest Kubernetes recorded and a hold ID derived from them.
While held, the prompt is not run again, the attempt stays `Settling`, the
outcome is not guessed, success is not reported and nothing is deleted.
When the record is found again, after a restore, the condition becomes
`False` with reason `RecordFound` and recovery settles the Task from it. An
operator who cannot restore it sets the Task's
`storage.orka.ai/recovery-acknowledged` annotation to the hold ID; recovery
then records the outcome as unknown, as it did before record holds existed,
and the condition becomes `False` with reason `RecoveryAcknowledged`. An
annotation naming another hold releases nothing. The Gateway Task admission
policy admits an update of a Gateway-owned Task that changes only this
annotation.

**Not yet covered.** Record holds for a finalized turn without its store
record and for reclamation without the turn cleanup receipt come with the
Session turn protocols (P2 and P6). Detecting a restart or switch of an
`async-replicated` dataset that happened while no controller ran needs the
incarnation recorded with the acknowledgement.

**Shared suite.** `storagetest.WithDurability(DurabilityHarness{Restart,
Failovers})` checks, for every write in the write catalog, that after a
restart and after each declared failover the change, its receipt, the
dataset's identity, controller epoch, installation and watermark survive, and
a replay changes nothing. Each failover must change the incarnation, and
watermark writes that run during a failover either survive or settle as
aborted through `ResolveOperation`. A `local-fsync` or `local-buffered`
provider declares no failover, and a `synchronous-replicated` one declares at
least one. SQLite supplies `Restart` by closing and reopening its file.

### Kubernetes control protocols

The Kubernetes control store (`store/kube`) runs four protocols whose store
half is a `state.v1` family: outbox delivery (P3), Session cleanup (P4),
prompt result receipts (P5) and Gateway Task cleanup receipts (P7).
Controller ownership (P1) is the takeover sequence in "Controller ownership"
above. Session turn open and finalization (P2) and Task reclamation barriers
(P6) run over the Session turn records, which are still an embedded family;
they are written up here when those records join `state.v1`.

In the tables, K is a Kubernetes step and S a store step. Every store step
is a write with an envelope, so it commits only under the current fence, and
a replay of the same operation returns its original outcome. The idempotency
key says how a step run again as a new operation, for example by a successor
controller, is recognized: each store step below either has a natural key or
is idempotent by the state it changes, so running it again converges on one
outcome. When the storage client cannot settle a step's outcome, the
protocol's caller sees `ErrOutcomeUnknown`, reports no success and goes on as
each protocol says below. The shared suite's `protocols` family drives each
store half step by step: it loses store steps' responses and retries them,
runs them again as new operations, replaces the controller between steps, and
delivers steps the replaced controller issued after the takeover.

**P3. Outbox projection delivery.** The ACP outbox projector delivers Task
status projections. `store/kube` checks the Kubernetes controller epoch
around each store step.

| Step | Side | Operation | Idempotency key |
| --- | --- | --- | --- |
| 1 | S | `EnqueueOutboxProjectionRecord`, alone or inside Session turn finalization | Projection ID and payload digest |
| 2 | S | `ClaimOutboxProjectionRecords(fence, worker, lease, now)` | None; a claim leases due or expired records |
| 3 | K | Apply the projection's terminal phase, execution and delivery status to the Task | None; applying it again writes the same terminal status |
| 4 | S | `CompleteOutboxProjectionRecord(id, expected version, lease owner, operation ID and digest, new state)` | Operation ID and digest |

- A lost claim response is retried with the same operation and returns the
  same claim. A claim whose outcome stays unknown only delays its records
  until the lease expires; a new claim before then finds nothing due.
- The projector claims again on its next pass, so a claim or completion
  whose outcome stays unknown is settled then: a completed projection is no
  longer due, and an uncompleted one is claimed again once its lease expired.
- A controller that stops after step 3 leaves the claim to expire; the
  successor claims again, applies the projection again, which rewrites the
  same terminal status with a new transition time, and completes.
- A delayed step 4 from the replaced controller returns `ErrFenced`, and the
  successor's claim and completion stand.
- Store obligations: a claim returns only due or expired records; a
  completion requires the exact version, lease owner, operation ID and
  digest; a completion run again returns the stored projection.

**P4. Session cleanup, including Gateway retention cleanup.** Session
deletion and Gateway transcript retention reclaim a Session.

| Step | Side | Operation | Idempotency key |
| --- | --- | --- | --- |
| 1 | S | Read the intent, or the completion; a completion for the same operation means: verify the Kubernetes records are absent and stop | — |
| 2 | S | `PrepareSessionCleanup(intent)`: the operation ID and digest, the expected Kubernetes versions and UIDs, and the BranchClaim expectations | Namespace, Session and operation, while the intent is pending |
| 3 | local | Retire the resident runtime, outside the epoch interlock | — |
| 4 | K | Delete the BranchClaims, the Session Lease and the RuntimeSessionControl with exact UID and version preconditions, and check they are gone | Exact UIDs and versions |
| 5 | S | `CompleteSessionCleanup`: delete the transcript, Session row and intent, write the completion and archive turn cleanup receipts, in one transaction | Namespace, Session, operation ID and digest |

- From step 2 on, the intent refuses new work on the Session in the
  transaction of each write: transcript appends, Task locks, chat turns and
  turn commits return `ErrConflict`, and Gateway admission returns
  `ErrGatewaySessionCleanupPending`. `HasSessionCleanupFenceForUID` fences the
  Session UID.
- The intent covers the crash window between the Kubernetes deletes (step 4)
  and the transcript delete (step 5): a successor's `ResumeSessionCleanups`
  runs every intent again under its own fence, with the operation ID the
  intent holds.
- A lost step 5 response is retried; once the intent is gone, the next run
  finds the completion for the same operation in step 1 and stops.
- A delayed step 2 or step 5 from the replaced controller returns
  `ErrFenced`. Step 2 depends on this: once the cleanup completed it has no
  natural key, and only step 1 keeps Orka from preparing it again.
- Store obligations: preparation returns the pending intent for the same
  operation and refuses another operation while one is pending; completion
  is idempotent; the intent check runs in the transaction of every write it
  blocks; a completion keeps fencing the Session UID and reserves the name of
  a Session with a Kubernetes identity. A transcript-only Session, which has
  none, may be created again, which removes its completion.
- A Kubernetes record that reappears after a completion, such as a
  RuntimeSessionControl, fails the cleanup with `ErrConflict` and deletes
  nothing.

**P5. Prompt result receipt before settling.** The ACP dispatcher settles a
completed prompt.

| Step | Side | Operation | Idempotency key |
| --- | --- | --- | --- |
| 1 | S | `SavePromptResultReceipt(attempt ID, operation ID and digest, result)` | Attempt ID; a receipt is immutable |
| 2 | K | Move the PromptAttempt from `Running` to `Settling`, recording the same operation ID and digest | The attempt's expected version, operation ID and digest |
| 3 | S | `SaveResult` | The Task, whose result it replaces |
| 4 | K | Settle the attempt, then deliver | The attempt's expected version, operation ID and digest |

- A successor repairs a result write interrupted after step 2 by reading the
  receipt and running step 3 again.
- The dispatcher does not run step 1 or step 3 again itself when its outcome
  stays unknown, and reports no success. A Session Task's prompt is recorded
  as unknown at once; a standalone Task waits for recovery after the next
  takeover, which records an attempt still `Running` as unknown and repairs
  step 3 from the receipt for one already `Settling`.
- A delayed step 1 or step 3 from the replaced controller returns
  `ErrFenced`.
- Step 2 moves a PromptAttempt to `Settling` only after its receipt was
  stored, so a missing receipt, with no result that matches the recorded
  digest, or a receipt that differs from the recorded digest, holds the Task
  (see "Record holds" above), also when the journal already records the
  prompt's completion. An attempt that reached `Settling` another way never
  had a receipt, and its recovery is unchanged.

**P7. Gateway Task cleanup receipts.** Gateway retention removes the Tasks
of compacted events.

| Step | Side | Operation | Idempotency key |
| --- | --- | --- | --- |
| 1 | S | `MaintainGatewayRecords` compacts terminal events past retention and writes a `GatewayTaskCleanupReceipt` for each event's Task | The state it changes: compaction run again finds nothing left to compact |
| 2 | K | Request ordinary deletion of the exact Task UID the receipt names, and prove with an uncached read that it is gone | Task UID precondition |
| 3 | S | `DeleteGatewayTaskCleanupReceipt` | The state it changes: deleting a missing receipt succeeds |

- A delayed step 1 from the replaced controller returns `ErrFenced` and
  leaves the event, so the successor's own compaction writes the receipt.
- A delayed step 3 returns `ErrFenced`; the successor deletes the receipt
  itself once it proved the Task gone.
- A Task with no event, tombstone, receipt or archive is never deleted.

**Grouped writes.** Each grouped write commits all of its parts or none. The
shared suite's `grouped-writes` family makes the part a provider would write
last fail and checks that no part is visible: an execution event and plan
update whose usage observation reuses a stored observation ID, a chat turn
whose last message reuses a stored message ID with other content, and a
Gateway terminal projection and expiry whose delivery ID belongs to another
delivery. The ACP dispatcher writes a plan update only together with its
execution event, through `AppendExecutionEventWithPlanIfAbsent`.

### Writer exclusivity and schema migration for adapters

| Property | Embedded SQLite | Network provider |
| --- | --- | --- |
| One active writer | Process file lock, leader election and one replica | Leader election and the epoch compare-and-swap pick the controller; the store refuses writes under any other fence, and a dataset bound to one installation refuses another installation's advance. |
| A second controller opening the dataset | Cannot take the file lock | May read (`Describe`, validation); cannot write until it wins the compare-and-swap and advances the store. |
| Schema migration | At open, under the file lock and `BEGIN IMMEDIATE` | Adapter-owned, under a dataset-wide lock, with the dataset `migrating` so writes are refused. |

A network adapter migrates its schema in three steps: it enters `migrating`
in a transaction that takes the same lock as an epoch advance, so writes in
progress finish first and later new writes return `ErrNotReady`; applies the
change and records the new schema version; and returns to `ready`. A crash
leaves the dataset `migrating`, refusing writes, until the migration runs
again. Concurrent migrators serialize, and the second finds the target
version and does nothing. For PostgreSQL:

- Run migrations on a direct connection (port 5432 on Azure), never through a
  transaction-pooling PgBouncer (port 6432), and never with session-level
  advisory locks.
- Serialize adapter replicas with `SELECT pg_advisory_xact_lock(<constant>)`
  inside the migration transaction, then check the schema version again.
- Enter `migrating` in one transaction that locks the controller epoch row
  (`SELECT ... FROM orka_controller_epoch FOR UPDATE`) and then sets
  `orka_dataset.state` to `migrating`.
- Use transactional DDL where possible. Steps that cannot run in a
  transaction, such as `CREATE INDEX CONCURRENTLY`, run only while the state
  is `migrating`.
- Set `lock_timeout` on DDL, so a migration cannot stall behind a long reader.

Orka treats `migrating` as a temporary storage outage: writes answer 503 and
reconcilers retry, and nothing restarts the controller. The shared suite's
`storagetest.WithMaintenance(MaintenanceHarness{Enter, Exit})` checks, for
every write in the write catalog, that while the dataset is migrating the
write is refused with `ErrNotReady` and changes nothing, a replay and reads
are still served, `Describe` reports `migrating`, and `ResolveOperation`
records nothing new; and that the refused operation commits once the dataset
is ready. SQLite migrates only while it opens, so its harness holds the
dataset in `migrating` through a test hook. The `installations` cases check,
for both contracts, that a second installation can neither advance nor write
to a dataset bound to the first, and that the first keeps writing; the
SQLite package also checks that a second `OpenLockedStore` on the same file
is refused.

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
  once the store is a separate service. The lease-clock rule in "Write
  envelopes, operation receipts and controller ownership" decides which side
  owns each time; the request changes that carry lease times follow it.
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
| `MemoryProposalStore.ApplyMemoryProposal` | Proposal application calls `AuthorizeExistingMemory` inside the database transaction. |
| `TaskDataTransactionStore` transaction methods | Go callbacks run inside a database transaction. |
| `SecurityStore.ApplyScanTaskIngestion` | Go callbacks run inside a database transaction. |

Embedded families share transactions with state and artifact records (a
task-data transaction context also covers `SaveArtifact`, for example), so the
client refuses a configuration that would place them on different stores. That
sharing is an embedded-store behavior, not a provider obligation; see "No
cross-contract transactions" above.

`pkg/storage/v1/storagetest` is the reusable behavioral suite. `RunState` and
`RunArtifacts` exercise each provider family through the public interfaces;
the options `WithMaintenance` and `WithDurability` add the cases that need a
harness only the provider's own tests can supply. The SQLite provider runs it
directly with both options, and the storage client runs it over SQLite.

SQLite also implements the duplicate SQL control-record methods described in
the next section. They are not part of the provider contract or the client. A
client test fails if any other SQLite method is missing from the client, so a
consumer's type assertion cannot silently lose a capability.

### Orka policy above the client

The storage client applies Orka's redaction, sanitization and usage
attribution before a provider sees the data, so a provider, including the
embedded SQLite store, stores only content Orka has already made safe to
persist:

- **Execution events.** The client redacts and bounds each event's summary,
  text and JSON content, records the redaction and truncation metrics once
  for each newly appended event, counts an event it refuses before calling
  the provider as an append failure, and stamps the creation time when the
  caller left it unset. A provider only requires structured content to be valid
  JSON, and stores content that is only whitespace as no content.
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
  `SaveThreatModel` and `SaveThreatModelIfVersion`, including writes that a
  scan ingestion callback makes through its transaction-scoped store.
- **Gateway text.** The client redacts, trims and bounds the text Orka and
  adapters attach to gateway records: an event's state message, sender
  display name and metadata (also when it looks up a duplicate), expiry, retry
  and dead-letter reasons, a delivery's last error, provider message ID and
  metadata, and the metadata of a terminal transcript message. Gateway
  identifiers that the protocol already bounds are stored as received, now
  including the copies in a gateway user message's transcript metadata.
- **Memory.** The client redacts memory content, every free-text proposal
  field, review fields and the applier. Proposal application still
  re-applies the redaction rule when it copies a stored proposal into a
  memory, because it runs inside the embedded transaction.

Two tests in `internal/storageclient` send secrets through these paths over a
recording SQLite provider and fail if the provider receives any of them:
`TestProviderReceivesOnlySanitizedGatewayText` covers the gateway writes and
`TestProviderReceivesOnlyRedactedContent` covers the others. Encryption of snapshots, native
sessions and connector custody still happens inside SQLite.

### Envelopes in the storage client

Below the policy layer, so a digest covers exactly what the provider
receives, the client runs every write through generated decorators of the two
providers (`internal/storageclient/envelope_gen.go`, written by
`go generate ./internal/storageclient` from the provider interfaces and the
method catalog; `make generate` runs it and a test fails if it is stale).
Reads and administrative methods reach the provider unchanged.

- **Fence.** A write first waits for the fence of the dataset that serves its
  contract. The controller epoch manager is the fence source: it opens a
  contract's fence once that contract's dataset has advanced to the acquired
  epoch. A write waits up to ten seconds for a fence source to be bound and
  for that fence to open, and is then refused with `ErrNotReady`. A task-data
  transaction waits for the fences of both contracts, which the embedded
  store serves together, before it opens, and the writes inside it reuse
  them, so they never wait for a fence while the transaction holds the store.
- **Envelope.** The client creates one operation per write: the dataset ID
  and fence the fence source gives, a new UUIDv7 operation ID, the digest of
  the arguments as they are when the write is issued, the issue time and the
  next sequence. A method that takes its own fence, such as the outbox
  family, carries that fence in its envelope, so a caller holding a replaced
  fence is still refused with `ErrFenced`. A context that already carries an
  envelope belongs to a caller that owns the operation, such as the shared
  suite, and the write runs once with it.
- **Retries.** After `ErrUnavailable` (nothing committed) or
  `ErrOutcomeUnknown` (it may have), the client retries the same envelope with
  the same arguments, with backoff, until the call's deadline or thirty
  seconds. A replay of a committed operation returns its original result.
  `ErrClockSkew` is returned as `ErrNotReady`.
- **Resolution.** When retrying cannot settle an unknown outcome, or a later
  attempt is refused, the client calls `ResolveOperation` once. A committed
  operation is attempted once more to read its result as a replay; an
  aborted one returns `ErrOperationAborted`, joined with the refusal when a
  refusal ended the attempts, or with `ErrUnavailable` when only transient
  failures did, because nothing committed and running it again is safe. An
  error that leaves the outcome unknown matches only `ErrOutcomeUnknown`. An
  operation the client cannot settle, because the store is
  unreachable, returns `ErrOutcomeUnknown` and joins the dataset's unresolved
  set: new writes to that dataset are refused with `ErrNotReady` while the
  client keeps resolving the set in the background. An operation that
  `ResolveOperation` finds too old to settle leaves the set with its outcome
  permanently unknown. A Kubernetes protocol step in that state is settled by
  running the step again as a new operation (see "Kubernetes control
  protocols" above); holding the work that other writes affect is not
  covered yet.
- **Embedded families** pass through unchanged. They carry no envelope and
  SQLite does not require one for them; the Session turn steps keep checking
  the fence their requests carry.
- **Callers.** `store.IsTemporaryStorageError` matches `ErrNotReady`,
  `ErrUnavailable` and `ErrOutcomeUnknown`. The API answers them with 503 and
  a `Retry-After` hint, in its shared error handler, the gateway ingress,
  gateway store and internal task-data paths, the worker message and
  execution event routes, and the memory routes. Task result collection
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
