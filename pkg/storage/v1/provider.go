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
// never receive Go callbacks; Kubernetes authority, authorization and
// controller-side orchestration stay in Orka. Some application families are
// not yet part of this contract because they still carry plaintext protected
// data or callbacks into the embedded SQLite store. Those families remain
// internal to Orka until their provider shape is defined.
//
// # Execution semantics
//
// Each provider method commits atomically and is linearizable with respect to
// every other call on the same dataset: the outcome is as if the calls ran one
// at a time, in an order consistent with when they were made. A method that
// reads and then writes, such as claiming a gateway event, reserving a chat
// turn or recording a terminal approval, keeps that guarantee across its read
// and its write, and a method that returns several records, such as a Session
// with its transcript, reads them from one snapshot. A nil error means the
// change is durable: it survives a
// provider restart and any failover the provider declares automatic (see
// [DurabilityClass]). No operation or transaction spans state.v1 and
// artifacts.v1.
//
// # Errors
//
// A provider reports each refusal with the sentinel error that names it, so
// callers can branch with errors.Is: [ErrNotFound], [ErrConflict] (including a
// reused identifier), [ErrFenced], [ErrValidation] (including an invalid
// cursor), [ErrTooLarge], [ErrCapacity], [ErrNotReady], or a family error such
// as [ErrDuplicateMismatch] or [ErrGatewaySessionCleanupPending]. A provider that could not run an operation
// returns [ErrUnavailable] only when nothing was committed. When a write may
// have committed, it returns [ErrOutcomeUnknown], never success.
//
// # Times
//
// A provider preserves every caller-supplied time exactly, to the nanosecond,
// and returns stored times in UTC. Some records bind exact times into
// encryption associated data or keys, so rounding to a database's native
// precision corrupts them.
//
// # Records and wire format
//
// The records in this package carry JSON tags for Orka's API, not a storage
// encoding: several persisted fields, such as gateway claim ownership and
// usage phase history, are tagged json:"-". A provider that crosses a process
// boundary defines its own wire records and must not encode these structs
// with encoding/json. The orka.storage.v1 wire protocol in package wire
// carries every field, including the json:"-" ones.
//
// Three rules let a record cross the wire unchanged, so callers and providers
// must not depend on anything they do not preserve:
//
//   - Nil and empty slices and maps are equivalent. A caller must not tell a
//     nil result apart from an empty one.
//   - Strings are valid UTF-8.
//   - Times are within 0001-01-01 and 9999-12-31 UTC. The zero time.Time is
//     "no time"; a *time.Time keeps nil apart from a pointer to the zero
//     time.
//
// The contract is unstable until a release advertises storage extension
// support; names and semantics may change before then.
package v1

import "context"

const (
	// ContractVersion identifies this storage extension contract.
	ContractVersion = "orka.storage.v1"
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

	ControllerEpochMirror
	OutboxPersistenceStore
	OutboxBacklogReader
	TaskJobAuthorityStore

	AtomicExecutionEventPlanStore

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
	ArtifactStore
}
