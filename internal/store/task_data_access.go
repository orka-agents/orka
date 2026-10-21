package store

import (
	"context"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// ErrTaskDataCleanupChanged requires fresh authorization after concurrent cleanup.
var ErrTaskDataCleanupChanged = storagev1.ErrTaskDataCleanupChanged

type (
	TaskDataFence      = storagev1.TaskDataFence
	TaskDataFenceStore = storagev1.TaskDataFenceStore
)

// WithTaskDataFence returns a context that carries fence to fence-checking
// store methods.
func WithTaskDataFence(ctx context.Context, fence TaskDataFence) context.Context {
	return storagev1.WithTaskDataFence(ctx, fence)
}

// TaskDataFenceFrom returns the fence carried by ctx, if any.
func TaskDataFenceFrom(ctx context.Context) (TaskDataFence, bool) {
	return storagev1.TaskDataFenceFrom(ctx)
}

// TaskDataAccessStore orders task data access against Task finalizer cleanup
// without holding a provider transaction across Orka's work. The storage
// client implements it over the contract's TaskDataFenceStore.
//
// WithAuthorizedTaskDataAccess resolves the cleanup fence, runs live
// Kubernetes authorization with no provider transaction open, and then runs
// access with the fence and the authorized Job identities in its context. A
// nonempty task name fences data belonging only to that Task. An empty task
// name uses the namespace fence for shared messages, events, and transcripts.
// Request bodies must be fully read first.
//
// Each fence-checking store method that access calls checks the fence in the
// same transaction as its own reads and writes (see
// storagev1.TaskDataFenceStore for the methods), and returns
// ErrTaskDataCleanupChanged or ErrTaskJobRevoked once cleanup or revocation
// has moved past the authorization. The calls are separate operations, not
// one transaction: each one either completes before an invalidating cleanup,
// which then removes its effect, or is refused. Callers retry
// ErrTaskDataCleanupChanged with fresh authorization; when access can write
// more than once, its writes must be safe to repeat.
//
// A Kubernetes write made inside access is checked against the fence before
// access runs, not serialized with a later cleanup or revocation, so it must
// keep its own Kubernetes preconditions.
//
// WithTaskDataAccess does the same for trusted controller code, which needs
// no caller authorization. A Task it registers stays registered only for the
// duration of the access.
type TaskDataAccessStore interface {
	TaskJobAuthorityStore
	WithAuthorizedTaskDataAccess(
		ctx context.Context,
		namespace, taskName string,
		authorize func(context.Context) ([]TaskJobIdentity, error),
		access func(context.Context) error,
	) error
	WithTaskDataAccess(ctx context.Context, namespace, taskName string, access func(context.Context) error) error
}
