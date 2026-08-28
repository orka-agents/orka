/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

	acpworkspacev1alpha1 "github.com/orka-agents/orka/api/acp.workspace/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
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
	// acpWorkspaceDurableAnnotation records at materialization that the
	// frozen class profile provisions durable workspace artifacts (a data-only
	// suspension checkpoint or durable PVC), so terminal dispositions report
	// what actually existed instead of inferring it from allowed detach
	// actions.
	acpWorkspaceDurableAnnotation = "acp.workspace.orka.ai/durable-workspace"
	// acpWorkspaceSuspendModeAnnotation records the profile-frozen Substrate
	// suspend mode. The terminal adapter still needs this after the linked
	// RuntimePool and its rendered DurableDir policy have been deleted.
	acpWorkspaceSuspendModeAnnotation = "acp.workspace.orka.ai/substrate-suspend-mode"
	// acpWorkspaceResumedLineageAnnotation marks a workspace whose current
	// physical runtime was cold-resumed from a preserved data checkpoint. The
	// linked pool then holds the ONLY copy of the session data for the
	// workspace's remaining lifetime, so a missing or foreign pool is
	// terminal data loss even after the adapter projected Ready or Attached.
	acpWorkspaceResumedLineageAnnotation = "acp.workspace.orka.ai/resumed-lineage"
	// acpWorkspaceRuntimeNamespaceAnnotation freezes the runtime namespace
	// the linked pool realizes its provider children (SandboxClaim, durable
	// PVC) in, so deletion proofs probe the ORIGINAL namespace even if the
	// controller's --acp-runtime-namespace changed since creation.
	acpWorkspaceRuntimeNamespaceAnnotation = "acp.workspace.orka.ai/runtime-namespace"
	// acpWorkspaceDurableDataAbsentAnnotation records, on a terminally failed
	// workspace, that the failure PROVED no durable data exists (a missing,
	// foreign, or non-suspend-capable linked pool, or a pool that settled
	// without a consensual checkpoint). Retention frees the suspension-quota
	// slot for such failures while resume-loss failures that preserve a
	// claim stay charged until deletion proves cleanup.
	acpWorkspaceDurableDataAbsentAnnotation = "acp.workspace.orka.ai/durable-data-absent"

	// acpWorkspaceDurableSessionCommittedAnnotation records that a
	// RuntimeSession creation completed on this workspace - and with it the
	// supervisor's synchronous durable checkpoint commit - so a resumed
	// lineage may assert the committed checkpoint's existence fail-closed.
	acpWorkspaceDurableSessionCommittedAnnotation = "acp.workspace.orka.ai/durable-session-committed"
	// acpWorkspaceRevocationStartedAnnotation stamps the first revocation
	// attempt so settlement can enforce the frozen detachTimeout instead of
	// requeueing forever behind an adapter that never releases the epoch.
	acpWorkspaceRevocationStartedAnnotation = "acp.workspace.orka.ai/revocation-started-at"
	// acpWorkspaceResumeRequestedAnnotation records that a continuation
	// requested cold resume and has not attached yet. Idle retention treats
	// it as live demand (a cold boot can outlast idleTimeout), and the next
	// successful attachment clears it; the frozen maxLifetime remains the
	// hard bound if the requester dies before attaching.
	acpWorkspaceResumeRequestedAnnotation = "acp.workspace.orka.ai/resume-requested-at"
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
	if err := verifyACPClassWorkspace(workspace, task, binding, plan.PoolName); err != nil {
		if queueACPClassWorkspaceBehindPredecessor(workspace, task, binding) {
			return "", false, nil
		}
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
			if workspace.Annotations == nil {
				workspace.Annotations = map[string]string{}
			}
			// The resume flip records pending demand without changing the last
			// detach instant: a cold boot can outlast idleTimeout, so retention
			// must treat the workspace as demanded until the continuation
			// attaches. If that requester dies, idle retention resumes from the
			// actual detach instead of granting another full retention interval. The
			// continuation's frozen effective detach action replaces the
			// suspender's in the same patch (a continuation that selects
			// Delete and dies during cold boot must settle with Delete, not
			// resuspend under the predecessor's stale action), and the
			// resumed-lineage marker makes later pool loss terminal even
			// after Ready is projected.
			now := time.Now().UTC().Format(time.RFC3339Nano)
			// The demand record carries the requesting Task's name so
			// retention can retire demand whose requester died before
			// attaching instead of holding the workspace forever.
			workspace.Annotations[acpWorkspaceResumeRequestedAnnotation] = now + " " + task.Name + " " + string(task.UID)
			workspace.Annotations[acpWorkspaceDetachActionAnnotation] = binding.Class.EffectiveOnDetach
			workspace.Annotations[acpWorkspaceResumedLineageAnnotation] = booleanTrueValue
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
			// The suspension has not settled yet, but the continuation's
			// demand must already be durable: if idleTimeout expires while
			// the checkpoint drains, retention must see outstanding demand
			// instead of deleting the retained workspace the continuation is
			// about to resume.
			if fields := strings.Fields(workspace.Annotations[acpWorkspaceResumeRequestedAnnotation]); len(fields) < 3 ||
				fields[2] != string(task.UID) {
				base := workspace.DeepCopy()
				if workspace.Annotations == nil {
					workspace.Annotations = map[string]string{}
				}
				workspace.Annotations[acpWorkspaceResumeRequestedAnnotation] =
					time.Now().UTC().Format(time.RFC3339Nano) + " " + task.Name + " " + string(task.UID)
				if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
					return "", false, client.IgnoreNotFound(err)
				}
			}
			return "", false, nil
		}
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
		// The adapter's Failed state is terminal for this incarnation (a
		// lost cold resume, a torn-down pool of a resumed lineage, or an
		// enforced maximum-lifetime expiry); waiting can never recover it.
		return "", false, fmt.Errorf(
			"%w: workspace %s reported a terminal Failed state", errACPWorkspaceTerminalFailure, workspace.Name)
	}
	if !workspaceCurrentlyAdmittedByCore(workspace) {
		if condition := workspaceprovider.FindCondition(
			workspace.Status.Conditions, string(workspacev1alpha1.ConditionWorkspaceAdmitted),
		); condition != nil && condition.Status == metav1.ConditionFalse &&
			condition.ObservedGeneration == workspace.Generation &&
			(condition.Reason == "ClassBindingMismatch" || condition.Reason == reasonProviderBindingMismatch ||
				condition.Reason == "ClassProfileMismatch" || condition.Reason == "ClassPolicyMismatch" ||
				condition.Reason == "ClassDeleting" || condition.Reason == reasonProviderDeleting ||
				condition.Reason == reasonProfileDrift ||
				condition.Reason == "ClassNotFound" || condition.Reason == reasonProviderNotFound ||
				condition.Reason == "ParametersDeleting" ||
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
			ready, err := r.ensureACPWorkspaceAttachmentFresh(
				ctx, workspace, task, binding.Class.EffectiveOnDetach,
			)
			if err != nil || !ready {
				return "", false, err
			}
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
	if acpWorkspaceRevocationStampMatchesCurrentEpoch(workspace) {
		// A settlement is pending on this workspace incarnation: the prior
		// Task's frozen detach action must complete before any continuation
		// runs. For Delete, this filesystem is destroyed and the
		// deterministic name is recreated fresh once the deletion lands. For
		// Suspend, the settlement retires this stamp in the same optimistic
		// patch that lands DesiredState=Suspended, and the continuation then
		// takes the cold-resume flip; attaching while the stamp stands would
		// reuse the workspace warm and let the old settlement observe a
		// foreign attachment as completion, so the requested checkpoint
		// would silently never be taken.
		return "", false, nil
	}
	// Persist the settlement link before attaching: if the controller dies
	// between attachment and a later link patch, a session-scoped workspace
	// would stay attached to a Task that cleanup can no longer associate with
	// it, and every later Task for that Session would queue forever.
	if err := r.linkTaskToACPWorkspace(ctx, task, workspace); err != nil {
		return "", false, err
	}
	manager := WorkspaceAttachmentManager{Client: r.Client, APIReader: r.APIReader, LeaseTTL: acpWorkspaceAttachmentTTL}
	if _, err := manager.Attach(ctx, workspace, task, map[string]string{
		acpWorkspaceDetachActionAnnotation: binding.Class.EffectiveOnDetach,
	}); err != nil {
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

// recordACPWorkspaceDetachAction records the attached Task's frozen effective
// detach action on the workspace so settlement applies exactly that choice.
func (r *TaskReconciler) recordACPWorkspaceDetachAction(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	binding *ACPRuntimeWorkspaceBinding,
) error {
	action := binding.Class.EffectiveOnDetach
	if workspace.Annotations[acpWorkspaceDetachActionAnnotation] == action &&
		workspace.Annotations[acpWorkspaceRevocationStartedAnnotation] == "" &&
		workspace.Annotations[acpWorkspaceResumeRequestedAnnotation] == "" {
		return nil
	}
	base := workspace.DeepCopy()
	if workspace.Annotations == nil {
		workspace.Annotations = map[string]string{}
	}
	workspace.Annotations[acpWorkspaceDetachActionAnnotation] = action
	// A fresh attachment invalidates any prior revocation stamp and fulfils
	// pending resume demand: the next detach measures its detachTimeout from
	// its own revocation, and retention resumes ordinary idle handling.
	delete(workspace.Annotations, acpWorkspaceRevocationStartedAnnotation)
	delete(workspace.Annotations, acpWorkspaceResumeRequestedAnnotation)
	return r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// queueACPClassWorkspaceBehindPredecessor recognizes an older class/provider
// revision of the same session workspace while its creating Task still holds
// the attachment or its epoch-matched detach settlement is pending. The
// predecessor's frozen action must finish first; identity conflicts outside
// the revision fields remain terminal.
func queueACPClassWorkspaceBehindPredecessor(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	binding *ACPRuntimeWorkspaceBinding,
) bool {
	if workspace == nil || task == nil || binding == nil {
		return false
	}
	attachedPredecessor := workspace.Spec.Attachment != nil &&
		workspace.Spec.Attachment.TaskRef.UID != "" && workspace.Spec.Attachment.TaskRef.UID != task.UID
	pendingRevocation := workspace.Spec.Attachment == nil && acpWorkspaceRevocationStampMatchesCurrentEpoch(workspace)
	if binding.ReusePolicy != corev1alpha1.WorkspaceReusePolicySession ||
		(!attachedPredecessor && !pendingRevocation) ||
		workspace.Labels[workspacev1alpha1.QuarantinedLabel] == booleanTrueValue ||
		workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined ||
		!workspaceCarriesACPMaterializationMarkers(workspace) ||
		workspace.Spec.Mode != workspacev1alpha1.ExecutionWorkspaceModeInteractive {
		return false
	}
	return workspace.Spec.ClassBinding.Name == binding.Class.Name &&
		string(workspace.Spec.ClassBinding.UID) == binding.Class.UID &&
		workspace.Spec.ProviderBinding.Name == binding.Class.ProviderName &&
		string(workspace.Spec.ProviderBinding.UID) == binding.Class.ProviderUID &&
		workspace.Spec.Slot == binding.WorkspaceSlot &&
		workspace.Spec.SessionRef != nil &&
		string(workspace.Spec.SessionRef.UID) == binding.SessionUID
}

func acpWorkspaceRevocationStampMatchesCurrentEpoch(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	if workspace == nil || workspace.Spec.AttachmentEpoch <= 0 {
		return false
	}
	stampedEpoch, _, ok := parseACPWorkspaceRevocationStamp(
		workspace.Annotations[acpWorkspaceRevocationStartedAnnotation],
	)
	return ok && stampedEpoch == workspace.Spec.AttachmentEpoch
}

// ensureACPWorkspaceAttachmentFresh keeps one Task's attachment enforced and
// rotates its bearer at the hard credential expiry without opening a detached
// ownership gap. It reports ready only after core re-admission and adapter
// acknowledgement of the current epoch.
func (r *TaskReconciler) ensureACPWorkspaceAttachmentFresh(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	detachAction string,
) (bool, error) {
	if workspace == nil || task == nil || workspace.Spec.Attachment == nil ||
		workspace.Spec.Attachment.TaskRef.UID != task.UID {
		return false, nil
	}
	attachment := workspace.Spec.Attachment
	if workspace.Status.AttachedEpoch != attachment.Epoch {
		// Attach bumped the generation: core must re-admit it and the
		// adapter must publish the enforced epoch before new RuntimePool
		// demand exists.
		return false, nil
	}
	now := time.Now()
	if maxLifetime := workspace.Spec.Lifecycle.MaxLifetime; maxLifetime != nil && maxLifetime.Duration > 0 &&
		!now.Before(workspace.CreationTimestamp.Add(maxLifetime.Duration)) {
		// The adapter enforces the immutable hard lifetime by tearing down the
		// linked pool. Never renew attachment authority past that deadline.
		return false, nil
	}
	if attachment.ExpiresAt.After(now) {
		if err := r.deleteSupersededACPWorkspaceAttachmentSecret(ctx, workspace); err != nil {
			return false, err
		}
		return true, nil
	}

	expiredSecretName := attachment.TokenSecretRef.Name
	manager := WorkspaceAttachmentManager{Client: r.Client, APIReader: r.APIReader, LeaseTTL: acpWorkspaceAttachmentTTL}
	annotations := map[string]string(nil)
	if detachAction = strings.TrimSpace(detachAction); detachAction != "" {
		annotations = map[string]string{acpWorkspaceDetachActionAnnotation: detachAction}
	}
	if _, err := manager.Attach(ctx, workspace, task, annotations); err != nil {
		if errors.Is(err, ErrWorkspaceAttachmentLocked) ||
			errors.Is(err, errWorkspaceAttachmentRotationNotReady) ||
			strings.Contains(err.Error(), "revalidate workspace") {
			return false, nil
		}
		return false, err
	}
	// The prior bearer is already unusable because its hard expiry elapsed.
	// Remove it immediately; later reconciles repeat this after the new epoch
	// is acknowledged to close the patch-before-delete crash window.
	if strings.TrimSpace(expiredSecretName) != "" {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: expiredSecretName, Namespace: workspace.Namespace,
		}}
		if err := r.deleteACPWorkspaceOwnedAttachmentObject(ctx, workspace, secret, "Secret"); err != nil {
			return false, err
		}
	}
	return false, nil
}

// reconcileRunningACPClassWorkspaceAttachment revisits the hard attachment
// expiry while the dispatcher owns an in-flight prompt. Rotation keeps the
// same Task attached, so the prompt can continue while a new bearer and epoch
// are admitted and acknowledged.
func (r *TaskReconciler) reconcileRunningACPClassWorkspaceAttachment(
	ctx context.Context,
	task *corev1alpha1.Task,
) error {
	name := strings.TrimSpace(task.Labels[acpExecutionWorkspaceLinkLabel])
	uid := strings.TrimSpace(task.Annotations[acpExecutionWorkspaceUIDAnnotation])
	if name == "" || uid == "" {
		return nil
	}
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace); err != nil {
		return client.IgnoreNotFound(err)
	}
	if string(workspace.UID) != uid || workspace.Spec.ClassBinding.Name == "" ||
		workspace.Labels[workspacev1alpha1.ProviderControllerLabel] != acpWorkspaceProviderControllerName ||
		workspace.Spec.Attachment == nil || workspace.Spec.Attachment.TaskRef.UID != task.UID {
		return nil
	}
	if current := task.Status.ExecutionWorkspace; current == nil || current.AttachedEpoch != workspace.Spec.Attachment.Epoch {
		if err := r.deleteSupersededACPWorkspaceAttachmentSecret(ctx, workspace); err != nil {
			return err
		}
	}
	if workspace.Spec.Attachment.ExpiresAt.After(time.Now()) {
		return nil
	}
	_, err := r.ensureACPWorkspaceAttachmentFresh(
		ctx, workspace, task, workspace.Annotations[acpWorkspaceDetachActionAnnotation],
	)
	return err
}

// deleteSupersededACPWorkspaceAttachmentSecret repeats deletion of the
// immediately prior bearer after the provider has enforced the replacement
// epoch. This closes the crash window after rotation. Epochs advance one at a
// time, and deletion is idempotent across controller restarts.
func (r *TaskReconciler) deleteSupersededACPWorkspaceAttachmentSecret(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) error {
	if workspace == nil || workspace.Spec.Attachment == nil || workspace.Spec.Attachment.Epoch <= 1 {
		return nil
	}
	name := attachmentSecretName(workspace.Name, workspace.Spec.Attachment.Epoch-1)
	if name == workspace.Spec.Attachment.TokenSecretRef.Name {
		return fmt.Errorf("workspace %s attachment epoch %d reuses the prior bearer Secret name", workspace.Name, workspace.Spec.Attachment.Epoch)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: workspace.Namespace}}
	return r.deleteACPWorkspaceOwnedAttachmentObject(ctx, workspace, secret, "Secret")
}

func (r *TaskReconciler) deleteACPWorkspaceOwnedAttachmentObject(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	obj client.Object,
	kind string,
) error {
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	return deleteWorkspaceOwnedAttachmentObject(ctx, reader, r.Client, workspace, obj, kind)
}

func (r *TaskReconciler) deleteACPWorkspaceAttachmentCredentials(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) error {
	if epoch := workspace.Spec.AttachmentEpoch; epoch > 0 {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: attachmentSecretName(workspace.Name, epoch), Namespace: workspace.Namespace,
		}}
		if err := r.deleteACPWorkspaceOwnedAttachmentObject(ctx, workspace, secret, "Secret"); err != nil {
			return err
		}
	}
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Name: attachmentLeaseName(workspace.Name), Namespace: workspace.Namespace,
	}}
	return r.deleteACPWorkspaceOwnedAttachmentObject(ctx, workspace, lease, "Lease")
}

// linkTaskToACPWorkspace records the workspace name on the Task so terminal
// settlement and Task finalization find the workspace without reloading the
// execution snapshot. It installs the cleanup finalizer in the same metadata
// patch, so a Task cannot disappear during deletion after it acquires
// settlement ownership. The label value is an Orka-owned deterministic name.
func (r *TaskReconciler) linkTaskToACPWorkspace(
	ctx context.Context,
	task *corev1alpha1.Task,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) error {
	if task.Labels[acpExecutionWorkspaceLinkLabel] == workspace.Name &&
		task.Annotations[acpExecutionWorkspaceUIDAnnotation] == string(workspace.UID) &&
		controllerutil.ContainsFinalizer(task, labels.TaskFinalizer) {
		return nil
	}
	base := task.DeepCopy()
	controllerutil.AddFinalizer(task, labels.TaskFinalizer)
	if task.Labels == nil {
		task.Labels = make(map[string]string)
	}
	if task.Annotations == nil {
		task.Annotations = make(map[string]string)
	}
	task.Labels[acpExecutionWorkspaceLinkLabel] = workspace.Name
	task.Annotations[acpExecutionWorkspaceUIDAnnotation] = string(workspace.UID)
	patch := client.MergeFrom(base)
	if base.ResourceVersion != "" {
		patch = client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
	}
	return r.Patch(ctx, task, patch)
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
			Annotations: workspaceCreationAnnotations(binding, poolName, task.Name, string(task.UID),
				resolveACPWorkspaceRuntimeNamespace(r.ACPRuntimeNamespace, task.Namespace)),
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
	if binding.Class.SuspendMode != "" {
		workspace.Annotations[acpWorkspaceSuspendModeAnnotation] = binding.Class.SuspendMode
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

// workspaceCreationAnnotations renders the Orka-owned annotations for a fresh
// class-backed workspace: the linked pool name and the frozen retention cap.
// resolveACPWorkspaceRuntimeNamespace resolves the namespace provider
// children realize in: the configured runtime namespace, or the workspace's
// own namespace when the flag is empty.
func resolveACPWorkspaceRuntimeNamespace(configured, workspaceNamespace string) string {
	if resolved := strings.TrimSpace(configured); resolved != "" {
		return resolved
	}
	return workspaceNamespace
}

func workspaceCreationAnnotations(binding *ACPRuntimeWorkspaceBinding, poolName, requesterName, requesterUID, runtimeNamespace string) map[string]string {
	annotations := map[string]string{
		acpExecutionWorkspacePoolAnnotation:     poolName,
		acpWorkspaceProviderConfigUIDAnnotation: binding.Class.ProviderConfigUID,
		acpWorkspaceBackendAnnotation:           string(binding.Provider),
		// The creator's frozen effective detach action is recorded at
		// materialization so retention honors the class default even for a
		// workspace whose Task never reached attachment; the attach path
		// refreshes it with the attached Task's frozen choice.
		acpWorkspaceDetachActionAnnotation: binding.Class.EffectiveOnDetach,
		// Initial materialization is pending demand exactly like a cold
		// resume: a slow first provisioning can outlast idleTimeout, and
		// retention must not suspend or delete the workspace out from under
		// the Task still waiting to attach. The record clears on attachment
		// (or when settlement suspends the workspace); the frozen maxLifetime
		// stays the hard bound if the creator dies before attaching.
		acpWorkspaceResumeRequestedAnnotation: time.Now().UTC().Format(time.RFC3339Nano) + " " + requesterName + " " + requesterUID,
	}
	if binding.Class.MaxSuspendedWorkspaces != nil {
		annotations[acpWorkspaceMaxSuspendedAnnotation] = strconv.FormatInt(int64(*binding.Class.MaxSuspendedWorkspaces), 10)
	}
	if runtimeNamespace != "" {
		// Freeze the realized runtime namespace so deletion proofs probe the
		// ORIGINAL provider-children namespace even if the controller flag
		// changes later. Callers resolve an empty flag to the workspace
		// namespace BEFORE this call so that resolution is frozen too: an
		// absent annotation would let a later non-empty flag redirect
		// deletion proofs to a namespace the original PVC never lived in.
		annotations[acpWorkspaceRuntimeNamespaceAnnotation] = runtimeNamespace
	}
	if binding.Class.SuspendMode == string(acpworkspacev1alpha1.SubstrateSuspendModeDataOnly) {
		// The frozen profile provisions durable artifacts (a checkpoint or a
		// durable PVC); the terminal disposition reports what actually
		// existed instead of inferring it from allowed detach actions.
		annotations[acpWorkspaceDurableAnnotation] = booleanTrueValue
	}
	return annotations
}

// verifyACPClassWorkspace fail-closes adoption: an existing workspace must
// carry exactly the frozen class, provider, session, and slot bindings.
func verifyACPClassWorkspace(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	binding *ACPRuntimeWorkspaceBinding,
	poolName string,
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
	if workspace.Labels[workspacev1alpha1.ProviderControllerLabel] != acpWorkspaceProviderControllerName ||
		workspace.Annotations[acpExecutionWorkspacePoolAnnotation] != poolName {
		return fmt.Errorf("%w: workspace %s materialization markers do not match the frozen RuntimePool binding", errACPWorkspaceBindingConflict, workspace.Name)
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
		workspace.Annotations[acpWorkspaceBackendAnnotation] != string(binding.Provider) ||
		workspace.Annotations[acpWorkspaceSuspendModeAnnotation] != binding.Class.SuspendMode {
		return fmt.Errorf("%w: workspace %s provider config, backend, or suspend mode does not match the frozen binding", errACPWorkspaceBindingConflict, workspace.Name)
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
	resumeProviderSupported := binding.Provider == corev1alpha1.WorkspaceProviderSubstrate ||
		binding.Provider == corev1alpha1.WorkspaceProviderAgentSandbox
	resumeFromSuspended := workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended &&
		workspace.Spec.Attachment == nil &&
		resumeProviderSupported &&
		binding.ReusePolicy == corev1alpha1.WorkspaceReusePolicySession &&
		binding.Class.SuspendMode == string(acpworkspacev1alpha1.SubstrateSuspendModeDataOnly)
	if workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady && !resumeFromSuspended {
		return fmt.Errorf("%w: workspace %s desired state %q cannot admit new work", errACPWorkspaceBindingConflict, workspace.Name, workspace.Spec.DesiredState)
	}
	return nil
}

// settleACPClassWorkspace revokes this Task's attachment and applies the
// Delete detach action once the Task is terminal or finalizing. Every step is
// idempotent and restart-safe; done=false requests a requeue while the
// adapter still enforces the revoked epoch.
//
//nolint:gocyclo // Settlement keeps the credential revocation and destructive lifecycle gates in one auditable state machine.
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
	if workspace.Spec.SessionRef != nil {
		binding, err := r.loadVerifiedACPWorkspaceBindingForSettlement(ctx, task)
		if err != nil {
			return false, err
		}
		if binding == nil || binding.ReusePolicy != corev1alpha1.WorkspaceReusePolicySession ||
			strings.TrimSpace(binding.SessionUID) != string(workspace.Spec.SessionRef.UID) {
			return true, nil
		}
	}
	if task.Annotations[acpExecutionWorkspaceUIDAnnotation] != string(workspace.UID) {
		// The controller-written incarnation pin is REQUIRED before any
		// privileged action: a forged or stripped link (the label and the
		// session name are client-visible metadata) must never let a Task
		// revoke or delete a workspace it did not attach, and a recorded UID
		// from a different incarnation (a Session recreated under the same
		// name) is equally skipped. One recoverable case exists: the
		// pre-creation link persists only the label, and a crash before the
		// post-create patch leaves the pin EMPTY. The pin is recovered here
		// by proving the Task's own resolved Session identity matches the
		// workspace's immutable Session binding through the durable stores -
		// releasing the Task without it would orphan the deterministic
		// session workspace forever.
		if strings.TrimSpace(task.Annotations[acpExecutionWorkspaceUIDAnnotation]) != "" ||
			workspace.Spec.SessionRef == nil ||
			r.DurableControlStore == nil || r.SessionManager == nil || r.ControllerEpochManager == nil {
			return true, nil
		}
		resolvedUID, resolveErr := r.planACPWorkspaceSessionUID(ctx, task)
		if resolveErr != nil {
			return false, resolveErr
		}
		if strings.TrimSpace(resolvedUID) != string(workspace.Spec.SessionRef.UID) {
			return true, nil
		}
		if err := r.linkTaskToACPWorkspace(ctx, task, workspace); err != nil {
			return false, err
		}
	}
	manager := WorkspaceAttachmentManager{Client: r.Client, APIReader: r.APIReader}
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
	if workspace.Spec.Attachment != nil {
		return true, nil
	}
	if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined {
		// Quarantine is terminal: a retry after the adapter released the
		// epoch must complete the credential cleanup idempotently and never
		// execute any detach action on the preserved evidence.
		if err := r.deleteACPWorkspaceAttachmentCredentials(ctx, workspace); err != nil {
			return false, err
		}
		return true, nil
	}
	// A failed settlement-ownership transfer can leave the candidate's detach
	// action on the workspace before its Task link commits. Before an
	// unattached owner settles, reassert that Task's own effective policy so a
	// vanished candidate cannot change Delete into Suspend, or vice versa.
	if taskNeverHeldACPWorkspaceAttachment(task) {
		taskAction, allowed := effectiveACPWorkspaceDetachAction(task, workspace)
		if !allowed {
			return false, fmt.Errorf(
				"task %s requests a detach action outside workspace %s policy; refusing settlement",
				task.Name, workspace.Name,
			)
		}
		if workspace.Annotations[acpWorkspaceDetachActionAnnotation] != taskAction {
			base := workspace.DeepCopy()
			if workspace.Annotations == nil {
				workspace.Annotations = map[string]string{}
			}
			workspace.Annotations[acpWorkspaceDetachActionAnnotation] = taskAction
			if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
					return false, nil
				}
				return false, err
			}
		}
	}
	// Apply the attached Task's frozen effective detach action. Suspend
	// quiesced above: the attachment is revoked and the adapter released the
	// enforced epoch, so no prompt or workspace writer remains. The
	// UID+resourceVersion preconditions keep a concurrent re-attachment from
	// losing its workspace: the API server rejects the write if the object
	// changed after this read, and a Task that attached in between settles at
	// its own settle time anyway.
	terminallyFailed := workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateFailed
	if workspace.Annotations[acpWorkspaceDetachActionAnnotation] == string(workspacev1alpha1.WorkspaceOnDetachSuspend) &&
		!terminallyFailed {
		if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
			return true, nil
		}
		if strings.TrimSpace(workspace.Annotations[acpWorkspaceDurableSessionCommittedAnnotation]) == "" {
			// RuntimePool creation alone does not prove that a durable session
			// exists. Only the synchronous durable-commit stamp permits
			// retention; without it, delete the empty incarnation and let the
			// adapter tear down any linked but checkpoint-free pool.
			if err := r.Delete(ctx, workspace, deleteCurrentObjectPreconditions(workspace)...); err != nil &&
				!apierrors.IsNotFound(err) {
				if apierrors.IsConflict(err) {
					return false, nil
				}
				return false, err
			}
			return true, nil
		}
		reader := client.Reader(r.Client)
		if r.APIReader != nil {
			reader = r.APIReader
		}
		err := suspendACPWorkspaceWithinQuota(
			ctx,
			r.Client,
			reader,
			workspace,
			time.Now(),
			taskNeverHeldACPWorkspaceAttachment(task),
		)
		switch {
		case errors.Is(err, errACPSuspendQuotaExhausted):
			// Quota exhaustion cannot replace the frozen Suspend action with
			// Delete. A live queued continuation can take the still-Ready
			// workspace directly; otherwise settlement remains pending until
			// capacity opens or maxLifetime independently forces cleanup.
			if deferred, retry, deferErr := r.deferACPSettlementToSuccessor(ctx, workspace, task); deferErr != nil {
				return false, deferErr
			} else if retry {
				return false, nil
			} else if deferred {
				return true, nil
			}
			if r.Recorder != nil {
				if limit := acpWorkspaceSuspendedCapFromAnnotation(workspace); limit != nil {
					r.Recorder.Eventf(workspace, corev1.EventTypeWarning, "SuspendQuotaExhausted",
						"class retention cap of %d suspended workspaces is exhausted; the frozen Suspend action remains pending", *limit)
				}
			}
			return false, nil
		case errors.Is(err, errACPSuspendQuotaBusy), apierrors.IsConflict(err), apierrors.IsNotFound(err):
			return false, nil
		case err != nil:
			return false, err
		}
		return true, nil
	}
	// The destructive path revalidates the frozen policy it is about to
	// execute: an action this controller cannot execute (written by a newer
	// controller) or retained deletion categories must fail closed after a
	// rollback instead of being deleted under a contract this version cannot
	// honor.
	if action := workspace.Annotations[acpWorkspaceDetachActionAnnotation]; action != "" &&
		action != string(workspacev1alpha1.WorkspaceOnDetachDelete) {
		if action != string(workspacev1alpha1.WorkspaceOnDetachSuspend) || !terminallyFailed {
			return false, fmt.Errorf(
				"workspace %s froze detach action %q, which this controller cannot execute; refusing destructive settlement",
				workspace.Name, action,
			)
		}
		// The adapter marked this incarnation terminally Failed (for example
		// maxLifetime expiry destroyed the pool): no checkpoint remains, so
		// executing the frozen Suspend would preserve nothing and wedge
		// every later Session Task against a Suspended/Failed incarnation.
		// The terminal failure is settled destructively so the Session can
		// recreate a clean workspace.
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
	// A requester that terminated before it ever executed against this
	// workspace (its cold-resume flip stored the Delete action, but no
	// RuntimePool demand was ever stamped) must not destroy the retained
	// repository out from under another live queued continuation: the
	// successor attaches and stamps its own frozen action instead. A Task
	// that actually executed keeps the normal contract - its frozen Delete
	// destroys the filesystem before any continuation runs.
	if taskNeverHeldACPWorkspaceAttachment(task) {
		if deferred, retry, deferErr := r.deferACPSettlementToSuccessor(ctx, workspace, task); deferErr != nil {
			return false, deferErr
		} else if retry {
			return false, nil
		} else if deferred {
			return true, nil
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

// deferACPSettlementToSuccessor completes a requester's settlement without
// destroying the workspace when another live continuation is queued for the
// same incarnation, transferring the successor's effective detach policy.
// Callers decide whether the requester must be pre-attachment: Delete requires
// that restriction, while quota-exhausted Suspend can hand off after execution.
// deferred reports completion; retry requests a settle requeue. An UNLINKED
// successor's resolved Session UID must match the workspace's immutable Session
// identity when the durable session stores are available - a recreated same-name
// Session can never adopt the old incarnation's cleanup.
func (r *TaskReconciler) deferACPSettlementToSuccessor(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
) (bool, bool, error) {
	if workspace.Spec.SessionRef == nil {
		return false, false, nil
	}
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	candidates, successorErr := liveACPSessionContinuations(ctx, reader, workspace, task.UID)
	if successorErr != nil {
		return false, false, successorErr
	}
	// Every candidate is examined: one ineligible waiter (an out-of-policy
	// override, a recreated Session) must not conclude no successor exists
	// while a later valid continuation is queued behind it.
	var successor *corev1alpha1.Task
	successorAction := ""
	settlementBindingPending := false
	for _, candidate := range candidates {
		linked := strings.TrimSpace(candidate.Labels[acpExecutionWorkspaceLinkLabel]) == workspace.Name &&
			strings.TrimSpace(candidate.Annotations[acpExecutionWorkspaceUIDAnnotation]) == string(workspace.UID)
		if !linked && r.DurableControlStore != nil && r.SessionManager != nil && r.ControllerEpochManager != nil {
			resolvedUID, resolveErr := r.planACPWorkspaceSessionUID(ctx, candidate)
			if resolveErr != nil {
				// The candidate's Session identity cannot be proven right
				// now; retry rather than either destroying under a possibly
				// valid waiter or transferring ownership to a recreated
				// Session.
				return false, true, nil //nolint:nilerr // fail closed into a settle retry
			}
			if strings.TrimSpace(resolvedUID) != string(workspace.Spec.SessionRef.UID) {
				// A recreated same-name Session resolves a different
				// immutable UID and can never attach this incarnation: it is
				// not a successor. Later candidates may still be.
				continue
			}
		}
		// The deferral transfers the SUCCESSOR's policy onto the workspace:
		// the surviving waiter stamps its own action only at attachment, and
		// leaving the dead requester's Delete in place would destroy the
		// retained workspace if the successor also terminates before
		// attaching.
		_, allowed := effectiveACPWorkspaceDetachAction(candidate, workspace)
		if !allowed {
			// The waiter's explicit override is outside the class policy, so
			// its own attachment resolution will reject it. Later candidates
			// may still be valid successors.
			continue
		}
		binding, bindingErr := r.loadVerifiedACPWorkspaceBindingForSettlement(ctx, candidate)
		if bindingErr != nil {
			return false, false, bindingErr
		}
		if binding == nil {
			// A live waiter may still be in its first planning pass. Keep the
			// predecessor responsible until the waiter's immutable binding is
			// durable; linking it sooner can orphan the workspace if it dies.
			settlementBindingPending = true
			continue
		}
		if binding.ReusePolicy != corev1alpha1.WorkspaceReusePolicySession ||
			strings.TrimSpace(binding.SessionUID) != string(workspace.Spec.SessionRef.UID) {
			continue
		}
		action := binding.Class.EffectiveOnDetach
		if !slices.Contains(workspace.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetach(action)) {
			continue
		}
		successor = candidate
		successorAction = action
		break
	}
	if successor == nil {
		if settlementBindingPending {
			return false, true, nil
		}
		return false, false, nil
	}
	// The successor's POLICY lands on the workspace BEFORE its ownership
	// link: a linked successor can settle immediately, and settling while
	// the workspace still stored the dead requester's Delete would destroy
	// the retained repository the successor asked to keep. With this
	// ordering, any Task that acquires the link reads its own committed
	// action; if the link below fails and the successor vanishes, the
	// already-transferred action errs toward preservation (a stale Suspend
	// retains data for retention to reclaim, never a premature Delete).
	if workspace.Annotations[acpWorkspaceDetachActionAnnotation] != successorAction {
		base := workspace.DeepCopy()
		if workspace.Annotations == nil {
			workspace.Annotations = map[string]string{}
		}
		workspace.Annotations[acpWorkspaceDetachActionAnnotation] = successorAction
		if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				return false, true, nil
			}
			return false, false, err
		}
	}
	// Settlement ownership is transferred DURABLY before the predecessor
	// releases: an unlinked successor deleted between this scan and its
	// first reconcile would otherwise leave a workspace nobody settles (with
	// idleTimeout and maxLifetime unset, nothing would ever reclaim it).
	// A conflict or a successor deleted in the window retries the settle and
	// re-scans.
	if err := r.linkTaskToACPWorkspace(ctx, successor, workspace); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return false, true, nil
		}
		return false, false, err
	}
	return true, false, nil
}

func effectiveACPWorkspaceDetachAction(
	task *corev1alpha1.Task,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (string, bool) {
	action := string(workspace.Spec.Lifecycle.DefaultOnDetach)
	if task.Spec.Execution == nil || task.Spec.Execution.Workspace == nil {
		return action, true
	}
	raw := strings.TrimSpace(string(task.Spec.Execution.Workspace.OnDetach))
	if raw == "" {
		return action, true
	}
	requested := workspacev1alpha1.WorkspaceOnDetach(raw)
	if !slices.Contains(workspace.Spec.Lifecycle.AllowedOnDetach, requested) {
		return "", false
	}
	return string(requested), true
}

// taskNeverHeldACPWorkspaceAttachment reports a Task that terminated before
// any RuntimePool demand existed for it: ACP dispatch stamps the pool
// identity into status.execution only after the workspace attachment is
// admitted, so its absence proves the Task never executed against the
// workspace it links.
func taskNeverHeldACPWorkspaceAttachment(task *corev1alpha1.Task) bool {
	return task.Status.Execution == nil || strings.TrimSpace(task.Status.Execution.RuntimePoolName) == ""
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
	now := time.Now().UTC().Format(time.RFC3339Nano)
	workspace.Annotations[acpWorkspaceRevocationStartedAnnotation] = fmt.Sprintf("%d %s", epoch, now)
	// The detach instant is stamped as revocation begins so idle retention
	// never falls back to the creation timestamp while the epoch settles.
	workspace.Annotations[acpWorkspaceLastDetachedAnnotation] = now
	if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
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
	// Quarantine is terminal. Remove the credentials before releasing the
	// Task; the adapter repeats this cleanup, but settlement cannot claim
	// completion while an attachment bearer still exists.
	if err := r.deleteACPWorkspaceAttachmentCredentials(ctx, workspace); err != nil {
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
		reader := client.Reader(r.Client)
		if r.APIReader != nil {
			reader = r.APIReader
		}
		// A workspace held only by its cleanup finalizer (a settlement Delete
		// in flight) can still serve cached pre-delete status; copying it would
		// freeze a permanently stale Ready/Attached claim into the released
		// Task. Read through the API server so its state stays cleared instead.
		if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace); err == nil &&
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
