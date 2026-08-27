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

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/metrics"
)

const (
	// acpWorkspaceLastDetachedAnnotation records when the last Task attachment
	// was revoked. The class idleTimeout counts from this instant (or from
	// creation for a workspace that never attached).
	acpWorkspaceLastDetachedAnnotation = "acp.workspace.orka.ai/last-detached-at"
	// acpWorkspaceMaxSuspendedAnnotation freezes the class retention cap on
	// the materialized workspace so settlement enforces it without reloading
	// the execution snapshot.
	acpWorkspaceMaxSuspendedAnnotation = "acp.workspace.orka.ai/max-suspended"
	// acpWorkspaceRetentionRequeue bounds how often retention re-evaluates a
	// workspace with no imminent deadline.
	acpWorkspaceRetentionRequeue = 5 * time.Minute
	// One class-owned Lease stores at most one pending suspended-capacity
	// claim. Its resourceVersion serializes transactions across leader handoff;
	// settled occupancy is counted from live workspaces, so the annotation is
	// constant-size regardless of the configured cap.
	acpSuspendQuotaLeaseClassUIDAnnotation = "acp.workspace.orka.ai/suspend-quota-class-uid"
	acpSuspendQuotaLeaseClaimsAnnotation   = "acp.workspace.orka.ai/suspend-quota-claims"
	acpTaskSessionNameField                = "spec.sessionRef.name"
	maxACPSuspendQuotaPendingClaims        = 1
)

// ACPWorkspaceRetentionReconciler enforces the frozen class lifetime policy on
// class-backed ACP execution workspaces: idleTimeout bounds how long an
// unattached workspace may stay Ready or Suspended, and maxLifetime is the
// hard upper bound that forces terminal cleanup regardless of state. It acts
// in Orka core's role (it writes desired state and object deletion only);
// the workspace adapter keeps executing the transitions.
type ACPWorkspaceRetentionReconciler struct {
	client.Client
	// APIReader bypasses the informer cache for suspended-capacity counts so
	// a quota claim never trusts a stale list. Falls back to Client when nil.
	APIReader client.Reader
	Recorder  events.EventRecorder
	Now       func() time.Time
}

func (r *ACPWorkspaceRetentionReconciler) quotaReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

//nolint:gocyclo // The retention decision table stays auditable in one place.
func (r *ACPWorkspaceRetentionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, req.NamespacedName, workspace); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if workspace.Labels[workspacev1alpha1.ProviderControllerLabel] != acpWorkspaceProviderControllerName {
		return ctrl.Result{}, nil
	}
	if err := releaseObsoleteACPSuspendQuotaLease(ctx, r.Client, r.quotaReader(), workspace); err != nil {
		return ctrl.Result{}, err
	}
	if !workspace.DeletionTimestamp.IsZero() ||
		workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredDeleted {
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(workspace, executionWorkspaceFinalizer) {
		// The cleanup finalizer is the guarantee that an expiry delete runs
		// the linked RuntimePool teardown and records the terminal
		// disposition; deleting before the core controller installs it would
		// remove the object immediately and orphan the pool. Wait for it.
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}

	// The lifetime deadline never bypasses idle evaluation: a class whose
	// maxLifetime is nearer than the poll interval must still apply an
	// already-elapsed idleTimeout now, so the earliest applicable deadline
	// only clamps the requeue below.
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
	if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined {
		// Quarantined workspaces are never reused and skip ordinary idle
		// handling, but the frozen maxLifetime above remains the hard upper
		// bound so terminal records cannot leak forever.
		return ctrl.Result{RequeueAfter: lifetimeRequeue}, nil
	}

	if workspace.Spec.Attachment != nil || workspace.Status.AttachedEpoch > 0 {
		// A live attachment, or a revoked one whose epoch the adapter still
		// enforces, defers idle evaluation; the detach instant stamped at
		// revocation start opens a fresh idle window. spec.attachmentEpoch is
		// deliberately NOT consulted: it is the monotonic high-water mark and
		// stays positive forever after the first attachment.
		return ctrl.Result{RequeueAfter: lifetimeRequeue}, nil
	}
	idle := workspace.Spec.Lifecycle.IdleTimeout
	if idle == nil || idle.Duration <= 0 {
		// With idle retention disabled, demand cannot change any decision
		// here (every branch below the demand lookup returns the same
		// lifetime requeue), so the uncached requester lookup and the
		// namespace-wide continuation scan are skipped entirely instead of
		// running O(workspaces x Tasks) every five-minute requeue.
		return ctrl.Result{RequeueAfter: lifetimeRequeue}, nil
	}
	demandOutstanding, err := r.pendingWorkspaceDemandOutstanding(ctx, workspace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredReady && demandOutstanding {
		// A live continuation requested cold resume and has not attached yet;
		// the workspace is actively demanded even after the boot completes.
		// Observed Suspended or Suspending alone is not demand: if the requester
		// dies, idle retention resumes from the actual detach timestamp.
		return ctrl.Result{RequeueAfter: lifetimeRequeue}, nil
	}
	idleStart := workspace.CreationTimestamp.Time
	if value, present := workspace.Annotations[acpWorkspaceLastDetachedAnnotation]; present {
		raw := strings.TrimSpace(value)
		parsed, parseErr := time.Parse(time.RFC3339Nano, raw)
		if parseErr != nil {
			// The stamp is controller-written, so a malformed value means the
			// admission-protected metadata was corrupted. Falling back to the
			// creation time would treat a long-lived workspace as instantly
			// idle-expired and destroy or suspend it; fail closed on the
			// bounded maxLifetime path instead.
			r.recordRetention(workspace, "RetentionIdleStampInvalid",
				"the last-detached-at annotation is not RFC3339Nano; idle retention is held and only maxLifetime applies")
			return ctrl.Result{RequeueAfter: lifetimeRequeue}, nil
		}
		idleStart = parsed
	}
	deadline := idleStart.Add(idle.Duration)
	if now.Before(deadline) {
		requeue := min(deadline.Sub(now)+time.Second, lifetimeRequeue)
		return ctrl.Result{RequeueAfter: requeue}, nil
	}

	switch workspace.Spec.DesiredState {
	case workspacev1alpha1.ExecutionWorkspaceDesiredSuspended:
		if demandOutstanding {
			// A continuation already registered UID-bound demand (it can
			// stamp it while the suspension still settles); the retained
			// checkpoint is about to be resumed, not expired. maxLifetime
			// remains the hard bound if that requester dies.
			return ctrl.Result{RequeueAfter: lifetimeRequeue}, nil
		}
		// A suspended workspace past its idle timeout has exhausted its
		// retention: only terminal deletion is admitted until richer retention
		// dispositions exist.
		return ctrl.Result{}, r.expireWorkspace(ctx, workspace, "IdleRetentionExpired",
			"class idleTimeout elapsed for the suspended workspace; retention is exhausted", true)
	case workspacev1alpha1.ExecutionWorkspaceDesiredReady:
		// Task detach overrides govern settlement only. Once an unattached
		// Ready workspace exhausts idleTimeout, the frozen class default is
		// the retention policy regardless of the last Task's choice.
		if recordedAction, present := workspace.Annotations[acpWorkspaceDetachActionAnnotation]; present &&
			recordedAction != string(workspacev1alpha1.WorkspaceOnDetachSuspend) &&
			recordedAction != string(workspacev1alpha1.WorkspaceOnDetachDelete) {
			// The controller-written settlement action is corrupt or from a
			// newer binary. Hold cleanup even though idle retention uses the
			// class default: another controller path may still consume it.
			r.recordRetention(workspace, "UnknownIdleAction",
				"class idleTimeout elapsed, but the recorded Task detach action is not executable by this controller; failing closed")
			return ctrl.Result{RequeueAfter: lifetimeRequeue}, nil
		}
		idleAction := string(workspace.Spec.Lifecycle.DefaultOnDetach)
		if idleAction != string(workspacev1alpha1.WorkspaceOnDetachSuspend) &&
			idleAction != string(workspacev1alpha1.WorkspaceOnDetachDelete) {
			r.recordRetention(workspace, "UnknownIdleAction",
				"class idleTimeout elapsed, but the frozen class default is not executable by this controller; failing closed")
			return ctrl.Result{RequeueAfter: lifetimeRequeue}, nil
		}
		if workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateFailed {
			// The adapter treats Failed as terminal and will not retry a cold
			// resume or suspension. Starting another suspension would only
			// refresh last-detached-at and extend retained data past idleTimeout.
			return ctrl.Result{}, r.expireWorkspace(ctx, workspace, "FailedWorkspaceIdleExpired",
				"class idleTimeout elapsed for the terminally failed workspace; applying the Delete disposition", true)
		}
		if durableMarker, present := workspace.Annotations[acpWorkspaceDurableAnnotation]; idleAction == string(workspacev1alpha1.WorkspaceOnDetachSuspend) && present && durableMarker != booleanTrueValue {
			// A present durable marker is controller-authenticated. Invalid
			// content is corruption, not evidence that Delete is safe.
			r.recordRetention(workspace, "InvalidDurableCapability",
				"class idleTimeout elapsed, but the durable-workspace marker is invalid; failing closed")
			return ctrl.Result{RequeueAfter: lifetimeRequeue}, nil
		}
		if idleAction == string(workspacev1alpha1.WorkspaceOnDetachSuspend) &&
			runtimePoolWorkspaceSuspendableAnnotationPresent(workspace) {
			_, resumeUnfulfilled := workspace.Annotations[acpWorkspaceResumeRequestedAnnotation]
			preserveLastDetached := resumeUnfulfilled &&
				workspace.Annotations[acpWorkspaceResumedLineageAnnotation] == booleanTrueValue
			err := suspendACPWorkspaceWithinQuota(
				ctx, r.Client, r.quotaReader(), workspace, now, preserveLastDetached,
			)
			switch {
			case errors.Is(err, errACPSuspendQuotaExhausted):
				// The freed slot was consumed while this workspace idled; the
				// only admitted fallback disposition is Delete.
				return ctrl.Result{}, r.expireWorkspace(ctx, workspace, "SuspendQuotaExhausted",
					"class idleTimeout elapsed and the retention cap is exhausted; applying the Delete disposition", true)
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
		return ctrl.Result{RequeueAfter: lifetimeRequeue}, nil
	}
}

// runtimePoolWorkspaceSuspendableAnnotationPresent reports whether the
// materialized workspace carries frozen evidence that its class profile
// permitted DataOnly suspension. The detach action remains accepted for
// workspaces created before the durable marker was introduced.
func runtimePoolWorkspaceSuspendableAnnotationPresent(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	durable, present := workspace.Annotations[acpWorkspaceDurableAnnotation]
	if present {
		return durable == booleanTrueValue
	}
	return workspace.Spec.Lifecycle.DefaultOnDetach == workspacev1alpha1.WorkspaceOnDetachSuspend ||
		workspace.Annotations[acpWorkspaceDetachActionAnnotation] == string(workspacev1alpha1.WorkspaceOnDetachSuspend)
}

func (r *ACPWorkspaceRetentionReconciler) expireWorkspace(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	reason, message string,
	fenced bool,
) error {
	// Idle-triggered deletions are fenced with UID+resourceVersion so a
	// concurrent attachment or resume settles as a retried conflict instead
	// of destroying a workspace that became actively demanded; the
	// maxLifetime hard bound stays intentionally unconditional.
	preconditions := []client.DeleteOption{client.Preconditions{UID: &workspace.UID}}
	if fenced {
		preconditions = deleteCurrentObjectPreconditions(workspace)
	}
	if err := r.Delete(ctx, workspace, preconditions...); err != nil && !apierrors.IsNotFound(err) {
		if fenced && apierrors.IsConflict(err) {
			// The fenced deletion lost to a concurrent update (an attachment
			// or resume made the workspace actively demanded); nothing was
			// applied, so no Event or action metric is recorded.
			return nil
		}
		return err
	}
	r.recordRetention(workspace, reason, message)
	metrics.RecordACPWorkspaceRetentionAction("delete", strings.ToLower(reason))
	return nil
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

// acpSuspendQuotaLocks avoids redundant Lease contention inside one process.
// The Kubernetes Lease below fences claims across leader handoff. Entries
// remain only while a holder or waiter references them, so deleted and
// recreated classes do not accumulate process-local locks.
var (
	acpSuspendQuotaLocksMu sync.Mutex
	acpSuspendQuotaLocks   = map[string]*acpSuspendQuotaLockEntry{}
)

// errACPSuspendQuotaExhausted reports a claim rejected by the frozen class
// retention cap; the caller applies its fallback disposition.
var errACPSuspendQuotaExhausted = errors.New("class suspended-workspace retention cap is exhausted")

// errACPSuspendQuotaBusy asks the caller to retry after another workspace's
// Kubernetes-backed count-and-patch transaction finishes or is recovered.
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
		if workspace.Labels[workspacev1alpha1.ProviderControllerLabel] != acpWorkspaceProviderControllerName ||
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
	workspaces, err := listACPSuspendQuotaWorkspaces(
		ctx, reader, workspace.Namespace, workspace.Spec.ClassBinding.UID,
	)
	if err != nil {
		return err
	}
	lease := &coordinationv1.Lease{}
	key := types.NamespacedName{
		Namespace: workspace.Namespace,
		Name:      acpSuspendQuotaLeaseName(workspace.Spec.ClassBinding.UID),
	}
	err = reader.Get(ctx, key, lease)
	if apierrors.IsNotFound(err) {
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
		desired, buildErr := newACPSuspendQuotaLease(workspace, claims)
		if buildErr != nil {
			return buildErr
		}
		if createErr := writer.Create(ctx, desired); createErr != nil {
			if apierrors.IsAlreadyExists(createErr) {
				return errACPSuspendQuotaBusy
			}
			return fmt.Errorf("create suspension quota Lease: %w", createErr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read suspension quota Lease: %w", err)
	}
	claims, err := readACPSuspendQuotaClaims(
		lease, workspace.Spec.ClassBinding.Name, workspace.Spec.ClassBinding.UID,
	)
	if err != nil {
		return err
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
	if !workspaceConsumesSuspendedQuota(workspace) && workspace.DeletionTimestamp.IsZero() &&
		workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredReady &&
		claim.WorkspaceName == workspace.Name && claim.ResourceVersion == workspace.ResourceVersion {
		// The workspace write recorded by this pending claim can still succeed.
		// Keep the slot reserved so a replacement leader can finish it.
		return nil
	}
	delete(claims, uid)
	return patchACPSuspendQuotaClaims(ctx, writer, lease, claims)
}

// suspendACPWorkspaceWithinQuota atomically claims one suspension slot under
// the workspace's frozen retention cap and patches it to Suspended, stamping
// the detach instant so the suspended state earns a full retention interval.
// It returns errACPSuspendQuotaExhausted when the cap is already consumed and
// passes patch conflicts through for the caller's requeue policy.
func suspendACPWorkspaceWithinQuota(
	ctx context.Context,
	writer client.Client,
	reader client.Reader,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	now time.Time,
	preserveLastDetached bool,
) error {
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
	// Suspension settles any pending provisioning or resume demand; a later
	// continuation stamps fresh demand when it flips the workspace back.
	delete(workspace.Annotations, acpWorkspaceResumeRequestedAnnotation)
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	return writer.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func workspaceConsumesSuspendedQuota(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	if workspace == nil {
		return false
	}
	_, durableMarkerPresent := workspace.Annotations[acpWorkspaceDurableAnnotation]
	durableDataAbsent := workspace.Annotations[acpWorkspaceDurableDataAbsentAnnotation] == booleanTrueValue
	if workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateFailed &&
		(!durableMarkerPresent || durableDataAbsent) {
		return false
	}
	suspendedCharge := workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	coldResumeCharge := workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredReady &&
		(workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateSuspended ||
			workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateSuspending) &&
		!durableDataAbsent
	failedDurableCharge := workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateFailed &&
		durableMarkerPresent
	deletingCharge := !workspace.DeletionTimestamp.IsZero() &&
		durableMarkerPresent &&
		workspace.Status.Disposition == nil
	return suspendedCharge || coldResumeCharge || failedDurableCharge || deletingCharge
}

// countSuspendedClassWorkspaces counts suspended workspaces bound to the exact
// class UID in one namespace, skipping candidates the exclude predicate
// accepts.
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
		if workspace.Labels[workspacev1alpha1.ProviderControllerLabel] != acpWorkspaceProviderControllerName ||
			workspace.Spec.ClassBinding.UID != classUID || (exclude != nil && exclude(workspace)) {
			continue
		}
		if workspaceConsumesSuspendedQuota(workspace) {
			count++
		}
	}
	return count, nil
}

// pendingWorkspaceDemandOutstanding reports whether the demand record on the
// workspace still has a live requester. The record binds the requesting
// Task's name; when that Task is gone or terminal, no attachment can ever
// fulfil the demand and ordinary idle handling resumes, so a create/link
// crash window cannot leak the workspace forever.
func (r *ACPWorkspaceRetentionReconciler) pendingWorkspaceDemandOutstanding(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (bool, error) {
	if outstanding, err := r.recordedWorkspaceDemandLive(ctx, workspace); err != nil || outstanding {
		return outstanding, err
	}
	// The single UID-bound stamp records only the LAST writer: when several
	// Tasks queue for the same suspended Session workspace, a later requester
	// overwrites an earlier one, and the recorded requester terminating must
	// not surrender the workspace while another live continuation still
	// waits. Any live, non-terminal Task on the workspace's Session keeps
	// demand outstanding; maxLifetime remains the hard bound.
	return r.liveSessionContinuationExists(ctx, workspace)
}

// recordedWorkspaceDemandLive reports whether the UID-bound demand stamp
// names a live, non-terminal requester.
func (r *ACPWorkspaceRetentionReconciler) recordedWorkspaceDemandLive(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (bool, error) {
	value, present := workspace.Annotations[acpWorkspaceResumeRequestedAnnotation]
	if !present {
		return false, nil
	}
	raw := strings.TrimSpace(value)
	if raw == "" {
		// The stamp is controller-written. A present but empty value means the
		// protected metadata was corrupted, so fail closed instead of expiring
		// a workspace whose requester may still be provisioning.
		return true, nil
	}
	fields := strings.Fields(raw)
	if len(fields) < 2 {
		// A legacy stamp without requester identity is honored; maxLifetime
		// remains its hard bound.
		return true, nil
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
		// A replacement Task under the recycled namespace/name is not the
		// requester; its unrelated lifetime must not keep stale demand alive.
		return false, nil
	}
	if !task.DeletionTimestamp.IsZero() ||
		task.Status.Phase == corev1alpha1.TaskPhaseSucceeded ||
		task.Status.Phase == corev1alpha1.TaskPhaseFailed ||
		task.Status.Phase == corev1alpha1.TaskPhaseCancelled {
		// The requester can never attach; its settlement (or deletion
		// settlement) owns any remaining cleanup.
		return false, nil
	}
	return true, nil
}

// liveSessionContinuationExists reports whether any live, non-terminal Task
// targets the workspace's Session. The Task CRD exposes sessionRef.name as a
// selectable field, so the uncached read remains bounded to that Session.
func (r *ACPWorkspaceRetentionReconciler) liveSessionContinuationExists(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (bool, error) {
	return liveACPSessionContinuationExists(ctx, r.quotaReader(), workspace)
}

// liveACPSessionContinuationExists reports whether any live, non-terminal
// Task in the workspace's namespace targets this exact workspace incarnation
// through its Session. The reader uses the Task CRD's server-side selectable
// field; list errors fail closed as outstanding demand.
func liveACPSessionContinuationExists(
	ctx context.Context,
	reader client.Reader,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (bool, error) {
	successors, err := liveACPSessionContinuations(ctx, reader, workspace, "")
	if err != nil {
		return true, err
	}
	return len(successors) > 0, nil
}

// liveACPSessionContinuations returns every live, non-terminal continuation
// Task targeting this exact workspace incarnation, in list order. It fails
// closed (error, treated as demand outstanding by callers) on list errors.
// Returning ALL candidates lets settlement scan past one ineligible waiter
// (an out-of-policy override, a recreated Session) instead of concluding no
// successor exists while a later valid continuation is queued.
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
	// The class-UID verification for unlinked candidates is resolved once: a
	// class deleted and recreated under the same name carries a different
	// immutable UID, and its Tasks resolve a NEW workspace incarnation, so
	// they are never demand for this one.
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
		// Demand binds to this workspace INCARNATION, not the Session name: a
		// Session deleted and recreated under the same namespace/name
		// resolves a different immutable Session UID, and its Tasks can never
		// attach here. A waiter that has been reconciled carries the
		// controller-written workspace link (name plus incarnation UID);
		// verification rejects cross-incarnation links before they are ever
		// stamped, so a Task linked elsewhere is never demand for this
		// workspace. A not-yet-linked waiter counts until its first
		// reconcile either links it here or fails it terminally.
		linkName := strings.TrimSpace(task.Labels[acpExecutionWorkspaceLinkLabel])
		linkUID := strings.TrimSpace(task.Annotations[acpExecutionWorkspaceUIDAnnotation])
		exactIncarnation := linkName == workspace.Name && linkUID == string(workspace.UID)
		if !exactIncarnation && linkName != "" {
			continue
		}
		if !exactIncarnation {
			// An unlinked Task counts only when it actually requests a
			// session-reused execution workspace: a plain transcript-backed
			// Task sharing the Session name can never attach here, and
			// counting it would suppress idle retention (and hold quota) for
			// its entire lifetime.
			if task.Spec.Execution == nil || task.Spec.Execution.Workspace == nil ||
				task.Spec.Execution.Workspace.ClassRef == nil ||
				task.Spec.Execution.Workspace.ClassRef.Name != workspace.Spec.ClassBinding.Name ||
				task.Spec.Execution.Workspace.ReusePolicy != corev1alpha1.WorkspaceReusePolicySession {
				// Different classes deliberately produce separate workspace
				// incarnations, and the legacy enabled-workspace path never
				// binds a class workspace: a Task that cannot resolve to
				// THIS workspace must not defer its settlement or hold its
				// retention.
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
					// The frozen class is gone: name-matched waiters resolve
					// nothing (or a future recreation's NEW incarnation).
				case err != nil:
					return nil, err
				default:
					classIdentityMatches = class.UID == workspace.Spec.ClassBinding.UID
				}
			}
			if !classIdentityMatches {
				// The class was deleted and recreated under the same name:
				// this waiter's classRef resolves the REPLACEMENT class and a
				// different workspace incarnation, so it must not suppress
				// this workspace's settlement or idle retention.
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

// acpWorkspaceSuspendedCapFromAnnotation parses the frozen retention cap
// recorded on the materialized workspace, or nil when unbounded.
func acpWorkspaceSuspendedCapFromAnnotation(workspace *workspacev1alpha1.ExecutionWorkspace) *int32 {
	value, present := workspace.Annotations[acpWorkspaceMaxSuspendedAnnotation]
	if !present {
		// Absent means the class froze no cap: retention is unbounded by
		// design.
		return nil
	}
	raw := strings.TrimSpace(value)
	parsed, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || parsed < 0 {
		// A present-but-invalid frozen value fails closed as an exhausted cap
		// (zero) instead of silently disabling the class's hard quota.
		zero := int32(0)
		return &zero
	}
	limit := int32(parsed)
	return &limit
}

// SetupWithManager registers retention enforcement for ACP class workspaces.
func (r *ACPWorkspaceRetentionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ours := predicate.NewPredicateFuncs(func(object client.Object) bool {
		return object.GetLabels()[workspacev1alpha1.ProviderControllerLabel] == acpWorkspaceProviderControllerName
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacev1alpha1.ExecutionWorkspace{}).
		WithEventFilter(ours).
		Named("acp-workspace-retention").
		Complete(r)
}
