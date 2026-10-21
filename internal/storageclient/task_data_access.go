package storageclient

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

// releaseTaskDataFenceTimeout bounds removing a registration after the access
// it was created for failed or was cancelled.
const releaseTaskDataFenceTimeout = 5 * time.Second

// WithAuthorizedTaskDataAccess implements store.TaskDataAccessStore. The
// provider resolves the cleanup fence and Orka authorizes the caller against
// live Kubernetes state with no provider transaction open. Each fence-checking
// method that access calls then refuses to act once cleanup or a Job
// revocation has invalidated the authorization.
//
// A Task without a registration is registered only after a first successful
// authorization, so an unauthenticated request cannot create one; that proof
// is discarded and the caller is authorized again against the registered
// fence. A registration this call created is released when access fails.
func (c *Client) WithAuthorizedTaskDataAccess(
	ctx context.Context,
	namespace, taskName string,
	authorize func(context.Context) ([]store.TaskJobIdentity, error),
	access func(context.Context) error,
) (accessErr error) {
	if authorize == nil || access == nil {
		return store.ValidationErrorf("task data access requires authorization and access functions")
	}
	if strings.TrimSpace(namespace) == "" {
		return store.ValidationErrorf("task data access requires a namespace")
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
				if accessErr == nil {
					return
				}
				// Failed or cancelled requests must not leave registrations
				// behind; a later cleanup generation is preserved.
				releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTaskDataFenceTimeout)
				defer cancel()
				accessErr = errors.Join(accessErr, c.ReleaseTaskDataFence(releaseCtx, registration))
			}()
		}
	}
	jobs, err := authorize(ctx)
	if err != nil {
		return err
	}
	fence.Jobs = jobs
	// Refuse before access runs, as well as in each fenced call, so access
	// that ends up calling no fence-checking method is still fenced.
	if err := c.CheckTaskDataFence(ctx, fence); err != nil {
		return err
	}
	if err := access(store.WithTaskDataFence(ctx, fence)); err != nil {
		// Callers may turn a refused fenced call into their own error. When
		// the fence has moved, report that instead, so the caller authorizes
		// again rather than surfacing the refusal as a failure.
		if fenceErr := c.CheckTaskDataFence(ctx, fence); errors.Is(fenceErr, store.ErrTaskDataCleanupChanged) ||
			errors.Is(fenceErr, store.ErrTaskJobRevoked) {
			return fenceErr
		}
		return err
	}
	return nil
}

// WithTaskDataAccess implements store.TaskDataAccessStore for trusted
// controller code that needs no caller authorization.
func (c *Client) WithTaskDataAccess(ctx context.Context, namespace, taskName string, access func(context.Context) error) error {
	return c.WithAuthorizedTaskDataAccess(ctx, namespace, taskName, func(context.Context) ([]store.TaskJobIdentity, error) {
		return nil, nil
	}, access)
}
