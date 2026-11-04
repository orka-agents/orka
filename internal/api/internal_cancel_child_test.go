/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/tools"
)

// setupCancelChildHandlers serves the cancel endpoint to my-task's worker Pod.
// my-task has a running child with a worker Job, a finished child, an agent
// child, a child with ACP execution, and a child whose parent label lacks the
// controller owner reference; worker-a is its sibling.
func setupCancelChildHandlers(t *testing.T) (*InternalHandlers, func(path string, body any) *http.Response) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	objects := make([]client.Object, 0, 12)
	objects = append(objects, internalCallerAuthTaskObject("coordinator", "coordinator-uid", "", "", ""))
	for _, name := range []string{"my-task", "worker-a"} {
		task := internalCallerAuthTaskObject(name, name+"-uid", name+"-job", "coordinator", "")
		job := internalCallerAuthJob(task, name+"-job", name+"-job-uid")
		objects = append(objects, task, job, internalCallerAuthPod(task, name+"-pod", name+"-pod-uid", job))
	}
	done := internalCallerAuthTaskObject("done-child", "done-child-uid", "", "my-task", "")
	done.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	agent := internalCallerAuthTaskObject("agent-child", "agent-child-uid", "", "my-task", "")
	agent.Spec.Type = corev1alpha1.TaskTypeAgent
	acp := internalCallerAuthTaskObject("acp-child", "acp-child-uid", "", "my-task", "")
	acp.Status.Execution = &corev1alpha1.TaskExecutionStatus{ControllerEpoch: 1}
	forged := internalCallerAuthTaskObject("forged-child", "forged-child-uid", "", "", "")
	forged.Labels[labels.LabelParentTask] = labels.SelectorValue("my-task")
	forged.Annotations[labels.AnnotationParentTaskName] = "my-task"
	objects = append(objects, internalCallerAuthTaskObject("my-child", "my-child-uid", "my-child-job", "my-task", ""),
		done, agent, acp, forged)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&corev1alpha1.Task{}).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	ss := testStorageClient(sqlite.NewStore(db, ":memory:"))
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

func cancelChildTask(t *testing.T, h *InternalHandlers, name string) *corev1alpha1.Task {
	t.Helper()
	task := &corev1alpha1.Task{}
	require.NoError(t, h.k8sClient.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: name}, task))
	return task
}

// requireNoCancelRequest checks that the endpoint left the Task untouched.
func requireNoCancelRequest(t *testing.T, h *InternalHandlers, name string, phase corev1alpha1.TaskPhase) {
	t.Helper()
	task := cancelChildTask(t, h, name)
	require.Equal(t, phase, task.Status.Phase)
	require.NotContains(t, task.Annotations, labels.AnnotationCancelRequested)
}

func decodeCancelResult(t *testing.T, resp *http.Response) tools.CancelTaskResult {
	t.Helper()
	var result tools.CancelTaskResult
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	return result
}

func TestCancelChildTaskRecordsTheRequestForTheController(t *testing.T) {
	h, post := setupCancelChildHandlers(t)

	resp := post("/internal/v1/tasks/default/my-task/children/my-child/cancel", map[string]string{"reason": "wrong approach"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, tools.CancelTaskResult{TaskName: "my-child", Status: "cancellation requested"}, decodeCancelResult(t, resp))
	child := cancelChildTask(t, h, "my-child")
	require.Equal(t, "cancelled by parent task: wrong approach", child.Annotations[labels.AnnotationCancelRequested])
	// The Task controller, not the API, writes the terminal status.
	require.Equal(t, corev1alpha1.TaskPhaseRunning, child.Status.Phase)

	// A repeated request keeps the first message.
	resp = post("/internal/v1/tasks/default/my-task/children/my-child/cancel", map[string]string{"reason": "again"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "cancellation requested", decodeCancelResult(t, resp).Status)
	require.Equal(t, "cancelled by parent task: wrong approach",
		cancelChildTask(t, h, "my-child").Annotations[labels.AnnotationCancelRequested])
}

func TestCancelChildTaskLeavesTerminalChildUnchanged(t *testing.T) {
	h, post := setupCancelChildHandlers(t)

	resp := post("/internal/v1/tasks/default/my-task/children/done-child/cancel", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "task is already in Succeeded phase, cannot cancel", decodeCancelResult(t, resp).Status)
	requireNoCancelRequest(t, h, "done-child", corev1alpha1.TaskPhaseSucceeded)
}

func TestCancelChildTaskRefusesAgentRuntimeChildren(t *testing.T) {
	for _, child := range []string{"agent-child", "acp-child"} {
		t.Run(child, func(t *testing.T) {
			h, post := setupCancelChildHandlers(t)
			resp := post("/internal/v1/tasks/default/my-task/children/"+child+"/cancel", nil)
			require.Equal(t, http.StatusConflict, resp.StatusCode)
			requireNoCancelRequest(t, h, child, corev1alpha1.TaskPhaseRunning)
		})
	}
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
				requireNoCancelRequest(t, h, tc.unchanged, corev1alpha1.TaskPhaseRunning)
			}
		})
	}
}

func TestCancelChildTaskRejectsRevokedParentJob(t *testing.T) {
	h, post := setupCancelChildHandlers(t)
	require.NoError(t, h.resultStore.(store.TaskJobAuthorityStore).RevokeTaskJob(t.Context(), store.TaskJobIdentity{
		Namespace: "default", TaskUID: "my-task-uid", JobUID: "my-task-job-uid",
	}))

	resp := post("/internal/v1/tasks/default/my-task/children/my-child/cancel", nil)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "task Job is no longer active")
	requireNoCancelRequest(t, h, "my-child", corev1alpha1.TaskPhaseRunning)
}

func TestCancelChildTaskRejectsLongReason(t *testing.T) {
	h, post := setupCancelChildHandlers(t)

	resp := post("/internal/v1/tasks/default/my-task/children/my-child/cancel",
		map[string]string{"reason": strings.Repeat("x", tools.MaxCancelReasonLength+1)})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	requireNoCancelRequest(t, h, "my-child", corev1alpha1.TaskPhaseRunning)
}

func TestCancelChildTaskRequiresProvenanceAdmission(t *testing.T) {
	h, post := setupCancelChildHandlers(t)
	h.taskProvenanceProtected = false

	resp := post("/internal/v1/tasks/default/my-task/children/my-child/cancel", nil)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	requireNoCancelRequest(t, h, "my-child", corev1alpha1.TaskPhaseRunning)
}

func cancellationIntents(t *testing.T, h *InternalHandlers) []store.ExecutionEvent {
	t.Helper()
	intents, err := h.resultStore.(store.ExecutionEventStore).ListExecutionEvents(t.Context(), store.ExecutionEventFilter{
		Namespace: "default", StreamID: "my-task", EventTypes: []string{events.ExecutionEventTypeTaskCancellationRequested},
	})
	require.NoError(t, err)
	return intents
}

func TestCancelChildTaskRejectsRevocationBeforeIntentCommit(t *testing.T) {
	h, post := setupCancelChildHandlers(t)
	revoked := false
	h.apiReader = interceptor.NewClient(h.k8sClient.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, object, opts...); err != nil {
				return err
			}
			// This child read is inside access, after the initial fence check,
			// immediately before cancellation commits its durable intent.
			if _, fenced := store.TaskDataFenceFrom(ctx); !fenced || key.Name != "my-child" || revoked {
				return nil
			}
			revoked = true
			return h.resultStore.(store.TaskJobAuthorityStore).RevokeTaskJob(ctx, store.TaskJobIdentity{
				Namespace: "default", TaskUID: "my-task-uid", JobUID: "my-task-job-uid",
			})
		},
	})
	resp := post("/internal/v1/tasks/default/my-task/children/my-child/cancel", nil)
	require.True(t, revoked)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	requireNoCancelRequest(t, h, "my-child", corev1alpha1.TaskPhaseRunning)
	require.Empty(t, cancellationIntents(t, h), "revocation before the intent authorizes nothing")
}

func TestCancelChildTaskPublishesIntentCommittedBeforeRevocation(t *testing.T) {
	h, post := setupCancelChildHandlers(t)
	patched := false
	h.k8sClient = interceptor.NewClient(h.k8sClient.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
			// The intent is durable before the external effect. Revocation can
			// commit here because no provider transaction spans this patch.
			require.Len(t, cancellationIntents(t, h), 1)
			require.NoError(t, h.resultStore.(store.TaskJobAuthorityStore).RevokeTaskJob(ctx, store.TaskJobIdentity{
				Namespace: "default", TaskUID: "my-task-uid", JobUID: "my-task-job-uid",
			}))
			patched = true
			return c.Patch(ctx, object, patch, opts...)
		},
	})
	resp := post("/internal/v1/tasks/default/my-task/children/my-child/cancel", map[string]string{"reason": "wrong approach"})
	require.True(t, patched)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "cancelled by parent task: wrong approach", cancelChildTask(t, h, "my-child").Annotations[labels.AnnotationCancelRequested])
	intents := cancellationIntents(t, h)
	require.Len(t, intents, 1)
	var intent childCancellationIntent
	require.NoError(t, json.Unmarshal(intents[0].Content, &intent))
	require.Equal(t, childCancellationIntent{
		ParentUID: "my-task-uid", ChildUID: "my-child-uid", ChildName: "my-child", Message: "cancelled by parent task: wrong approach",
	}, intent)
}
