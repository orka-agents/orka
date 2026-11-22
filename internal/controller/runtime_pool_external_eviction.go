/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	workspaceprovider "github.com/orka-agents/orka-workspace/sdk"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const (
	externalRuntimeEvictionAnnotation            = "orka.ai/external-runtime-eviction"
	externalRuntimeRetirementRequestedAnnotation = "orka.ai/external-runtime-retirement-requested"

	externalEvictionSourceWorker     = "WorkerTermination"
	externalEvictionSourceSupervisor = "SupervisorSignal"

	podConditionDisruptionTarget corev1.PodConditionType = "DisruptionTarget"
)

type externalRuntimeEviction struct {
	WorkspaceUID types.UID                          `json:"workspaceUID"`
	Sequence     int64                              `json:"sequence"`
	Identity     workspacev1alpha1.InstanceIdentity `json:"identity"`
	Source       string                             `json:"source"`
	SignaledAt   time.Time                          `json:"signaledAt"`
	Deadline     time.Time                          `json:"deadline"`
	Suspendable  bool                               `json:"suspendable"`
	Outcome      string                             `json:"outcome,omitempty"`
}

func (e *externalRuntimeEviction) matches(w *workspacev1alpha1.ExecutionWorkspace) bool {
	a := w.Status.Allocation
	return e != nil && a != nil && w.Spec.Workload != nil && e.WorkspaceUID == w.UID &&
		e.Sequence == a.Sequence && e.Sequence == w.Spec.Workload.Sequence && e.Identity == a.Identity
}

func externalRuntimeEvictionRecord(pool *corev1alpha1.RuntimePool) (*externalRuntimeEviction, error) {
	value := pool.Annotations[externalRuntimeEvictionAnnotation]
	if value == "" {
		return nil, nil
	}
	var record externalRuntimeEviction
	if err := json.Unmarshal([]byte(value), &record); err != nil {
		return nil, err
	}
	if record.WorkspaceUID == "" || record.Sequence < 1 || !record.Identity.Valid() || record.SignaledAt.IsZero() || record.Deadline.IsZero() {
		return nil, workspaceprovider.ErrStaleIdentity
	}
	return &record, nil
}

func currentExternalRuntimeEviction(pool *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace) (*externalRuntimeEviction, bool) {
	record, err := externalRuntimeEvictionRecord(pool)
	if err != nil {
		return nil, true
	}
	if record == nil || !record.matches(w) {
		return nil, false
	}
	return record, false
}

type externalWorkerEvictionError struct {
	signaledAt time.Time
	deadline   time.Time
}

func (e *externalWorkerEvictionError) Error() string {
	return "native runtime worker is terminating"
}

func (e *externalWorkerEvictionError) Unwrap() error { return workspaceprovider.ErrStaleIdentity }

func externalWorkerEvictionSignal(worker *corev1.Pod, now time.Time) (time.Time, time.Time, bool) {
	if worker == nil {
		return time.Time{}, time.Time{}, false
	}
	now = now.UTC()
	clamp := func(at time.Time) time.Time {
		if at.IsZero() || at.After(now) {
			return now
		}
		return at.UTC()
	}
	var signaledAt, deadline time.Time
	evicting := false
	observe := func(at, until time.Time) {
		if !evicting || at.Before(signaledAt) {
			signaledAt = at
		}
		if !evicting || until.Before(deadline) {
			deadline = until
		}
		evicting = true
	}
	if worker.DeletionTimestamp != nil {
		podDeadline := worker.DeletionTimestamp.UTC()
		at, window := now, harnessv2.WorkerEvictionSuspendWindow
		if grace := worker.DeletionGracePeriodSeconds; grace != nil && *grace >= 0 {
			graceWindow := time.Duration(*grace) * time.Second
			at = clamp(podDeadline.Add(-graceWindow))
			window = min(window, graceWindow)
		}
		until := at.Add(window)
		if podDeadline.Before(until) {
			until = podDeadline
		}
		observe(at, until)
	}
	for _, condition := range worker.Status.Conditions {
		if condition.Type == podConditionDisruptionTarget && condition.Status == corev1.ConditionTrue {
			at := clamp(condition.LastTransitionTime.Time)
			observe(at, at.Add(harnessv2.WorkerEvictionSuspendWindow))
		}
	}
	return signaledAt, deadline, evicting
}

func externalWorkspaceSuspendable(w *workspacev1alpha1.ExecutionWorkspace) bool {
	return w.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredReady &&
		w.Status.State != workspacev1alpha1.ExecutionWorkspaceStateFailed &&
		slices.Contains(w.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend)
}

func externalRetirementAction(w *workspacev1alpha1.ExecutionWorkspace, deleting bool) workspacev1alpha1.WorkloadRetirementAction {
	quarantined := w.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined
	failed := w.Status.State == workspacev1alpha1.ExecutionWorkspaceStateFailed
	switch {
	case deleting:
		return workspacev1alpha1.WorkloadRetirementDelete
	case !quarantined && !failed && (w.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended || slices.Contains(w.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend)):
		return workspacev1alpha1.WorkloadRetirementSuspend
	default:
		return workspacev1alpha1.WorkloadRetirementStop
	}
}

func externalRetirementDrainReason(action workspacev1alpha1.WorkloadRetirementAction) string {
	if action == workspacev1alpha1.WorkloadRetirementSuspend {
		return harnessv2.DrainReasonWorkspaceSuspend
	}
	return harnessv2.DrainReasonWorkspaceRetirement
}

func (r *RuntimePoolReconciler) supervisorWorkerEvictionSignal(status harnessv2.StatusResponse) (time.Time, bool) {
	if !status.Drain.Requested {
		return time.Time{}, false
	}
	signaledAt := status.Drain.WorkerEvictionAt
	if signaledAt.IsZero() {
		if status.Drain.Reason != harnessv2.DrainReasonWorkerEviction {
			return time.Time{}, false
		}
		signaledAt = status.Drain.RequestedAt
	}
	signaledAt = signaledAt.UTC()
	if now := r.now().UTC(); signaledAt.IsZero() || signaledAt.After(now) {
		signaledAt = now
	}
	return signaledAt, true
}

func (r *RuntimePoolReconciler) observeExternalSupervisorEviction(ctx context.Context, pool *corev1alpha1.RuntimePool, status harnessv2.StatusResponse) (ctrl.Result, bool, error) {
	if !runtimePoolHasExternalWorkspace(pool) {
		return ctrl.Result{}, false, nil
	}
	signaledAt, evicting := r.supervisorWorkerEvictionSignal(status)
	if !evicting {
		return ctrl.Result{}, false, nil
	}
	w, err := r.externalPoolWorkspace(ctx, pool)
	if err != nil {
		return ctrl.Result{}, false, err
	}
	result, err := r.beginExternalRuntimeEviction(ctx, pool, w, externalEvictionSourceSupervisor, signaledAt, signaledAt.Add(harnessv2.WorkerEvictionSuspendWindow))
	return result, true, err
}

func (r *RuntimePoolReconciler) beginExternalRuntimeEviction(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	w *workspacev1alpha1.ExecutionWorkspace,
	source string,
	signaledAt, deadline time.Time,
) (ctrl.Result, error) {
	record, err := r.recordExternalRuntimeEviction(ctx, pool, w, source, signaledAt, deadline, true)
	if errors.Is(err, errExternalEvictionUnbound) {
		return r.externalPoolProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDegraded, err.Error())
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if !record.Suspendable {
		return r.externalEvictionProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDegraded, corev1alpha1.RuntimePoolReasonEvictionNotSuspendable,
			"runtime worker is terminating and this workspace cannot preserve data; the instance is lost and uncertain work is never replayed")
	}
	return r.externalEvictionProgress(ctx, pool, corev1alpha1.RuntimePoolLifecycleDraining, corev1alpha1.RuntimePoolReasonEvictionDraining,
		fmt.Sprintf("runtime worker is terminating; draining for data-only suspension before %s", record.Deadline.Format(time.RFC3339)))
}

var errExternalEvictionUnbound = errors.New("runtime worker eviction observed without an exact instance identity or with an invalid record")

func (r *RuntimePoolReconciler) recordExternalRuntimeEviction(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	w *workspacev1alpha1.ExecutionWorkspace,
	source string,
	signaledAt, deadline time.Time,
	requestRetirement bool,
) (*externalRuntimeEviction, error) {
	a := w.Status.Allocation
	if w.Spec.Workload == nil || a == nil || !a.Identity.Valid() || a.Sequence != w.Spec.Workload.Sequence || a.Key != w.Spec.Workload.Key {
		return nil, errExternalEvictionUnbound
	}
	record := externalRuntimeEviction{
		WorkspaceUID: w.UID, Sequence: a.Sequence, Identity: a.Identity, Source: source,
		SignaledAt: signaledAt.UTC(), Deadline: deadline.UTC(), Suspendable: externalWorkspaceSuspendable(w),
	}
	existing, malformed := currentExternalRuntimeEviction(pool, w)
	if malformed {
		return nil, errExternalEvictionUnbound
	}
	if existing != nil {
		record.Source, record.Suspendable, record.Outcome = existing.Source, existing.Suspendable, existing.Outcome
		if existing.SignaledAt.Before(record.SignaledAt) {
			record.SignaledAt = existing.SignaledAt
		}
		if existing.Deadline.Before(record.Deadline) {
			record.Deadline = existing.Deadline
		}
	}
	requestRetirement = requestRetirement && record.Suspendable
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if pool.Annotations[externalRuntimeEvictionAnnotation] != string(encoded) ||
		(requestRetirement && pool.Annotations[externalRuntimeRetirementRequestedAnnotation] != booleanTrueValue) {
		before := pool.DeepCopy()
		if pool.Annotations == nil {
			pool.Annotations = map[string]string{}
		}
		pool.Annotations[externalRuntimeEvictionAnnotation] = string(encoded)
		if requestRetirement {
			pool.Annotations[externalRuntimeRetirementRequestedAnnotation] = booleanTrueValue
		}
		if err := r.Patch(ctx, pool, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return nil, err
		}
	}
	return &record, nil
}

func (r *RuntimePoolReconciler) observeExternalRetiringWorkerEviction(ctx context.Context, pool *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace, status *harnessv2.StatusResponse, action workspacev1alpha1.WorkloadRetirementAction) error {
	a := w.Status.Allocation
	if a == nil || a.Startup == nil {
		return nil
	}
	if _, err := r.attestExternalWorkspaceStartup(ctx, w.Spec.Workload, a.Startup); err != nil {
		if eviction := (*externalWorkerEvictionError)(nil); errors.As(err, &eviction) {
			record, err := r.recordExternalRuntimeEviction(ctx, pool, w, externalEvictionSourceWorker, eviction.signaledAt, eviction.deadline, false)
			if err != nil {
				return err
			}
			if err := r.markExternalRetiringEviction(ctx, pool, record, action); err != nil {
				return err
			}
		}
	}
	if status == nil {
		return nil
	}
	if signaledAt, evicting := r.supervisorWorkerEvictionSignal(*status); evicting {
		record, err := r.recordExternalRuntimeEviction(ctx, pool, w, externalEvictionSourceSupervisor, signaledAt, signaledAt.Add(harnessv2.WorkerEvictionSuspendWindow), false)
		if err != nil {
			return err
		}
		if err := r.markExternalRetiringEviction(ctx, pool, record, action); err != nil {
			return err
		}
	}
	return nil
}

func (r *RuntimePoolReconciler) markExternalRetiringEviction(ctx context.Context, pool *corev1alpha1.RuntimePool, record *externalRuntimeEviction, action workspacev1alpha1.WorkloadRetirementAction) error {
	if record == nil {
		return nil
	}
	reason := corev1alpha1.RuntimePoolReasonEvictionNotSuspendable
	message := "runtime worker is terminating during a retirement that does not preserve data; uncertain work is never replayed"
	if action == workspacev1alpha1.WorkloadRetirementSuspend {
		reason = corev1alpha1.RuntimePoolReasonEvictionDraining
		message = fmt.Sprintf("runtime worker is terminating; draining for data-only suspension before %s", record.Deadline.Format(time.RFC3339))
	}
	if condition := meta.FindStatusCondition(pool.Status.Conditions, corev1alpha1.RuntimePoolConditionWorkerEviction); condition != nil && condition.Status == metav1.ConditionTrue &&
		(condition.Reason == reason || (condition.Reason == corev1alpha1.RuntimePoolReasonEvictionDeadlineExceeded && !r.now().Before(record.Deadline))) {
		return nil
	}
	base := pool.DeepCopy()
	r.setRuntimePoolCondition(pool, &pool.Status, corev1alpha1.RuntimePoolConditionWorkerEviction, metav1.ConditionTrue, reason, message)
	return r.Status().Patch(ctx, pool, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func (r *RuntimePoolReconciler) externalEvictionProgress(ctx context.Context, pool *corev1alpha1.RuntimePool, lifecycle corev1alpha1.RuntimePoolLifecycle, reason, message string) (ctrl.Result, error) {
	status := r.baseRuntimePoolStatus(pool, pool.Status.CurrentReplicas)
	status.ActiveInstance = pool.Status.ActiveInstance
	status.Lifecycle, status.AdmissionState, status.Message = lifecycle, corev1alpha1.RuntimePoolAdmissionClosed, message
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, message)
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionWorkerEviction, metav1.ConditionTrue, reason, message)
	return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
}

func (r *RuntimePoolReconciler) externalEvictionDeadlineExceeded(pool *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace) bool {
	record, malformed := currentExternalRuntimeEviction(pool, w)
	if malformed {
		return true
	}
	return record != nil && !r.now().Before(record.Deadline)
}

func externalSuspendNotAfter(pool *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace, action workspacev1alpha1.WorkloadRetirementAction) *metav1.Time {
	if action != workspacev1alpha1.WorkloadRetirementSuspend {
		return nil
	}
	var notAfter *metav1.Time
	if existing := w.Spec.Retirement; existing != nil && existing.Action == workspacev1alpha1.WorkloadRetirementSuspend &&
		existing.Sequence == w.Spec.Workload.Sequence && existing.NotAfter != nil {
		notAfter = existing.NotAfter.DeepCopy()
	}
	if record, malformed := currentExternalRuntimeEviction(pool, w); !malformed && record != nil {
		deadline := metav1.NewTime(record.Deadline)
		if notAfter == nil || deadline.Before(notAfter) {
			notAfter = &deadline
		}
	}
	return notAfter
}

func externalProviderSuspendDeadlineExceeded(w *workspacev1alpha1.ExecutionWorkspace) bool {
	condition := meta.FindStatusCondition(w.Status.Conditions, string(workspacev1alpha1.ConditionWorkspaceDataPlaneReady))
	return condition != nil && condition.ObservedGeneration == w.Generation &&
		condition.Reason == string(workspacev1alpha1.ReasonSuspendDeadlineExceeded)
}

func externalEvictionSettlementPending(pool *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace) bool {
	condition := meta.FindStatusCondition(pool.Status.Conditions, corev1alpha1.RuntimePoolConditionWorkerEviction)
	if condition != nil && condition.Status == metav1.ConditionTrue && condition.Reason == corev1alpha1.RuntimePoolReasonEvictionDraining {
		return true
	}
	record := externalEvictionAwaitingOutcome(pool, w)
	if record == nil {
		return false
	}
	reason, _ := externalEvictionOutcome(w)
	return record.Outcome != reason
}

func externalEvictionAwaitingOutcome(pool *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace) *externalRuntimeEviction {
	if w == nil {
		return nil
	}
	record, malformed := currentExternalRuntimeEviction(pool, w)
	if malformed || record == nil || !record.Suspendable {
		return nil
	}
	return record
}

func (r *RuntimePoolReconciler) acknowledgeExternalEvictionOutcome(ctx context.Context, pool *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace) error {
	record := externalEvictionAwaitingOutcome(pool, w)
	if record == nil {
		return nil
	}
	reason, _ := externalEvictionOutcome(w)
	if record.Outcome == reason {
		return nil
	}
	record.Outcome = reason
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	before := pool.DeepCopy()
	pool.Annotations[externalRuntimeEvictionAnnotation] = string(encoded)
	return r.Patch(ctx, pool, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func externalEvictionOutcome(w *workspacev1alpha1.ExecutionWorkspace) (string, string) {
	a := (*workspacev1alpha1.AllocationObservation)(nil)
	if w != nil {
		a = w.Status.Allocation
	}
	if w != nil && externalProviderSuspendDeadlineExceeded(w) {
		return corev1alpha1.RuntimePoolReasonEvictionDeadlineExceeded,
			"the suspension authorization expired before the provider started suspending the runtime, so it stopped without its data; its last checkpoint is retained and uncertain work is never replayed"
	}
	if a != nil && a.State == workspacev1alpha1.AllocationStopped && a.RetainedData != nil && w.Status.State != workspacev1alpha1.ExecutionWorkspaceStateFailed {
		return corev1alpha1.RuntimePoolReasonEvictionSuspended, "the evicted runtime was suspended with its data and resumes on a new worker"
	}
	return corev1alpha1.RuntimePoolReasonEvictionDeadlineExceeded,
		"the evicted runtime stopped without a data-only suspension; its last checkpoint is retained and uncertain work is never replayed"
}

func (r *RuntimePoolReconciler) externalEvictionSettledCondition(pool *corev1alpha1.RuntimePool, status *corev1alpha1.RuntimePoolStatus, w *workspacev1alpha1.ExecutionWorkspace) {
	condition := meta.FindStatusCondition(status.Conditions, corev1alpha1.RuntimePoolConditionWorkerEviction)
	reason, message := externalEvictionOutcome(w)
	draining := condition != nil && condition.Status == metav1.ConditionTrue && condition.Reason == corev1alpha1.RuntimePoolReasonEvictionDraining
	if !draining {
		if record := externalEvictionAwaitingOutcome(pool, w); record == nil || record.Outcome == reason {
			return
		}
		meta.RemoveStatusCondition(&status.Conditions, corev1alpha1.RuntimePoolConditionWorkerEviction)
	}
	r.setRuntimePoolCondition(pool, status, corev1alpha1.RuntimePoolConditionWorkerEviction, metav1.ConditionFalse, reason, message)
}
