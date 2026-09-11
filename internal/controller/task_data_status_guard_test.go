package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	storekube "github.com/orka-agents/orka/internal/store/kube"
)

func TestACPTaskStatusWaitsForBrokeredDataAccess(t *testing.T) {
	for _, operation := range []string{"terminal phase", "execution outcome", "monitor cancellation"} {
		t.Run(operation, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			require.NoError(t, coordinationv1.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "active", UID: "active-uid", Labels: map[string]string{
					labels.LabelRepositoryMonitor: "monitor", labels.LabelGitHubTarget: "pr", labels.LabelGitHubNumber: "315",
				}},
				Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
				Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, Execution: &corev1alpha1.TaskExecutionStatus{
					ControllerEpoch: 1, State: corev1alpha1.TaskExecutionStateRunning, Attempt: 1, PromptID: "prompt", RuntimeInstanceID: "runtime",
				}},
			}
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).
				WithStatusSubresource(&corev1alpha1.Task{}, &corev1alpha1.ControllerEpoch{}).
				WithInterceptorFuncs(interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
						if object.GetUID() == "" {
							object.SetUID(types.UID("fixture-" + object.GetName()))
						}
						return c.Create(ctx, object, opts...)
					},
				}).Build()
			control, err := storekube.New(kube, "orka-system", storekube.WithAPIReader(kube))
			require.NoError(t, err)
			epoch, err := control.CompareAndSwapControllerEpoch(t.Context(), store.ControllerEpochCAS{
				NewEpoch: 1, HolderID: "controller", RequestDigest: store.CanonicalBytesDigest([]byte("epoch")),
			})
			require.NoError(t, err)
			fence := store.ControllerEpochFence{Name: epoch.Name, Epoch: epoch.Epoch, HolderID: epoch.HolderID}
			epochs := &ControllerEpochManager{current: epoch, ready: make(chan struct{})}
			close(epochs.ready)
			other, err := storekube.New(kube, "orka-system", storekube.WithAPIReader(kube))
			require.NoError(t, err)
			reconciler := &TaskReconciler{Client: kube, DurableControlStore: other, ControllerEpochManager: epochs}
			monitor := &RepositoryMonitorReconciler{Client: kube, DurableControlStore: other, ControllerEpochManager: epochs}
			monitorObject := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: "monitor"}}
			write := func(ctx context.Context) error {
				if operation == "monitor cancellation" {
					return monitor.cancelRepositoryMonitorTargetTasks(ctx, monitorObject, "pr", 315, "target closed")
				}
				return reconciler.updateStatusWithRetry(ctx, task, func(current *corev1alpha1.Task) {
					if operation == "terminal phase" {
						current.Status.Phase = corev1alpha1.TaskPhaseCancelled
					} else {
						current.Status.ExecutionOutcome = &corev1alpha1.TaskWorkloadExecutionOutcome{Phase: corev1alpha1.TaskPhaseSucceeded}
					}
				})
			}
			require.NoError(t, control.WithControllerEpochMutation(t.Context(), fence, func(ctx context.Context) error {
				bounded, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
				defer cancel()
				writeErr := write(bounded)
				current := &corev1alpha1.Task{}
				require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(task), current))
				require.ErrorIs(t, writeErr, context.DeadlineExceeded)
				require.Equal(t, corev1alpha1.TaskPhaseRunning, current.Status.Phase)
				require.Nil(t, current.Status.ExecutionOutcome)
				return nil
			}))
			reconciler.DurableControlStore, monitor.DurableControlStore = nil, nil
			require.ErrorContains(t, write(t.Context()), "authoritative epoch mutation guard")
			current := &corev1alpha1.Task{}
			require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(task), current))
			require.Equal(t, corev1alpha1.TaskPhaseRunning, current.Status.Phase)
			require.Nil(t, current.Status.ExecutionOutcome)
			reconciler.DurableControlStore, monitor.DurableControlStore = other, other
			require.NoError(t, write(t.Context()), "status write must proceed after data access releases authority")
		})
	}
}
