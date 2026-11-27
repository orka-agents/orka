package storageclient

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

const releaseTaskDataFenceTimeout = 5 * time.Second

type taskDataOutcomeKey struct{}

func (c *Client) WithAuthorizedTaskDataAccess(
	ctx context.Context,
	namespace, taskName string,
	authorize func(context.Context) ([]store.TaskJobIdentity, error),
	access func(context.Context) error,
) error {
	return c.withTaskDataAccess(ctx, namespace, taskName, true, authorize, access)
}

func (c *Client) WithTaskDataAccess(ctx context.Context, namespace, taskName string, access func(context.Context) error) error {
	return c.withTaskDataAccess(ctx, namespace, taskName, false, func(context.Context) ([]store.TaskJobIdentity, error) {
		return nil, nil
	}, access)
}

func (c *Client) withTaskDataAccess(
	ctx context.Context,
	namespace, taskName string,
	keep bool,
	authorize func(context.Context) ([]store.TaskJobIdentity, error),
	access func(context.Context) error,
) (accessErr error) {
	if authorize == nil || access == nil {
		return store.ValidationErrorf("task data access requires authorization and access functions")
	}
	if strings.TrimSpace(namespace) == "" {
		return store.ValidationErrorf("task data access requires a namespace")
	}
	unknownWrite, _ := ctx.Value(taskDataOutcomeKey{}).(*atomic.Bool)
	if unknownWrite == nil {
		unknownWrite = &atomic.Bool{}
		ctx = context.WithValue(ctx, taskDataOutcomeKey{}, unknownWrite)
	}
	fence, registered, err := c.ResolveTaskDataFence(ctx, namespace, taskName)
	if err != nil {
		return err
	}
	if taskName != "" && !registered {
		if _, err := authorize(ctx); err != nil {
			return err
		}
		var created bool
		fence, created, err = c.RegisterTaskDataFence(ctx, namespace, taskName)
		if err != nil {
			return err
		}
		if created {
			registration := fence
			defer func() {
				if accessErr == nil && keep {
					return
				}
				releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTaskDataFenceTimeout)
				defer cancel()
				if releaseErr := c.ReleaseTaskDataFence(releaseCtx, registration); accessErr != nil {
					accessErr = errors.Join(accessErr, releaseErr)
				}
			}()
		}
	}
	jobs, err := authorize(ctx)
	if err != nil {
		return err
	}
	fence.Jobs = jobs
	if err := c.CheckTaskDataFence(ctx, fence); err != nil {
		return err
	}
	err = access(store.WithTaskDataFence(ctx, fence))
	if unknownWrite.Load() {
		return store.ErrOutcomeUnknown
	}
	if err != nil {
		if errors.Is(err, store.ErrOutcomeUnknown) {
			return err
		}
		if fenceErr := c.CheckTaskDataFence(ctx, fence); errors.Is(fenceErr, store.ErrTaskDataCleanupChanged) ||
			errors.Is(fenceErr, store.ErrTaskJobRevoked) {
			return fenceErr
		}
		return err
	}
	return nil
}
