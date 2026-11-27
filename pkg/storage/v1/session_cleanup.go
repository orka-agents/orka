package v1

import (
	"context"
	"time"
)

type SessionCleanupPersistenceStore interface {
	SessionTurnCleanupReceiptStore
	GetSessionCleanupIntent(ctx context.Context, namespace, sessionName string) (*SessionCleanupIntent, error)
	ListSessionCleanupIntents(ctx context.Context) ([]SessionCleanupIntent, error)
	ListSessionCleanupTurns(ctx context.Context, intent SessionCleanupIntent) ([]SessionTurn, error)
	GetSessionCleanupCompletion(ctx context.Context, namespace, sessionName string) (*SessionCleanupCompletion, error)
	HasSessionCleanupFenceForUID(ctx context.Context, sessionUID string) (bool, error)
	GetSessionCleanupIdentity(ctx context.Context, namespace, sessionName string) (string, error)
	BindSessionCleanupIdentity(ctx context.Context, namespace, sessionName, sessionUID string) error
	HasSessionCleanupIntent(ctx context.Context, namespace, sessionName string) (bool, error)
	PrepareSessionCleanup(ctx context.Context, intent SessionCleanupIntent) (*SessionCleanupIntent, error)
	CompleteSessionCleanup(ctx context.Context, request CompleteSessionCleanupRequest) error
}

type SessionTurnCleanupReceiptStore interface {
	GetSessionTurnCleanupReceipt(ctx context.Context, namespace, sessionName, promptAttemptID string) (*SessionTurnCleanupReceipt, error)
}

type SessionCleanupBranchClaim struct {
	ID                    string                  `json:"id"`
	ObjectUID             string                  `json:"objectUid"`
	ExpectedVersion       int64                   `json:"expectedVersion"`
	ExpectedGeneration    int64                   `json:"expectedGeneration"`
	ExpectedRepositoryID  string                  `json:"expectedRepositoryId"`
	ExpectedRef           string                  `json:"expectedRef"`
	ExpectedOwnerUID      string                  `json:"expectedOwnerUid"`
	ExpectedLastVerified  RemoteRefState          `json:"expectedLastVerified"`
	ExpectedAvailability  BranchClaimAvailability `json:"expectedAvailability"`
	ExpectedBlockedReason string                  `json:"expectedBlockedReason,omitempty"`
	ExpectedPublicationID string                  `json:"expectedPublicationId,omitempty"`
	ExpectedRequestDigest string                  `json:"expectedRequestDigest"`
}

type SessionCleanupIntent struct {
	Namespace                      string                      `json:"namespace"`
	SessionName                    string                      `json:"sessionName"`
	SessionUID                     string                      `json:"sessionUid,omitempty"`
	ControlObjectUID               string                      `json:"controlObjectUid,omitempty"`
	ControlRequestDigest           string                      `json:"controlRequestDigest,omitempty"`
	ExpectedControlVersion         int64                       `json:"expectedControlVersion,omitempty"`
	ExpectedLeaseGeneration        int64                       `json:"expectedLeaseGeneration,omitempty"`
	ExpectedControlLastOperationID string                      `json:"expectedControlLastOperationId,omitempty"`
	ExpectedControlLastDigest      string                      `json:"expectedControlLastDigest,omitempty"`
	ExpectedVerifiedBaseline       *VerifiedBranchBaseline     `json:"expectedVerifiedBaseline,omitempty"`
	LeaseName                      string                      `json:"leaseName,omitempty"`
	LeaseObjectUID                 string                      `json:"leaseObjectUid,omitempty"`
	BranchClaims                   []SessionCleanupBranchClaim `json:"branchClaims,omitempty"`
	OperationID                    string                      `json:"operationId"`
	OperationDigest                string                      `json:"operationDigest"`
	PreparedAt                     time.Time                   `json:"preparedAt"`
	Gateway                        *GatewaySessionCleanupProof `json:"gateway,omitempty"`
}

type CompleteSessionCleanupRequest struct {
	Namespace       string `json:"namespace"`
	SessionName     string `json:"sessionName"`
	OperationID     string `json:"operationId"`
	OperationDigest string `json:"operationDigest"`
}

type SessionCleanupCompletion struct {
	Namespace       string    `json:"namespace"`
	SessionName     string    `json:"sessionName"`
	SessionUID      string    `json:"sessionUid,omitempty"`
	OperationID     string    `json:"operationId"`
	OperationDigest string    `json:"operationDigest"`
	CompletedAt     time.Time `json:"completedAt"`
}
