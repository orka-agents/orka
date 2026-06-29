/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package harness

import (
	"context"
	"time"
)

// RuntimeSessionFilter constrains internal RuntimeSession persistence queries.
type RuntimeSessionFilter struct {
	Namespace       string
	SessionName     string
	ActiveTask      string
	AgentName       string
	Provider        ProviderKind
	States          []RuntimeSessionState
	CleanupPolicies []RuntimeCleanupPolicy
	IncludeDeleted  bool
	Limit           int
	Cursor          string
}

// RuntimeSessionTransition describes an optimistic state transition for a
// namespace-owned RuntimeSession.
type RuntimeSessionTransition struct {
	Namespace string
	ID        RuntimeSessionID
	From      RuntimeSessionState
	To        RuntimeSessionState
	// ActiveTask nil preserves the existing active task. A non-nil pointer is
	// trimmed and written; an empty string clears the active task.
	ActiveTask *string
	// UpdatedAt defaults to time.Now().UTC() when zero.
	UpdatedAt time.Time
}

// RuntimeSessionStore is the internal-store-first persistence seam for
// backend-neutral runtime sessions. Implementations should return store.ErrNotFound
// for missing sessions, store.ErrConflict for stale transitions, and
// store.ErrValidation for invalid records or requests.
type RuntimeSessionStore interface {
	CreateRuntimeSession(ctx context.Context, session *RuntimeSession) error
	GetRuntimeSession(ctx context.Context, namespace string, id RuntimeSessionID) (*RuntimeSession, error)
	ListRuntimeSessions(ctx context.Context, filter RuntimeSessionFilter) ([]RuntimeSession, string, error)
	TransitionRuntimeSession(ctx context.Context, transition RuntimeSessionTransition) (*RuntimeSession, error)
	DeleteRuntimeSession(ctx context.Context, namespace string, id RuntimeSessionID) error
}
