package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v3"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func withInternalTaskDataTransaction(c fiber.Ctx, backing any, mutate func(context.Context) error) error {
	transactions, ok := backing.(store.TaskDataTransactionStore)
	if !ok {
		return fiber.NewError(fiber.StatusServiceUnavailable, "task data transactions unavailable")
	}
	// Bound uncached Kubernetes checks while holding the SQLite writer so an
	// unavailable API server cannot block unrelated store work indefinitely.
	previousContext := c.Context()
	ctx, cancel := context.WithTimeout(previousContext, 10*time.Second)
	defer cancel()
	if err := transactions.WithTaskDataTransaction(ctx, func(txCtx context.Context) error {
		c.SetContext(txCtx)
		defer c.SetContext(previousContext)
		return mutate(txCtx)
	}); err != nil {
		if fiberErr, ok := errors.AsType[*fiber.Error](err); ok {
			return fiberErr
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to persist task data: %v", err))
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
