package api

import (
	"context"
	"errors"
	"time"

	"github.com/gofiber/fiber/v3"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

type internalTaskJobAuthoritiesKey struct{}

func recordInternalTaskJobAuthority(ctx context.Context, task *corev1alpha1.Task) {
	if identities, ok := ctx.Value(internalTaskJobAuthoritiesKey{}).(map[store.TaskJobIdentity]struct{}); ok {
		identities[store.TaskJobIdentity{Namespace: task.Namespace, TaskUID: string(task.UID), JobUID: task.Status.JobUID}] = struct{}{}
	}
}

func withInternalTaskDataTransaction(c fiber.Ctx, backing any, taskName string, authorize, mutate func(context.Context) error) error {
	transactions, ok := backing.(store.TaskDataTransactionStore)
	if !ok {
		return fiber.NewError(fiber.StatusServiceUnavailable, "task data transactions unavailable")
	}
	// Bound live authorization and cleanup retries without holding the SQLite
	// writer during Kubernetes reads.
	previousContext := c.Context()
	ctx, cancel := context.WithTimeout(previousContext, 10*time.Second)
	defer cancel()
	defer c.SetContext(previousContext)
	var err error
	for range 3 {
		identities := make(map[store.TaskJobIdentity]struct{})
		attemptCtx := context.WithValue(ctx, internalTaskJobAuthoritiesKey{}, identities)
		err = transactions.WithAuthorizedTaskDataTransaction(attemptCtx, c.Params("namespace"), taskName, func(authCtx context.Context) error {
			c.SetContext(authCtx)
			return authorize(authCtx)
		}, func(txCtx context.Context) error {
			c.SetContext(txCtx)
			for identity := range identities {
				if err := transactions.CheckTaskJobAuthority(txCtx, identity); err != nil {
					return err
				}
			}
			return mutate(txCtx)
		})
		if !errors.Is(err, store.ErrTaskDataCleanupChanged) {
			break
		}
	}
	if err != nil {
		if fiberErr, ok := errors.AsType[*fiber.Error](err); ok {
			return fiberErr
		}
		if errors.Is(err, store.ErrTaskDataCleanupChanged) {
			return fiber.NewError(fiber.StatusServiceUnavailable, "task data cleanup is in progress; retry the request")
		}
		if errors.Is(err, store.ErrTaskJobRevoked) {
			return fiber.NewError(fiber.StatusForbidden, "task Job is no longer active")
		}
		logf.FromContext(ctx).Error(err, "internal task data access failed", "namespace", c.Params("namespace"))
		return fiber.NewError(fiber.StatusInternalServerError, "task data access failed")
	}
	return nil
}

func (a internalCallerAuthorizer) revalidateTaskCaller(c fiber.Ctx, authorized *corev1alpha1.Task) error {
	current, err := a.verifyTaskCaller(c, authorized.Namespace, authorized.Name)
	if err != nil {
		return err
	}
	if current.UID != authorized.UID {
		return fiber.NewError(fiber.StatusForbidden, "task identity changed")
	}
	return nil
}
