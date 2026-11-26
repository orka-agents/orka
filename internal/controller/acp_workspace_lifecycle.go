/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
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

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	workspaceprovider "github.com/orka-agents/orka-workspace/sdk"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/operatoraccess"
	"github.com/orka-agents/orka/internal/store"
)

const (
	acpExecutionWorkspaceLinkLabel          = "acp.workspace.orka.ai/execution-workspace"
	acpExecutionWorkspacePoolAnnotation     = "acp.workspace.orka.ai/runtime-pool"
	acpWorkspaceDetachActionAnnotation      = "acp.workspace.orka.ai/detach-action"
	acpTaskWorkspaceSettledAnnotation       = "acp.workspace.orka.ai/workspace-settled"
	acpTaskAttachmentEpochAnnotation        = "acp.workspace.orka.ai/attachment-epoch"
	acpWorkspaceLastSettledTaskAnnotation   = "acp.workspace.orka.ai/last-settled-task-uid"
	acpExecutionWorkspaceUIDAnnotation      = "acp.workspace.orka.ai/execution-workspace-uid"
	acpWorkspaceNamePrefix                  = "acp-ws"
	acpWorkspaceProviderConfigUIDAnnotation = "acp.workspace.orka.ai/provider-config-uid"
	acpWorkspaceBackendAnnotation           = "acp.workspace.orka.ai/backend"
	acpWorkspaceDurableAnnotation           = "acp.workspace.orka.ai/durable-workspace"
	acpWorkspaceSuspendModeAnnotation       = "acp.workspace.orka.ai/substrate-suspend-mode"
	acpWorkspaceResumedLineageAnnotation    = "acp.workspace.orka.ai/resumed-lineage"
	acpWorkspaceRuntimeNamespaceAnnotation  = "acp.workspace.orka.ai/runtime-namespace"
	acpWorkspaceDurableDataAbsentAnnotation = "acp.workspace.orka.ai/durable-data-absent"

	acpWorkspaceDurableSessionCommittedAnnotation = "acp.workspace.orka.ai/durable-session-committed"
	acpWorkspaceRevocationStartedAnnotation       = "acp.workspace.orka.ai/revocation-started-at"
	acpWorkspaceResumeRequestedAnnotation         = "acp.workspace.orka.ai/resume-requested-at"
	acpWorkspaceAttachmentTTL                     = 24 * time.Hour
)

var errACPWorkspaceBindingConflict = errors.New("execution workspace binding conflict")

var errACPWorkspaceTerminalFailure = errors.New("execution workspace failed terminally")

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
		blocked, fenceErr := r.taskBlockedByACPWorkspaceRetentionFence(ctx, task, binding, name, nil)
		if fenceErr != nil {
			return "", false, fenceErr
		}
		if blocked {
			return "", false, nil
		}
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
	blocked, fenceErr := r.taskBlockedByACPWorkspaceRetentionFence(ctx, task, binding, name, workspace)
	if fenceErr != nil {
		return "", false, fenceErr
	}
	if blocked {
		return "", false, nil
	}
	if !workspace.DeletionTimestamp.IsZero() || workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredDeleted {
		return "", false, nil
	}
	if err := verifyACPClassWorkspace(workspace, task, binding, plan.PoolName); err != nil {
		if queueACPClassWorkspaceBehindPredecessor(workspace, task, binding) {
			return "", false, nil
		}
		return "", false, err
	}
	tainted := false
	if workspace.Spec.Attachment == nil || workspace.Spec.Attachment.TaskRef.UID != task.UID {
		tainted, err = acpWorkspaceOperatorTainted(ctx, uncachedReader(r.APIReader, r.Client), workspace)
		if err != nil {
			return "", false, err
		}
	}
	if tainted {
		if workspace.Spec.Attachment != nil || acpWorkspaceRevocationStampMatchesCurrentEpoch(workspace) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("%w: workspace %s: %w; delete the workspace or start a new Session",
			errACPWorkspaceTerminalFailure, workspace.Name, operatoraccess.ErrTainted)
	}
	if acpWorkspaceRetainedDataLost(workspace) && !acpWorkspaceRevocationStampMatchesCurrentEpoch(workspace) {
		next := "start a new Session"
		if strings.TrimSpace(workspace.Annotations[acpWorkspaceDurableSessionCommittedAnnotation]) == "" {
			next = "retry the Task"
		}
		return "", false, fmt.Errorf("%w: workspace %s: its runtime stopped without retained data and cannot be resumed; %s",
			errACPWorkspaceTerminalFailure, workspace.Name, next)
	}
	if err := r.backfillACPWorkspaceSuspendedCap(ctx, workspace, binding.Class.MaxSuspendedWorkspaces); err != nil {
		return "", false, err
	}
	if err := r.backfillACPWorkspaceSuspendAfterDetach(ctx, workspace, binding.Class.SuspendAfterDetach); err != nil {
		return "", false, err
	}
	if err := r.linkTaskToACPWorkspace(ctx, task, workspace); err != nil {
		return "", false, err
	}
	if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
		switch workspace.Status.State {
		case workspacev1alpha1.ExecutionWorkspaceStateSuspended:
			now := time.Now()
			if acpWorkspaceSpeculativeResumePending(workspace, now) {
				return "", false, nil
			}
			deadline, owner, recorded := parseACPWorkspaceSpeculativeStart(workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation])
			ownExpired := recorded && owner == string(task.UID) && !now.Before(deadline)
			if !ownExpired && acpWorkspaceSuspendCapable(workspace) && acpWorkspaceRetainedDataAvailable(workspace) {
				if err := r.requestACPWorkspaceSpeculativeResume(ctx, workspace, task, binding, plan.PoolName, now); err != nil {
					return "", false, client.IgnoreNotFound(err)
				}
				return "", false, nil
			}
			if err := r.flipACPWorkspaceForColdResume(ctx, workspace, task, binding.Class.EffectiveOnDetach); err != nil {
				return "", false, client.IgnoreNotFound(err)
			}
			return "", false, nil
		case workspacev1alpha1.ExecutionWorkspaceStateFailed:
			return "", false, fmt.Errorf(
				"%w: workspace %s suspension failed without a preserved data checkpoint; cold resume is impossible",
				errACPWorkspaceBindingConflict, workspace.Name,
			)
		default:
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
			return "", false, nil
		}
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
			if err := r.markACPTaskAttachmentEpoch(ctx, task, workspace.Spec.Attachment.Epoch); err != nil {
				return "", false, err
			}
			return name, true, r.linkTaskToACPWorkspace(ctx, task, workspace)
		}
		return "", false, nil
	}
	if acpWorkspaceRevocationStampMatchesCurrentEpoch(workspace) {
		return "", false, nil
	}
	if err := r.linkTaskToACPWorkspace(ctx, task, workspace); err != nil {
		return "", false, err
	}
	if err := r.stampACPWorkspaceWarmDemand(ctx, workspace, task, time.Now()); err != nil {
		if apierrors.IsConflict(err) {
			return "", false, nil
		}
		return "", false, client.IgnoreNotFound(err)
	}
	if r.acpWorkspaceAwaitsStagedInstall(ctx, workspace, task, plan.PoolName, time.Now()) {
		return "", false, nil
	}
	manager := WorkspaceAttachmentManager{Client: r.Client, APIReader: r.APIReader, LeaseTTL: acpWorkspaceAttachmentTTL}
	attachment, err := manager.attach(ctx, workspace, task, map[string]string{
		acpWorkspaceDetachActionAnnotation: binding.Class.EffectiveOnDetach,
	}, func(current *workspacev1alpha1.ExecutionWorkspace, candidate *corev1alpha1.Task, now time.Time) error {
		if err := validateACPClassWorkspaceAttachmentAttempt(current, candidate, binding, plan.PoolName, now); err != nil {
			return err
		}
		return refuseOperatorTaintedACPWorkspaceHandoff(ctx, uncachedReader(r.APIReader, r.Client), current, candidate)
	})
	if err != nil {
		if errors.Is(err, ErrWorkspaceAttachmentLocked) {
			return "", false, nil
		}
		if strings.Contains(err.Error(), "not reusable") || strings.Contains(err.Error(), "revalidate workspace") {
			return "", false, nil
		}
		return "", false, err
	}
	acpPreDispatch.mark(task, acpPreDispatchWorkspaceAttachRequested)
	if err := r.markACPTaskAttachmentEpoch(ctx, task, attachment.Epoch); err != nil {
		return "", false, err
	}
	return "", false, nil
}

func refuseOperatorTaintedACPWorkspaceHandoff(
	ctx context.Context,
	reader client.Reader,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
) error {
	if workspace.Spec.Attachment != nil && workspace.Spec.Attachment.TaskRef.UID == task.UID {
		return nil
	}
	tainted, err := acpWorkspaceOperatorTainted(ctx, reader, workspace)
	if err != nil {
		return err
	}
	if tainted {
		return fmt.Errorf("workspace %s: %w", workspace.Name, operatoraccess.ErrTainted)
	}
	return nil
}

func validateUnattachedACPClassWorkspaceReuse(workspace *workspacev1alpha1.ExecutionWorkspace, now time.Time) error {
	if workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady {
		return fmt.Errorf("workspace desired state %q is not reusable for direct attachment", workspace.Spec.DesiredState)
	}
	if deadline, open := acpWorkspaceWarmWindowOpen(workspace); open {
		drained := strings.TrimSpace(workspace.Annotations[acpWorkspaceWarmWindowDrainedAnnotation]) != ""
		limit := deadline
		if !drained {
			limit = deadline.Add(acpWorkspaceWarmExpiryGrace)
		}
		if !now.Before(limit) {
			return fmt.Errorf("workspace warm-detach window expired at %s; wait for suspension", deadline.UTC().Format(time.RFC3339))
		}
	}
	return nil
}

func validateACPClassWorkspaceAttachmentAttempt(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	binding *ACPRuntimeWorkspaceBinding,
	poolName string,
	now time.Time,
) error {
	if err := verifyACPClassWorkspace(workspace, task, binding, poolName); err != nil {
		return err
	}
	if operatoraccess.Tainted(workspace.Annotations) &&
		(workspace.Spec.Attachment == nil || workspace.Spec.Attachment.TaskRef.UID != task.UID) {
		return fmt.Errorf("workspace %s: %w", workspace.Name, operatoraccess.ErrTainted)
	}
	if workspace.Spec.Attachment == nil {
		if err := validateUnattachedACPClassWorkspaceReuse(workspace, now); err != nil {
			return err
		}
	}
	err := validateWorkspaceAttachmentAttempt(workspace, task, now)
	if err == nil || binding.Class.ControllerName == "" || workspace.Spec.Attachment != nil {
		return err
	}
	if !workspaceCurrentlyAdmittedByCore(workspace) || workspace.Status.ObservedGeneration != workspace.Generation ||
		workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady ||
		workspace.Status.State != workspacev1alpha1.ExecutionWorkspaceStatePending ||
		workspaceprovider.ConditionIsTrue(workspace.Status.Conditions, string(workspacev1alpha1.ConditionWorkspaceQuarantined)) ||
		acpWorkspaceRevocationStampMatchesCurrentEpoch(workspace) {
		return err
	}
	request, observation := workspace.Spec.Workload, workspace.Status.Allocation
	if request == nil && observation == nil && workspace.Spec.AttachmentEpoch == 0 && workspace.Status.AttachedEpoch == 0 {
		return nil
	}
	if request != nil && observation != nil && observation.State == workspacev1alpha1.AllocationStopped &&
		observation.Sequence == request.Sequence && observation.Key == request.Key &&
		observation.Identity.RequestRevision == request.Revision && observation.Startup == nil && observation.RetainedData != nil {
		return nil
	}
	if acpWorkspaceSpeculativeStartAttachable(workspace) {
		return nil
	}
	return err
}

func failedACPWorkspaceRetainsCheckpoint(workspace *workspacev1alpha1.ExecutionWorkspace, now time.Time) bool {
	if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredDeleted {
		return false
	}
	if remaining, bounded := acpWorkspaceMaxLifetimeRemaining(workspace, now); bounded && remaining <= 0 {
		return false
	}
	request := workspace.Spec.Workload
	return request != nil && request.RetainedData != nil && request.RetainedData.Valid()
}

func (r *TaskReconciler) recordACPWorkspaceDetachAction(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	binding *ACPRuntimeWorkspaceBinding,
) error {
	action := binding.Class.EffectiveOnDetach
	_, warmDeadline := workspace.Annotations[acpWorkspaceSuspendAtAnnotation]
	_, warmPending := workspace.Annotations[acpWorkspaceSuspendPendingAnnotation]
	if workspace.Annotations[acpWorkspaceDetachActionAnnotation] == action &&
		workspace.Annotations[acpWorkspaceRevocationStartedAnnotation] == "" &&
		workspace.Annotations[acpWorkspaceResumeRequestedAnnotation] == "" && !warmDeadline && !warmPending &&
		workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation] == "" &&
		workspace.Annotations[acpWorkspaceAttachmentIntentAnnotation] == "" {
		return nil
	}
	base := workspace.DeepCopy()
	if workspace.Annotations == nil {
		workspace.Annotations = map[string]string{}
	}
	workspace.Annotations[acpWorkspaceDetachActionAnnotation] = action
	delete(workspace.Annotations, acpWorkspaceRevocationStartedAnnotation)
	delete(workspace.Annotations, acpWorkspaceResumeRequestedAnnotation)
	delete(workspace.Annotations, acpWorkspaceSuspendAtAnnotation)
	delete(workspace.Annotations, acpWorkspaceSuspendPendingAnnotation)
	delete(workspace.Annotations, acpWorkspaceWarmWindowDrainedAnnotation)
	delete(workspace.Annotations, acpWorkspaceSpeculativeStartAnnotation)
	delete(workspace.Annotations, acpWorkspaceAttachmentIntentAnnotation)
	return r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

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
		!workspaceHasACPControllerOwnership(workspace) ||
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
		return false, nil
	}
	now := time.Now()
	if maxLifetime := workspace.Spec.Lifecycle.MaxLifetime; maxLifetime != nil && maxLifetime.Duration > 0 &&
		!now.Before(workspace.CreationTimestamp.Add(maxLifetime.Duration)) {
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
		!workspaceHasACPControllerOwnership(workspace) ||
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
	if binding.RestoreFrom != nil && binding.Class.ControllerName != "" {
		if err := r.validateACPWorkspaceRestoreCheckpoint(ctx, task, binding); err != nil {
			return nil, err
		}
	}
	lifecycle, err := binding.Class.Lifecycle()
	if err != nil {
		return nil, err
	}
	workspace := &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: task.Namespace,
			Name:      name,
			Labels: map[string]string{
				workspacev1alpha1.ProviderControllerLabel: acpWorkspaceBindingControllerLabel(binding),
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
	defaultWindow, err := r.acpWorkspaceDefaultSuspendAfterDetachAnnotations(ctx, task.Namespace, binding)
	if err != nil {
		return nil, err
	}
	maps.Copy(workspace.Annotations, defaultWindow)
	if binding.Class.SuspendMode == acpWorkspaceSuspendDataOnly &&
		slices.Contains(binding.Class.AllowedOnDetach, string(workspacev1alpha1.WorkspaceOnDetachSuspend)) {
		workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation] =
			formatACPWorkspaceSpeculativeStart(time.Now().Add(acpWorkspaceSpeculativeStartTTL), task.UID)
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
		if err := controllerutil.SetOwnerReference(task, workspace, r.Scheme); err != nil {
			return nil, fmt.Errorf("set execution workspace owner: %w", err)
		}
	}
	if workspace.Spec.SessionRef != nil {
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
	if _, recorded := workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation]; recorded {
		acpPreDispatch.mark(task, acpPreDispatchSpeculativeStart)
	}
	return workspace, nil
}

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
		acpWorkspaceDetachActionAnnotation:      binding.Class.EffectiveOnDetach,
		acpWorkspaceResumeRequestedAnnotation:   time.Now().UTC().Format(time.RFC3339Nano) + " " + requesterName + " " + requesterUID,
	}
	if binding.Class.MaxSuspendedWorkspaces != nil {
		annotations[acpWorkspaceMaxSuspendedAnnotation] = strconv.FormatInt(int64(*binding.Class.MaxSuspendedWorkspaces), 10)
	}
	if binding.Class.SuspendAfterDetach != "" {
		annotations[acpWorkspaceSuspendAfterDetachAnnotation] = binding.Class.SuspendAfterDetach
	}
	if runtimeNamespace != "" {
		annotations[acpWorkspaceRuntimeNamespaceAnnotation] = runtimeNamespace
	}
	if binding.Class.SuspendMode == acpWorkspaceSuspendDataOnly {
		annotations[acpWorkspaceDurableAnnotation] = booleanTrueValue
	}
	return annotations
}

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
	if err := verifyACPClassWorkspaceBindings(workspace, binding, poolName); err != nil {
		return err
	}
	if err := validateACPWorkspaceSuspendedCap(workspace, binding.Class.MaxSuspendedWorkspaces); err != nil {
		return err
	}
	if err := validateACPWorkspaceSuspendAfterDetach(workspace, binding.Class.SuspendAfterDetach); err != nil {
		return err
	}
	if workspace.Spec.Slot != binding.WorkspaceSlot {
		return fmt.Errorf("%w: workspace %s slot does not match the frozen binding", errACPWorkspaceBindingConflict, workspace.Name)
	}
	if err := verifyACPClassWorkspaceOwnership(workspace, task, binding); err != nil {
		return err
	}
	if workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady &&
		!acpClassWorkspaceResumesFromSuspended(workspace, binding) {
		return fmt.Errorf("%w: workspace %s desired state %q cannot admit new work", errACPWorkspaceBindingConflict, workspace.Name, workspace.Spec.DesiredState)
	}
	return nil
}

func verifyACPClassWorkspaceBindings(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	binding *ACPRuntimeWorkspaceBinding,
	poolName string,
) error {
	if workspace.Labels[workspacev1alpha1.ProviderControllerLabel] != acpWorkspaceBindingControllerLabel(binding) ||
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
	return nil
}

func verifyACPClassWorkspaceOwnership(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	binding *ACPRuntimeWorkspaceBinding,
) error {
	if binding.ReusePolicy == corev1alpha1.WorkspaceReusePolicySession {
		if workspace.Spec.SessionRef == nil || string(workspace.Spec.SessionRef.UID) != binding.SessionUID {
			return fmt.Errorf("%w: workspace %s session identity does not match the immutable Session UID", errACPWorkspaceBindingConflict, workspace.Name)
		}
		return nil
	}
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
	return nil
}

func acpClassWorkspaceResumesFromSuspended(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	binding *ACPRuntimeWorkspaceBinding,
) bool {
	resumeProviderSupported := binding.Class.ControllerName != "" &&
		string(binding.Provider) == binding.Class.ControllerName &&
		binding.Class.LifecycleContractVersion == workspacev1alpha1.LifecycleContractV1
	if binding.Class.ControllerName == "" {
		resumeProviderSupported = binding.Provider == corev1alpha1.WorkspaceProviderSubstrate ||
			binding.Provider == corev1alpha1.WorkspaceProviderAgentSandbox
	}
	return workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended &&
		workspace.Spec.Attachment == nil &&
		resumeProviderSupported &&
		binding.ReusePolicy == corev1alpha1.WorkspaceReusePolicySession &&
		binding.Class.SuspendMode == acpWorkspaceSuspendDataOnly
}

func validateACPWorkspaceSuspendedCap(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	expected *int32,
) error {
	raw, present := workspace.Annotations[acpWorkspaceMaxSuspendedAnnotation]
	if !present {
		return nil
	}
	if expected == nil || raw != strconv.FormatInt(int64(*expected), 10) {
		return fmt.Errorf("%w: workspace %s suspended-workspace cap does not match the frozen binding", errACPWorkspaceBindingConflict, workspace.Name)
	}
	return nil
}

func (r *TaskReconciler) backfillACPWorkspaceSuspendedCap(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	expected *int32,
) error {
	if expected == nil {
		return nil
	}
	if _, present := workspace.Annotations[acpWorkspaceMaxSuspendedAnnotation]; present {
		return nil
	}
	base := workspace.DeepCopy()
	if workspace.Annotations == nil {
		workspace.Annotations = map[string]string{}
	}
	workspace.Annotations[acpWorkspaceMaxSuspendedAnnotation] = strconv.FormatInt(int64(*expected), 10)
	if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("backfill workspace %s suspended-workspace cap: %w", workspace.Name, err)
	}
	return nil
}

//nolint:gocyclo // Settlement keeps the credential revocation and destructive lifecycle gates in one auditable state machine.
func (r *TaskReconciler) settleACPClassWorkspace(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	if task == nil {
		return true, nil
	}
	if !r.WorkspaceSettlementProtected {
		logf.FromContext(ctx).Info("skipping privileged class-workspace settlement; Task provenance admission is disabled",
			"task", task.Name)
		return true, nil
	}
	name := strings.TrimSpace(task.Labels[acpExecutionWorkspaceLinkLabel])
	if name == "" {
		return true, nil
	}
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
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
	if !workspace.DeletionTimestamp.IsZero() || workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredDeleted {
		return true, nil
	}
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
	if task.Annotations[acpTaskWorkspaceSettledAnnotation] != "" {
		return true, nil
	}
	if changed, err := r.releaseACPWorkspaceSpeculativeClaims(ctx, workspace, task); err != nil || changed {
		return false, err
	}
	taskOwnsLiveAttachment := workspace.Spec.Attachment != nil &&
		workspace.Spec.Attachment.TaskRef.UID == task.UID
	taskOwnsPendingResumeDemand := acpWorkspaceResumeDemandBelongsToTask(
		workspace.Annotations[acpWorkspaceResumeRequestedAnnotation], string(task.UID),
	)
	if !taskOwnsLiveAttachment && !taskOwnsPendingResumeDemand && acpWorkspaceSettlementReceiptCoversTask(
		workspace.Annotations[acpWorkspaceLastSettledTaskAnnotation],
		string(task.UID),
		acpTaskRecordedAttachmentEpoch(task),
	) {
		return true, r.markACPTaskWorkspaceSettled(ctx, task)
	}
	if task.Annotations[acpExecutionWorkspaceUIDAnnotation] != string(workspace.UID) {
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
			return true, r.markACPTaskWorkspaceSettled(ctx, task)
		}
		if err := r.markACPTaskAttachmentEpoch(ctx, task, attachment.Epoch); err != nil {
			return false, err
		}
		if err := r.markACPWorkspaceRevocationStarted(ctx, workspace, task, attachment.Epoch); err != nil {
			return false, err
		}
		if err := manager.BeginRevocation(ctx, workspace, attachment.Epoch); err != nil {
			return false, err
		}
		return false, nil
	}
	if workspace.Spec.Attachment == nil && workspace.Spec.AttachmentEpoch > 0 {
		epoch := workspace.Spec.AttachmentEpoch
		if workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
			if err := r.markACPWorkspaceRevocationStarted(ctx, workspace, task, epoch); err != nil {
				return false, err
			}
		}
		if err := manager.FinalizeRevocation(ctx, workspace, epoch, attachmentSecretName(workspace.Name, epoch)); err != nil {
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
		if err := r.deleteACPWorkspaceAttachmentCredentials(ctx, workspace); err != nil {
			return false, err
		}
		return true, nil
	}
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
	terminallyFailed := workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateFailed
	operatorTainted, err := acpWorkspaceOperatorTainted(ctx, uncachedReader(r.APIReader, r.Client), workspace)
	if err != nil {
		return false, err
	}
	if terminallyFailed && !operatorTainted &&
		workspace.Annotations[acpWorkspaceDetachActionAnnotation] == string(workspacev1alpha1.WorkspaceOnDetachSuspend) &&
		failedACPWorkspaceRetainsCheckpoint(workspace, time.Now()) {
		if err := clearACPWorkspaceSuspendAt(ctx, r.Client, workspace, false, time.Now()); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return true, r.markACPTaskWorkspaceSettled(ctx, task)
	}

	if workspace.Annotations[acpWorkspaceDetachActionAnnotation] == string(workspacev1alpha1.WorkspaceOnDetachSuspend) &&
		!terminallyFailed && !operatorTainted {
		if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
			taskEpoch := acpTaskRecordedAttachmentEpoch(task)
			existingUID, existingEpoch, existingOK := parseACPWorkspaceSettlementReceipt(
				workspace.Annotations[acpWorkspaceLastSettledTaskAnnotation])
			clearDemand := acpWorkspaceResumeDemandBelongsToTask(
				workspace.Annotations[acpWorkspaceResumeRequestedAnnotation], string(task.UID))
			writeReceipt := existingUID != string(task.UID) && (!existingOK || existingEpoch <= taskEpoch)
			if writeReceipt || clearDemand {
				base := workspace.DeepCopy()
				if workspace.Annotations == nil {
					workspace.Annotations = map[string]string{}
				}
				if writeReceipt {
					workspace.Annotations[acpWorkspaceLastSettledTaskAnnotation] =
						formatACPWorkspaceSettlementReceipt(string(task.UID), taskEpoch)
				}
				if clearDemand {
					delete(workspace.Annotations, acpWorkspaceResumeRequestedAnnotation)
				}
				if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
					if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
						return false, nil
					}
					return false, err
				}
			}
			return true, r.markACPTaskWorkspaceSettled(ctx, task)
		}
		if strings.TrimSpace(workspace.Annotations[acpWorkspaceDurableSessionCommittedAnnotation]) == "" {
			if err := r.Delete(ctx, workspace, deleteCurrentObjectPreconditions(workspace)...); err != nil &&
				!apierrors.IsNotFound(err) {
				if apierrors.IsConflict(err) {
					return false, nil
				}
				return false, err
			}
			return true, nil
		}
		if applied, retry, warmErr := r.settleACPWorkspaceWarm(ctx, workspace, task, time.Now()); warmErr != nil {
			return false, warmErr
		} else if retry {
			return false, nil
		} else if applied {
			return true, r.markACPTaskWorkspaceSettled(ctx, task)
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
			string(task.UID),
			acpTaskRecordedAttachmentEpoch(task),
		)
		switch {
		case errors.Is(err, errACPSuspendQuotaExhausted):
			if clearErr := clearACPWorkspaceSuspendAt(ctx, r.Client, workspace, false, time.Now()); clearErr != nil {
				if apierrors.IsConflict(clearErr) || apierrors.IsNotFound(clearErr) {
					return false, nil
				}
				return false, clearErr
			}
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
		return true, r.markACPTaskWorkspaceSettled(ctx, task)
	}
	if action := workspace.Annotations[acpWorkspaceDetachActionAnnotation]; action != "" &&
		action != string(workspacev1alpha1.WorkspaceOnDetachDelete) {
		if action != string(workspacev1alpha1.WorkspaceOnDetachSuspend) || (!terminallyFailed && !operatorTainted) {
			return false, fmt.Errorf(
				"workspace %s froze detach action %q, which this controller cannot execute; refusing destructive settlement",
				workspace.Name, action,
			)
		}
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
	if taskNeverHeldACPWorkspaceAttachment(task) && !operatorTainted {
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

//nolint:gocyclo // Candidate validation and ordered ownership transfer form one fail-closed handoff.
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
	var successor *corev1alpha1.Task
	successorAction := ""
	settlementBindingPending := false
	for _, candidate := range candidates {
		linked := strings.TrimSpace(candidate.Labels[acpExecutionWorkspaceLinkLabel]) == workspace.Name &&
			strings.TrimSpace(candidate.Annotations[acpExecutionWorkspaceUIDAnnotation]) == string(workspace.UID)
		if !linked && r.DurableControlStore != nil && r.SessionManager != nil && r.ControllerEpochManager != nil {
			resolvedUID, resolveErr := r.planACPWorkspaceSessionUID(ctx, candidate)
			if resolveErr != nil {
				return false, true, nil //nolint:nilerr // fail closed into a settle retry
			}
			if strings.TrimSpace(resolvedUID) != string(workspace.Spec.SessionRef.UID) {
				continue
			}
		}
		_, allowed := effectiveACPWorkspaceDetachAction(candidate, workspace)
		if !allowed {
			continue
		}
		binding, bindingErr := r.loadVerifiedACPWorkspaceBindingForSettlement(ctx, candidate)
		if bindingErr != nil {
			return false, false, bindingErr
		}
		if binding == nil {
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
	transferSuccessorAction := func() (bool, error) {
		if workspace.Annotations[acpWorkspaceDetachActionAnnotation] == successorAction {
			return false, nil
		}
		base := workspace.DeepCopy()
		if workspace.Annotations == nil {
			workspace.Annotations = map[string]string{}
		}
		workspace.Annotations[acpWorkspaceDetachActionAnnotation] = successorAction
		if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}
		return false, nil
	}
	if successorAction == string(workspacev1alpha1.WorkspaceOnDetachSuspend) {
		if retry, err := transferSuccessorAction(); err != nil {
			return false, false, err
		} else if retry {
			return false, true, nil
		}
	}
	if err := r.linkTaskToACPWorkspace(ctx, successor, workspace); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return false, true, nil
		}
		return false, false, err
	}
	if successorAction == string(workspacev1alpha1.WorkspaceOnDetachDelete) {
		if retry, err := transferSuccessorAction(); err != nil {
			return false, false, err
		} else if retry {
			return false, true, nil
		}
	}
	if _, stamped := workspace.Annotations[acpWorkspaceRevocationStartedAnnotation]; stamped {
		base := workspace.DeepCopy()
		delete(workspace.Annotations, acpWorkspaceRevocationStartedAnnotation)
		if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				return false, true, nil
			}
			return false, false, err
		}
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

func taskNeverHeldACPWorkspaceAttachment(task *corev1alpha1.Task) bool {
	return task.Status.Execution == nil || strings.TrimSpace(task.Status.Execution.RuntimePoolName) == ""
}

func settlementWorkspaceBelongsToTask(workspace *workspacev1alpha1.ExecutionWorkspace, task *corev1alpha1.Task) bool {
	if !workspaceHasACPControllerOwnership(workspace) {
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

func (r *TaskReconciler) markACPWorkspaceRevocationStarted(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	epoch int64,
) error {
	if workspace.Spec.Attachment == nil && workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
		return nil
	}
	if stampedEpoch, _, ok := parseACPWorkspaceRevocationStamp(workspace.Annotations[acpWorkspaceRevocationStartedAnnotation]); ok && stampedEpoch == epoch {
		return nil
	}
	base := workspace.DeepCopy()
	if workspace.Annotations == nil {
		workspace.Annotations = map[string]string{}
	}
	startedAt := time.Now().UTC()
	now := startedAt.Format(time.RFC3339Nano)
	workspace.Annotations[acpWorkspaceRevocationStartedAnnotation] = fmt.Sprintf("%d %s", epoch, now)
	workspace.Annotations[acpWorkspaceLastDetachedAnnotation] = now
	delete(workspace.Annotations, acpWorkspaceSuspendPendingAnnotation)
	delete(workspace.Annotations, acpWorkspaceWarmWindowDrainedAnnotation)
	if window := acpWorkspaceWarmDetachWindow(workspace, task); window > 0 {
		workspace.Annotations[acpWorkspaceSuspendAtAnnotation] = startedAt.Add(window).Format(time.RFC3339Nano)
	} else {
		delete(workspace.Annotations, acpWorkspaceSuspendAtAnnotation)
	}
	if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	return nil
}

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

func (r *TaskReconciler) quarantineACPWorkspacePastDetachTimeout(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (bool, error) {
	stampedEpoch, startedAt, ok := parseACPWorkspaceRevocationStamp(workspace.Annotations[acpWorkspaceRevocationStartedAnnotation])
	if !ok || stampedEpoch != workspace.Spec.AttachmentEpoch {
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
	if err := r.deleteACPWorkspaceAttachmentCredentials(ctx, workspace); err != nil {
		return false, err
	}
	return true, nil
}

func (r *TaskReconciler) markACPTaskAttachmentEpoch(
	ctx context.Context,
	task *corev1alpha1.Task,
	epoch int64,
) error {
	value := strconv.FormatInt(epoch, 10)
	if task.Annotations[acpTaskAttachmentEpochAnnotation] == value {
		return nil
	}
	base := task.DeepCopy()
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[acpTaskAttachmentEpochAnnotation] = value
	if err := r.Patch(ctx, task, client.MergeFrom(base)); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func acpTaskRecordedAttachmentEpoch(task *corev1alpha1.Task) int64 {
	epoch, err := strconv.ParseInt(strings.TrimSpace(task.Annotations[acpTaskAttachmentEpochAnnotation]), 10, 64)
	if err != nil || epoch < 0 {
		return 0
	}
	return epoch
}

func parseACPWorkspaceSettlementReceipt(raw string) (string, int64, bool) {
	fields := strings.Fields(strings.TrimSpace(raw))
	switch len(fields) {
	case 1:
		return fields[0], 0, true
	case 2:
		epoch, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || epoch < 0 {
			return "", 0, false
		}
		return fields[0], epoch, true
	default:
		return "", 0, false
	}
}

func acpWorkspaceResumeDemandBelongsToTask(raw, taskUID string) bool {
	fields := strings.Fields(strings.TrimSpace(raw))
	return taskUID != "" && len(fields) >= 3 && fields[2] == taskUID
}

func acpWorkspaceSettlementReceiptCoversTask(raw, taskUID string, taskEpoch int64) bool {
	receiptUID, receiptEpoch, ok := parseACPWorkspaceSettlementReceipt(raw)
	if !ok {
		return false
	}
	if receiptUID == taskUID {
		return true
	}
	return receiptEpoch > 0 && (taskEpoch == 0 || receiptEpoch >= taskEpoch)
}

func formatACPWorkspaceSettlementReceipt(taskUID string, epoch int64) string {
	return taskUID + " " + strconv.FormatInt(epoch, 10)
}

func (r *TaskReconciler) markACPTaskWorkspaceSettled(
	ctx context.Context,
	task *corev1alpha1.Task,
) error {
	if task.Annotations[acpTaskWorkspaceSettledAnnotation] != "" {
		return nil
	}
	base := task.DeepCopy()
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[acpTaskWorkspaceSettledAnnotation] = booleanTrueValue
	if err := r.Patch(ctx, task, client.MergeFrom(base)); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *TaskReconciler) reconcileACPClassWorkspaceSettlement(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	if task == nil || task.Spec.Type != corev1alpha1.TaskTypeAgent || task.Status.Execution == nil {
		return true, nil
	}
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
	if task.DeletionTimestamp.IsZero() &&
		task.Status.Phase != corev1alpha1.TaskPhaseSucceeded &&
		task.Status.Phase != corev1alpha1.TaskPhaseFailed &&
		task.Status.Phase != corev1alpha1.TaskPhaseCancelled {
		return true, nil
	}
	done, err := r.settleACPClassWorkspace(ctx, task)
	if err != nil || !done {
		return done, err
	}
	return true, r.refreshACPReleasedWorkspaceProjection(ctx, task)
}

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
		if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace); err == nil &&
			workspace.DeletionTimestamp.IsZero() &&
			workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined &&
			workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredDeleted &&
			task.Annotations[acpExecutionWorkspaceUIDAnnotation] == string(workspace.UID) {
			next.State = string(workspace.Status.State)
		}
	}
	base := task.DeepCopy()
	task.Status.ExecutionWorkspace = next
	return r.Status().Patch(ctx, task, client.MergeFrom(base))
}

func acpWorkspaceBindingControllerLabel(binding *ACPRuntimeWorkspaceBinding) string {
	if binding != nil && binding.Class != nil && binding.Class.ControllerName != "" {
		return binding.Class.ControllerName
	}
	return acpWorkspaceControllerLabelValue
}
