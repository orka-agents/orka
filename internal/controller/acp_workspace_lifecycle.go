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

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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

// errACPWorkspaceTerminalFailure marks a workspace whose provider adapter
// reported a terminal Failed state (for example an enforced maximum-lifetime
// expiry): the Task waiting on it must fail instead of requeueing forever.
var errACPWorkspaceTerminalFailure = errors.New("execution workspace failed terminally")

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
	// able to settle (and apply its frozen Delete action to) the session
	// workspace it materialized, which carries no Task owner reference.
	if err := r.linkTaskToACPWorkspace(ctx, task, workspace); err != nil {
		return "", false, err
	}
	if workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateFailed {
		if attachment := workspace.Spec.Attachment; attachment != nil && attachment.TaskRef.UID != task.UID {
			// The failed incarnation is still held by its predecessor (for
			// example maxLifetime expired mid-execution): that Task's own
			// settlement shortly applies its frozen detach action and removes
			// this incarnation, after which the deterministic name is
			// recreated fresh. The continuation queues for that instead of
			// failing permanently.
			return "", false, nil
		}
		// The adapter's Failed state is terminal for this incarnation (for
		// example the frozen maximum lifetime elapsed and the RuntimePool was
		// torn down); waiting can never recover it.
		return "", false, fmt.Errorf(
			"%w: workspace %s reported a terminal Failed state", errACPWorkspaceTerminalFailure, workspace.Name)
	}
	if !workspaceCurrentlyAdmittedByCore(workspace) {
		if condition := workspaceprovider.FindCondition(
			workspace.Status.Conditions, string(workspacev1alpha1.ConditionWorkspaceAdmitted),
		); condition != nil && condition.Status == metav1.ConditionFalse &&
			condition.ObservedGeneration == workspace.Generation &&
			(condition.Reason == "ClassBindingMismatch" || condition.Reason == reasonProviderBindingMismatch ||
				condition.Reason == "ClassDeleting" || condition.Reason == reasonProviderDeleting ||
				condition.Reason == reasonProfileDrift ||
				condition.Reason == "ClassNotFound" || condition.Reason == "ParametersDeleting" ||
				condition.Reason == "ParametersNotFound") {
			// The frozen identity can never become admissible (the class or
			// provider generation moved after the snapshot froze, the class
			// or provider is deleting and admits no new workspaces - holding
			// the unadmitted reference would deadlock their finalizers - or
			// the immutable profile was replaced); requeueing
			// forever would leave the Task permanently Pending.
			return "", false, fmt.Errorf(
				"%w: workspace %s was denied core admission (%s) and its frozen identity can never be admitted; create a new Task",
				errACPWorkspaceBindingConflict, workspace.Name, condition.Reason,
			)
		}
		return "", false, nil
	}
	if workspace.Spec.Attachment != nil {
		if workspace.Spec.Attachment.TaskRef.UID == task.UID {
			if workspace.Status.AttachedEpoch != workspace.Spec.Attachment.Epoch {
				// Attach bumped the generation: core must re-admit it and the
				// adapter must publish the enforced epoch before RuntimePool
				// demand exists, or the prompt could execute under a provider
				// policy core has since withdrawn.
				return "", false, nil
			}
			if maxLifetime := workspace.Spec.Lifecycle.MaxLifetime; maxLifetime != nil && maxLifetime.Duration > 0 &&
				!time.Now().Before(workspace.CreationTimestamp.Add(maxLifetime.Duration)) {
				// The frozen hard deadline elapsed between Attach and the
				// adapter's Failed publication: admitting RuntimePool demand
				// from the stale Ready observation would execute the prompt
				// past the lifetime until the adapter catches up. Both
				// inputs are immutable, so the cached read is authoritative;
				// the adapter's enforcement fails the workspace shortly.
				return "", false, nil
			}
			return name, true, r.linkTaskToACPWorkspace(ctx, task, workspace)
		}
		// Another Task holds the one-writer attachment. Session-reused
		// workspaces queue behind it; the deterministic per-Task name makes
		// this unreachable for reuse none.
		return "", false, nil
	}
	if workspace.Annotations[acpWorkspaceRevocationStartedAnnotation] != "" {
		// A Delete settlement is pending on this workspace incarnation: the
		// prior Task's frozen detach action must destroy this filesystem
		// before any continuation runs. The deterministic name is recreated
		// fresh once the deletion lands; attaching now would execute in state
		// the class policy required to destroy.
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
	// The attachment bumped the workspace generation: readiness waits for
	// core re-admission and the adapter's enforced-epoch acknowledgement on
	// the next pass, so RuntimePool demand can never precede them.
	return "", false, nil
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
	if workspace.Spec.SessionRef != nil {
		// A session workspace has no Task owner reference, so the settlement
		// link must be durable BEFORE creation: a crash between the Create
		// below and the post-create link patch would otherwise orphan the
		// deterministic session workspace forever - nothing would ever apply
		// its Delete lifecycle, and the name would stay occupied. The name
		// label alone is recoverable (settlement revalidates session
		// ownership before acting); the incarnation UID annotation completes
		// after creation as before.
		if task.Labels[acpExecutionWorkspaceLinkLabel] != name {
			base := task.DeepCopy()
			if task.Labels == nil {
				task.Labels = map[string]string{}
			}
			task.Labels[acpExecutionWorkspaceLinkLabel] = name
			if err := r.Patch(ctx, task, client.MergeFrom(base)); err != nil {
				return nil, fmt.Errorf("persist the pre-creation workspace settlement link: %w", err)
			}
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
	if !r.WorkspaceSettlementProtected {
		// Without Task provenance admission the reserved workspace-link
		// metadata is forgeable by any direct Task writer; the privileged
		// revocation and detach actions below must not run from it. The Task
		// releases and the workspace is cleaned through explicit workspace
		// deletion (adapter finalizers do not depend on Task metadata).
		logf.FromContext(ctx).Info("skipping privileged class-workspace settlement; Task provenance admission is disabled",
			"task", task.Name)
		return true, nil
	}
	name := strings.TrimSpace(task.Labels[acpExecutionWorkspaceLinkLabel])
	if name == "" {
		return true, nil
	}
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	// Absence is proven through the uncached reader: a just-created session
	// workspace can be invisible to the informer cache, and a cache miss must
	// never release the Task finalizer while an orphan without a Task owner
	// reference survives to occupy the deterministic session name.
	settleReader := client.Reader(r.Client)
	if r.APIReader != nil {
		settleReader = r.APIReader
	}
	if err := settleReader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace); err != nil {
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
	if task.Annotations[acpExecutionWorkspaceUIDAnnotation] != string(workspace.UID) {
		// The controller-written incarnation pin is REQUIRED before any
		// privileged action: a forged or stripped link (the label and the
		// session name are client-visible metadata) must never let a Task
		// revoke or delete a workspace it did not attach, and a recorded UID
		// from a different incarnation (a Session recreated under the same
		// name) is equally skipped.
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
		// Settlement stays pending after initiating revocation: an immediate
		// cached re-read can still show the pre-patch attachment and would
		// otherwise report the detach action applied without ever running
		// it. The caller requeues, and the next pass finalizes against the
		// converged object.
		return false, nil
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
	// Only Delete is executable as a detach action, for per-Task and
	// session-reused workspaces alike. The UID+resourceVersion preconditions
	// keep a concurrent re-attachment from losing its workspace, and a
	// conflict retries settlement against the fresh object: reporting the
	// detach action applied on a swallowed conflict would let this Task
	// release while the workspace it was frozen to delete lives on.
	if workspace.Spec.Attachment != nil {
		return true, nil
	}
	if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined {
		// Quarantine is terminal: a retry after the adapter released the
		// epoch must complete the credential cleanup idempotently and never
		// execute the Delete detach action on the preserved evidence.
		if epoch := workspace.Spec.AttachmentEpoch; epoch > 0 {
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: attachmentSecretName(workspace.Name, epoch), Namespace: workspace.Namespace,
			}}
			if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
				return false, fmt.Errorf("delete quarantined attachment Secret: %w", err)
			}
		}
		lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
			Name: attachmentLeaseName(workspace.Name), Namespace: workspace.Namespace,
		}}
		if err := r.Delete(ctx, lease); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("delete quarantined attachment Lease: %w", err)
		}
		return true, nil
	}
	// The destructive path revalidates the frozen policy it is about to
	// execute: a workspace created by a newer controller with a Suspend
	// action or retained deletion categories must fail closed after a
	// rollback instead of being deleted under a contract this version cannot
	// honor.
	// The detach-action key is written by newer controllers; this version
	// freezes only Delete, so any other recorded action is a rollback marker.
	if action := workspace.Annotations["acp.workspace.orka.ai/detach-action"]; action != "" &&
		action != string(workspacev1alpha1.WorkspaceOnDetachDelete) {
		return false, fmt.Errorf(
			"workspace %s froze detach action %q, which this controller cannot execute; refusing destructive settlement",
			workspace.Name, action,
		)
	}
	for _, deletionAction := range []workspacev1alpha1.WorkspaceDeletionAction{
		workspace.Spec.Lifecycle.DeletionPolicy.ProviderResources,
		workspace.Spec.Lifecycle.DeletionPolicy.PersistentVolumes,
		workspace.Spec.Lifecycle.DeletionPolicy.Checkpoints,
	} {
		if deletionAction != workspacev1alpha1.WorkspaceDeletionActionDelete {
			return false, fmt.Errorf(
				"workspace %s deletion policy retains resources; this controller only executes the all-Delete lifecycle and fails closed",
				workspace.Name,
			)
		}
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
	// Quarantine is terminal and nothing revisits this workspace's
	// credentials afterwards (the adapter destroys only the pool, and the
	// Task releases now): the attachment Secret and Lease must be removed
	// here or the bearer credential outlives the workspace's usable life.
	epoch := workspace.Spec.AttachmentEpoch
	if epoch > 0 {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: attachmentSecretName(workspace.Name, epoch), Namespace: workspace.Namespace,
		}}
		if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("delete quarantined attachment Secret: %w", err)
		}
	}
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Name: attachmentLeaseName(workspace.Name), Namespace: workspace.Namespace,
	}}
	if err := r.Delete(ctx, lease); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("delete quarantined attachment Lease: %w", err)
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
	done, err := r.settleACPClassWorkspace(ctx, task)
	if err != nil || !done {
		return done, err
	}
	return true, r.refreshACPReleasedWorkspaceProjection(ctx, task)
}

// refreshACPReleasedWorkspaceProjection re-projects the class attachment
// identity once settlement completes: the Released transition runs before
// revocation, so without this refresh the terminal status would permanently
// claim state Attached and the pre-revocation attachment epoch.
func (r *TaskReconciler) refreshACPReleasedWorkspaceProjection(ctx context.Context, task *corev1alpha1.Task) error {
	current := task.Status.ExecutionWorkspace
	if current == nil || current.Phase != corev1alpha1.ExecutionWorkspacePhaseReleased ||
		(current.AttachedEpoch == 0 && current.State != string(workspacev1alpha1.ExecutionWorkspaceStateAttached)) {
		return nil
	}
	next := current.DeepCopy()
	next.AttachedEpoch = 0
	next.State = ""
	if name := strings.TrimSpace(task.Labels[acpExecutionWorkspaceLinkLabel]); name != "" {
		workspace := &workspacev1alpha1.ExecutionWorkspace{}
		// A workspace held only by its cleanup finalizer (a settlement Delete
		// in flight) still serves cached pre-delete status; copying it would
		// freeze a permanently stale Ready/Attached claim into the released
		// Task. Its state stays cleared instead.
		if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace); err == nil &&
			workspace.DeletionTimestamp.IsZero() &&
			workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined &&
			workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredDeleted &&
			task.Annotations[acpExecutionWorkspaceUIDAnnotation] == string(workspace.UID) {
			// A maintenance intent (quarantine or deletion) means the cached
			// status may still show the pre-patch Attached/Ready state; the
			// Task controller never revisits it, so nothing stale is copied.
			next.State = string(workspace.Status.State)
		}
	}
	base := task.DeepCopy()
	task.Status.ExecutionWorkspace = next
	return r.Status().Patch(ctx, task, client.MergeFrom(base))
}
