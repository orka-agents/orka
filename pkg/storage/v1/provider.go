// Package v1 defines version orka.storage.v1 of the Orka storage extension
// contract: the records, errors and provider interfaces that an application
// storage backend implements.
//
// The contract has two parts, which an installation may bind to the same
// provider or to different providers:
//
//   - state.v1 ([StateProvider]) holds all database-owned application state
//     except public Task artifacts. Records that must commit together stay in
//     this contract.
//   - artifacts.v1 ([ArtifactProvider]) holds public Task artifact bytes and
//     their metadata together.
//
// Orka reaches every provider through its storage client. Provider methods
// never receive Go callbacks and never hold a transaction open while Orka
// works; Kubernetes authority, authorization and controller-side
// orchestration stay in Orka. Where Orka must check something between a read
// and a write, the contract splits the operation into a resolve step and a
// conditional command that applies only while the resolved target and
// version still match, and otherwise changes nothing and returns
// [ErrPreconditionFailed]; the sealed families' commands, which name the
// versions and digests Orka read, return [ErrConflict] instead.
// Protected records, such as execution snapshots, native session state and
// connector custody, reach a provider only sealed.
//
// # Execution semantics
//
// Each provider method is atomic and linearizable with respect to every other
// call on the same dataset: the outcome is as if the calls ran one at a time,
// in an order consistent with when they were made. A method that reads and
// then writes, such as claiming a gateway event, reserving a chat turn or
// recording a terminal approval, keeps that guarantee across its read and its
// write, and a method that returns several records, such as a Session with its
// transcript, reads them from one snapshot. A nil error from a write means its
// change is durable to the degree the provider's [DurabilityClass] states:
// every class survives a restart of the provider process, and the class says
// whether the change also survives host loss and an automatic failover. No
// operation or transaction spans state.v1 and artifacts.v1.
//
// # Errors
//
// A provider reports each refusal with the sentinel error that names it, so
// callers can branch with errors.Is: [ErrNotFound], [ErrConflict] (including a
// reused identifier), [ErrFenced], [ErrPreconditionFailed], [ErrValidation]
// (including an invalid cursor), [ErrTooLarge], [ErrCapacity], [ErrNotReady],
// or a family error such as [ErrDuplicateMismatch], [ErrTaskDataCleanupChanged]
// or [ErrGatewaySessionCleanupPending]. A provider that could not run an operation
// returns [ErrUnavailable] only when nothing was committed. When a write may
// have committed, it returns [ErrOutcomeUnknown], never success. A provider
// that rejects the caller's credentials returns [ErrUnauthenticated], and one
// that cannot serve the contract revision a call needs, or the dataset's
// schema, returns [ErrIncompatible]; neither commits anything.
//
// # Validation and health
//
// Before Orka uses a provider, and again while it runs, it validates the
// binding with [ProviderDescriber.Describe]: the contract version and
// [ContractRevision], the contract parts served, the dataset's identity and
// state, the limits, the durability class and the provider clock. Orka does
// not interpret a provider's SchemaVersion; the provider gives its own verdict
// on the dataset's schema through [ProviderInfo.State]. Between validations
// Orka probes the binding with [HealthChecker.HealthCheck].
//
// [LookupMethod] gives each method a [CallClass] as well as a kind, so a
// storage client or transport can keep long transfers and background work
// from delaying health checks and ordinary calls.
//
// # Times
//
// A provider preserves every time a caller supplies exactly, to the
// nanosecond, and returns stored times in UTC. Some records bind exact times
// into encryption associated data or keys, so rounding to a database's native
// precision corrupts them. The exceptions are the record times the provider
// assigns from its own clock and for which it ignores any value the caller
// set: artifact and message creation times, Task result timestamps, a plan's
// CreatedAt and UpdatedAt, and the repository monitor times listed under
// "Clock ownership" in docs/development/storage-contracts.md.
//
// # Write envelopes, operation receipts and controller ownership
//
// [LookupMethod] classifies every provider method as a read, a write or an
// administrative method. Each write is one operation: a [WriteEnvelope] names
// its dataset, the controller epoch fence it was issued under, its operation
// ID, its [RequestDigest], when it was issued and its issue order. A provider
// checks the envelope in the transaction that commits the write and records an
// operation receipt with the change, so a retry returns the original outcome
// and a delayed write from a replaced controller cannot commit
// ([OperationReceiptStore], [ControllerEpochAdvancer]). Both providers
// implement those two interfaces. Orka's storage client attaches an envelope
// to every write, and a provider refuses a write without one with
// [ErrValidation].
//
// # Installation binding
//
// Each dataset records the one installation that owns it, written once
// ([DatasetBindingStore]): a second installation can never claim a dataset
// another one bound, and a dataset the installation has left refuses writes.
// The controller epoch advance accepts only the bound installation. Both
// providers implement it; Orka does not bind datasets yet.
//
// # Records and wire format
//
// The records in this package carry JSON tags for Orka's API, not a storage
// encoding: several persisted fields, such as gateway claim ownership and
// usage phase history, are tagged json:"-". A provider that crosses a process
// boundary defines its own wire records and must not encode these structs
// with encoding/json.
//
// The contract is unstable until a release advertises storage extension
// support; names and semantics may change before then.
package v1

import "context"

const (
	// ContractVersion identifies this storage extension contract.
	ContractVersion = "orka.storage.v1"
	// ContractRevision is the latest revision of ContractVersion this package
	// defines. A provider that meets its obligations, which this package's
	// shared suite checks, reports it in [ProviderInfo.ContractRevision]. A
	// change that adds an obligation every provider must meet increments it,
	// so Orka can refuse a provider that is too old for a contract part with
	// one precise reason instead of failing at the first missing method. An
	// optional family is announced through [ProviderInfo.Capabilities]
	// instead. Revision 2 requires [OperationReceiptStore] and
	// [ControllerEpochAdvancer] on both provider contracts. Revision 3 added
	// [DatasetBindingStore] and [ProviderInfo.Binding]. Revision 4 requires a
	// valid [WriteEnvelope] on every write and [ErrValidation] when missing.
	// Revision 5 split the inbox read into [MessageStore.ListUnreadMessages]
	// and [MessageStore.ConsumeMessages], bounded by [MaxMessageBatch], made
	// [MessageStore.SendMessage] write the assigned ID and creation time back,
	// and made every Session lease decision, and the claim check when
	// freezing a gateway event's allowed tools, use the time the request
	// carries. Revision 6 orders whole-object replacement by write stamp: a
	// SaveResult or SaveArtifact replaces a stored result or file only when
	// the (Fence.Epoch, Sequence) of its [WriteEnvelope] is greater than the
	// stored one's, and results are limited to [MaxResultBytes], reported in
	// [ProviderLimits.MaxResultBytes]. Revision 7 requires [WriteWatermarkStore]
	// on both provider contracts.
	ContractRevision = 7
	// StateContract names the application state part of the contract.
	StateContract = "state.v1"
	// ArtifactsContract names the public artifact part of the contract.
	ArtifactsContract = "artifacts.v1"
)

// OutboxBacklogReader reports unsettled outbox projections without claiming
// them. The planned-upgrade drain barrier uses it to prove the outbox is empty.
type OutboxBacklogReader interface {
	CountUnsettledOutboxProjections(ctx context.Context) (int64, error)
}

// SessionTypeReader returns a Session's stored type without loading its
// transcript.
type SessionTypeReader interface {
	GetSessionType(ctx context.Context, namespace, name string) (string, error)
}

// StateProvider is the provider side of state.v1.
type StateProvider interface {
	HealthChecker
	ProviderDescriber

	ResultStore
	PromptResultReceiptStore
	PlanStore
	MessageStore

	SessionStore
	SessionTypeReader
	SessionTurnCommitter
	FencedSessionWriteStore
	SessionSoulReader
	SessionSoulWriter
	SessionLineageStore
	SessionCleanupPersistenceStore

	SessionTurnRecordStore
	SealedNativeSessionStore
	SealedAgentExecutionSnapshotStore
	AgentExecutionSnapshotLifecycleStore
	SealedConnectorCustodyStore
	SealedConnectorConsentStore

	ControllerEpochMirror
	ControllerEpochAdvancer
	OperationReceiptStore
	DatasetBindingStore
	WriteWatermarkStore
	OutboxPersistenceStore
	OutboxBacklogReader
	TaskJobAuthorityStore
	TaskDataFenceStore

	AtomicExecutionEventPlanStore
	GroupedExecutionEventStore

	GatewayEventStore
	GatewayDeliveryStore
	GatewayTaskCleanupReceiptStore
	GatewayTaskCleanupReceiptMaintenanceStore
	GatewaySessionCleanupCandidateStore

	MemoryStore
	MemoryProposalStore

	SecurityStore
	RepositoryMonitorStore
	UsageStore
	TaskUsageObservationReader
}

// ArtifactProvider is the provider side of artifacts.v1. Artifact bytes and
// their metadata stay together under this contract.
type ArtifactProvider interface {
	HealthChecker
	ProviderDescriber
	ControllerEpochAdvancer
	OperationReceiptStore
	DatasetBindingStore
	WriteWatermarkStore
	ArtifactStore
}
