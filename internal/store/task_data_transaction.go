package store

import "context"

// TaskDataTransactionStore serializes task data access with Task finalizer
// cleanup. The callback must revalidate the live Task identity after entering
// the transaction and use its context for every store operation. Request bodies
// must be fully read before entering the transaction.
//
// The transactional context supports SaveResult, SaveArtifact, SavePlan,
// SendMessage, GetMessages, AppendExecutionEvent (including deduplicated and
// plan-aware variants), and ListHarnessV1AttemptsByTask. Other store methods
// must not be called from the callback.
type TaskDataTransactionStore interface {
	WithTaskDataTransaction(context.Context, func(context.Context) error) error
}
