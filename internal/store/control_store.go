package store

import "context"

// ControllerEpochStore owns the durable controller epoch CAS record.
type ControllerEpochStore interface {
	GetControllerEpoch(ctx context.Context, name string) (*ControllerEpoch, error)
	CompareAndSwapControllerEpoch(ctx context.Context, change ControllerEpochCAS) (*ControllerEpoch, error)
}

// ControllerEpochMirror receives the exact epoch acquired from the
// Kubernetes-authoritative ControllerEpochStore. Mirrors are subordinate
// fencing records for SQLite payload transactions; they never select or
// advance controller ownership themselves.
type ControllerEpochMirror interface {
	SyncControllerEpochMirror(ctx context.Context, epoch ControllerEpoch) error
}

// PromptAttemptStore persists the full prompt execution and delivery state machines.
type PromptAttemptStore interface {
	CreatePromptAttempt(ctx context.Context, attempt *PromptAttempt, fence ControllerEpochFence) (*PromptAttempt, error)
	GetPromptAttempt(ctx context.Context, id string) (*PromptAttempt, error)
	TransitionPromptAttemptExecution(ctx context.Context, transition PromptAttemptExecutionTransition) (*PromptAttempt, error)
	RecoverPromptAttemptPreSubmission(ctx context.Context, recovery PromptAttemptPreSubmissionRecovery) (*PromptAttempt, error)
	TransitionPromptAttemptDelivery(ctx context.Context, transition PromptAttemptDeliveryTransition) (*PromptAttempt, error)
}

// PromptAttemptRetentionStore reclaims Task-owned PromptAttempt records only
// after the broader durable control plane has crossed its finalization barriers.
type PromptAttemptRetentionStore interface {
	PreparePromptAttemptReclamation(ctx context.Context, request ReclaimPromptAttemptsRequest) error
	ReclaimPromptAttempts(ctx context.Context, request ReclaimPromptAttemptsRequest) (int, error)
}

// SessionControlStore persists fenced Session mutation leases and atomic turn finalization.
type SessionControlStore interface {
	CreateSessionControl(ctx context.Context, control *SessionControl, fence ControllerEpochFence) (*SessionControl, error)
	GetSessionControl(ctx context.Context, namespace, sessionName string) (*SessionControl, error)
	AcquireSessionMutationLease(ctx context.Context, request AcquireSessionMutationLeaseRequest) (*SessionControl, error)
	ReleaseSessionMutationLease(ctx context.Context, request ReleaseSessionMutationLeaseRequest) (*SessionControl, error)
	ReconcileSessionControl(ctx context.Context, request ReconcileSessionControlRequest) (*SessionControl, error)
	CreateSessionTurn(ctx context.Context, request CreateSessionTurnRequest) (*SessionTurn, error)
	GetSessionTurn(ctx context.Context, id string) (*SessionTurn, error)
	FinalizeSessionTurn(ctx context.Context, request FinalizeSessionTurnRequest) (*SessionTurn, error)
	ResumeSessionTurnFinalization(ctx context.Context, request ResumeSessionTurnFinalizationRequest) (*SessionTurn, error)
}

// BranchClaimStore persists exact branch ownership and verified baselines.
type BranchClaimStore interface {
	CreateBranchClaim(ctx context.Context, claim *BranchClaim, fence ControllerEpochFence) (*BranchClaim, error)
	GetBranchClaim(ctx context.Context, id string) (*BranchClaim, error)
	CompareAndSwapBranchClaim(ctx context.Context, change BranchClaimCAS) (*BranchClaim, error)
	ReclaimBranchClaim(ctx context.Context, request ReclaimBranchClaimRequest) error
}

// BranchClaimCreationStore reports whether this exact call inserted the claim,
// allowing callers to safely clean up only creation they actually won.
type BranchClaimCreationStore interface {
	CreateBranchClaimWithResult(ctx context.Context, claim *BranchClaim, fence ControllerEpochFence) (*BranchClaim, bool, error)
}

// PublicationStore persists clean-room prepare, publish, and independent verification receipts.
type PublicationStore interface {
	CreatePublication(ctx context.Context, publication *Publication, fence ControllerEpochFence) (*Publication, error)
	GetPublication(ctx context.Context, id string) (*Publication, error)
	SetPublicationPRIntent(ctx context.Context, request SetPublicationPRIntentRequest) (*Publication, error)
	SetPublicationPRReceipt(ctx context.Context, request SetPublicationPRReceiptRequest) (*Publication, error)
	TransitionPublication(ctx context.Context, transition PublicationTransition) (*Publication, error)
}

// ExternalEffectStore persists canonical idempotency identities for operations outside SQLite.
type ExternalEffectStore interface {
	ReserveExternalEffect(ctx context.Context, request ReserveExternalEffectRequest) (*ExternalEffect, error)
	GetExternalEffect(ctx context.Context, id string) (*ExternalEffect, error)
	TransitionExternalEffect(ctx context.Context, transition ExternalEffectTransition) (*ExternalEffect, error)
}

// ExternalEffectIdentityReader resolves one exact external-effect identity.
// Kubernetes-backed authorization paths use this instead of broad LIST reads
// so a just-committed in-flight lease is observed without cache/list races.
type ExternalEffectIdentityReader interface {
	GetExternalEffectByIdentity(ctx context.Context, identity ExternalEffectIdentity) (*ExternalEffect, error)
}

// OutboxProjectionStore persists restart-safe task/status projection records.
type OutboxProjectionStore interface {
	EnqueueOutboxProjection(ctx context.Context, projection *OutboxProjection, fence ControllerEpochFence) (*OutboxProjection, error)
	GetOutboxProjection(ctx context.Context, id string) (*OutboxProjection, error)
	ClaimOutboxProjections(ctx context.Context, request ClaimOutboxProjectionsRequest) ([]OutboxProjection, error)
	CompleteOutboxProjection(ctx context.Context, request CompleteOutboxProjectionRequest) (*OutboxProjection, error)
}

// DurableControlStore is the complete first-release durable-control-store foundation.
type DurableControlStore interface {
	ControllerEpochStore
	PromptAttemptStore
	PromptAttemptRetentionStore
	SessionControlStore
	BranchClaimStore
	PublicationStore
	ExternalEffectStore
	OutboxProjectionStore
}
