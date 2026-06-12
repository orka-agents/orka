package api

import (
	"fmt"
	"time"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/tasktrace"
)

// GetTaskTrace handles GET /api/v1/tasks/{id}/trace.
func (h *Handlers) GetTaskTrace(c fiber.Ctx) error {
	taskName := c.Params("id")
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenTaskRead(c, "getTaskTrace", namespace, taskName); err != nil {
		return err
	}
	if h.executionEventStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "execution event storage not enabled")
	}

	task := &corev1alpha1.Task{}
	if err := h.client.Get(c.Context(), types.NamespacedName{Name: taskName, Namespace: namespace}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "task not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
	}
	if err := h.authorizeContextTokenLoadedTask(c, "getTaskTrace", task); err != nil {
		return err
	}

	latestSeq, err := h.executionEventStore.GetLatestExecutionEventSeq(c.Context(), namespace, events.ExecutionEventStreamTypeTask, taskName)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get latest execution event sequence: %v", err))
	}
	listed, err := listTaskEventsThrough(c.Context(), h.executionEventStore, namespace, taskName, latestSeq)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list execution events: %v", err))
	}
	trace := tasktrace.BuildTaskTrace(tasktrace.MetadataFromTask(task), listed, time.Now().UTC())
	trace.LatestSeq = latestSeq
	return c.JSON(trace)
}
