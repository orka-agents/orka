package controller

import (
	"context"
	"errors"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

var errTaskJobIdentity = errors.New("task Job identity cannot be verified")

const taskJobIdentityRejectedReason = "JobIdentityRejected"

func taskJobIdentityRejected(task *corev1alpha1.Task) bool {
	condition := meta.FindStatusCondition(task.Status.Conditions, ConditionTypeJobCreated)
	return condition != nil && condition.Status == metav1.ConditionFalse && condition.Reason == taskJobIdentityRejectedReason
}

func taskJobAuthorityChanged(previous store.TaskJobIdentity, jobName string, task *corev1alpha1.Task) bool {
	if previous.JobUID == "" {
		return false
	}
	if previous.TaskUID != string(task.UID) || previous.JobUID != task.Status.JobUID || jobName != task.Status.JobName {
		return true
	}
	return taskDataAuthorityEnded(task)
}

func taskDataAuthorityEnded(task *corev1alpha1.Task) bool {
	if !task.DeletionTimestamp.IsZero() || task.Status.ExecutionOutcome != nil {
		return true
	}
	switch task.Status.Phase {
	case "", corev1alpha1.TaskPhasePending, corev1alpha1.TaskPhaseRunning, corev1alpha1.TaskPhaseFinalizing:
		return false
	default:
		return true
	}
}

func revokeTaskJobAuthority(ctx context.Context, resultStore store.ResultStore, identity store.TaskJobIdentity) error {
	if identity.JobUID == "" {
		return nil
	}
	authority, ok := resultStore.(store.TaskJobAuthorityStore)
	if !ok {
		return errors.New("task Job authority store unavailable")
	}
	return authority.RevokeTaskJob(ctx, identity)
}

// recoverTaskJob only adopts a Job whose UID was already recorded by the
// controller. An unbound Job may have executed before a crash, so it must not
// be adopted or automatically replaced. Names and owner references alone are
// forgeable by namespace Job creators.
func (r *TaskReconciler) recoverTaskJob(ctx context.Context, task *corev1alpha1.Task, expected *batchv1.Job, validationTask bool) (*batchv1.Job, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	existing := &batchv1.Job{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(expected), existing); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s/%s disappeared during recovery", errTaskJobIdentity, expected.Namespace, expected.Name)
		}
		return nil, err
	}
	if validationTask {
		if err := validateRepositoryMonitorValidationJobAgainstExpected(task, existing, expected); err != nil {
			return nil, err
		}
	}
	if !metav1.IsControlledBy(existing, task) {
		return nil, fmt.Errorf("%w: %s/%s has a different controller owner", errTaskJobIdentity, existing.Namespace, existing.Name)
	}
	if !existing.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("%w: %s/%s is being deleted", errTaskJobIdentity, existing.Namespace, existing.Name)
	}
	if task.Status.JobName == existing.Name && task.Status.JobUID != "" && task.Status.JobUID == string(existing.UID) {
		return existing, nil
	}
	identityErr := fmt.Errorf("%w: %s/%s has no matching recorded UID", errTaskJobIdentity, existing.Namespace, existing.Name)
	if err := r.recordTaskJobIdentityRejection(ctx, task, existing, identityErr.Error()); err != nil {
		return nil, err
	}
	if err := r.deleteRejectedTaskJob(ctx, existing); err != nil {
		return nil, err
	}
	return nil, identityErr
}

func (r *TaskReconciler) recordTaskJobIdentityRejection(ctx context.Context, task *corev1alpha1.Task, job *batchv1.Job, message string) error {
	taskUID, jobName, jobUID := task.UID, task.Status.JobName, task.Status.JobUID
	recorded := false
	if err := r.updateStatusWithRetry(ctx, task, func(current *corev1alpha1.Task) {
		recorded = false
		if current.UID != taskUID || !canStartTaskJob(current.Status.Phase) || current.Status.ExecutionOutcome != nil ||
			current.Status.JobName != jobName || current.Status.JobUID != jobUID {
			return
		}
		// Persist the cleanup name and replay barrier before deleting the Job.
		// Its unverified UID must never become worker authorization.
		current.Status.JobName = job.Name
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type: ConditionTypeJobCreated, Status: metav1.ConditionFalse, Reason: taskJobIdentityRejectedReason,
			Message: message, LastTransitionTime: metav1.Now(),
		})
		recorded = true
	}); err != nil {
		return err
	}
	if !recorded {
		return errors.New("task changed while rejecting Job identity")
	}
	return nil
}

func (r *TaskReconciler) retireRejectedTaskJob(ctx context.Context, task *corev1alpha1.Task) error {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	job := &batchv1.Job{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: task.Status.JobName}, job); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(job, task) {
		return nil
	}
	return r.deleteRejectedTaskJob(ctx, job)
}

func (r *TaskReconciler) deleteRejectedTaskJob(ctx context.Context, job *batchv1.Job) error {
	if job.UID == "" {
		return errors.New("cannot retire a Job without its UID")
	}
	if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationForeground), client.Preconditions{
		UID: &job.UID, ResourceVersion: &job.ResourceVersion,
	}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("retire rejected Task Job: %w", err)
	}
	return nil
}
