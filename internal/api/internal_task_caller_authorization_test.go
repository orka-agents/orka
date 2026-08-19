package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func TestInternalTaskCallerRejectsRecreatedWorkloadIdentity(t *testing.T) {
	scheme := internalCallerAuthScheme(t)

	t.Run("pod UID", func(t *testing.T) {
		task := internalCallerAuthTask()
		job := internalCallerAuthJob(task, "job-a", "job-uid")
		pod := internalCallerAuthPod(task, "pod-a", "new-pod-uid", job)
		authorizer := internalCallerAuthorizer{k8sReader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, job, pod).Build()}
		_, err := authorizer.resolveTaskWorker(context.Background(), internalCallerAuthWorkerUser("pod-a", "old-pod-uid"), "default")
		requireInternalForbidden(t, err)
	})

	t.Run("job UID", func(t *testing.T) {
		task := internalCallerAuthTask()
		job := internalCallerAuthJob(task, "job-a", "new-job-uid")
		pod := internalCallerAuthPod(task, "pod-a", "pod-uid", job)
		pod.OwnerReferences[0].UID = types.UID("old-job-uid")
		authorizer := internalCallerAuthorizer{k8sReader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, job, pod).Build()}
		_, err := authorizer.resolveTaskWorker(context.Background(), internalCallerAuthWorkerUser("pod-a", "pod-uid"), "default")
		requireInternalForbidden(t, err)
	})

	t.Run("Task UID", func(t *testing.T) {
		task := internalCallerAuthTask()
		job := internalCallerAuthJob(task, "job-a", "job-uid")
		job.OwnerReferences[0].UID = types.UID("old-task-uid")
		pod := internalCallerAuthPod(task, "pod-a", "pod-uid", job)
		authorizer := internalCallerAuthorizer{k8sReader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, job, pod).Build()}
		_, err := authorizer.resolveTaskWorker(context.Background(), internalCallerAuthWorkerUser("pod-a", "pod-uid"), "default")
		requireInternalForbidden(t, err)
	})

	t.Run("Pod ServiceAccount mismatch", func(t *testing.T) {
		task := internalCallerAuthTask()
		job := internalCallerAuthJob(task, "job-a", "job-uid")
		pod := internalCallerAuthPod(task, "pod-a", "pod-uid", job)
		pod.Spec.ServiceAccountName = "other-worker"
		authorizer := internalCallerAuthorizer{k8sReader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, job, pod).Build()}
		_, err := authorizer.resolveTaskWorker(context.Background(), internalCallerAuthWorkerUser("pod-a", "pod-uid"), "default")
		requireInternalForbidden(t, err)
	})

	t.Run("non-controller Pod owner", func(t *testing.T) {
		task := internalCallerAuthTask()
		job := internalCallerAuthJob(task, "job-a", "job-uid")
		pod := internalCallerAuthPod(task, "pod-a", "pod-uid", job)
		pod.OwnerReferences[0].Controller = nil
		authorizer := internalCallerAuthorizer{k8sReader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, job, pod).Build()}
		_, err := authorizer.resolveTaskWorker(context.Background(), internalCallerAuthWorkerUser("pod-a", "pod-uid"), "default")
		requireInternalForbidden(t, err)
	})

	t.Run("non-controller Job owner", func(t *testing.T) {
		task := internalCallerAuthTask()
		job := internalCallerAuthJob(task, "job-a", "job-uid")
		job.OwnerReferences[0].Controller = nil
		pod := internalCallerAuthPod(task, "pod-a", "pod-uid", job)
		authorizer := internalCallerAuthorizer{k8sReader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, job, pod).Build()}
		_, err := authorizer.resolveTaskWorker(context.Background(), internalCallerAuthWorkerUser("pod-a", "pod-uid"), "default")
		requireInternalForbidden(t, err)
	})

	t.Run("inactive pod", func(t *testing.T) {
		task := internalCallerAuthTask()
		job := internalCallerAuthJob(task, "job-a", "job-uid")
		pod := internalCallerAuthPod(task, "pod-a", "pod-uid", job)
		pod.Status.Phase = corev1.PodFailed
		authorizer := internalCallerAuthorizer{k8sReader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, job, pod).Build()}
		_, err := authorizer.resolveTaskWorker(context.Background(), internalCallerAuthWorkerUser("pod-a", "pod-uid"), "default")
		requireInternalForbidden(t, err)
	})
}

func TestInternalTaskScopedHandlersRequireActiveOwningWorker(t *testing.T) { //nolint:gocyclo // One matrix proves the shared authorization boundary.
	scheme := internalCallerAuthScheme(t)
	coordinatorA := internalCallerAuthTaskObject("coordinator-a", "coordinator-a-uid", "", "", "")
	coordinatorB := internalCallerAuthTaskObject("coordinator-b", "coordinator-b-uid", "", "", "")
	taskA := internalCallerAuthTaskObject("task-a", "task-a-uid", "task-a-job", "coordinator-a", "session-a")
	peerA := internalCallerAuthTaskObject("peer-a", "peer-a-uid", "peer-a-job", "coordinator-a", "session-peer-a")
	taskB := internalCallerAuthTaskObject("task-b", "task-b-uid", "task-b-job", "coordinator-b", "session-b")
	terminal := internalCallerAuthTaskObject("terminal", "terminal-uid", "terminal-job", "coordinator-a", "terminal-session")
	terminal.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	outcomeRecorded := internalCallerAuthTaskObject("outcome", "outcome-uid", "outcome-job", "coordinator-a", "outcome-session")
	outcomeRecorded.Status.ExecutionOutcome = &corev1alpha1.TaskExecutionOutcome{
		Phase: corev1alpha1.TaskPhaseSucceeded, Attempt: 1, RecordedAt: metav1.Now(),
	}

	objects := []client.Object{coordinatorA, coordinatorB, taskA, peerA, taskB, terminal, outcomeRecorded}
	for _, task := range []*corev1alpha1.Task{taskA, peerA, taskB, terminal, outcomeRecorded} {
		job := internalCallerAuthJob(task, task.Status.JobName, task.Name+"-job-uid")
		pod := internalCallerAuthPod(task, task.Name+"-pod", task.Name+"-pod-uid", job)
		objects = append(objects, job, pod)
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(objects...).
		Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	dataStore := sqlite.NewStore(db, ":memory:")
	for _, name := range []string{"session-a", "session-peer-a", "session-b", "terminal-session", "outcome-session"} {
		require.NoError(t, dataStore.CreateSession(context.Background(), &store.SessionRecord{
			Namespace: "default", Name: name, SessionType: "task",
		}))
		require.NoError(t, dataStore.AppendMessages(context.Background(), "default", name, []store.SessionMessage{{
			Role: "assistant", Content: "history for " + name,
		}}))
	}
	require.NoError(t, dataStore.SendMessage(context.Background(), &store.Message{
		Namespace: "default", FromTask: "peer-a", ToTask: "task-a", ParentTask: "coordinator-a", Content: "peer update",
	}))
	h := NewInternalHandlers(dataStore, dataStore, dataStore, dataStore, dataStore, InternalHandlersConfig{
		Client: k8sClient, APIReader: k8sClient, ExecutionEventStore: store.NewFakeExecutionEventStore(),
	})

	active := newTaskScopedInternalApp(h, internalCallerAuthWorkerUser("task-a-pod", "task-a-pod-uid"))
	for _, request := range taskScopedInternalRequests("task-a", "session-a", "coordinator-a", "peer-a") {
		resp := doTaskScopedInternalRequest(t, active, request)
		require.Equal(t, request.successStatus, resp.StatusCode, "%s %s", request.method, request.path)
	}

	peerTranscript := doTaskScopedInternalRequest(t, active, taskScopedRequest{
		method: http.MethodGet,
		path:   "/internal/v1/sessions/default/session-peer-a/transcript",
	})
	require.Equal(t, http.StatusForbidden, peerTranscript.StatusCode)
	peerSearch := doTaskScopedInternalRequest(t, active, taskScopedRequest{
		method:        http.MethodGet,
		path:          "/internal/v1/sessions/default/search?query=history&sessionName=session-peer-a",
		successStatus: http.StatusOK,
	})
	require.Equal(t, http.StatusOK, peerSearch.StatusCode)

	crossTask := newTaskScopedInternalApp(h, internalCallerAuthWorkerUser("task-b-pod", "task-b-pod-uid"))
	assertTaskScopedRequestsDenied(t, crossTask, "task-a", "session-a", "peer-a", http.StatusForbidden)

	terminalWorker := newTaskScopedInternalApp(h, internalCallerAuthWorkerUser("terminal-pod", "terminal-pod-uid"))
	assertTaskScopedRequestsDenied(t, terminalWorker, "terminal", "terminal-session", "*", http.StatusConflict)

	outcomeWorker := newTaskScopedInternalApp(h, internalCallerAuthWorkerUser("outcome-pod", "outcome-pod-uid"))
	assertTaskScopedRequestsDenied(t, outcomeWorker, "outcome", "outcome-session", "*", http.StatusConflict)

	for name, user := range map[string]*UserInfo{
		"controller token": {Username: "controller", Namespace: "default", AuthType: AuthTypeOIDC},
		"harness token": {
			Username: "system:serviceaccount:orka-system:agent-harness-wrapper", Namespace: "orka-system", AuthType: AuthTypeTokenReview,
		},
	} {
		t.Run(name, func(t *testing.T) {
			app := newTaskScopedInternalApp(h, user)
			assertTaskScopedRequestsDenied(t, app, "task-a", "session-a", "peer-a", http.StatusForbidden)
		})
	}
}

func TestInternalCallerAuthorizationUsesAPIReader(t *testing.T) {
	scheme := internalCallerAuthScheme(t)
	currentTask := internalCallerAuthTaskObject("task-a", "current-task-uid", "current-job", "", "")
	currentJob := internalCallerAuthJob(currentTask, "current-job", "current-job-uid")
	currentPod := internalCallerAuthPod(currentTask, "current-pod", "current-pod-uid", currentJob)
	staleTask := currentTask.DeepCopy()
	staleTask.UID = types.UID("stale-task-uid")
	staleTask.Status.JobName = "stale-job"
	staleJob := internalCallerAuthJob(staleTask, "stale-job", "stale-job-uid")
	stalePod := internalCallerAuthPod(staleTask, "stale-pod", "stale-pod-uid", staleJob)
	cachedClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(staleTask, staleJob, stalePod).Build()
	apiReader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(currentTask, currentJob, currentPod).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	dataStore := sqlite.NewStore(db, ":memory:")
	h := NewInternalHandlers(dataStore, dataStore, dataStore, dataStore, dataStore, InternalHandlersConfig{
		Client: cachedClient, APIReader: apiReader,
	})
	app := newTaskScopedInternalApp(h, internalCallerAuthWorkerUser("current-pod", "current-pod-uid"))
	resp := doTaskScopedInternalRequest(t, app, taskScopedRequest{
		method: http.MethodPost, path: "/internal/v1/results/default/task-a", body: []byte("result"), successStatus: http.StatusNoContent,
	})
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}

type taskScopedRequest struct {
	method        string
	path          string
	body          any
	successStatus int
}

func taskScopedInternalRequests(taskName, sessionName, parentTask, peerTask string) []taskScopedRequest {
	return []taskScopedRequest{
		{method: http.MethodPost, path: "/internal/v1/results/default/" + taskName, body: []byte("result"), successStatus: http.StatusNoContent},
		{method: http.MethodPost, path: "/internal/v1/artifacts/default/" + taskName + "/output.txt", body: []byte("artifact"), successStatus: http.StatusCreated},
		{method: http.MethodPost, path: "/internal/v1/plans/default/" + taskName, body: map[string]any{"summary": "plan"}, successStatus: http.StatusNoContent},
		{method: http.MethodGet, path: "/internal/v1/plans/default/" + taskName, successStatus: http.StatusOK},
		{method: http.MethodGet, path: "/internal/v1/sessions/default/" + sessionName + "/transcript", successStatus: http.StatusOK},
		{method: http.MethodGet, path: "/internal/v1/sessions/default/search?query=history&sessionName=" + sessionName, successStatus: http.StatusOK},
		{method: http.MethodPost, path: "/internal/v1/messages/default", body: map[string]string{
			"fromTask": taskName, "toTask": peerTask, "parentTask": parentTask, "content": "update",
		}, successStatus: http.StatusNoContent},
		{method: http.MethodGet, path: "/internal/v1/messages/default/" + taskName + "?parentTask=" + parentTask + "&markRead=false", successStatus: http.StatusOK},
		{method: http.MethodPost, path: "/internal/v1/tasks/default/" + taskName + "/execution-workspace/status", body: map[string]any{
			"provider": "substrate", "phase": "Ready", "reason": "WorkspaceReady",
		}, successStatus: http.StatusNoContent},
		{method: http.MethodPost, path: "/internal/v1/events/default/task/" + taskName, body: map[string]any{
			"type": events.ExecutionEventTypeWorkerStarted,
		}, successStatus: http.StatusCreated},
	}
}

func assertTaskScopedRequestsDenied(
	t *testing.T,
	app *fiber.App,
	taskName, sessionName, peerTask string,
	eventStatus int,
) {
	t.Helper()
	for _, request := range taskScopedInternalRequests(taskName, sessionName, "coordinator-a", peerTask) {
		want := http.StatusForbidden
		if request.path == "/internal/v1/events/default/task/"+taskName {
			want = eventStatus
		}
		resp := doTaskScopedInternalRequest(t, app, request)
		require.Equal(t, want, resp.StatusCode, "%s %s", request.method, request.path)
	}
}

func newTaskScopedInternalApp(h *InternalHandlers, user *UserInfo) *fiber.App {
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, user)
		return c.Next()
	})
	app.Post("/internal/v1/results/:namespace/:taskName", h.SubmitResult)
	app.Post("/internal/v1/artifacts/:namespace/:taskName/:filename", h.UploadArtifact)
	app.Post("/internal/v1/plans/:namespace/:taskName", h.SubmitPlan)
	app.Get("/internal/v1/plans/:namespace/:taskName", h.GetPlan)
	app.Get("/internal/v1/sessions/:namespace/search", h.SearchTranscript)
	app.Get("/internal/v1/sessions/:namespace/:name/transcript", h.GetSessionTranscript)
	app.Post("/internal/v1/messages/:namespace", h.SendMessage)
	app.Get("/internal/v1/messages/:namespace/:taskName", h.GetMessages)
	app.Post("/internal/v1/tasks/:namespace/:taskName/execution-workspace/status", h.UpdateExecutionWorkspaceStatus)
	app.Post("/internal/v1/events/:namespace/:streamType/:streamID", h.SubmitExecutionEvent)
	return app
}

func doTaskScopedInternalRequest(t *testing.T, app *fiber.App, request taskScopedRequest) *http.Response {
	t.Helper()
	var body []byte
	switch value := request.body.(type) {
	case nil:
	case []byte:
		body = value
	default:
		var err error
		body, err = json.Marshal(value)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(request.method, request.path, bytes.NewReader(body))
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 30 * time.Second, FailOnTimeout: true})
	require.NoError(t, err)
	return resp
}

func requireInternalForbidden(t *testing.T, err error) {
	t.Helper()
	fiberErr := &fiber.Error{}
	require.ErrorAs(t, err, &fiberErr)
	require.Equal(t, http.StatusForbidden, fiberErr.Code)
}

func TestActiveInternalWorkerTaskPhases(t *testing.T) {
	tests := []struct {
		name    string
		phase   corev1alpha1.TaskPhase
		outcome *corev1alpha1.TaskExecutionOutcome
		want    bool
	}{
		{name: "pending", phase: corev1alpha1.TaskPhasePending, want: true},
		{name: "running", phase: corev1alpha1.TaskPhaseRunning, want: true},
		{name: "pre-outcome finalizing", phase: corev1alpha1.TaskPhaseFinalizing, want: true},
		{name: "finalizing with outcome", phase: corev1alpha1.TaskPhaseFinalizing, outcome: &corev1alpha1.TaskExecutionOutcome{Phase: corev1alpha1.TaskPhaseSucceeded}, want: false},
		{name: "succeeded", phase: corev1alpha1.TaskPhaseSucceeded, want: false},
		{name: "cancelled", phase: corev1alpha1.TaskPhaseCancelled, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := internalCallerAuthTask()
			task.Status.Phase = tt.phase
			task.Status.ExecutionOutcome = tt.outcome
			require.Equal(t, tt.want, activeInternalWorkerTask(task))
		})
	}
}
