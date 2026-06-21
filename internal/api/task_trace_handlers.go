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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/tasktrace"
)

const maxTaskTraceEvents = 5000

var errTaskTraceTooLarge = errors.New("task trace exceeds event limit")

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
	if throughSeq == 0 {
		return nil, nil
	}
	if maxEvents <= 0 {
		return nil, errTaskTraceTooLarge
	}
	var out []store.ExecutionEvent
	var after int64
	for {
		batch, err := eventStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
			Namespace:  namespace,
			StreamType: events.ExecutionEventStreamTypeTask,
			StreamID:   taskName,
			AfterSeq:   after,
			Limit:      store.MaxExecutionEventLimit,
		})
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		for _, event := range batch {
			if event.Seq > throughSeq {
				return out, nil
			}
			if len(out) >= maxEvents {
				return nil, errTaskTraceTooLarge
			}
			out = append(out, event)
			after = event.Seq
			if after >= throughSeq {
				return out, nil
			}
		}
		if len(batch) < store.MaxExecutionEventLimit {
			break
		}
	}
	return out, nil
}
