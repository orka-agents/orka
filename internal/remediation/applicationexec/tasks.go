package applicationexec

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	taskCreateRejected  = "task-create-rejected"
	taskNeverDispatched = "task-never-dispatched"
)

func anyTaskRejected(r *run) bool {
	if r.state.Task.Rejected {
		return true
	}
	for _, receipt := range r.state.ArmTasks {
		if receipt.Rejected {
			return true
		}
	}
	return false
}

func (b *Backend) settleRejectedRootTask(ctx context.Context, r *run) error {
	return b.settleRootWithoutExecution(ctx, r, true)
}

func (b *Backend) settleRootWithoutExecution(ctx context.Context, r *run, rejected bool) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.config.CleanupTimeout)
	defer cancel()
	if err := b.fresh(ctx, r); err != nil {
		return err
	}
	if r.state.Task.UID != "" || r.state.Task.Rejected != rejected || r.state.Task.Attempted != rejected ||
		len(r.state.ArmTasks) != 0 || len(r.state.Effects) != 0 || len(r.state.Pods) != 0 ||
		len(r.state.Actions) != 0 || len(r.state.Credentials) != 0 || len(r.state.Volumes) != 0 ||
		len(r.state.ReceiverRoutes) != 0 || len(r.state.APIBaselines) != 0 ||
		r.state.Binding != nil || r.state.Manifest != nil || r.state.RecordCreated {
		return contract.ErrConflict
	}
	if r.state.Settled {
		return nil
	}
	r.state.Failed, r.state.Settled = taskCreateRejected, true
	if !rejected {
		r.state.Cancelled, r.state.Failed = true, taskNeverDispatched
	}
	// This CAS races the dispatch-intent CAS; a winning Attempted write
	// cannot be overwritten as an abandoned reservation.
	return b.save(ctx, r)
}

func (b *Backend) task(r *run, side string) *corev1alpha1.Task {
	name := r.state.Task.Name
	var owner *metav1.OwnerReference
	if side != "" {
		name += "-" + side
		owner = &metav1.OwnerReference{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task",
			Name: r.state.Task.Name, UID: r.state.Task.UID, Controller: new(true), BlockOwnerDeletion: new(true)}
	} else if !r.state.Qualification {
		owner = &metav1.OwnerReference{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task",
			Name: r.state.Request.ParentTaskName, UID: types.UID(r.state.Request.ParentTaskUID),
			Controller: new(true), BlockOwnerDeletion: new(true)}
	}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.state.Request.Namespace,
		Finalizers: []string{cleanupFinalizer},
		Annotations: map[string]string{pv.ValidationBackendKey: backendName, inputAnnotation: r.state.InputDigest,
			ledgerAnnotation: r.object.Name, "applicationexec.orka.ai/arm": side},
		Labels: map[string]string{runLabel: string(r.object.UID)}},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer, Image: b.config.WorkerImage,
			Command: []string{pv.ApplicationWorkerPath}, Args: []string{"not-a-dispatch-entrypoint"},
			Priority: new(int32(500)), ConcurrencyPolicy: corev1alpha1.ForbidConcurrent,
			StartingDeadlineSeconds: new(int64(100)), SuccessfulRunsHistoryLimit: new(int32(3)), FailedRunsHistoryLimit: new(int32(1)),
			RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 0, BackoffMultiplier: 2}}}
	if owner != nil {
		task.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	return task
}

func (b *Backend) ensureTask(ctx context.Context, r *run, side string) error {
	// Settlement may have fenced this reservation since the caller loaded it.
	if err := b.fresh(ctx, r); err != nil {
		return err
	}
	if r.state.Cancelled || r.state.Finished || r.state.Failed != "" || r.state.Settled ||
		!r.object.DeletionTimestamp.IsZero() || !time.Now().Before(r.state.Deadline) {
		return contract.ErrConflict
	}
	entry := r.state.Task
	if side != "" {
		entry = r.state.ArmTasks[side]
		if entry.Name == "" {
			entry.Name = r.state.Task.Name + "-" + side
		}
	}
	expected := b.task(r, side)
	if entry.UID != "" {
		return b.checkOneTask(ctx, r, side, false)
	}
	if entry.Rejected {
		return contract.ErrConflict
	}
	if entry.Attempted {
		return ErrUncertain
	}
	entry.Attempted = true
	if side == "" {
		r.state.Task = entry
	} else {
		r.state.ArmTasks[side] = entry
	}
	if err := b.save(ctx, r); err != nil {
		return err
	}
	if err := b.control.Create(ctx, expected); err != nil {
		if definitivelyRejectedCreate(err) {
			if recordErr := b.recordTaskResult(ctx, r, side, entry, "", true); recordErr != nil {
				return errors.Join(safeError(ErrUncertain, err), recordErr)
			}
			if side == "" {
				return errors.Join(safeError(contract.ErrConflict, err), b.settleRejectedRootTask(ctx, r))
			}
			return safeError(contract.ErrConflict, err)
		}
		return safeError(ErrUncertain, err)
	}
	if expected.UID == "" {
		return ErrUncertain
	}
	if err := b.recordTaskResult(ctx, r, side, entry, expected.UID, false); err != nil {
		return err
	}
	return b.checkOneTask(ctx, r, side, false)
}

func (b *Backend) recordTaskResult(ctx context.Context, r *run, side string, entry taskReceipt, uid types.UID, rejected bool) error {
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.config.CleanupTimeout)
	defer cancel()
	for range 4 {
		current, err := b.load(recordCtx, r.object.Name)
		if err != nil {
			return err
		}
		if current.object.UID != r.object.UID || current.state.InputDigest != r.state.InputDigest {
			return contract.ErrConflict
		}
		currentEntry := current.state.Task
		if side != "" {
			currentEntry = current.state.ArmTasks[side]
		}
		if !currentEntry.Attempted || currentEntry.Name != entry.Name ||
			(currentEntry.UID != "" && currentEntry.UID != uid) ||
			(currentEntry.Rejected && !rejected) || (rejected && uid != "") {
			return contract.ErrConflict
		}
		currentEntry.UID, currentEntry.Rejected = uid, rejected
		if side == "" {
			current.state.Task = currentEntry
		} else {
			current.state.ArmTasks[side] = currentEntry
		}
		if err := b.save(recordCtx, current); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return err
		}
		*r = *current
		return nil
	}
	return contract.ErrUnavailable
}

func (b *Backend) checkTask(ctx context.Context, r *run, cleanup bool) error {
	if err := b.checkOneTask(ctx, r, "", cleanup); err != nil {
		return err
	}
	for side := range r.state.ArmTasks {
		if err := b.checkOneTask(ctx, r, side, cleanup); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) checkOneTask(ctx context.Context, r *run, side string, cleanup bool) error {
	entry := r.state.Task
	if side != "" {
		entry = r.state.ArmTasks[side]
	}
	if entry.UID == "" {
		if entry.Rejected && cleanup {
			return nil
		}
		if entry.Rejected {
			return contract.ErrConflict
		}
		return ErrUncertain
	}
	current := &corev1alpha1.Task{}
	if err := b.controlReader.Get(ctx, client.ObjectKey{Namespace: r.state.Request.Namespace, Name: entry.Name}, current); err != nil {
		if apierrors.IsNotFound(err) && cleanup {
			return nil
		}
		return safeError(contract.ErrUnavailable, err)
	}
	if current.UID != entry.UID || current.Annotations[pv.ValidationBackendKey] != backendName ||
		current.Annotations[inputAnnotation] != r.state.InputDigest || current.Annotations[ledgerAnnotation] != r.object.Name ||
		current.Labels[runLabel] != string(r.object.UID) {
		return contract.ErrConflict
	}
	if !cleanup {
		expected := b.task(r, side)
		if _, cancelled := current.Annotations[labels.AnnotationCancelRequested]; cancelled {
			return contract.ErrConflict
		}
		if !current.DeletionTimestamp.IsZero() || current.Status.Phase == corev1alpha1.TaskPhaseCancelled ||
			!reflect.DeepEqual(expected.OwnerReferences, current.OwnerReferences) ||
			!reflect.DeepEqual(expected.Spec, current.Spec) ||
			(!r.state.Finished && !r.state.Settled && !controllerutil.ContainsFinalizer(current, cleanupFinalizer)) {
			return contract.ErrConflict
		}
	}
	return nil
}

func (b *Backend) finishTasks(ctx context.Context, r *run) error {
	for _, side := range append(pv.ActionSides(r.state.Request.Action), "") {
		entry := r.state.Task
		if side != "" {
			var ok bool
			entry, ok = r.state.ArmTasks[side]
			if !ok {
				continue
			}
		}
		if entry.UID == "" {
			if entry.Rejected {
				continue
			}
			return ErrUncertain
		}
		task := &corev1alpha1.Task{}
		if err := b.controlReader.Get(ctx, client.ObjectKey{Namespace: r.state.Request.Namespace, Name: entry.Name}, task); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return safeError(contract.ErrUnavailable, err)
		}
		if task.UID != entry.UID {
			return contract.ErrConflict
		}
		base := task.DeepCopy()
		switch {
		case r.state.Cancelled:
			task.Status.Phase = corev1alpha1.TaskPhaseCancelled
		case r.state.Failed != "":
			task.Status.Phase = corev1alpha1.TaskPhaseFailed
		default:
			task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
		}
		task.Status.Message = "isolated application effects settled"
		if task.Status.CompletionTime == nil {
			task.Status.CompletionTime = new(metav1.Now())
		}
		if !reflect.DeepEqual(base.Status, task.Status) {
			if err := b.control.Status().Patch(ctx, task, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return safeError(contract.ErrUnavailable, err)
			}
		}
		if controllerutil.ContainsFinalizer(task, cleanupFinalizer) {
			base = task.DeepCopy()
			controllerutil.RemoveFinalizer(task, cleanupFinalizer)
			if err := b.control.Patch(ctx, task, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return safeError(contract.ErrUnavailable, err)
			}
		}
	}
	return nil
}

// Target workloads are never rendered into a control-cluster Job.
func (b *Backend) BuildValidationJob(_ context.Context, task *corev1alpha1.Task) (*batchv1.Job, error) {
	if task.Annotations[pv.ValidationBackendKey] == backendName {
		return nil, contract.ErrForbidden
	}
	return nil, contract.ErrInvalid
}

func (b *Backend) ReconcileValidationTask(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, bool, error) {
	if task.Annotations[pv.ValidationBackendKey] != backendName {
		return ctrl.Result{}, false, nil
	}
	if b == nil || !b.config.Enabled {
		return ctrl.Result{}, true, contract.ErrDisabled
	}
	name := task.Annotations[ledgerAnnotation]
	if !strings.HasPrefix(name, "app-exec-") {
		return ctrl.Result{}, true, contract.ErrConflict
	}
	r, err := b.load(ctx, name)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	side := task.Annotations["applicationexec.orka.ai/arm"]
	entry := r.state.Task
	if side != "" {
		entry = r.state.ArmTasks[side]
	}
	if task.UID != entry.UID || task.Name != entry.Name || task.Annotations[inputAnnotation] != r.state.InputDigest {
		return ctrl.Result{}, true, contract.ErrConflict
	}
	parentErr := b.parent(ctx, r)
	_, cancelled := task.Annotations[labels.AnnotationCancelRequested]
	if !task.DeletionTimestamp.IsZero() || task.Status.Phase == corev1alpha1.TaskPhaseCancelled ||
		cancelled ||
		parentErr != nil && (errors.Is(parentErr, contract.ErrConflict) || apierrors.IsNotFound(parentErr)) {
		err = b.Cancel(ctx, r.receipt())
		return ctrl.Result{}, true, err
	}
	if parentErr != nil {
		return ctrl.Result{}, true, parentErr
	}
	return ctrl.Result{RequeueAfter: b.config.ReconcileInterval}, true, nil
}
