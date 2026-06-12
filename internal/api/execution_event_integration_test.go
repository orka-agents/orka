package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/store/sqlite"
)

func TestSubmitListExecutionEventSQLite(t *testing.T) {
	eventStore := newSQLiteExecutionEventStoreForTest(t)
	app := setupExecutionEventIntegrationApp(
		t,
		eventStore,
		&UserInfo{Username: "system:serviceaccount:default:worker", Namespace: "default"},
		testTask("default", "task-a"),
	)

	first := doJSONRequest(t, app, "/internal/v1/events/default/task/task-a", map[string]any{
		"id":        "client-id-ignored",
		"seq":       99,
		"type":      events.ExecutionEventTypeWorkerStarted,
		"severity":  "warning",
		"summary":   "worker started",
		"taskName":  "client-task-name-preserved",
		"createdAt": "2000-01-01T00:00:00Z",
	})
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first POST status = %d, want 201", first.StatusCode)
	}
	var firstSubmitted SubmitExecutionEventResponse
	if err := json.NewDecoder(first.Body).Decode(&firstSubmitted); err != nil {
		t.Fatalf("decode first submit response: %v", err)
	}
	if firstSubmitted.Seq != 1 || firstSubmitted.ID != "default/task/task-a/1" {
		t.Fatalf("first submit = %#v, want server/store assigned seq 1 id", firstSubmitted)
	}

	listed := getTaskEvents(t, app, "/api/v1/tasks/task-a/events?namespace=default")
	if listed.LatestSeq != 1 ||
		len(listed.Events) != 1 ||
		listed.Events[0].Seq != 1 ||
		listed.Events[0].Type != events.ExecutionEventTypeWorkerStarted {
		t.Fatalf("listed after first POST = %#v", listed)
	}
	if listed.Events[0].ID == "client-id-ignored" {
		t.Fatalf("public event kept client id: %#v", listed.Events[0])
	}

	second := doJSONRequest(t, app, "/internal/v1/events/default/task/task-a", map[string]any{
		"seq":     1,
		"type":    events.ExecutionEventTypeWorkerCompleted,
		"summary": "worker completed",
	})
	if second.StatusCode != http.StatusCreated {
		t.Fatalf("second POST status = %d, want 201", second.StatusCode)
	}
	var secondSubmitted SubmitExecutionEventResponse
	if err := json.NewDecoder(second.Body).Decode(&secondSubmitted); err != nil {
		t.Fatalf("decode second submit response: %v", err)
	}
	if secondSubmitted.Seq != 2 || secondSubmitted.ID != "default/task/task-a/2" {
		t.Fatalf("second submit = %#v, want server/store assigned seq 2 id", secondSubmitted)
	}

	listed = getTaskEvents(t, app, "/api/v1/tasks/task-a/events?namespace=default")
	if listed.LatestSeq != 2 || len(listed.Events) != 2 || listed.Events[0].Seq != 1 || listed.Events[1].Seq != 2 {
		t.Fatalf("listed after second POST = %#v, want seq 1,2", listed)
	}
	after := getTaskEvents(t, app, "/api/v1/tasks/task-a/events?namespace=default&after=1")
	if after.AfterSeq != 1 || after.LatestSeq != 2 || len(after.Events) != 1 || after.Events[0].Seq != 2 {
		t.Fatalf("listed after=1 = %#v, want only seq 2", after)
	}
}

func TestTaskEventsSQLiteReadsPersistedEvents(t *testing.T) {
	eventStore := newSQLiteExecutionEventStoreForTest(t)
	if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "task-persisted",
		TaskName:   "task-persisted",
		Type:       events.ExecutionEventTypeTaskStarted,
		Severity:   events.ExecutionEventSeverityInfo,
		Summary:    "persisted event",
	}); err != nil {
		t.Fatalf("AppendExecutionEvent: %v", err)
	}
	app := setupExecutionEventIntegrationApp(
		t,
		eventStore,
		&UserInfo{Username: "system:serviceaccount:default:worker", Namespace: "default"},
		testTask("default", "task-persisted"),
	)
	listed := getTaskEvents(t, app, "/api/v1/tasks/task-persisted/events?namespace=default")
	if len(listed.Events) != 1 ||
		listed.Events[0].Type != events.ExecutionEventTypeTaskStarted ||
		listed.Events[0].Summary != "persisted event" {
		t.Fatalf("listed persisted event = %#v", listed)
	}
}

func TestTaskLifecycleEventsQueryableThroughAPI(t *testing.T) {
	eventStore := newSQLiteExecutionEventStoreForTest(t)
	for _, typ := range []string{
		events.ExecutionEventTypeTaskJobCreated,
		events.ExecutionEventTypeTaskStarted,
		events.ExecutionEventTypeTaskSucceeded,
	} {
		appendIntegrationTaskEvent(t, eventStore, "task-lifecycle-api", typ)
	}
	app := setupExecutionEventIntegrationApp(
		t,
		eventStore,
		&UserInfo{Username: "system:serviceaccount:default:worker", Namespace: "default"},
		testTask("default", "task-lifecycle-api"),
	)

	listed := getTaskEvents(t, app, "/api/v1/tasks/task-lifecycle-api/events?namespace=default&after=0")
	if len(listed.Events) != 3 || listed.LatestSeq != 3 {
		t.Fatalf("listed lifecycle events = %#v, want 3 events with latest seq 3", listed)
	}
	var previousSeq int64
	for i, event := range listed.Events {
		if event.Seq <= previousSeq {
			t.Fatalf("event seqs not strictly increasing: %#v", listed.Events)
		}
		previousSeq = event.Seq
		if want := []string{
			events.ExecutionEventTypeTaskJobCreated,
			events.ExecutionEventTypeTaskStarted,
			events.ExecutionEventTypeTaskSucceeded,
		}[i]; event.Type != want {
			t.Fatalf("event[%d].Type = %s, want %s", i, event.Type, want)
		}
	}
	afterFirst := getTaskEvents(t, app, "/api/v1/tasks/task-lifecycle-api/events?namespace=default&after=1")
	if len(afterFirst.Events) != 2 || afterFirst.AfterSeq != 1 {
		t.Fatalf("after=1 response = %#v, want two later events", afterFirst)
	}
	for _, event := range afterFirst.Events {
		if event.Seq <= 1 {
			t.Fatalf("after=1 returned old event: %#v", afterFirst.Events)
		}
	}
}

func TestStreamTaskEventsSQLiteReplayLiveAndTerminal(t *testing.T) {
	eventStore := newSQLiteExecutionEventStoreForTest(t)
	appendIntegrationTaskEvent(t, eventStore, "task-stream", events.ExecutionEventTypeTaskStarted)

	h, app := setupTaskEventHandlers(t, eventStore, testTask("default", "task-stream"))
	h.eventStreamPollInterval = 5 * time.Millisecond
	h.eventStreamHeartbeatEvery = 250 * time.Millisecond
	app.Get("/api/v1/tasks/:id/stream", h.StreamTaskEvents)

	appendErr := make(chan error, 1)
	go func() {
		time.Sleep(15 * time.Millisecond)
		_, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
			Namespace:  "default",
			StreamType: store.ExecutionEventStreamTypeTask,
			StreamID:   "task-stream",
			TaskName:   "task-stream",
			Type:       events.ExecutionEventTypeWorkerStarted,
			Severity:   events.ExecutionEventSeverityInfo,
			Summary:    "live worker event",
		})
		if err != nil {
			appendErr <- err
			return
		}
		_, err = eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
			Namespace:  "default",
			StreamType: store.ExecutionEventStreamTypeTask,
			StreamID:   "task-stream",
			TaskName:   "task-stream",
			Type:       events.ExecutionEventTypeTaskSucceeded,
			Severity:   events.ExecutionEventSeverityInfo,
			Summary:    "terminal event",
		})
		appendErr <- err
	}()

	body := doStreamRequest(t, app, "/api/v1/tasks/task-stream/stream?namespace=default")
	if err := <-appendErr; err != nil {
		t.Fatalf("append live/terminal event: %v", err)
	}
	for _, want := range []string{
		"id: 1",
		events.ExecutionEventTypeTaskStarted,
		"id: 2",
		events.ExecutionEventTypeWorkerStarted,
		"id: 3",
		events.ExecutionEventTypeTaskSucceeded,
		"event: stream_complete",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("SSE body missing %q: %q", want, body)
		}
	}
}

func TestReconnectSkipsSeenExecutionEventsSQLite(t *testing.T) {
	eventStore := newSQLiteExecutionEventStoreForTest(t)
	appendIntegrationTaskEvent(t, eventStore, "task-reconnect", events.ExecutionEventTypeTaskStarted)
	appendIntegrationTaskEvent(t, eventStore, "task-reconnect", events.ExecutionEventTypeTaskSucceeded)

	h, app := setupTaskEventHandlers(t, eventStore, testTask("default", "task-reconnect"))
	configureShortTaskEventStream(h)
	app.Get("/api/v1/tasks/:id/stream", h.StreamTaskEvents)

	body := doStreamRequest(t, app, "/api/v1/tasks/task-reconnect/stream?namespace=default&after=1")
	if strings.Contains(body, "id: 1\n") || strings.Contains(body, events.ExecutionEventTypeTaskStarted) {
		t.Fatalf("reconnect body replayed old event: %q", body)
	}
	if !strings.Contains(body, "id: 2") ||
		!strings.Contains(body, events.ExecutionEventTypeTaskSucceeded) ||
		!strings.Contains(body, "event: stream_complete") {
		t.Fatalf("reconnect body missing terminal new event: %q", body)
	}
}

func TestReconnectAfterTerminalExecutionEventCompletesSQLite(t *testing.T) {
	eventStore := newSQLiteExecutionEventStoreForTest(t)
	appendIntegrationTaskEvent(t, eventStore, "task-terminal-reconnect", events.ExecutionEventTypeTaskStarted)
	appendIntegrationTaskEvent(t, eventStore, "task-terminal-reconnect", events.ExecutionEventTypeTaskSucceeded)

	h, app := setupTaskEventHandlers(t, eventStore, testTask("default", "task-terminal-reconnect"))
	configureShortTaskEventStream(h)
	app.Get("/api/v1/tasks/:id/stream", h.StreamTaskEvents)

	body := doStreamRequest(
		t,
		app,
		"/api/v1/tasks/task-terminal-reconnect/stream?namespace=default&after=2",
	)
	if strings.Contains(body, "event: execution_event") {
		t.Fatalf("reconnect after terminal replayed event frames: %q", body)
	}
	if !strings.Contains(body, "id: 2") || !strings.Contains(body, "event: stream_complete") {
		t.Fatalf("reconnect after terminal did not complete stream: %q", body)
	}
}

func TestStreamTaskEventsFilteredSQLiteCompletesAfterTerminal(t *testing.T) {
	eventStore := newSQLiteExecutionEventStoreForTest(t)
	appendIntegrationTaskEvent(t, eventStore, "task-filtered", events.ExecutionEventTypeToolCallCompleted)
	appendIntegrationTaskEvent(t, eventStore, "task-filtered", events.ExecutionEventTypeTaskSucceeded)

	h, app := setupTaskEventHandlers(t, eventStore, testTask("default", "task-filtered"))
	configureShortTaskEventStream(h)
	app.Get("/api/v1/tasks/:id/stream", h.StreamTaskEvents)

	body := doStreamRequest(
		t,
		app,
		"/api/v1/tasks/task-filtered/stream?namespace=default&type=ToolCallCompleted",
	)
	if !strings.Contains(body, events.ExecutionEventTypeToolCallCompleted) {
		t.Fatalf("filtered stream missing matching event: %q", body)
	}
	if strings.Contains(body, `"type":"TaskSucceeded","severity"`) {
		t.Fatalf("filtered stream included excluded terminal execution event: %q", body)
	}
	if !strings.Contains(body, "id: 2") || !strings.Contains(body, "event: stream_complete") {
		t.Fatalf("filtered stream did not complete at terminal seq: %q", body)
	}
}

func TestStreamTaskEventsCompletesWhenTaskDeleted(t *testing.T) {
	eventStore := newSQLiteExecutionEventStoreForTest(t)
	task := testTask("default", "task-delete-stream")
	h, app := setupTaskEventHandlers(t, eventStore, task)
	configureShortTaskEventStream(h)
	app.Get("/api/v1/tasks/:id/stream", h.StreamTaskEvents)

	deleteErr := make(chan error, 1)
	go func() {
		time.Sleep(15 * time.Millisecond)
		deleteErr <- h.client.Delete(context.Background(), task.DeepCopy())
	}()
	body := doStreamRequestAllowUnexpectedEOF(t, app, "/api/v1/tasks/task-delete-stream/stream?namespace=default")
	if err := <-deleteErr; err != nil {
		t.Fatalf("Delete task: %v", err)
	}
	if !strings.Contains(body, "event: stream_complete") || !strings.Contains(body, "TaskDeleted") {
		t.Fatalf("deleted task stream did not complete: %q", body)
	}
}

func TestStreamTaskEventsReconnectCatchUpSQLite(t *testing.T) {
	eventStore := newSQLiteExecutionEventStoreForTest(t)
	postApp := setupExecutionEventIntegrationApp(
		t,
		eventStore,
		&UserInfo{Username: "system:serviceaccount:default:worker", Namespace: "default"},
		testTask("default", "task-reconnect-e2e"),
	)
	resp := doJSONRequest(t, postApp, "/internal/v1/events/default/task/task-reconnect-e2e", map[string]any{
		"type":    events.ExecutionEventTypeTaskStarted,
		"summary": "initial event from internal POST",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("initial POST status = %d, want 201", resp.StatusCode)
	}

	firstStream, firstApp := setupTaskEventHandlers(t, eventStore, testTask("default", "task-reconnect-e2e"))
	configureShortTaskEventStream(firstStream)
	firstApp.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{Username: "system:serviceaccount:default:worker", Namespace: "default"})
		return c.Next()
	})
	useCancelingContext(firstApp, 25*time.Millisecond)
	firstApp.Get("/api/v1/tasks/:id/stream", firstStream.StreamTaskEvents)
	firstBody := doStreamRequest(t, firstApp, "/api/v1/tasks/task-reconnect-e2e/stream?namespace=default&after=0")
	firstIDs := sseIDs(firstBody)
	if len(firstIDs) != 1 || firstIDs[0] != 1 {
		t.Fatalf("first stream IDs = %v, body = %q; want only initial seq 1 before disconnect", firstIDs, firstBody)
	}
	if !strings.Contains(firstBody, ": heartbeat") {
		t.Fatalf("first stream body missing shortened heartbeat before disconnect: %q", firstBody)
	}
	lastSeq := firstIDs[len(firstIDs)-1]

	resp = doJSONRequest(t, postApp, "/internal/v1/events/default/task/task-reconnect-e2e", map[string]any{
		"type":    events.ExecutionEventTypeWorkerStarted,
		"summary": "missed event from internal POST",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("missed POST status = %d, want 201", resp.StatusCode)
	}
	terminalErr := make(chan string, 1)
	go func() {
		time.Sleep(15 * time.Millisecond)
		_, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
			Namespace:  "default",
			StreamType: store.ExecutionEventStreamTypeTask,
			StreamID:   "task-reconnect-e2e",
			TaskName:   "task-reconnect-e2e",
			Type:       events.ExecutionEventTypeTaskSucceeded,
			Severity:   events.ExecutionEventSeverityInfo,
			Summary:    "terminal event from controller",
		})
		if err != nil {
			terminalErr <- err.Error()
			return
		}
		terminalErr <- ""
	}()

	secondStream, secondApp := setupTaskEventHandlers(t, eventStore, testTask("default", "task-reconnect-e2e"))
	configureShortTaskEventStream(secondStream)
	secondApp.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{Username: "system:serviceaccount:default:worker", Namespace: "default"})
		return c.Next()
	})
	secondApp.Get("/api/v1/tasks/:id/stream", secondStream.StreamTaskEvents)
	secondBody := doStreamRequest(t, secondApp, "/api/v1/tasks/task-reconnect-e2e/stream?namespace=default&after=1")
	if errMsg := <-terminalErr; errMsg != "" {
		t.Fatal(errMsg)
	}
	secondIDs := sseIDs(secondBody)
	if len(secondIDs) != 3 || secondIDs[0] != 2 || secondIDs[1] != 3 || secondIDs[2] != 3 {
		t.Fatalf("second stream IDs = %v, body = %q; want event seqs 2,3 plus stream_complete id 3", secondIDs, secondBody)
	}
	for _, id := range secondIDs {
		if id <= lastSeq {
			t.Fatalf("reconnect replayed duplicate seq <= %d: IDs=%v body=%q", lastSeq, secondIDs, secondBody)
		}
	}
	if strings.Contains(secondBody, "initial event from internal POST") ||
		!strings.Contains(secondBody, "missed event from internal POST") ||
		!strings.Contains(secondBody, "event: stream_complete") {
		t.Fatalf("second stream body did not catch up correctly: %q", secondBody)
	}
}

func TestSSEExecutionEventFromInternalPostSQLite(t *testing.T) {
	eventStore := newSQLiteExecutionEventStoreForTest(t)
	app := setupExecutionEventIntegrationApp(
		t,
		eventStore,
		&UserInfo{Username: "system:serviceaccount:default:worker", Namespace: "default"},
		testTask("default", "task-post-stream"),
	)

	if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "task-post-stream",
		TaskName:   "task-post-stream",
		Type:       events.ExecutionEventTypeTaskSucceeded,
		Severity:   events.ExecutionEventSeverityInfo,
		Summary:    "posted terminal event",
	}); err != nil {
		t.Fatalf("AppendExecutionEvent: %v", err)
	}

	body := doStreamRequest(t, app, "/api/v1/tasks/task-post-stream/stream?namespace=default")
	if !strings.Contains(body, events.ExecutionEventTypeTaskSucceeded) ||
		!strings.Contains(body, "posted terminal event") ||
		!strings.Contains(body, "event: stream_complete") {
		t.Fatalf("SSE body missing internal POST event: %q", body)
	}
}

func TestEventAuthListAndStream(t *testing.T) {
	eventStore := newSQLiteExecutionEventStoreForTest(t)
	appendIntegrationTaskEvent(t, eventStore, "task-auth", events.ExecutionEventTypeTaskSucceeded)

	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	app := setupAuthenticatedExecutionEventApp(
		t, eventStore, ctxTokenConfig, false, testTaskWithSpec("default", "task-auth"),
	)

	unauthList := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/task-auth/events?namespace=default", nil)
	resp, err := app.Test(unauthList)
	if err != nil {
		t.Fatalf("unauth list app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth list status = %d, want 401", resp.StatusCode)
	}

	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeTaskGet,
		"tctx":  map[string]any{"namespace": "default", "taskName": "task-auth"},
	})
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/task-auth/events?namespace=default", nil)
	listReq.Header.Set(KontxtHeaderName, token)
	resp, err = app.Test(listReq)
	if err != nil {
		t.Fatalf("auth list app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auth list status = %d, want 200", resp.StatusCode)
	}

	streamReq := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/task-auth/stream?namespace=default", nil)
	streamReq.Header.Set(KontxtHeaderName, token)
	resp, err = app.Test(streamReq, fiber.TestConfig{Timeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("auth stream app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auth stream status = %d, want 200", resp.StatusCode)
	}
}

func TestEventNamespaceIsolationPublicAndInternal(t *testing.T) {
	eventStore := newSQLiteExecutionEventStoreForTest(t)
	appendIntegrationTaskEvent(t, eventStore, "task-other", events.ExecutionEventTypeTaskStarted)

	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	app := setupAuthenticatedExecutionEventApp(
		t, eventStore, ctxTokenConfig, true, testTaskWithSpec("other", "task-other"),
	)
	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeTaskGet,
		"tctx":  map[string]any{"namespace": "default", "taskName": "task-other"},
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/task-other/events?namespace=other", nil)
	req.Header.Set(KontxtHeaderName, token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("namespace list app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-namespace public list status = %d, want 403", resp.StatusCode)
	}

	internalApp := setupInternalExecutionEventApp(
		eventStore,
		&UserInfo{Username: "system:serviceaccount:default:worker", Namespace: "default"},
	)
	internalResp := doJSONRequest(
		t,
		internalApp,
		"/internal/v1/events/other/task/task-other",
		map[string]any{"type": events.ExecutionEventTypeTaskStarted},
	)
	if internalResp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-namespace internal submit status = %d, want 403", internalResp.StatusCode)
	}
}

func TestEventRedactPublicDTO(t *testing.T) {
	eventStore := newSQLiteExecutionEventStoreForTest(t)
	app := setupExecutionEventIntegrationApp(
		t,
		eventStore,
		&UserInfo{Username: "system:serviceaccount:default:worker", Namespace: "default"},
		testTask("default", "task-redact"),
	)
	secret := testDashToken("bearer")
	resp := doJSONRequest(t, app, "/internal/v1/events/default/task/task-redact", map[string]any{
		"type":        events.ExecutionEventTypeModelMessage,
		"summary":     "Authorization: Bearer " + secret,
		"content":     map[string]any{"token": secret, "safe": "ok"},
		"contentText": "token=" + secret,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d, want 201", resp.StatusCode)
	}

	listed := getTaskEvents(t, app, "/api/v1/tasks/task-redact/events?namespace=default")
	data, err := json.Marshal(listed)
	if err != nil {
		t.Fatalf("marshal listed response: %v", err)
	}
	if strings.Contains(string(data), secret) || strings.Contains(string(data), "Authorization: Bearer") {
		t.Fatalf("public response leaked secret: %s", data)
	}
	dataText := string(data)
	if strings.Contains(dataText, "Internal") ||
		strings.Contains(dataText, "rawToken") ||
		strings.Contains(dataText, "authorization") {
		t.Fatalf("public DTO leaked internal/auth fields: %s", data)
	}
	if len(listed.Events) != 1 || !strings.Contains(string(listed.Events[0].Content), events.ExecutionEventRedactedValue) {
		t.Fatalf("listed event content not redacted as expected: %#v", listed.Events)
	}
}

func TestEventRedactionAuditPersistsAndServesRedactedPayloads(t *testing.T) {
	eventStore := newSQLiteExecutionEventStoreForTest(t)
	app := setupExecutionEventIntegrationApp(
		t,
		eventStore,
		&UserInfo{Username: "system:serviceaccount:default:worker", Namespace: "default"},
		testTask("default", "task-redaction-audit"),
	)
	secrets := eventAuditSecrets()
	contentText := strings.Join([]string{
		"Authorization: Bearer " + secrets["bearer"],
		"Cookie: session=" + secrets["cookie"],
		"Transaction-Token: " + secrets["txn"],
		"jwt=" + secrets["jwt"],
		"openai=" + secrets["openai"],
		"github=" + secrets["github"],
		"anthropic=" + secrets["anthropic"],
	}, "\n")
	resp := doJSONRequest(t, app, "/internal/v1/events/default/task/task-redaction-audit", map[string]any{
		"type":    events.ExecutionEventTypeModelMessage,
		"summary": "api_key=" + secrets["openai"] + " Authorization: Bearer " + secrets["bearer"],
		"content": map[string]any{
			"authorization":     "Bearer " + secrets["bearer"],
			"jwt":               secrets["jwt"],
			"apiKey":            secrets["openai"],
			"cookie":            "session=" + secrets["cookie"],
			"transaction-token": secrets["txn"],
			"githubToken":       secrets["github"],
			"anthropic_api_key": secrets["anthropic"],
			"safe":              "preserved",
		},
		"contentText": contentText + "\n" + strings.Repeat("x", events.MaxExecutionEventContentTextChars),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d, want 201", resp.StatusCode)
	}

	persisted, err := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "task-redaction-audit",
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents persisted: %v", err)
	}
	persistedData, err := json.Marshal(persisted)
	if err != nil {
		t.Fatalf("marshal persisted events: %v", err)
	}
	listed := getTaskEvents(t, app, "/api/v1/tasks/task-redaction-audit/events?namespace=default")
	publicData, err := json.Marshal(listed)
	if err != nil {
		t.Fatalf("marshal public response: %v", err)
	}
	for name, secret := range secrets {
		if strings.Contains(string(persistedData), secret) {
			t.Fatalf("persisted events leaked %s secret %q in %s", name, secret, persistedData)
		}
		if strings.Contains(string(publicData), secret) {
			t.Fatalf("public response leaked %s secret %q in %s", name, secret, publicData)
		}
	}
	if !strings.Contains(string(persistedData), events.ExecutionEventRedactedValue) ||
		!strings.Contains(string(publicData), events.ExecutionEventRedactedValue) {
		t.Fatalf("redaction marker missing persisted=%s public=%s", persistedData, publicData)
	}
	if len(listed.Events) != 1 || listed.Events[0].Truncation == nil || !listed.Events[0].Truncation.ContentTextTruncated {
		t.Fatalf("listed truncation = %#v, want contentText truncation preserved", listed.Events)
	}
}

func setupExecutionEventIntegrationApp(
	t *testing.T,
	eventStore store.ExecutionEventStore,
	userInfo *UserInfo,
	objs ...runtime.Object,
) *fiber.App {
	t.Helper()
	h, app := setupTaskEventHandlers(t, eventStore, objs...)
	configureShortTaskEventStream(h)
	if userInfo != nil {
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, userInfo)
			return c.Next()
		})
	}
	internal := NewInternalHandlers(nil, nil, nil, nil, nil, InternalHandlersConfig{ExecutionEventStore: eventStore})
	app.Post("/internal/v1/events/:namespace/:streamType/:streamID", internal.SubmitExecutionEvent)
	app.Get("/api/v1/tasks/:id/events", h.ListTaskEvents)
	app.Get("/api/v1/tasks/:id/stream", h.StreamTaskEvents)
	return app
}

func setupAuthenticatedExecutionEventApp(
	t *testing.T,
	eventStore store.ExecutionEventStore,
	ctxTokenConfig ContextTokenConfig,
	enforceNamespaceIsolation bool,
	objs ...runtime.Object,
) *fiber.App {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{
		Mode: ContextTokenAuthorizationModeEnforce,
	})
	if err != nil {
		t.Fatalf("NewContextTokenAuthorizationConfig: %v", err)
	}
	h := NewHandlers(HandlersConfig{
		Client:                    fakeClient,
		ExecutionEventStore:       eventStore,
		EnforceNamespaceIsolation: enforceNamespaceIsolation,
		ContextTokenAuthorization: authz,
	})
	configureShortTaskEventStream(h)
	app := fiber.New()
	app.Use(NewAuthMiddleware(nil, AuthConfig{ContextTokens: ctxTokenConfig}))
	app.Get("/api/v1/tasks/:id/events", h.ListTaskEvents)
	app.Get("/api/v1/tasks/:id/stream", h.StreamTaskEvents)
	return app
}

func newSQLiteExecutionEventStoreForTest(t *testing.T) *sqlite.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "events.db")
	db, err := sqlite.NewDB(dbPath)
	if err != nil {
		t.Fatalf("sqlite.NewDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return sqlite.NewStore(db, dbPath)
}

func doStreamRequestAllowUnexpectedEOF(t *testing.T, app *fiber.App, target string) string {
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

func getTaskEvents(t *testing.T, app *fiber.App, target string) ListExecutionEventsResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("GET %s app.Test: %v", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", target, resp.StatusCode)
	}
	var listed ListExecutionEventsResponse
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatalf("decode %s response: %v", target, err)
	}
	return listed
}

func appendIntegrationTaskEvent(t *testing.T, eventStore store.ExecutionEventStore, taskName, eventType string) {
	t.Helper()
	if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   taskName,
		TaskName:   taskName,
		Type:       eventType,
		Severity:   events.ExecutionEventSeverityInfo,
		Summary:    eventType + " summary",
	}); err != nil {
		t.Fatalf("AppendExecutionEvent %s/%s: %v", taskName, eventType, err)
	}
}

func sseIDs(body string) []int64 {
	ids := []int64{}
	for line := range strings.SplitSeq(body, "\n") {
		raw, ok := strings.CutPrefix(line, "id: ")
		if !ok {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func testDashToken(prefix string) string {
	return strings.Join([]string{prefix, "value", "for", "redaction"}, "-")
}

func eventAuditSecrets() map[string]string {
	return map[string]string{
		"bearer": strings.Join([]string{"bearer", "value", "for", "redaction"}, "-"),
		"jwt": strings.Join([]string{
			"ey" + "JhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9",
			"ey" + "JzdWIiOiJ0YXNrIiwiYXVkIjoib3JrYSJ9",
			"signature" + strings.Repeat("a", 24),
		}, "."),
		"openai":    strings.Join([]string{"sk", strings.Repeat("a", 24)}, "-"),
		"cookie":    strings.Join([]string{"cookie", "value", "for", "redaction"}, "-"),
		"txn":       strings.Join([]string{"txn", "value", "for", "redaction"}, "-"),
		"github":    "github" + "_pat_" + strings.Repeat("a", 32),
		"anthropic": strings.Join([]string{"sk", "ant", "api03", strings.Repeat("a", 32)}, "-"),
	}
}

func testTaskWithSpec(namespace, name string) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
	}
}

type terminalAppearsBetweenStreamQueriesStore struct {
	terminalReturned bool
}

func (s *terminalAppearsBetweenStreamQueriesStore) AppendExecutionEvent(
	context.Context,
	*store.ExecutionEvent,
) (*store.ExecutionEvent, error) {
	return nil, nil
}

func (s *terminalAppearsBetweenStreamQueriesStore) ListExecutionEvents(
	ctx context.Context,
	filter store.ExecutionEventFilter,
) ([]store.ExecutionEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	terminal := store.ExecutionEvent{
		ID:         "default/task/task-terminal-race/1",
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "task-terminal-race",
		Seq:        1,
		TaskName:   "task-terminal-race",
		Type:       events.ExecutionEventTypeTaskSucceeded,
		Severity:   events.ExecutionEventSeverityInfo,
		Summary:    "terminal event",
		CreatedAt:  time.Date(2026, 6, 12, 3, 0, 0, 0, time.UTC),
	}
	if len(filter.EventTypes) > 0 {
		return []store.ExecutionEvent{terminal}, nil
	}
	if !s.terminalReturned {
		s.terminalReturned = true
		return nil, nil
	}
	return []store.ExecutionEvent{terminal}, nil
}

func (s *terminalAppearsBetweenStreamQueriesStore) ListSessionExecutionEvents(
	context.Context,
	store.SessionExecutionEventFilter,
) ([]store.SessionExecutionEvent, int64, error) {
	return nil, 0, nil
}

func (s *terminalAppearsBetweenStreamQueriesStore) GetLatestExecutionEventSeq(
	context.Context,
	string,
	string,
	string,
) (int64, error) {
	return 1, nil
}

func (s *terminalAppearsBetweenStreamQueriesStore) DeleteExecutionEvents(context.Context, string, string, string) error {
	return nil
}

func TestSSEExecutionEventTerminalRaceDeliversTerminalFrame(t *testing.T) {
	eventStore := &terminalAppearsBetweenStreamQueriesStore{}
	h, app := setupTaskEventHandlers(t, eventStore, testTask("default", "task-terminal-race"))
	configureShortTaskEventStream(h)
	useCancelingContext(app, 50*time.Millisecond)
	app.Get("/api/v1/tasks/:id/stream", h.StreamTaskEvents)

	body := doStreamRequest(t, app, "/api/v1/tasks/task-terminal-race/stream?namespace=default")
	if !strings.Contains(body, "event: execution_event") ||
		!strings.Contains(body, events.ExecutionEventTypeTaskSucceeded) {
		t.Fatalf("SSE body did not include terminal execution_event frame: %q", body)
	}
	if !strings.Contains(body, "event: stream_complete") {
		t.Fatalf("SSE body missing stream_complete after terminal event: %q", body)
	}
}
