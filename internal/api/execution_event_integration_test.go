package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
		"type":    events.ExecutionEventTypeTaskSucceeded,
		"summary": "task completed",
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

func TestSSEExecutionEventFromInternalPostSQLite(t *testing.T) {
	eventStore := newSQLiteExecutionEventStoreForTest(t)
	app := setupExecutionEventIntegrationApp(
		t,
		eventStore,
		&UserInfo{Username: "system:serviceaccount:default:worker", Namespace: "default"},
		testTask("default", "task-post-stream"),
	)

	resp := doJSONRequest(t, app, "/internal/v1/events/default/task/task-post-stream", map[string]any{
		"type":    events.ExecutionEventTypeTaskSucceeded,
		"summary": "posted terminal event",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d, want 201", resp.StatusCode)
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

func testTaskWithSpec(namespace, name string) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
	}
}
