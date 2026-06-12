package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/approvals"
	"github.com/sozercan/orka/internal/events"
	forkcontext "github.com/sozercan/orka/internal/fork"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/tasktrace"
)

type postP0FakeSessionStore struct {
	records map[string]*store.SessionRecord
}

func (f *postP0FakeSessionStore) key(namespace, name string) string { return namespace + "/" + name }
func (f *postP0FakeSessionStore) CreateSession(ctx context.Context, session *store.SessionRecord) error {
	f.records[f.key(session.Namespace, session.Name)] = session
	return nil
}
func (f *postP0FakeSessionStore) GetSession(ctx context.Context, namespace, name string) (*store.SessionRecord, error) {
	if s := f.records[f.key(namespace, name)]; s != nil {
		return s, nil
	}
	return nil, store.ErrNotFound
}
func (f *postP0FakeSessionStore) ListSessions(ctx context.Context, namespace string) ([]store.SessionMetadata, error) {
	return nil, nil
}
func (f *postP0FakeSessionStore) DeleteSession(ctx context.Context, namespace, name string) error {
	delete(f.records, f.key(namespace, name))
	return nil
}
func (f *postP0FakeSessionStore) AcquireLock(ctx context.Context, namespace, name, taskName string) error {
	return nil
}
func (f *postP0FakeSessionStore) ReleaseLock(ctx context.Context, namespace, name, taskName string) error {
	return nil
}
func (f *postP0FakeSessionStore) IsLocked(ctx context.Context, namespace, name, currentTask string) (bool, error) {
	return false, nil
}
func (f *postP0FakeSessionStore) AppendMessages(ctx context.Context, namespace, name string, messages []store.SessionMessage) error {
	return nil
}
func (f *postP0FakeSessionStore) LoadTranscript(ctx context.Context, namespace, name string, maxMessages int) ([]store.SessionMessage, error) {
	return nil, nil
}
func (f *postP0FakeSessionStore) SearchTranscript(ctx context.Context, filter store.TranscriptSearchFilter) ([]store.TranscriptSearchResult, error) {
	return nil, nil
}
func (f *postP0FakeSessionStore) UpdateTokenCounts(ctx context.Context, namespace, name string, inputTokens, outputTokens int) error {
	return nil
}

func TestListSessionEventsAggregatesTaskEvents(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	appendSessionEvent(t, eventStore, "task-a", events.ExecutionEventTypeTaskStarted, now)
	appendSessionEvent(t, eventStore, "task-b", events.ExecutionEventTypeWorkerStarted, now.Add(time.Second))
	appendSessionEvent(t, eventStore, "task-a", events.ExecutionEventTypeTaskSucceeded, now.Add(2*time.Second))

	h, app := setupPostP0Handlers(t, eventStore, &postP0FakeSessionStore{records: map[string]*store.SessionRecord{"default/session-1": {Namespace: "default", Name: "session-1"}}})
	app.Get("/api/v1/sessions/:id/events", h.ListSessionEvents)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/session-1/events?namespace=default&after=1", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var listed ListSessionExecutionEventsResponse
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if listed.LatestSeq != 3 || len(listed.Events) != 2 || listed.Events[0].Seq != 2 || listed.Events[0].TaskName != "task-b" || listed.Events[0].TaskSeq != 1 {
		t.Fatalf("listed = %#v", listed)
	}
}

func TestStreamSessionEventsReconnect(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	appendSessionEvent(t, eventStore, "task-a", events.ExecutionEventTypeTaskStarted, now)
	appendSessionEvent(t, eventStore, "task-b", events.ExecutionEventTypeWorkerStarted, now.Add(time.Second))
	h, app := setupPostP0Handlers(t, eventStore, &postP0FakeSessionStore{records: map[string]*store.SessionRecord{"default/session-1": {Namespace: "default", Name: "session-1"}}})
	configureShortTaskEventStream(h)
	useCancelingContext(app, 20*time.Millisecond)
	app.Get("/api/v1/sessions/:id/stream", h.StreamSessionEvents)
	body := doStreamRequest(t, app, "/api/v1/sessions/session-1/stream?namespace=default&after=1")
	if strings.Contains(body, "id: 1\n") || !strings.Contains(body, "id: 2") || !strings.Contains(body, events.ExecutionEventTypeWorkerStarted) {
		t.Fatalf("session stream body = %q", body)
	}
}

func TestGetTaskTraceAPI(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	appendTestTaskEvent(t, eventStore, "trace-task", events.ExecutionEventTypeTaskStarted)
	appendToolEvent(t, eventStore, "trace-task", events.ExecutionEventTypeToolCallStarted, "call-1")
	appendToolEvent(t, eventStore, "trace-task", events.ExecutionEventTypeToolCallCompleted, "call-1")
	appendTestTaskEvent(t, eventStore, "trace-task", events.ExecutionEventTypeTaskSucceeded)
	task := testTask("default", "trace-task")
	task.Spec.Type = corev1alpha1.TaskTypeAI
	task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	h, app := setupTaskEventHandlers(t, eventStore, task)
	app.Get("/api/v1/tasks/:id/trace", h.GetTaskTrace)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/trace-task/trace?namespace=default", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var trace tasktrace.TaskTrace
	if err := json.NewDecoder(resp.Body).Decode(&trace); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if trace.LatestSeq != 4 || len(trace.ToolCalls) != 1 || trace.ToolCalls[0].Status != "completed" || !trace.Task.ResultAvailable {
		t.Fatalf("trace = %#v", trace)
	}
}

func TestTaskApprovalDecisionAPIAppendsEvent(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	content, _ := json.Marshal(map[string]string{"approvalID": "approval-1", "action": "create_pr", "riskSummary": "opens a PR"})
	if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "approval-task", TaskName: "approval-task", SessionName: "session-1", Type: events.ExecutionEventTypeApprovalRequested, Content: content}); err != nil {
		t.Fatal(err)
	}
	task := testTask("default", "approval-task")
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "session-1"}
	h, app := setupTaskEventHandlers(t, eventStore, task)
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{Username: "reviewer"})
		return c.Next()
	})
	app.Get("/api/v1/tasks/:id/approvals", h.ListTaskApprovals)
	app.Post("/api/v1/tasks/:id/approvals/:approvalID/decision", h.DecideTaskApproval)

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/approval-task/approvals?namespace=default", nil)
	listResp, err := app.Test(listReq)
	if err != nil {
		t.Fatal(err)
	}
	var listed ListTaskApprovalsResponse
	if err := json.NewDecoder(listResp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Approvals) != 1 || listed.Approvals[0].Status != approvals.StatusPending {
		t.Fatalf("listed=%#v", listed)
	}

	decisionReq := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/approval-task/approvals/approval-1/decision?namespace=default", bytes.NewBufferString(`{"decision":"approve","reason":"safe"}`))
	decisionReq.Header.Set("Content-Type", "application/json")
	decisionResp, err := app.Test(decisionReq)
	if err != nil {
		t.Fatal(err)
	}
	if decisionResp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", decisionResp.StatusCode)
	}
	var approved approvals.Approval
	if err := json.NewDecoder(decisionResp.Body).Decode(&approved); err != nil {
		t.Fatal(err)
	}
	if approved.Status != approvals.StatusApproved || approved.DecisionReason != "safe" || approved.DecisionActor != "reviewer" {
		t.Fatalf("approved=%#v", approved)
	}

	declineReq := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/approval-task/approvals/approval-1/decision?namespace=default", bytes.NewBufferString(`{"decision":"decline"}`))
	declineReq.Header.Set("Content-Type", "application/json")
	declineResp, err := app.Test(declineReq)
	if err != nil {
		t.Fatal(err)
	}
	if declineResp.StatusCode != http.StatusConflict {
		t.Fatalf("decline after approval status=%d, want conflict", declineResp.StatusCode)
	}
	sessionEvents, latest, err := eventStore.ListSessionExecutionEvents(context.Background(), store.SessionExecutionEventFilter{Namespace: "default", SessionName: "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	if latest != 2 || len(sessionEvents) != 2 || sessionEvents[1].Type != events.ExecutionEventTypeApprovalApproved {
		t.Fatalf("session approval events latest=%d events=%#v", latest, sessionEvents)
	}
}

func TestTaskApprovalDecisionAPIRejectsPendingDecisionForCompletedTask(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	content, _ := json.Marshal(map[string]string{"approvalID": "approval-1"})
	if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "approval-complete-task",
		TaskName:   "approval-complete-task",
		Type:       events.ExecutionEventTypeApprovalRequested,
		Content:    content,
	}); err != nil {
		t.Fatal(err)
	}
	task := testTask("default", "approval-complete-task")
	task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	h, app := setupTaskEventHandlers(t, eventStore, task)
	app.Post("/api/v1/tasks/:id/approvals/:approvalID/decision", h.DecideTaskApproval)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/approval-complete-task/approvals/approval-1/decision?namespace=default", bytes.NewBufferString(`{"decision":"approve"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d, want conflict", resp.StatusCode)
	}
	listed, err := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace: "default", StreamID: "approval-complete-task", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Type != events.ExecutionEventTypeApprovalRequested {
		t.Fatalf("events after rejected decision = %#v", listed)
	}
}

func TestTaskApprovalsAPIPagesLifecycleEvents(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	for i := range store.MaxExecutionEventLimit {
		content, _ := json.Marshal(map[string]string{"approvalID": fmt.Sprintf("old-%d", i)})
		if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
			Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "approval-page-task",
			TaskName: "approval-page-task", Type: events.ExecutionEventTypeApprovalRequested, Content: content,
		}); err != nil {
			t.Fatal(err)
		}
	}
	content, _ := json.Marshal(map[string]string{"approvalID": "target"})
	if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "approval-page-task",
		TaskName: "approval-page-task", Type: events.ExecutionEventTypeApprovalRequested, Content: content,
	}); err != nil {
		t.Fatal(err)
	}
	h, app := setupTaskEventHandlers(t, eventStore, testTask("default", "approval-page-task"))
	app.Get("/api/v1/tasks/:id/approvals", h.ListTaskApprovals)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/approval-page-task/approvals?namespace=default", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var listed ListTaskApprovalsResponse
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if _, found := findApproval(listed.Approvals, "target"); !found {
		t.Fatalf("target approval missing from paged response of %d approvals", len(listed.Approvals))
	}
}

func TestForkTaskAPI(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	appendTestTaskEvent(t, eventStore, "source-task", events.ExecutionEventTypeTaskStarted)
	appendTestTaskEvent(t, eventStore, "source-task", events.ExecutionEventTypeWorkerStarted)
	source := testTask("default", "source-task")
	source.Spec.Type = corev1alpha1.TaskTypeAgent
	source.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "session-1"}
	h, app := setupTaskEventHandlers(t, eventStore, source)
	app.Post("/api/v1/tasks/:id/fork", h.ForkTask)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/source-task/fork?namespace=default", bytes.NewBufferString(`{"afterSeq":1,"newTaskName":"forked-task","prompt":"continue"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var out ForkTaskResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.NewTaskName != "forked-task" || out.AfterSeq != 1 || len(out.ForkContext.Events) != 1 {
		t.Fatalf("response=%#v", out)
	}
	created := &corev1alpha1.Task{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "forked-task"}, created); err != nil {
		t.Fatalf("get created: %v", err)
	}
	if created.Annotations[labels.AnnotationForkSourceTask] != "source-task" || created.Annotations[labels.AnnotationForkSourceSeq] != "1" || created.Spec.Prompt != "continue" {
		t.Fatalf("created task = %#v", created)
	}
	listed, err := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{Namespace: "default", StreamID: "forked-task"})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Type != events.ExecutionEventTypeTaskForkCreated || listed[0].SessionName != "session-1" {
		t.Fatalf("fork events = %#v", listed)
	}
}

func TestForkTaskAPIBoundsContextTail(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	for range forkcontext.DefaultMaxEvents + 5 {
		appendTestTaskEvent(t, eventStore, "long-source-task", events.ExecutionEventTypeModelMessage)
	}
	source := testTask("default", "long-source-task")
	source.Spec.Type = corev1alpha1.TaskTypeAgent
	h, app := setupTaskEventHandlers(t, eventStore, source)
	app.Post("/api/v1/tasks/:id/fork", h.ForkTask)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/long-source-task/fork?namespace=default", bytes.NewBufferString(`{"newTaskName":"long-fork"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var out ForkTaskResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.ForkContext.Truncated || len(out.ForkContext.Events) != forkcontext.DefaultMaxEvents {
		t.Fatalf("fork context truncated=%v len=%d, want truncated tail of %d", out.ForkContext.Truncated, len(out.ForkContext.Events), forkcontext.DefaultMaxEvents)
	}
	if got := out.ForkContext.Events[0].Seq; got != 6 {
		t.Fatalf("first retained seq=%d, want 6", got)
	}
}

func TestGetTaskTraceAPIPagesBeyondEventLimit(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	for range store.MaxExecutionEventLimit {
		appendToolEvent(t, eventStore, "long-trace-task", events.ExecutionEventTypeToolCallStarted, "call")
	}
	appendTestTaskEvent(t, eventStore, "long-trace-task", events.ExecutionEventTypeTaskSucceeded)
	h, app := setupTaskEventHandlers(t, eventStore, testTask("default", "long-trace-task"))
	app.Get("/api/v1/tasks/:id/trace", h.GetTaskTrace)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/long-trace-task/trace?namespace=default", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var trace tasktrace.TaskTrace
	if err := json.NewDecoder(resp.Body).Decode(&trace); err != nil {
		t.Fatal(err)
	}
	if trace.LatestSeq != store.MaxExecutionEventLimit+1 || trace.TerminalEvent == nil {
		t.Fatalf("trace latest=%d terminal=%#v", trace.LatestSeq, trace.TerminalEvent)
	}
}

func setupPostP0Handlers(t *testing.T, eventStore store.ExecutionEventStore, sessionStore store.SessionStore, objs ...runtime.Object) (*Handlers, *fiber.App) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	h := NewHandlers(HandlersConfig{Client: fakeClient, ExecutionEventStore: eventStore, SessionStore: sessionStore})
	app := fiber.New()
	return h, app
}

func appendSessionEvent(t *testing.T, eventStore store.ExecutionEventStore, taskName, typ string, createdAt time.Time) {
	t.Helper()
	if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: taskName, TaskName: taskName, SessionName: "session-1", Type: typ, Severity: events.ExecutionEventSeverityInfo, Summary: typ + " summary", CreatedAt: createdAt}); err != nil {
		t.Fatalf("AppendExecutionEvent: %v", err)
	}
}

func appendToolEvent(t *testing.T, eventStore store.ExecutionEventStore, taskName, typ, callID string) {
	t.Helper()
	if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: taskName, TaskName: taskName, Type: typ, Severity: events.ExecutionEventSeverityInfo, ToolName: "web_search", ToolCallID: callID}); err != nil {
		t.Fatalf("AppendExecutionEvent: %v", err)
	}
}
