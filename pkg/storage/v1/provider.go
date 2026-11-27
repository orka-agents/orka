package v1

import (
	"context"
	"time"
)

const (
	ContractVersion   = "orka.storage.v1"
	ContractRevision  = 16
	StateContract     = "state.v1"
	ArtifactsContract = "artifacts.v1"
)

type OutboxBacklogReader interface {
	CountUnsettledOutboxProjections(ctx context.Context) (int64, error)
}

type SessionTypeReader interface {
	GetSessionType(ctx context.Context, namespace, name string) (string, error)
}

type SessionCreationReader interface {
	GetSessionCreatedAt(ctx context.Context, namespace, name string) (time.Time, error)
}

type StateProvider interface {
	HealthChecker
	ProviderDescriber

	ResultStore
	PromptResultReceiptStore
	PlanStore
	MessageStore

	SessionStore
	SessionTypeReader
	SessionCreationReader
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
	MemoryConsolidationStore

	SecurityStore
	RepositoryMonitorStore
	UsageStore
	TaskUsageObservationReader

	AHPStore
	AuthSessionStore
}

type ArtifactProvider interface {
	HealthChecker
	ProviderDescriber
	ControllerEpochAdvancer
	OperationReceiptStore
	DatasetBindingStore
	WriteWatermarkStore
	ArtifactStore
}
