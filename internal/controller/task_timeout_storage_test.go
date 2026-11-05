package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// A Task whose Job succeeded before the Task timeout, but whose completion was
// deferred past it, for example while the store was not accepting writes,
// completes as the Job's success. A Job that succeeded after the deadline, or
// whose completion time is unknown, still times out.
func TestHandleRunningCompletesAJobThatSucceededBeforeTheTimeout(t *testing.T) {
	timeout := metav1.Duration{Duration: 10 * time.Second}
	started := metav1.NewTime(time.Now().Add(-time.Minute))
	beforeDeadline := metav1.NewTime(started.Add(5 * time.Second))
	afterDeadline := metav1.NewTime(started.Add(20 * time.Second))
	for _, tc := range []struct {
		name       string
		completion *metav1.Time
		jobUID     types.UID
		lookupErr  error
		want       corev1alpha1.TaskPhase
	}{
		{"succeeded before the deadline", &beforeDeadline, "job-uid", nil, corev1alpha1.TaskPhaseSucceeded},
		{"succeeded after the deadline", &afterDeadline, "job-uid", nil, corev1alpha1.TaskPhaseFailed},
		{"completion time unknown", nil, "job-uid", nil, corev1alpha1.TaskPhaseFailed},
		{"another Job with the same name", &beforeDeadline, "other-job-uid", nil, corev1alpha1.TaskPhaseFailed},
		{"transient Job lookup failure", &beforeDeadline, "job-uid", apierrors.NewServiceUnavailable("temporary Job lookup failure"), corev1alpha1.TaskPhaseRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: "deadline-task", Namespace: "default", UID: "deadline-task-uid"},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer, Timeout: &timeout},
				Status: corev1alpha1.TaskStatus{
					Phase: corev1alpha1.TaskPhaseRunning, Attempts: 1, StartTime: &started,
					JobName: "deadline-job", JobUID: "job-uid",
				},
			}
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "deadline-job", Namespace: "default", UID: tc.jobUID},
				Status:     batchv1.JobStatus{Succeeded: 1, CompletionTime: tc.completion},
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "deadline-job-pod", Namespace: "default", Labels: map[string]string{"job-name": "deadline-job"},
			}}
			r := newUnitReconciler(newTestScheme(), task, job, pod)
			r.KubeClient = k8sfake.NewSimpleClientset(pod)
			if tc.lookupErr != nil {
				failed := false
				r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
						if _, isJob := object.(*batchv1.Job); isJob && !failed {
							failed = true
							return tc.lookupErr
						}
						return c.Get(ctx, key, object, opts...)
					},
				})
			}

			_, err := r.handleRunning(ctx, task.DeepCopy())
			if tc.lookupErr != nil {
				// Later calls would succeed, so the first lookup failure must
				// not let timeout finalization overwrite an actual success.
				require.ErrorIs(t, err, tc.lookupErr)
			}
			current := &corev1alpha1.Task{}
			if err := r.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
				t.Fatal(err)
			}
			if current.Status.Phase != tc.want {
				t.Fatalf("phase = %s (%q), want %s", current.Status.Phase, current.Status.Message, tc.want)
			}
			if tc.lookupErr != nil {
				require.Nil(t, current.Status.CompletionTime)
				_, err = r.handleRunning(ctx, task.DeepCopy())
				require.NoError(t, err)
				require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(task), current))
				require.Equal(t, corev1alpha1.TaskPhaseSucceeded, current.Status.Phase)
			}
		})
	}
}
