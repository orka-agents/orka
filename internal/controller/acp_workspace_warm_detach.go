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

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	workspaceprovider "github.com/orka-agents/orka-workspace/sdk"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/operatoraccess"
)

// ADR 0055: a detached session workspace whose frozen detach action is
// Suspend may stay Ready, with its runtime running, for a bounded window
// before the Suspend applies. The window is frozen per workspace through
// acpWorkspaceSuspendAfterDetachAnnotation; one window instance is the
// absolute acpWorkspaceSuspendAtAnnotation deadline.

// parseACPWorkspaceSuspendAfterDetach parses a frozen warm-detach window: a
// positive Go duration of at most maxACPWorkspaceSuspendAfterDetach.
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

// validateACPWorkspaceSuspendAfterDetachBounds rejects a window that is not
// strictly shorter than a set idle timeout or maximum lifetime.
func validateACPWorkspaceSuspendAfterDetachBounds(window, idleTimeout, maxLifetime time.Duration) error {
	if idleTimeout > 0 && window >= idleTimeout {
		return fmt.Errorf("suspendAfterDetach must be shorter than idleTimeout")
	}
	if maxLifetime > 0 && window >= maxLifetime {
		return fmt.Errorf("suspendAfterDetach must be shorter than maxLifetime")
	}
	return nil
}

// acpWorkspaceSuspendAfterDetachFromAnnotation returns the frozen window, or
// false when the workspace froze none. A malformed controller-written value
// disables the window: the frozen Suspend then applies at detach as before.
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

// acpWorkspaceSuspendAtDeadline parses the current warm-window deadline.
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

// acpWorkspaceWarmWindowOpen reports an unattached Ready workspace carrying a
// parseable warm-window deadline for its recorded Suspend action.
func acpWorkspaceWarmWindowOpen(workspace *workspacev1alpha1.ExecutionWorkspace) (time.Time, bool) {
	if workspace == nil || !workspace.DeletionTimestamp.IsZero() ||
		workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady ||
		workspace.Spec.Attachment != nil ||
		workspace.Annotations[acpWorkspaceDetachActionAnnotation] != string(workspacev1alpha1.WorkspaceOnDetachSuspend) {
		return time.Time{}, false
	}
	return acpWorkspaceSuspendAtDeadline(workspace)
}

// acpWorkspaceWarmWindowHoldsPool is the RuntimePool demand rule for a
// detached workspace: the pool keeps its replicas only while an open warm
// window exists on a workspace that is not Failed, quarantined, or tainted.
// It never evaluates the deadline; retention alone ends the window.
func acpWorkspaceWarmWindowHoldsPool(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	if _, open := acpWorkspaceWarmWindowOpen(workspace); !open {
		return false
	}
	return workspace.Status.State != workspacev1alpha1.ExecutionWorkspaceStateFailed &&
		!acpWorkspaceQuarantined(workspace) && !operatoraccess.Tainted(workspace.Annotations)
}

// acpWorkspaceWarmDetachWindow returns the frozen window when the settling
// Task's detach may keep the workspace warm, or zero. It requires an executed
// turn that did not park (ADR 0041 promises to release parked capacity), a
// session-reused Ready workspace whose recorded detach action is Suspend, the
// durable-session commit stamp, and no failure, quarantine, or operator taint.
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

// settleACPWorkspaceWarm completes an eligible Suspend settlement without
// suspending: in one optimistic patch it records the Task receipt and the
// detach instant, and clears the revocation stamp and any resume demand so a
// continuation can attach the Ready workspace directly. The deadline stamped
// when revocation began stands unchanged; it is never extended. applied=false
// with retry=false means the cold path applies.
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
		// No window was opened at revocation, or it already elapsed: the
		// frozen Suspend applies now, and that write removes the deadline.
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

// clearACPWorkspaceSuspendAt ends a warm window without changing desired
// state; the RuntimePool then scales to zero with data-preserving retirement.
// pending records that the frozen Suspend still has to be retried.
func clearACPWorkspaceSuspendAt(ctx context.Context, writer client.Client, workspace *workspacev1alpha1.ExecutionWorkspace, pending bool, now time.Time) error {
	_, present := workspace.Annotations[acpWorkspaceSuspendAtAnnotation]
	_, alreadyPending := workspace.Annotations[acpWorkspaceSuspendPendingAnnotation]
	if !present && alreadyPending == pending {
		return nil
	}
	base := workspace.DeepCopy()
	delete(workspace.Annotations, acpWorkspaceSuspendAtAnnotation)
	if pending {
		workspace.Annotations[acpWorkspaceSuspendPendingAnnotation] = now.UTC().Format(time.RFC3339Nano)
	} else {
		delete(workspace.Annotations, acpWorkspaceSuspendPendingAnnotation)
	}
	return writer.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// acpWorkspaceWarmSuspendRetryable reports a detached Ready workspace whose
// warm window ended while the retention cap was exhausted and whose frozen
// Suspend can still apply.
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

// retryPendingWarmSuspend retries the frozen Suspend of a workspace whose
// warm window ended at an exhausted cap. While the cap stays exhausted it
// falls through to ordinary idle retention, exactly as a quota-blocked
// detach without a window leaves idle retention in force.
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

// reconcileWarmDetachWindow ends an open warm window. It runs before, and
// independently of, idle retention. handled=false means no window is open.
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
	tainted := operatoraccess.Tainted(workspace.Annotations)
	if !tainted && now.Before(deadline) {
		requeue := min(deadline.Sub(now)+time.Second, lifetimeRequeue)
		return ctrl.Result{RequeueAfter: requeue}, true, nil
	}
	// A fenced expiry can return without deleting: a Session continuation
	// admitted before the retention fence vetoes it, or a concurrent update
	// wins the precondition. Retention watches only workspaces, so keep the
	// bounded lifetime requeue; a completed deletion makes it a no-op.
	expire := func(reason, message string) (ctrl.Result, bool, error) {
		if err := r.expireWorkspace(ctx, workspace, reason, message, true); err != nil {
			return ctrl.Result{}, true, err
		}
		return ctrl.Result{RequeueAfter: lifetimeRequeue}, true, nil
	}
	if tainted {
		// Operator access forfeits retention and cuts the window short.
		return expire("OperatorTaintedWarmWindowExpired",
			"the warm-detach window ended early for a workspace tainted by operator access; applying the Delete disposition instead of Suspend")
	}
	if workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateFailed {
		return expire("FailedWorkspaceWarmWindowExpired",
			"the warm-detach window elapsed for the terminally failed workspace; applying the Delete disposition")
	}
	if durableMarker, present := workspace.Annotations[acpWorkspaceDurableAnnotation]; present && durableMarker != booleanTrueValue {
		// Invalid controller-authenticated content is corruption, not
		// evidence that Delete is safe. End the window so the runtime stops
		// with data-preserving retirement; idle retention stays fail-closed.
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
		// The frozen Suspend stays pending, but the runtime must not keep
		// running until maxLifetime: end the window and leave the workspace
		// Ready so the pool scales to zero. retryPendingWarmSuspend retries
		// the Suspend once capacity opens.
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

// validateACPWorkspaceSuspendAfterDetach verifies a materialized window when
// present. An absent window stays admissible so a workspace created before
// the annotation existed can be backfilled. A window frozen from the
// controller default (ADR 0057) is admissible only for a binding whose
// profile sets none, whatever default the current controller uses.
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

// backfillACPWorkspaceSuspendAfterDetach migrates a workspace only when the
// protected window annotation is absent. Present mismatches are rejected by
// verifyACPClassWorkspace and never rewritten.
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
