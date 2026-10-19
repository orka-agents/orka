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

// TaskTokenUsageReader returns the input and output tokens attributed to one
// Task, as the usage report counts them.
type TaskTokenUsageReader interface {
	TaskTokenUsage(ctx context.Context, namespace, taskUID string) (inputTokens, outputTokens int64, err error)
}

// StateProvider is the provider side of state.v1.
type StateProvider interface {
	HealthChecker

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
	HarnessV1AttemptStore

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

	SecurityStore
	RepositoryMonitorStore
	UsageStore
	TaskTokenUsageReader
}

// ArtifactProvider is the provider side of artifacts.v1. Artifact bytes and
// their metadata stay together under this contract.
type ArtifactProvider interface {
	HealthChecker
	ArtifactStore
}
