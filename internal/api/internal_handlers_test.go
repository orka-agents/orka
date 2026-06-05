/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/store/sqlite"
)

func setupTestInternalHandlers() (*InternalHandlers, *fiber.App, *sqlite.Store) {
	db, _ := sqlite.NewDB(":memory:")
	ss := sqlite.NewStore(db, ":memory:")
	h := NewInternalHandlers(ss, ss, ss, ss, ss, InternalHandlersConfig{
		MemoryStore:         ss,
		MemoryProposalStore: ss,
	})
	app := fiber.New()

	// Inject a default UserInfo so verifyCallerNamespace passes
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{
			Username: "system:serviceaccount:default:worker",
		})
		return c.Next()
	})

	return h, app, ss
}

func TestSubmitPlan(t *testing.T) {
	h, app, _ := setupTestInternalHandlers()
	app.Post("/internal/v1/plans/:namespace/:taskName", h.SubmitPlan)

	t.Run("success", func(t *testing.T) {
		body := map[string]any{
			"summary":       "step 1 done",
			"progress_pct":  50,
			"goal_complete": false,
			"plan_document": "# Plan\n- step 1\n- step 2",
		}
		bodyBytes, _ := json.Marshal(body)

		req := httptest.NewRequest(http.MethodPost, "/internal/v1/plans/default/my-task", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
	})

	t.Run("missing taskName", func(t *testing.T) {
		body := map[string]any{"summary": "test"}
		bodyBytes, _ := json.Marshal(body)

		// Use a route that provides namespace but omits taskName
		app2 := fiber.New()
		app2.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{
				Username: "system:serviceaccount:default:worker",
			})
			return c.Next()
		})
		app2.Post("/internal/v1/plans/:namespace", h.SubmitPlan)

		req := httptest.NewRequest(http.MethodPost, "/internal/v1/plans/default", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app2.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
}

func TestGetPlan(t *testing.T) {
	h, app, ss := setupTestInternalHandlers()
	app.Get("/internal/v1/plans/:namespace/:taskName", h.GetPlan)

	// Pre-save a plan
	err := ss.SavePlan(context.Background(), "default", "my-task", &store.PlanState{
		TaskName:     "my-task",
		Namespace:    "default",
		Summary:      "halfway there",
		ProgressPct:  50,
		GoalComplete: false,
		PlanDocument: "# Plan",
	})
	require.NoError(t, err)

	t.Run("existing plan", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/plans/default/my-task", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var plan store.PlanState
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&plan))
		require.Equal(t, "halfway there", plan.Summary)
		require.Equal(t, 50, plan.ProgressPct)
		require.Equal(t, "# Plan", plan.PlanDocument)
	})

	t.Run("not found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/plans/default/nonexistent", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

func TestSendMessage(t *testing.T) {
	h, app, _ := setupTestInternalHandlers()
	app.Post("/internal/v1/messages/:namespace", h.SendMessage)

	t.Run("success", func(t *testing.T) {
		body := map[string]string{
			"fromTask":   "worker-a",
			"toTask":     "worker-b",
			"parentTask": "coordinator",
			"content":    "found a bug",
		}
		bodyBytes, _ := json.Marshal(body)

		req := httptest.NewRequest(http.MethodPost, "/internal/v1/messages/default", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
	})

	t.Run("missing required fields", func(t *testing.T) {
		body := map[string]string{
			"fromTask": "worker-a",
		}
		bodyBytes, _ := json.Marshal(body)

		req := httptest.NewRequest(http.MethodPost, "/internal/v1/messages/default", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("invalid body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/messages/default", bytes.NewReader([]byte("not json")))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
}

func TestGetMessages(t *testing.T) {
	h, app, ss := setupTestInternalHandlers()
	app.Get("/internal/v1/messages/:namespace/:taskName", h.GetMessages)

	// Pre-send a message
	err := ss.SendMessage(context.Background(), &store.Message{
		Namespace:  "default",
		FromTask:   "worker-a",
		ToTask:     "worker-b",
		ParentTask: "coordinator",
		Content:    "hello from a",
	})
	require.NoError(t, err)

	t.Run("has messages", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/messages/default/worker-b?parentTask=coordinator&markRead=false", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var messages []store.Message
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&messages))
		require.Len(t, messages, 1)
		require.Equal(t, "hello from a", messages[0].Content)
	})

	t.Run("no messages", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/messages/default/worker-c?parentTask=coordinator", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var messages []store.Message
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&messages))
		require.Empty(t, messages)
	})

	t.Run("missing parentTask", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/messages/default/worker-b", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("messaging not enabled", func(t *testing.T) {
		hNoMsg := NewInternalHandlers(ss, ss, ss, nil, nil)
		app2 := fiber.New()
		app2.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{Username: "system:serviceaccount:default:worker"})
			return c.Next()
		})
		app2.Get("/internal/v1/messages/:namespace/:taskName", hNoMsg.GetMessages)

		req := httptest.NewRequest(http.MethodGet, "/internal/v1/messages/default/worker-b?parentTask=coordinator", nil)
		resp, err := app2.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	})

	t.Run("missing taskName", func(t *testing.T) {
		app2 := fiber.New()
		app2.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{Username: "system:serviceaccount:default:worker"})
			return c.Next()
		})
		app2.Get("/internal/v1/messages/:namespace", h.GetMessages)

		req := httptest.NewRequest(http.MethodGet, "/internal/v1/messages/default?parentTask=coordinator", nil)
		resp, err := app2.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("markRead defaults to true", func(t *testing.T) {
		// Send a fresh message for this sub-test
		err := ss.SendMessage(context.Background(), &store.Message{
			Namespace: "default", FromTask: "sender", ToTask: "reader",
			ParentTask: "coord", Content: "read me",
		})
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/internal/v1/messages/default/reader?parentTask=coord", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var msgs []store.Message
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&msgs))
		require.Len(t, msgs, 1)

		// Second call should return empty because markRead defaulted to true
		req2 := httptest.NewRequest(http.MethodGet, "/internal/v1/messages/default/reader?parentTask=coord", nil)
		resp2, err := app.Test(req2)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp2.StatusCode)

		var msgs2 []store.Message
		require.NoError(t, json.NewDecoder(resp2.Body).Decode(&msgs2))
		require.Empty(t, msgs2)
	})
}

func TestSubmitResult(t *testing.T) {
	h, app, _ := setupTestInternalHandlers()
	app.Post("/internal/v1/results/:namespace/:taskName", h.SubmitResult)

	t.Run("success", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/results/default/my-task",
			bytes.NewReader([]byte(`{"output":"done"}`)))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
	})

	t.Run("missing namespace", func(t *testing.T) {
		app2 := fiber.New()
		app2.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{Username: "system:serviceaccount:default:worker"})
			return c.Next()
		})
		app2.Post("/internal/v1/results/:namespace", h.SubmitResult)

		req := httptest.NewRequest(http.MethodPost, "/internal/v1/results/default",
			bytes.NewReader([]byte(`{"output":"done"}`)))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app2.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("empty body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/results/default/my-task",
			bytes.NewReader([]byte{}))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("cross-namespace denied", func(t *testing.T) {
		app2 := fiber.New()
		app2.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{
				Username: "system:serviceaccount:other-ns:worker",
			})
			return c.Next()
		})
		app2.Post("/internal/v1/results/:namespace/:taskName", h.SubmitResult)

		req := httptest.NewRequest(http.MethodPost, "/internal/v1/results/default/my-task",
			bytes.NewReader([]byte(`{"output":"done"}`)))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app2.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}

func TestUpdateExecutionWorkspaceStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	taskUID := types.UID("task-uid")
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "my-task", Namespace: "default", UID: taskUID},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status:     corev1alpha1.TaskStatus{JobName: "my-task-job"},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-task-job",
			Namespace: "default",
			UID:       types.UID("job-uid"),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(),
				Kind:       "Task",
				Name:       "my-task",
				UID:        taskUID,
			}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-task-pod",
			Namespace: "default",
			UID:       types.UID("pod-uid"),
			Labels: map[string]string{
				labels.LabelTask: labels.SelectorValue("my-task"),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: batchv1.SchemeGroupVersion.String(),
				Kind:       "Job",
				Name:       "my-task-job",
				UID:        types.UID("job-uid"),
			}},
		},
	}
	oldJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-task-old-job",
			Namespace: "default",
			UID:       types.UID("old-job-uid"),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(),
				Kind:       "Task",
				Name:       "my-task",
				UID:        taskUID,
			}},
		},
	}
	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-task-old-pod",
			Namespace: "default",
			UID:       types.UID("old-pod-uid"),
			Labels: map[string]string{
				labels.LabelTask: labels.SelectorValue("my-task"),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: batchv1.SchemeGroupVersion.String(),
				Kind:       "Job",
				Name:       "my-task-old-job",
				UID:        types.UID("old-job-uid"),
			}},
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(task, job, pod, oldJob, oldPod).
		Build()
	h := NewInternalHandlers(nil, nil, nil, nil, nil, InternalHandlersConfig{Client: k8sClient})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{
			Username: "system:serviceaccount:default:worker",
			AuthType: AuthTypeTokenReview,
			Extra: map[string]authenticationv1.ExtraValue{
				"authentication.kubernetes.io/pod-name": {"my-task-pod"},
				"authentication.kubernetes.io/pod-uid":  {"pod-uid"},
			},
		})
		return c.Next()
	})
	app.Post("/internal/v1/tasks/:namespace/:taskName/execution-workspace/status", h.UpdateExecutionWorkspaceStatus)

	body := map[string]any{
		"provider":      "substrate",
		"phase":         "Ready",
		"reason":        "WorkspaceReady",
		"templateRef":   map[string]string{"name": "orka-codex", "namespace": "ate-demo"},
		"reusePolicy":   "session",
		"cleanupPolicy": "retain",
		"reused":        true,
		"placement": map[string]string{
			"workerNamespace": "ate-demo",
			"workerPool":      "codex-pool",
			"workerPodName":   "ateom-worker-1",
		},
		"density": map[string]any{
			"workerCount":         1,
			"actorCount":          3,
			"runningActorCount":   1,
			"suspendedActorCount": 2,
			"actorsPerWorker":     "3.00",
		},
		"resumeLatency": "750ms",
		"message":       "workspace ready",
	}
	bodyBytes, err := json.Marshal(body)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/internal/v1/tasks/default/my-task/execution-workspace/status", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	updated := &corev1alpha1.Task{}
	require.NoError(t, k8sClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "my-task"}, updated))
	require.NotNil(t, updated.Status.ExecutionWorkspace)
	require.Equal(t, corev1alpha1.WorkspaceProviderSubstrate, updated.Status.ExecutionWorkspace.Provider)
	require.Equal(t, corev1alpha1.ExecutionWorkspacePhaseReady, updated.Status.ExecutionWorkspace.Phase)
	require.True(t, updated.Status.ExecutionWorkspace.Reused)
	require.Equal(t, &corev1alpha1.ExecutionWorkspacePlacementStatus{
		WorkerNamespace: "ate-demo",
		WorkerPool:      "codex-pool",
		WorkerPodName:   "ateom-worker-1",
	}, updated.Status.ExecutionWorkspace.Placement)
	require.Equal(t, &corev1alpha1.ExecutionWorkspaceDensityStatus{
		WorkerCount:         1,
		ActorCount:          3,
		RunningActorCount:   1,
		SuspendedActorCount: 2,
		ActorsPerWorker:     "3.00",
	}, updated.Status.ExecutionWorkspace.Density)
	require.Equal(t, 750*time.Millisecond, updated.Status.ExecutionWorkspace.ResumeLatency.Duration)

	appOldWorker := fiber.New()
	appOldWorker.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{
			Username: "system:serviceaccount:default:worker",
			AuthType: AuthTypeTokenReview,
			Extra: map[string]authenticationv1.ExtraValue{
				"authentication.kubernetes.io/pod-name": {"my-task-old-pod"},
				"authentication.kubernetes.io/pod-uid":  {"old-pod-uid"},
			},
		})
		return c.Next()
	})
	appOldWorker.Post("/internal/v1/tasks/:namespace/:taskName/execution-workspace/status", h.UpdateExecutionWorkspaceStatus)
	staleBody := map[string]any{
		"provider": "substrate",
		"phase":    "Failed",
		"reason":   "WorkspaceCommandFailed",
		"message":  "stale worker update",
	}
	staleBodyBytes, err := json.Marshal(staleBody)
	require.NoError(t, err)
	staleReq := httptest.NewRequest(http.MethodPost, "/internal/v1/tasks/default/my-task/execution-workspace/status", bytes.NewReader(staleBodyBytes))
	staleReq.Header.Set("Content-Type", "application/json")
	staleResp, err := appOldWorker.Test(staleReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, staleResp.StatusCode)

	afterStale := &corev1alpha1.Task{}
	require.NoError(t, k8sClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "my-task"}, afterStale))
	require.Equal(t, corev1alpha1.ExecutionWorkspacePhaseReady, afterStale.Status.ExecutionWorkspace.Phase)
	require.NotNil(t, afterStale.Status.ExecutionWorkspace.Placement)
	require.Equal(t, "codex-pool", afterStale.Status.ExecutionWorkspace.Placement.WorkerPool)
	require.NotNil(t, afterStale.Status.ExecutionWorkspace.Density)
	require.Equal(t, int32(3), afterStale.Status.ExecutionWorkspace.Density.ActorCount)

	appForbidden := fiber.New()
	appForbidden.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{
			Username: "system:serviceaccount:default:worker",
			AuthType: AuthTypeTokenReview,
			Extra: map[string]authenticationv1.ExtraValue{
				"authentication.kubernetes.io/pod-name": {"other-pod"},
				"authentication.kubernetes.io/pod-uid":  {"other-pod-uid"},
			},
		})
		return c.Next()
	})
	appForbidden.Post("/internal/v1/tasks/:namespace/:taskName/execution-workspace/status", h.UpdateExecutionWorkspaceStatus)
	forbiddenReq := httptest.NewRequest(http.MethodPost, "/internal/v1/tasks/default/my-task/execution-workspace/status", bytes.NewReader(bodyBytes))
	forbiddenReq.Header.Set("Content-Type", "application/json")
	forbiddenResp, err := appForbidden.Test(forbiddenReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, forbiddenResp.StatusCode)

	terminalBody := map[string]any{
		"provider": "substrate",
		"phase":    "Failed",
		"reason":   "WorkspaceCommandFailed",
		"message":  "workspace command failed",
	}
	terminalBodyBytes, err := json.Marshal(terminalBody)
	require.NoError(t, err)
	terminalReq := httptest.NewRequest(http.MethodPost, "/internal/v1/tasks/default/my-task/execution-workspace/status", bytes.NewReader(terminalBodyBytes))
	terminalReq.Header.Set("Content-Type", "application/json")
	terminalResp, err := app.Test(terminalReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, terminalResp.StatusCode)

	afterTerminal := &corev1alpha1.Task{}
	require.NoError(t, k8sClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "my-task"}, afterTerminal))
	require.Equal(t, corev1alpha1.ExecutionWorkspacePhaseFailed, afterTerminal.Status.ExecutionWorkspace.Phase)
	require.NotNil(t, afterTerminal.Status.ExecutionWorkspace.Placement)
	require.Equal(t, "codex-pool", afterTerminal.Status.ExecutionWorkspace.Placement.WorkerPool)
	require.NotNil(t, afterTerminal.Status.ExecutionWorkspace.Density)
	require.Equal(t, int32(3), afterTerminal.Status.ExecutionWorkspace.Density.ActorCount)
	require.Equal(t, 750*time.Millisecond, afterTerminal.Status.ExecutionWorkspace.ResumeLatency.Duration)

	claimedBody := map[string]any{
		"provider": "substrate",
		"phase":    "Pending",
		"reason":   "WorkspaceClaimed",
		"message":  "workspace claimed",
	}
	claimedBodyBytes, err := json.Marshal(claimedBody)
	require.NoError(t, err)
	claimedReq := httptest.NewRequest(http.MethodPost, "/internal/v1/tasks/default/my-task/execution-workspace/status", bytes.NewReader(claimedBodyBytes))
	claimedReq.Header.Set("Content-Type", "application/json")
	claimedResp, err := app.Test(claimedReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, claimedResp.StatusCode)

	afterClaimed := &corev1alpha1.Task{}
	require.NoError(t, k8sClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "my-task"}, afterClaimed))
	require.Equal(t, corev1alpha1.ExecutionWorkspacePhasePending, afterClaimed.Status.ExecutionWorkspace.Phase)
	require.Nil(t, afterClaimed.Status.ExecutionWorkspace.Placement)
	require.Nil(t, afterClaimed.Status.ExecutionWorkspace.Density)
	require.Nil(t, afterClaimed.Status.ExecutionWorkspace.ResumeLatency)
}

func TestGetSessionTranscript(t *testing.T) {
	h, app, ss := setupTestInternalHandlers()
	app.Get("/internal/v1/sessions/:namespace/:name/transcript", h.GetSessionTranscript)

	// Create a session and append messages
	err := ss.CreateSession(context.Background(), &store.SessionRecord{
		Namespace:   "default",
		Name:        "my-session",
		SessionType: "task",
	})
	require.NoError(t, err)

	err = ss.AppendMessages(context.Background(), "default", "my-session", []store.SessionMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
	})
	require.NoError(t, err)

	t.Run("success", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/sessions/default/my-session/transcript", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "application/x-ndjson", resp.Header.Get("Content-Type"))

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		lines := strings.Split(strings.TrimSpace(string(body)), "\n")
		require.Len(t, lines, 2)

		var msg1, msg2 store.SessionMessage
		require.NoError(t, json.Unmarshal([]byte(lines[0]), &msg1))
		require.NoError(t, json.Unmarshal([]byte(lines[1]), &msg2))
		require.Equal(t, "user", msg1.Role)
		require.Equal(t, "hello", msg1.Content)
		require.Equal(t, "assistant", msg2.Role)
		require.Equal(t, "hi there", msg2.Content)
	})

	t.Run("missing name", func(t *testing.T) {
		app2 := fiber.New()
		app2.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{Username: "system:serviceaccount:default:worker"})
			return c.Next()
		})
		app2.Get("/internal/v1/sessions/:namespace/transcript", h.GetSessionTranscript)

		req := httptest.NewRequest(http.MethodGet, "/internal/v1/sessions/default/transcript", nil)
		resp, err := app2.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("empty transcript", func(t *testing.T) {
		err := ss.CreateSession(context.Background(), &store.SessionRecord{
			Namespace:   "default",
			Name:        "empty-session",
			SessionType: "task",
		})
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/internal/v1/sessions/default/empty-session/transcript", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Empty(t, strings.TrimSpace(string(body)))
	})
}

func TestSearchTranscript(t *testing.T) {
	h, app, ss := setupTestInternalHandlers()
	app.Get("/internal/v1/sessions/:namespace/search", h.SearchTranscript)

	now := time.Now().UTC()
	for i, name := range []string{"prior", "current"} {
		err := ss.CreateSession(context.Background(), &store.SessionRecord{
			Namespace:   "default",
			Name:        name,
			SessionType: "task",
			CreatedAt:   now.Add(time.Duration(i) * time.Second),
			UpdatedAt:   now.Add(time.Duration(i) * time.Second),
		})
		require.NoError(t, err)
	}

	priorLong := strings.Repeat("prefix ", 20) + "needle migration detail" + strings.Repeat(" suffix", 20)
	require.NoError(t, ss.AppendMessages(context.Background(), "default", "prior", []store.SessionMessage{
		{Role: "user", Content: "unrelated setup", Timestamp: now},
		{Role: "assistant", Content: priorLong, Timestamp: now.Add(time.Second)},
	}))
	require.NoError(t, ss.AppendMessages(context.Background(), "default", "current", []store.SessionMessage{
		{Role: "assistant", Content: "needle from current session should be excluded", Timestamp: now.Add(2 * time.Second)},
	}))

	t.Run("success", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/sessions/default/search?query=needle&excludeSessionName=current&limit=5&maxSnippetLength=80", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var results []store.TranscriptSearchResult
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&results))
		require.Len(t, results, 1)
		require.Equal(t, "prior", results[0].SessionName)
		require.Equal(t, "assistant", results[0].Role)
		require.Contains(t, results[0].Snippet, "needle")
	})

	t.Run("missing query", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/sessions/default/search", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("blank query", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/sessions/default/search?query=%20%20", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("invalid limit", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/sessions/default/search?query=needle&limit=bad", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("negative max snippet length", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/sessions/default/search?query=needle&maxSnippetLength=-1", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("cross namespace denied", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/sessions/other/search?query=needle", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("nil session store returns not implemented", func(t *testing.T) {
		hNil := NewInternalHandlers(nil, nil, nil, nil, nil)
		appNil := fiber.New()
		appNil.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{Username: "system:serviceaccount:default:worker"})
			return c.Next()
		})
		appNil.Get("/internal/v1/sessions/:namespace/search", hNil.SearchTranscript)

		req := httptest.NewRequest(http.MethodGet, "/internal/v1/sessions/default/search?query=needle", nil)
		resp, err := appNil.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	})
}

func TestVerifyCallerNamespace(t *testing.T) {
	h, _, _ := setupTestInternalHandlers()

	t.Run("no auth returns unauthorized", func(t *testing.T) {
		app := fiber.New()
		// No UserInfo middleware — simulates unauthenticated request
		app.Post("/internal/v1/results/:namespace/:taskName", h.SubmitResult)

		req := httptest.NewRequest(http.MethodPost, "/internal/v1/results/default/my-task",
			bytes.NewReader([]byte(`{"output":"done"}`)))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("non-SA user passes through", func(t *testing.T) {
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{Username: "admin"})
			return c.Next()
		})
		app.Post("/internal/v1/results/:namespace/:taskName", h.SubmitResult)

		req := httptest.NewRequest(http.MethodPost, "/internal/v1/results/default/my-task",
			bytes.NewReader([]byte(`{"output":"done"}`)))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
	})

	t.Run("cross-namespace SA denied", func(t *testing.T) {
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{
				Username: "system:serviceaccount:kube-system:controller",
			})
			return c.Next()
		})
		app.Post("/internal/v1/plans/:namespace/:taskName", h.SubmitPlan)

		body, _ := json.Marshal(map[string]any{"summary": "test"})
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/plans/default/my-task",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}

func TestSubmitPlanAdditional(t *testing.T) {
	h, _, _ := setupTestInternalHandlers()

	t.Run("invalid json body", func(t *testing.T) {
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{Username: "system:serviceaccount:default:worker"})
			return c.Next()
		})
		app.Post("/internal/v1/plans/:namespace/:taskName", h.SubmitPlan)

		req := httptest.NewRequest(http.MethodPost, "/internal/v1/plans/default/my-task",
			bytes.NewReader([]byte("not json")))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("cross-namespace denied", func(t *testing.T) {
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{
				Username: "system:serviceaccount:other-ns:worker",
			})
			return c.Next()
		})
		app.Post("/internal/v1/plans/:namespace/:taskName", h.SubmitPlan)

		body, _ := json.Marshal(map[string]any{"summary": "test"})
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/plans/default/my-task",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}

func TestSendMessageAdditional(t *testing.T) {
	_, _, ss := setupTestInternalHandlers()

	t.Run("messaging not enabled", func(t *testing.T) {
		hNoMsg := NewInternalHandlers(ss, ss, ss, nil, nil)
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{Username: "system:serviceaccount:default:worker"})
			return c.Next()
		})
		app.Post("/internal/v1/messages/:namespace", hNoMsg.SendMessage)

		body, _ := json.Marshal(map[string]string{
			"fromTask": "a", "toTask": "b", "parentTask": "c", "content": "hi",
		})
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/messages/default",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	})

	t.Run("missing namespace", func(t *testing.T) {
		h := NewInternalHandlers(ss, ss, ss, ss, ss, InternalHandlersConfig{
			MemoryStore:         ss,
			MemoryProposalStore: ss,
		})
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{Username: "system:serviceaccount:default:worker"})
			return c.Next()
		})
		// Route without :namespace param to simulate empty namespace
		app.Post("/internal/v1/messages/", h.SendMessage)

		body, _ := json.Marshal(map[string]string{
			"fromTask": "a", "toTask": "b", "parentTask": "c", "content": "hi",
		})
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/messages/",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("cross-namespace denied", func(t *testing.T) {
		h := NewInternalHandlers(ss, ss, ss, ss, ss, InternalHandlersConfig{
			MemoryStore:         ss,
			MemoryProposalStore: ss,
		})
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{
				Username: "system:serviceaccount:other-ns:worker",
			})
			return c.Next()
		})
		app.Post("/internal/v1/messages/:namespace", h.SendMessage)

		body, _ := json.Marshal(map[string]string{
			"fromTask": "a", "toTask": "b", "parentTask": "c", "content": "hi",
		})
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/messages/default",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}

func TestInternalApplyMemoryProposal(t *testing.T) {
	h, app, ss := setupTestInternalHandlers()
	app.Post("/internal/v1/memory-proposals/:namespace/:id/apply", h.ApplyMemoryProposal)

	proposal := &store.MemoryProposal{
		Namespace:   "default",
		TaskName:    "task-a",
		AgentName:   "agent-a",
		Type:        "memory",
		Title:       "Remember handler apply flow",
		Description: "Apply via internal API.\n\nTags: api, memory",
		Content:     "Accepted memory proposals can be applied explicitly.",
	}
	require.NoError(t, ss.CreateMemoryProposal(context.Background(), proposal))
	require.NoError(t, ss.ReviewMemoryProposal(context.Background(), store.MemoryProposalReview{
		Namespace: "default",
		ID:        proposal.ID,
		Status:    "accepted",
		Reviewer:  "reviewer",
	}))

	body, _ := json.Marshal(map[string]string{"appliedBy": "coordinator"})
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/memory-proposals/default/"+proposal.ID+"/apply", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var memory store.Memory
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&memory))
	require.Equal(t, proposal.ID, memory.SourceProposalID)
	require.Equal(t, "memory_proposal", memory.Source)
	require.Equal(t, []string{"api", "memory"}, memory.Tags)

	updated, err := ss.GetMemoryProposal(context.Background(), "default", proposal.ID)
	require.NoError(t, err)
	require.Equal(t, "applied", updated.Status)
	require.Equal(t, memory.ID, updated.AppliedMemoryID)
	require.Equal(t, "coordinator", updated.AppliedBy)
}

func TestInternalApplyMemoryProposalRejectsNamespaceMismatch(t *testing.T) {
	h, app, _ := setupTestInternalHandlers()
	app.Post("/internal/v1/memory-proposals/:namespace/:id/apply", h.ApplyMemoryProposal)

	body, _ := json.Marshal(map[string]string{"namespace": "other"})
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/memory-proposals/default/mprop-1/apply", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestGetPlanAdditional(t *testing.T) {
	h, _, _ := setupTestInternalHandlers()

	t.Run("missing taskName", func(t *testing.T) {
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{Username: "system:serviceaccount:default:worker"})
			return c.Next()
		})
		app.Get("/internal/v1/plans/:namespace", h.GetPlan)

		req := httptest.NewRequest(http.MethodGet, "/internal/v1/plans/default", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
}
