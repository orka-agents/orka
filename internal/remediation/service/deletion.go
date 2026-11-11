package service

import (
	"context"
	"encoding/json"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/remediation/contract"
)

func (s *Coordinator) reconcileDeletingTask(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, bool, error) {
	current := new(corev1alpha1.Task)
	if err := s.config.Reader.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, true, nil
		}
		return ctrl.Result{}, true, contract.ErrUnavailable
	}
	if current.UID == "" || current.UID != task.UID || current.DeletionTimestamp == nil {
		return ctrl.Result{}, true, contract.ErrConflict
	}
	if err := s.checkDeletingJob(ctx, current); err != nil {
		return ctrl.Result{}, true, err
	}
	if err := s.recordDeletingTask(ctx, current); err != nil {
		return ctrl.Result{}, true, err
	}
	// The ordinary controller owns finalizer cleanup. Use the authoritative
	// deleting UID/status, never a stale cached Job name or a live orphan.
	*task = *current
	return ctrl.Result{}, false, nil
}

func (s *Coordinator) checkDeletingJob(ctx context.Context, task *corev1alpha1.Task) error {
	if task.Status.JobName == "" {
		return nil
	}
	job := new(batchv1.Job)
	if err := s.config.Reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: task.Status.JobName}, job); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return contract.ErrUnavailable
	}
	owner := metav1.GetControllerOf(job)
	if job.UID == "" || (task.Status.JobUID != "" && string(job.UID) != task.Status.JobUID) ||
		owner == nil || owner.UID != task.UID || owner.Name != task.Name || owner.Kind != taskKind ||
		owner.APIVersion != corev1alpha1.GroupVersion.String() {
		return contract.ErrConflict
	}
	return nil
}

func (s *Coordinator) recordDeletingTask(ctx context.Context, task *corev1alpha1.Task) error {
	object := new(corev1.ConfigMap)
	if err := s.config.Reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: task.Labels[runLabel]}, object); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return contract.ErrUnavailable
	}
	if object.DeletionTimestamp != nil {
		return nil
	}
	var saved state
	if json.Unmarshal([]byte(object.Data["state"]), &saved) != nil {
		return contract.ErrConflict
	}
	r, err := decodeRun(object, saved.ID, task.Namespace)
	if err != nil {
		return err
	}
	role := task.Labels[roleLabel]
	_, err = s.update(ctx, r.state.ID, func(current *run) error {
		op := current.state.Agents[role]
		if role == parentRole {
			op = &current.state.Parent
		}
		if op == nil || !op.Attempted || op.Name != task.Name || (op.UID != "" && op.UID != string(task.UID)) {
			return contract.ErrConflict
		}
		if op.UID == "" {
			expected := new(corev1alpha1.Task)
			if err := s.readJSON(ctx, current, op.Spec, expected); err != nil {
				return err
			}
			if !taskIdentityMatches(task, expected, "") {
				return contract.ErrConflict
			}
			op.UID = string(task.UID)
		}
		if role == parentRole {
			current.state.ParentDeleted = true
		}
		if (role == parentRole || op.Output.empty()) && !contract.TerminalPhase(current.state.Status.Phase) {
			current.state.CancelRequested, current.state.StopReason = true, reasonCancelled
		}
		return nil
	})
	return err
}
