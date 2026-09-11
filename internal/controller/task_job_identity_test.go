package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

func TestCreateTaskJobPersistsCreatedUID(t *testing.T) {
	task := taskJobIdentityFixture()
	r := newUnitReconciler(newTestScheme(), task)
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			if job, ok := object.(*batchv1.Job); ok {
				job.UID = "controller-created-job-uid"
			}
			return c.Create(ctx, object, opts...)
		},
	})
	_, err := r.createTaskJob(t.Context(), task, nil, nil)
	require.NoError(t, err)
	current := &corev1alpha1.Task{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), current))
	require.Equal(t, "controller-created-job-uid", current.Status.JobUID)
	job := &batchv1.Job{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: task.Namespace, Name: current.Status.JobName}, job))
	require.Equal(t, string(job.UID), current.Status.JobUID)

	_, err = r.retryTask(t.Context(), current)
	require.NoError(t, err)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), current))
	require.Empty(t, current.Status.JobName)
	require.Empty(t, current.Status.JobUID)
	require.ErrorIs(t, r.ResultStore.(store.TaskJobAuthorityStore).CheckTaskJobAuthority(t.Context(), store.TaskJobIdentity{
		Namespace: task.Namespace, TaskUID: string(task.UID), JobUID: string(job.UID),
	}), store.ErrTaskJobRevoked)
}

func TestTaskJobAuthorityRevokedBeforeStatusChange(t *testing.T) {
	for _, test := range []struct {
		name   string
		revoke bool
		mutate func(*corev1alpha1.Task)
	}{
		{"retry", true, func(task *corev1alpha1.Task) {
			task.Status.Phase = corev1alpha1.TaskPhasePending
			task.Status.JobName, task.Status.JobUID = "", ""
		}},
		{"completion", true, func(task *corev1alpha1.Task) { task.Status.Phase = corev1alpha1.TaskPhaseSucceeded }},
		{"execution outcome", true, func(task *corev1alpha1.Task) {
			task.Status.Phase = corev1alpha1.TaskPhaseFinalizing
			task.Status.ExecutionOutcome = &corev1alpha1.TaskWorkloadExecutionOutcome{Phase: corev1alpha1.TaskPhaseSucceeded}
		}},
		{"progress", false, func(task *corev1alpha1.Task) { task.Status.Message = "still running" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			task := taskJobIdentityFixture()
			task.Status.Phase, task.Status.JobName, task.Status.JobUID = corev1alpha1.TaskPhaseRunning, "job", "job-uid"
			r := newUnitReconciler(newTestScheme(), task)
			authority := r.ResultStore.(store.TaskJobAuthorityStore)
			identity := store.TaskJobIdentity{Namespace: task.Namespace, TaskUID: string(task.UID), JobUID: task.Status.JobUID}
			checked := false
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c client.Client, name string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					checked = true
					err := authority.CheckTaskJobAuthority(ctx, identity)
					if test.revoke {
						require.ErrorIs(t, err, store.ErrTaskJobRevoked, "revocation must precede the Kubernetes status write")
					} else {
						require.NoError(t, err)
					}
					return c.SubResource(name).Update(ctx, obj, opts...)
				},
			})
			require.NoError(t, r.updateStatusWithRetry(t.Context(), task, test.mutate))
			require.True(t, checked)
		})
	}
}

type failingTaskJobAuthorityStore struct {
	store.ResultStore
	store.TaskJobAuthorityStore
	err error
}

func (s failingTaskJobAuthorityStore) RevokeTaskJob(context.Context, store.TaskJobIdentity) error {
	return s.err
}

func (s failingTaskJobAuthorityStore) DeleteTaskJobRevocations(context.Context, string, string, string) error {
	return s.err
}

func TestTaskJobRevocationCleanupBeforeFinalizerRemoval(t *testing.T) {
	for _, failCleanup := range []bool{false, true} {
		t.Run(map[bool]string{false: "cleanup succeeds", true: "cleanup fails"}[failCleanup], func(t *testing.T) {
			task := taskJobIdentityFixture()
			task.Finalizers = []string{labels.TaskFinalizer}
			r := newUnitReconciler(newTestScheme(), task)
			authority := r.ResultStore.(store.TaskJobAuthorityStore)
			identity := store.TaskJobIdentity{Namespace: task.Namespace, TaskUID: string(task.UID), JobUID: "retired-job-uid"}
			require.NoError(t, authority.RevokeTaskJob(t.Context(), identity))
			require.NoError(t, r.ResultStore.SaveResult(t.Context(), task.Namespace, task.Name, []byte("old result")))
			require.NoError(t, r.Delete(t.Context(), task))
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
			wantErr := errors.New("revocation cleanup unavailable")
			if failCleanup {
				r.ResultStore = failingTaskJobAuthorityStore{ResultStore: r.ResultStore, TaskJobAuthorityStore: authority, err: wantErr}
			}
			finalizerRemoved := false
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if current, ok := object.(*corev1alpha1.Task); ok && !controllerutil.ContainsFinalizer(current, labels.TaskFinalizer) {
						finalizerRemoved = true
						require.NoError(t, authority.CheckTaskJobAuthority(ctx, identity), "revocations must be reclaimed before finalizer removal")
						_, err := r.ResultStore.GetResult(ctx, task.Namespace, task.Name)
						require.ErrorIs(t, err, store.ErrNotFound)
					}
					return c.Patch(ctx, object, patch, opts...)
				},
			})
			_, err := r.handleDeletion(t.Context(), task)
			if failCleanup {
				require.ErrorIs(t, err, wantErr)
				require.False(t, finalizerRemoved)
				current := &corev1alpha1.Task{}
				require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), current))
				require.True(t, controllerutil.ContainsFinalizer(current, labels.TaskFinalizer))
				require.ErrorIs(t, authority.CheckTaskJobAuthority(t.Context(), identity), store.ErrTaskJobRevoked)
				return
			}
			require.NoError(t, err)
			require.True(t, finalizerRemoved)
			require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(task), &corev1alpha1.Task{})))
		})
	}
}

func TestTaskJobRevocationFailurePreservesStatus(t *testing.T) {
	task := taskJobIdentityFixture()
	task.Status.Phase, task.Status.JobName, task.Status.JobUID = corev1alpha1.TaskPhaseRunning, "job", "job-uid"
	r := newUnitReconciler(newTestScheme(), task)
	wantErr := errors.New("authority write unavailable")
	r.ResultStore = failingTaskJobAuthorityStore{ResultStore: r.ResultStore, err: wantErr}
	err := r.updateStatusWithRetry(t.Context(), task, func(current *corev1alpha1.Task) {
		current.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	})
	require.ErrorIs(t, err, wantErr)
	current := &corev1alpha1.Task{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), current))
	require.Equal(t, corev1alpha1.TaskPhaseRunning, current.Status.Phase)
	require.Equal(t, "job-uid", current.Status.JobUID)
}

func TestCreateTaskJobDoesNotAdoptUnboundOrReplacedJob(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "unbound after crash", true: "replaced after binding"}[bound], func(t *testing.T) {
			task := taskJobIdentityFixture()
			r := newUnitReconciler(newTestScheme(), task)
			existing, err := r.JobBuilder.Build(t.Context(), task, nil, nil)
			require.NoError(t, err)
			existing.UID = "untrusted-job-uid"
			require.NoError(t, controllerutil.SetControllerReference(task, existing, r.Scheme))
			require.NoError(t, r.Create(t.Context(), existing))
			if bound {
				task.Status.JobName, task.Status.JobUID = existing.Name, "original-job-uid"
				require.NoError(t, r.Status().Update(t.Context(), task))
			}
			result, err := r.createTaskJob(t.Context(), task, nil, nil)
			require.NoError(t, err)
			require.Equal(t, time.Second, result.RequeueAfter)
			require.Equal(t, corev1alpha1.TaskPhaseFailed, task.Status.Phase)
			require.NotEqual(t, string(existing.UID), task.Status.JobUID)
			require.Contains(t, task.Status.Message, "no matching recorded UID")
			observed := &batchv1.Job{}
			require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(existing), observed)),
				"uncertain execution must be retired before the Task is reported failed")
			_, err = r.createTaskJob(t.Context(), task, nil, nil)
			require.NoError(t, err)
			require.Zero(t, task.Status.Attempts, "failed identity recovery must not restart execution")
		})
	}
}

func TestRejectedTaskJobRetirementRetriesWithoutReplay(t *testing.T) {
	for _, stage := range []string{"record rejection", "delete Job", "record failure"} {
		t.Run(stage, func(t *testing.T) {
			task := taskJobIdentityFixture()
			r := newUnitReconciler(newTestScheme(), task)
			job, err := r.JobBuilder.Build(t.Context(), task, nil, nil)
			require.NoError(t, err)
			job.UID = "unbound-job-uid"
			require.NoError(t, controllerutil.SetControllerReference(task, job, r.Scheme))
			require.NoError(t, r.Create(t.Context(), job))
			blocked := true
			creates, deletes := 0, 0
			injected := errors.New("temporary " + stage + " failure")
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
					if _, ok := object.(*batchv1.Job); ok {
						creates++
					}
					return c.Create(ctx, object, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
					if _, ok := object.(*batchv1.Job); ok {
						deletes++
						options := &client.DeleteOptions{}
						for _, opt := range opts {
							opt.ApplyToDelete(options)
						}
						require.NotNil(t, options.Preconditions)
						require.Equal(t, job.UID, *options.Preconditions.UID)
						require.Equal(t, job.ResourceVersion, *options.Preconditions.ResourceVersion)
						require.Equal(t, metav1.DeletePropagationForeground, *options.PropagationPolicy)
						if blocked && stage == "delete Job" {
							return injected
						}
					}
					return c.Delete(ctx, object, opts...)
				},
				SubResourceUpdate: func(ctx context.Context, c client.Client, name string, object client.Object, opts ...client.SubResourceUpdateOption) error {
					current, ok := object.(*corev1alpha1.Task)
					if blocked && ok && ((stage == "record rejection" && current.Status.Phase == corev1alpha1.TaskPhasePending) ||
						(stage == "record failure" && current.Status.Phase == corev1alpha1.TaskPhaseFailed)) {
						return injected
					}
					return c.SubResource(name).Update(ctx, object, opts...)
				},
			})
			_, err = r.createTaskJob(t.Context(), task, nil, nil)
			require.ErrorIs(t, err, injected)
			current := &corev1alpha1.Task{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), current))
			require.Equal(t, corev1alpha1.TaskPhasePending, current.Status.Phase)
			require.Empty(t, current.Status.JobUID, "retirement must never authorize the unbound worker")
			jobErr := r.Get(t.Context(), client.ObjectKeyFromObject(job), &batchv1.Job{})
			if stage == "record failure" {
				require.True(t, apierrors.IsNotFound(jobErr))
			} else {
				require.NoError(t, jobErr)
			}
			if stage == "record rejection" {
				require.Zero(t, deletes, "rejection must be durable before deletion")
			}

			// Resume from persisted state, as a restarted controller would.
			blocked = false
			_, err = r.createTaskJob(t.Context(), current, nil, nil)
			require.NoError(t, err)
			require.Equal(t, corev1alpha1.TaskPhaseFailed, current.Status.Phase)
			require.Empty(t, current.Status.JobUID)
			require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(job), &batchv1.Job{})))
			wantCreates := 1
			if stage == "record rejection" {
				wantCreates = 2 // Both requests encounter the original Job.
			}
			require.Equal(t, wantCreates, creates, "a persisted rejection must never recreate execution")
		})
	}
}

func TestRejectedTaskJobRetirementPreservesReplacement(t *testing.T) {
	task := taskJobIdentityFixture()
	r := newUnitReconciler(newTestScheme(), task)
	job, err := r.JobBuilder.Build(t.Context(), task, nil, nil)
	require.NoError(t, err)
	job.UID = "unbound-job-uid"
	require.NoError(t, controllerutil.SetControllerReference(task, job, r.Scheme))
	require.NoError(t, r.Create(t.Context(), job))
	replacement := job.DeepCopy()
	replacement.UID, replacement.ResourceVersion = "foreign-job-uid", ""
	replacement.OwnerReferences = nil
	replaced := false
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
			if _, ok := object.(*batchv1.Job); ok && !replaced {
				replaced = true
				require.NoError(t, c.Delete(ctx, job))
				require.NoError(t, c.Create(ctx, replacement))
				// The fake checks resourceVersion but not UID preconditions.
				// Give the replacement a distinct version to exercise conflict handling.
				replacement.Annotations = map[string]string{"test.orka.ai/replacement": "true"}
				require.NoError(t, c.Update(ctx, replacement))
			}
			return c.Delete(ctx, object, opts...)
		},
	})
	_, err = r.createTaskJob(t.Context(), task, nil, nil)
	require.True(t, apierrors.IsConflict(err), "deletion preconditions must preserve the replacement")
	current := &corev1alpha1.Task{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), current))
	require.Equal(t, corev1alpha1.TaskPhasePending, current.Status.Phase)
	_, err = r.createTaskJob(t.Context(), current, nil, nil)
	require.NoError(t, err)
	require.Equal(t, corev1alpha1.TaskPhaseFailed, current.Status.Phase)
	_, err = r.cleanupTerminalTaskJob(t.Context(), current)
	require.NoError(t, err)
	_, err = r.cleanupDeletedTaskJob(t.Context(), current)
	require.NoError(t, err)
	observed := &batchv1.Job{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(job), observed))
	require.Equal(t, replacement.UID, observed.UID)
}

func TestCreateTaskJobRecoversOnlyRecordedJobUID(t *testing.T) {
	task := taskJobIdentityFixture()
	r := newUnitReconciler(newTestScheme(), task)
	existing, err := r.JobBuilder.Build(t.Context(), task, nil, nil)
	require.NoError(t, err)
	existing.UID = "recorded-job-uid"
	require.NoError(t, controllerutil.SetControllerReference(task, existing, r.Scheme))
	require.NoError(t, r.Create(t.Context(), existing))
	task.Status.JobName, task.Status.JobUID = existing.Name, string(existing.UID)
	require.NoError(t, r.Status().Update(t.Context(), task))
	_, err = r.createTaskJob(t.Context(), task, nil, nil)
	require.NoError(t, err)
	require.Equal(t, corev1alpha1.TaskPhaseRunning, task.Status.Phase)
	require.Equal(t, string(existing.UID), task.Status.JobUID)
}

func TestCreateTaskJobDoesNotReplayJobRemovedDuringRecovery(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(map[bool]string{false: "disappeared", true: "deleting"}[deleting], func(t *testing.T) {
			task := taskJobIdentityFixture()
			r := newUnitReconciler(newTestScheme(), task)
			existing, err := r.JobBuilder.Build(t.Context(), task, nil, nil)
			require.NoError(t, err)
			existing.UID = "uncertain-job-uid"
			if deleting {
				existing.Finalizers = []string{"test.orka.ai/hold-deletion"}
			}
			require.NoError(t, controllerutil.SetControllerReference(task, existing, r.Scheme))
			require.NoError(t, r.Create(t.Context(), existing))
			creates := 0
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
					if _, ok := object.(*batchv1.Job); !ok {
						return c.Create(ctx, object, opts...)
					}
					creates++
					err := c.Create(ctx, object, opts...)
					require.True(t, apierrors.IsAlreadyExists(err))
					require.NoError(t, c.Delete(ctx, existing))
					return err
				},
			})
			_, err = r.createTaskJob(t.Context(), task, nil, nil)
			require.NoError(t, err)
			require.Equal(t, corev1alpha1.TaskPhaseFailed, task.Status.Phase)
			require.Empty(t, task.Status.JobUID)
			require.Contains(t, task.Status.Message, "task Job identity cannot be verified")
			_, err = r.createTaskJob(t.Context(), task, nil, nil)
			require.NoError(t, err)
			require.Equal(t, 1, creates, "uncertain execution must not be automatically replayed")
		})
	}
}

func TestCreateTaskJobPreservesLiveBindingWhenCacheIsStale(t *testing.T) {
	task := taskJobIdentityFixture()
	r := newUnitReconciler(newTestScheme(), task)
	existing, err := r.JobBuilder.Build(t.Context(), task, nil, nil)
	require.NoError(t, err)
	existing.UID = "recorded-job-uid"
	require.NoError(t, controllerutil.SetControllerReference(task, existing, r.Scheme))
	require.NoError(t, r.Create(t.Context(), existing))
	current := task.DeepCopy()
	current.Status.Phase = corev1alpha1.TaskPhaseRunning
	current.Status.JobName, current.Status.JobUID = existing.Name, string(existing.UID)
	r.APIReader = fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(current, existing).Build()
	_, err = r.createTaskJob(t.Context(), task, nil, nil)
	require.NoError(t, err)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(existing), &batchv1.Job{}))
	require.Equal(t, current.Status, task.Status)
}

func TestCreateTaskJobDoesNotBindRecreatedTask(t *testing.T) {
	task := taskJobIdentityFixture()
	r := newUnitReconciler(newTestScheme(), task)
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			job, ok := object.(*batchv1.Job)
			if !ok {
				return c.Create(ctx, object, opts...)
			}
			job.UID = "original-task-job-uid"
			if err := c.Create(ctx, job, opts...); err != nil {
				return err
			}
			replacement := task.DeepCopy()
			if err := c.Delete(ctx, replacement); err != nil {
				return err
			}
			replacement.UID, replacement.ResourceVersion = "replacement-task-uid", ""
			return c.Create(ctx, replacement)
		},
	})
	_, err := r.createTaskJob(t.Context(), task, nil, nil)
	require.NoError(t, err)
	current := &corev1alpha1.Task{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), current))
	require.EqualValues(t, "replacement-task-uid", current.UID)
	require.Equal(t, corev1alpha1.TaskPhasePending, current.Status.Phase)
	require.Empty(t, current.Status.JobName)
	require.Empty(t, current.Status.JobUID)
}

func taskJobIdentityFixture() *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "bound-task-job", Namespace: "default", UID: "task-uid"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer, Image: "busybox:latest", Command: []string{"true"},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
}
