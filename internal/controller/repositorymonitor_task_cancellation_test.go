package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

func TestRepositoryMonitorCancellationRevokesJobBeforeStatusWrite(t *testing.T) {
	for _, entrypoint := range []string{"target cancellation", "validation cleanup"} {
		for _, failure := range []string{"none", "revocation unavailable", "no Job"} {
			t.Run(entrypoint+"/"+failure, func(t *testing.T) {
				ctx := t.Context()
				resultStore := setupControllerSQLiteStore(t)
				scheme := repositoryMonitorValidationTestScheme(t)
				monitor := repositoryMonitorReviewIngestTestMonitor("cancel-job-authority")
				monitor.Spec.Validation.Image = repositoryMonitorValidationTestImage
				review := repositoryMonitorReviewIngestTestTask("cancel-review", monitor.Name, 1, repositoryMonitorTestHeadSHA)
				repositoryMonitorBindValidationForTest(review)
				review.Status.Phase = corev1alpha1.TaskPhaseFailed
				task := repositoryMonitorValidationTaskForTest(monitor, review, corev1alpha1.TaskPhaseRunning, repositoryMonitorTestHeadSHA)
				task.UID = "validation-task-uid"
				task.Labels[labels.LabelGitHubTarget] = repositoryMonitorPullRequestKind
				task.Labels[labels.LabelGitHubNumber] = "1"
				if failure != "no Job" {
					task.Status.JobName, task.Status.JobUID = "validation-job", "validation-job-uid"
				}
				seedRepositoryMonitorValidationBindingForTest(t, ctx, resultStore, monitor, review, task, repositoryMonitorValidationTestCommand)
				identity := store.TaskJobIdentity{Namespace: task.Namespace, TaskUID: string(task.UID), JobUID: task.Status.JobUID}
				statusWritten := false
				k8sClient := fake.NewClientBuilder().WithScheme(scheme).
					WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(monitor, review, task).
					WithInterceptorFuncs(interceptor.Funcs{
						SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
							if obj.GetUID() == task.UID && subresource == "status" {
								statusWritten = true
								if identity.JobUID != "" {
									require.ErrorIs(t, resultStore.CheckTaskJobAuthority(ctx, identity), store.ErrTaskJobRevoked,
										"revocation must precede the Kubernetes cancellation write")
								}
							}
							return c.SubResource(subresource).Update(ctx, obj, opts...)
						},
					}).Build()
				r := &RepositoryMonitorReconciler{Client: k8sClient, Scheme: scheme, Store: resultStore, ResultStore: resultStore}
				injected := errors.New("revocation unavailable")
				switch failure {
				case "revocation unavailable":
					r.ResultStore = failingTaskJobAuthorityStore{ResultStore: resultStore, err: injected}
				case "no Job":
					r.ResultStore = nil
				}
				cancel := func() error {
					if entrypoint == "target cancellation" {
						return r.cancelRepositoryMonitorTargetTasks(ctx, monitor, repositoryMonitorPullRequestKind, 1, "target closed")
					}
					cleaned, err := r.cleanupRepositoryMonitorValidationTask(ctx, monitor, review, &store.ReviewRecord{ValidationTask: task.Name})
					require.False(t, cleaned, "validation cleanup must wait for cancellation to settle")
					return err
				}
				err := cancel()
				current := &corev1alpha1.Task{}
				if failure == "revocation unavailable" {
					require.ErrorIs(t, err, injected)
					require.False(t, statusWritten)
					require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(task), current))
					require.Equal(t, corev1alpha1.TaskPhaseRunning, current.Status.Phase)
					require.Nil(t, current.Status.CompletionTime)
					require.NoError(t, resultStore.CheckTaskJobAuthority(ctx, identity))
					r.ResultStore = resultStore
					err = cancel()
				}
				require.NoError(t, err)
				require.True(t, statusWritten)
				require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(task), current))
				require.Equal(t, corev1alpha1.TaskPhaseCancelled, current.Status.Phase)
				require.NotNil(t, current.Status.CompletionTime)
				require.Equal(t, task.Status.JobUID, current.Status.JobUID)
			})
		}
	}
}
