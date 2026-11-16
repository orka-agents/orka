package store

import (
	"context"
	"time"
)

// SessionCleanupStore coordinates deletion across the Kubernetes-authoritative
// ACP Session records and the SQLite-owned transcript state.
type SessionCleanupStore interface {
	ReclaimSession(ctx context.Context, request ReclaimSessionRequest) error
}

// SessionCleanupRecoveryStore resumes durable cleanup intents after controller
// restart without requiring the original DELETE caller to retry.
type SessionCleanupRecoveryStore interface {
	ResumeSessionCleanups(ctx context.Context, fence ControllerEpochFence) error
}

// SessionRuntimeCleanupFunc retires resident runtime state after the durable
// intent has fenced new work and before Session authority is removed.
// It runs outside the controller-epoch mutation lock. Runtime mutations must
// revalidate the supplied fence; short receipt writes must acquire that lock.
type SessionRuntimeCleanupFunc func(context.Context, SessionCleanupIntent, ControllerEpochFence) error

// ReclaimSessionRequest identifies one idempotent user-requested Session
// deletion under the current controller epoch.
type ReclaimSessionRequest struct {
	Namespace       string               `json:"namespace"`
	SessionName     string               `json:"sessionName"`
	Fence           ControllerEpochFence `json:"fence"`
	OperationID     string               `json:"operationId"`
	OperationDigest string               `json:"operationDigest"`
	RequestedAt     time.Time            `json:"requestedAt"`
}
