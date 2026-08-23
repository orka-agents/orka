/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/pkg/workspaceprovider"
)

const (
	// acpExecutionWorkspaceLinkLabel links a RuntimePool or Task to its
	// controller-first ExecutionWorkspace by Orka-owned name. It never carries
	// a provider-native identifier.
	acpExecutionWorkspaceLinkLabel = "acp.workspace.orka.ai/execution-workspace"
	// acpExecutionWorkspacePoolAnnotation records the deterministic RuntimePool
	// name bound to a class-backed ExecutionWorkspace.
	acpExecutionWorkspacePoolAnnotation = "acp.workspace.orka.ai/runtime-pool"
	// acpWorkspaceDetachActionAnnotation records the frozen effective detach
	// action of the currently attached Task so settlement applies exactly the
	// validated choice without reloading the execution snapshot.
	acpWorkspaceDetachActionAnnotation = "acp.workspace.orka.ai/detach-action"
	// acpExecutionWorkspaceUIDAnnotation pins the exact ExecutionWorkspace
	// incarnation a Task attached, alongside the name-bearing link label.
	// Settlement acts only on that incarnation: a Session deleted and
	// recreated under the same name produces a different workspace UID, and
	// an old Task must never revoke or delete the new incarnation's
	// workspace.
	acpExecutionWorkspaceUIDAnnotation = "acp.workspace.orka.ai/execution-workspace-uid"
	// acpWorkspaceNamePrefix prefixes deterministic class-backed workspace names.
	acpWorkspaceNamePrefix = "acp-ws"
	// acpWorkspaceProviderConfigUIDAnnotation pins the exact cluster-scoped
	// RuntimeProviderConfig that selected this workspace's physical backend.
	acpWorkspaceProviderConfigUIDAnnotation = "acp.workspace.orka.ai/provider-config-uid"
	// acpWorkspaceBackendAnnotation records the resolved physical backend so a
	// recreated same-name provider config can never silently re-serve an
	// existing workspace through a different backend.
	acpWorkspaceBackendAnnotation = "acp.workspace.orka.ai/backend"
	// acpWorkspaceRevocationStartedAnnotation stamps the first revocation
	// attempt so settlement can enforce the frozen detachTimeout instead of
	// requeueing forever behind an adapter that never releases the epoch.
	acpWorkspaceRevocationStartedAnnotation = "acp.workspace.orka.ai/revocation-started-at"
	// acpWorkspaceAttachmentTTL bounds one ACP Task attachment. The attachment
	// Secret is not the RuntimePool data-plane credential; the epoch enforces
	// the one-writer rule, and class maxLifetime still clamps the expiry.
	acpWorkspaceAttachmentTTL = 24 * time.Hour
)

// errACPWorkspaceBindingConflict marks fail-closed adoption mismatches: an
// existing ExecutionWorkspace does not carry the exact frozen bindings, so the
// Task must fail instead of executing in an unverified workspace.
var errACPWorkspaceBindingConflict = errors.New("execution workspace binding conflict")

// acpClassWorkspaceName derives the deterministic controller-first workspace
// name for a frozen class-backed binding.
func acpClassWorkspaceName(task *corev1alpha1.Task, binding *ACPRuntimeWorkspaceBinding) string {
	sessionUID := types.UID("")
	if binding.ReusePolicy == corev1alpha1.WorkspaceReusePolicySession {
		sessionUID = types.UID(binding.SessionUID)
	}
	identity := workspaceprovider.InteractiveWorkspaceIdentity(
		task.Namespace, sessionUID, task.UID, types.UID(binding.Class.UID), binding.WorkspaceSlot,
	)
	return workspaceprovider.WorkspaceName(acpWorkspaceNamePrefix, identity)
}

// ensureACPClassWorkspace materializes, verifies, and attaches the
// controller-first ExecutionWorkspace for a class-backed plan. It returns the
// workspace name once the workspace is core-admitted and this Task holds the
// exclusive attachment; ready=false requests a requeue while admission,
// adapter status, or a competing attachment settles.
//
//nolint:gocyclo // Materialization, fail-closed adoption, and attachment stay auditable together.
func (r *TaskReconciler) ensureACPClassWorkspace(
	ctx context.Context,
	task *corev1alpha1.Task,
	plan ACPRuntimePlan,
) (string, bool, error) {
	if plan.Workspace == nil || plan.Workspace.Class == nil {
		return "", true, nil
	}
	binding := plan.Workspace
	name := acpClassWorkspaceName(task, binding)
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace)
	if apierrors.IsNotFound(err) {
		workspace, err = r.createACPClassWorkspace(ctx, task, binding, plan.PoolName, name)
		if err != nil {
			if apierrors.IsAlreadyExists(err) {
				return "", false, nil
			}
			return "", false, err
		}
	} else if err != nil {
		return "", false, err
	}
	if !workspace.DeletionTimestamp.IsZero() {
		// The predecessor detach is still deleting the deterministic-name
		// workspace; the replacement is created once deletion completes.
		return "", false, nil
	}
	if err := verifyACPClassWorkspace(workspace, task, binding); err != nil {
		return "", false, err
	}
	// The settlement link is persisted as soon as the deterministic workspace
	// exists: a Task deleted while core admission is still pending must be
	// able to settle (and apply its frozen detach action to) the session
	// workspace it materialized, which carries no Task owner reference.
	if err := r.linkTaskToACPWorkspace(ctx, task, workspace); err != nil {
		return "", false, err
	}
	if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
		// A continuation Task requests cold resume: the same concrete
		// workspace returns to Ready, and the adapter replaces the physical
		// runtime with a fresh boot and credential bootstrap before this
		// Task can attach. The flip waits for the suspension to actually
		// settle: resuming while the backend still drains or checkpoints
		// would lift the pool's suspend intent mid-flight and strand the
		// generic serving path in Draining with no undrain transition.
		switch workspace.Status.State {
		case workspacev1alpha1.ExecutionWorkspaceStateSuspended:
			base := workspace.DeepCopy()
			workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredReady
			if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return "", false, client.IgnoreNotFound(err)
			}
			return "", false, nil
		case workspacev1alpha1.ExecutionWorkspaceStateFailed:
			// The suspension failed closed and preserved no data checkpoint;
			// resuming would fabricate a workspace the contract cannot back.
			return "", false, fmt.Errorf(
				"%w: workspace %s suspension failed without a preserved data checkpoint; cold resume is impossible",
				errACPWorkspaceBindingConflict, workspace.Name,
			)
		default:
			return "", false, nil
		}
	}
	if !workspaceCurrentlyAdmittedByCore(workspace) {
		return "", false, nil
	}
	if workspace.Spec.Attachment != nil {
		if workspace.Spec.Attachment.TaskRef.UID == task.UID {
			if err := r.recordACPWorkspaceDetachAction(ctx, workspace, binding); err != nil {
				return "", false, err
			}
			return name, true, r.linkTaskToACPWorkspace(ctx, task, workspace)
		}
		// Another Task holds the one-writer attachment. Session-reused
		// workspaces queue behind it; the deterministic per-Task name makes
		// this unreachable for reuse none.
		return "", false, nil
	}
	if workspace.Annotations[acpWorkspaceRevocationStartedAnnotation] != "" &&
		workspace.Annotations[acpWorkspaceDetachActionAnnotation] != string(workspacev1alpha1.WorkspaceOnDetachSuspend) {
		// A Delete settlement is pending on this workspace incarnation: the
		// prior Task's frozen detach action must destroy this filesystem
		// before any continuation runs. The deterministic name is recreated
		// fresh once the deletion lands; attaching now would execute in state
		// the class policy required to destroy. Suspend settlements retire
		// their stamp when the suspension patch lands, and cold resume then
		// admits the continuation.
		return "", false, nil
	}
	// Persist the settlement link before attaching: if the controller dies
	// between attachment and a later link patch, a session-scoped workspace
	// would stay attached to a Task that cleanup can no longer associate with
	// it, and every later Task for that Session would queue forever.
	if err := r.linkTaskToACPWorkspace(ctx, task, workspace); err != nil {
		return "", false, err
	}
	manager := WorkspaceAttachmentManager{Client: r.Client, LeaseTTL: acpWorkspaceAttachmentTTL}
	if _, err := manager.Attach(ctx, workspace, task); err != nil {
		if errors.Is(err, ErrWorkspaceAttachmentLocked) {
			return "", false, nil
		}
		if strings.Contains(err.Error(), "not reusable") || strings.Contains(err.Error(), "revalidate workspace") {
			// The adapter has not yet observed the current generation or the
			// workspace is still provisioning; attachment retries.
			return "", false, nil
		}
		return "", false, err
	}
	if err := r.recordACPWorkspaceDetachAction(ctx, workspace, binding); err != nil {
		return "", false, err
	}
	return name, true, nil
}

// recordACPWorkspaceDetachAction records the attached Task's frozen effective
// detach action on the workspace so settlement applies exactly that choice.
func (r *TaskReconciler) recordACPWorkspaceDetachAction(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	binding *ACPRuntimeWorkspaceBinding,
) error {
	action := binding.Class.EffectiveOnDetach
	if workspace.Annotations[acpWorkspaceDetachActionAnnotation] == action {
		return nil
	}
	base := workspace.DeepCopy()
	if workspace.Annotations == nil {
		workspace.Annotations = map[string]string{}
	}
	workspace.Annotations[acpWorkspaceDetachActionAnnotation] = action
	return r.Patch(ctx, workspace, client.MergeFrom(base))
}

// linkTaskToACPWorkspace records the workspace name on the Task so terminal
// settlement and Task finalization find the workspace without reloading the
// execution snapshot. The label value is an Orka-owned deterministic name.
func (r *TaskReconciler) linkTaskToACPWorkspace(
	ctx context.Context,
	task *corev1alpha1.Task,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) error {
	if task.Labels[acpExecutionWorkspaceLinkLabel] == workspace.Name &&
		task.Annotations[acpExecutionWorkspaceUIDAnnotation] == string(workspace.UID) {
		return nil
	}
	base := task.DeepCopy()
	if task.Labels == nil {
		task.Labels = make(map[string]string)
	}
	if task.Annotations == nil {
		task.Annotations = make(map[string]string)
	}
	task.Labels[acpExecutionWorkspaceLinkLabel] = workspace.Name
	task.Annotations[acpExecutionWorkspaceUIDAnnotation] = string(workspace.UID)
	return r.Patch(ctx, task, client.MergeFrom(base))
}

func (r *TaskReconciler) createACPClassWorkspace(
	ctx context.Context,
	task *corev1alpha1.Task,
	binding *ACPRuntimeWorkspaceBinding,
	poolName string,
	name string,
) (*workspacev1alpha1.ExecutionWorkspace, error) {
	lifecycle, err := binding.Class.Lifecycle()
	if err != nil {
		return nil, err
	}
	workspace := &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: task.Namespace,
			Name:      name,
			Labels: map[string]string{
				workspacev1alpha1.ProviderControllerLabel: acpWorkspaceProviderControllerName,
			},
			Annotations: map[string]string{
				acpExecutionWorkspacePoolAnnotation:     poolName,
				acpWorkspaceProviderConfigUIDAnnotation: binding.Class.ProviderConfigUID,
				acpWorkspaceBackendAnnotation:           string(binding.Provider),
				// The creator's frozen effective detach action is bound
				// atomically with the workspace's existence: a crash between
				// Attach and the later refresh patch can never leave a
				// Suspend-frozen workspace defaulting to Delete.
				acpWorkspaceDetachActionAnnotation: binding.Class.EffectiveOnDetach,
			},
		},
		Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
			Mode: workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			ClassBinding: workspacev1alpha1.ImmutableObjectBinding{
				Name:        binding.Class.Name,
				UID:         types.UID(binding.Class.UID),
				Generation:  binding.Class.Generation,
				ProfileHash: binding.Class.ProfileHash,
			},
			ProviderBinding: workspacev1alpha1.ImmutableObjectBinding{
				Name:       binding.Class.ProviderName,
				UID:        types.UID(binding.Class.ProviderUID),
				Generation: binding.Class.ProviderGeneration,
			},
			Slot:         binding.WorkspaceSlot,
			DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredReady,
			Lifecycle:    lifecycle,
		},
	}
	if binding.ReusePolicy == corev1alpha1.WorkspaceReusePolicySession {
		if task.Spec.SessionRef == nil || strings.TrimSpace(task.Spec.SessionRef.Name) == "" {
			return nil, fmt.Errorf("session-reused execution workspace requires spec.sessionRef.name")
		}
		workspace.Spec.SessionRef = &workspacev1alpha1.ObjectIdentityReference{
			Name: strings.TrimSpace(task.Spec.SessionRef.Name),
			UID:  types.UID(binding.SessionUID),
		}
	} else {
		// A per-Task workspace is garbage-collected with its Task even if the
		// explicit settle-time deletion never runs. The reference is
		// deliberately not a controller reference: Task workspace status is
		// projected by the ACP execution state machine, and a controller owner
		// would let the generic owner projection fight it.
		if err := controllerutil.SetOwnerReference(task, workspace, r.Scheme); err != nil {
			return nil, fmt.Errorf("set execution workspace owner: %w", err)
		}
	}
	if err := r.Create(ctx, workspace); err != nil {
		return nil, err
	}
	return workspace, nil
}

// verifyACPClassWorkspace fail-closes adoption: an existing workspace must
// carry exactly the frozen class, provider, session, and slot bindings.
func verifyACPClassWorkspace(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	binding *ACPRuntimeWorkspaceBinding,
) error {
	if workspace == nil {
		return fmt.Errorf("%w: workspace is missing", errACPWorkspaceBindingConflict)
	}
	if !workspace.DeletionTimestamp.IsZero() {
		return fmt.Errorf("%w: workspace %s is deleting", errACPWorkspaceBindingConflict, workspace.Name)
	}
	if workspace.Labels[workspacev1alpha1.QuarantinedLabel] == booleanTrueValue {
		return fmt.Errorf("%w: workspace %s is quarantined and must never be reused", errACPWorkspaceBindingConflict, workspace.Name)
	}
	if workspace.Spec.Mode != workspacev1alpha1.ExecutionWorkspaceModeInteractive {
		return fmt.Errorf("%w: workspace %s mode %q is not Interactive", errACPWorkspaceBindingConflict, workspace.Name, workspace.Spec.Mode)
	}
	if workspace.Spec.ClassBinding.Name != binding.Class.Name ||
		string(workspace.Spec.ClassBinding.UID) != binding.Class.UID ||
		workspace.Spec.ClassBinding.Generation != binding.Class.Generation ||
		workspace.Spec.ClassBinding.ProfileHash != binding.Class.ProfileHash {
		return fmt.Errorf("%w: workspace %s class binding does not match the frozen class identity", errACPWorkspaceBindingConflict, workspace.Name)
	}
	if workspace.Spec.ProviderBinding.Name != binding.Class.ProviderName ||
		string(workspace.Spec.ProviderBinding.UID) != binding.Class.ProviderUID ||
		workspace.Spec.ProviderBinding.Generation != binding.Class.ProviderGeneration {
		return fmt.Errorf("%w: workspace %s provider binding does not match the frozen provider identity", errACPWorkspaceBindingConflict, workspace.Name)
	}
	if workspace.Annotations[acpWorkspaceProviderConfigUIDAnnotation] != binding.Class.ProviderConfigUID ||
		workspace.Annotations[acpWorkspaceBackendAnnotation] != string(binding.Provider) {
		return fmt.Errorf("%w: workspace %s provider config or backend does not match the frozen binding", errACPWorkspaceBindingConflict, workspace.Name)
	}
	if workspace.Spec.Slot != binding.WorkspaceSlot {
		return fmt.Errorf("%w: workspace %s slot does not match the frozen binding", errACPWorkspaceBindingConflict, workspace.Name)
	}
	if binding.ReusePolicy == corev1alpha1.WorkspaceReusePolicySession {
		if workspace.Spec.SessionRef == nil || string(workspace.Spec.SessionRef.UID) != binding.SessionUID {
			return fmt.Errorf("%w: workspace %s session identity does not match the immutable Session UID", errACPWorkspaceBindingConflict, workspace.Name)
		}
	} else {
		owned := false
		for _, owner := range workspace.OwnerReferences {
			if owner.UID == task.UID {
				owned = true
				break
			}
		}
		if workspace.Spec.SessionRef != nil || !owned {
			return fmt.Errorf("%w: workspace %s is not owned by this Task", errACPWorkspaceBindingConflict, workspace.Name)
		}
	}
	if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredDeleted ||
		workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined {
		return fmt.Errorf("%w: workspace %s desired state %q cannot admit new work", errACPWorkspaceBindingConflict, workspace.Name, workspace.Spec.DesiredState)
	}
	return nil
}

// settleACPClassWorkspace revokes this Task's attachment and applies the
// Delete detach action once the Task is terminal or finalizing. Every step is
// idempotent and restart-safe; done=false requests a requeue while the
// adapter still enforces the revoked epoch.
func (r *TaskReconciler) settleACPClassWorkspace(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	if task == nil {
		return true, nil
	}
	name := strings.TrimSpace(task.Labels[acpExecutionWorkspaceLinkLabel])
	if name == "" {
		return true, nil
	}
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if !workspace.DeletionTimestamp.IsZero() {
		return true, nil
	}
	// The link label is mutable Task metadata: revalidate that the named
	// workspace is an ACP-owned workspace that actually belongs to this Task
	// (or its Session) before revoking or deleting anything with controller
	// privileges. An unverified target is skipped, never acted on.
	if !settlementWorkspaceBelongsToTask(workspace, task) {
		return true, nil
	}
	if recorded := task.Annotations[acpExecutionWorkspaceUIDAnnotation]; recorded != "" &&
		recorded != string(workspace.UID) {
		// The named workspace is a different incarnation (for example a
		// Session recreated under the same name); this Task's settlement
		// never acts on it.
		return true, nil
	}
	manager := WorkspaceAttachmentManager{Client: r.Client}
	if attachment := workspace.Spec.Attachment; attachment != nil {
		if attachment.TaskRef.UID != task.UID {
			// Another Task attached the session-reused workspace; its own
			// settle flow owns revocation and the detach action.
			return true, nil
		}
		// The pending-detach stamp becomes durable BEFORE the attachment
		// clears: a continuation observing a nil attachment must also see the
		// pending Delete settlement and wait for the fresh workspace.
		if err := r.markACPWorkspaceRevocationStarted(ctx, workspace, attachment.Epoch); err != nil {
			return false, err
		}
		if err := manager.BeginRevocation(ctx, workspace, attachment.Epoch); err != nil {
			return false, err
		}
		if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace); err != nil {
			return false, client.IgnoreNotFound(err)
		}
	}
	if workspace.Spec.Attachment == nil && workspace.Spec.AttachmentEpoch > 0 {
		epoch := workspace.Spec.AttachmentEpoch
		// Restart-safe: ensure the deadline stamp exists for exactly this
		// epoch before enforcing it.
		if err := r.markACPWorkspaceRevocationStarted(ctx, workspace, epoch); err != nil {
			return false, err
		}
		if err := manager.FinalizeRevocation(ctx, workspace, epoch, attachmentSecretName(workspace.Name, epoch)); err != nil {
			// The adapter has not cleared the enforced epoch yet. The frozen
			// detachTimeout bounds how long Task finalization waits: past the
			// deadline the workspace is quarantined fail-closed and this Task
			// releases instead of retaining its finalizer forever.
			expired, deadlineErr := r.quarantineACPWorkspacePastDetachTimeout(ctx, workspace)
			if deadlineErr != nil {
				return false, deadlineErr
			}
			return expired, nil
		}
	}
	if workspace.Spec.Attachment != nil {
		return true, nil
	}
	// Apply the attached Task's frozen effective detach action. Suspend
	// quiesced above: the attachment is revoked and the adapter released the
	// enforced epoch, so no prompt or workspace writer remains. The
	// UID+resourceVersion preconditions keep a concurrent re-attachment from
	// losing its workspace: the API server rejects the write if the object
	// changed after this read, and a Task that attached in between settles at
	// its own settle time anyway.
	if workspace.Annotations[acpWorkspaceDetachActionAnnotation] == string(workspacev1alpha1.WorkspaceOnDetachSuspend) {
		if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
			return true, nil
		}
		base := workspace.DeepCopy()
		// The suspension retires the revocation stamp: the detach settled
		// with a preserved workspace, and a continuation's cold resume must
		// not be blocked by a stale pending-detach record.
		delete(workspace.Annotations, acpWorkspaceRevocationStartedAnnotation)
		workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
		if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
	if err := r.Delete(ctx, workspace, deleteCurrentObjectPreconditions(workspace)...); err != nil &&
		!apierrors.IsNotFound(err) {
		if apierrors.IsConflict(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// settlementWorkspaceBelongsToTask revalidates a settlement target found
// through the mutable Task link label: it must be an ACP-owned workspace whose
// session identity matches this Task's Session, or a per-Task workspace owned
// by exactly this Task.
func settlementWorkspaceBelongsToTask(workspace *workspacev1alpha1.ExecutionWorkspace, task *corev1alpha1.Task) bool {
	if workspace.Labels[workspacev1alpha1.ProviderControllerLabel] != acpWorkspaceProviderControllerName {
		return false
	}
	if workspace.Spec.SessionRef != nil {
		return task.Spec.SessionRef != nil &&
			strings.TrimSpace(task.Spec.SessionRef.Name) == workspace.Spec.SessionRef.Name
	}
	for _, owner := range workspace.OwnerReferences {
		if owner.UID == task.UID {
			return true
		}
	}
	return false
}

// markACPWorkspaceRevocationStarted stamps the revocation instant for exactly
// one attachment epoch ("<epoch> <RFC3339Nano>"), so the frozen detachTimeout
// is measured per revocation: a stale stamp from a previous epoch can never
// serve as a later attachment's deadline and instantly quarantine a live
// workspace.
func (r *TaskReconciler) markACPWorkspaceRevocationStarted(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	epoch int64,
) error {
	if stampedEpoch, _, ok := parseACPWorkspaceRevocationStamp(workspace.Annotations[acpWorkspaceRevocationStartedAnnotation]); ok && stampedEpoch == epoch {
		return nil
	}
	base := workspace.DeepCopy()
	if workspace.Annotations == nil {
		workspace.Annotations = map[string]string{}
	}
	workspace.Annotations[acpWorkspaceRevocationStartedAnnotation] = fmt.Sprintf("%d %s", epoch, time.Now().UTC().Format(time.RFC3339Nano))
	if err := r.Patch(ctx, workspace, client.MergeFrom(base)); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// parseACPWorkspaceRevocationStamp parses an epoch-bound revocation stamp.
func parseACPWorkspaceRevocationStamp(raw string) (int64, time.Time, bool) {
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) != 2 {
		return 0, time.Time{}, false
	}
	epoch, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, time.Time{}, false
	}
	startedAt, err := time.Parse(time.RFC3339Nano, fields[1])
	if err != nil {
		return 0, time.Time{}, false
	}
	return epoch, startedAt, true
}

// quarantineACPWorkspacePastDetachTimeout enforces the frozen detachTimeout on
// a revocation the adapter never finalizes: past the deadline the workspace is
// quarantined fail-closed and settlement reports done so the Task releases.
func (r *TaskReconciler) quarantineACPWorkspacePastDetachTimeout(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (bool, error) {
	stampedEpoch, startedAt, ok := parseACPWorkspaceRevocationStamp(workspace.Annotations[acpWorkspaceRevocationStartedAnnotation])
	if !ok || stampedEpoch != workspace.Spec.AttachmentEpoch {
		// No stamp for the epoch being finalized: the deadline is measured
		// from this revocation, never from a previous attachment's stamp.
		return false, nil
	}
	if time.Since(startedAt) <= workspace.Spec.Lifecycle.DetachTimeout.Duration {
		return false, nil
	}
	base := workspace.DeepCopy()
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined
	if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// reconcileACPClassWorkspaceSettlement applies workspace settlement for a
// terminal ACP Task: it mirrors the Released projection predicate so the
// attachment is revoked and the Delete detach action applied exactly when the
// Task's workspace demand is released.
func (r *TaskReconciler) reconcileACPClassWorkspaceSettlement(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	if task == nil || task.Spec.Type != corev1alpha1.TaskTypeAgent || task.Status.Execution == nil {
		return true, nil
	}
	// The controller-owned workspace link is the settlement trigger, not the
	// later runtime-name projection: a terminal planning failure can occur
	// after the workspace attached but before RuntimePoolName was written,
	// and that attachment must still be revoked.
	if !taskManagedByACP(task) && strings.TrimSpace(task.Labels[acpExecutionWorkspaceLinkLabel]) == "" {
		return true, nil
	}
	if !taskExecutionStateTerminal(task.Status.Execution.State) {
		return true, nil
	}
	if task.Status.Execution.State == corev1alpha1.TaskExecutionStateSucceeded &&
		(task.Status.Delivery == nil || !store.IsTerminalPromptDeliveryState(store.PromptDeliveryState(task.Status.Delivery.State))) {
		return true, nil
	}
	return r.settleACPClassWorkspace(ctx, task)
}
