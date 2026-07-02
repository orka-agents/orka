package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	authenticationv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/harness"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/store"
)

const brokerHarnessEndpoint = "http://harness.default.svc:8080"

func TestBrokeredToolAPIWebFetchUnsupportedWithoutEgressProxy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello from broker api"))
	}))
	defer server.Close()
	eventStore := store.NewFakeExecutionEventStore()
	app := setupBrokerHarnessToolApp(t, eventStore, "web_fetch")
	req := brokeredAPIToolRequest("web_fetch", "call-a", json.RawMessage(`{"url":"`+server.URL+`"}`))
	resp := doJSONRequest(t, app, "/internal/v1/harness/tools/default/task-a", req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var result harness.ToolCallResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Error == nil || result.Error.Code != "tool_unsupported" {
		t.Fatalf("result=%#v, want tool_unsupported", result)
	}
}

func TestBrokeredToolAPIListToolsBuiltInSucceeds(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	customTool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "brokered-tool"}}
	app := setupBrokerHarnessToolApp(t, eventStore, "list_tools", customTool)
	req := brokeredAPIToolRequest("list_tools", "call-list-tools", json.RawMessage(`{}`))
	resp := doJSONRequest(t, app, "/internal/v1/harness/tools/default/task-a", req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var result harness.ToolCallResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Error != nil || !result.Approved || !strings.Contains(string(result.Output), "brokered-tool") {
		t.Fatalf("result=%#v output=%s, want approved list_tools output", result, result.Output)
	}
	listed, err := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace: "default", StreamID: "task-a", Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents: %v", err)
	}
	if !hasBrokeredAPIEvent(listed, "ToolCallStarted") || !hasBrokeredAPIEvent(listed, "ToolCallCompleted") {
		t.Fatalf("events=%#v, want brokered tool start/completion", listed)
	}
}

func TestBrokeredToolAPIRejectsServiceHarnessProviderPodUntilTrusted(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	servicePodUID := types.UID("service-pod-uid")
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "harness"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "harness"}},
	}
	servicePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "harness-pod", UID: servicePodUID,
			Labels: map[string]string{"app": "harness"},
		},
	}
	app := setupBrokerHarnessToolApp(
		t,
		eventStore,
		"list_tools",
		service,
		servicePod,
		func(task *corev1alpha1.Task) {
			task.Status.JobName = ""
			task.Annotations[labels.AnnotationHarnessEndpoint] = brokerHarnessEndpoint
			task.Annotations[labels.AnnotationHarnessRuntimeSession] = "runtime-a"
			task.Annotations[labels.AnnotationHarnessTurn] = "turn-a"
		},
		withBrokerUser(&UserInfo{
			Username:  "system:serviceaccount:default:harness",
			Namespace: "default",
			AuthType:  AuthTypeTokenReview,
			Extra: map[string]authenticationv1.ExtraValue{
				"authentication.kubernetes.io/pod-name": {"harness-pod"},
				"authentication.kubernetes.io/pod-uid":  {string(servicePodUID)},
			},
		}),
	)
	req := brokeredAPIToolRequest("list_tools", "call-service-harness", json.RawMessage(`{}`))
	resp := doJSONRequest(t, app, "/internal/v1/harness/tools/default/task-a", req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want service harness broker authorization to fail closed", resp.StatusCode)
	}
}

func TestBrokeredToolAPIListToolsRejectsCrossNamespaceInput(t *testing.T) {
	app := setupBrokerHarnessToolApp(t, store.NewFakeExecutionEventStore(), "list_tools")
	req := brokeredAPIToolRequest("list_tools", "call-list-tools-denied", json.RawMessage(`{"namespace":"other"}`))
	resp := doJSONRequest(t, app, "/internal/v1/harness/tools/default/task-a", req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var result harness.ToolCallResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Error != nil || !result.Approved || !strings.Contains(string(result.Output), "permission_denied") {
		t.Fatalf("result=%#v output=%s, want tool-level permission denial", result, result.Output)
	}
}

func TestBrokeredToolAPIRejectsServiceHarnessWithoutWorkerOwnership(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	customTool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "brokered-tool"}}
	req := brokeredAPIToolRequest("list_tools", "call-service-harness", json.RawMessage(`{}`))
	app := setupBrokerHarnessToolApp(t, eventStore, "list_tools", customTool, func(task *corev1alpha1.Task) {
		task.Status.JobName = ""
		task.Annotations[labels.AnnotationHarnessEndpoint] = brokerHarnessEndpoint
		task.Annotations[labels.AnnotationHarnessRuntimeSession] = string(req.RuntimeSessionID)
		task.Annotations[labels.AnnotationHarnessTurn] = string(req.TurnID)
		task.Annotations[labels.AnnotationHarnessTurnStartedAt] = "2026-06-20T00:00:00Z"
	})
	resp := doJSONRequest(t, app, "/internal/v1/harness/tools/default/task-a", req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want service harness broker request rejected without worker ownership", resp.StatusCode)
	}
}

func TestBrokeredToolAPIRequiresTaskWorkerOwnership(t *testing.T) {
	app := setupBrokerHarnessToolApp(t, store.NewFakeExecutionEventStore(), "list_tools", withBrokerUser(&UserInfo{Username: "system:serviceaccount:default:other", Namespace: "default"}))
	req := brokeredAPIToolRequest("list_tools", "call-not-owner", json.RawMessage(`{}`))
	resp := doJSONRequest(t, app, "/internal/v1/harness/tools/default/task-a", req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", resp.StatusCode)
	}
}

func TestBrokeredToolAPIAppendsEventsToTaskSession(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	customTool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "brokered-tool"}}
	app := setupBrokerHarnessToolApp(t, eventStore, "list_tools", customTool)
	req := brokeredAPIToolRequest("list_tools", "call-list-tools-session", json.RawMessage(`{}`))
	resp := doJSONRequest(t, app, "/internal/v1/harness/tools/default/task-a", req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	listed, latest, err := eventStore.ListSessionExecutionEvents(context.Background(), store.SessionExecutionEventFilter{
		Namespace: "default", SessionName: "session-a", Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListSessionExecutionEvents: %v", err)
	}
	if latest != 2 || len(listed) != 2 || listed[0].Type != "ToolCallStarted" || listed[1].Type != "ToolCallCompleted" {
		t.Fatalf("latest=%d listed=%#v, want brokered tool events in task session", latest, listed)
	}
}

func TestBrokeredToolAPIDisabledToolRejected(t *testing.T) {
	app := setupBrokerHarnessToolApp(t, store.NewFakeExecutionEventStore(), "web_fetch")
	req := brokeredAPIToolRequest("file_read", "call-disabled", json.RawMessage(`{}`))
	resp := doJSONRequest(t, app, "/internal/v1/harness/tools/default/task-a", req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var result harness.ToolCallResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Error == nil || result.Error.Code != "tool_disabled" {
		t.Fatalf("result=%#v, want tool_disabled", result)
	}
}

func TestBrokeredToolAPIApprovalPendingReservesIdempotencyKey(t *testing.T) {
	app := setupBrokerHarnessToolApp(t, store.NewFakeExecutionEventStore(), "web_fetch")
	req := brokeredAPIToolRequest("web_fetch", "call-approval", json.RawMessage(`{"path":"a.txt"}`))
	req.RequiresApproval = true
	resp := doJSONRequest(t, app, "/internal/v1/harness/tools/default/task-a", req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	changed := req
	changed.Input = json.RawMessage(`{"path":"b.txt"}`)
	resp = doJSONRequest(t, app, "/internal/v1/harness/tools/default/task-a", changed)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("changed status=%d", resp.StatusCode)
	}
	var result harness.ToolCallResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Error == nil || result.Error.Code != "idempotency_conflict" {
		t.Fatalf("result=%#v, want idempotency conflict", result)
	}
}

func TestBrokeredToolHTTPKeyScopesTaskUID(t *testing.T) {
	req := brokeredAPIToolRequest("list_tools", "call-key", json.RawMessage(`{}`))
	req.IdempotencyKey = "same-key"
	first := brokeredToolHTTPKey("default", "task-a", "uid-a", req)
	second := brokeredToolHTTPKey("default", "task-a", "uid-b", req)
	if first == "" || second == "" || first == second {
		t.Fatalf("keys = %q/%q, want non-empty generation-scoped keys", first, second)
	}
}

func TestBrokeredToolAPIRejectsServiceHarnessWithoutActiveTurn(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	servicePodUID := types.UID("service-pod-uid")
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "harness"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "harness"}},
	}
	servicePod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "harness-pod", UID: servicePodUID, Labels: map[string]string{"app": "harness"}}}
	app := setupBrokerHarnessToolApp(
		t, eventStore, "list_tools", service, servicePod,
		func(task *corev1alpha1.Task) {
			task.Status.JobName = ""
			task.Annotations[labels.AnnotationHarnessEndpoint] = brokerHarnessEndpoint
		},
		withBrokerUser(&UserInfo{
			Username: "system:serviceaccount:default:harness", Namespace: "default", AuthType: AuthTypeTokenReview,
			Extra: map[string]authenticationv1.ExtraValue{
				"authentication.kubernetes.io/pod-name": {"harness-pod"},
				"authentication.kubernetes.io/pod-uid":  {string(servicePodUID)},
			},
		}),
	)
	req := brokeredAPIToolRequest("list_tools", "call-empty-turn", json.RawMessage(`{}`))
	req.RuntimeSessionID = ""
	req.TurnID = ""
	req.IdempotencyKey = "empty"
	resp := doJSONRequest(t, app, "/internal/v1/harness/tools/default/task-a", req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 without active harness turn", resp.StatusCode)
	}
}

func TestBrokeredToolAPIRequiresServiceSelectorLabelPresence(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	servicePodUID := types.UID("service-pod-uid")
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "harness"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"empty-label": ""}},
	}
	servicePod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "harness-pod", UID: servicePodUID, Labels: map[string]string{"app": "harness"}}}
	app := setupBrokerHarnessToolApp(
		t, eventStore, "list_tools", service, servicePod,
		func(task *corev1alpha1.Task) {
			task.Status.JobName = ""
			task.Annotations[labels.AnnotationHarnessEndpoint] = brokerHarnessEndpoint
			task.Annotations[labels.AnnotationHarnessRuntimeSession] = "runtime-a"
			task.Annotations[labels.AnnotationHarnessTurn] = "turn-a"
		},
		withBrokerUser(&UserInfo{
			Username: "system:serviceaccount:default:harness", Namespace: "default", AuthType: AuthTypeTokenReview,
			Extra: map[string]authenticationv1.ExtraValue{
				"authentication.kubernetes.io/pod-name": {"harness-pod"},
				"authentication.kubernetes.io/pod-uid":  {string(servicePodUID)},
			},
		}),
	)
	req := brokeredAPIToolRequest("list_tools", "call-missing-empty-label", json.RawMessage(`{}`))
	resp := doJSONRequest(t, app, "/internal/v1/harness/tools/default/task-a", req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 when selector label is absent", resp.StatusCode)
	}
}

func TestBrokeredToolAPICacheIsBounded(t *testing.T) {
	h := NewInternalHandlers(nil, nil, nil, nil, nil, InternalHandlersConfig{})
	for i := range maxBrokeredToolHTTPCacheEntries + 25 {
		req := brokeredAPIToolRequest("list_tools", fmt.Sprintf("call-%d", i), json.RawMessage(`{}`))
		data, _ := json.Marshal(harness.ToolCallResult{Version: harness.ProtocolVersion, RuntimeSessionID: req.RuntimeSessionID, TurnID: req.TurnID, ToolCallID: req.ToolCallID, IdempotencyKey: req.IdempotencyKey, Approved: true})
		h.rememberBrokeredToolResult("default", "task-a", "uid-a", req, data, true)
	}
	if len(h.brokeredToolResults) > maxBrokeredToolHTTPCacheEntries {
		t.Fatalf("cached results = %d, want <= %d", len(h.brokeredToolResults), maxBrokeredToolHTTPCacheEntries)
	}
	for i := range maxBrokeredToolHTTPCacheEntries + 25 {
		req := brokeredAPIToolRequest("list_tools", fmt.Sprintf("pending-%d", i), json.RawMessage(`{}`))
		_ = h.reserveBrokeredToolRequest("default", "task-a", "uid-a", req)
	}
	if len(h.brokeredToolPending) > maxBrokeredToolHTTPCacheEntries {
		t.Fatalf("pending results = %d, want <= %d", len(h.brokeredToolPending), maxBrokeredToolHTTPCacheEntries)
	}
}

func TestBrokeredToolAPIReservesApprovalGatedRequestDuringExecution(t *testing.T) {
	h := NewInternalHandlers(nil, nil, nil, nil, nil, InternalHandlersConfig{})
	req := brokeredAPIToolRequest("list_tools", "call-approved-retry", json.RawMessage(`{}`))
	req.RequiresApproval = true
	if pending := h.reserveBrokeredToolRequest("default", "task-a", "uid-a", req); pending != nil {
		t.Fatalf("first reservation returned pending result: %s", pending)
	}
	pending := h.reserveBrokeredToolRequest("default", "task-a", "uid-a", req)
	if pending == nil {
		t.Fatal("second reservation returned nil, want in-progress result")
	}
	var result harness.ToolCallResult
	if err := json.Unmarshal(pending, &result); err != nil {
		t.Fatalf("decode pending: %v", err)
	}
	if result.Error == nil || result.Error.Code != "idempotency_in_progress" {
		t.Fatalf("pending result=%#v, want idempotency_in_progress", result)
	}
}

func TestShouldCacheBrokeredToolResultSkipsRetryableFailures(t *testing.T) {
	for _, code := range []string{
		"approval_required",
		"approval_check_failed",
		"approval_request_failed",
		"event_record_failed",
		"idempotency_check_failed",
	} {
		t.Run(code, func(t *testing.T) {
			if shouldCacheBrokeredToolResult(harness.ToolCallResult{Error: &harness.ErrorInfo{Code: code}}) {
				t.Fatalf("shouldCacheBrokeredToolResult(%s) = true, want false", code)
			}
		})
	}
	if !shouldCacheBrokeredToolResult(harness.ToolCallResult{Error: &harness.ErrorInfo{Code: "tool_disabled"}}) {
		t.Fatal("terminal broker error was not cached")
	}
}

func setupBrokerHarnessToolApp(t *testing.T, eventStore store.ExecutionEventStore, allowedTools string, optsOrObjects ...any) *fiber.App {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	taskUID := types.UID("task-a-uid")
	jobUID := types.UID("job-a-uid")
	podUID := types.UID("pod-a-uid")
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "task-a",
			UID:       taskUID,
			Annotations: map[string]string{
				labels.AnnotationHarnessBrokeredTools:  allowedTools,
				labels.AnnotationHarnessRuntimeSession: "runtime-a",
				labels.AnnotationHarnessTurn:           "turn-a",
				labels.AnnotationHarnessTurnStartedAt:  metav1.Now().Format(time.RFC3339Nano),
			},
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{Name: "session-a"},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseRunning,
			JobName: "job-a",
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "job-a", UID: jobUID, OwnerReferences: []metav1.OwnerReference{{Kind: "Task", Name: "task-a", UID: taskUID}}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "pod-a", UID: podUID,
			Labels:          map[string]string{labels.LabelTask: labels.SelectorValue("task-a")},
			OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: "job-a", UID: jobUID}},
		},
	}
	objects := []runtime.Object{task, job, pod}
	userInfo := brokerWorkerUser(podUID)
	for _, item := range optsOrObjects {
		switch value := item.(type) {
		case runtime.Object:
			objects = append(objects, value)
		case func(*UserInfo):
			value(userInfo)
		case func(*corev1alpha1.Task):
			value(task)
		}
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()
	h := NewInternalHandlers(nil, nil, nil, nil, nil, InternalHandlersConfig{
		Client:                      k8sClient,
		ExecutionEventStore:         eventStore,
		AllowInsecureBrokerLoopback: true,
	})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, userInfo)
		return c.Next()
	})
	app.Post("/internal/v1/harness/tools/:namespace/:taskName", h.BrokerHarnessTool)
	return app
}

func brokerWorkerUser(podUID types.UID) *UserInfo {
	return &UserInfo{
		Username:  "system:serviceaccount:default:worker",
		Namespace: "default",
		AuthType:  AuthTypeTokenReview,
		Extra: map[string]authenticationv1.ExtraValue{
			"authentication.kubernetes.io/pod-name": {"pod-a"},
			"authentication.kubernetes.io/pod-uid":  {string(podUID)},
		},
	}
}

func withBrokerUser(user *UserInfo) func(*UserInfo) {
	return func(target *UserInfo) { *target = *user }
}

func brokeredAPIToolRequest(toolName, callID string, input json.RawMessage) harness.ToolCallRequest {
	runtimeID := harness.RuntimeSessionID("runtime-a")
	turnID := harness.HarnessTurnID("turn-a")
	return harness.ToolCallRequest{
		Version:          harness.ProtocolVersion,
		RuntimeSessionID: runtimeID,
		TurnID:           turnID,
		ToolCallID:       callID,
		ToolName:         toolName,
		IdempotencyKey:   harness.ToolRequestIdempotencyKey(runtimeID, turnID, callID),
		Input:            input,
	}
}

func hasBrokeredAPIEvent(listed []store.ExecutionEvent, typ string) bool {
	for _, event := range listed {
		if event.Type == typ {
			return true
		}
	}
	return false
}
