package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

type brokeredTaskDataAccess struct {
	authorizer internalCallerAuthorizer
	taskKey    client.ObjectKey
	taskUID    string
	guard      func(context.Context, func(context.Context) error) error
}

func (a brokeredTaskDataAccess) withData(ctx context.Context, backing any, authorize func(context.Context, *corev1alpha1.Task) error, access func(context.Context) error) error {
	if a.authorizer.k8sReader == nil || a.taskKey.Namespace == "" || a.taskKey.Name == "" || a.taskUID == "" {
		return fiber.NewError(fiber.StatusForbidden, "authenticated task identity required")
	}
	if a.guard == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "authenticated prompt data guard unavailable")
	}
	transactions, ok := backing.(store.TaskDataTransactionStore)
	if !ok {
		return fiber.NewError(fiber.StatusServiceUnavailable, "task data transactions unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return a.guard(ctx, func(guardCtx context.Context) error {
		var err error
		for range 3 {
			// Brokered coordination can access data from several Tasks. Fence
			// namespace cleanup, with every Kubernetes read outside the transaction.
			err = transactions.WithAuthorizedTaskDataTransaction(guardCtx, a.taskKey.Namespace, "", func(authCtx context.Context) error {
				task, err := a.activeTask(authCtx)
				if err != nil {
					return err
				}
				return authorize(authCtx, task)
			}, access)
			if !errors.Is(err, store.ErrTaskDataCleanupChanged) {
				break
			}
		}
		return err
	})
}

func (a brokeredTaskDataAccess) activeTask(ctx context.Context) (*corev1alpha1.Task, error) {
	task := &corev1alpha1.Task{}
	if err := a.authorizer.k8sReader.Get(ctx, a.taskKey, task); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusForbidden, "caller task is unavailable")
		}
		return nil, fmt.Errorf("load caller task: %w", err)
	}
	if string(task.UID) != a.taskUID || !activeInternalWorkerTask(task) {
		return nil, fiber.NewError(fiber.StatusForbidden, "caller task identity is no longer active")
	}
	return task, nil
}
