package store

import (
	"context"
	"errors"
)

// ErrTaskDataCleanupChanged requires fresh authorization after concurrent cleanup.
var ErrTaskDataCleanupChanged = errors.New("task data cleanup changed during authorization")

// TaskDataTransactionStore serializes task data access with Task finalizer
// cleanup. WithAuthorizedTaskDataTransaction runs live Kubernetes authorization
// without a database transaction, then verifies that cleanup has not invalidated
// that proof before accessing data. A namespace fence covers coordination reads
// spanning multiple Tasks and sessions. Request bodies must be fully read first.
// Data callbacks use the transactional context and must not make network calls.
//
// The transactional context supports SaveResult, SaveArtifact, SavePlan,
// GetPlan, SendMessage, GetMessages, GetSession, GetSessionType, LoadTranscript,
// LoadTranscriptThrough, SearchTranscript, GetGatewayEventForTask,
// AppendExecutionEvent (including deduplicated and plan-aware variants), and
// ListHarnessV1AttemptsByTask. Other store methods must not be called from the
// callback.
type TaskDataTransactionStore interface {
	WithTaskDataTransaction(context.Context, func(context.Context) error) error
	WithAuthorizedTaskDataTransaction(context.Context, string, func(context.Context) error, func(context.Context) error) error
}
