package v1

import (
	"context"
	"errors"
	"slices"
)

var ErrTaskJobRevoked = errors.New("task Job authority revoked")

var ErrTaskDataCleanupChanged = errors.New("task data cleanup changed during authorization")

type TaskJobIdentity struct {
	Namespace string
	TaskUID   string
	JobUID    string
}

type TaskJobAuthorityStore interface {
	RevokeTaskJob(context.Context, TaskJobIdentity) error
	CheckTaskJobAuthority(context.Context, TaskJobIdentity) error
	DeleteTaskJobRevocations(context.Context, string, string, string) error
}

type TaskDataFence struct {
	Namespace  string
	TaskName   string
	Generation int64
	Jobs       []TaskJobIdentity
}

type TaskDataFenceStore interface {
	ResolveTaskDataFence(ctx context.Context, namespace, taskName string) (fence TaskDataFence, registered bool, err error)
	RegisterTaskDataFence(ctx context.Context, namespace, taskName string) (fence TaskDataFence, created bool, err error)
	ReleaseTaskDataFence(ctx context.Context, fence TaskDataFence) error
	CheckTaskDataFence(ctx context.Context, fence TaskDataFence) error
}

type taskDataFenceKey struct{}

func WithTaskDataFence(ctx context.Context, fence TaskDataFence) context.Context {
	return context.WithValue(ctx, taskDataFenceKey{}, cloneTaskDataFence(fence))
}

func TaskDataFenceFrom(ctx context.Context) (TaskDataFence, bool) {
	fence, ok := ctx.Value(taskDataFenceKey{}).(TaskDataFence)
	if !ok {
		return TaskDataFence{}, false
	}
	return cloneTaskDataFence(fence), true
}

func cloneTaskDataFence(fence TaskDataFence) TaskDataFence {
	fence.Jobs = slices.Clone(fence.Jobs)
	return fence
}
