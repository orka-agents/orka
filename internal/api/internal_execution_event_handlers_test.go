package api

import (
	"bytes"
	"context"
	"encoding/json"
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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/store"
)

func TestInternalSubmitExecutionEvent(t *testing.T) {
	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	eventStore := store.NewFakeExecutionEventStoreWithClock(func() time.Time { return now })
	app := setupOwnedInternalExecutionEventApp(t, eventStore, "task-1", "worker-pod", "worker-pod-uid")

	redactionValue := strings.Join([]string{"bearer", "value", "for", "redaction"}, "-")
	body := map[string]any{
		"id":          "client-id-is-ignored",
		"seq":         999,
		"createdAt":   "2000-01-01T00:00:00Z",
		"type":        events.ExecutionEventTypeModelMessage,
		"severity":    "ERROR",
		"summary":     "Authorization: Bearer " + redactionValue,
		"content":     map[string]any{"token": redactionValue, "safe": "ok"},
		"contentText": strings.Repeat("x", events.MaxExecutionEventContentTextChars+5),
	}
	resp := doJSONRequest(t, app, "/internal/v1/events/default/task/task-1", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var submitted SubmitExecutionEventResponse
	if err := json.NewDecoder(resp.Body).Decode(&submitted); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if submitted.ID == "" || submitted.ID == "client-id-is-ignored" || submitted.Seq != 1 || !submitted.CreatedAt.Equal(now) {
		t.Fatalf("response = %#v, want assigned id seq createdAt", submitted)
	}

	stored, err := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "task-1",
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored len = %d, want 1", len(stored))
	}
	event := stored[0]
	if event.ID == "client-id-is-ignored" || event.Seq != 1 || !event.CreatedAt.Equal(now) {
		t.Fatalf("stored assignment = %#v", event)
	}
	if event.TaskName != "task-1" || event.Type != events.ExecutionEventTypeModelMessage || event.Severity != events.ExecutionEventSeverityError {
		t.Fatalf("stored event fields = %#v", event)
	}
	if strings.Contains(event.Summary, redactionValue) || strings.Contains(event.ContentText, redactionValue) || len([]rune(event.ContentText)) != events.MaxExecutionEventContentTextChars {
		t.Fatalf("stored text not sanitized/truncated: summary=%q contentTextLen=%d", event.Summary, len([]rune(event.ContentText)))
	}
	var content map[string]string
	if err := json.Unmarshal(event.Content, &content); err != nil {
		t.Fatalf("unmarshal stored content: %v", err)
	}
	if content["token"] != events.ExecutionEventRedactedValue || content["safe"] != "ok" {
		t.Fatalf("stored content = %#v, want redacted token", content)
	}
	if event.Truncation == nil || !event.Truncation.ContentTextTruncated {
		t.Fatalf("truncation = %#v, want content text truncation", event.Truncation)
	}
}

func TestInternalSubmitExecutionEventTaskOwnership(t *testing.T) {
	task, job, pod := testInternalExecutionEventOwnedWorkerObjects("owned-task")
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "session-owned"}
	eventStore := store.NewFakeExecutionEventStore()
	app := setupInternalExecutionEventAppWithClient(
		eventStore,
		testInternalExecutionEventClient(t, task, job, pod),
		testInternalExecutionEventWorkerUser("owned-task-pod"),
	)

	resp := doJSONRequest(
		t,
		app,
		"/internal/v1/events/default/task/owned-task",
		map[string]any{"type": events.ExecutionEventTypeWorkerStarted},
	)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("owned worker status = %d, want 201", resp.StatusCode)
	}
	stored, err := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace:  "default",
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   "owned-task",
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored len = %d, want 1", len(stored))
	}
	if stored[0].SessionName != "session-owned" {
		t.Fatalf("stored sessionName = %q, want task session", stored[0].SessionName)
	}
	mismatchResp := doJSONRequest(
		t,
		app,
		"/internal/v1/events/default/task/owned-task",
		map[string]any{"type": events.ExecutionEventTypeWorkerCompleted, "sessionName": "other-session"},
	)
	if mismatchResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("mismatched session status = %d, want 400", mismatchResp.StatusCode)
	}
}

func TestInternalSubmitExecutionEventRejectsWrongOrDeletingTask(t *testing.T) {
	task, job, pod := testInternalExecutionEventOwnedWorkerObjects("owned-task")
	otherTask, otherJob, _ := testInternalExecutionEventOwnedWorkerObjects("other-task")
	deletingTask, deletingJob, deletingPod := testInternalExecutionEventOwnedWorkerObjects("deleting-task")
	deletingTask.Finalizers = []string{labels.TaskFinalizer}
	deletingTask.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	terminalTask, terminalJob, terminalPod := testInternalExecutionEventOwnedWorkerObjects("terminal-task")
	terminalTask.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	eventStore := store.NewFakeExecutionEventStore()
	k8sClient := testInternalExecutionEventClient(
		t,
		task, job, pod,
		otherTask, otherJob,
		deletingTask, deletingJob, deletingPod,
		terminalTask, terminalJob, terminalPod,
	)
	app := setupInternalExecutionEventAppWithClient(
		eventStore,
		k8sClient,
		testInternalExecutionEventWorkerUser("owned-task-pod"),
	)

	tests := []struct {
		name string
		path string
		want int
	}{
		{
			name: "wrong task",
			path: "/internal/v1/events/default/task/other-task",
			want: http.StatusForbidden,
		},
		{
			name: "deleting task not owned",
			path: "/internal/v1/events/default/task/deleting-task",
			want: http.StatusForbidden,
		},
		{
			name: "missing task",
			path: "/internal/v1/events/default/task/missing-task",
			want: http.StatusForbidden,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := doJSONRequest(t, app, tt.path, map[string]any{"type": events.ExecutionEventTypeTaskSucceeded})
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}

	deletingApp := setupInternalExecutionEventAppWithClient(
		eventStore,
		k8sClient,
		testInternalExecutionEventWorkerUser("deleting-task-pod"),
	)
	deletingResp := doJSONRequest(
		t,
		deletingApp,
		"/internal/v1/events/default/task/deleting-task",
		map[string]any{"type": events.ExecutionEventTypeTaskSucceeded},
	)
	if deletingResp.StatusCode != http.StatusGone {
		t.Fatalf("owned deleting task status = %d, want 410", deletingResp.StatusCode)
	}

	terminalApp := setupInternalExecutionEventAppWithClient(
		eventStore,
		k8sClient,
		testInternalExecutionEventWorkerUser("terminal-task-pod"),
	)
	terminalResp := doJSONRequest(
		t,
		terminalApp,
		"/internal/v1/events/default/task/terminal-task",
		map[string]any{"type": events.ExecutionEventTypeWorkerCompleted},
	)
	if terminalResp.StatusCode != http.StatusConflict {
		t.Fatalf("terminal task status = %d, want 409", terminalResp.StatusCode)
	}
}

func TestInternalSubmitExecutionEventValidationAndAuth(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	authenticatedApp := setupInternalExecutionEventApp(eventStore, &UserInfo{Username: "system:serviceaccount:default:worker", Namespace: "default"})

	tests := []struct {
		name string
		path string
		body map[string]any
		want int
	}{
		{
			name: "invalid stream type",
			path: "/internal/v1/events/default/session/session-1",
			body: map[string]any{"type": events.ExecutionEventTypeTaskStarted},
			want: http.StatusBadRequest,
		},
		{
			name: "missing event type",
			path: "/internal/v1/events/default/task/task-1",
			body: map[string]any{"summary": "missing type"},
			want: http.StatusBadRequest,
		},
		{
			name: "unknown event type",
			path: "/internal/v1/events/default/task/task-1",
			body: map[string]any{"type": "UnknownEvent"},
			want: http.StatusBadRequest,
		},
		{
			name: "approval terminal event denied",
			path: "/internal/v1/events/default/task/task-1",
			body: map[string]any{
				"type":    events.ExecutionEventTypeApprovalApproved,
				"content": map[string]any{"approvalID": "approval-1"},
			},
			want: http.StatusForbidden,
		},
		{
			name: "cross namespace service account denied",
			path: "/internal/v1/events/other/task/task-1",
			body: map[string]any{"type": events.ExecutionEventTypeTaskStarted},
			want: http.StatusForbidden,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := doJSONRequest(t, authenticatedApp, tt.path, tt.body)
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}

	t.Run("payload too large", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/events/default/task/task-1", strings.NewReader(strings.Repeat("x", maxSubmitExecutionEventRequestBytes+1)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := authenticatedApp.Test(req)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", resp.StatusCode)
		}
	})

	t.Run("unauthenticated", func(t *testing.T) {
		app := setupInternalExecutionEventApp(eventStore, nil)
		resp := doJSONRequest(t, app, "/internal/v1/events/default/task/task-1", map[string]any{"type": events.ExecutionEventTypeTaskStarted})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})
}

func TestInternalSubmitExecutionEventRequiresCurrentTaskWorker(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	app := setupOwnedInternalExecutionEventApp(t, eventStore, "task-1", "other-pod", "other-pod-uid")
	resp := doJSONRequest(t, app, "/internal/v1/events/default/task/task-1", map[string]any{"type": events.ExecutionEventTypeTaskStarted})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func setupInternalExecutionEventApp(eventStore store.ExecutionEventStore, userInfo *UserInfo) *fiber.App {
	h := NewInternalHandlers(nil, nil, nil, nil, nil, InternalHandlersConfig{ExecutionEventStore: eventStore})
	return setupInternalExecutionEventAppWithHandler(h, userInfo)
}

func setupOwnedInternalExecutionEventApp(t *testing.T, eventStore store.ExecutionEventStore, taskName, callerPodName, callerPodUID string) *fiber.App {
	t.Helper()
	taskUID := types.UID(taskName + "-uid")
	jobUID := types.UID(taskName + "-job-uid")
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme task: %v", err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme batch: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme core: %v", err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: taskName, Namespace: "default", UID: taskUID},
		Status:     corev1alpha1.TaskStatus{JobName: taskName + "-job"},
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:      taskName + "-job",
		Namespace: "default",
		UID:       jobUID,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: corev1alpha1.GroupVersion.String(),
			Kind:       "Task",
			Name:       taskName,
			UID:        taskUID,
		}},
	}}
	workerPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "worker-pod",
		Namespace: "default",
		UID:       types.UID("worker-pod-uid"),
		Labels:    map[string]string{labels.LabelTask: labels.SelectorValue(taskName)},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: batchv1.SchemeGroupVersion.String(),
			Kind:       "Job",
			Name:       taskName + "-job",
			UID:        jobUID,
		}},
	}}
	otherPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "other-pod",
		Namespace: "default",
		UID:       types.UID("other-pod-uid"),
		Labels:    map[string]string{labels.LabelTask: labels.SelectorValue("other-task")},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: batchv1.SchemeGroupVersion.String(),
			Kind:       "Job",
			Name:       taskName + "-job",
			UID:        jobUID,
		}},
	}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, job, workerPod, otherPod).Build()
	h := NewInternalHandlers(nil, nil, nil, nil, nil, InternalHandlersConfig{Client: k8sClient, ExecutionEventStore: eventStore})
	return setupInternalExecutionEventAppWithHandler(h, &UserInfo{
		Username:  "system:serviceaccount:default:worker",
		Namespace: "default",
		AuthType:  AuthTypeTokenReview,
		Extra: map[string]authenticationv1.ExtraValue{
			"authentication.kubernetes.io/pod-name": {callerPodName},
			"authentication.kubernetes.io/pod-uid":  {callerPodUID},
		},
	})
}

func setupInternalExecutionEventAppWithHandler(h *InternalHandlers, userInfo *UserInfo) *fiber.App {
	app := fiber.New()
	if userInfo != nil {
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, userInfo)
			return c.Next()
		})
	}
	app.Post("/internal/v1/events/:namespace/:streamType/:streamID", h.SubmitExecutionEvent)
	return app
}

func setupInternalExecutionEventAppWithClient(
	eventStore store.ExecutionEventStore,
	k8sClient client.Client,
	userInfo *UserInfo,
) *fiber.App {
	h := NewInternalHandlers(nil, nil, nil, nil, nil, InternalHandlersConfig{
		Client:              k8sClient,
		ExecutionEventStore: eventStore,
	})
	app := fiber.New()
	if userInfo != nil {
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, userInfo)
			return c.Next()
		})
	}
	app.Post("/internal/v1/events/:namespace/:streamType/:streamID", h.SubmitExecutionEvent)
	return app
}

func testInternalExecutionEventClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(clientObjectsToRuntimeObjects(objs)...).
		Build()
}

func clientObjectsToRuntimeObjects(objs []client.Object) []runtime.Object {
	runtimeObjects := make([]runtime.Object, 0, len(objs))
	for _, obj := range objs {
		runtimeObjects = append(runtimeObjects, obj)
	}
	return runtimeObjects
}

func testInternalExecutionEventOwnedWorkerObjects(taskName string) (*corev1alpha1.Task, *batchv1.Job, *corev1.Pod) {
	taskUID := types.UID(taskName + "-uid")
	jobUID := types.UID(taskName + "-job-uid")
	podUID := types.UID("pod-uid")
	jobName := taskName + "-job"
	podName := taskName + "-pod"
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: taskName, Namespace: "default", UID: taskUID},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status:     corev1alpha1.TaskStatus{JobName: jobName},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: "default",
			UID:       jobUID,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(),
				Kind:       "Task",
				Name:       taskName,
				UID:        taskUID,
			}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: "default",
			UID:       podUID,
			Labels: map[string]string{
				labels.LabelTask: labels.SelectorValue(taskName),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: batchv1.SchemeGroupVersion.String(),
				Kind:       "Job",
				Name:       jobName,
				UID:        jobUID,
			}},
		},
	}
	return task, job, pod
}

func testInternalExecutionEventWorkerUser(podName string) *UserInfo {
	return &UserInfo{
		Username:  "system:serviceaccount:default:worker",
		Namespace: "default",
		AuthType:  AuthTypeTokenReview,
		Extra: map[string]authenticationv1.ExtraValue{
			"authentication.kubernetes.io/pod-name": {podName},
			"authentication.kubernetes.io/pod-uid":  {"pod-uid"},
		},
	}
}

func doJSONRequest(t *testing.T, app *fiber.App, target string, body any) *http.Response {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return resp
}
