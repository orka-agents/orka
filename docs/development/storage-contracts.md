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
  option, so every time column is written and read in UTC, including rows
  written earlier in another zone; its queries compare times as text, so one
  zone also keeps those comparisons correct. Usage records are stored as JSON,
  so SQLite converts their times to UTC when it writes and reads them.
- **Records are not the wire format.** Records carry JSON tags for Orka's API.
  Persisted fields such as gateway claim ownership, lease expiry, the frozen
  Task tool policy, usage phase history, scan-run cancellation state and
  finding decision times are tagged `json:"-"`, so a provider that sends these
  structs as JSON loses them. The wire protocol (see "Wire protocol" below)
  defines its own wire records, carries every field including the `json:"-"`
  ones, and round-trips every record type in a test. `ExecutionEvent.Usage` is
  also tagged `json:"-"`: it carries the usage observation Orka derived from
  the event (see "Orka policy above the client").
- **What the wire preserves.** Nil and empty slices and maps are equivalent,
  so a caller must not tell a nil result apart from an empty one. Strings are
  valid UTF-8. Times are within 0001-01-01 and 9999-12-31 UTC; the zero
  `time.Time` means "no time", and a `*time.Time` keeps nil apart from a
  pointer to the zero time.
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
| `MemoryProposalStore.ApplyMemoryProposal` | Proposal application calls `AuthorizeExistingMemory` inside the database transaction. |
| `TaskDataTransactionStore` transaction methods | Go callbacks run inside a database transaction. |
| `SecurityStore.ApplyScanTaskIngestion` | Go callbacks run inside a database transaction. |

Embedded families share transactions with state and artifact records (a
task-data transaction context also covers `SaveArtifact`, for example), so the
client refuses a configuration that would place them on different stores. That
sharing is an embedded-store behavior, not a provider obligation; see "No
cross-contract transactions" above.

`pkg/storage/v1/storagetest` is the reusable behavioral suite. `RunState` and
`RunArtifacts` exercise each provider family through the public interfaces.
The SQLite provider runs it directly and again over the wire loopback (see
"Wire protocol"), and the storage client runs it over SQLite. The
`storagetest.Parallel()` option runs the families in parallel for a factory
that returns isolated datasets concurrently.

SQLite also implements the duplicate SQL control-record methods described in
the next section. They are not part of the provider contract or the client. A
client test fails if any other SQLite method is missing from the client, so a
consumer's type assertion cannot silently lose a capability.

### Wire protocol

`pkg/storage` is its own Go module, `github.com/orka-agents/orka/pkg/storage`,
so a separately built provider imports the contract, the suite and the wire
library without Orka's module graph. Its only direct requirements are gRPC,
protobuf, the `google.rpc` status types and the assertion library the suite
uses, and no package in it imports Orka's internal packages. The root module
requires it through a `replace` directive, so Dockerfiles copy
`pkg/storage/go.mod` and `go.sum` before downloading modules, and root `./...`
patterns do not include it: `make fmt`, `vet`, `test` and `lint` run it
separately. Each release also tags the module as `pkg/storage/<version>`.

The protocol is gRPC with protobuf. The hand-written `.proto`
files under `pkg/storage/proto/orka/storage/v1` are the wire source of truth:
one service per contract family, one RPC per contract method, named after it,
and wire records named after the Go records, with fields in snake case.
`make generate-storage` (part of `make generate`) compiles them with a pinned
buf and protoc plugins into `pkg/storage/v1/wire/storagepb`, then runs
`wiregen`, which reads the Go contract and the descriptors and writes the
mapping in `pkg/storage/v1/wire`. Parameters map to request fields by name and
results to response fields by position. Generation fails when a contract
method has no RPC or an RPC no method, when any parameter, result or record
field, including a `json:"-"` field, has no wire field or the reverse, or when
a pair of types is outside the mapping table. CI regenerates the code and
fails on a difference.

The wire currently serves `Describe` and `HealthCheck`, inter-agent messages
and plan state. The other families stay in process until their RPCs land.
`wiretest.State` serves a provider over an in-memory loopback with TLS 1.3 and
bearer authentication, sending each family that has RPCs through the server
library and the generated client; the SQLite provider passes the whole suite
that way.

- **Call metadata.** Every request names `orka-storage-contract`
  (`orka.storage.v1`) and its `orka-storage-revision`; the server refuses a
  call without them (`CONTRACT_MISMATCH`). A bound client names its dataset in
  `orka-storage-dataset`, and responses name the contract, the server's
  revision and, once a `Describe` has reported it, the serving dataset. A write's `WriteEnvelope` travels as the
  binary header `orka-storage-envelope-bin`, and the server puts it in the
  provider's context with `WithWriteEnvelope`.
- **Unknown fields.** The server refuses a request with a field it does not
  know anywhere in its message tree (`UNSUPPORTED_FIELD`), so an older server
  never ignores a newer precondition. The protocol revision starts at 1; a
  release raises it when it adds RPCs or request fields, and the client calls
  an RPC only on a server of at least the revision that introduced it.
- **Errors.** Every status the server library produces carries a
  `google.rpc.ErrorInfo` in domain `storage.orka.ai` whose reason names the
  contract error. The client rebuilds the typed error, so `errors.Is` and
  `errors.As` behave as in process, including a `FencedError` that matches
  `ErrConflict`. Only `VALIDATION`, `FENCED` and `CONFLICT` forward their
  message, bounded and stripped of control characters. Any other provider
  error, and a provider panic, is reported without its text as
  `PROVIDER_ERROR` with a correlation ID the server logs; the client returns
  it as `ErrOutcomeUnknown` for a write and a plain error for a read.
- **Send evidence.** A status without that ErrorInfo came from the gRPC
  library or a proxy. On a connection built with `wire.Dial`, which takes TLS
  credentials, per-call credentials and a dialer but no interceptors, and
  given to the client directly rather than wrapped, the client's stats
  handler records whether a call's request headers could have left the
  process: a call that never sent them is `ErrUnavailable`, and a write that
  may have been sent is `ErrOutcomeUnknown`. On any other connection, a
  write's transport failure is `ErrOutcomeUnknown`. Storage connections disable gRPC's configured retries;
  only its transparent retries of attempts the server never processed remain.
  The client refuses invalid UTF-8, out-of-range times and requests over the
  16 MiB message limit before sending anything.

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
