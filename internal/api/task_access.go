/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"fmt"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// taskAccess owns the safe Task read path for public API handlers: name-scoped
// context-token authorization, Kubernetes object loading, not-found mapping, and
// loaded-object authorization for effective Task context.
type taskAccess struct {
	h *Handlers
}

func (h *Handlers) taskAccess() taskAccess {
	return taskAccess{h: h}
}

func (a taskAccess) load(c fiber.Ctx, namespace, taskName string) (*corev1alpha1.Task, error) {
	task := &corev1alpha1.Task{}
	if err := a.h.client.Get(c.Context(), types.NamespacedName{Name: taskName, Namespace: namespace}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusNotFound, "task not found")
		}
		return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
	}
	return task, nil
}

func (a taskAccess) authorizeReadable(c fiber.Ctx, action, namespace, taskName string) error {
	return a.h.authorizeContextTokenTaskRead(c, action, namespace, taskName)
}

func (a taskAccess) loadReadable(c fiber.Ctx, action, namespace, taskName string) (*corev1alpha1.Task, error) {
	if err := a.authorizeReadable(c, action, namespace, taskName); err != nil {
		return nil, err
	}
	return a.loadAuthorized(c, action, namespace, taskName)
}

func (a taskAccess) loadAuthorized(c fiber.Ctx, action, namespace, taskName string) (*corev1alpha1.Task, error) {
	task, err := a.load(c, namespace, taskName)
	if err != nil {
		return nil, err
	}
	if err := a.h.authorizeContextTokenLoadedTask(c, action, task); err != nil {
		return nil, err
	}
	return task, nil
}

func (a taskAccess) ensureReadable(c fiber.Ctx, action, namespace, taskName string) error {
	_, err := a.loadReadable(c, action, namespace, taskName)
	return err
}

// loadReadableForContextToken preserves handlers whose non-context-token callers
// historically did not need the parent Task object to exist before continuing.
// The caller should already have run authorizeReadable for the URL task name.
func (a taskAccess) loadReadableForContextToken(c fiber.Ctx, action, namespace, taskName string) (*corev1alpha1.Task, error) {
	if !a.h.contextTokenAuthorization.Enabled() {
		return nil, nil
	}
	ui := GetUserInfo(c)
	if ui == nil || ui.AuthType != AuthTypeContextToken || ui.ContextToken == nil {
		return nil, nil
	}
	task, err := a.load(c, namespace, taskName)
	if err != nil {
		return nil, err
	}
	if err := a.h.authorizeContextTokenLoadedTask(c, action, task); err != nil {
		return nil, err
	}
	return task, nil
}
