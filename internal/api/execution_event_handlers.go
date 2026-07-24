package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/store"
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
	if err := h.taskAccess().ensureReadable(c, "listTaskEvents", namespace, taskName); err != nil {
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
//
//nolint:gocyclo // Replay, reconnect, heartbeat, terminal, and post-terminal completion are one stream state machine.
func (h *Handlers) StreamTaskEvents(c fiber.Ctx) error {
	taskName := c.Params("id")
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.taskAccess().ensureReadable(c, "streamTaskEvents", namespace, taskName); err != nil {
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
	streamStore := h.executionEventStore
	// SendStreamWriter outlives the Fiber handler, so clone strings derived
	// from fiber.Ctx before Fiber can recycle request buffers.
	namespace = strings.Clone(namespace)
	taskName = strings.Clone(taskName)

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	return c.SendStreamWriter(func(w *bufio.Writer) {
		// Fiber recycles request contexts after the handler returns. Use a
		// stream-owned context and rely on write/heartbeat failures to detect
		// client disconnects.
		ctx, cancelStream := context.WithCancel(context.Background())
		defer cancelStream()
		done := metrics.RecordExecutionEventStreamOpen("task", query.afterSeq > 0)
		defer done()
		lastSeq := query.afterSeq
		terminalScanSeq := query.afterSeq
		var pendingTerminal *store.ExecutionEvent
		if query.afterSeq > 0 {
			terminalEvent, found, err := terminalExecutionEventThroughCursor(ctx, streamStore, namespace, taskName, query.afterSeq)
			if err != nil {
				metrics.RecordExecutionEventStreamError("task", "list")
				log.Error(err, "failed to find prior terminal execution event for stream", "namespace", namespace, "task", taskName)
				return
			}
			if found {
				pendingTerminal = &terminalEvent
				terminalScanSeq = max(terminalScanSeq, terminalEvent.Seq)
			}
		}
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
				metrics.RecordExecutionEventStreamError("task", "list")
				log.Error(err, "failed to list execution events for stream", "namespace", namespace, "task", taskName)
				return true
			}
			terminal := false
			wroteComplete := false
			for _, event := range listed {
				if event.Seq > lastSeq {
					lastSeq = event.Seq
				}
				if !writeExecutionEventSSEFrame(w, event) {
					metrics.RecordExecutionEventStreamError("task", "write")
					return false
				}
				if pendingTerminal == nil && events.IsTerminalTaskEventType(event.Type) {
					terminalCopy := event
					pendingTerminal = &terminalCopy
				}
			}
			if pendingTerminal == nil && len(listed) < store.MaxExecutionEventLimit {
				terminalEvent, found, scannedThrough, err := terminalExecutionEventForCompletion(ctx, streamStore, namespace, taskName, terminalScanSeq)
				if err != nil {
					metrics.RecordExecutionEventStreamError("task", "list")
					log.Error(err, "failed to find terminal execution event for stream", "namespace", namespace, "task", taskName)
					return true
				}
				terminalScanSeq = max(terminalScanSeq, scannedThrough)
				if found {
					shouldWriteDiscoveredTerminal := len(query.eventTypes) == 0 && terminalEvent.Seq > lastSeq
					pendingTerminal = &terminalEvent
					if shouldWriteDiscoveredTerminal {
						if !writeExecutionEventSSEFrame(w, terminalEvent) {
							metrics.RecordExecutionEventStreamError("task", "write")
							return false
						}
					}
					lastSeq = max(lastSeq, terminalEvent.Seq)
				}
			}
			if pendingTerminal == nil && len(listed) == 0 {
				deleted, err := h.taskDeletedForEventStream(ctx, namespace, taskName)
				if err != nil {
					metrics.RecordExecutionEventStreamError("task", "list")
					log.Error(err, "failed to get task for event stream", "namespace", namespace, "task", taskName)
					return true
				}
				if deleted {
					pendingTerminal = &store.ExecutionEvent{
						Namespace:  namespace,
						StreamType: events.ExecutionEventStreamTypeTask,
						StreamID:   taskName,
						Seq:        lastSeq,
						Type:       "TaskDeleted",
						Severity:   events.ExecutionEventSeverityInfo,
						TaskName:   taskName,
						Summary:    "task deleted",
					}
				}
			}
			if pendingTerminal != nil && len(listed) < store.MaxExecutionEventLimit {
				terminal = true
				if !writeExecutionEventStreamCompleteSSEFrame(w, *pendingTerminal, lastSeq) {
					metrics.RecordExecutionEventStreamError("task", "write")
					return false
				}
				wroteComplete = true
			}
			if len(listed) > 0 || wroteComplete {
				if err := w.Flush(); err != nil {
					metrics.RecordExecutionEventStreamError("task", "write")
					return false
				}
			}
			return !terminal
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
	streamStore := h.executionEventStore
	// SendStreamWriter outlives the Fiber handler, so clone strings derived
	// from fiber.Ctx before Fiber can recycle request buffers.
	namespace = strings.Clone(namespace)
	sessionName = strings.Clone(sessionName)

	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	return c.SendStreamWriter(func(w *bufio.Writer) {
		// Fiber recycles request contexts after the handler returns. Use a
		// stream-owned context and rely on write/heartbeat failures to detect
		// client disconnects.
		ctx, cancelStream := context.WithCancel(context.Background())
		defer cancelStream()
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
				if !h.writeSessionDeletedStreamComplete(ctx, w, namespace, sessionName, lastSeq) {
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

func (h *Handlers) writeSessionDeletedStreamComplete(
	ctx context.Context,
	w *bufio.Writer,
	namespace string,
	sessionName string,
	lastSeq int64,
) bool {
	if h == nil || h.sessionStore == nil {
		return true
	}
	if _, err := h.sessionStore.GetSession(ctx, namespace, sessionName); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			if !writeExecutionEventStreamCompleteFrame(w, lastSeq, "SessionDeleted") {
				metrics.RecordExecutionEventStreamError("session", "write")
				return false
			}
			if err := w.Flush(); err != nil {
				metrics.RecordExecutionEventStreamError("session", "write")
				return false
			}
			return false
		}
		log.Error(err, "failed to check session existence for event stream", "namespace", namespace, "session", sessionName)
	}
	return true
}

func sessionNameForTask(task *corev1alpha1.Task) string {
	if task == nil || task.Spec.SessionRef == nil {
		return ""
	}
	return strings.TrimSpace(task.Spec.SessionRef.Name)
}

func (h *Handlers) existingSessionNameForTask(ctx context.Context, namespace string, task *corev1alpha1.Task) (string, error) {
	sessionName := sessionNameForTask(task)
	if sessionName == "" {
		return "", nil
	}
	if h == nil || h.sessionStore == nil {
		return sessionName, nil
	}
	if _, err := h.sessionStore.GetSession(ctx, namespace, sessionName); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	return sessionName, nil
}

func (h *Handlers) ensureSessionReadable(c fiber.Ctx, namespace, sessionName string) error {
	if h.sessionStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "session store not configured")
	}
	sessionType, err := transcriptSessionType(c.Context(), h.sessionStore, namespace, sessionName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "session not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get session type: %v", err))
	}
	if sessionType == store.SessionTypeGateway {
		return fiber.NewError(fiber.StatusNotFound, "session not found")
	}
	return nil
}

func (h *Handlers) taskDeletedForEventStream(ctx context.Context, namespace, taskName string) (bool, error) {
	if h.client == nil {
		return false, nil
	}
	task := &corev1alpha1.Task{}
	if err := h.client.Get(ctx, types.NamespacedName{Name: taskName, Namespace: namespace}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	// A terminating-but-readable Task can still emit cancellation, failure, or
	// cleanup events. Keep the stream open until the object is actually gone.
	return false, nil
}

func terminalExecutionEventThroughCursor(
	ctx context.Context,
	eventStore store.ExecutionEventStore,
	namespace string,
	taskName string,
	cursor int64,
) (store.ExecutionEvent, bool, error) {
	return newTaskTimelineReader(eventStore, namespace, taskName).terminalThroughCursor(ctx, cursor)
}

func terminalExecutionEventForCompletion(
	ctx context.Context,
	eventStore store.ExecutionEventStore,
	namespace string,
	taskName string,
	cursor int64,
) (store.ExecutionEvent, bool, int64, error) {
	return newTaskTimelineReader(eventStore, namespace, taskName).terminalForCompletion(ctx, cursor)
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

func writeExecutionEventStreamCompleteSSEFrame(w *bufio.Writer, event store.ExecutionEvent, lastSeq int64) bool {
	return writeExecutionEventStreamCompleteFrame(w, max(lastSeq, event.Seq), event.Type)
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
