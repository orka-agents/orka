package api

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
)

func TestSubmitResultWaitsForJobIdentityPublication(t *testing.T) {
	task := internalCallerAuthTask()
	task.Status = corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending}
	job := internalCallerAuthJob(task, "job-a", "job-uid")
	pod := internalCallerAuthPod(task, "pod-a", "pod-uid", job)
	kube := fake.NewClientBuilder().WithScheme(internalCallerAuthScheme(t)).
		WithStatusSubresource(task).WithObjects(task, job, pod).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	data := sqlite.NewStore(db, ":memory:")
	h := NewInternalHandlers(data, data, data, data, data, InternalHandlersConfig{Client: kube, APIReader: kube})

	type publicationObservation struct {
		authorizationErr error
		resultErr        error
		publicationErr   error
	}
	first := make(chan publicationObservation, 1)
	var requests atomic.Int32
	app := fiber.New()
	app.Post("/internal/v1/results/:namespace/:taskName", func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, internalCallerAuthWorkerUser(pod.Name, string(pod.UID)))
		submitErr := h.SubmitResult(c)
		if requests.Add(1) == 1 {
			_, resultErr := data.GetResult(c.Context(), task.Namespace, task.Name)
			// Publish only after the first submission has passed through the real
			// handler, reproducing a worker that finishes before the status write.
			current := &corev1alpha1.Task{}
			publicationErr := kube.Get(c.Context(), client.ObjectKeyFromObject(task), current)
			if publicationErr == nil {
				current.Status.Phase = corev1alpha1.TaskPhaseRunning
				current.Status.JobName = job.Name
				current.Status.JobUID = string(job.UID)
				publicationErr = kube.Status().Update(c.Context(), current)
			}
			first <- publicationObservation{submitErr, resultErr, publicationErr}
			if publicationErr != nil {
				return fiber.NewError(http.StatusBadRequest, "test Job identity publication failed")
			}
		}
		return submitErr
	})
	server := httptest.NewServer(adaptor.FiberApp(app))
	t.Cleanup(server.Close)
	t.Setenv(workerenv.ResultStdout, "false")
	t.Setenv(workerenv.ResultEndpoint, server.URL+"/internal/v1/results/default/task-a")

	result := []byte("completed before Job identity publication")
	submitErr := common.SubmitResult(result)
	observation := <-first
	require.NoError(t, observation.publicationErr)
	require.ErrorIs(t, observation.resultErr, store.ErrNotFound, "result must not be stored before Job identity publication")
	fiberErr := &fiber.Error{}
	require.ErrorAs(t, observation.authorizationErr, &fiberErr)
	require.Equal(t, http.StatusServiceUnavailable, fiberErr.Code)
	require.NoError(t, submitErr)
	require.EqualValues(t, 2, requests.Load())
	saved, err := data.GetResult(t.Context(), task.Namespace, task.Name)
	require.NoError(t, err)
	require.Equal(t, result, saved)
}

func TestSubmitResultRejectsInvalidUnpublishedJob(t *testing.T) {
	for _, change := range []string{"terminal task", "recorded outcome", "mislabeled pod"} {
		t.Run(change, func(t *testing.T) {
			task := internalCallerAuthTask()
			task.Status = corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending}
			job := internalCallerAuthJob(task, "job-a", "job-uid")
			pod := internalCallerAuthPod(task, "pod-a", "pod-uid", job)
			switch change {
			case "terminal task":
				task.Status.Phase = corev1alpha1.TaskPhaseCancelled
			case "recorded outcome":
				task.Status.ExecutionOutcome = &corev1alpha1.TaskWorkloadExecutionOutcome{Phase: corev1alpha1.TaskPhaseSucceeded}
			case "mislabeled pod":
				pod.Labels[labels.LabelTask] = "another-task"
			}
			kube := fake.NewClientBuilder().WithScheme(internalCallerAuthScheme(t)).WithObjects(task, job, pod).Build()
			h := NewInternalHandlers(nil, nil, nil, nil, nil, InternalHandlersConfig{Client: kube, APIReader: kube})
			app := newTaskScopedInternalApp(h, internalCallerAuthWorkerUser(pod.Name, string(pod.UID)))
			response := doTaskScopedInternalRequest(t, app, taskScopedRequest{
				method: http.MethodPost, path: "/internal/v1/results/default/task-a", body: []byte("result"),
			})
			t.Cleanup(func() { _ = response.Body.Close() })
			require.Equal(t, http.StatusForbidden, response.StatusCode)
		})
	}
}
