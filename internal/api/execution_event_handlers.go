package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/metrics"
	"github.com/sozercan/orka/internal/store"
)

const (
	defaultEventStreamPollInterval   = 2 * time.Second
	defaultEventStreamHeartbeatEvery = 15 * time.Second
)

// ListTaskEvents handles GET /api/v1/tasks/{id}/events.
func (h *Handlers) ListTaskEvents(c fiber.Ctx) error {
	started := time.Now()
	success := false
	defer func() {
		metrics.RecordExecutionEventList("task_api", success, time.Since(started).Seconds())
	}()
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

	success = true
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
		done := metrics.RecordExecutionEventStreamOpen("task", query.afterSeq > 0)
		defer done()
		lastSeq := query.afterSeq
		writeAvailable := func() bool {
			return writeAvailableExecutionEventSSEFrames(
				ctx,
				w,
				streamStore,
				namespace,
				taskName,
				query,
				&lastSeq,
			)
		}
		if !writeAvailable() {
			return
		}
		if !h.writeTaskDeletedStreamComplete(ctx, w, namespace, taskName, lastSeq) {
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
				if !h.writeTaskDeletedStreamComplete(ctx, w, namespace, taskName, lastSeq) {
					return
				}
			case <-heartbeat.C:
				if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
					metrics.RecordExecutionEventStreamError("task", "write")
					return
				}
				if err := w.Flush(); err != nil {
					metrics.RecordExecutionEventStreamError("task", "write")
					return
				}
			}
		}
	})
}

// ListSessionEvents handles GET /api/v1/sessions/{id}/events.
func (h *Handlers) ListSessionEvents(c fiber.Ctx) error {
	started := time.Now()
	success := false
	defer func() {
		metrics.RecordExecutionEventList("session_api", success, time.Since(started).Seconds())
	}()
	sessionName := c.Params("id")
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listSessionEvents", h.contextTokenAuthorization.SessionReadScopes); err != nil {
		return err
	}
	if err := h.ensureSessionReadable(c, namespace, sessionName); err != nil {
		return err
	}
	if h.executionEventStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "execution event storage not enabled")
	}

	query, err := parseExecutionEventListQuery(c)
	if err != nil {
		return err
	}
	listed, latestSeq, err := h.executionEventStore.ListSessionExecutionEvents(c.Context(), store.SessionExecutionEventFilter{
		Namespace:   namespace,
		SessionName: sessionName,
		EventTypes:  query.eventTypes,
		AfterSeq:    query.afterSeq,
		Limit:       query.limit,
	})
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list session execution events: %v", err))
	}

	success = true
	return c.JSON(NewListSessionExecutionEventsResponse(namespace, sessionName, query.afterSeq, latestSeq, listed))
}

// StreamSessionEvents handles GET /api/v1/sessions/{id}/stream?after=N.
func (h *Handlers) StreamSessionEvents(c fiber.Ctx) error {
	sessionName := c.Params("id")
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "streamSessionEvents", h.contextTokenAuthorization.SessionReadScopes); err != nil {
		return err
	}
	if err := h.ensureSessionReadable(c, namespace, sessionName); err != nil {
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
		done := metrics.RecordExecutionEventStreamOpen("session", query.afterSeq > 0)
		defer done()
		lastSeq := query.afterSeq
		writeAvailable := func() bool {
			listed, _, err := streamStore.ListSessionExecutionEvents(ctx, store.SessionExecutionEventFilter{
				Namespace:   namespace,
				SessionName: sessionName,
				EventTypes:  query.eventTypes,
				AfterSeq:    lastSeq,
				Limit:       store.MaxExecutionEventLimit,
			})
			if err != nil {
				metrics.RecordExecutionEventStreamError("session", "list")
				log.Error(err, "failed to list execution events for session stream", "namespace", namespace, "session", sessionName)
				return true
			}
			for _, event := range listed {
				if event.SessionSeq > lastSeq {
					lastSeq = event.SessionSeq
				}
				if !writeSessionExecutionEventSSEFrame(w, event) {
					metrics.RecordExecutionEventStreamError("session", "write")
					return false
				}
			}
			if len(listed) > 0 {
				if err := w.Flush(); err != nil {
					metrics.RecordExecutionEventStreamError("session", "write")
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
					metrics.RecordExecutionEventStreamError("session", "write")
					return
				}
				if err := w.Flush(); err != nil {
					metrics.RecordExecutionEventStreamError("session", "write")
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

func (h *Handlers) writeTaskDeletedStreamComplete(
	ctx context.Context,
	w *bufio.Writer,
	namespace string,
	taskName string,
	lastSeq int64,
) bool {
	if h == nil || h.client == nil {
		return true
	}
	task := &corev1alpha1.Task{}
	err := h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: taskName}, task)
	if err == nil {
		return true
	}
	if !apierrors.IsNotFound(err) {
		log.Error(err, "failed to check task existence for event stream", "namespace", namespace, "task", taskName)
		return true
	}
	if !writeExecutionEventStreamCompleteFrame(w, lastSeq, "TaskDeleted") {
		return false
	}
	if err := w.Flush(); err != nil {
		return false
	}
	return false
}

func writeAvailableExecutionEventSSEFrames(
	ctx context.Context,
	w *bufio.Writer,
	eventStore store.ExecutionEventStore,
	namespace string,
	taskName string,
	query executionEventListQuery,
	lastSeq *int64,
) bool {
	listed, err := eventStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
		Namespace:  namespace,
		StreamType: events.ExecutionEventStreamTypeTask,
		StreamID:   taskName,
		EventTypes: query.eventTypes,
		AfterSeq:   *lastSeq,
		Limit:      store.MaxExecutionEventLimit,
	})
	if err != nil {
		metrics.RecordExecutionEventStreamError("task", "list")
		log.Error(err, "failed to list execution events for stream", "namespace", namespace, "task", taskName)
		return true
	}

	var terminalEvent *store.ExecutionEvent
	for _, event := range listed {
		if event.Seq > *lastSeq {
			*lastSeq = event.Seq
		}
		if !writeExecutionEventSSEFrame(w, event) {
			metrics.RecordExecutionEventStreamError("task", "write")
			return false
		}
		if isTerminalExecutionEventType(event.Type) {
			eventCopy := event
			terminalEvent = &eventCopy
			break
		}
	}

	if terminalEvent == nil && len(listed) < store.MaxExecutionEventLimit {
		terminalCandidate := latestTerminalExecutionEvent(ctx, eventStore, namespace, taskName)
		if terminalCandidate != nil && shouldCompleteForTerminalCandidate(
			terminalCandidate,
			*lastSeq,
			query.eventTypes,
		) {
			terminalEvent = terminalCandidate
		}
	}
	if terminalEvent != nil {
		if !writeExecutionEventStreamCompleteSSEFrame(w, *terminalEvent) {
			metrics.RecordExecutionEventStreamError("task", "write")
			return false
		}
	}
	if len(listed) > 0 || terminalEvent != nil {
		if err := w.Flush(); err != nil {
			metrics.RecordExecutionEventStreamError("task", "write")
			return false
		}
	}
	return terminalEvent == nil
}

func shouldCompleteForTerminalCandidate(
	terminalCandidate *store.ExecutionEvent,
	lastSeq int64,
	eventTypes []string,
) bool {
	return terminalCandidate.Seq <= lastSeq || executionEventTypeFilterExcludes(eventTypes, terminalCandidate.Type)
}

func (h *Handlers) ensureSessionReadable(c fiber.Ctx, namespace, sessionName string) error {
	if h.sessionStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "session store not configured")
	}
	if _, err := h.sessionStore.GetSession(c.Context(), namespace, sessionName); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "session not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get session: %v", err))
	}
	return nil
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

func latestTerminalExecutionEvent(
	ctx context.Context,
	eventStore store.ExecutionEventStore,
	namespace string,
	taskName string,
) *store.ExecutionEvent {
	if eventStore == nil {
		return nil
	}
	listed, err := eventStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
		Namespace:  namespace,
		StreamType: events.ExecutionEventStreamTypeTask,
		StreamID:   taskName,
		EventTypes: terminalExecutionEventTypes(),
		AfterSeq:   0,
		Limit:      store.MaxExecutionEventLimit,
	})
	if err != nil {
		log.Error(err, "failed to list terminal execution events for stream", "namespace", namespace, "task", taskName)
		return nil
	}
	if len(listed) == 0 {
		return nil
	}
	terminal := listed[len(listed)-1]
	return &terminal
}

func terminalExecutionEventTypes() []string {
	return []string{
		events.ExecutionEventTypeTaskSucceeded,
		events.ExecutionEventTypeTaskFailed,
		events.ExecutionEventTypeTaskCancelled,
	}
}

func executionEventTypeFilterExcludes(eventTypes []string, eventType string) bool {
	if len(eventTypes) == 0 {
		return false
	}
	return !slices.Contains(eventTypes, eventType)
}

func writeSessionExecutionEventSSEFrame(w *bufio.Writer, event store.SessionExecutionEvent) bool {
	data, err := json.Marshal(NewSessionExecutionEventResponse(event))
	if err != nil {
		log.Error(err, "failed to marshal session execution event for SSE", "eventID", event.ID, "sessionSeq", event.SessionSeq)
		return true
	}
	if _, err := fmt.Fprintf(w, "id: %d\nevent: execution_event\ndata: %s\n\n", event.SessionSeq, data); err != nil {
		return false
	}
	return true
}

func writeExecutionEventStreamCompleteSSEFrame(w *bufio.Writer, event store.ExecutionEvent) bool {
	return writeExecutionEventStreamCompleteFrame(w, event.Seq, event.Type)
}

func writeExecutionEventStreamCompleteFrame(w *bufio.Writer, lastSeq int64, eventType string) bool {
	data, err := json.Marshal(struct {
		LastSeq int64  `json:"lastSeq"`
		Type    string `json:"type"`
	}{
		LastSeq: lastSeq,
		Type:    eventType,
	})
	if err != nil {
		log.Error(err, "failed to marshal execution event stream completion", "eventType", eventType)
		return true
	}
	if _, err := fmt.Fprintf(w, "id: %d\nevent: stream_complete\ndata: %s\n\n", lastSeq, data); err != nil {
		return false
	}
	return true
}

func isTerminalExecutionEventType(eventType string) bool {
	switch eventType {
	case events.ExecutionEventTypeTaskSucceeded,
		events.ExecutionEventTypeTaskFailed,
		events.ExecutionEventTypeTaskCancelled:
		return true
	default:
		return false
	}
}
