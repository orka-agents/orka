/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/artifactcap"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/tools"
)

// Repair validation (ADR 0038) gates a repository monitor repair's patch
// before the clean-room publisher pushes it.
const (
	// repositoryMonitorRepairValidationFailedDeliveryReason is the repair
	// Task's status.delivery.reason when its patch ran the operator's
	// repairCommand and the command failed. Nothing was pushed. The
	// status.delivery.message carries bounded, redacted evidence only.
	repositoryMonitorRepairValidationFailedDeliveryReason = "RepairValidationFailed"
	// repositoryMonitorRepairValidationUnavailableDeliveryReason is the repair
	// Task's status.delivery.reason when the gate could not run the command
	// (for example an image pull failure or a rejected patch). Nothing was
	// pushed; the gate failed closed.
	repositoryMonitorRepairValidationUnavailableDeliveryReason = "RepairValidationUnavailable"

	// repositoryMonitorRepairValidationPatchContainer applies the repair patch
	// inside the validation Pod, after the checkout and before the network
	// gate.
	repositoryMonitorRepairValidationPatchContainer  = "apply-repair-patch"
	repositoryMonitorRepairValidationPatchVolume     = "repair-patch-capability"
	repositoryMonitorRepairValidationPatchMount      = "/var/run/orka/repair-patch"
	repositoryMonitorRepairValidationPatchWorkerMode = "--apply-repository-repair-patch"
)

// isRepositoryMonitorRepairValidationTask reports whether task is a
// controller-shaped repair validation Task. Callers that grant execution must
// still verify its durable binding.
func isRepositoryMonitorRepairValidationTask(task *corev1alpha1.Task) bool {
	return isRepositoryMonitorValidationTask(task) &&
		task.Annotations[labels.AnnotationRepositoryValidationKind] == tools.RepositoryValidationKindRepair &&
		strings.TrimSpace(task.Annotations[labels.AnnotationRepositoryValidationPatchDigest]) != ""
}

// retireRepositoryMonitorRepairValidationIdentity retires a deleted repair
// validation Task's own patch-download identity. The repair's finalizer also
// retires the identities of its live validation Tasks, but one deleted before
// the repair settles is no longer listed there, and its download record would
// otherwise keep the patch live indefinitely. Retirement only blocks that
// Task's own capabilities and is idempotent, so retiring it twice is harmless.
func (r *TaskReconciler) retireRepositoryMonitorRepairValidationIdentity(ctx context.Context, task *corev1alpha1.Task) error {
	if r.ACPArtifactRetirer == nil || task == nil || task.UID == "" || !isRepositoryMonitorRepairValidationTask(task) {
		return nil
	}
	if err := r.ACPArtifactRetirer.Retire(ctx, artifactcap.Identity{Namespace: task.Namespace, TaskID: string(task.UID)}); err != nil {
		return fmt.Errorf("retire repair validation patch identity: %w", err)
	}
	return nil
}

// verifyRepositoryMonitorRepairValidationTask proves a repair validation Task
// still matches the binding persisted before the controller created it: the
// exact repair Task incarnation, RepositoryMonitor, image, head, command, and
// patch digest. The live monitor must still require the bound image and
// command, so a changed check is never released under the old one.
func (r *TaskReconciler) verifyRepositoryMonitorRepairValidationTask(ctx context.Context, task *corev1alpha1.Task, binding *tools.RepositoryValidationCommandBinding) error {
	if binding == nil || binding.Kind != tools.RepositoryValidationKindRepair || !isRepositoryMonitorRepairValidationTask(task) {
		return repositoryMonitorValidationConfinementErrorf("repair validation provenance kind does not match the Task")
	}
	reader := r.validationResourceReader()
	monitor := &corev1alpha1.RepositoryMonitor{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: binding.MonitorNamespace, Name: binding.MonitorName}, monitor); err != nil {
		if apierrors.IsNotFound(err) {
			return repositoryMonitorValidationConfinementErrorf("bound RepositoryMonitor is missing")
		}
		return fmt.Errorf("load bound RepositoryMonitor: %w", err)
	}
	if string(monitor.UID) != binding.MonitorUID {
		return repositoryMonitorValidationConfinementErrorf("bound RepositoryMonitor identity changed")
	}
	repair := &corev1alpha1.Task{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: binding.MonitorNamespace, Name: binding.ReviewTaskName}, repair); err != nil {
		if apierrors.IsNotFound(err) {
			return repositoryMonitorValidationConfinementErrorf("bound repair Task is missing")
		}
		return fmt.Errorf("load bound repair Task: %w", err)
	}
	if string(repair.UID) != binding.ReviewTaskUID || !metav1.IsControlledBy(repair, monitor) ||
		!binding.MatchesRepair(repair, monitor, binding.Image) ||
		strings.TrimSpace(monitor.Spec.Validation.Image) != binding.Image ||
		!binding.MatchesCommand(monitor.Spec.Validation.RepairCommand) ||
		task.Annotations[labels.AnnotationRepositoryValidationPatchDigest] != binding.PatchDigest {
		return repositoryMonitorValidationConfinementErrorf("immutable repair validation provenance does not match its repair Task")
	}
	commandSecret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{
		Namespace: task.Namespace, Name: tools.RepositoryValidationCommandSecretName(task.Name),
	}, commandSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return repositoryMonitorValidationConfinementErrorf("immutable validation command Secret is missing")
		}
		return fmt.Errorf("load immutable validation command Secret: %w", err)
	}
	if err := tools.ValidateRepositoryValidationCommandSecret(repair, task, commandSecret, binding); err != nil {
		return repositoryMonitorValidationConfinementErrorf("immutable validation command Secret does not match its binding")
	}
	input := tools.RepositoryRepairValidationInput{
		Monitor: monitor, RepairTask: repair, Image: binding.Image,
		Command:     string(commandSecret.Data[tools.RepositoryValidationCommandSecretKey]),
		DeltaDigest: binding.PatchDigest, RelativeRoot: repair.Spec.Workspace.SubPath,
	}
	if !tools.RepositoryRepairValidationTaskMatches(task, input) {
		return repositoryMonitorValidationConfinementErrorf("repair validation Task no longer matches immutable provenance")
	}
	return nil
}
