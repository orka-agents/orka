package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/enforcement"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func testCompleteEnforcementRequirement() *corev1alpha1.EnforcementRequirement {
	return &corev1alpha1.EnforcementRequirement{
		Format: "example.policy/v1", Policy: []byte(" complete policy \x00\n"),
		RequiredCapabilities: []string{"filesystem/deny-write/v1"}, Mode: "enforce",
	}
}

func TestTaskResultAdvertisesEnforcementBeforeTaskCreation(t *testing.T) {
	h, app := setupTestHandlers()
	app.Get("/tasks/:id/result", h.GetTaskResult)
	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/tasks/absent/result?namespace=default", nil))
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	require.Equal(t, "container-v1", response.Header.Get("Orka-Task-Enforcement"))
}

func TestCreateTaskPreservesEnforcementBytes(t *testing.T) {
	for _, flat := range []bool{true, false} {
		t.Run(map[bool]string{true: "flat", false: "manifest"}[flat], func(t *testing.T) {
			h, app := setupTestHandlers()
			app.Post("/tasks", h.CreateTask)
			requirement := testCompleteEnforcementRequirement()
			spec := &corev1alpha1.TaskSpec{
				Type:        corev1alpha1.TaskTypeAI,
				AI:          &corev1alpha1.AISpec{Provider: "openai", Model: "test-model", Prompt: "hello"},
				Enforcement: requirement,
			}
			body := CreateTaskRequest{Name: "protected-ai", Namespace: "default", Spec: spec}
			if flat {
				body.Spec = nil
				body.Type, body.AI, body.Enforcement = spec.Type, spec.AI, requirement
			}
			blob, err := json.Marshal(body)
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewReader(blob))
			request.Header.Set("Content-Type", "application/json")
			response, err := app.Test(request)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Equal(t, http.StatusCreated, response.StatusCode)
			task := &corev1alpha1.Task{}
			require.NoError(t, h.client.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "protected-ai"}, task))
			require.Equal(t, requirement, task.Spec.Enforcement)
			wire, err := enforcement.TaskRequirement(task)
			require.NoError(t, err)
			require.Equal(t, requirement.Policy, wire.Policy)
			wire.Policy[0] = '!'
			require.Equal(t, requirement.Policy, task.Spec.Enforcement.Policy, "provider conversion must not alias Task input")
		})
	}
}

func TestEnforcedWorkerAccountingRequiresCurrentAdmission(t *testing.T) {
	for _, name := range []string{"admitted", "pending", "wrong-requirement", "container-replaced", "old-command", "unenforced-alias"} {
		t.Run(name, func(t *testing.T) {
			backend := newInternalExecutionEventStore(t)
			task, job, pod := testInternalExecutionEventOwnedWorkerObjects("owned-task")
			task.Spec.Type = corev1alpha1.TaskTypeAI
			task.Spec.Enforcement = testCompleteEnforcementRequirement()
			pod.Spec.RestartPolicy = corev1.RestartPolicyNever
			pod.Spec.Containers = []corev1.Container{{
				Name: "worker", Command: []string{"/worker-enforced"}, Args: []string{"--mode=ai"},
			}}
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "worker", ContainerID: "containerd://" + strings.Repeat("a", 64),
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}}
			requirement, err := enforcement.TaskRequirement(task)
			require.NoError(t, err)
			digest, err := enforcement.RequirementDigest(requirement)
			require.NoError(t, err)
			approval := enforcement.PodAdmission{
				Version: enforcement.PodAdmissionVersion, BindingID: "test-binding",
				RequirementDigest: digest, PodUID: string(pod.UID), ContainerID: pod.Status.ContainerStatuses[0].ContainerID,
			}
			switch name {
			case "wrong-requirement":
				approval.RequirementDigest = "sha256:" + strings.Repeat("b", 64)
			case "container-replaced":
				pod.Status.ContainerStatuses[0].ContainerID = "containerd://" + strings.Repeat("c", 64)
			case "old-command":
				pod.Spec.Containers[0].Command = []string{"/worker"}
			case "unenforced-alias":
				task.Spec.Enforcement = nil
			}
			if name != "pending" {
				blob, err := json.Marshal(approval)
				require.NoError(t, err)
				pod.Annotations = map[string]string{enforcement.PodAdmissionAnnotation: string(blob)}
			}
			app := setupInternalExecutionEventAppWithClient(backend, testInternalExecutionEventClient(t, task, job, pod), testInternalExecutionEventWorkerUser(pod.Name))
			body := map[string]any{"type": events.ExecutionEventTypeModelUsageUpdated, "content": map[string]any{"usage": store.UsageObservation{
				ID: "call", CounterID: "call", Status: store.UsageStatusCompleted,
				InputTokens: new(int64(100)), OutputTokens: new(int64(10)), ObservedAt: time.Now().UTC(),
			}}}
			response := doJSONRequest(t, app, "/internal/v1/events/default/task/owned-task", body)
			require.NoError(t, response.Body.Close())
			expected := http.StatusForbidden
			if name == "admitted" {
				expected = http.StatusCreated
			}
			require.Equal(t, expected, response.StatusCode)
		})
	}
}
