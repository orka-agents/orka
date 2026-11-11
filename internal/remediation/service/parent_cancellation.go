package service

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/remediation/contract"
)

// Publish revocation on the exact inert parent before persisting cancellation.
// A backend recovering before the coordinator starts must see the cancellation
// without relying on process-local driver state.
func (s *Coordinator) markParentCancelled(ctx context.Context, r *run) error {
	if r.state.Parent.UID == "" {
		return nil
	}
	if err := s.requireCurrentRun(ctx, r); err != nil {
		return err
	}
	task := new(corev1alpha1.Task)
	if err := s.config.Reader.Get(ctx, client.ObjectKey{Namespace: r.state.Namespace, Name: r.state.Parent.Name}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return contract.ErrUnavailable
	}
	if string(task.UID) != r.state.Parent.UID {
		return contract.ErrConflict
	}
	if task.Annotations[labels.AnnotationCancelRequested] != "" || task.DeletionTimestamp != nil {
		return nil
	}
	original := task.DeepCopy()
	if task.Annotations == nil {
		task.Annotations = make(map[string]string)
	}
	task.Annotations[labels.AnnotationCancelRequested] = "remediation run cancelled"
	if err := s.config.Client.Patch(ctx, task, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
		return contract.ErrUnavailable
	}
	return nil
}
