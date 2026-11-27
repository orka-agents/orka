package store

import "context"

type ControllerEpochStore interface {
	GetControllerEpoch(ctx context.Context, name string) (*ControllerEpoch, error)
	CompareAndSwapControllerEpoch(ctx context.Context, change ControllerEpochCAS) (*ControllerEpoch, error)
}

type ControllerEpochInstallationReader interface {
	ControllerEpochInstallationID(ctx context.Context, name string) (string, error)
}

type PromptAttemptStore interface {
	CreatePromptAttempt(ctx context.Context, attempt *PromptAttempt, fence ControllerEpochFence) (*PromptAttempt, error)
	GetPromptAttempt(ctx context.Context, id string) (*PromptAttempt, error)
	TransitionPromptAttemptExecution(ctx context.Context, transition PromptAttemptExecutionTransition) (*PromptAttempt, error)
	RecoverPromptAttemptPreSubmission(ctx context.Context, recovery PromptAttemptPreSubmissionRecovery) (*PromptAttempt, error)
	TransitionPromptAttemptDelivery(ctx context.Context, transition PromptAttemptDeliveryTransition) (*PromptAttempt, error)
}

type PromptAttemptRetentionStore interface {
	PreparePromptAttemptReclamation(ctx context.Context, request ReclaimPromptAttemptsRequest) error
	ReclaimPromptAttempts(ctx context.Context, request ReclaimPromptAttemptsRequest) (int, error)
}

type SessionControlStore interface {
	CreateSessionControl(ctx context.Context, control *SessionControl, fence ControllerEpochFence) (*SessionControl, error)
	GetSessionControl(ctx context.Context, namespace, sessionName string) (*SessionControl, error)
	AcquireSessionMutationLease(ctx context.Context, request AcquireSessionMutationLeaseRequest) (*SessionControl, error)
	CommitSessionRuntimeGeneration(ctx context.Context, request CommitSessionRuntimeGenerationRequest) (*SessionControl, error)
	ReleaseSessionMutationLease(ctx context.Context, request ReleaseSessionMutationLeaseRequest) (*SessionControl, error)
	ReconcileSessionControl(ctx context.Context, request ReconcileSessionControlRequest) (*SessionControl, error)
	CreateSessionTurn(ctx context.Context, request CreateSessionTurnRequest) (*SessionTurn, error)
	GetSessionTurn(ctx context.Context, id string) (*SessionTurn, error)
	FinalizeSessionTurn(ctx context.Context, request FinalizeSessionTurnRequest) (*SessionTurn, error)
	ResumeSessionTurnFinalization(ctx context.Context, request ResumeSessionTurnFinalizationRequest) (*SessionTurn, error)
}

type BranchClaimStore interface {
	CreateBranchClaim(ctx context.Context, claim *BranchClaim, fence ControllerEpochFence) (*BranchClaim, error)
	CreateBranchClaimWithResult(ctx context.Context, claim *BranchClaim, fence ControllerEpochFence) (*BranchClaim, bool, error)
	GetBranchClaim(ctx context.Context, id string) (*BranchClaim, error)
	CompareAndSwapBranchClaim(ctx context.Context, change BranchClaimCAS) (*BranchClaim, error)
	ReclaimBranchClaim(ctx context.Context, request ReclaimBranchClaimRequest) error
}

type PublicationStore interface {
	CreatePublication(ctx context.Context, publication *Publication, fence ControllerEpochFence) (*Publication, error)
	GetPublication(ctx context.Context, id string) (*Publication, error)
	SetPublicationPRIntent(ctx context.Context, request SetPublicationPRIntentRequest) (*Publication, error)
	SetPublicationPRReceipt(ctx context.Context, request SetPublicationPRReceiptRequest) (*Publication, error)
	TransitionPublication(ctx context.Context, transition PublicationTransition) (*Publication, error)
}

type ExternalEffectIdentityReader interface {
	GetExternalEffectByIdentity(ctx context.Context, identity ExternalEffectIdentity) (*ExternalEffect, error)
}

type ExternalEffectStore interface {
	ExternalEffectIdentityReader
	ReserveExternalEffect(ctx context.Context, request ReserveExternalEffectRequest) (*ExternalEffect, error)
	GetExternalEffect(ctx context.Context, id string) (*ExternalEffect, error)
	TransitionExternalEffect(ctx context.Context, transition ExternalEffectTransition) (*ExternalEffect, error)
}

type OutboxProjectionStore interface {
	EnqueueOutboxProjection(ctx context.Context, projection *OutboxProjection, fence ControllerEpochFence) (*OutboxProjection, error)
	GetOutboxProjection(ctx context.Context, id string) (*OutboxProjection, error)
	ClaimOutboxProjections(ctx context.Context, request ClaimOutboxProjectionsRequest) ([]OutboxProjection, error)
	CompleteOutboxProjection(ctx context.Context, request CompleteOutboxProjectionRequest) (*OutboxProjection, error)
}

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
