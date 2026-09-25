package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestWorkspaceStatusUpdatePreservesControllerAuthority(t *testing.T) {
	for _, test := range []struct {
		name    string
		change  func(*corev1alpha1.Task)
		refresh bool
	}{
		{name: "active"},
		{name: "cancelled during authorization", change: func(task *corev1alpha1.Task) {
			task.Status.Phase = corev1alpha1.TaskPhaseCancelled
		}},
		{name: "finalizing during authorization", change: func(task *corev1alpha1.Task) {
			task.Status.Phase = corev1alpha1.TaskPhaseFinalizing
			task.Status.ExecutionOutcome = &corev1alpha1.TaskWorkloadExecutionOutcome{Phase: corev1alpha1.TaskPhaseSucceeded}
		}},
		{name: "Job replaced during authorization", change: func(task *corev1alpha1.Task) {
			task.Status.JobName, task.Status.JobUID = "new-job", "new-job-uid"
		}},
		{name: "Job replaced before final authorization read", refresh: true, change: func(task *corev1alpha1.Task) {
			task.Status.JobName, task.Status.JobUID = "new-job", "new-job-uid"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			task := internalCallerAuthTaskObject("status-task", "task-uid", "status-task-job", "", "")
			workspace := &corev1alpha1.ExecutionWorkspaceStatus{
				ClassRef:      &corev1alpha1.WorkspaceClassReference{Name: "workspace-class"},
				WorkspaceRef:  &corev1alpha1.WorkspaceObjectReference{Name: "workspace", UID: "workspace-uid"},
				State:         "Attached",
				AttachedEpoch: 3,
				Conditions:    []metav1.Condition{{Type: "Attached", Status: metav1.ConditionTrue, Reason: "Attached"}},
			}
			task.Status.ExecutionWorkspace = workspace.DeepCopy()
			job := internalCallerAuthJob(task, task.Status.JobName, task.Status.JobUID)
			pod := internalCallerAuthPod(task, "status-pod", "pod-uid", job)
			kube := fake.NewClientBuilder().WithScheme(internalCallerAuthScheme(t)).
				WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task, job, pod).Build()
			reads := 0
			reader := interceptor.NewClient(kube, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, object, opts...); err != nil {
						return err
					}
					current, ok := object.(*corev1alpha1.Task)
					if !ok {
						return nil
					}
					reads++
					if reads == 4 && test.change != nil {
						// Return the last authorized Task while a controller change
						// commits before the worker can publish its status update.
						changed := current.DeepCopy()
						test.change(changed)
						require.NoError(t, c.Status().Update(ctx, changed))
						if test.refresh {
							return c.Get(ctx, key, object, opts...)
						}
					}
					return nil
				},
			})
			h := NewInternalHandlers(nil, nil, nil, nil, nil, InternalHandlersConfig{Client: kube, APIReader: reader})
			app := newTaskScopedInternalApp(h, internalCallerAuthWorkerUser(pod.Name, string(pod.UID)))
			response := doTaskScopedInternalRequest(t, app, taskScopedRequest{
				method: http.MethodPost, path: "/internal/v1/tasks/default/status-task/execution-workspace/status",
				body: map[string]string{"provider": "substrate", "phase": "Ready", "reason": "WorkspaceReady"},
			})
			t.Cleanup(func() { _ = response.Body.Close() })
			want := http.StatusNoContent
			if test.change != nil {
				want = http.StatusForbidden
			}
			require.Equal(t, want, response.StatusCode)
			require.GreaterOrEqual(t, reads, 4)
			current := &corev1alpha1.Task{}
			require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(task), current))
			require.Equal(t, workspace.ClassRef, current.Status.ExecutionWorkspace.ClassRef)
			require.Equal(t, workspace.WorkspaceRef, current.Status.ExecutionWorkspace.WorkspaceRef)
			require.Equal(t, workspace.State, current.Status.ExecutionWorkspace.State)
			require.Equal(t, workspace.AttachedEpoch, current.Status.ExecutionWorkspace.AttachedEpoch)
			require.Equal(t, workspace.Conditions, current.Status.ExecutionWorkspace.Conditions)
		})
	}
}
