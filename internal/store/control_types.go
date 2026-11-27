package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxControlTextBytes = 4 * 1024 * 1024
)

type ControllerEpochCAS struct {
	Name            string    `json:"name"`
	ExpectedVersion int64     `json:"expectedVersion"`
	ExpectedEpoch   int64     `json:"expectedEpoch"`
	NewEpoch        int64     `json:"newEpoch"`
	HolderID        string    `json:"holderId"`
	RequestDigest   string    `json:"requestDigest"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

type PromptAttemptKey struct {
	Namespace string `json:"namespace"`
	TaskUID   string `json:"taskUid"`
	Attempt   int64  `json:"attempt"`
	PromptID  string `json:"promptId"`
}

func (k PromptAttemptKey) CanonicalID() (string, error) {
	if err := k.Validate(); err != nil {
		return "", err
	}
	return CanonicalControlID("prompt-attempt", k.Namespace, k.TaskUID, strconv.FormatInt(k.Attempt, 10), k.PromptID), nil
}

func (k PromptAttemptKey) Validate() error {
	if err := validateControlIdentifier("prompt attempt namespace", k.Namespace); err != nil {
		return err
	}
	if err := validateControlIdentifier("prompt attempt task UID", k.TaskUID); err != nil {
		return err
	}
	if k.Attempt < 1 {
		return ValidationErrorf("prompt attempt must be at least 1")
	}
	return validateControlIdentifier("prompt ID", k.PromptID)
}

type PromptExecutionState string

const (
	PromptExecutionQueued           PromptExecutionState = "Queued"
	PromptExecutionReserved         PromptExecutionState = "Reserved"
	PromptExecutionSessionStarting  PromptExecutionState = "SessionStarting"
	PromptExecutionPlanned          PromptExecutionState = "Planned"
	PromptExecutionSubmitting       PromptExecutionState = "Submitting"
	PromptExecutionSubmittedUnknown PromptExecutionState = "SubmittedUnknown"
	PromptExecutionAccepted         PromptExecutionState = "Accepted"
	PromptExecutionRunning          PromptExecutionState = "Running"
	PromptExecutionSettling         PromptExecutionState = "Settling"
	PromptExecutionSucceeded        PromptExecutionState = "Succeeded"
	PromptExecutionFailed           PromptExecutionState = "Failed"
	PromptExecutionCancelled        PromptExecutionState = "Cancelled"
	PromptExecutionOutcomeUnknown   PromptExecutionState = "OutcomeUnknown"
)

var promptExecutionTransitions = map[PromptExecutionState]map[PromptExecutionState]struct{}{
	PromptExecutionQueued: {
		PromptExecutionReserved: {}, PromptExecutionFailed: {}, PromptExecutionCancelled: {},
	},
	PromptExecutionReserved: {
		PromptExecutionSessionStarting: {}, PromptExecutionFailed: {}, PromptExecutionCancelled: {},
	},
	PromptExecutionSessionStarting: {
		PromptExecutionPlanned: {}, PromptExecutionFailed: {}, PromptExecutionCancelled: {},
	},
	PromptExecutionPlanned: {
		PromptExecutionSubmitting: {}, PromptExecutionFailed: {}, PromptExecutionCancelled: {},
	},
	PromptExecutionSubmitting: {
		PromptExecutionAccepted: {}, PromptExecutionSubmittedUnknown: {}, PromptExecutionFailed: {}, PromptExecutionCancelled: {},
	},
	PromptExecutionSubmittedUnknown: {
		PromptExecutionOutcomeUnknown: {},
	},
	PromptExecutionAccepted: {
		PromptExecutionRunning: {}, PromptExecutionSettling: {}, PromptExecutionFailed: {}, PromptExecutionCancelled: {}, PromptExecutionOutcomeUnknown: {},
	},
	PromptExecutionRunning: {
		PromptExecutionSettling: {}, PromptExecutionFailed: {}, PromptExecutionCancelled: {}, PromptExecutionOutcomeUnknown: {},
	},
	PromptExecutionSettling: {
		PromptExecutionSucceeded: {}, PromptExecutionFailed: {}, PromptExecutionCancelled: {}, PromptExecutionOutcomeUnknown: {},
	},
}

func IsKnownPromptExecutionState(state PromptExecutionState) bool {
	switch state {
	case PromptExecutionQueued, PromptExecutionReserved, PromptExecutionSessionStarting,
		PromptExecutionPlanned, PromptExecutionSubmitting, PromptExecutionSubmittedUnknown,
		PromptExecutionAccepted, PromptExecutionRunning, PromptExecutionSettling,
		PromptExecutionSucceeded, PromptExecutionFailed, PromptExecutionCancelled,
		PromptExecutionOutcomeUnknown:
		return true
	default:
		return false
	}
}

func IsTerminalPromptExecutionState(state PromptExecutionState) bool {
	switch state {
	case PromptExecutionSucceeded, PromptExecutionFailed, PromptExecutionCancelled, PromptExecutionOutcomeUnknown:
		return true
	default:
		return false
	}
}

func ValidatePromptExecutionTransition(from, to PromptExecutionState) error {
	if !IsKnownPromptExecutionState(from) {
		return ValidationErrorf("unsupported prompt execution state %q", from)
	}
	if !IsKnownPromptExecutionState(to) {
		return ValidationErrorf("unsupported prompt execution state %q", to)
	}
	if _, ok := promptExecutionTransitions[from][to]; !ok {
		return ValidationErrorf("prompt execution transition %s -> %s is not allowed", from, to)
	}
	return nil
}

type PromptDeliveryState string

const (
	PromptDeliveryNotRequested              PromptDeliveryState = "NotRequested"
	PromptDeliveryValidating                PromptDeliveryState = "Validating"
	PromptDeliveryPreparing                 PromptDeliveryState = "Preparing"
	PromptDeliveryPrepared                  PromptDeliveryState = "Prepared"
	PromptDeliveryPublishing                PromptDeliveryState = "Publishing"
	PromptDeliveryVerifying                 PromptDeliveryState = "Verifying"
	PromptDeliveryVerifiedExact             PromptDeliveryState = "VerifiedExact"
	PromptDeliveryDeliveredSuperseded       PromptDeliveryState = "DeliveredSuperseded"
	PromptDeliveryReadValidated             PromptDeliveryState = "ReadValidated"
	PromptDeliveryNoChange                  PromptDeliveryState = "NoChange"
	PromptDeliveryCancelledBeforePublish    PromptDeliveryState = "CancelledBeforePublish"
	PromptDeliveryReadOnlyWorkspaceModified PromptDeliveryState = "ReadOnlyWorkspaceModified"
	PromptDeliveryConflict                  PromptDeliveryState = "DeliveryConflict"
	PromptDeliveryCredentialBlocked         PromptDeliveryState = "CredentialBlocked"
	PromptDeliveryPublicationOutcomeUnknown PromptDeliveryState = "PublicationOutcomeUnknown"
)

var promptDeliveryTransitions = map[PromptDeliveryState]map[PromptDeliveryState]struct{}{
	PromptDeliveryNotRequested: {PromptDeliveryValidating: {}},
	PromptDeliveryValidating: {
		PromptDeliveryPreparing: {}, PromptDeliveryReadValidated: {}, PromptDeliveryNoChange: {},
		PromptDeliveryReadOnlyWorkspaceModified: {}, PromptDeliveryCredentialBlocked: {}, PromptDeliveryConflict: {},
		PromptDeliveryPublicationOutcomeUnknown: {},
	},
	PromptDeliveryPreparing: {
		PromptDeliveryPrepared: {}, PromptDeliveryCredentialBlocked: {}, PromptDeliveryConflict: {}, PromptDeliveryPublicationOutcomeUnknown: {},
	},
	PromptDeliveryPrepared: {
		PromptDeliveryPublishing: {}, PromptDeliveryCancelledBeforePublish: {},
	},
	PromptDeliveryPublishing: {
		PromptDeliveryVerifying: {}, PromptDeliveryPublicationOutcomeUnknown: {},
	},
	PromptDeliveryVerifying: {
		PromptDeliveryVerifiedExact: {}, PromptDeliveryDeliveredSuperseded: {},
		PromptDeliveryConflict: {}, PromptDeliveryPublicationOutcomeUnknown: {},
	},
}

func IsKnownPromptDeliveryState(state PromptDeliveryState) bool {
	switch state {
	case PromptDeliveryNotRequested, PromptDeliveryValidating, PromptDeliveryPreparing,
		PromptDeliveryPrepared, PromptDeliveryPublishing, PromptDeliveryVerifying,
		PromptDeliveryVerifiedExact, PromptDeliveryDeliveredSuperseded,
		PromptDeliveryReadValidated, PromptDeliveryNoChange,
		PromptDeliveryCancelledBeforePublish, PromptDeliveryReadOnlyWorkspaceModified,
		PromptDeliveryConflict, PromptDeliveryCredentialBlocked,
		PromptDeliveryPublicationOutcomeUnknown:
		return true
	default:
		return false
	}
}

func IsTerminalPromptDeliveryState(state PromptDeliveryState) bool {
	switch state {
	case PromptDeliveryNotRequested, PromptDeliveryVerifiedExact, PromptDeliveryDeliveredSuperseded,
		PromptDeliveryReadValidated, PromptDeliveryNoChange, PromptDeliveryCancelledBeforePublish,
		PromptDeliveryReadOnlyWorkspaceModified, PromptDeliveryConflict,
		PromptDeliveryCredentialBlocked, PromptDeliveryPublicationOutcomeUnknown:
		return true
	default:
		return false
	}
}

func ValidatePromptDeliveryTransition(from, to PromptDeliveryState) error {
	if !IsKnownPromptDeliveryState(from) {
		return ValidationErrorf("unsupported prompt delivery state %q", from)
	}
	if !IsKnownPromptDeliveryState(to) {
		return ValidationErrorf("unsupported prompt delivery state %q", to)
	}
	if _, ok := promptDeliveryTransitions[from][to]; !ok {
		return ValidationErrorf("prompt delivery transition %s -> %s is not allowed", from, to)
	}
	return nil
}

type PromptCredentialRole string

const (
	PromptCredentialSourceRead  PromptCredentialRole = "SourceRead"
	PromptCredentialTargetRead  PromptCredentialRole = "TargetRead"
	PromptCredentialTargetWrite PromptCredentialRole = "TargetWrite"
	PromptCredentialForge       PromptCredentialRole = "Forge"
	PromptCredentialCommitSign  PromptCredentialRole = "CommitSign"
)

type PromptCredentialBinding struct {
	Role            PromptCredentialRole `json:"role"`
	Namespace       string               `json:"namespace"`
	SecretName      string               `json:"secretName"`
	SecretKey       string               `json:"secretKey"`
	SecretUID       string               `json:"secretUid"`
	ResourceVersion string               `json:"resourceVersion"`
}

func (b PromptCredentialBinding) Validate() error {
	switch b.Role {
	case PromptCredentialSourceRead, PromptCredentialTargetRead, PromptCredentialTargetWrite, PromptCredentialForge, PromptCredentialCommitSign:
	default:
		return ValidationErrorf("unsupported prompt credential role %q", b.Role)
	}
	for field, value := range map[string]string{
		"credential namespace": b.Namespace, "credential Secret name": b.SecretName,
		"credential Secret key": b.SecretKey, "credential Secret UID": b.SecretUID,
		"credential Secret resourceVersion": b.ResourceVersion,
	} {
		if err := validateControlIdentifier(field, value); err != nil {
			return err
		}
	}
	return nil
}

type PromptAttempt struct {
	ID                     string                    `json:"id"`
	Key                    PromptAttemptKey          `json:"key"`
	SessionUID             string                    `json:"sessionUid,omitempty"`
	SessionLeaseGeneration int64                     `json:"sessionLeaseGeneration,omitempty"`
	RuntimeInstanceID      string                    `json:"runtimeInstanceId,omitempty"`
	RequestDigest          string                    `json:"requestDigest"`
	BindingDigest          string                    `json:"bindingDigest,omitempty"`
	SnapshotDigest         string                    `json:"snapshotDigest,omitempty"`
	CredentialBindings     []PromptCredentialBinding `json:"credentialBindings,omitempty"`
	ExecutionState         PromptExecutionState      `json:"executionState"`
	DeliveryState          PromptDeliveryState       `json:"deliveryState"`
	TerminalReason         string                    `json:"terminalReason,omitempty"`
	OutcomeMarker          string                    `json:"outcomeMarker,omitempty"`
	ControllerEpochName    string                    `json:"controllerEpochName"`
	ControllerEpoch        int64                     `json:"controllerEpoch"`
	LastOperationID        string                    `json:"lastOperationId,omitempty"`
	LastOperationDigest    string                    `json:"lastOperationDigest,omitempty"`
	Version                int64                     `json:"version"`
	CreatedAt              time.Time                 `json:"createdAt"`
	UpdatedAt              time.Time                 `json:"updatedAt"`
}

type PromptAttemptExecutionTransition struct {
	ID                     string               `json:"id"`
	Fence                  ControllerEpochFence `json:"fence"`
	ExpectedVersion        int64                `json:"expectedVersion"`
	ExpectedState          PromptExecutionState `json:"expectedState"`
	NewState               PromptExecutionState `json:"newState"`
	OperationID            string               `json:"operationId"`
	OperationDigest        string               `json:"operationDigest"`
	RuntimeInstanceID      string               `json:"runtimeInstanceId,omitempty"`
	SessionUID             string               `json:"sessionUid,omitempty"`
	SessionLeaseGeneration int64                `json:"sessionLeaseGeneration,omitempty"`
	TerminalReason         string               `json:"terminalReason,omitempty"`
	OutcomeMarker          string               `json:"outcomeMarker,omitempty"`
	UpdatedAt              time.Time            `json:"updatedAt"`
}

type PromptAttemptPreSubmissionRecovery struct {
	ID                string               `json:"id"`
	Fence             ControllerEpochFence `json:"fence"`
	ExpectedVersion   int64                `json:"expectedVersion"`
	ExpectedState     PromptExecutionState `json:"expectedState"`
	ProvenNotAccepted bool                 `json:"provenNotAccepted,omitempty"`
	PreserveBindings  bool                 `json:"preserveBindings,omitempty"`
	OperationID       string               `json:"operationId"`
	OperationDigest   string               `json:"operationDigest"`
	RecoveredAt       time.Time            `json:"recoveredAt"`
}

type PromptAttemptDeliveryTransition struct {
	ID              string               `json:"id"`
	Fence           ControllerEpochFence `json:"fence"`
	ExpectedVersion int64                `json:"expectedVersion"`
	ExpectedState   PromptDeliveryState  `json:"expectedState"`
	NewState        PromptDeliveryState  `json:"newState"`
	OperationID     string               `json:"operationId"`
	OperationDigest string               `json:"operationDigest"`
	TerminalReason  string               `json:"terminalReason,omitempty"`
	UpdatedAt       time.Time            `json:"updatedAt"`
}

type PromptAttemptReclamationMode string

const (
	PromptAttemptReclamationProjected PromptAttemptReclamationMode = "Projected"
	PromptAttemptReclamationUnbound   PromptAttemptReclamationMode = "Unbound"
	PromptAttemptReclamationNoAttempt PromptAttemptReclamationMode = "NoAttempt"
)

type ReclaimPromptAttemptsRequest struct {
	Namespace                         string                       `json:"namespace"`
	TaskName                          string                       `json:"taskName"`
	TaskUID                           string                       `json:"taskUid"`
	Mode                              PromptAttemptReclamationMode `json:"mode"`
	ContinuitySession                 bool                         `json:"continuitySession,omitempty"`
	FinalContinuitySession            bool                         `json:"finalContinuitySession,omitempty"`
	FinalPromptAttemptID              string                       `json:"finalPromptAttemptId,omitempty"`
	TerminalProjectionID              string                       `json:"terminalProjectionId,omitempty"`
	RelatedExternalEffectAggregateIDs []string                     `json:"relatedExternalEffectAggregateIds,omitempty"`
	Fence                             ControllerEpochFence         `json:"fence"`
}

type SessionAvailability string

const (
	SessionAvailable             SessionAvailability = "Available"
	SessionReconciliationBlocked SessionAvailability = "ReconciliationBlocked"
)

type SessionMutationLease struct {
	Generation    int64      `json:"generation"`
	TaskUID       string     `json:"taskUid"`
	Attempt       int64      `json:"attempt"`
	PromptID      string     `json:"promptId"`
	RequestDigest string     `json:"requestDigest"`
	AcquiredAt    time.Time  `json:"acquiredAt"`
	ExpiresAt     *time.Time `json:"expiresAt,omitempty"`
}

type SessionControl struct {
	Namespace                string                  `json:"namespace"`
	SessionName              string                  `json:"sessionName"`
	SessionUID               string                  `json:"sessionUid"`
	RequestDigest            string                  `json:"requestDigest"`
	Availability             SessionAvailability     `json:"availability"`
	RuntimeSessionGeneration int64                   `json:"runtimeSessionGeneration,omitempty"`
	LeaseGeneration          int64                   `json:"leaseGeneration"`
	Lease                    *SessionMutationLease   `json:"lease,omitempty"`
	BlockedReason            string                  `json:"blockedReason,omitempty"`
	RelatedPromptAttemptID   string                  `json:"relatedPromptAttemptId,omitempty"`
	RelatedPublicationID     string                  `json:"relatedPublicationId,omitempty"`
	VerifiedBaseline         *VerifiedBranchBaseline `json:"verifiedBaseline,omitempty"`
	Lineage                  *SessionLineage         `json:"lineage,omitempty"`
	ControllerEpochName      string                  `json:"controllerEpochName"`
	ControllerEpoch          int64                   `json:"controllerEpoch"`
	LastOperationID          string                  `json:"lastOperationId,omitempty"`
	LastOperationDigest      string                  `json:"lastOperationDigest,omitempty"`
	Version                  int64                   `json:"version"`
	CreatedAt                time.Time               `json:"createdAt"`
	UpdatedAt                time.Time               `json:"updatedAt"`
}

type CommitSessionRuntimeGenerationRequest struct {
	Namespace              string               `json:"namespace"`
	SessionName            string               `json:"sessionName"`
	SessionUID             string               `json:"sessionUid"`
	Key                    SessionTurnKey       `json:"key"`
	Fence                  ControllerEpochFence `json:"fence"`
	ExpectedSessionVersion int64                `json:"expectedSessionVersion"`
	Generation             int64                `json:"generation"`
	CommittedAt            time.Time            `json:"committedAt"`
}

type AcquireSessionMutationLeaseRequest struct {
	Namespace               string                      `json:"namespace"`
	SessionName             string                      `json:"sessionName"`
	SessionUID              string                      `json:"sessionUid"`
	Fence                   ControllerEpochFence        `json:"fence"`
	ExpectedVersion         int64                       `json:"expectedVersion"`
	ExpectedLeaseGeneration int64                       `json:"expectedLeaseGeneration"`
	TaskUID                 string                      `json:"taskUid"`
	Attempt                 int64                       `json:"attempt"`
	PromptID                string                      `json:"promptId"`
	RequestDigest           string                      `json:"requestDigest"`
	AcquiredAt              time.Time                   `json:"acquiredAt"`
	ExpiresAt               *time.Time                  `json:"expiresAt,omitempty"`
	Lineage                 *ClaimSessionLineageRequest `json:"lineage,omitempty"`
}

type ReleaseSessionMutationLeaseRequest struct {
	Namespace              string               `json:"namespace"`
	SessionName            string               `json:"sessionName"`
	SessionUID             string               `json:"sessionUid"`
	Key                    SessionTurnKey       `json:"key"`
	Fence                  ControllerEpochFence `json:"fence"`
	ExpectedSessionVersion int64                `json:"expectedSessionVersion"`
	LeaseRequestDigest     string               `json:"leaseRequestDigest"`
	OperationID            string               `json:"operationId"`
	OperationDigest        string               `json:"operationDigest"`
	ReleasedAt             time.Time            `json:"releasedAt"`
}

type ReconcileSessionControlRequest struct {
	Namespace                     string                 `json:"namespace"`
	SessionName                   string                 `json:"sessionName"`
	SessionUID                    string                 `json:"sessionUid"`
	Fence                         ControllerEpochFence   `json:"fence"`
	ExpectedVersion               int64                  `json:"expectedVersion"`
	ExpectedLeaseGeneration       int64                  `json:"expectedLeaseGeneration"`
	ExpectedRelatedPublicationID  string                 `json:"expectedRelatedPublicationId"`
	BranchClaimID                 string                 `json:"branchClaimId"`
	ExpectedBranchClaimVersion    int64                  `json:"expectedBranchClaimVersion"`
	ExpectedBranchClaimGeneration int64                  `json:"expectedBranchClaimGeneration"`
	ExpectedBranchBaseline        RemoteRefState         `json:"expectedBranchBaseline"`
	VerifiedBaseline              VerifiedBranchBaseline `json:"verifiedBaseline"`
	OperationID                   string                 `json:"operationId"`
	OperationDigest               string                 `json:"operationDigest"`
	ReconciledAt                  time.Time              `json:"reconciledAt"`
}

type CreateSessionTurnRequest struct {
	Turn                   SessionTurn          `json:"turn"`
	Fence                  ControllerEpochFence `json:"fence"`
	ExpectedSessionVersion int64                `json:"expectedSessionVersion"`
}

type FinalizeSessionTurnRequest struct {
	Key                    SessionTurnKey          `json:"key"`
	Fence                  ControllerEpochFence    `json:"fence"`
	ExpectedSessionVersion int64                   `json:"expectedSessionVersion"`
	ExpectedTurnVersion    int64                   `json:"expectedTurnVersion"`
	FinalizationDigest     string                  `json:"finalizationDigest"`
	TerminalKind           SessionTurnTerminalKind `json:"terminalKind"`
	TerminalContent        string                  `json:"terminalContent"`
	SkipTranscriptAppend   bool                    `json:"skipTranscriptAppend,omitempty"`
	SkipUserPromptAppend   bool                    `json:"skipUserPromptAppend,omitempty"`
	PublicationID          string                  `json:"publicationId,omitempty"`
	VerifiedBaseline       *VerifiedBranchBaseline `json:"verifiedBaseline,omitempty"`
	BlockReason            string                  `json:"blockReason,omitempty"`
	Projection             OutboxProjection        `json:"projection"`
	FinalizedAt            time.Time               `json:"finalizedAt"`
	NativeSession          *NativeSessionRecord    `json:"-"`
	NativeSessionCarried   bool                    `json:"-"`
}

type ResumeSessionTurnFinalizationRequest struct {
	Key                SessionTurnKey       `json:"key"`
	PromptAttemptID    string               `json:"promptAttemptId"`
	FinalizationDigest string               `json:"finalizationDigest"`
	Fence              ControllerEpochFence `json:"fence"`
}

type BranchClaimOwnerKind string

const (
	BranchClaimOwnerTask    BranchClaimOwnerKind = "Task"
	BranchClaimOwnerSession BranchClaimOwnerKind = "Session"
)

type BranchClaim struct {
	ID                   string                  `json:"id"`
	RepositoryID         string                  `json:"repositoryId"`
	Ref                  string                  `json:"ref"`
	OwnerKind            BranchClaimOwnerKind    `json:"ownerKind"`
	OwnerUID             string                  `json:"ownerUid"`
	Generation           int64                   `json:"generation"`
	LastVerified         RemoteRefState          `json:"lastVerified"`
	Availability         BranchClaimAvailability `json:"availability"`
	BlockedReason        string                  `json:"blockedReason,omitempty"`
	RelatedPublicationID string                  `json:"relatedPublicationId,omitempty"`
	RequestDigest        string                  `json:"requestDigest"`
	ControllerEpochName  string                  `json:"controllerEpochName"`
	ControllerEpoch      int64                   `json:"controllerEpoch"`
	LastOperationID      string                  `json:"lastOperationId,omitempty"`
	LastOperationDigest  string                  `json:"lastOperationDigest,omitempty"`
	Version              int64                   `json:"version"`
	CreatedAt            time.Time               `json:"createdAt"`
	UpdatedAt            time.Time               `json:"updatedAt"`
}

func CanonicalBranchClaimID(repositoryID, ref string) (string, error) {
	if err := validateControlIdentifier("publication repository ID", repositoryID); err != nil {
		return "", err
	}
	if err := ValidateFullBranchRef(ref); err != nil {
		return "", err
	}
	return CanonicalControlID("branch-claim", repositoryID, ref), nil
}

type BranchClaimCAS struct {
	ID                   string                  `json:"id"`
	Fence                ControllerEpochFence    `json:"fence"`
	ExpectedVersion      int64                   `json:"expectedVersion"`
	ExpectedGeneration   int64                   `json:"expectedGeneration"`
	NewGeneration        int64                   `json:"newGeneration"`
	ExpectedLastVerified RemoteRefState          `json:"expectedLastVerified"`
	NewLastVerified      RemoteRefState          `json:"newLastVerified"`
	ExpectedAvailability BranchClaimAvailability `json:"expectedAvailability"`
	NewAvailability      BranchClaimAvailability `json:"newAvailability"`
	BlockedReason        string                  `json:"blockedReason,omitempty"`
	RelatedPublicationID string                  `json:"relatedPublicationId,omitempty"`
	OperationID          string                  `json:"operationId"`
	OperationDigest      string                  `json:"operationDigest"`
	UpdatedAt            time.Time               `json:"updatedAt"`
}

type ReclaimBranchClaimRequest struct {
	ID                    string                  `json:"id"`
	Fence                 ControllerEpochFence    `json:"fence"`
	ExpectedVersion       int64                   `json:"expectedVersion"`
	ExpectedGeneration    int64                   `json:"expectedGeneration"`
	ExpectedRepositoryID  string                  `json:"expectedRepositoryId"`
	ExpectedRef           string                  `json:"expectedRef"`
	ExpectedOwnerKind     BranchClaimOwnerKind    `json:"expectedOwnerKind"`
	ExpectedOwnerUID      string                  `json:"expectedOwnerUid"`
	ExpectedLastVerified  RemoteRefState          `json:"expectedLastVerified"`
	ExpectedAvailability  BranchClaimAvailability `json:"expectedAvailability"`
	ExpectedRequestDigest string                  `json:"expectedRequestDigest"`
}

var publicationTransitions = map[PublicationState]map[PublicationState]struct{}{
	PublicationPreparing: {
		PublicationPrepared: {}, PublicationCredentialBlocked: {}, PublicationPreparationFailed: {}, PublicationDeliveryConflict: {}, PublicationOutcomeUnknown: {},
	},
	PublicationPrepared: {
		PublicationPublishing: {}, PublicationCancelledBeforePublish: {},
	},
	PublicationPublishing: {
		PublicationVerifying: {}, PublicationOutcomeUnknown: {},
	},
	PublicationVerifying: {
		PublicationVerifiedExact: {}, PublicationDeliveredSuperseded: {}, PublicationDeliveryConflict: {}, PublicationOutcomeUnknown: {},
	},
}

func IsKnownPublicationState(state PublicationState) bool {
	switch state {
	case PublicationPreparing, PublicationPrepared, PublicationPublishing, PublicationVerifying,
		PublicationVerifiedExact, PublicationDeliveredSuperseded,
		PublicationCancelledBeforePublish, PublicationDeliveryConflict,
		PublicationCredentialBlocked, PublicationPreparationFailed, PublicationOutcomeUnknown:
		return true
	default:
		return false
	}
}

func ValidatePublicationTransition(from, to PublicationState) error {
	if !IsKnownPublicationState(from) {
		return ValidationErrorf("unsupported publication state %q", from)
	}
	if !IsKnownPublicationState(to) {
		return ValidationErrorf("unsupported publication state %q", to)
	}
	if _, ok := publicationTransitions[from][to]; !ok {
		return ValidationErrorf("publication transition %s -> %s is not allowed", from, to)
	}
	return nil
}

const PreparedBundleMediaType = "application/vnd.orka.git-bundle.v1"

type Publication struct {
	ID                       string                          `json:"id"`
	Namespace                string                          `json:"namespace"`
	Generation               int64                           `json:"generation"`
	TaskUID                  string                          `json:"taskUid"`
	Attempt                  int64                           `json:"attempt"`
	PromptID                 string                          `json:"promptId"`
	SessionUID               string                          `json:"sessionUid,omitempty"`
	BranchClaimID            string                          `json:"branchClaimId"`
	BranchClaimGeneration    int64                           `json:"branchClaimGeneration"`
	SourceRepositoryID       string                          `json:"sourceRepositoryId"`
	SourceRef                string                          `json:"sourceRef"`
	SourceBaselineSHA        string                          `json:"sourceBaselineSha"`
	TargetRepositoryID       string                          `json:"targetRepositoryId"`
	TargetRef                string                          `json:"targetRef"`
	Baseline                 RemoteRefState                  `json:"baseline"`
	ArtifactID               string                          `json:"artifactId"`
	ArtifactDigest           string                          `json:"artifactDigest"`
	ArtifactSizeBytes        int64                           `json:"artifactSizeBytes"`
	ArtifactMediaType        string                          `json:"artifactMediaType"`
	PublicationCredentialRef string                          `json:"publicationCredentialRef"`
	CommitIdentity           string                          `json:"commitIdentity"`
	CommitMessage            string                          `json:"commitMessage"`
	CommitTimestamp          time.Time                       `json:"commitTimestamp"`
	PRIntent                 *PullRequestIntent              `json:"prIntent,omitempty"`
	RequestDigest            string                          `json:"requestDigest"`
	State                    PublicationState                `json:"state"`
	PreparedReceipt          *PreparedPublicationReceipt     `json:"preparedReceipt,omitempty"`
	PublishReceipt           *PublishOperationReceipt        `json:"publishReceipt,omitempty"`
	VerificationReceipt      *PublicationVerificationReceipt `json:"verificationReceipt,omitempty"`
	PullRequestReceipt       *PullRequestOperationReceipt    `json:"pullRequestReceipt,omitempty"`
	TerminalReason           string                          `json:"terminalReason,omitempty"`
	ControllerEpochName      string                          `json:"controllerEpochName"`
	ControllerEpoch          int64                           `json:"controllerEpoch"`
	LastOperationID          string                          `json:"lastOperationId,omitempty"`
	LastOperationDigest      string                          `json:"lastOperationDigest,omitempty"`
	Version                  int64                           `json:"version"`
	CreatedAt                time.Time                       `json:"createdAt"`
	UpdatedAt                time.Time                       `json:"updatedAt"`
}

type PublicationTransition struct {
	ID                  string                          `json:"id"`
	Fence               ControllerEpochFence            `json:"fence"`
	ExpectedVersion     int64                           `json:"expectedVersion"`
	ExpectedGeneration  int64                           `json:"expectedGeneration"`
	ExpectedState       PublicationState                `json:"expectedState"`
	NewState            PublicationState                `json:"newState"`
	OperationID         string                          `json:"operationId"`
	OperationDigest     string                          `json:"operationDigest"`
	PreparedReceipt     *PreparedPublicationReceipt     `json:"preparedReceipt,omitempty"`
	PublishReceipt      *PublishOperationReceipt        `json:"publishReceipt,omitempty"`
	VerificationReceipt *PublicationVerificationReceipt `json:"verificationReceipt,omitempty"`
	TerminalReason      string                          `json:"terminalReason,omitempty"`
	UpdatedAt           time.Time                       `json:"updatedAt"`
}

type SetPublicationPRIntentRequest struct {
	ID                 string               `json:"id"`
	Fence              ControllerEpochFence `json:"fence"`
	ExpectedVersion    int64                `json:"expectedVersion"`
	ExpectedGeneration int64                `json:"expectedGeneration"`
	ExpectedState      PublicationState     `json:"expectedState"`
	Intent             PullRequestIntent    `json:"intent"`
	OperationID        string               `json:"operationId"`
	OperationDigest    string               `json:"operationDigest"`
	UpdatedAt          time.Time            `json:"updatedAt"`
}

type SetPublicationPRReceiptRequest struct {
	ID                 string                      `json:"id"`
	Fence              ControllerEpochFence        `json:"fence"`
	ExpectedVersion    int64                       `json:"expectedVersion"`
	ExpectedGeneration int64                       `json:"expectedGeneration"`
	ExpectedState      PublicationState            `json:"expectedState"`
	Receipt            PullRequestOperationReceipt `json:"receipt"`
	OperationID        string                      `json:"operationId"`
	OperationDigest    string                      `json:"operationDigest"`
	UpdatedAt          time.Time                   `json:"updatedAt"`
}

type ExternalEffectState string

const (
	ExternalEffectPending        ExternalEffectState = "Pending"
	ExternalEffectInFlight       ExternalEffectState = "InFlight"
	ExternalEffectSucceeded      ExternalEffectState = "Succeeded"
	ExternalEffectFailed         ExternalEffectState = "Failed"
	ExternalEffectOutcomeUnknown ExternalEffectState = "OutcomeUnknown"
)

type ExternalEffectIdentity struct {
	Kind        string `json:"kind"`
	Namespace   string `json:"namespace"`
	AggregateID string `json:"aggregateId"`
	OperationID string `json:"operationId"`
}

func (i ExternalEffectIdentity) CanonicalID() (string, error) {
	if err := i.Validate(); err != nil {
		return "", err
	}
	return CanonicalControlID("external-effect", i.Kind, i.Namespace, i.AggregateID, i.OperationID), nil
}

func (i ExternalEffectIdentity) Validate() error {
	if err := validateControlIdentifier("external effect kind", i.Kind); err != nil {
		return err
	}
	if err := validateControlIdentifier("external effect namespace", i.Namespace); err != nil {
		return err
	}
	if err := validateControlIdentifier("external effect aggregate ID", i.AggregateID); err != nil {
		return err
	}
	return validateControlIdentifier("external effect operation ID", i.OperationID)
}

type ExternalEffect struct {
	ID                  string                 `json:"id"`
	Identity            ExternalEffectIdentity `json:"identity"`
	RequestDigest       string                 `json:"requestDigest"`
	State               ExternalEffectState    `json:"state"`
	ResponseDigest      string                 `json:"responseDigest,omitempty"`
	Response            json.RawMessage        `json:"response,omitempty"`
	LeaseOwner          string                 `json:"leaseOwner,omitempty"`
	LeaseExpiresAt      *time.Time             `json:"leaseExpiresAt,omitempty"`
	Attempts            int64                  `json:"attempts"`
	ControllerEpochName string                 `json:"controllerEpochName"`
	ControllerEpoch     int64                  `json:"controllerEpoch"`
	Version             int64                  `json:"version"`
	CreatedAt           time.Time              `json:"createdAt"`
	UpdatedAt           time.Time              `json:"updatedAt"`
}

type ReserveExternalEffectRequest struct {
	Identity        ExternalEffectIdentity `json:"identity"`
	RequestDigest   string                 `json:"requestDigest"`
	Fence           ControllerEpochFence   `json:"fence"`
	CreatedAt       time.Time              `json:"createdAt"`
	ApprovalTaskUID string                 `json:"approvalTaskUid,omitempty"`
}

type ExternalEffectTransition struct {
	ID                 string               `json:"id"`
	Fence              ControllerEpochFence `json:"fence"`
	ExpectedVersion    int64                `json:"expectedVersion"`
	ExpectedState      ExternalEffectState  `json:"expectedState"`
	NewState           ExternalEffectState  `json:"newState"`
	RequestDigest      string               `json:"requestDigest"`
	ResponseDigest     string               `json:"responseDigest,omitempty"`
	Response           json.RawMessage      `json:"response,omitempty"`
	ExpectedLeaseOwner string               `json:"expectedLeaseOwner,omitempty"`
	LeaseOwner         string               `json:"leaseOwner,omitempty"`
	LeaseExpiresAt     *time.Time           `json:"leaseExpiresAt,omitempty"`
	UpdatedAt          time.Time            `json:"updatedAt"`
}

func SessionMutationLeaseRequestDigest(
	sessionUID string,
	leaseGeneration int64,
	taskUID string,
	attempt int64,
	promptID string,
	promptRequestDigest string,
) (string, error) {
	key := SessionTurnKey{
		SessionUID: sessionUID, LeaseGeneration: leaseGeneration,
		TaskUID: taskUID, Attempt: attempt, PromptID: promptID,
	}
	if err := key.Validate(); err != nil {
		return "", err
	}
	if err := ValidateCanonicalDigest("prompt request digest", promptRequestDigest); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(map[string]any{
		"sessionUID": sessionUID, "leaseGeneration": leaseGeneration,
		"taskUID": taskUID, "attempt": attempt, "promptID": promptID,
		"promptRequestDigest": promptRequestDigest,
	})
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = h.Write([]byte("orka.acp.session-mutation-lease\x00"))
	_, _ = h.Write(canonical)
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func SessionLeaseReleaseOperationDigest(turnID, leaseRequestDigest string) (string, error) {
	turnID = strings.TrimSpace(turnID)
	if err := ValidateControlIdentifier("session turn ID", turnID); err != nil {
		return "", err
	}
	if err := ValidateCanonicalDigest("session lease request digest", leaseRequestDigest); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(map[string]string{
		"leaseRequestDigest": leaseRequestDigest,
		"turnID":             turnID,
	})
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = h.Write([]byte("orka.acp.session-lease-release\x00"))
	_, _ = h.Write(canonical)
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func ValidateWorkspaceRelativeRoot(value string) error {
	if value == "" || value == "." {
		return nil
	}
	if value != strings.TrimSpace(value) || len(value) > maxControlIdentifierBytes || path.IsAbs(value) || strings.Contains(value, `\`) {
		return ValidationErrorf("workspace relative root is not canonical")
	}
	clean := path.Clean(value)
	if clean != value || clean == ".." || strings.HasPrefix(clean, "../") {
		return ValidationErrorf("workspace relative root is not canonical")
	}
	return nil
}

func ValidateControlText(field, value string) error {
	if len(value) > maxControlTextBytes {
		return ValidationErrorf("%s exceeds %d bytes", field, maxControlTextBytes)
	}
	if !utf8.ValidString(value) {
		return ValidationErrorf("%s must be valid UTF-8", field)
	}
	return nil
}
