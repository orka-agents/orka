package v1

import (
	"context"
	"errors"
	"slices"
)

// ErrTaskJobRevoked rejects authority from a completed or superseded Job.
var ErrTaskJobRevoked = errors.New("task Job authority revoked")

// ErrTaskDataCleanupChanged rejects task data access whose fence is stale:
// cleanup changed the fenced data after Orka resolved the fence. The caller
// authorizes again with a fresh fence.
var ErrTaskDataCleanupChanged = errors.New("task data cleanup changed during authorization")

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

// TaskDataFence binds task data access to the cleanup state Orka authorized
// it against. Orka resolves a fence, authorizes the caller against live
// Kubernetes state with no provider transaction open, and then sends the
// fence with each data operation in its context (see [WithTaskDataFence]).
type TaskDataFence struct {
	Namespace string
	// TaskName selects the fence of data that belongs to one Task, such as
	// its result, plan and artifacts. An empty TaskName selects the
	// namespace fence, used for data shared between Tasks: messages,
	// execution events and transcripts.
	TaskName string
	// Generation is the cleanup generation the fence was resolved at.
	Generation int64
	// Jobs are the Job authorities the access was authorized with. A fenced
	// operation is refused once any of them is revoked.
	Jobs []TaskJobIdentity
	// HarnessV1Attempt, when set, is the harness-v1 attempt the access was
	// authorized for. A fenced operation is refused once that attempt is no
	// longer the Task's latest attempt or has left the active states
	// (Submitting, SubmittedUnknown, Accepted and Running).
	HarnessV1Attempt *HarnessV1AttemptKey
}

// TaskDataFenceStore orders task data access against cleanup without a
// transaction that spans Orka's authorization.
//
// Each deletion of Task data (results, artifacts, plans, messages, execution
// events, Sessions and Session cleanup) advances the namespace's cleanup
// generation, and that of each Task whose data it deletes, in the same
// transaction as the deletion. DeleteTaskJobRevocations, the finalizer's last
// step, also advances the namespace generation and removes the Task's
// registration. A Task without a registration has the namespace generation.
//
//   - ResolveTaskDataFence returns the current fence of a namespace or Task
//     without changing anything, and whether the Task has a registration.
//   - RegisterTaskDataFence registers a Task at the current namespace
//     generation, so cleanup of other Tasks no longer moves its fence. It
//     changes nothing for a registered Task and reports whether it created
//     the registration. Orka registers a Task only after authorizing the
//     caller.
//   - ReleaseTaskDataFence removes a Task registration while it still has the
//     fence's generation, and otherwise changes nothing. Orka releases a
//     registration it created when the access it was created for failed.
//   - CheckTaskDataFence returns nil while the fence is current and no fence
//     Job is revoked.
//
// A stale fence is refused with ErrTaskDataCleanupChanged, and a revoked fence
// Job or an ended or superseded fence attempt with ErrTaskJobRevoked. The
// fence is checked before the operation's own outcome, so a refused operation
// changes nothing and reports the fence error even when it would have found
// nothing. These methods check a fence carried in their
// context in the same transaction as their reads and writes, so an operation
// either completes before the cleanup that would invalidate it, and the
// cleanup then removes its effect, or is refused: SaveResult, SaveArtifact,
// SavePlan, GetPlan, SendMessage, GetMessages, GetSession, GetSessionType,
// LoadTranscript, LoadTranscriptThrough, SearchTranscript, ReadSessionSoul,
// GetGatewayEventForTask, GetGatewayMessageBudget, EnqueueGatewayMessage,
// AppendExecutionEvent, AppendExecutionEventIfAbsent,
// AppendExecutionEventWithPlanIfAbsent, AppendExecutionEventsIfAbsent,
// ListExecutionEvents,
// ListHarnessV1AttemptsByTask and CheckTaskJobAuthority. Other methods ignore a
// fence.
//
// A provider checks a fence against the cleanup generations and revocations
// it holds. Orka resolves every fence from state.v1, so a fenced SaveArtifact
// needs the artifact provider to be the same store.
type TaskDataFenceStore interface {
	ResolveTaskDataFence(ctx context.Context, namespace, taskName string) (fence TaskDataFence, registered bool, err error)
	RegisterTaskDataFence(ctx context.Context, namespace, taskName string) (fence TaskDataFence, created bool, err error)
	ReleaseTaskDataFence(ctx context.Context, fence TaskDataFence) error
	CheckTaskDataFence(ctx context.Context, fence TaskDataFence) error
}

type taskDataFenceKey struct{}

// WithTaskDataFence returns a context that carries fence. A network transport
// carries the fence as request metadata.
func WithTaskDataFence(ctx context.Context, fence TaskDataFence) context.Context {
	return context.WithValue(ctx, taskDataFenceKey{}, cloneTaskDataFence(fence))
}

// TaskDataFenceFrom returns the fence carried by ctx, if any.
func TaskDataFenceFrom(ctx context.Context) (TaskDataFence, bool) {
	fence, ok := ctx.Value(taskDataFenceKey{}).(TaskDataFence)
	if !ok {
		return TaskDataFence{}, false
	}
	return cloneTaskDataFence(fence), true
}

func cloneTaskDataFence(fence TaskDataFence) TaskDataFence {
	fence.Jobs = slices.Clone(fence.Jobs)
	if fence.HarnessV1Attempt != nil {
		attempt := *fence.HarnessV1Attempt
		fence.HarnessV1Attempt = &attempt
	}
	return fence
}
