package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func TestInternalDataRejectsJobRevokedDuringAuthorization(t *testing.T) {
	for _, test := range []struct {
		name       string
		method     string
		path       string
		taskReads  int
		registered bool
	}{
		{"first plan read", http.MethodGet, "/internal/v1/plans/default/my-task", 4, false},
		{"first result write", http.MethodPost, "/internal/v1/results/default/my-task", 6, false},
		{"registered plan read", http.MethodGet, "/internal/v1/plans/default/my-task", 2, true},
		{"registered result write", http.MethodPost, "/internal/v1/results/default/my-task", 4, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, _, data := setupTestInternalHandlers()
			require.NoError(t, data.SavePlan(t.Context(), "default", "my-task", &store.PlanState{Summary: "current plan"}))
			require.NoError(t, data.SaveResult(t.Context(), "default", "my-task", []byte("current result")))
			app := newTaskScopedInternalApp(h, internalCallerAuthWorkerUser("my-task-pod", "my-task-pod-uid"))
			if test.registered {
				response, err := app.Test(httptest.NewRequest(http.MethodGet, "/internal/v1/plans/default/my-task", nil))
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, response.StatusCode)
				require.NoError(t, response.Body.Close())
			}
			reads := 0
			h.apiReader = interceptor.NewClient(h.k8sClient.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, object, opts...); err != nil {
						return err
					}
					task, ok := object.(*corev1alpha1.Task)
					if !ok || task.Name != "my-task" {
						return nil
					}
					reads++
					if reads != test.taskReads {
						return nil
					}
					// The API read already returned the old active binding. The
					// controller revokes it before publishing the retry state.
					if err := data.RevokeTaskJob(ctx, store.TaskJobIdentity{
						Namespace: task.Namespace, TaskUID: string(task.UID), JobUID: task.Status.JobUID,
					}); err != nil {
						return err
					}
					current := task.DeepCopy()
					current.Status.JobName, current.Status.JobUID = "", ""
					current.Status.Phase = corev1alpha1.TaskPhasePending
					err := c.Update(ctx, current)
					require.NoError(t, err)
					return err
				},
			})
			response, err := app.Test(httptest.NewRequest(test.method, test.path, strings.NewReader("stale result")))
			require.NoError(t, err)
			t.Cleanup(func() { _ = response.Body.Close() })
			require.Equal(t, test.taskReads, reads)
			require.Equal(t, http.StatusForbidden, response.StatusCode)
			result, err := data.GetResult(t.Context(), "default", "my-task")
			require.NoError(t, err)
			require.Equal(t, "current result", string(result))
		})
	}
}

func TestInternalPlanReadAllowsOtherTaskCleanupDuringAuthorization(t *testing.T) {
	h, _, data := setupTestInternalHandlers()
	require.NoError(t, data.SavePlan(t.Context(), "default", "my-task", &store.PlanState{Summary: "current plan"}))
	reads := 0
	h.apiReader = interceptor.NewClient(h.k8sClient.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, object, opts...); err != nil {
				return err
			}
			if _, ok := object.(*corev1alpha1.Task); !ok {
				return nil
			}
			reads++
			return data.DeleteArtifacts(ctx, "default", "other-task")
		},
	})
	app := newTaskScopedInternalApp(h, internalCallerAuthWorkerUser("my-task-pod", "my-task-pod-uid"))
	// The first request authorizes once to register its Task, then again under
	// that stable fence. Later reads require only the normal authorization.
	for _, expectedReads := range []int{4, 2} {
		reads = 0
		response, err := app.Test(httptest.NewRequest(http.MethodGet, "/internal/v1/plans/default/my-task", nil))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode)
		require.NoError(t, response.Body.Close())
		require.Equal(t, expectedReads, reads, "another Task's cleanup must not cause extra authorization retries")
	}
}
