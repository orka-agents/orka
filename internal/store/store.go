package store

import (
	"context"
	"errors"
)

// ErrNotFound is returned when a requested resource does not exist.
var ErrNotFound = errors.New("not found")

// HealthChecker can verify its underlying storage is reachable.
type HealthChecker interface {
	HealthCheck(ctx context.Context) error
}

// ResultStore handles task result persistence.
type ResultStore interface {
	SaveResult(ctx context.Context, namespace, taskName string, data []byte) error
	GetResult(ctx context.Context, namespace, taskName string) ([]byte, error)
	DeleteResult(ctx context.Context, namespace, taskName string) error
}

// SessionStore handles session transcript persistence.
type SessionStore interface {
	CreateSession(ctx context.Context, session *SessionRecord) error
	GetSession(ctx context.Context, namespace, name string) (*SessionRecord, error)
	ListSessions(ctx context.Context, namespace string) ([]SessionMetadata, error)
	DeleteSession(ctx context.Context, namespace, name string) error

	// Locking
	AcquireLock(ctx context.Context, namespace, name, taskName string) error
	ReleaseLock(ctx context.Context, namespace, name, taskName string) error
	IsLocked(ctx context.Context, namespace, name, currentTask string) (bool, error)

	// Transcript
	AppendMessages(ctx context.Context, namespace, name string, messages []SessionMessage) error
	LoadTranscript(ctx context.Context, namespace, name string, maxMessages int) ([]SessionMessage, error)

	// Token tracking
	UpdateTokenCounts(ctx context.Context, namespace, name string, inputTokens, outputTokens int) error
}

// PlanStore handles autonomous plan state persistence.
type PlanStore interface {
	SavePlan(ctx context.Context, namespace, taskName string, plan *PlanState) error
	GetPlan(ctx context.Context, namespace, taskName string) (*PlanState, error)
	DeletePlan(ctx context.Context, namespace, taskName string) error
}
