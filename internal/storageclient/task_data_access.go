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
) error {
	return c.withTaskDataAccess(ctx, namespace, taskName, true, authorize, access)
}

// WithTaskDataAccess implements store.TaskDataAccessStore for trusted
// controller code that needs no caller authorization. A Task it registers is
// registered only for the duration of the access, so cleanup of other Tasks
// does not refuse the access, yet no registration is left behind for a Task
// that may already be finalized.
func (c *Client) WithTaskDataAccess(ctx context.Context, namespace, taskName string, access func(context.Context) error) error {
	return c.withTaskDataAccess(ctx, namespace, taskName, false, func(context.Context) ([]store.TaskJobIdentity, error) {
		return nil, nil
	}, access)
}

// withTaskDataAccess runs access under a fence. A Task without a registration
// is registered first; keep decides whether a registration this call created
// outlives a successful access (authorized callers) or is always released
// (trusted controller access).
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
				// Failed or cancelled requests, and trusted access, must not
				// leave registrations behind; a later cleanup generation is
				// preserved. Releasing after a successful access is best
				// effort and does not turn that success into a failure.
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
