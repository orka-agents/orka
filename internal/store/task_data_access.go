package store

import (
	"context"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

var ErrTaskDataCleanupChanged = storagev1.ErrTaskDataCleanupChanged

type (
	TaskDataFence      = storagev1.TaskDataFence
	TaskDataFenceStore = storagev1.TaskDataFenceStore
)

func WithTaskDataFence(ctx context.Context, fence TaskDataFence) context.Context {
	return storagev1.WithTaskDataFence(ctx, fence)
}

func TaskDataFenceFrom(ctx context.Context) (TaskDataFence, bool) {
	return storagev1.TaskDataFenceFrom(ctx)
}

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
