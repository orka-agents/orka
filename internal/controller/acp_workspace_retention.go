/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	workspaceprovider "github.com/orka-agents/orka-workspace/sdk"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/operatoraccess"
	"github.com/orka-agents/orka/internal/store"
	storekube "github.com/orka-agents/orka/internal/store/kube"
)

const (
	acpWorkspaceLastDetachedAnnotation             = "acp.workspace.orka.ai/last-detached-at"
	acpWorkspaceMaxSuspendedAnnotation             = "acp.workspace.orka.ai/max-suspended"
	acpWorkspaceSuspendAfterDetachAnnotation       = "acp.workspace.orka.ai/suspend-after-detach"
	acpWorkspaceSuspendAtAnnotation                = "acp.workspace.orka.ai/suspend-at"
	acpWorkspaceSuspendPendingAnnotation           = "acp.workspace.orka.ai/suspend-pending"
	maxACPWorkspaceSuspendAfterDetach              = time.Hour
	acpWorkspaceLegacyRetentionDeadlineAnnotation  = "acp.workspace.orka.ai/legacy-retention-deadline"
	acpWorkspaceRetentionRequeue                   = 5 * time.Minute
	acpWorkspaceLegacyRetentionGrace               = 24 * time.Hour
	acpSuspendQuotaLeaseClassUIDAnnotation         = "acp.workspace.orka.ai/suspend-quota-class-uid"
	acpSuspendQuotaLeaseClaimsAnnotation           = "acp.workspace.orka.ai/suspend-quota-claims"
	acpWorkspaceRetentionFenceIdentityLabel        = "acp.workspace.orka.ai/retention-fence-for"
	acpWorkspaceRetentionFenceNameAnnotation       = "acp.workspace.orka.ai/retention-fence-workspace"
	acpWorkspaceRetentionFenceUIDAnnotation        = "acp.workspace.orka.ai/retention-fence-workspace-uid"
	acpWorkspaceRetentionFenceSessionUIDAnnotation = "acp.workspace.orka.ai/retention-fence-session-uid"
	acpWorkspaceRetentionFenceClassUIDAnnotation   = "acp.workspace.orka.ai/retention-fence-class-uid"
	acpWorkspaceRetentionFenceActivatedAnnotation  = "acp.workspace.orka.ai/retention-fence-activated-at"
	acpTaskSessionNameField                        = "spec.sessionRef.name"
	maxACPSuspendQuotaPendingClaims                = 1
)

var errACPWorkspaceRevocationStampInvalid = errors.New("workspace revocation stamp is invalid")

type ACPWorkspaceRetentionReconciler struct {
	client.Client
	APIReader           client.Reader
	DurableControlStore store.DurableControlStore
	Recorder            events.EventRecorder
	Now                 func() time.Time
}

func (r *ACPWorkspaceRetentionReconciler) quotaReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *ACPWorkspaceRetentionReconciler) now() time.Time {
	return clockNow(r.Now)
}

//nolint:gocyclo // The retention decision table stays auditable in one place.
func (r *ACPWorkspaceRetentionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, req.NamespacedName, workspace); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !workspaceHasACPControllerOwnership(workspace) {
		return ctrl.Result{}, nil
	}
	if !workspace.DeletionTimestamp.IsZero() ||
		workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredDeleted {
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(workspace, executionWorkspaceFinalizer) {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	now := r.now()
	fallbackDeadline, deadlineStamped, err := r.ensureRetentionFallbackDeadline(ctx, workspace, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	if deadlineStamped {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	retentionFence, demandCutoff, err := r.currentACPWorkspaceRetentionFence(ctx, workspace)
	if err != nil {
		return ctrl.Result{}, err
	}

	lifetimeRequeue := acpWorkspaceRetentionRequeue
	if lifetime := workspace.Spec.Lifecycle.MaxLifetime; lifetime != nil && lifetime.Duration > 0 {
		deadline := workspace.CreationTimestamp.Add(lifetime.Duration)
		if !now.Before(deadline) {
			return ctrl.Result{}, r.expireWorkspace(ctx, workspace, "MaxLifetimeExpired",
				"class maxLifetime elapsed; the workspace is forced into terminal cleanup", false)
		}
		if requeue := deadline.Sub(now) + time.Second; requeue < lifetimeRequeue {
			lifetimeRequeue = requeue
		}
	}
	if !fallbackDeadline.IsZero() {
		if !now.Before(fallbackDeadline) {
			return ctrl.Result{}, r.expireWorkspace(ctx, workspace, "FallbackRetentionExpired",
				"the persisted fallback retention deadline elapsed; forcing terminal cleanup", false)
		}
		if requeue := fallbackDeadline.Sub(now) + time.Second; requeue < lifetimeRequeue {
			lifetimeRequeue = requeue
		}
	}
	releaseQuota := func(result ctrl.Result) (ctrl.Result, error) {
		if err := releaseObsoleteACPSuspendQuotaLease(ctx, r.Client, r.quotaReader(), workspace); err != nil {
			return ctrl.Result{}, err
		}
		return result, nil
	}
	if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined {
		return r.reconcileQuarantinedWorkspaceRetention(ctx, workspace, now, lifetimeRequeue)
	}

	if workspace.Spec.Attachment != nil || workspace.Status.AttachedEpoch > 0 {
		return releaseQuota(ctrl.Result{RequeueAfter: lifetimeRequeue})
	}
	if refreshed, err := refreshACPWorkspaceDetachInstantAfterEpochRelease(ctx, r.Client, workspace, now); err != nil {
		if errors.Is(err, errACPWorkspaceRevocationStampInvalid) {
			r.recordRetention(workspace, "RetentionRevocationStampInvalid",
				"the revocation-started-at annotation is invalid; idle retention is held and only maxLifetime applies")
			return releaseQuota(ctrl.Result{RequeueAfter: lifetimeRequeue})
		}
		return ctrl.Result{}, err
	} else if refreshed {
		return releaseQuota(ctrl.Result{RequeueAfter: time.Second})
	}
	if deadline, open := acpWorkspaceWarmWindowOpen(workspace); open {
		tainted, taintErr := acpWorkspaceOperatorTainted(ctx, uncachedReader(r.APIReader, r.Client), workspace)
		if taintErr != nil {
			return ctrl.Result{}, taintErr
		}
		if !tainted && now.Before(deadline) {
			return releaseQuota(ctrl.Result{RequeueAfter: min(deadline.Sub(now)+time.Second, lifetimeRequeue)})
		}
	}
	if result, handled, err := r.reconcileWarmDetachWindow(ctx, workspace, now, lifetimeRequeue); err != nil {
		return ctrl.Result{}, err
	} else if handled {
		return result, nil
	}
	if acpWorkspaceWarmSuspendRetryable(workspace) {
		demandOutstanding, err := r.pendingWorkspaceDemandOutstanding(ctx, workspace, demandCutoff)
		if err != nil {
			return ctrl.Result{}, err
		}
		if demandOutstanding {
			if retentionFence != nil {
				if err := r.deleteACPWorkspaceRetentionFence(ctx, retentionFence); err != nil {
					return ctrl.Result{}, err
				}
			}
			return releaseQuota(ctrl.Result{RequeueAfter: lifetimeRequeue})
		}
	}
	if result, handled, err := r.retryPendingWarmSuspend(ctx, workspace, now); err != nil {
		return ctrl.Result{}, err
	} else if handled {
		return result, nil
	}
	idle := workspace.Spec.Lifecycle.IdleTimeout
	if idle == nil || idle.Duration <= 0 {
		return releaseQuota(ctrl.Result{RequeueAfter: lifetimeRequeue})
	}
	if _, present := workspace.Annotations[acpWorkspaceLastDetachedAnnotation]; !present && workspace.Spec.AttachmentEpoch > 0 {
		base := workspace.DeepCopy()
		if workspace.Annotations == nil {
			workspace.Annotations = map[string]string{}
		}
		workspace.Annotations[acpWorkspaceLastDetachedAnnotation] = now.Format(time.RFC3339Nano)
		if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			return ctrl.Result{}, err
		}
		return releaseQuota(ctrl.Result{RequeueAfter: time.Second})
	}
	demandOutstanding, err := r.pendingWorkspaceDemandOutstanding(ctx, workspace, demandCutoff)
	if err != nil {
		return ctrl.Result{}, err
	}
	if demandOutstanding && retentionFence != nil {
		if err := r.deleteACPWorkspaceRetentionFence(ctx, retentionFence); err != nil {
			return ctrl.Result{}, err
		}
	}
	if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredReady && demandOutstanding {
		return releaseQuota(ctrl.Result{RequeueAfter: lifetimeRequeue})
	}
	idleStart := workspace.CreationTimestamp.Time
	if value, present := workspace.Annotations[acpWorkspaceLastDetachedAnnotation]; present {
		raw := strings.TrimSpace(value)
		parsed, parseErr := time.Parse(time.RFC3339Nano, raw)
		if parseErr != nil {
			r.recordRetention(workspace, "RetentionIdleStampInvalid",
				"the last-detached-at annotation is not RFC3339Nano; idle retention is held and only maxLifetime applies")
			return releaseQuota(ctrl.Result{RequeueAfter: lifetimeRequeue})
		}
		idleStart = parsed
	}
	deadline := idleStart.Add(idle.Duration)
	if now.Before(deadline) {
		requeue := min(deadline.Sub(now)+time.Second, lifetimeRequeue)
		return releaseQuota(ctrl.Result{RequeueAfter: requeue})
	}

	switch workspace.Spec.DesiredState {
	case workspacev1alpha1.ExecutionWorkspaceDesiredSuspended:
		if demandOutstanding {
			return releaseQuota(ctrl.Result{RequeueAfter: lifetimeRequeue})
		}
		return ctrl.Result{}, r.expireWorkspace(ctx, workspace, "IdleRetentionExpired",
			"class idleTimeout elapsed for the suspended workspace; retention is exhausted", true)
	case workspacev1alpha1.ExecutionWorkspaceDesiredReady:
		if recordedAction, present := workspace.Annotations[acpWorkspaceDetachActionAnnotation]; present &&
			recordedAction != string(workspacev1alpha1.WorkspaceOnDetachSuspend) &&
			recordedAction != string(workspacev1alpha1.WorkspaceOnDetachDelete) {
			r.recordRetention(workspace, "UnknownIdleAction",
				"class idleTimeout elapsed, but the recorded Task detach action is not executable by this controller; failing closed")
			return releaseQuota(ctrl.Result{RequeueAfter: lifetimeRequeue})
		}
		idleAction := string(workspace.Spec.Lifecycle.DefaultOnDetach)
		if idleAction != string(workspacev1alpha1.WorkspaceOnDetachSuspend) &&
			idleAction != string(workspacev1alpha1.WorkspaceOnDetachDelete) {
			r.recordRetention(workspace, "UnknownIdleAction",
				"class idleTimeout elapsed, but the frozen class default is not executable by this controller; failing closed")
			return releaseQuota(ctrl.Result{RequeueAfter: lifetimeRequeue})
		}
		if workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateFailed {
			return ctrl.Result{}, r.expireWorkspace(ctx, workspace, "FailedWorkspaceIdleExpired",
				"class idleTimeout elapsed for the terminally failed workspace; applying the Delete disposition", true)
		}
		if durableMarker, present := workspace.Annotations[acpWorkspaceDurableAnnotation]; idleAction == string(workspacev1alpha1.WorkspaceOnDetachSuspend) && present && durableMarker != booleanTrueValue {
			r.recordRetention(workspace, "InvalidDurableCapability",
				"class idleTimeout elapsed, but the durable-workspace marker is invalid; failing closed")
			return releaseQuota(ctrl.Result{RequeueAfter: lifetimeRequeue})
		}
		operatorTainted := false
		if idleAction == string(workspacev1alpha1.WorkspaceOnDetachSuspend) {
			tainted, err := acpWorkspaceOperatorTainted(ctx, r.quotaReader(), workspace)
			if err != nil {
				return ctrl.Result{}, err
			}
			operatorTainted = tainted
		}
		if operatorTainted {
			return ctrl.Result{}, r.expireWorkspace(ctx, workspace, "OperatorTaintedIdleExpired",
				"class idleTimeout elapsed for a workspace tainted by operator access; applying the Delete disposition instead of Suspend", true)
		}
		if idleAction == string(workspacev1alpha1.WorkspaceOnDetachSuspend) &&
			runtimePoolWorkspaceSuspendableAnnotationPresent(workspace) &&
			strings.TrimSpace(workspace.Annotations[acpWorkspaceDurableSessionCommittedAnnotation]) == "" {
			return ctrl.Result{}, r.expireWorkspace(ctx, workspace, "UncommittedWorkspaceIdleExpired",
				"class idleTimeout elapsed before a durable RuntimeSession checkpoint was committed; deleting the empty workspace incarnation", true)
		}
		if idleAction == string(workspacev1alpha1.WorkspaceOnDetachSuspend) &&
			runtimePoolWorkspaceSuspendableAnnotationPresent(workspace) {
			_, resumeUnfulfilled := workspace.Annotations[acpWorkspaceResumeRequestedAnnotation]
			preserveLastDetached := resumeUnfulfilled &&
				workspace.Annotations[acpWorkspaceResumedLineageAnnotation] == booleanTrueValue
			err := suspendACPWorkspaceWithinQuota(
				ctx, r.Client, r.quotaReader(), workspace, now, preserveLastDetached, "", 0,
			)
			switch {
			case errors.Is(err, errACPSuspendQuotaExhausted):
				r.recordRetention(workspace, "SuspendQuotaExhausted",
					"class idleTimeout elapsed and the retention cap is exhausted; the class Suspend action remains pending")
				return ctrl.Result{RequeueAfter: lifetimeRequeue}, nil
			case errors.Is(err, errACPSuspendQuotaBusy), apierrors.IsConflict(err), apierrors.IsNotFound(err):
				return ctrl.Result{RequeueAfter: time.Second}, nil
			case err != nil:
				return ctrl.Result{}, err
			}
			r.recordRetention(workspace, "IdleSuspended", "class idleTimeout elapsed; applying the class default Suspend action")
			metrics.RecordACPWorkspaceRetentionAction("suspend", "idle_timeout")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, r.expireWorkspace(ctx, workspace, "IdleExpired",
			"class idleTimeout elapsed for the unattached workspace; applying the class Delete disposition", true)
	default:
		return releaseQuota(ctrl.Result{RequeueAfter: lifetimeRequeue})
	}
}

func (r *ACPWorkspaceRetentionReconciler) reconcileQuarantinedWorkspaceRetention(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	now time.Time,
	lifetimeRequeue time.Duration,
) (ctrl.Result, error) {
	idle := workspace.Spec.Lifecycle.IdleTimeout
	if idle == nil || idle.Duration <= 0 {
		if err := releaseObsoleteACPSuspendQuotaLease(ctx, r.Client, r.quotaReader(), workspace); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: lifetimeRequeue}, nil
	}
	idleStart := workspace.CreationTimestamp.Time
	if value, present := workspace.Annotations[acpWorkspaceLastDetachedAnnotation]; present {
		parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
		if err != nil {
			r.recordRetention(workspace, "RetentionIdleStampInvalid",
				"the last-detached-at annotation is not RFC3339Nano; quarantined cleanup is held and only maxLifetime applies")
			if releaseErr := releaseObsoleteACPSuspendQuotaLease(ctx, r.Client, r.quotaReader(), workspace); releaseErr != nil {
				return ctrl.Result{}, releaseErr
			}
			return ctrl.Result{RequeueAfter: lifetimeRequeue}, nil
		}
		idleStart = parsed
	}
	deadline := idleStart.Add(idle.Duration)
	if now.Before(deadline) {
		if err := releaseObsoleteACPSuspendQuotaLease(ctx, r.Client, r.quotaReader(), workspace); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: min(deadline.Sub(now)+time.Second, lifetimeRequeue)}, nil
	}
	return ctrl.Result{}, r.expireWorkspace(ctx, workspace, "QuarantineIdleExpired",
		"class idleTimeout elapsed for the terminal quarantined workspace; forcing terminal cleanup", false)
}

func (r *ACPWorkspaceRetentionReconciler) ensureRetentionFallbackDeadline(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	now time.Time,
) (time.Time, bool, error) {
	if workspace.Spec.Lifecycle.MaxLifetime != nil && workspace.Spec.Lifecycle.MaxLifetime.Duration > 0 {
		return time.Time{}, false, nil
	}
	reason, required := acpWorkspaceRetentionFallbackReason(workspace)
	if !required {
		return time.Time{}, false, nil
	}
	if raw, present := workspace.Annotations[acpWorkspaceLegacyRetentionDeadlineAnnotation]; present {
		if deadline, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw)); err == nil {
			return deadline, false, nil
		}
	}

	deadline := now.Add(acpWorkspaceLegacyRetentionGrace).UTC()
	base := workspace.DeepCopy()
	if workspace.Annotations == nil {
		workspace.Annotations = map[string]string{}
	}
	workspace.Annotations[acpWorkspaceLegacyRetentionDeadlineAnnotation] = deadline.Format(time.RFC3339Nano)
	if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return time.Time{}, true, nil
		}
		return time.Time{}, false, err
	}
	r.recordRetention(workspace, "FallbackRetentionBounded",
		reason+"; terminal cleanup is scheduled after a 24-hour migration grace period")
	return deadline, true, nil
}

func acpWorkspaceRetentionFallbackReason(workspace *workspacev1alpha1.ExecutionWorkspace) (string, bool) {
	idle := workspace.Spec.Lifecycle.IdleTimeout
	if raw, present := workspace.Annotations[acpWorkspaceRevocationStartedAnnotation]; present {
		if _, _, ok := parseACPWorkspaceRevocationStamp(raw); !ok {
			return "the protected revocation stamp is invalid", true
		}
	}
	if idle != nil && idle.Duration > 0 {
		if raw, present := workspace.Annotations[acpWorkspaceResumeRequestedAnnotation]; present {
			if _, valid := parseACPWorkspaceResumeRequestStamp(raw); !valid {
				return "the protected pending-demand stamp is invalid", true
			}
		}
		if raw, present := workspace.Annotations[acpWorkspaceLastDetachedAnnotation]; present {
			if _, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw)); err != nil {
				return "the protected idle stamp is invalid", true
			}
		}
		if workspace.Spec.Lifecycle.DefaultOnDetach == workspacev1alpha1.WorkspaceOnDetachSuspend {
			if _, capped := workspace.Annotations[acpWorkspaceMaxSuspendedAnnotation]; capped {
				return "a legacy quota-capped Suspend binding has no maxLifetime", true
			}
		}
		return "", false
	}
	if durable, present := workspace.Annotations[acpWorkspaceDurableAnnotation]; present && durable != booleanTrueValue {
		return "the protected durable-workspace marker is invalid", true
	}
	if slices.Contains(workspace.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend) &&
		runtimePoolWorkspaceMayContainDurableData(workspace) {
		return "a legacy suspend-capable workspace has no frozen expiry", true
	}
	return "", false
}

func refreshACPWorkspaceDetachInstantAfterEpochRelease(
	ctx context.Context,
	writer client.Client,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	now time.Time,
) (bool, error) {
	rawStamp, present := workspace.Annotations[acpWorkspaceRevocationStartedAnnotation]
	if !present {
		return false, nil
	}
	stampedEpoch, revocationStartedAt, ok := parseACPWorkspaceRevocationStamp(
		rawStamp,
	)
	if !ok {
		return false, errACPWorkspaceRevocationStampInvalid
	}
	if stampedEpoch != workspace.Spec.AttachmentEpoch {
		return false, nil
	}
	detachedAt, err := time.Parse(time.RFC3339Nano, workspace.Annotations[acpWorkspaceLastDetachedAnnotation])
	if err != nil || !detachedAt.Equal(revocationStartedAt) {
		return false, nil
	}
	refreshedAt := now.UTC()
	if !refreshedAt.After(revocationStartedAt) {
		refreshedAt = revocationStartedAt.Add(time.Nanosecond)
	}
	base := workspace.DeepCopy()
	workspace.Annotations[acpWorkspaceLastDetachedAnnotation] = refreshedAt.Format(time.RFC3339Nano)
	if err := writer.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return true, nil
		}
		return true, err
	}
	return true, nil
}

func runtimePoolWorkspaceSuspendableAnnotationPresent(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	durable, present := workspace.Annotations[acpWorkspaceDurableAnnotation]
	if present {
		return durable == booleanTrueValue
	}
	return workspace.Spec.Lifecycle.DefaultOnDetach == workspacev1alpha1.WorkspaceOnDetachSuspend ||
		workspace.Annotations[acpWorkspaceDetachActionAnnotation] == string(workspacev1alpha1.WorkspaceOnDetachSuspend)
}

func runtimePoolWorkspaceMayContainDurableData(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	if workspace == nil || workspace.Annotations[acpWorkspaceDurableDataAbsentAnnotation] == booleanTrueValue {
		return false
	}
	if _, present := workspace.Annotations[acpWorkspaceDurableAnnotation]; present {
		return true
	}
	return runtimePoolWorkspaceSuspendableAnnotationPresent(workspace)
}

func (r *ACPWorkspaceRetentionReconciler) expireWorkspace(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	reason, message string,
	fenced bool,
) error {
	var retentionFence *coordinationv1.Lease
	if fenced && workspace.Spec.SessionRef != nil {
		var err error
		retentionFence, err = r.ensureACPWorkspaceRetentionFence(ctx, workspace)
		if err != nil {
			return err
		}
		if retentionFence != nil {
			activatedAt, activationErr := acpWorkspaceRetentionFenceActivation(retentionFence)
			if activationErr != nil {
				return activationErr
			}
			demandOutstanding, demandErr := r.pendingWorkspaceDemandOutstanding(ctx, workspace, &activatedAt)
			if demandErr != nil {
				return demandErr
			}
			if demandOutstanding {
				return r.deleteACPWorkspaceRetentionFence(ctx, retentionFence)
			}
		}
	}
	preconditions := []client.DeleteOption{client.Preconditions{UID: &workspace.UID}}
	if fenced {
		preconditions = deleteCurrentObjectPreconditions(workspace)
	}
	if err := r.Delete(ctx, workspace, preconditions...); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		if fenced && apierrors.IsConflict(err) {
			if retentionFence != nil {
				return r.deleteACPWorkspaceRetentionFence(ctx, retentionFence)
			}
			return nil
		}
		return err
	}
	r.recordRetention(workspace, reason, message)
	metrics.RecordACPWorkspaceRetentionAction("delete", strings.ToLower(reason))
	return nil
}

func (r *ACPWorkspaceRetentionReconciler) ensureACPWorkspaceRetentionFence(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (*coordinationv1.Lease, error) {
	control, err := r.acpWorkspaceRetentionControl(ctx, workspace)
	if err != nil || control == nil {
		return nil, err
	}

	key := types.NamespacedName{
		Namespace: workspace.Namespace,
		Name:      acpWorkspaceRetentionFenceLeaseName(workspace),
	}
	lease := &coordinationv1.Lease{}
	err = r.quotaReader().Get(ctx, key, lease)
	if err == nil {
		return r.activateACPWorkspaceRetentionFence(ctx, lease, workspace, control)
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("read workspace retention fence Lease: %w", err)
	}

	lease = newACPWorkspaceRetentionFence(workspace, control)
	if err := r.Create(ctx, lease); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create workspace retention fence Lease: %w", err)
		}
		if getErr := r.quotaReader().Get(ctx, key, lease); getErr != nil {
			return nil, fmt.Errorf("read concurrently created workspace retention fence Lease: %w", getErr)
		}
		return r.activateACPWorkspaceRetentionFence(ctx, lease, workspace, control)
	}
	if _, err := r.ensureACPWorkspaceRetentionFenceActivation(ctx, lease); err != nil {
		return nil, err
	}
	return lease, nil
}

func (r *ACPWorkspaceRetentionReconciler) acpWorkspaceRetentionControl(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (*corev1alpha1.RuntimeSessionControl, error) {
	if workspace == nil || workspace.Spec.SessionRef == nil {
		return nil, nil
	}
	sessionName := strings.TrimSpace(workspace.Spec.SessionRef.Name)
	sessionUID := strings.TrimSpace(string(workspace.Spec.SessionRef.UID))
	classUID := strings.TrimSpace(string(workspace.Spec.ClassBinding.UID))
	if sessionName == "" || sessionUID == "" || classUID == "" || workspace.UID == "" {
		return nil, fmt.Errorf("session workspace retention fence requires complete workspace, class, and Session identities")
	}

	control := &corev1alpha1.RuntimeSessionControl{}
	controlKey := types.NamespacedName{
		Namespace: workspace.Namespace,
		Name:      storekube.RuntimeSessionControlObjectName(sessionName),
	}
	if err := r.quotaReader().Get(ctx, controlKey, control); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read RuntimeSessionControl for workspace retention fence: %w", err)
	}
	if control.UID == "" || control.Spec.SessionName != sessionName {
		return nil, fmt.Errorf("RuntimeSessionControl identity does not match the retained workspace Session")
	}
	if control.Spec.SessionUID != sessionUID {
		return nil, nil
	}
	return control, nil
}

func newACPWorkspaceRetentionFence(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	control *corev1alpha1.RuntimeSessionControl,
) *coordinationv1.Lease {
	return &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Namespace: workspace.Namespace,
		Name:      acpWorkspaceRetentionFenceLeaseName(workspace),
		Labels: map[string]string{
			acpWorkspaceRetentionFenceIdentityLabel: labels.SelectorValue(workspace.Name),
		},
		Annotations: map[string]string{
			acpWorkspaceRetentionFenceNameAnnotation:       workspace.Name,
			acpWorkspaceRetentionFenceUIDAnnotation:        string(workspace.UID),
			acpWorkspaceRetentionFenceSessionUIDAnnotation: string(workspace.Spec.SessionRef.UID),
			acpWorkspaceRetentionFenceClassUIDAnnotation:   string(workspace.Spec.ClassBinding.UID),
		},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: corev1alpha1.GroupVersion.String(),
			Kind:       "RuntimeSessionControl",
			Name:       control.Name,
			UID:        control.UID,
		}},
	}}
}

func (r *ACPWorkspaceRetentionReconciler) activateACPWorkspaceRetentionFence(
	ctx context.Context,
	lease *coordinationv1.Lease,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	control *corev1alpha1.RuntimeSessionControl,
) (*coordinationv1.Lease, error) {
	if err := validateACPWorkspaceRetentionFence(lease, workspace, control); err != nil {
		return r.replaceACPWorkspaceRetentionFence(ctx, lease, workspace, control)
	}
	if lease.Annotations[acpWorkspaceRetentionFenceUIDAnnotation] == string(workspace.UID) {
		if _, err := r.ensureACPWorkspaceRetentionFenceActivation(ctx, lease); err != nil {
			return nil, err
		}
		return lease, nil
	}
	return r.replaceACPWorkspaceRetentionFence(ctx, lease, workspace, control)
}

func (r *ACPWorkspaceRetentionReconciler) replaceACPWorkspaceRetentionFence(
	ctx context.Context,
	lease *coordinationv1.Lease,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	control *corev1alpha1.RuntimeSessionControl,
) (*coordinationv1.Lease, error) {
	if err := r.deleteACPWorkspaceRetentionFence(ctx, lease); err != nil {
		return nil, err
	}
	replacement := newACPWorkspaceRetentionFence(workspace, control)
	if err := r.Create(ctx, replacement); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("recreate workspace retention fence Lease: %w", err)
		}
		if getErr := r.quotaReader().Get(ctx, client.ObjectKeyFromObject(replacement), replacement); getErr != nil {
			return nil, fmt.Errorf("read concurrently recreated workspace retention fence Lease: %w", getErr)
		}
		return r.activateACPWorkspaceRetentionFence(ctx, replacement, workspace, control)
	}
	if _, err := r.ensureACPWorkspaceRetentionFenceActivation(ctx, replacement); err != nil {
		return nil, err
	}
	return replacement, nil
}

func (r *ACPWorkspaceRetentionReconciler) ensureACPWorkspaceRetentionFenceActivation(
	ctx context.Context,
	lease *coordinationv1.Lease,
) (time.Time, error) {
	if lease == nil || lease.CreationTimestamp.IsZero() {
		return time.Time{}, fmt.Errorf("workspace retention fence has no API-server creation timestamp")
	}
	activatedAt := lease.CreationTimestamp.UTC()
	canonical := activatedAt.Format(time.RFC3339Nano)
	if raw := strings.TrimSpace(lease.Annotations[acpWorkspaceRetentionFenceActivatedAnnotation]); raw == canonical {
		return activatedAt, nil
	}
	base := lease.DeepCopy()
	if lease.Annotations == nil {
		lease.Annotations = map[string]string{}
	}
	lease.Annotations[acpWorkspaceRetentionFenceActivatedAnnotation] = canonical
	if err := r.Patch(ctx, lease, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return time.Time{}, fmt.Errorf("canonicalize workspace retention fence activation: %w", err)
	}
	return activatedAt, nil
}

func acpWorkspaceRetentionFenceActivation(lease *coordinationv1.Lease) (time.Time, error) {
	if lease == nil || lease.CreationTimestamp.IsZero() {
		return time.Time{}, fmt.Errorf("workspace retention fence has no API-server creation timestamp")
	}
	return lease.CreationTimestamp.UTC(), nil
}

func validateACPWorkspaceRetentionFence(
	lease *coordinationv1.Lease,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	control *corev1alpha1.RuntimeSessionControl,
) error {
	if lease == nil || workspace == nil || workspace.Spec.SessionRef == nil || control == nil {
		return fmt.Errorf("workspace retention fence validation requires complete objects")
	}
	if !strings.HasPrefix(lease.Name, labels.ACPWorkspaceRetentionFenceLeaseNamePrefix) ||
		lease.Labels[acpWorkspaceRetentionFenceIdentityLabel] != labels.SelectorValue(workspace.Name) ||
		lease.Annotations[acpWorkspaceRetentionFenceNameAnnotation] != workspace.Name ||
		strings.TrimSpace(lease.Annotations[acpWorkspaceRetentionFenceUIDAnnotation]) == "" ||
		lease.Annotations[acpWorkspaceRetentionFenceSessionUIDAnnotation] != string(workspace.Spec.SessionRef.UID) ||
		lease.Annotations[acpWorkspaceRetentionFenceClassUIDAnnotation] != string(workspace.Spec.ClassBinding.UID) {
		return fmt.Errorf("workspace retention fence Lease does not match the logical Session workspace")
	}
	for _, owner := range lease.OwnerReferences {
		if owner.APIVersion == corev1alpha1.GroupVersion.String() && owner.Kind == "RuntimeSessionControl" &&
			owner.Name == control.Name && owner.UID == control.UID {
			return nil
		}
	}
	return fmt.Errorf("workspace retention fence Lease is not owned by the exact RuntimeSessionControl")
}

func (r *ACPWorkspaceRetentionReconciler) deleteACPWorkspaceRetentionFence(
	ctx context.Context,
	lease *coordinationv1.Lease,
) error {
	if lease == nil {
		return nil
	}
	if err := r.Delete(ctx, lease, deleteCurrentObjectPreconditions(lease)...); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete workspace retention fence Lease: %w", err)
	}
	return nil
}

func (r *ACPWorkspaceRetentionReconciler) currentACPWorkspaceRetentionFence(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (*coordinationv1.Lease, *time.Time, error) {
	if workspace == nil || workspace.Spec.SessionRef == nil || workspace.UID == "" {
		return nil, nil, nil
	}
	lease := &coordinationv1.Lease{}
	err := r.quotaReader().Get(ctx, types.NamespacedName{
		Namespace: workspace.Namespace,
		Name:      acpWorkspaceRetentionFenceLeaseName(workspace),
	}, lease)
	if apierrors.IsNotFound(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read current workspace retention fence Lease: %w", err)
	}
	control, err := r.acpWorkspaceRetentionControl(ctx, workspace)
	if err != nil {
		return nil, nil, err
	}
	if control == nil || validateACPWorkspaceRetentionFence(lease, workspace, control) != nil {
		if err := r.deleteACPWorkspaceRetentionFence(ctx, lease); err != nil {
			return nil, nil, err
		}
		return nil, nil, nil
	}
	fencedWorkspaceUID := strings.TrimSpace(lease.Annotations[acpWorkspaceRetentionFenceUIDAnnotation])
	if fencedWorkspaceUID != string(workspace.UID) {
		return nil, nil, nil
	}
	activatedAt, err := r.ensureACPWorkspaceRetentionFenceActivation(ctx, lease)
	if err != nil {
		return nil, nil, err
	}
	return lease, &activatedAt, nil
}

func acpWorkspaceRetentionFenceLeaseName(workspace *workspacev1alpha1.ExecutionWorkspace) string {
	identity := workspace.Namespace + "\x00" + workspace.Name
	sum := sha256.Sum256([]byte(identity))
	return labels.ACPWorkspaceRetentionFenceLeaseNamePrefix + hex.EncodeToString(sum[:12])
}

func (r *TaskReconciler) taskBlockedByACPWorkspaceRetentionFence(
	ctx context.Context,
	task *corev1alpha1.Task,
	binding *ACPRuntimeWorkspaceBinding,
	workspaceName string,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (bool, error) {
	if task == nil || binding == nil || binding.Class == nil ||
		binding.ReusePolicy != corev1alpha1.WorkspaceReusePolicySession {
		return false, nil
	}
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	fences := &coordinationv1.LeaseList{}
	if err := reader.List(ctx, fences,
		client.InNamespace(task.Namespace),
		client.MatchingLabels{acpWorkspaceRetentionFenceIdentityLabel: labels.SelectorValue(workspaceName)},
	); err != nil {
		return false, fmt.Errorf("list workspace retention fences: %w", err)
	}
	for i := range fences.Items {
		fence := &fences.Items[i]
		if !strings.HasPrefix(fence.Name, labels.ACPWorkspaceRetentionFenceLeaseNamePrefix) {
			continue
		}
		fencedWorkspaceUID := strings.TrimSpace(fence.Annotations[acpWorkspaceRetentionFenceUIDAnnotation])
		if fence.Annotations[acpWorkspaceRetentionFenceNameAnnotation] != workspaceName ||
			fence.Annotations[acpWorkspaceRetentionFenceSessionUIDAnnotation] != binding.SessionUID ||
			fence.Annotations[acpWorkspaceRetentionFenceClassUIDAnnotation] != binding.Class.UID ||
			fencedWorkspaceUID == "" {
			return false, fmt.Errorf("%w: workspace retention fence metadata is invalid", errACPWorkspaceBindingConflict)
		}
		if workspace != nil && string(workspace.UID) == fencedWorkspaceUID {
			return true, nil
		}
	}
	return false, nil
}

func (r *ACPWorkspaceRetentionReconciler) recordRetention(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	reason, message string,
) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(workspace, nil, corev1.EventTypeNormal, reason, "Retention", "%s", message)
}

type acpSuspendQuotaLockEntry struct {
	mutex      sync.Mutex
	references int
}

var (
	acpSuspendQuotaLocksMu sync.Mutex
	acpSuspendQuotaLocks   = map[string]*acpSuspendQuotaLockEntry{}
)

var errACPSuspendQuotaExhausted = errors.New("class suspended-workspace retention cap is exhausted")

var errACPSuspendQuotaBusy = errors.New("class suspended-workspace quota claim is in progress")

func lockACPSuspendQuota(namespace string, classUID types.UID) func() {
	key := namespace + "/" + string(classUID)
	acpSuspendQuotaLocksMu.Lock()
	entry := acpSuspendQuotaLocks[key]
	if entry == nil {
		entry = &acpSuspendQuotaLockEntry{}
		acpSuspendQuotaLocks[key] = entry
	}
	entry.references++
	acpSuspendQuotaLocksMu.Unlock()

	entry.mutex.Lock()
	return func() {
		entry.mutex.Unlock()
		acpSuspendQuotaLocksMu.Lock()
		entry.references--
		if entry.references == 0 {
			delete(acpSuspendQuotaLocks, key)
		}
		acpSuspendQuotaLocksMu.Unlock()
	}
}

func acpSuspendQuotaLeaseName(classUID types.UID) string {
	sum := sha256.Sum256([]byte(classUID))
	return labels.ACPSuspendQuotaLeaseNamePrefix + hex.EncodeToString(sum[:12])
}

type acpSuspendQuotaClaim struct {
	WorkspaceName   string `json:"workspaceName"`
	ResourceVersion string `json:"resourceVersion"`
}

type acpSuspendQuotaClaims map[string]acpSuspendQuotaClaim

func newACPSuspendQuotaLease(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	claims acpSuspendQuotaClaims,
) (*coordinationv1.Lease, error) {
	if workspace == nil || workspace.UID == "" || workspace.Spec.ClassBinding.Name == "" ||
		workspace.Spec.ClassBinding.UID == "" || workspace.ResourceVersion == "" {
		return nil, errors.New("workspace, class, and resourceVersion identities are required for a suspension quota claim")
	}
	controller := true
	holder := string(workspace.Spec.ClassBinding.UID)
	now := metav1.NewMicroTime(time.Now().UTC())
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: workspace.Namespace,
			Name:      acpSuspendQuotaLeaseName(workspace.Spec.ClassBinding.UID),
			Annotations: map[string]string{
				acpSuspendQuotaLeaseClassUIDAnnotation: string(workspace.Spec.ClassBinding.UID),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: workspacev1alpha1.GroupVersion.String(),
				Kind:       "ExecutionWorkspaceClass",
				Name:       workspace.Spec.ClassBinding.Name,
				UID:        workspace.Spec.ClassBinding.UID,
				Controller: &controller,
			}},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: &holder,
			AcquireTime:    &now,
			RenewTime:      &now,
		},
	}
	if err := setACPSuspendQuotaClaims(lease, claims); err != nil {
		return nil, err
	}
	return lease, nil
}

func readACPSuspendQuotaClaims(
	lease *coordinationv1.Lease,
	className string,
	classUID types.UID,
) (acpSuspendQuotaClaims, error) {
	if lease == nil || lease.Annotations == nil ||
		lease.Annotations[acpSuspendQuotaLeaseClassUIDAnnotation] != string(classUID) {
		return nil, errors.New("suspension quota Lease has invalid class identity")
	}
	owner := metav1.GetControllerOf(lease)
	if owner == nil || owner.APIVersion != workspacev1alpha1.GroupVersion.String() ||
		owner.Kind != "ExecutionWorkspaceClass" || owner.Name != className || owner.UID != classUID {
		return nil, errors.New("suspension quota Lease has invalid class ownership")
	}
	claims := acpSuspendQuotaClaims{}
	if err := json.Unmarshal([]byte(lease.Annotations[acpSuspendQuotaLeaseClaimsAnnotation]), &claims); err != nil {
		return nil, fmt.Errorf("decode suspension quota claims: %w", err)
	}
	if len(claims) > maxACPSuspendQuotaPendingClaims {
		return nil, errors.New("suspension quota Lease carries more than one pending workspace claim")
	}
	for uid, claim := range claims {
		if strings.TrimSpace(uid) == "" || strings.TrimSpace(claim.WorkspaceName) == "" ||
			strings.TrimSpace(claim.ResourceVersion) == "" {
			return nil, errors.New("suspension quota Lease has an invalid workspace claim")
		}
	}
	return claims, nil
}

func setACPSuspendQuotaClaims(lease *coordinationv1.Lease, claims acpSuspendQuotaClaims) error {
	if len(claims) > maxACPSuspendQuotaPendingClaims {
		return errors.New("suspension quota Lease cannot store more than one pending workspace claim")
	}
	encoded, err := json.Marshal(claims)
	if err != nil {
		return fmt.Errorf("encode suspension quota claims: %w", err)
	}
	if lease.Annotations == nil {
		lease.Annotations = map[string]string{}
	}
	lease.Annotations[acpSuspendQuotaLeaseClaimsAnnotation] = string(encoded)
	return nil
}

func listACPSuspendQuotaWorkspaces(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	classUID types.UID,
) (map[string]*workspacev1alpha1.ExecutionWorkspace, error) {
	list := &workspacev1alpha1.ExecutionWorkspaceList{}
	if err := reader.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list execution workspaces for retention accounting: %w", err)
	}
	workspaces := make(map[string]*workspacev1alpha1.ExecutionWorkspace, len(list.Items))
	for i := range list.Items {
		workspace := &list.Items[i]
		if !workspaceHasACPControllerOwnership(workspace) ||
			workspace.Spec.ClassBinding.UID != classUID || workspace.UID == "" {
			continue
		}
		workspaces[string(workspace.UID)] = workspace.DeepCopy()
	}
	return workspaces, nil
}

func normalizeACPSuspendQuotaClaims(
	claims acpSuspendQuotaClaims,
	workspaces map[string]*workspacev1alpha1.ExecutionWorkspace,
) (acpSuspendQuotaClaims, map[string]struct{}, bool) {
	normalized := make(acpSuspendQuotaClaims, len(claims))
	occupied := make(map[string]struct{}, len(workspaces))
	for uid, workspace := range workspaces {
		if workspaceConsumesSuspendedQuota(workspace) {
			occupied[uid] = struct{}{}
		}
	}
	changed := false
	for uid, claim := range claims {
		workspace := workspaces[uid]
		keep := workspace != nil && claim.WorkspaceName == workspace.Name &&
			!workspaceConsumesSuspendedQuota(workspace) && workspace.DeletionTimestamp.IsZero() &&
			workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredReady &&
			claim.ResourceVersion == workspace.ResourceVersion
		if !keep {
			changed = true
			continue
		}
		normalized[uid] = claim
		occupied[uid] = struct{}{}
	}
	return normalized, occupied, changed
}

func patchACPSuspendQuotaClaims(
	ctx context.Context,
	writer client.Client,
	lease *coordinationv1.Lease,
	claims acpSuspendQuotaClaims,
) error {
	base := lease.DeepCopy()
	if err := setACPSuspendQuotaClaims(lease, claims); err != nil {
		return err
	}
	now := metav1.NewMicroTime(time.Now().UTC())
	lease.Spec.RenewTime = &now
	if err := writer.Patch(ctx, lease, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return errACPSuspendQuotaBusy
		}
		return fmt.Errorf("patch suspension quota Lease: %w", err)
	}
	return nil
}

func createACPSuspendQuotaClaimLease(
	ctx context.Context,
	writer client.Client,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	limit int32,
	workspaces map[string]*workspacev1alpha1.ExecutionWorkspace,
) error {
	occupied := 0
	for uid, candidate := range workspaces {
		if uid != string(workspace.UID) && workspaceConsumesSuspendedQuota(candidate) {
			occupied++
		}
	}
	if occupied >= int(limit) {
		return errACPSuspendQuotaExhausted
	}
	claims := acpSuspendQuotaClaims{
		string(workspace.UID): {
			WorkspaceName:   workspace.Name,
			ResourceVersion: workspace.ResourceVersion,
		},
	}
	desired, err := newACPSuspendQuotaLease(workspace, claims)
	if err != nil {
		return err
	}
	if err := writer.Create(ctx, desired); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return errACPSuspendQuotaBusy
		}
		return fmt.Errorf("create suspension quota Lease: %w", err)
	}
	return nil
}

func claimACPSuspendQuotaSlot(
	ctx context.Context,
	writer client.Client,
	reader client.Reader,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	limit int32,
) error {
	if limit <= 0 {
		return errACPSuspendQuotaExhausted
	}
	lease := &coordinationv1.Lease{}
	key := types.NamespacedName{
		Namespace: workspace.Namespace,
		Name:      acpSuspendQuotaLeaseName(workspace.Spec.ClassBinding.UID),
	}
	err := reader.Get(ctx, key, lease)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("read suspension quota Lease: %w", err)
	}
	workspaces, listErr := listACPSuspendQuotaWorkspaces(
		ctx, reader, workspace.Namespace, workspace.Spec.ClassBinding.UID,
	)
	if listErr != nil {
		return listErr
	}
	if apierrors.IsNotFound(err) {
		return createACPSuspendQuotaClaimLease(ctx, writer, workspace, limit, workspaces)
	}
	claims, err := readACPSuspendQuotaClaims(
		lease, workspace.Spec.ClassBinding.Name, workspace.Spec.ClassBinding.UID,
	)
	if err != nil {
		if deleteErr := writer.Delete(ctx, lease, deleteCurrentObjectPreconditions(lease)...); deleteErr != nil {
			if apierrors.IsConflict(deleteErr) || apierrors.IsNotFound(deleteErr) {
				return errACPSuspendQuotaBusy
			}
			return fmt.Errorf("delete malformed suspension quota Lease: %w", deleteErr)
		}
		return createACPSuspendQuotaClaimLease(ctx, writer, workspace, limit, workspaces)
	}
	claims, occupied, changed := normalizeACPSuspendQuotaClaims(claims, workspaces)
	uid := string(workspace.UID)
	delete(occupied, uid)
	desiredClaim := acpSuspendQuotaClaim{
		WorkspaceName:   workspace.Name,
		ResourceVersion: workspace.ResourceVersion,
	}
	currentClaim, claimed := claims[uid]
	if !claimed && len(occupied) >= int(limit) {
		if changed {
			if err := patchACPSuspendQuotaClaims(ctx, writer, lease, claims); err != nil {
				return err
			}
		}
		return errACPSuspendQuotaExhausted
	}
	if !claimed && len(claims) > 0 {
		if changed {
			if err := patchACPSuspendQuotaClaims(ctx, writer, lease, claims); err != nil {
				return err
			}
		}
		return errACPSuspendQuotaBusy
	}
	if !claimed || currentClaim != desiredClaim {
		claims[uid] = desiredClaim
		changed = true
	}
	if !changed {
		return nil
	}
	return patchACPSuspendQuotaClaims(ctx, writer, lease, claims)
}

func releaseObsoleteACPSuspendQuotaLease(
	ctx context.Context,
	writer client.Client,
	reader client.Reader,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) error {
	if workspace == nil || acpWorkspaceSuspendedCapFromAnnotation(workspace) == nil ||
		workspace.Spec.ClassBinding.Name == "" || workspace.Spec.ClassBinding.UID == "" || workspace.UID == "" {
		return nil
	}
	unlock := lockACPSuspendQuota(workspace.Namespace, workspace.Spec.ClassBinding.UID)
	defer unlock()

	lease := &coordinationv1.Lease{}
	key := types.NamespacedName{
		Namespace: workspace.Namespace,
		Name:      acpSuspendQuotaLeaseName(workspace.Spec.ClassBinding.UID),
	}
	if err := reader.Get(ctx, key, lease); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read suspension quota Lease for recovery: %w", err)
	}
	claims, err := readACPSuspendQuotaClaims(
		lease, workspace.Spec.ClassBinding.Name, workspace.Spec.ClassBinding.UID,
	)
	if err != nil {
		return err
	}
	uid := string(workspace.UID)
	claim, claimed := claims[uid]
	if !claimed {
		return nil
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	workspaceKey := client.ObjectKeyFromObject(workspace)
	if err := reader.Get(ctx, workspaceKey, current); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("read execution workspace for suspension quota recovery: %w", err)
		}
		current = nil
	}
	if current != nil && current.UID == workspace.UID &&
		!workspaceConsumesSuspendedQuota(current) && current.DeletionTimestamp.IsZero() &&
		current.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredReady &&
		claim.WorkspaceName == current.Name && claim.ResourceVersion == current.ResourceVersion {
		return nil
	}
	delete(claims, uid)
	return patchACPSuspendQuotaClaims(ctx, writer, lease, claims)
}

func suspendACPWorkspaceWithinQuota(
	ctx context.Context,
	writer client.Client,
	reader client.Reader,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	now time.Time,
	preserveLastDetached bool,
	settledTaskUID string,
	settledTaskEpoch int64,
) error {
	tainted, err := acpWorkspaceOperatorTainted(ctx, reader, workspace)
	if err != nil {
		return fmt.Errorf("refusing to suspend workspace %s: %w", workspace.Name, err)
	}
	if tainted {
		return fmt.Errorf("refusing to suspend workspace %s: %w", workspace.Name, operatoraccess.ErrTainted)
	}
	if settledTaskUID != "" &&
		!acpWorkspaceResumeDemandBelongsToTask(
			workspace.Annotations[acpWorkspaceResumeRequestedAnnotation], settledTaskUID,
		) && acpWorkspaceSettlementReceiptCoversTask(
		workspace.Annotations[acpWorkspaceLastSettledTaskAnnotation], settledTaskUID, settledTaskEpoch,
	) {
		return nil
	}
	unlock := lockACPSuspendQuota(workspace.Namespace, workspace.Spec.ClassBinding.UID)
	defer unlock()
	limit := acpWorkspaceSuspendedCapFromAnnotation(workspace)
	if limit != nil {
		if err := claimACPSuspendQuotaSlot(ctx, writer, reader, workspace, *limit); err != nil {
			return err
		}
	}
	base := workspace.DeepCopy()
	if workspace.Annotations == nil {
		workspace.Annotations = map[string]string{}
	}
	if !preserveLastDetached {
		workspace.Annotations[acpWorkspaceLastDetachedAnnotation] = now.UTC().Format(time.RFC3339Nano)
	}
	delete(workspace.Annotations, acpWorkspaceRevocationStartedAnnotation)
	if settledTaskUID != "" {
		workspace.Annotations[acpWorkspaceLastSettledTaskAnnotation] =
			formatACPWorkspaceSettlementReceipt(settledTaskUID, settledTaskEpoch)
	}
	delete(workspace.Annotations, acpWorkspaceResumeRequestedAnnotation)
	delete(workspace.Annotations, acpWorkspaceSuspendAtAnnotation)
	delete(workspace.Annotations, acpWorkspaceSuspendPendingAnnotation)
	delete(workspace.Annotations, acpWorkspaceWarmWindowDrainedAnnotation)
	delete(workspace.Annotations, acpWorkspaceSpeculativeStartAnnotation)
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	return writer.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func workspaceConsumesSuspendedQuota(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	if workspace == nil {
		return false
	}
	durableDataAbsent := workspace.Annotations[acpWorkspaceDurableDataAbsentAnnotation] == booleanTrueValue
	mayContainDurableData := runtimePoolWorkspaceMayContainDurableData(workspace)
	if workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateFailed &&
		!mayContainDurableData {
		return false
	}
	suspendedCharge := workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	coldResumeCharge := workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredReady &&
		(workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateSuspended ||
			workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateSuspending) &&
		!durableDataAbsent
	failedDurableCharge := workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateFailed &&
		mayContainDurableData
	deletedCleanupProven := workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateDeleted &&
		workspaceprovider.ValidateInteractiveDeletedDisposition(
			workspace.Status.Disposition, workspace.Spec.Lifecycle.DeletionPolicy,
		) == nil
	maintenanceCharge := mayContainDurableData &&
		((workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined &&
			workspace.Status.State != workspacev1alpha1.ExecutionWorkspaceStateQuarantined) ||
			(workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredDeleted &&
				!deletedCleanupProven))
	deletingCharge := !workspace.DeletionTimestamp.IsZero() &&
		mayContainDurableData &&
		!deletedCleanupProven
	return suspendedCharge || coldResumeCharge || failedDurableCharge || maintenanceCharge || deletingCharge
}

func countSuspendedClassWorkspaces(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	classUID types.UID,
	exclude func(*workspacev1alpha1.ExecutionWorkspace) bool,
) (int, error) {
	list := &workspacev1alpha1.ExecutionWorkspaceList{}
	if err := reader.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return 0, fmt.Errorf("list execution workspaces for retention accounting: %w", err)
	}
	count := 0
	for i := range list.Items {
		workspace := &list.Items[i]
		if !workspaceHasACPControllerOwnership(workspace) ||
			workspace.Spec.ClassBinding.UID != classUID || (exclude != nil && exclude(workspace)) {
			continue
		}
		if workspaceConsumesSuspendedQuota(workspace) {
			count++
		}
	}
	return count, nil
}

func (r *ACPWorkspaceRetentionReconciler) pendingWorkspaceDemandOutstanding(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	createdNotAfter *time.Time,
) (bool, error) {
	if outstanding, err := r.recordedWorkspaceDemandLive(ctx, workspace, createdNotAfter); err != nil || outstanding {
		return outstanding, err
	}
	return r.liveSessionContinuationExists(ctx, workspace, createdNotAfter)
}

func (r *ACPWorkspaceRetentionReconciler) recordedWorkspaceDemandLive(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	createdNotAfter *time.Time,
) (bool, error) {
	value, present := workspace.Annotations[acpWorkspaceResumeRequestedAnnotation]
	if !present {
		return false, nil
	}
	fields, valid := parseACPWorkspaceResumeRequestStamp(value)
	if !valid {
		return true, nil
	}
	if len(fields) < 3 {
		return false, nil
	}
	task := &corev1alpha1.Task{}
	err := r.quotaReader().Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: fields[1]}, task)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if len(fields) >= 3 && string(task.UID) != fields[2] {
		return false, nil
	}
	if taskCreatedAfter(task, createdNotAfter) {
		return false, nil
	}
	if !task.DeletionTimestamp.IsZero() ||
		task.Status.Phase == corev1alpha1.TaskPhaseSucceeded ||
		task.Status.Phase == corev1alpha1.TaskPhaseFailed ||
		task.Status.Phase == corev1alpha1.TaskPhaseCancelled {
		return false, nil
	}
	return true, nil
}

func parseACPWorkspaceResumeRequestStamp(value string) ([]string, bool) {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return nil, false
	}
	if _, err := time.Parse(time.RFC3339Nano, fields[0]); err != nil {
		return nil, false
	}
	return fields, true
}

func (r *ACPWorkspaceRetentionReconciler) liveSessionContinuationExists(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	createdNotAfter *time.Time,
) (bool, error) {
	candidates, err := liveACPSessionContinuations(ctx, r.quotaReader(), workspace, "")
	if err != nil {
		return true, err
	}
	for _, task := range candidates {
		if taskCreatedAfter(task, createdNotAfter) {
			continue
		}
		linked := strings.TrimSpace(task.Labels[acpExecutionWorkspaceLinkLabel]) == workspace.Name &&
			strings.TrimSpace(task.Annotations[acpExecutionWorkspaceUIDAnnotation]) == string(workspace.UID)
		if linked {
			return true, nil
		}
		if r.DurableControlStore == nil {
			return true, nil
		}
		control, controlErr := r.DurableControlStore.GetSessionControl(
			ctx, task.Namespace, strings.TrimSpace(task.Spec.SessionRef.Name),
		)
		switch {
		case errors.Is(controlErr, store.ErrNotFound):
			continue
		case controlErr != nil:
			return true, controlErr
		case control == nil:
			return true, fmt.Errorf("session control lookup returned no identity for %s/%s", task.Namespace, task.Spec.SessionRef.Name)
		case strings.TrimSpace(control.SessionUID) == string(workspace.Spec.SessionRef.UID):
			return true, nil
		}
	}
	return false, nil
}

func taskCreatedAfter(task *corev1alpha1.Task, cutoff *time.Time) bool {
	return task != nil && cutoff != nil && !task.CreationTimestamp.IsZero() && task.CreationTimestamp.After(*cutoff)
}

func liveACPSessionContinuations(
	ctx context.Context,
	reader client.Reader,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	excludeTaskUID types.UID,
) ([]*corev1alpha1.Task, error) {
	if workspace.Spec.SessionRef == nil || strings.TrimSpace(workspace.Spec.SessionRef.Name) == "" {
		return nil, nil
	}
	sessionName := strings.TrimSpace(workspace.Spec.SessionRef.Name)
	tasks := &corev1alpha1.TaskList{}
	if err := reader.List(ctx, tasks,
		client.InNamespace(workspace.Namespace),
		client.MatchingFields{acpTaskSessionNameField: sessionName},
	); err != nil {
		return nil, err
	}
	classIdentityChecked := false
	classIdentityMatches := false
	var candidates []*corev1alpha1.Task
	for i := range tasks.Items {
		task := &tasks.Items[i]
		if excludeTaskUID != "" && task.UID == excludeTaskUID {
			continue
		}
		if task.Spec.SessionRef == nil || strings.TrimSpace(task.Spec.SessionRef.Name) != sessionName {
			continue
		}
		linkName := strings.TrimSpace(task.Labels[acpExecutionWorkspaceLinkLabel])
		linkUID := strings.TrimSpace(task.Annotations[acpExecutionWorkspaceUIDAnnotation])
		exactIncarnation := linkName == workspace.Name && linkUID == string(workspace.UID)
		if !exactIncarnation && linkName != "" {
			continue
		}
		if !exactIncarnation {
			if task.Spec.Execution == nil || task.Spec.Execution.Workspace == nil ||
				task.Spec.Execution.Workspace.ClassRef == nil ||
				task.Spec.Execution.Workspace.ClassRef.Name != workspace.Spec.ClassBinding.Name ||
				task.Spec.Execution.Workspace.ReusePolicy != corev1alpha1.WorkspaceReusePolicySession {
				continue
			}
			if !classIdentityChecked {
				classIdentityChecked = true
				class := &workspacev1alpha1.ExecutionWorkspaceClass{}
				err := reader.Get(ctx, types.NamespacedName{
					Namespace: workspace.Namespace, Name: workspace.Spec.ClassBinding.Name,
				}, class)
				switch {
				case apierrors.IsNotFound(err):
				case err != nil:
					return nil, err
				default:
					classIdentityMatches = class.UID == workspace.Spec.ClassBinding.UID
				}
			}
			if !classIdentityMatches {
				continue
			}
		}
		if !task.DeletionTimestamp.IsZero() ||
			task.Status.Phase == corev1alpha1.TaskPhaseSucceeded ||
			task.Status.Phase == corev1alpha1.TaskPhaseFailed ||
			task.Status.Phase == corev1alpha1.TaskPhaseCancelled {
			continue
		}
		candidates = append(candidates, task)
	}
	return candidates, nil
}

func acpWorkspaceSuspendedCapFromAnnotation(workspace *workspacev1alpha1.ExecutionWorkspace) *int32 {
	value, present := workspace.Annotations[acpWorkspaceMaxSuspendedAnnotation]
	if !present {
		return nil
	}
	raw := strings.TrimSpace(value)
	parsed, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || parsed < 0 {
		zero := int32(0)
		return &zero
	}
	limit := int32(parsed)
	return &limit
}

func (r *ACPWorkspaceRetentionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ours := predicate.NewPredicateFuncs(func(object client.Object) bool {
		return object.GetLabels()[workspacev1alpha1.ProviderControllerLabel] == acpWorkspaceControllerLabelValue || (object.GetLabels()[workspacev1alpha1.ProviderControllerLabel] != "" && object.GetAnnotations()[acpExecutionWorkspacePoolAnnotation] != "")
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacev1alpha1.ExecutionWorkspace{}).
		WithEventFilter(ours).
		Named("acp-workspace-retention").
		Complete(r)
}
