/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

const (
	testRetryGateCommand    = "true"
	testStatusSubresource   = "status"
	testCurrentJobName      = "current-job"
	testMissingJobName      = "nonexistent"
	testRetryMissingJobName = "missing-job"
	testNonexistentJobName  = "nonexistent-job"
	testTaskResource        = "tasks"
)

func retryTestTaskOwner(task *corev1alpha1.Task) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: corev1alpha1.GroupVersion.String(),
		Kind:       retryTaskOwnerKind,
		Name:       task.Name,
		UID:        task.UID,
		Controller: &controller,
	}
}

func retryTestJob(task *corev1alpha1.Task, name string, uid types.UID) *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:            name,
		Namespace:       task.Namespace,
		UID:             uid,
		OwnerReferences: []metav1.OwnerReference{retryTestTaskOwner(task)},
	}}
}

func TestHandlePending_DueRetryCleanupRequiresOriginalJobUIDAndTaskOwner(t *testing.T) {
	tests := []struct {
		name        string
		reusedUID   types.UID
		reusedOwner func(*corev1alpha1.Task) metav1.OwnerReference
	}{
		{
			name:      "different job uid",
			reusedUID: "replacement-job-uid",
			reusedOwner: func(task *corev1alpha1.Task) metav1.OwnerReference {
				return retryTestTaskOwner(task)
			},
		},
		{
			name:      "different task owner",
			reusedUID: "original-job-uid",
			reusedOwner: func(task *corev1alpha1.Task) metav1.OwnerReference {
				other := task.DeepCopy()
				other.Name = "other" + "-task"
				other.UID = "other-task-uid"
				return retryTestTaskOwner(other)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newTestScheme()
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "retry-reused-job-" + strings.ReplaceAll(tt.name, " ", "-"),
					Namespace:  defaultNS,
					UID:        "retry-reused-task-uid",
					Finalizers: []string{labels.TaskFinalizer},
				},
				Spec: corev1alpha1.TaskSpec{
					Type:        corev1alpha1.TaskTypeContainer,
					Image:       testBusyboxImage,
					Command:     []string{testRetryGateCommand},
					RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 3},
				},
				Status: corev1alpha1.TaskStatus{
					Phase:    corev1alpha1.TaskPhaseRunning,
					JobName:  "retry-reused-source-job",
					Attempts: 1,
				},
			}
			originalJob := retryTestJob(task, task.Status.JobName, "original-job-uid")
			r := newUnitReconciler(scheme, task, originalJob)

			if _, err := r.retryTask(context.Background(), task); err != nil {
				t.Fatalf("retryTask() error = %v", err)
			}

			nn := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}
			pending := &corev1alpha1.Task{}
			if err := r.Get(context.Background(), nn, pending); err != nil {
				t.Fatalf("Get(Pending Task) error = %v", err)
			}
			if got := pending.Annotations[labels.AnnotationRetryJobUID]; got != string(originalJob.UID) {
				t.Fatalf("retry job UID = %q, want %q", got, originalJob.UID)
			}
			pending.Annotations[labels.AnnotationRetryNotBefore] = time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
			if err := r.Update(context.Background(), pending); err != nil {
				t.Fatalf("Update(due Task) error = %v", err)
			}

			reusedJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
				Name:            originalJob.Name,
				Namespace:       originalJob.Namespace,
				UID:             tt.reusedUID,
				OwnerReferences: []metav1.OwnerReference{tt.reusedOwner(task)},
			}}
			if err := r.Create(context.Background(), reusedJob); err != nil {
				t.Fatalf("Create(reused Job) error = %v", err)
			}

			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: nn}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}

			preserved := &batchv1.Job{}
			if err := r.Get(context.Background(), types.NamespacedName{Name: reusedJob.Name, Namespace: reusedJob.Namespace}, preserved); err != nil {
				t.Fatalf("reused same-name Job was deleted: %v", err)
			}
			if preserved.UID != reusedJob.UID {
				t.Fatalf("reused Job UID = %q, want %q", preserved.UID, reusedJob.UID)
			}
		})
	}
}

func TestHandlePending_GateClearConflictPreservesNewerRetryGate(t *testing.T) {
	scheme := newTestScheme()
	oldGate := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	newGate := time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339Nano)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "retry-gate-newer-generation",
			Namespace:  defaultNS,
			UID:        "retry-gate-newer-generation-uid",
			Finalizers: []string{labels.TaskFinalizer},
			Annotations: map[string]string{
				labels.AnnotationRetryNotBefore: oldGate,
				labels.AnnotationRetryJobUID:    "",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:        corev1alpha1.TaskTypeContainer,
			Image:       testBusyboxImage,
			Command:     []string{testRetryGateCommand},
			RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 3},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending, Attempts: 1},
	}
	replacementJobName := buildTaskJobName(task)
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(task).
		Build()
	injectedConflict := false
	fc := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*corev1alpha1.Task); ok && !injectedConflict {
				injectedConflict = true
				latest := &corev1alpha1.Task{}
				nn := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}
				if err := c.Get(ctx, nn, latest); err != nil {
					return err
				}
				latest.Annotations[labels.AnnotationRetryNotBefore] = newGate
				latest.Annotations[labels.AnnotationRetryJobUID] = "newer-source-job-uid"
				if err := c.Update(ctx, latest); err != nil {
					return err
				}
				if err := c.Get(ctx, nn, latest); err != nil {
					return err
				}
				latest.Status.Phase = corev1alpha1.TaskPhasePending
				latest.Status.Attempts = 2
				latest.Status.JobName = replacementJobName
				if err := c.SubResource(testStatusSubresource).Update(ctx, latest); err != nil {
					return err
				}
				return apierrors.NewConflict(
					schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: testTaskResource},
					obj.GetName(),
					errors.New("injected newer retry gate"),
				)
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	r := newUnitReconciler(scheme)
	r.Client = fc
	r.JobBuilder = NewJobBuilder(fc)
	nn := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: nn}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !injectedConflict {
		t.Fatal("retry gate clear did not reach the injected conflict")
	}

	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), nn, updated); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhasePending || updated.Status.Attempts != 2 || updated.Status.JobName != replacementJobName {
		t.Fatalf("status = %#v, want newer Pending retry generation unchanged", updated.Status)
	}
	if got := updated.Annotations[labels.AnnotationRetryNotBefore]; got != newGate {
		t.Fatalf("retry-not-before = %q, want newer value %q", got, newGate)
	}
	if got := updated.Annotations[labels.AnnotationRetryJobUID]; got != "newer-source-job-uid" {
		t.Fatalf("retry job UID = %q, want newer value", got)
	}
	if err := r.Get(context.Background(), types.NamespacedName{Name: replacementJobName, Namespace: task.Namespace}, &batchv1.Job{}); err != nil {
		t.Fatalf("newer retry Job was removed: %v", err)
	}
}

func TestHandlePending_GateClearConflictDeletesUntrackedReplacement(t *testing.T) {
	scheme := newTestScheme()
	gate := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "retry-gate-untracked-replacement",
			Namespace:  defaultNS,
			UID:        "retry-gate-untracked-replacement-uid",
			Finalizers: []string{labels.TaskFinalizer},
			Annotations: map[string]string{
				labels.AnnotationRetryNotBefore: gate,
				labels.AnnotationRetryJobUID:    "",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:        corev1alpha1.TaskTypeContainer,
			Image:       testBusyboxImage,
			Command:     []string{testRetryGateCommand},
			RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 3},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending, Attempts: 1},
	}
	replacementJobName := buildTaskJobName(task)
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(task).
		Build()
	injectedConflict := false
	fc := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*corev1alpha1.Task); ok && !injectedConflict {
				injectedConflict = true
				nn := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}
				latest := &corev1alpha1.Task{}
				if err := c.Get(ctx, nn, latest); err != nil {
					return err
				}
				latest.Spec.RetryPolicy = nil
				if err := c.Update(ctx, latest); err != nil {
					return err
				}
				if err := c.Get(ctx, nn, latest); err != nil {
					return err
				}
				latest.Status.Phase = corev1alpha1.TaskPhaseRunning
				latest.Status.JobName = "disallowed-source-job"
				if err := c.SubResource(testStatusSubresource).Update(ctx, latest); err != nil {
					return err
				}
				return apierrors.NewConflict(
					schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: testTaskResource},
					obj.GetName(),
					errors.New("retry became disallowed"),
				)
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	r := newUnitReconciler(scheme)
	r.Client = fc
	r.JobBuilder = NewJobBuilder(fc)
	nn := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: nn}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !injectedConflict {
		t.Fatal("retry gate clear did not reach the injected conflict")
	}
	if err := r.Get(context.Background(), types.NamespacedName{Name: replacementJobName, Namespace: task.Namespace}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("replacement Job error = %v, want deleted after it became untracked", err)
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), nn, updated); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseRunning || updated.Status.JobName != "disallowed-source-job" {
		t.Fatalf("status = %#v, want concurrent disallowed state preserved", updated.Status)
	}
}

func TestRetryTask_DeleteErrorPropagatesAndKeepsRunning(t *testing.T) {
	scheme := newTestScheme()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "retry-delete-error-job", Namespace: defaultNS},
	}
	initialDelay := metav1.Duration{Duration: time.Minute}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "retry-delete-error", Namespace: defaultNS},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			RetryPolicy: &corev1alpha1.RetryPolicy{
				MaxRetries:   3,
				InitialDelay: &initialDelay,
			},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:    corev1alpha1.TaskPhaseRunning,
			JobName:  job.Name,
			Attempts: 1,
		},
	}
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(task, job).
		Build()
	deleteErr := errors.New("injected retry job delete failure")
	fc := interceptor.NewClient(base, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*batchv1.Job); ok {
				return deleteErr
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	r := newUnitReconciler(scheme)
	r.Client = fc
	r.JobBuilder = NewJobBuilder(fc)

	if _, err := r.retryTask(context.Background(), task); !errors.Is(err, deleteErr) {
		t.Fatalf("retryTask() error = %v, want %v", err, deleteErr)
	}

	updated := &corev1alpha1.Task{}
	nn := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}
	if err := r.Get(context.Background(), nn, updated); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseRunning {
		t.Fatalf("phase = %s, want Running until the old Job is deleted", updated.Status.Phase)
	}
	if updated.Status.JobName != job.Name {
		t.Fatalf("JobName = %q, want %q", updated.Status.JobName, job.Name)
	}
	raw := updated.Annotations[labels.AnnotationRetryNotBefore]
	if _, err := time.Parse(time.RFC3339Nano, raw); err != nil {
		t.Fatalf("retry-not-before = %q, want persisted timestamp: %v", raw, err)
	}
	if err := r.Get(context.Background(), types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, &batchv1.Job{}); err != nil {
		t.Fatalf("old Job should remain after delete failure: %v", err)
	}
}

func TestRetryTask_WaitsForForegroundJobDeletionBeforeReset(t *testing.T) {
	scheme := newTestScheme()
	jobName := "retry-deleting-job"
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:       jobName,
			Namespace:  defaultNS,
			Finalizers: []string{metav1.FinalizerDeleteDependents},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "retry-deleting", Namespace: defaultNS},
		Spec: corev1alpha1.TaskSpec{
			Type:        corev1alpha1.TaskTypeAI,
			RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 3},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:    corev1alpha1.TaskPhaseRunning,
			JobName:  jobName,
			Attempts: 1,
		},
	}
	r := newUnitReconciler(scheme, task, job)

	result, err := r.retryTask(context.Background(), task)
	if err != nil {
		t.Fatalf("retryTask() error = %v", err)
	}
	if result.RequeueAfter != 2*time.Second {
		t.Fatalf("RequeueAfter = %v, want 2s while foreground Job deletion is pending", result.RequeueAfter)
	}
	updated := &corev1alpha1.Task{}
	nn := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}
	if err := r.Get(context.Background(), nn, updated); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseRunning || updated.Status.JobName != jobName {
		t.Fatalf("status = %#v, want Running with old Job until foreground deletion completes", updated.Status)
	}
	deletingJob := &batchv1.Job{}
	jobNN := types.NamespacedName{Name: jobName, Namespace: task.Namespace}
	if err := r.Get(context.Background(), jobNN, deletingJob); err != nil {
		t.Fatalf("Get(deleting Job) error = %v", err)
	}
	if deletingJob.DeletionTimestamp.IsZero() {
		t.Fatal("old Job is not marked for foreground deletion")
	}

	deletingJob.Finalizers = nil
	if err := r.Update(context.Background(), deletingJob); err != nil {
		t.Fatalf("Update(Job finalizers) error = %v", err)
	}
	result, err = r.retryTask(context.Background(), updated)
	if err != nil {
		t.Fatalf("retryTask() after foreground deletion error = %v", err)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter == 2*time.Second {
		t.Fatalf("RequeueAfter = %v, want durable retry delay after cleanup", result.RequeueAfter)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhasePending || updated.Status.JobName != jobName {
		t.Fatalf("status = %#v, want Pending with old Job identity through backoff", updated.Status)
	}
}

func TestRetryTask_ForegroundDeleteRescansActiveOwnedPod(t *testing.T) {
	scheme := newTestScheme()
	jobName := "retry-foreground-active-job"
	controller := true
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: defaultNS}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            jobName + "-pod",
			Namespace:       defaultNS,
			Labels:          map[string]string{batchv1.JobNameLabel: jobName},
			OwnerReferences: []metav1.OwnerReference{{Kind: jobOwnerKind, Name: jobName, Controller: &controller}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "retry-foreground-active", Namespace: defaultNS},
		Spec: corev1alpha1.TaskSpec{
			Type:        corev1alpha1.TaskTypeAI,
			RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 3},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, JobName: jobName, Attempts: 1},
	}
	r := newUnitReconciler(scheme, task, job, pod)

	result, err := r.retryTask(context.Background(), task)
	if err != nil {
		t.Fatalf("retryTask() error = %v", err)
	}
	if result.RequeueAfter != 2*time.Second {
		t.Fatalf("RequeueAfter = %v, want 2s after foreground delete leaves active owned Pod", result.RequeueAfter)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseRunning || task.Status.JobName != jobName {
		t.Fatalf("status = %#v, want Running until active owned Pod is gone", task.Status)
	}
	if err := r.Get(context.Background(), types.NamespacedName{Name: jobName, Namespace: task.Namespace}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("old Job error = %v, want NotFound after foreground delete", err)
	}

	if err := r.Delete(context.Background(), pod); err != nil {
		t.Fatalf("Delete(Pod) error = %v", err)
	}
	result, err = r.retryTask(context.Background(), task)
	if err != nil {
		t.Fatalf("retryTask() after Pod deletion error = %v", err)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter == 2*time.Second {
		t.Fatalf("RequeueAfter = %v, want durable retry delay after Pod deletion", result.RequeueAfter)
	}
	if task.Status.Phase != corev1alpha1.TaskPhasePending || task.Status.JobName != jobName {
		t.Fatalf("status = %#v, want Pending with old Job identity through backoff", task.Status)
	}
}

func TestRetryTask_MissingJobWaitsForActiveOwnedPod(t *testing.T) {
	scheme := newTestScheme()
	jobName := "retry-missing-active-job"
	controller := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-pod",
			Namespace: defaultNS,
			Labels: map[string]string{
				batchv1.JobNameLabel: jobName,
			},
			OwnerReferences: []metav1.OwnerReference{{Kind: jobOwnerKind, Name: jobName, Controller: &controller}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "retry-missing-active", Namespace: defaultNS},
		Spec: corev1alpha1.TaskSpec{
			Type:        corev1alpha1.TaskTypeAI,
			RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 3},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, JobName: jobName, Attempts: 1},
	}
	r := newUnitReconciler(scheme, task, pod)

	result, err := r.retryTask(context.Background(), task)
	if err != nil {
		t.Fatalf("retryTask() error = %v", err)
	}
	if result.RequeueAfter != 2*time.Second {
		t.Fatalf("RequeueAfter = %v, want 2s while active owned Pod remains", result.RequeueAfter)
	}
	updated := &corev1alpha1.Task{}
	nn := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}
	if err := r.Get(context.Background(), nn, updated); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseRunning || updated.Status.JobName != jobName {
		t.Fatalf("status = %#v, want Running with missing Job identity while Pod remains", updated.Status)
	}

	if err := r.Delete(context.Background(), pod); err != nil {
		t.Fatalf("Delete(Pod) error = %v", err)
	}
	result, err = r.retryTask(context.Background(), updated)
	if err != nil {
		t.Fatalf("retryTask() after Pod deletion error = %v", err)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter == 2*time.Second {
		t.Fatalf("RequeueAfter = %v, want durable retry delay after active Pod deletion", result.RequeueAfter)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhasePending || updated.Status.JobName != jobName {
		t.Fatalf("status = %#v, want Pending with old Job identity through backoff", updated.Status)
	}
}

func TestRetryTask_MissingJobIgnoresUnownedAndTerminalPods(t *testing.T) {
	scheme := newTestScheme()
	jobName := "retry-missing-safe-pods-job"
	controller := true
	unownedRunning := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-unowned",
			Namespace: defaultNS,
			Labels:    map[string]string{batchv1.JobNameLabel: jobName},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	ownedTerminal := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            jobName + "-terminal",
			Namespace:       defaultNS,
			Labels:          map[string]string{batchv1.JobNameLabel: jobName},
			OwnerReferences: []metav1.OwnerReference{{Kind: jobOwnerKind, Name: jobName, Controller: &controller}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodFailed},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "retry-missing-safe-pods", Namespace: defaultNS},
		Spec: corev1alpha1.TaskSpec{
			Type:        corev1alpha1.TaskTypeAI,
			RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 3},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, JobName: jobName, Attempts: 1},
	}
	r := newUnitReconciler(scheme, task, unownedRunning, ownedTerminal)

	result, err := r.retryTask(context.Background(), task)
	if err != nil {
		t.Fatalf("retryTask() error = %v", err)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter == 2*time.Second {
		t.Fatalf("RequeueAfter = %v, want durable retry delay without an active owned Pod", result.RequeueAfter)
	}
	if task.Status.Phase != corev1alpha1.TaskPhasePending || task.Status.JobName != jobName {
		t.Fatalf("status = %#v, want Pending with old Job identity through backoff", task.Status)
	}
}

func TestRetryTask_ZeroRetryNotBeforeIsReplaced(t *testing.T) {
	scheme := newTestScheme()
	initialDelay := metav1.Duration{Duration: time.Minute}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "retry-zero-gate",
			Namespace: defaultNS,
			Annotations: map[string]string{
				labels.AnnotationRetryNotBefore: time.Time{}.Format(time.RFC3339Nano),
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			RetryPolicy: &corev1alpha1.RetryPolicy{
				MaxRetries:   3,
				InitialDelay: &initialDelay,
			},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:    corev1alpha1.TaskPhaseRunning,
			JobName:  testRetryMissingJobName,
			Attempts: 1,
		},
	}
	r := newUnitReconciler(scheme, task)
	startedAt := time.Now()

	result, err := r.retryTask(context.Background(), task)
	if err != nil {
		t.Fatalf("retryTask() error = %v", err)
	}
	if result.RequeueAfter < 59*time.Second {
		t.Fatalf("RequeueAfter = %v, want replacement full backoff", result.RequeueAfter)
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, updated); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	notBefore, err := time.Parse(time.RFC3339Nano, updated.Annotations[labels.AnnotationRetryNotBefore])
	if err != nil {
		t.Fatalf("retry-not-before parse error = %v", err)
	}
	if notBefore.IsZero() || notBefore.Before(startedAt.Add(59*time.Second)) {
		t.Fatalf("retry-not-before = %s, want repaired future timestamp", notBefore)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhasePending {
		t.Fatalf("phase = %s, want Pending after repairing zero timestamp", updated.Status.Phase)
	}
}

func TestRetryTask_RecoversAfterCleanupClaimStatusFailure(t *testing.T) {
	scheme := newTestScheme()
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "retry-status-error-job", Namespace: defaultNS}}
	job.Status.Failed = 1
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "retry-status-error",
			Namespace:  defaultNS,
			Finalizers: []string{labels.TaskFinalizer},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:        corev1alpha1.TaskTypeAI,
			RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 3},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:    corev1alpha1.TaskPhaseRunning,
			JobName:  job.Name,
			Attempts: 1,
		},
	}
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(task, job).
		Build()
	statusErr := errors.New("injected retry status update failure")
	failStatusOnce := true
	fc := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if subResourceName == testStatusSubresource && failStatusOnce {
				if _, ok := obj.(*corev1alpha1.Task); ok {
					failStatusOnce = false
					return statusErr
				}
			}
			return c.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	})
	r := newUnitReconciler(scheme)
	r.Client = fc
	r.JobBuilder = NewJobBuilder(fc)

	if _, err := r.retryTask(context.Background(), task); !errors.Is(err, statusErr) {
		t.Fatalf("first retryTask() error = %v, want %v", err, statusErr)
	}
	nn := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}
	intermediate := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), nn, intermediate); err != nil {
		t.Fatalf("Get(intermediate Task) error = %v", err)
	}
	if intermediate.Status.Phase != corev1alpha1.TaskPhaseRunning || intermediate.Status.JobName != job.Name {
		t.Fatalf("intermediate status = %#v, want recoverable Running state", intermediate.Status)
	}
	if intermediate.Annotations[labels.AnnotationRetryNotBefore] == "" {
		t.Fatal("retry-not-before gate was not persisted before status failure")
	}
	if err := r.Get(context.Background(), types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, &batchv1.Job{}); err != nil {
		t.Fatalf("old Job was deleted before the retry cleanup claim succeeded: %v", err)
	}

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: nn})
	if err != nil {
		t.Fatalf("recovery Reconcile() error = %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("recovery RequeueAfter = %v, want durable retry delay", result.RequeueAfter)
	}
	recovered := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), nn, recovered); err != nil {
		t.Fatalf("Get(recovered Task) error = %v", err)
	}
	if recovered.Status.Phase != corev1alpha1.TaskPhasePending || recovered.Status.JobName != job.Name {
		t.Fatalf("recovered status = %#v, want Pending with old Job identity", recovered.Status)
	}
}

func TestRetryTask_DoesNotRetryStaleAttemptState(t *testing.T) {
	scheme := newTestScheme()
	latest := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "retry-stale-attempt", Namespace: defaultNS, UID: "retry-stale-attempt-uid"},
		Spec: corev1alpha1.TaskSpec{
			Type:        corev1alpha1.TaskTypeAI,
			RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 3},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:    corev1alpha1.TaskPhaseRunning,
			JobName:  testCurrentJobName,
			Attempts: 2,
		},
	}
	currentJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: latest.Status.JobName, Namespace: latest.Namespace}}
	stale := latest.DeepCopy()
	stale.Status.JobName = "old-job"
	stale.Status.Attempts = 1
	r := newUnitReconciler(scheme, latest, currentJob)

	result, err := r.retryTask(context.Background(), stale)
	if err != nil {
		t.Fatalf("retryTask() error = %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("retryTask() result = %#v, want no retry for stale attempt state", result)
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: latest.Name, Namespace: latest.Namespace}, updated); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	if updated.Status.JobName != currentJob.Name || updated.Status.Attempts != 2 || updated.Status.Phase != corev1alpha1.TaskPhaseRunning {
		t.Fatalf("status = %#v, want current Running attempt unchanged", updated.Status)
	}
	if _, ok := updated.Annotations[labels.AnnotationRetryNotBefore]; ok {
		t.Fatal("stale attempt unexpectedly persisted a retry gate")
	}
	if err := r.Get(context.Background(), types.NamespacedName{Name: currentJob.Name, Namespace: currentJob.Namespace}, &batchv1.Job{}); err != nil {
		t.Fatalf("current Job was removed by stale retry: %v", err)
	}
}

func TestRetryTask_UsesAuthoritativeRetryPolicy(t *testing.T) {
	scheme := newTestScheme()
	authoritativeDelay := metav1.Duration{Duration: time.Minute}
	latest := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "retry-authoritative-policy", Namespace: defaultNS, UID: "retry-authoritative-policy-uid"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			RetryPolicy: &corev1alpha1.RetryPolicy{
				MaxRetries:   3,
				InitialDelay: &authoritativeDelay,
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, JobName: testRetryMissingJobName, Attempts: 1},
	}
	stale := latest.DeepCopy()
	staleDelay := metav1.Duration{Duration: time.Second}
	stale.Spec.RetryPolicy.InitialDelay = &staleDelay
	r := newUnitReconciler(scheme, latest)
	startedAt := time.Now()

	result, err := r.retryTask(context.Background(), stale)
	if err != nil {
		t.Fatalf("retryTask() error = %v", err)
	}
	if result.RequeueAfter < 59*time.Second {
		t.Fatalf("RequeueAfter = %v, want authoritative one-minute delay", result.RequeueAfter)
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: latest.Name, Namespace: latest.Namespace}, updated); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	notBefore, err := time.Parse(time.RFC3339Nano, updated.Annotations[labels.AnnotationRetryNotBefore])
	if err != nil {
		t.Fatalf("retry-not-before parse error = %v", err)
	}
	if notBefore.Before(startedAt.Add(59 * time.Second)) {
		t.Fatalf("retry-not-before = %s, want authoritative policy delay", notBefore)
	}
}

func TestRetryTask_ConflictRecomputesDeadlineFromLatestPolicy(t *testing.T) {
	scheme := newTestScheme()
	initialDelay := metav1.Duration{Duration: time.Second}
	authoritativeDelay := metav1.Duration{Duration: 2 * time.Minute}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "retry-deadline-policy-conflict",
			Namespace: defaultNS,
			UID:       "retry-deadline-policy-conflict-uid",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			RetryPolicy: &corev1alpha1.RetryPolicy{
				MaxRetries:   3,
				InitialDelay: &initialDelay,
			},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:    corev1alpha1.TaskPhaseRunning,
			JobName:  testRetryMissingJobName,
			Attempts: 1,
		},
	}
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(task).
		Build()
	injectedConflict := false
	fc := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*corev1alpha1.Task); ok && !injectedConflict {
				injectedConflict = true
				latest := &corev1alpha1.Task{}
				nn := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}
				if err := c.Get(ctx, nn, latest); err != nil {
					return err
				}
				latest.Spec.RetryPolicy.InitialDelay = &authoritativeDelay
				if err := c.Update(ctx, latest); err != nil {
					return err
				}
				return apierrors.NewConflict(
					schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: testTaskResource},
					obj.GetName(),
					errors.New("retry policy changed"),
				)
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	r := newUnitReconciler(scheme)
	r.Client = fc
	r.JobBuilder = NewJobBuilder(fc)
	startedAt := time.Now()

	if _, err := r.retryTask(context.Background(), task); err != nil {
		t.Fatalf("retryTask() error = %v", err)
	}
	if !injectedConflict {
		t.Fatal("retry deadline update did not reach the injected conflict")
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, updated); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	notBefore, err := time.Parse(time.RFC3339Nano, updated.Annotations[labels.AnnotationRetryNotBefore])
	if err != nil {
		t.Fatalf("retry-not-before parse error = %v", err)
	}
	if notBefore.Before(startedAt.Add(119 * time.Second)) {
		t.Fatalf("retry-not-before = %s, want recomputed authoritative two-minute delay", notBefore)
	}
}

func TestRetryTask_DoesNotRetryWhenAuthoritativePolicyRemoved(t *testing.T) {
	scheme := newTestScheme()
	latest := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "retry-policy-removed", Namespace: defaultNS, UID: "retry-policy-removed-uid"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, JobName: "current-job-policy-removed", Attempts: 1},
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: latest.Status.JobName, Namespace: latest.Namespace}}
	stale := latest.DeepCopy()
	stale.Spec.RetryPolicy = &corev1alpha1.RetryPolicy{MaxRetries: 3}
	r := newUnitReconciler(scheme, latest, job)

	result, err := r.retryTask(context.Background(), stale)
	if err != nil {
		t.Fatalf("retryTask() error = %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("retryTask() result = %#v, want no retry after policy removal", result)
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: latest.Name, Namespace: latest.Namespace}, updated); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseRunning || updated.Status.JobName != job.Name {
		t.Fatalf("status = %#v, want current attempt unchanged", updated.Status)
	}
	if _, ok := updated.Annotations[labels.AnnotationRetryNotBefore]; ok {
		t.Fatal("retry gate persisted after authoritative policy removal")
	}
	if err := r.Get(context.Background(), types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, &batchv1.Job{}); err != nil {
		t.Fatalf("current Job removed after policy removal: %v", err)
	}
}

func TestHandlePending_MalformedRetryNotBeforeRearmsBackoff(t *testing.T) {
	scheme := newTestScheme()
	initialDelay := metav1.Duration{Duration: time.Minute}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "retry-malformed-gate",
			Namespace:  defaultNS,
			UID:        "retry-malformed-gate-uid",
			Finalizers: []string{labels.TaskFinalizer},
			Annotations: map[string]string{
				labels.AnnotationRetryNotBefore: "not-a-timestamp",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   testBusyboxImage,
			Command: []string{testRetryGateCommand},
			RetryPolicy: &corev1alpha1.RetryPolicy{
				MaxRetries:   2,
				InitialDelay: &initialDelay,
			},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:    corev1alpha1.TaskPhasePending,
			Attempts: 1,
		},
	}
	r := newUnitReconciler(scheme, task)
	nn := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}
	startedAt := time.Now()

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: nn})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter < 59*time.Second {
		t.Fatalf("RequeueAfter = %v, want repaired full backoff", result.RequeueAfter)
	}

	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), nn, updated); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	raw := updated.Annotations[labels.AnnotationRetryNotBefore]
	notBefore, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("retry-not-before = %q, want RFC3339Nano: %v", raw, err)
	}
	if notBefore.Before(startedAt.Add(59 * time.Second)) {
		t.Fatalf("retry-not-before = %s, want at least one minute after repair start %s", notBefore, startedAt)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhasePending {
		t.Fatalf("phase = %s, want Pending", updated.Status.Phase)
	}
	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace(task.Namespace)); err != nil {
		t.Fatalf("List(Jobs) error = %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("Jobs = %d, want none while repaired gate is active", len(jobs.Items))
	}

	result, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: nn})
	if err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("second RequeueAfter = %v, want positive remaining delay", result.RequeueAfter)
	}
	unchanged := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), nn, unchanged); err != nil {
		t.Fatalf("second Get() error = %v", err)
	}
	if got := unchanged.Annotations[labels.AnnotationRetryNotBefore]; got != raw {
		t.Fatalf("retry-not-before changed from %q to %q on immediate reconcile", raw, got)
	}
}

func TestHandlePending_RepairConflictRecomputesDeadlineFromLatestPolicy(t *testing.T) {
	scheme := newTestScheme()
	initialDelay := metav1.Duration{Duration: time.Second}
	authoritativeDelay := metav1.Duration{Duration: 2 * time.Minute}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "retry-repair-policy-conflict",
			Namespace: defaultNS,
			UID:       "retry-repair-policy-conflict-uid",
			Annotations: map[string]string{
				labels.AnnotationRetryNotBefore: "not-a-timestamp",
				labels.AnnotationRetryJobUID:    "",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			RetryPolicy: &corev1alpha1.RetryPolicy{
				MaxRetries:   3,
				InitialDelay: &initialDelay,
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending, Attempts: 1},
	}
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(task).
		Build()
	injectedConflict := false
	fc := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*corev1alpha1.Task); ok && !injectedConflict {
				injectedConflict = true
				latest := &corev1alpha1.Task{}
				nn := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}
				if err := c.Get(ctx, nn, latest); err != nil {
					return err
				}
				latest.Spec.RetryPolicy.InitialDelay = &authoritativeDelay
				if err := c.Update(ctx, latest); err != nil {
					return err
				}
				return apierrors.NewConflict(
					schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: testTaskResource},
					obj.GetName(),
					errors.New("retry policy changed"),
				)
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	r := newUnitReconciler(scheme)
	r.Client = fc
	r.JobBuilder = NewJobBuilder(fc)
	startedAt := time.Now()

	result, handled, err := r.handleRetryNotBefore(context.Background(), task)
	if err != nil {
		t.Fatalf("handleRetryNotBefore() error = %v", err)
	}
	if !handled || result.RequeueAfter <= 0 {
		t.Fatalf("handleRetryNotBefore() = (%#v, %t), want repaired wait", result, handled)
	}
	if !injectedConflict {
		t.Fatal("retry deadline repair did not reach the injected conflict")
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, updated); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	notBefore, err := time.Parse(time.RFC3339Nano, updated.Annotations[labels.AnnotationRetryNotBefore])
	if err != nil {
		t.Fatalf("retry-not-before parse error = %v", err)
	}
	if notBefore.Before(startedAt.Add(119 * time.Second)) {
		t.Fatalf("retry-not-before = %s, want recomputed authoritative two-minute delay", notBefore)
	}
}

func TestHandlePending_ZeroRetryNotBeforeRearmsBackoff(t *testing.T) {
	scheme := newTestScheme()
	initialDelay := metav1.Duration{Duration: time.Minute}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "retry-zero-pending-gate",
			Namespace:  defaultNS,
			Finalizers: []string{labels.TaskFinalizer},
			Annotations: map[string]string{
				labels.AnnotationRetryNotBefore: time.Time{}.Format(time.RFC3339Nano),
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer,
			RetryPolicy: &corev1alpha1.RetryPolicy{
				MaxRetries:   2,
				InitialDelay: &initialDelay,
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending, Attempts: 1},
	}
	r := newUnitReconciler(scheme, task)
	nn := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}
	startedAt := time.Now()

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: nn})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter < 59*time.Second {
		t.Fatalf("RequeueAfter = %v, want repaired full backoff", result.RequeueAfter)
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), nn, updated); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	notBefore, err := time.Parse(time.RFC3339Nano, updated.Annotations[labels.AnnotationRetryNotBefore])
	if err != nil {
		t.Fatalf("retry-not-before parse error = %v", err)
	}
	if notBefore.IsZero() || notBefore.Before(startedAt.Add(59*time.Second)) {
		t.Fatalf("retry-not-before = %s, want repaired future timestamp", notBefore)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhasePending {
		t.Fatalf("phase = %s, want Pending", updated.Status.Phase)
	}
}

func TestHandlePending_UsesAPIReaderBeforeStartingWithoutGate(t *testing.T) {
	scheme := newTestScheme()
	stale := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "retry-stale-cache", Namespace: defaultNS, Finalizers: []string{labels.TaskFinalizer}},
		Spec: corev1alpha1.TaskSpec{
			Type:        corev1alpha1.TaskTypeContainer,
			Image:       testBusyboxImage,
			Command:     []string{testRetryGateCommand},
			RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 1},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending, Attempts: 1},
	}
	latest := stale.DeepCopy()
	latest.Status = corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, Attempts: 2, JobName: testCurrentJobName}
	r := newUnitReconciler(scheme, stale)
	r.APIReader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(latest).Build()

	result, err := r.handlePending(context.Background(), stale)
	if err != nil {
		t.Fatalf("handlePending() error = %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("handlePending() result = %#v, want no work for latest Running state", result)
	}
	if stale.Status.Phase != corev1alpha1.TaskPhaseRunning || stale.Status.JobName != testCurrentJobName {
		t.Fatalf("Task status = %#v, want latest APIReader state", stale.Status)
	}
	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace(stale.Namespace)); err != nil {
		t.Fatalf("List(Jobs) error = %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("Jobs = %d, want no stale-cache retry start", len(jobs.Items))
	}
}

func TestHandlePending_DoesNotStartWhenRetryPolicyWasRemoved(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "retry-pending-policy-removed",
			Namespace:  defaultNS,
			Finalizers: []string{labels.TaskFinalizer},
			Annotations: map[string]string{
				labels.AnnotationRetryNotBefore: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
			},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status: corev1alpha1.TaskStatus{
			Phase:    corev1alpha1.TaskPhasePending,
			JobName:  "removed-policy-job",
			Attempts: 1,
		},
	}
	r := newUnitReconciler(scheme, task)
	nn := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: nn})
	if err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if result.RequeueAfter != time.Second {
		t.Fatalf("first RequeueAfter = %v, want 1s terminal re-evaluation", result.RequeueAfter)
	}
	intermediate := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), nn, intermediate); err != nil {
		t.Fatalf("Get(intermediate Task) error = %v", err)
	}
	if intermediate.Status.Phase != corev1alpha1.TaskPhaseRunning || intermediate.Status.JobName != "removed-policy-job" {
		t.Fatalf("intermediate status = %#v, want Running missing-Job evaluation", intermediate.Status)
	}
	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace(task.Namespace)); err != nil {
		t.Fatalf("List(Jobs) error = %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("Jobs = %d, want none after retry policy removal", len(jobs.Items))
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: nn}); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	terminal := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), nn, terminal); err != nil {
		t.Fatalf("Get(terminal Task) error = %v", err)
	}
	if terminal.Status.Phase != corev1alpha1.TaskPhaseFailed {
		t.Fatalf("phase = %s, want Failed when retry policy no longer permits another attempt", terminal.Status.Phase)
	}
}

func TestHandlePending_DueRetryRechecksActiveOwnedPod(t *testing.T) {
	scheme := newTestScheme()
	jobName := "retry-due-active-job"
	jobUID := types.UID("retry-due-active-job-uid")
	controller := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            jobName + "-pod",
			Namespace:       defaultNS,
			Labels:          map[string]string{batchv1.JobNameLabel: jobName},
			OwnerReferences: []metav1.OwnerReference{{Kind: jobOwnerKind, Name: jobName, UID: jobUID, Controller: &controller}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "retry-due-active",
			Namespace:  defaultNS,
			UID:        "12345678-retry-due-active",
			Finalizers: []string{labels.TaskFinalizer},
			Annotations: map[string]string{
				labels.AnnotationRetryNotBefore: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano),
				labels.AnnotationRetryJobUID:    string(jobUID),
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:        corev1alpha1.TaskTypeContainer,
			Image:       testBusyboxImage,
			Command:     []string{testRetryGateCommand},
			RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 3},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending, JobName: jobName, Attempts: 1},
	}
	r := newUnitReconciler(scheme, task, pod)
	nn := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: nn})
	if err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if result.RequeueAfter < time.Second {
		t.Fatalf("first RequeueAfter = %v, want cleanup wait", result.RequeueAfter)
	}
	waiting := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), nn, waiting); err != nil {
		t.Fatalf("Get(waiting Task) error = %v", err)
	}
	if waiting.Status.Phase != corev1alpha1.TaskPhasePending || waiting.Status.JobName != jobName {
		t.Fatalf("waiting status = %#v, want Pending with old Job identity", waiting.Status)
	}
	if waiting.Annotations[labels.AnnotationRetryNotBefore] == "" {
		t.Fatal("retry gate cleared while active owned Pod remained")
	}

	if err := r.Delete(context.Background(), pod); err != nil {
		t.Fatalf("Delete(Pod) error = %v", err)
	}
	result, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: nn})
	if err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Fatalf("second RequeueAfter = %v, want normal running poll", result.RequeueAfter)
	}
	running := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), nn, running); err != nil {
		t.Fatalf("Get(running Task) error = %v", err)
	}
	if running.Status.Phase != corev1alpha1.TaskPhaseRunning || running.Status.JobName == "" || running.Status.JobName == jobName {
		t.Fatalf("running status = %#v, want replacement Job after cleanup", running.Status)
	}
	if _, ok := running.Annotations[labels.AnnotationRetryNotBefore]; ok {
		t.Fatal("retry gate remained after due cleanup and replacement creation")
	}
}

func TestHandlePending_StaleRetryNotBeforeClearsAndStarts(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "retry-stale-gate",
			Namespace:  defaultNS,
			UID:        "12345678-retry-stale-gate",
			Finalizers: []string{labels.TaskFinalizer},
			Annotations: map[string]string{
				labels.AnnotationRetryNotBefore: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   testBusyboxImage,
			Command: []string{testRetryGateCommand},
			RetryPolicy: &corev1alpha1.RetryPolicy{
				MaxRetries: 2,
			},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:    corev1alpha1.TaskPhasePending,
			Attempts: 1,
		},
	}
	r := newUnitReconciler(scheme, task)
	nn := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: nn})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Fatalf("RequeueAfter = %v, want normal running poll", result.RequeueAfter)
	}

	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), nn, updated); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseRunning {
		t.Fatalf("phase = %s, want Running after stale gate is cleared", updated.Status.Phase)
	}
	if updated.Status.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", updated.Status.Attempts)
	}
	if _, ok := updated.Annotations[labels.AnnotationRetryNotBefore]; ok {
		t.Fatal("stale retry-not-before annotation was not cleared")
	}
	if updated.Status.JobName == "" {
		t.Fatal("JobName is empty after stale gate became due")
	}
	if err := r.Get(context.Background(), types.NamespacedName{Name: updated.Status.JobName, Namespace: task.Namespace}, &batchv1.Job{}); err != nil {
		t.Fatalf("Get(Job) error = %v", err)
	}
}

func TestHandlePending_RetryNotBeforeUpdateErrorLeavesRecoverableJob(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "retry-gate-update-error",
			Namespace:  defaultNS,
			UID:        "12345678-retry-gate-update-error",
			Finalizers: []string{labels.TaskFinalizer},
			Annotations: map[string]string{
				labels.AnnotationRetryNotBefore: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   testBusyboxImage,
			Command: []string{testRetryGateCommand},
			RetryPolicy: &corev1alpha1.RetryPolicy{
				MaxRetries: 2,
			},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:    corev1alpha1.TaskPhasePending,
			Attempts: 1,
		},
	}
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(task).
		Build()
	updateErr := errors.New("injected retry gate update failure")
	failUpdateOnce := true
	fc := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*corev1alpha1.Task); ok && failUpdateOnce {
				failUpdateOnce = false
				return updateErr
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	r := newUnitReconciler(scheme)
	r.Client = fc
	r.JobBuilder = NewJobBuilder(fc)
	nn := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: nn}); !errors.Is(err, updateErr) {
		t.Fatalf("Reconcile() error = %v, want %v", err, updateErr)
	}

	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), nn, updated); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseRunning || updated.Status.Attempts != 2 || updated.Status.JobName == "" {
		t.Fatalf("status = %#v, want tracked Running replacement after gate update failure", updated.Status)
	}
	if updated.Annotations[labels.AnnotationRetryNotBefore] == "" {
		t.Fatal("retry-not-before annotation unexpectedly disappeared")
	}
	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace(task.Namespace)); err != nil {
		t.Fatalf("List(Jobs) error = %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("Jobs = %d, want one recoverable replacement after gate-clear failure", len(jobs.Items))
	}
	if jobs.Items[0].Spec.Suspend != nil && *jobs.Items[0].Spec.Suspend {
		t.Fatal("tracked replacement remained suspended after JobStart completed")
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: nn}); err != nil {
		t.Fatalf("recovery Reconcile() error = %v", err)
	}
	recovered := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), nn, recovered); err != nil {
		t.Fatalf("Get(recovered Task) error = %v", err)
	}
	if recovered.Status.Phase != corev1alpha1.TaskPhaseRunning || recovered.Status.JobName != jobs.Items[0].Name {
		t.Fatalf("recovered status = %#v, want Running with existing replacement Job", recovered.Status)
	}
	if _, ok := recovered.Annotations[labels.AnnotationRetryNotBefore]; ok {
		t.Fatal("retry gate remained after recovery")
	}
}

func TestHandlePending_DeletesSuspendedReplacementAfterRetryBecomesDisallowed(t *testing.T) {
	tests := []struct {
		name         string
		mutatePolicy func(*corev1alpha1.Task)
	}{
		{
			name: "policy removed",
			mutatePolicy: func(task *corev1alpha1.Task) {
				task.Spec.RetryPolicy = nil
			},
		},
		{
			name: "retry budget exhausted",
			mutatePolicy: func(task *corev1alpha1.Task) {
				task.Spec.RetryPolicy = &corev1alpha1.RetryPolicy{MaxRetries: 0}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newTestScheme()
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "retry-adopt-disallowed-" + strings.ReplaceAll(tt.name, " ", "-"),
					Namespace:  defaultNS,
					UID:        "retry-adopt-disallowed-uid",
					Finalizers: []string{labels.TaskFinalizer},
					Annotations: map[string]string{
						labels.AnnotationRetryNotBefore: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
						labels.AnnotationRetryJobUID:    "",
					},
				},
				Spec: corev1alpha1.TaskSpec{
					Type:        corev1alpha1.TaskTypeContainer,
					Image:       testBusyboxImage,
					Command:     []string{testRetryGateCommand},
					RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 2},
				},
				Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending, Attempts: 1},
			}
			meta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
				Type:   ConditionTypeRetryCleanup,
				Status: metav1.ConditionFalse,
				Reason: retryCleanupCompleteReason,
			})
			replacementJob := retryTestJob(task, buildTaskJobName(task), "retry-adopt-disallowed-job-uid")
			suspend := true
			replacementJob.Spec.Suspend = &suspend
			replacementJob.Annotations = map[string]string{
				labels.AnnotationRetryTaskGeneration: "0",
				labels.AnnotationRetryAttempt:        "2",
			}
			tt.mutatePolicy(task)
			r := newUnitReconciler(scheme, task, replacementJob)
			nn := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}

			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: nn}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			recovered := &corev1alpha1.Task{}
			if err := r.Get(context.Background(), nn, recovered); err != nil {
				t.Fatalf("Get(recovered Task) error = %v", err)
			}
			if recovered.Status.Phase != corev1alpha1.TaskPhasePending || recovered.Status.JobName != "" || recovered.Status.Attempts != 1 {
				t.Fatalf("recovered status = %#v, want disallowed suspended replacement rejected", recovered.Status)
			}
			if _, exists := recovered.Annotations[labels.AnnotationRetryNotBefore]; !exists {
				t.Fatal("retry gate was cleared before disallowed retry recovery completed")
			}
			if err := r.Get(context.Background(), types.NamespacedName{Name: replacementJob.Name, Namespace: replacementJob.Namespace}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
				t.Fatalf("disallowed suspended replacement still exists: %v", err)
			}
		})
	}
}

func TestHandlePending_AdoptsReplacementWithoutGateBeforeAdmissionChecks(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "retry-adopt-without-gate",
			Namespace:  defaultNS,
			UID:        "retry-adopt-without-gate-uid",
			Finalizers: []string{labels.TaskFinalizer},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:        corev1alpha1.TaskTypeContainer,
			Image:       testBusyboxImage,
			Command:     []string{testRetryGateCommand},
			RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 1},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending, Attempts: 1},
	}
	meta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
		Type:   ConditionTypeRetryCleanup,
		Status: metav1.ConditionFalse,
		Reason: retryCleanupCompleteReason,
	})
	replacementJob := retryTestJob(task, buildTaskJobName(task), "retry-adopt-without-gate-job-uid")
	otherActiveTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "other-active-task", Namespace: defaultNS, UID: "other-active-task-uid"},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	r := newUnitReconciler(scheme, task, replacementJob, otherActiveTask)
	r.MaxTasksPerNamespace = 1
	nn := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: nn}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), nn, updated); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseRunning ||
		updated.Status.JobName != replacementJob.Name ||
		updated.Status.Attempts != 2 {
		t.Fatalf("status = %#v, want existing replacement adopted before namespace admission limit", updated.Status)
	}
}

func TestHandlePending_RetryNotBeforeConflictIsRetried(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "retry-gate-conflict",
			Namespace:  defaultNS,
			UID:        "12345678-retry-gate-conflict",
			Finalizers: []string{labels.TaskFinalizer},
			Annotations: map[string]string{
				labels.AnnotationRetryNotBefore: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   testBusyboxImage,
			Command: []string{testRetryGateCommand},
			RetryPolicy: &corev1alpha1.RetryPolicy{
				MaxRetries: 2,
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending, Attempts: 1},
	}
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(task).
		Build()
	updateCalls := 0
	fc := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*corev1alpha1.Task); ok {
				updateCalls++
				if updateCalls == 1 {
					return apierrors.NewConflict(schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: testTaskResource}, obj.GetName(), errors.New("injected conflict"))
				}
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	r := newUnitReconciler(scheme)
	r.Client = fc
	r.JobBuilder = NewJobBuilder(fc)
	nn := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: nn}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if updateCalls < 2 {
		t.Fatalf("Task update calls = %d, want conflict retry", updateCalls)
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), nn, updated); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseRunning {
		t.Fatalf("phase = %s, want Running after conflict retry", updated.Status.Phase)
	}
	if _, ok := updated.Annotations[labels.AnnotationRetryNotBefore]; ok {
		t.Fatal("retry-not-before annotation was not cleared after conflict retry")
	}
}

func TestReconcile_RunningPhase_CacheMissDoesNotRetryHealthyAPIJob(t *testing.T) {
	scheme := newTestScheme()
	started := metav1.NewTime(time.Now().Add(-time.Minute))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "rec-run-cache-miss",
			Namespace:  defaultNS,
			UID:        "rec-run-cache-miss-uid",
			Finalizers: []string{labels.TaskFinalizer},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:        corev1alpha1.TaskTypeContainer,
			RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 2},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseRunning,
			JobName:   "healthy-api-job",
			Attempts:  1,
			StartTime: &started,
		},
	}
	healthyJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: task.Status.JobName, Namespace: task.Namespace}}
	r := newUnitReconciler(scheme, task)
	r.APIReader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(task.DeepCopy(), healthyJob).Build()

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: task.Name, Namespace: task.Namespace},
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 2*time.Second {
		t.Fatalf("RequeueAfter = %v, want 2s cache visibility retry", result.RequeueAfter)
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, updated); err != nil {
		t.Fatalf("Get(Task) error = %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseRunning || updated.Status.JobName != healthyJob.Name || updated.Status.Attempts != 1 {
		t.Fatalf("status = %#v, want healthy Running attempt unchanged", updated.Status)
	}
	if _, ok := updated.Annotations[labels.AnnotationRetryNotBefore]; ok {
		t.Fatal("cache miss unexpectedly persisted a retry gate")
	}
	if err := r.APIReader.Get(context.Background(), types.NamespacedName{Name: healthyJob.Name, Namespace: healthyJob.Namespace}, &batchv1.Job{}); err != nil {
		t.Fatalf("healthy API Job was removed: %v", err)
	}
}
