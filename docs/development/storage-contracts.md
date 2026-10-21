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
process lock and one `BEGIN IMMEDIATE` transaction. The final 166-to-167 transition
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
| `SessionLineageStore`, `HarnessV1AttemptStore`, `harness.RuntimeSessionStore` | Lineage claims, harness-v1 attempt receipts and runtime-session history; state. | Session continuity, harness-v1 dispatch/recovery and session APIs. These remain necessary while current upstream supports harness v1. |
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
  writes, soul pinning and lineage), harness-v1 attempts, Session cleanup
  persistence, the controller epoch mirror, outbox persistence, Task Job
  revocations and task data fences, execution events (including grouped
  appends), gateway records, security records (including scan Task
  ingestion), repository monitors, usage, and memories with their proposals
  (creation, review, archiving and application).
- `ArtifactProvider` serves `artifacts.v1`: artifact bytes and metadata.

`internal/store` keeps aliases for the moved names, so Orka callers keep their
existing types. The contract and its test suite do not import Orka's internal
packages.

### Provider obligations

These rules are part of the contract. The package documentation in
`pkg/storage/v1` states them for provider authors, and the shared suite checks
the observable parts.

- **Execution.** Each provider method commits atomically and is linearizable
  with respect to every other call on the same dataset, as if calls ran one at
  a time. SQLite's single writer gives this for free, so a serial suite cannot
  tell a correct provider from one running at a weaker isolation level. A
  PostgreSQL provider at the default `READ COMMITTED` isolation could pass a
  serial suite and still let two dispatchers claim one gateway event, because
  claims, chat-turn reservations and terminal approvals read and then write.
  A method that returns several records, such as a Session with its
  transcript, reads them from one snapshot; SQLite does this for `GetSession`,
  `LoadTranscriptThrough`, `ReadSessionSoul`, `GetGatewayQueueStats` and
  `ListPatchProposals`, the read methods found issuing more than one query. The
  suite's concurrency cases release eight callers at once against Session
  creation, chat-turn reservation, deduplicated event appends, terminal
  approvals and gateway claims, and require exactly one winner (or one claim
  per event). Another case reads a Session while messages are appended and
  requires its message count to match the messages returned.
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
  below.
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
  failures; no commit outcome can be lost in transit.
- **Write envelope.** `WriteEnvelope{DatasetID, Fence, OperationID,
  RequestDigest}` is reserved and travels in the context
  (`WithWriteEnvelope`, `WriteEnvelopeFrom`). Orka does not attach it yet and
  providers must accept writes without one. Operation receipts (step 3) will
  check the dataset, the controller epoch fence and the operation identity at
  commit time. Only the harness-v1 attempt family and the outbox family take a
  `ControllerEpochFence` today, as an explicit parameter that stays
  authoritative for them; the envelope reaches every other write family
  without another change to each of the contract's roughly 190
  context-taking methods. Operation-specific preconditions, such as an
  expected version or message count, stay explicit parameters.
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
  option, so every time column is written and read in UTC, including rows
  written earlier in another zone; its queries compare times as text, so one
  zone also keeps those comparisons correct. Usage records are stored as JSON,
  so SQLite converts their times to UTC when it writes and reads them.
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
  `ListActiveScanRuns`, `ListHarnessV1AttemptsByTask` and `GetMessages` return
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
and passes it to every application consumer. The SQLite store itself is used
only for its process lock, bootstrap and key setup, configuration and manager
lifecycle. The client composes the state and artifact providers and serves the
remaining families through `storageclient.Embedded`, which only the SQLite
store implements:

| Embedded family | Why it is outside the provider contract |
| --- | --- |
| `AgentExecutionSnapshotStore`, `AgentExecutionSnapshotLifecycleStore` | Snapshot bodies arrive in plaintext; SQLite encrypts them. |
| `NativeSessionImportStore`, `SessionTurnPersistenceStore` | Native session bytes arrive in plaintext, including the native capture in turn finalization. |
| `ConnectorCredentialStore`, `ConnectorConsentStore` | Tokens arrive in plaintext; SQLite performs row-bound sealing. |
| `harness.RuntimeSessionStore` | Its records are defined in `internal/harness`. |

The embedded families live in the same SQLite database as state and artifact
records, and fenced artifact writes rely on the cleanup generations held with
state, so the client refuses a configuration that would place them on
different stores. That sharing is an embedded-store behavior, not a provider
obligation; see "No cross-contract transactions" above.

`pkg/storage/v1/storagetest` is the reusable behavioral suite. `RunState` and
`RunArtifacts` exercise each provider family through the public interfaces.
The SQLite provider runs it directly, and the storage client runs it over
SQLite.

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
  JSON.
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
  metadata, and the metadata of a terminal transcript message. Gateway
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
`TestProviderReceivesOnlyRedactedContent` covers the others. Encryption of snapshots, native
sessions and connector custody still happens inside SQLite.

## Kubernetes control authority and retained SQL compatibility

`store/kube.NewComposite` receives precisely `SessionTurnPersistenceStore`,
`OutboxPersistenceStore` and `SessionCleanupPersistenceStore` through
`kube.SQLitePersistence`, plus optional harness-v1 receipt reads. It implements
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
not redefine those counts. [#568](https://github.com/orka-agents/orka/issues/568)
and [PR #574](https://github.com/orka-agents/orka/pull/574) own harness-v1 retirement.
Retain currently supported persistence until that work lands.

## Conditional commands

Provider methods take no Go callbacks. Each operation that used to run Orka
code inside a database transaction is now a protocol with the same three
steps: the provider resolves the target without returning its content, Orka
authorizes or computes with no provider transaction open, and a conditional
command applies only while the target and its version still match. A changed
target or version returns `ErrPreconditionFailed` (which also matches
`ErrConflict`) with nothing applied or disclosed. The storage client runs
Orka's side, so callers keep their interfaces. The shared suite covers each
protocol, including competing callers.

| Operation | Protocol |
| --- | --- |
| Memory proposal application (`ApplyMemoryProposal`, `AuthorizeExistingMemory`) | `ResolveMemoryProposalApplication` returns the memory ID the application would return (the applied memory, or one already created from the proposal) and the proposal status and applied ID it read, but no memory content. Orka authorizes that exact memory for an external caller; a new memory is covered by the caller's create permission, and Orka builds and redacts it from the stored proposal. `CommitMemoryProposalApplication` applies only if the proposal still resolves to the same target. Competing applications create one memory: the losers' retries resolve, authorize and return the winner's memory. |
| Task data access (`WithAuthorizedTaskDataAccess`, `WithTaskDataAccess`, which replace the task-data transaction methods) | `ResolveTaskDataFence` returns the namespace or Task cleanup generation. A Task is registered (`RegisterTaskDataFence`) only after a first successful authorization, and the registration is released when the access it was created for fails. Orka authorizes against live Kubernetes state, then sends the fence, with the authorized Job identities, in the context of each data call. A harness-v1 artifact upload also binds the attempt it verified, so its write is refused once that attempt ends or a newer one starts. Trusted controller access (`WithTaskDataAccess`) registers a Task only for the duration of the access, so it leaves nothing behind for a Task that may already be finalized. Every fence-checking method listed on `TaskDataFenceStore` refuses a moved generation with `ErrTaskDataCleanupChanged` or a revoked Job with `ErrTaskJobRevoked` in the same transaction as its own reads and writes. The calls of one access are separate operations: each either completes before an invalidating cleanup, which then removes its effect, or is refused, and callers retry with fresh authorization. A refused call that a caller turned into its own error is still reported as the fence error. A Kubernetes write made inside an access, such as a parent's request to cancel a child, is checked against the fence before the access runs but is not serialized with a later cleanup or revocation; it keeps its own UID and resourceVersion preconditions. Evidence that must not be recorded in part, such as a recovered approval decision and its execution outcome, uses one grouped append (`AppendExecutionEventsIfAbsent`). |
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
by Orka above an eventual external adapter; this contribution leaves the current
implementations and formats intact.
