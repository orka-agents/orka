/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/tools"
)

// setupCancelChildHandlers serves the cancel endpoint to my-task's worker Pod.
// my-task has a running child with a worker Job, a finished child, a child
// with ACP execution, and a child whose parent label lacks the controller
// owner reference; worker-a is its sibling.
func setupCancelChildHandlers(t *testing.T) (*InternalHandlers, func(path string, body any) *http.Response) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	objects := make([]client.Object, 0, 10)
	objects = append(objects, internalCallerAuthTaskObject("coordinator", "coordinator-uid", "", "", ""))
	for _, name := range []string{"my-task", "worker-a"} {
		task := internalCallerAuthTaskObject(name, name+"-uid", name+"-job", "coordinator", "")
		job := internalCallerAuthJob(task, name+"-job", name+"-job-uid")
		objects = append(objects, task, job, internalCallerAuthPod(task, name+"-pod", name+"-pod-uid", job))
	}
	done := internalCallerAuthTaskObject("done-child", "done-child-uid", "", "my-task", "")
	done.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	forged := internalCallerAuthTaskObject("forged-child", "forged-child-uid", "", "", "")
	forged.Labels[labels.LabelParentTask] = labels.SelectorValue("my-task")
	forged.Annotations[labels.AnnotationParentTaskName] = "my-task"
	acp := internalCallerAuthTaskObject("acp-child", "acp-child-uid", "", "my-task", "")
	acp.Status.Execution = &corev1alpha1.TaskExecutionStatus{ControllerEpoch: 1}
	objects = append(objects, internalCallerAuthTaskObject("my-child", "my-child-uid", "my-child-job", "my-task", ""),
		done, acp, forged)

	// Register the status subresource so Status().Update behaves like the
	// API server.
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&corev1alpha1.Task{}).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	ss := sqlite.NewStore(db, ":memory:")
	h := NewInternalHandlers(ss, ss, ss, ss, ss, InternalHandlersConfig{
		Client: k8sClient, APIReader: k8sClient, TaskProvenanceProtected: true,
	})
	app := newTaskScopedInternalApp(h, internalCallerAuthWorkerUser("my-task-pod", "my-task-pod-uid"))
	return h, func(path string, body any) *http.Response {
		resp := doTaskScopedInternalRequest(t, app, taskScopedRequest{method: http.MethodPost, path: path, body: body})
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
}

func cancelChildTaskStatus(t *testing.T, h *InternalHandlers, name string) corev1alpha1.TaskStatus {
	t.Helper()
	task := &corev1alpha1.Task{}
	require.NoError(t, h.k8sClient.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: name}, task))
	return task.Status
}

func TestCancelChildTaskCancelsRunningChild(t *testing.T) {
	h, post := setupCancelChildHandlers(t)

	resp := post("/internal/v1/tasks/default/my-task/children/my-child/cancel", map[string]string{"reason": "wrong approach"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var result tools.CancelTaskResult
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.Equal(t, tools.CancelTaskResult{TaskName: "my-child", Status: "cancelled"}, result)

	status := cancelChildTaskStatus(t, h, "my-child")
	require.Equal(t, corev1alpha1.TaskPhaseCancelled, status.Phase)
	require.Equal(t, "cancelled by parent task: wrong approach", status.Message)
	require.NotNil(t, status.CompletionTime)
	err := h.resultStore.(store.TaskJobAuthorityStore).CheckTaskJobAuthority(t.Context(), store.TaskJobIdentity{
		Namespace: "default", TaskUID: "my-child-uid", JobUID: "my-child-job-uid",
	})
	require.ErrorIs(t, err, store.ErrTaskJobRevoked)
}

func TestCancelChildTaskRefusesACPExecution(t *testing.T) {
	h, post := setupCancelChildHandlers(t)

	resp := post("/internal/v1/tasks/default/my-task/children/acp-child/cancel", nil)
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.Equal(t, corev1alpha1.TaskPhaseRunning, cancelChildTaskStatus(t, h, "acp-child").Phase)
}

func TestCancelChildTaskLeavesTerminalChildUnchanged(t *testing.T) {
	h, post := setupCancelChildHandlers(t)

	resp := post("/internal/v1/tasks/default/my-task/children/done-child/cancel", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var result tools.CancelTaskResult
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.Equal(t, "task is already in Succeeded phase, cannot cancel", result.Status)
	require.Equal(t, corev1alpha1.TaskPhaseSucceeded, cancelChildTaskStatus(t, h, "done-child").Phase)
}

func TestCancelChildTaskRejectsTasksOutsideCallerScope(t *testing.T) {
	for _, tc := range []struct {
		name, path, unchanged string
	}{
		{name: "sibling", path: "/internal/v1/tasks/default/my-task/children/worker-a/cancel", unchanged: "worker-a"},
		{name: "forged parent label", path: "/internal/v1/tasks/default/my-task/children/forged-child/cancel", unchanged: "forged-child"},
		{name: "missing", path: "/internal/v1/tasks/default/my-task/children/no-such-task/cancel"},
		// The caller is my-task's Pod, so it cannot act as worker-a.
		{name: "impersonated parent", path: "/internal/v1/tasks/default/worker-a/children/my-child/cancel", unchanged: "my-child"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, post := setupCancelChildHandlers(t)
			resp := post(tc.path, nil)
			require.Equal(t, http.StatusForbidden, resp.StatusCode)
			if tc.unchanged != "" {
				require.Equal(t, corev1alpha1.TaskPhaseRunning, cancelChildTaskStatus(t, h, tc.unchanged).Phase)
			}
		})
	}
}

func TestCancelChildTaskRequiresProvenanceAdmission(t *testing.T) {
	h, post := setupCancelChildHandlers(t)
	h.taskProvenanceProtected = false

	resp := post("/internal/v1/tasks/default/my-task/children/my-child/cancel", nil)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Equal(t, corev1alpha1.TaskPhaseRunning, cancelChildTaskStatus(t, h, "my-child").Phase)
}
