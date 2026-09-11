package controller

import (
	"context"
	"errors"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

var errTaskJobIdentity = errors.New("task Job identity cannot be verified")

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
	return nil, fmt.Errorf("%w: %s/%s has no matching recorded UID", errTaskJobIdentity, existing.Namespace, existing.Name)
}
