package store

import (
	"context"
	"errors"
)

// ErrTaskDataCleanupChanged requires fresh authorization after concurrent cleanup.
var ErrTaskDataCleanupChanged = errors.New("task data cleanup changed during authorization")

// ErrTaskJobRevoked rejects authority from a completed or superseded Job.
var ErrTaskJobRevoked = errors.New("task Job authority revoked")

type TaskJobIdentity struct {
	Namespace string
	TaskUID   string
	JobUID    string
}

// TaskJobAuthorityStore revokes a Job before its Task binding or active state
// changes. Revocations survive ordinary data cleanup and are reclaimed only by
// the Task finalizer, together with a fresh cleanup generation.
type TaskJobAuthorityStore interface {
	RevokeTaskJob(context.Context, TaskJobIdentity) error
	CheckTaskJobAuthority(context.Context, TaskJobIdentity) error
	DeleteTaskJobRevocations(context.Context, string, string, string) error
}

// TaskDataTransactionStore serializes task data access with Task finalizer
// cleanup. WithAuthorizedTaskDataTransaction runs live Kubernetes authorization
// without a database transaction, then verifies that cleanup has not invalidated
// that proof before accessing data. A nonempty task name fences data belonging
// only to that Task. An empty task name uses the namespace fence for shared
// messages, events, and transcripts. Request bodies must be fully read first.
// Data callbacks use the transactional context and must not make network calls.
//
// The transactional context supports SaveResult, SaveArtifact, SavePlan,
// GetPlan, SendMessage, GetMessages, GetSession, GetSessionType, LoadTranscript,
// LoadTranscriptThrough, SearchTranscript, GetGatewayEventForTask,
// AppendExecutionEvent (including deduplicated and plan-aware variants), and
// ListHarnessV1AttemptsByTask and CheckTaskJobAuthority. Other store methods must
// not be called from the callback.
type TaskDataTransactionStore interface {
	TaskJobAuthorityStore
	WithTaskDataTransaction(context.Context, func(context.Context) error) error
	WithAuthorizedTaskDataTransaction(context.Context, string, string, func(context.Context) error, func(context.Context) error) error
}
