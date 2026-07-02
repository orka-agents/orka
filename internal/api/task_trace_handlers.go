/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tasktrace"
)

const maxTaskTraceEvents = 5000

var errTaskTraceTooLarge = errTaskTimelineReadLimitExceeded

// GetTaskTrace handles GET /api/v1/tasks/{id}/trace.
func (h *Handlers) GetTaskTrace(c fiber.Ctx) error {
	taskName := c.Params("id")
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	access := h.taskAccess()
	if err := access.authorizeReadable(c, "getTaskTrace", namespace, taskName); err != nil {
		return err
	}
	if h.executionEventStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "execution event storage not enabled")
	}
	task, err := access.loadAuthorized(c, "getTaskTrace", namespace, taskName)
	if err != nil {
		return err
	}

	latestSeq, err := h.executionEventStore.GetLatestExecutionEventSeq(c.Context(), namespace, events.ExecutionEventStreamTypeTask, taskName)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get latest execution event sequence: %v", err))
	}
	listed, err := listAllTaskEventsThrough(c.Context(), h.executionEventStore, namespace, taskName, latestSeq, maxTaskTraceEvents)
	if err != nil {
		if errors.Is(err, errTaskTraceTooLarge) {
			return fiber.NewError(fiber.StatusRequestEntityTooLarge, fmt.Sprintf("task trace exceeds %d events", maxTaskTraceEvents))
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list execution events: %v", err))
	}
	trace := tasktrace.BuildTaskTrace(tasktrace.MetadataFromTask(task), listed, time.Now().UTC())
	trace.LatestSeq = latestSeq
	return c.JSON(trace)
}

func listAllTaskEventsThrough(
	ctx context.Context,
	eventStore store.ExecutionEventStore,
	namespace,
	taskName string,
	throughSeq int64,
	maxEvents int,
) ([]store.ExecutionEvent, error) {
	return newTaskTimelineReader(eventStore, namespace, taskName).listThrough(ctx, throughSeq, maxEvents)
}
