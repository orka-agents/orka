/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	workspaceprovider "github.com/orka-agents/orka-workspace/sdk"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/operatoraccess"
)

const acpWorkspaceWarmExpiryGrace = 2 * time.Minute

func (r *TaskReconciler) stampACPWorkspaceWarmDemand(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	now time.Time,
) error {
	deadline, open := acpWorkspaceWarmWindowOpen(workspace)
	if !open || deadline.Sub(now) >= acpWorkspaceWarmExpiryGrace ||
		acpWorkspaceResumeDemandBelongsToTask(workspace.Annotations[acpWorkspaceResumeRequestedAnnotation], string(task.UID)) {
		return nil
	}
	base := workspace.DeepCopy()
	workspace.Annotations[acpWorkspaceResumeRequestedAnnotation] =
		now.UTC().Format(time.RFC3339Nano) + " " + task.Name + " " + string(task.UID)
	return r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func (r *ACPWorkspaceRetentionReconciler) warmWindowDemandOutstanding(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	now time.Time,
) (bool, error) {
	if deadline, _, ok := parseACPWorkspaceSpeculativeStart(workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation]); ok && now.Before(deadline) {
		return true, nil
	}
	if live, err := r.recordedWorkspaceDemandLive(ctx, workspace, nil); err != nil || live {
		return true, err
	}
	return r.liveSessionContinuationExists(ctx, workspace, nil)
}

func parseACPWorkspaceSuspendAfterDetach(raw string) (time.Duration, error) {
	window, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("suspendAfterDetach %q is not a duration", raw)
	}
	if window <= 0 || window > maxACPWorkspaceSuspendAfterDetach {
		return 0, fmt.Errorf("suspendAfterDetach must be positive and at most %s", maxACPWorkspaceSuspendAfterDetach)
	}
	return window, nil
}

func validateACPWorkspaceSuspendAfterDetachBounds(window, idleTimeout, maxLifetime time.Duration) error {
	if idleTimeout > 0 && window >= idleTimeout {
		return fmt.Errorf("suspendAfterDetach must be shorter than idleTimeout")
	}
	if maxLifetime > 0 && window >= maxLifetime {
		return fmt.Errorf("suspendAfterDetach must be shorter than maxLifetime")
	}
	return nil
}

func acpWorkspaceSuspendAfterDetachFromAnnotation(workspace *workspacev1alpha1.ExecutionWorkspace) (time.Duration, bool) {
	if workspace == nil {
		return 0, false
	}
	raw, present := workspace.Annotations[acpWorkspaceSuspendAfterDetachAnnotation]
	if !present {
		return 0, false
	}
	window, err := parseACPWorkspaceSuspendAfterDetach(raw)
	if err != nil {
		return 0, false
	}
	return window, true
}

func acpWorkspaceSuspendAtDeadline(workspace *workspacev1alpha1.ExecutionWorkspace) (time.Time, bool) {
	if workspace == nil {
		return time.Time{}, false
	}
	raw, present := workspace.Annotations[acpWorkspaceSuspendAtAnnotation]
	if !present {
		return time.Time{}, false
	}
	deadline, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, false
	}
	return deadline, true
}

func acpWorkspaceQuarantined(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	return workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined ||
		workspace.Labels[workspacev1alpha1.QuarantinedLabel] == booleanTrueValue ||
		workspaceprovider.ConditionIsTrue(workspace.Status.Conditions, string(workspacev1alpha1.ConditionWorkspaceQuarantined))
}

const acpWorkspaceWarmWindowDrainedAnnotation = "acp.workspace.orka.ai/warm-window-drained-at"

func endACPWorkspaceWarmWindowForDrain(
	ctx context.Context,
	writer client.Client,
	pool *corev1alpha1.RuntimePool,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	now time.Time,
) (bool, error) {
	if pool.Annotations[externalRuntimeRetirementRequestedAnnotation] != booleanTrueValue &&
		strings.TrimSpace(pool.Annotations[externalRuntimeEvictionAnnotation]) == "" {
		return false, nil
	}
	if !acpWorkspaceWarmWindowHoldsPool(workspace) {
		return false, nil
	}
	deadline, open := acpWorkspaceWarmWindowOpen(workspace)
	_, drained := workspace.Annotations[acpWorkspaceWarmWindowDrainedAnnotation]
	if !open || (drained && !deadline.After(now)) {
		return false, nil
	}
	base := workspace.DeepCopy()
	if deadline.After(now) {
		workspace.Annotations[acpWorkspaceSuspendAtAnnotation] = now.UTC().Format(time.RFC3339Nano)
	}
	workspace.Annotations[acpWorkspaceWarmWindowDrainedAnnotation] = now.UTC().Format(time.RFC3339Nano)
	if err := writer.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, err
	}
	return true, nil
}

func acpWorkspaceWarmWindowOpen(workspace *workspacev1alpha1.ExecutionWorkspace) (time.Time, bool) {
	if workspace == nil || !workspace.DeletionTimestamp.IsZero() ||
		workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady ||
		workspace.Spec.Attachment != nil ||
		workspace.Annotations[acpWorkspaceDetachActionAnnotation] != string(workspacev1alpha1.WorkspaceOnDetachSuspend) {
		return time.Time{}, false
	}
	return acpWorkspaceSuspendAtDeadline(workspace)
}

func acpWorkspaceWarmWindowHoldsPool(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	if _, open := acpWorkspaceWarmWindowOpen(workspace); !open {
		return false
	}
	return workspace.Status.State != workspacev1alpha1.ExecutionWorkspaceStateFailed &&
		!acpWorkspaceQuarantined(workspace) && !operatoraccess.Tainted(workspace.Annotations)
}

func acpWorkspaceWarmDetachWindow(workspace *workspacev1alpha1.ExecutionWorkspace, task *corev1alpha1.Task) time.Duration {
	window, frozen := acpWorkspaceSuspendAfterDetachFromAnnotation(workspace)
	if !frozen || task == nil || task.Status.Wait != nil || taskNeverHeldACPWorkspaceAttachment(task) {
		return 0
	}
	if workspace.Spec.SessionRef == nil || !workspace.DeletionTimestamp.IsZero() ||
		workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady ||
		workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateFailed ||
		acpWorkspaceQuarantined(workspace) || operatoraccess.Tainted(workspace.Annotations) ||
		workspace.Annotations[acpWorkspaceDetachActionAnnotation] != string(workspacev1alpha1.WorkspaceOnDetachSuspend) ||
		strings.TrimSpace(workspace.Annotations[acpWorkspaceDurableSessionCommittedAnnotation]) == "" {
		return 0
	}
	return window
}

func (r *TaskReconciler) settleACPWorkspaceWarm(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	now time.Time,
) (applied, retry bool, err error) {
	if acpWorkspaceWarmDetachWindow(workspace, task) <= 0 {
		return false, false, nil
	}
	deadline, open := acpWorkspaceWarmWindowOpen(workspace)
	if !open || !now.Before(deadline) {
		return false, false, nil
	}
	base := workspace.DeepCopy()
	workspace.Annotations[acpWorkspaceLastSettledTaskAnnotation] =
		formatACPWorkspaceSettlementReceipt(string(task.UID), acpTaskRecordedAttachmentEpoch(task))
	workspace.Annotations[acpWorkspaceLastDetachedAnnotation] = now.UTC().Format(time.RFC3339Nano)
	delete(workspace.Annotations, acpWorkspaceRevocationStartedAnnotation)
	delete(workspace.Annotations, acpWorkspaceResumeRequestedAnnotation)
	delete(workspace.Annotations, acpWorkspaceSpeculativeStartAnnotation)
	if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return false, true, nil
		}
		return false, false, err
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(workspace, corev1.EventTypeNormal, "WarmDetachWindowStarted",
			"the detached workspace stays Ready until %s before its frozen Suspend applies",
			deadline.UTC().Format(time.RFC3339))
	}
	return true, false, nil
}

func clearACPWorkspaceSuspendAt(ctx context.Context, writer client.Client, workspace *workspacev1alpha1.ExecutionWorkspace, pending bool, now time.Time) error {
	_, present := workspace.Annotations[acpWorkspaceSuspendAtAnnotation]
	_, alreadyPending := workspace.Annotations[acpWorkspaceSuspendPendingAnnotation]
	if !present && alreadyPending == pending {
		return nil
	}
	base := workspace.DeepCopy()
	delete(workspace.Annotations, acpWorkspaceSuspendAtAnnotation)
	delete(workspace.Annotations, acpWorkspaceWarmWindowDrainedAnnotation)
	if pending {
		workspace.Annotations[acpWorkspaceSuspendPendingAnnotation] = now.UTC().Format(time.RFC3339Nano)
	} else {
		delete(workspace.Annotations, acpWorkspaceSuspendPendingAnnotation)
	}
	return writer.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func acpWorkspaceWarmSuspendRetryable(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	if _, pending := workspace.Annotations[acpWorkspaceSuspendPendingAnnotation]; !pending {
		return false
	}
	durableMarker, durablePresent := workspace.Annotations[acpWorkspaceDurableAnnotation]
	return workspace.DeletionTimestamp.IsZero() &&
		workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredReady &&
		workspace.Spec.Attachment == nil &&
		workspace.Annotations[acpWorkspaceDetachActionAnnotation] == string(workspacev1alpha1.WorkspaceOnDetachSuspend) &&
		workspace.Status.State != workspacev1alpha1.ExecutionWorkspaceStateFailed &&
		!acpWorkspaceQuarantined(workspace) && !operatoraccess.Tainted(workspace.Annotations) &&
		(!durablePresent || durableMarker == booleanTrueValue) &&
		strings.TrimSpace(workspace.Annotations[acpWorkspaceDurableSessionCommittedAnnotation]) != ""
}

func (r *ACPWorkspaceRetentionReconciler) retryPendingWarmSuspend(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	now time.Time,
) (ctrl.Result, bool, error) {
	if !acpWorkspaceWarmSuspendRetryable(workspace) {
		return ctrl.Result{}, false, nil
	}
	err := suspendACPWorkspaceWithinQuota(ctx, r.Client, r.quotaReader(), workspace, now, false, "", 0)
	switch {
	case errors.Is(err, errACPSuspendQuotaExhausted):
		return ctrl.Result{}, false, nil
	case errors.Is(err, errACPSuspendQuotaBusy), apierrors.IsConflict(err), apierrors.IsNotFound(err):
		return ctrl.Result{RequeueAfter: time.Second}, true, nil
	case err != nil:
		return ctrl.Result{}, true, err
	}
	r.recordRetention(workspace, "WarmWindowSuspended", "retention capacity opened; applying the frozen Suspend action pending since the warm-detach window ended")
	metrics.RecordACPWorkspaceRetentionAction("suspend", "warm_window")
	return ctrl.Result{}, true, nil
}

func (r *ACPWorkspaceRetentionReconciler) reconcileWarmDetachWindow(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	now time.Time,
	lifetimeRequeue time.Duration,
) (ctrl.Result, bool, error) {
	deadline, open := acpWorkspaceWarmWindowOpen(workspace)
	if !open {
		return ctrl.Result{}, false, nil
	}
	tainted, taintErr := acpWorkspaceOperatorTainted(ctx, uncachedReader(r.APIReader, r.Client), workspace)
	if taintErr != nil {
		return ctrl.Result{}, true, taintErr
	}
	if !tainted && now.Before(deadline) {
		requeue := min(deadline.Sub(now)+time.Second, lifetimeRequeue)
		return ctrl.Result{RequeueAfter: requeue}, true, nil
	}
	drained := strings.TrimSpace(workspace.Annotations[acpWorkspaceWarmWindowDrainedAnnotation]) != ""
	if !tainted && !drained && now.Before(deadline.Add(acpWorkspaceWarmExpiryGrace)) &&
		workspace.Status.State != workspacev1alpha1.ExecutionWorkspaceStateFailed {
		outstanding, err := r.warmWindowDemandOutstanding(ctx, workspace, now)
		if err != nil {
			return ctrl.Result{}, true, err
		}
		if outstanding {
			graceEnds := deadline.Add(acpWorkspaceWarmExpiryGrace)
			logf.FromContext(ctx).Info("Warm-detach window expiry deferred for live continuation demand",
				"workspace", workspace.Name, "deadline", deadline.UTC().Format(time.RFC3339Nano),
				"graceEnds", graceEnds.UTC().Format(time.RFC3339Nano), "pastDeadlineMs", now.Sub(deadline).Milliseconds())
			r.recordRetention(workspace, "WarmWindowExpiryDeferred",
				"the warm-detach window elapsed while a continuation still demands the workspace; its Suspend waits at most until "+
					graceEnds.UTC().Format(time.RFC3339))
			return ctrl.Result{RequeueAfter: min(5*time.Second, graceEnds.Sub(now)+time.Second)}, true, nil
		}
	}
	expire := func(reason, message string) (ctrl.Result, bool, error) {
		if err := r.expireWorkspace(ctx, workspace, reason, message, true); err != nil {
			return ctrl.Result{}, true, err
		}
		return ctrl.Result{RequeueAfter: lifetimeRequeue}, true, nil
	}
	if tainted {
		if err := r.expireWorkspace(ctx, workspace, "OperatorTaintedWarmWindowExpired",
			"the warm-detach window ended early for a workspace tainted by operator access; applying the Delete disposition instead of Suspend", false); err != nil {
			return ctrl.Result{}, true, err
		}
		return ctrl.Result{RequeueAfter: lifetimeRequeue}, true, nil
	}
	if workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateFailed {
		return expire("FailedWorkspaceWarmWindowExpired",
			"the warm-detach window elapsed for the terminally failed workspace; applying the Delete disposition")
	}
	if durableMarker, present := workspace.Annotations[acpWorkspaceDurableAnnotation]; present && durableMarker != booleanTrueValue {
		if err := clearACPWorkspaceSuspendAt(ctx, r.Client, workspace, false, now); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				return ctrl.Result{RequeueAfter: time.Second}, true, nil
			}
			return ctrl.Result{}, true, err
		}
		r.recordRetention(workspace, "InvalidDurableCapability",
			"the warm-detach window elapsed, but the durable-workspace marker is invalid; the runtime stops without suspension")
		return ctrl.Result{RequeueAfter: lifetimeRequeue}, true, nil
	}
	if strings.TrimSpace(workspace.Annotations[acpWorkspaceDurableSessionCommittedAnnotation]) == "" {
		return expire("UncommittedWorkspaceWarmWindowExpired",
			"the warm-detach window elapsed before a durable RuntimeSession checkpoint was committed; deleting the empty workspace incarnation")
	}
	err := suspendACPWorkspaceWithinQuota(ctx, r.Client, r.quotaReader(), workspace, now, false, "", 0)
	switch {
	case errors.Is(err, errACPSuspendQuotaExhausted):
		if clearErr := clearACPWorkspaceSuspendAt(ctx, r.Client, workspace, true, now); clearErr != nil {
			if apierrors.IsConflict(clearErr) || apierrors.IsNotFound(clearErr) {
				return ctrl.Result{RequeueAfter: time.Second}, true, nil
			}
			return ctrl.Result{}, true, clearErr
		}
		r.recordRetention(workspace, "WarmWindowSuspendQuotaExhausted",
			"the warm-detach window elapsed and the retention cap is exhausted; the runtime stops and the frozen Suspend action remains pending")
		return ctrl.Result{RequeueAfter: lifetimeRequeue}, true, nil
	case errors.Is(err, errACPSuspendQuotaBusy), apierrors.IsConflict(err), apierrors.IsNotFound(err):
		return ctrl.Result{RequeueAfter: time.Second}, true, nil
	case err != nil:
		return ctrl.Result{}, true, err
	}
	r.recordRetention(workspace, "WarmWindowSuspended", "the warm-detach window elapsed; applying the frozen Suspend action")
	metrics.RecordACPWorkspaceRetentionAction("suspend", "warm_window")
	return ctrl.Result{}, true, nil
}

func validateACPWorkspaceSuspendAfterDetach(workspace *workspacev1alpha1.ExecutionWorkspace, expected string) error {
	raw, present := workspace.Annotations[acpWorkspaceSuspendAfterDetachAnnotation]
	source, sourced := workspace.Annotations[acpWorkspaceSuspendAfterDetachSourceAnnotation]
	if sourced {
		if source != acpWorkspaceSuspendAfterDetachSourceDefault || !present || expected != "" {
			return fmt.Errorf("%w: workspace %s default warm-detach window does not match the frozen binding", errACPWorkspaceBindingConflict, workspace.Name)
		}
		if _, err := parseACPWorkspaceSuspendAfterDetach(raw); err != nil {
			return fmt.Errorf("%w: workspace %s default warm-detach window is invalid", errACPWorkspaceBindingConflict, workspace.Name)
		}
		return nil
	}
	if !present {
		return nil
	}
	if expected == "" || raw != expected {
		return fmt.Errorf("%w: workspace %s warm-detach window does not match the frozen binding", errACPWorkspaceBindingConflict, workspace.Name)
	}
	return nil
}

func (r *TaskReconciler) backfillACPWorkspaceSuspendAfterDetach(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	expected string,
) error {
	if expected == "" {
		return nil
	}
	if _, present := workspace.Annotations[acpWorkspaceSuspendAfterDetachAnnotation]; present {
		return nil
	}
	base := workspace.DeepCopy()
	if workspace.Annotations == nil {
		workspace.Annotations = map[string]string{}
	}
	workspace.Annotations[acpWorkspaceSuspendAfterDetachAnnotation] = expected
	if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("backfill workspace %s warm-detach window: %w", workspace.Name, err)
	}
	return nil
}
