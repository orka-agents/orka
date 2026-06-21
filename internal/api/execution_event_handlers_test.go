package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/store"
)

func TestListTaskEvents(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStoreWithClock(func() time.Time {
		return time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	})
	appendTestTaskEvent(t, eventStore, "task-1", events.ExecutionEventTypeTaskStarted)
	appendTestTaskEvent(t, eventStore, "task-1", events.ExecutionEventTypeToolCallCompleted)
	appendTestTaskEvent(t, eventStore, "task-1", events.ExecutionEventTypeTaskSucceeded)
	appendTestTaskEvent(t, eventStore, "task-2", events.ExecutionEventTypeTaskFailed)

	h, app := setupTaskEventHandlers(t, eventStore, testTask("default", "task-1"), testTask("default", "task-2"))
	app.Get("/api/v1/tasks/:id/events", h.ListTaskEvents)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/task-1/events?namespace=default&after=1&type=ToolCallCompleted&type=TaskSucceeded&limit=1", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var listed ListExecutionEventsResponse
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if listed.Namespace != "default" || listed.StreamType != store.ExecutionEventStreamTypeTask || listed.StreamID != "task-1" {
		t.Fatalf("stream metadata = %#v", listed)
	}
	if listed.AfterSeq != 1 || listed.LatestSeq != 3 {
		t.Fatalf("seq metadata after=%d latest=%d, want 1 and 3", listed.AfterSeq, listed.LatestSeq)
	}
	if len(listed.Events) != 1 || listed.Events[0].Seq != 2 || listed.Events[0].Type != events.ExecutionEventTypeToolCallCompleted {
		t.Fatalf("events = %#v, want limited filtered seq 2", listed.Events)
	}
}

func TestTaskEventsMissingTaskAndNamespaceAuthorization(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()

	t.Run("missing task returns 404", func(t *testing.T) {
		h, app := setupTaskEventHandlers(t, eventStore)
		app.Get("/api/v1/tasks/:id/events", h.ListTaskEvents)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/missing/events?namespace=default", nil)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("watch namespace mismatch returns 403", func(t *testing.T) {
		h, app := setupTaskEventHandlers(t, eventStore, testTask("allowed", "task-1"))
		h.watchNamespace = "allowed"
		app.Get("/api/v1/tasks/:id/events", h.ListTaskEvents)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/task-1/events?namespace=other", nil)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.StatusCode)
		}
	})
}

func TestListTaskEventsUsesStreamIDWhenTaskNameMetadataDiffers(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "task-1",
		TaskName:   "stale-task-metadata",
		Type:       events.ExecutionEventTypeWorkerStarted,
		Severity:   events.ExecutionEventSeverityInfo,
	}); err != nil {
		t.Fatalf("AppendExecutionEvent: %v", err)
	}
	h, app := setupTaskEventHandlers(t, eventStore, testTask("default", "task-1"))
	app.Get("/api/v1/tasks/:id/events", h.ListTaskEvents)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/task-1/events?namespace=default", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var listed ListExecutionEventsResponse
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(listed.Events) != 1 || listed.Events[0].TaskName != "stale-task-metadata" {
		t.Fatalf("events = %#v, want stream event despite taskName metadata mismatch", listed.Events)
	}
}

func TestStreamTaskEventsSSEReplayHeartbeatAndPolling(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStoreWithClock(func() time.Time {
		return time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	})
	appendTestTaskEvent(t, eventStore, "task-1", events.ExecutionEventTypeTaskStarted)
	appendTestTaskEvent(t, eventStore, "task-1", events.ExecutionEventTypeTaskSucceeded)

	t.Run("replays existing events in sequence order", func(t *testing.T) {
		h, app := setupTaskEventHandlers(t, eventStore, testTask("default", "task-1"))
		configureShortTaskEventStream(h)
		useCancelingContext(app, 25*time.Millisecond)
		app.Get("/api/v1/tasks/:id/stream", h.StreamTaskEvents)
		body := doStreamRequest(t, app, "/api/v1/tasks/task-1/stream?namespace=default")
		first := strings.Index(body, "id: 1")
		second := strings.Index(body, "id: 2")
		if first < 0 || second < 0 || first > second {
			t.Fatalf("SSE body does not replay seq 1 then 2: %q", body)
		}
		if !strings.Contains(body, "event: execution_event") {
			t.Fatalf("SSE body missing execution_event frame: %q", body)
		}
	})

	t.Run("emits newly appended event without reconnect", func(t *testing.T) {
		liveStore := store.NewFakeExecutionEventStore()
		h, app := setupTaskEventHandlers(t, liveStore, testTask("default", "task-live"))
		configureShortTaskEventStream(h)
		useCancelingContext(app, 60*time.Millisecond)
		app.Get("/api/v1/tasks/:id/stream", h.StreamTaskEvents)
		appendErr := make(chan error, 1)
		go func() {
			time.Sleep(15 * time.Millisecond)
			_, err := liveStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
				Namespace:  "default",
				StreamType: store.ExecutionEventStreamTypeTask,
				StreamID:   "task-live",
				TaskName:   "task-live",
				Type:       events.ExecutionEventTypeWorkerStarted,
				Severity:   events.ExecutionEventSeverityInfo,
				Summary:    "worker started",
			})
			appendErr <- err
		}()
		body := doStreamRequest(t, app, "/api/v1/tasks/task-live/stream?namespace=default")
		if err := <-appendErr; err != nil {
			t.Fatalf("AppendExecutionEvent live: %v", err)
		}
		if !strings.Contains(body, events.ExecutionEventTypeWorkerStarted) || !strings.Contains(body, "id: 1") {
			t.Fatalf("SSE body missing appended event: %q", body)
		}
	})

	t.Run("heartbeat when idle", func(t *testing.T) {
		idleStore := store.NewFakeExecutionEventStore()
		h, app := setupTaskEventHandlers(t, idleStore, testTask("default", "task-idle"))
		configureShortTaskEventStream(h)
		useCancelingContext(app, 25*time.Millisecond)
		app.Get("/api/v1/tasks/:id/stream", h.StreamTaskEvents)
		body := doStreamRequest(t, app, "/api/v1/tasks/task-idle/stream?namespace=default")
		if !strings.Contains(body, ": heartbeat") {
			t.Fatalf("SSE body missing heartbeat: %q", body)
		}
	})
}

func TestStreamTaskEventsFlushesCompletionAfterExactLimitBatch(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	for range store.MaxExecutionEventLimit - 1 {
		appendTestTaskEvent(t, eventStore, "task-limit", events.ExecutionEventTypeWorkerStarted)
	}
	appendTestTaskEvent(t, eventStore, "task-limit", events.ExecutionEventTypeTaskSucceeded)
	h, app := setupTaskEventHandlers(t, eventStore, testTask("default", "task-limit"))
	configureShortTaskEventStream(h)
	useCancelingContext(app, 100*time.Millisecond)
	app.Get("/api/v1/tasks/:id/stream", h.StreamTaskEvents)
	body := doStreamRequest(t, app, "/api/v1/tasks/task-limit/stream?namespace=default")
	if !strings.Contains(body, "event: stream_complete") {
		t.Fatalf("stream body missing completion frame after exact limit batch")
	}
}

func TestStreamTaskEventsReplaysPostTerminalEventsBeforeComplete(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	appendTestTaskEvent(t, eventStore, "task-terminal", events.ExecutionEventTypeTaskStarted)
	appendTestTaskEvent(t, eventStore, "task-terminal", events.ExecutionEventTypeTaskSucceeded)
	appendTestTaskEvent(t, eventStore, "task-terminal", events.ExecutionEventTypeTaskForkCreated)
	h, app := setupTaskEventHandlers(t, eventStore, testTask("default", "task-terminal"))
	configureShortTaskEventStream(h)
	useCancelingContext(app, 50*time.Millisecond)
	app.Get("/api/v1/tasks/:id/stream", h.StreamTaskEvents)
	body := doStreamRequest(t, app, "/api/v1/tasks/task-terminal/stream?namespace=default")
	forkIndex := strings.Index(body, events.ExecutionEventTypeTaskForkCreated)
	completeIndex := strings.Index(body, "event: stream_complete")
	if forkIndex < 0 || completeIndex < 0 || forkIndex > completeIndex {
		t.Fatalf("stream body = %q, want fork event before stream_complete", body)
	}
	if !strings.Contains(body, "id: 3\nevent: stream_complete") || !strings.Contains(body, `"lastSeq":3`) {
		t.Fatalf("stream body = %q, want completion cursor at last delivered event", body)
	}
}

func TestSSETaskEventsCancellationExitsCleanly(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	h, app := setupTaskEventHandlers(t, eventStore, testTask("default", "task-cancel"))
	configureShortTaskEventStream(h)
	app.Use(func(c fiber.Ctx) error {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(time.Millisecond, cancel)
		c.SetContext(ctx)
		return c.Next()
	})
	app.Get("/api/v1/tasks/:id/stream", h.StreamTaskEvents)
	_ = doStreamRequest(t, app, "/api/v1/tasks/task-cancel/stream?namespace=default")
}

func setupTaskEventHandlers(t *testing.T, eventStore store.ExecutionEventStore, objs ...runtime.Object) (*Handlers, *fiber.App) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	h := NewHandlers(HandlersConfig{Client: fakeClient, ExecutionEventStore: eventStore})
	app := fiber.New()
	return h, app
}

func testTask(namespace, name string) *corev1alpha1.Task {
	return &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
}

func appendTestTaskEvent(t *testing.T, eventStore store.ExecutionEventStore, taskName, typ string) {
	t.Helper()
	if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   taskName,
		TaskName:   taskName,
		Type:       typ,
		Severity:   events.ExecutionEventSeverityInfo,
		Summary:    typ + " summary",
	}); err != nil {
		t.Fatalf("AppendExecutionEvent default/%s/%s: %v", taskName, typ, err)
	}
}

func configureShortTaskEventStream(h *Handlers) {
	h.eventStreamPollInterval = 5 * time.Millisecond
	h.eventStreamHeartbeatEvery = 5 * time.Millisecond
}

func useCancelingContext(app *fiber.App, after time.Duration) {
	app.Use(func(c fiber.Ctx) error {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(after, cancel)
		c.SetContext(ctx)
		return c.Next()
	})
}

func doStreamRequest(t *testing.T, app *fiber.App, target string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read body: %v", err)
	}
	return string(data)
}
