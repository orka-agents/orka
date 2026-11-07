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
	// externalRuntimeEvictionAnnotation records the observed eviction of the
	// exact current instance. It is bound to the workspace, sequence and
	// instance identity, and a mismatching record authorizes nothing.
	externalRuntimeEvictionAnnotation = "orka.ai/external-runtime-eviction"
	// externalRuntimeRetirementRequestedAnnotation requests exact retirement and
	// replacement of the current external instance.
	externalRuntimeRetirementRequestedAnnotation = "orka.ai/external-runtime-retirement-requested"

	externalEvictionSourceWorker     = "WorkerTermination"
	externalEvictionSourceSupervisor = "SupervisorSignal"

	// podConditionDisruptionTarget is set by Kubernetes before a disruptive
	// termination that may not delete the Pod first, such as graceful node
	// shutdown.
	podConditionDisruptionTarget corev1.PodConditionType = "DisruptionTarget"
)

// externalRuntimeEviction is the durable eviction record for one exact
// external runtime instance.
type externalRuntimeEviction struct {
	WorkspaceUID types.UID                          `json:"workspaceUID"`
	Sequence     int64                              `json:"sequence"`
	Identity     workspacev1alpha1.InstanceIdentity `json:"identity"`
	Source       string                             `json:"source"`
	SignaledAt   time.Time                          `json:"signaledAt"`
	// Deadline is the end of the eviction window. Suspension is never
	// authorized at or after it.
	Deadline    time.Time `json:"deadline"`
	Suspendable bool      `json:"suspendable"`
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

// currentExternalRuntimeEviction returns the eviction record only when it binds
// the workspace's exact current instance. A malformed record fails closed: it
// is treated as an eviction whose window has already ended.
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

// externalWorkerEvictionError reports that the exact attested native worker
// began graceful termination. It is a stale identity for admission.
type externalWorkerEvictionError struct {
	signaledAt time.Time
	deadline   time.Time
}

func (e *externalWorkerEvictionError) Error() string {
	return "native runtime worker is terminating"
}

func (e *externalWorkerEvictionError) Unwrap() error { return workspaceprovider.ErrStaleIdentity }

// externalWorkerEvictionSignal derives the eviction window from the worker
// Pod. The worker forwards SIGTERM to its Actors when the kubelet signals it,
// which is the Pod's deletion time less its grace period. The window ends at
// the worker's shutdown budget or the Pod's own grace, whichever is first.
// Every signal time is clamped to Core's clock, as the supervisor signal is:
// a node or API server clock ahead of Core never extends the window.
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
	if worker.DeletionTimestamp != nil {
		podDeadline := worker.DeletionTimestamp.UTC()
		signaledAt, window := now, harnessv2.WorkerEvictionSuspendWindow
		if grace := worker.DeletionGracePeriodSeconds; grace != nil && *grace >= 0 {
			graceWindow := time.Duration(*grace) * time.Second
			signaledAt = clamp(podDeadline.Add(-graceWindow))
			window = min(window, graceWindow)
		}
		deadline := signaledAt.Add(window)
		if podDeadline.Before(deadline) {
			deadline = podDeadline
		}
		return signaledAt, deadline, true
	}
	for _, condition := range worker.Status.Conditions {
		if condition.Type == podConditionDisruptionTarget && condition.Status == corev1.ConditionTrue {
			signaledAt := clamp(condition.LastTransitionTime.Time)
			return signaledAt, signaledAt.Add(harnessv2.WorkerEvictionSuspendWindow), true
		}
	}
	return time.Time{}, time.Time{}, false
}

func externalWorkspaceSuspendable(w *workspacev1alpha1.ExecutionWorkspace) bool {
	return w.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredReady &&
		w.Status.State != workspacev1alpha1.ExecutionWorkspaceStateFailed &&
		slices.Contains(w.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend)
}

// externalRetirementAction is the provider action Core will authorize for the
// exact current instance once its drain settles. Drain requests carry it so a
// supervisor whose worker is terminated afterward knows whether its data must
// remain suspendable.
func externalRetirementAction(w *workspacev1alpha1.ExecutionWorkspace, deleting bool) workspacev1alpha1.WorkloadRetirementAction {
	// Quarantine is terminal and never reusable, so it never earns the data-preserving
	// Suspend authorization that a Suspend-capable class otherwise receives. Providers
	// act on quarantine only through a Stop (or Delete) authorization.
	quarantined := w.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined
	// A failed instance has no new data to capture; its last verified
	// checkpoint stays with the provider for explicit recovery.
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

// observeExternalSupervisorEviction handles a supervisor that self-reported
// its worker's termination signal through an authenticated status probe.
func (r *RuntimePoolReconciler) observeExternalSupervisorEviction(ctx context.Context, pool *corev1alpha1.RuntimePool, status harnessv2.StatusResponse) (ctrl.Result, bool, error) {
	if !runtimePoolHasExternalWorkspace(pool) || !status.Drain.Requested || status.Drain.Reason != harnessv2.DrainReasonWorkerEviction {
		return ctrl.Result{}, false, nil
	}
	w, err := r.externalPoolWorkspace(ctx, pool)
	if err != nil {
		return ctrl.Result{}, false, err
	}
	signaledAt := status.Drain.RequestedAt.UTC()
	if now := r.now().UTC(); signaledAt.IsZero() || signaledAt.After(now) {
		// A supervisor clock ahead of Core never extends the window.
		signaledAt = now
	}
	result, err := r.beginExternalRuntimeEviction(ctx, pool, w, externalEvictionSourceSupervisor, signaledAt, signaledAt.Add(harnessv2.WorkerEvictionSuspendWindow))
	return result, true, err
}

// beginExternalRuntimeEviction records the eviction of the exact current
// instance and, for a data-only suspendable workspace, requests its exact
// retirement. Retirement then reuses the authenticated drain: admission stays
// closed, prompts already accepted settle, and only a quiescent runtime earns a
// Suspend authorization before the deadline. A non-suspendable workspace keeps
// the existing failure semantics for a lost instance.
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

// recordExternalRuntimeEviction persists an eviction signal for the exact
// current instance, merging it with any earlier record: the earliest signal
// and the shortest window win, so a later observation never extends the time
// left to suspend. It is also used while a retirement drain is already in
// progress, where requestRetirement is false.
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
		record.Source, record.Suspendable = existing.Source, existing.Suspendable
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

// observeExternalRetiringWorkerEviction keeps collecting eviction signals
// while a retirement drain runs: termination can begin during a drain that
// started for another reason, and the Pod can reveal a shorter window than a
// supervisor signal recorded earlier. The action is the retirement the drain
// will authorize; it decides whether the eviction is reported as draining for
// suspension.
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
	if status != nil && status.Drain.Requested && status.Drain.Reason == harnessv2.DrainReasonWorkerEviction {
		signaledAt := status.Drain.RequestedAt.UTC()
		if now := r.now().UTC(); signaledAt.IsZero() || signaledAt.After(now) {
			signaledAt = now
		}
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

// markExternalRetiringEviction publishes an eviction observed during a
// retirement drain. A Suspend retirement drains for suspension, so its
// settlement can later report EvictionSuspended; any other retirement cannot
// preserve the instance's data. A condition already reporting this eviction,
// including an expired window, is preserved; one left by an earlier instance
// is replaced.
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

// externalEvictionProgress closes admission and reports the eviction
// condition without changing the bound instance.
func (r *RuntimePoolReconciler) externalEvictionProgress(ctx context.Context, pool *corev1alpha1.RuntimePool, lifecycle corev1alpha1.RuntimePoolLifecycle, reason, message string) (ctrl.Result, error) {
	status := r.baseRuntimePoolStatus(pool, pool.Status.CurrentReplicas)
	status.ActiveInstance = pool.Status.ActiveInstance
	status.Lifecycle, status.AdmissionState, status.Message = lifecycle, corev1alpha1.RuntimePoolAdmissionClosed, message
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, message)
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionWorkerEviction, metav1.ConditionTrue, reason, message)
	return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
}

// externalEvictionDeadlineExceeded refuses a Suspend authorization at or after
// the eviction deadline. The worker has killed (or is about to kill) the
// instance, so a late snapshot could not be trusted to hold settled work.
func (r *RuntimePoolReconciler) externalEvictionDeadlineExceeded(pool *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace) bool {
	record, malformed := currentExternalRuntimeEviction(pool, w)
	if malformed {
		return true
	}
	return record != nil && !r.now().Before(record.Deadline)
}

// externalEvictionSettlementPending reports whether the pool's persisted
// status still holds an eviction drain whose outcome has not been recorded.
func externalEvictionSettlementPending(pool *corev1alpha1.RuntimePool) bool {
	condition := meta.FindStatusCondition(pool.Status.Conditions, corev1alpha1.RuntimePoolConditionWorkerEviction)
	return condition != nil && condition.Status == metav1.ConditionTrue && condition.Reason == corev1alpha1.RuntimePoolReasonEvictionDraining
}

// externalEvictionSettledCondition reports how a draining eviction ended once
// its exact instance stopped: suspended with verified retained data, or lost.
func (r *RuntimePoolReconciler) externalEvictionSettledCondition(pool *corev1alpha1.RuntimePool, status *corev1alpha1.RuntimePoolStatus, w *workspacev1alpha1.ExecutionWorkspace) {
	condition := meta.FindStatusCondition(status.Conditions, corev1alpha1.RuntimePoolConditionWorkerEviction)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != corev1alpha1.RuntimePoolReasonEvictionDraining {
		return
	}
	a := (*workspacev1alpha1.AllocationObservation)(nil)
	if w != nil {
		a = w.Status.Allocation
	}
	if a != nil && a.State == workspacev1alpha1.AllocationStopped && a.RetainedData != nil && w.Status.State != workspacev1alpha1.ExecutionWorkspaceStateFailed {
		r.setRuntimePoolCondition(pool, status, corev1alpha1.RuntimePoolConditionWorkerEviction, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonEvictionSuspended,
			"the evicted runtime was suspended with its data and resumes on a new worker")
		return
	}
	r.setRuntimePoolCondition(pool, status, corev1alpha1.RuntimePoolConditionWorkerEviction, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonEvictionDeadlineExceeded,
		"the evicted runtime stopped without a data-only suspension; its last checkpoint is retained and uncertain work is never replayed")
}
