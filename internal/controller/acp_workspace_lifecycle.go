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
	// acpWorkspaceNamePrefix prefixes deterministic class-backed workspace names.
	acpWorkspaceNamePrefix = "acp-ws"
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
	if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
		// A continuation Task requests cold resume: the same concrete
		// workspace returns to Ready, and the adapter replaces the physical
		// runtime with a fresh boot and credential bootstrap before this
		// Task can attach.
		base := workspace.DeepCopy()
		workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredReady
		if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return "", false, client.IgnoreNotFound(err)
		}
		return "", false, nil
	}
	if !workspaceCurrentlyAdmittedByCore(workspace) {
		return "", false, nil
	}
	if workspace.Spec.Attachment != nil {
		if workspace.Spec.Attachment.TaskRef.UID == task.UID {
			if err := r.recordACPWorkspaceDetachAction(ctx, workspace, binding); err != nil {
				return "", false, err
			}
			return name, true, r.linkTaskToACPWorkspace(ctx, task, name)
		}
		// Another Task holds the one-writer attachment. Session-reused
		// workspaces queue behind it; the deterministic per-Task name makes
		// this unreachable for reuse none.
		return "", false, nil
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
	return name, true, r.linkTaskToACPWorkspace(ctx, task, name)
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
func (r *TaskReconciler) linkTaskToACPWorkspace(ctx context.Context, task *corev1alpha1.Task, name string) error {
	if task.Labels[acpExecutionWorkspaceLinkLabel] == name {
		return nil
	}
	base := task.DeepCopy()
	if task.Labels == nil {
		task.Labels = make(map[string]string)
	}
	task.Labels[acpExecutionWorkspaceLinkLabel] = name
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
				acpExecutionWorkspacePoolAnnotation: poolName,
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
		string(workspace.Spec.ProviderBinding.UID) != binding.Class.ProviderUID {
		return fmt.Errorf("%w: workspace %s provider binding does not match the frozen provider identity", errACPWorkspaceBindingConflict, workspace.Name)
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
	manager := WorkspaceAttachmentManager{Client: r.Client}
	if attachment := workspace.Spec.Attachment; attachment != nil {
		if attachment.TaskRef.UID != task.UID {
			// Another Task attached the session-reused workspace; its own
			// settle flow owns revocation and the detach action.
			return true, nil
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
		if err := manager.FinalizeRevocation(ctx, workspace, epoch, attachmentSecretName(workspace.Name, epoch)); err != nil {
			// The adapter has not cleared the enforced epoch yet; requeue
			// instead of deleting under an active attachment.
			return false, nil
		}
	}
	if workspace.Spec.Attachment != nil {
		return true, nil
	}
	// Apply the attached Task's frozen effective detach action. Suspend
	// quiesced above: the attachment is revoked and the adapter released the
	// enforced epoch, so no prompt or workspace writer remains. The UID
	// precondition keeps a concurrent re-attachment from losing its
	// workspace: the API server rejects the write if the object changed
	// identity, and a Task that attached after this read settles at its own
	// settle time anyway.
	if workspace.Annotations[acpWorkspaceDetachActionAnnotation] == string(workspacev1alpha1.WorkspaceOnDetachSuspend) {
		if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
			return true, nil
		}
		base := workspace.DeepCopy()
		workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
		if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
	if err := r.Delete(ctx, workspace, client.Preconditions{UID: &workspace.UID}); err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	return true, nil
}

// reconcileACPClassWorkspaceSettlement applies workspace settlement for a
// terminal ACP Task: it mirrors the Released projection predicate so the
// attachment is revoked and the Delete detach action applied exactly when the
// Task's workspace demand is released.
func (r *TaskReconciler) reconcileACPClassWorkspaceSettlement(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	if task == nil || task.Spec.Type != corev1alpha1.TaskTypeAgent || task.Status.Execution == nil || !taskManagedByACP(task) {
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
