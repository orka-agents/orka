package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/store"
)

const (
	defaultEventStreamPollInterval   = 2 * time.Second
	defaultEventStreamHeartbeatEvery = 15 * time.Second
)

// ListTaskEvents handles GET /api/v1/tasks/{id}/events.
func (h *Handlers) ListTaskEvents(c fiber.Ctx) error {
	taskName := c.Params("id")
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenTaskRead(c, "listTaskEvents", namespace, taskName); err != nil {
		return err
	}
	if err := h.ensureTaskReadable(c, namespace, taskName, "listTaskEvents"); err != nil {
		return err
	}
	if h.executionEventStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "execution event storage not enabled")
	}

	query, err := parseExecutionEventListQuery(c)
	if err != nil {
		return err
	}
	filter := store.ExecutionEventFilter{
		Namespace:  namespace,
		StreamType: events.ExecutionEventStreamTypeTask,
		StreamID:   taskName,
		EventTypes: query.eventTypes,
		AfterSeq:   query.afterSeq,
		Limit:      query.limit,
	}
	listed, err := h.executionEventStore.ListExecutionEvents(c.Context(), filter)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list execution events: %v", err))
	}
	latestSeq, err := h.executionEventStore.GetLatestExecutionEventSeq(c.Context(), namespace, events.ExecutionEventStreamTypeTask, taskName)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get latest execution event sequence: %v", err))
	}

	return c.JSON(NewListExecutionEventsResponse(namespace, events.ExecutionEventStreamTypeTask, taskName, query.afterSeq, latestSeq, listed))
}

// StreamTaskEvents handles GET /api/v1/tasks/{id}/stream?after=N.
func (h *Handlers) StreamTaskEvents(c fiber.Ctx) error {
	taskName := c.Params("id")
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenTaskRead(c, "streamTaskEvents", namespace, taskName); err != nil {
		return err
	}
	if err := h.ensureTaskReadable(c, namespace, taskName, "streamTaskEvents"); err != nil {
		return err
	}
	if h.executionEventStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "execution event storage not enabled")
	}

	query, err := parseExecutionEventListQuery(c)
	if err != nil {
		return err
	}
	pollInterval := h.eventStreamPollInterval
	if pollInterval <= 0 {
		pollInterval = defaultEventStreamPollInterval
	}
	heartbeatEvery := h.eventStreamHeartbeatEvery
	if heartbeatEvery <= 0 {
		heartbeatEvery = defaultEventStreamHeartbeatEvery
	}
	ctx := c.Context()
	if ctx == nil {
		ctx = c.RequestCtx()
	}
	streamStore := h.executionEventStore

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	return c.SendStreamWriter(func(w *bufio.Writer) {
		lastSeq := query.afterSeq
		writeAvailable := func() bool {
			listed, err := streamStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
				Namespace:  namespace,
				StreamType: events.ExecutionEventStreamTypeTask,
				StreamID:   taskName,
				EventTypes: query.eventTypes,
				AfterSeq:   lastSeq,
				Limit:      store.MaxExecutionEventLimit,
			})
			if err != nil {
				log.Error(err, "failed to list execution events for stream", "namespace", namespace, "task", taskName)
				return true
			}
			for _, event := range listed {
				if event.Seq > lastSeq {
					lastSeq = event.Seq
				}
				if !writeExecutionEventSSEFrame(w, event) {
					return false
				}
			}
			if len(listed) > 0 {
				if err := w.Flush(); err != nil {
					return false
				}
			}
			return true
		}
		if !writeAvailable() {
			return
		}

		poll := time.NewTicker(pollInterval)
		defer poll.Stop()
		heartbeat := time.NewTicker(heartbeatEvery)
		defer heartbeat.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-poll.C:
				if !writeAvailable() {
					return
				}
			case <-heartbeat.C:
				if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
					return
				}
				if err := w.Flush(); err != nil {
					return
				}
			}
		}
	})
}

type executionEventListQuery struct {
	afterSeq   int64
	limit      int
	eventTypes []string
}

func parseExecutionEventListQuery(c fiber.Ctx) (executionEventListQuery, error) {
	var query executionEventListQuery
	if rawAfter := strings.TrimSpace(c.Query("after", "")); rawAfter != "" {
		after, err := strconv.ParseInt(rawAfter, 10, 64)
		if err != nil || after < 0 {
			return query, fiber.NewError(fiber.StatusBadRequest, "after must be a non-negative integer")
		}
		query.afterSeq = after
	}
	if rawLimit := strings.TrimSpace(c.Query("limit", "")); rawLimit != "" {
		limit, err := strconv.Atoi(rawLimit)
		if err != nil || limit <= 0 {
			return query, fiber.NewError(fiber.StatusBadRequest, "limit must be a positive integer")
		}
		query.limit = limit
	}

	values, err := url.ParseQuery(string(c.Request().URI().QueryString()))
	if err != nil {
		return query, fiber.NewError(fiber.StatusBadRequest, "invalid query string")
	}
	for _, typ := range values["type"] {
		typ = strings.TrimSpace(typ)
		if typ == "" {
			continue
		}
		if !events.IsValidExecutionEventType(typ) {
			return query, fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("unsupported execution event type %q", typ))
		}
		query.eventTypes = append(query.eventTypes, typ)
	}
	return query, nil
}

func (h *Handlers) ensureTaskReadable(c fiber.Ctx, namespace, taskName, action string) error {
	task := &corev1alpha1.Task{}
	if err := h.client.Get(c.Context(), types.NamespacedName{Name: taskName, Namespace: namespace}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "task not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
	}
	return h.authorizeContextTokenLoadedTask(c, action, task)
}

func writeExecutionEventSSEFrame(w *bufio.Writer, event store.ExecutionEvent) bool {
	data, err := json.Marshal(NewExecutionEventResponse(event))
	if err != nil {
		log.Error(err, "failed to marshal execution event for SSE", "eventID", event.ID)
		return true
	}
	if _, err := fmt.Fprintf(w, "id: %d\nevent: execution_event\ndata: %s\n\n", event.Seq, data); err != nil {
		return false
	}
	return true
}
