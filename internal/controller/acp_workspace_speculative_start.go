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
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/operatoraccess"
)

const (
	acpWorkspaceSpeculativeStartAnnotation = "acp.workspace.orka.ai/speculative-start"
	acpWorkspaceSpeculativeStartTTL        = 2 * time.Minute
)

func formatACPWorkspaceSpeculativeStart(deadline time.Time, taskUID types.UID) string {
	return deadline.UTC().Format(time.RFC3339Nano) + " " + string(taskUID)
}

func parseACPWorkspaceSpeculativeStart(raw string) (time.Time, string, bool) {
	fields := strings.Fields(raw)
	if len(fields) != 2 || fields[1] == "" {
		return time.Time{}, "", false
	}
	deadline, err := time.Parse(time.RFC3339Nano, fields[0])
	if err != nil {
		return time.Time{}, "", false
	}
	return deadline, fields[1], true
}

func acpWorkspaceSpeculativeStartAllowed(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	if workspace == nil || !workspace.DeletionTimestamp.IsZero() ||
		workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady ||
		workspace.Spec.Attachment != nil ||
		strings.TrimSpace(workspace.Annotations[acpWorkspaceResumeRequestedAnnotation]) == "" ||
		strings.TrimSpace(workspace.Annotations[acpWorkspaceRevocationStartedAnnotation]) != "" {
		return false
	}
	return workspace.Status.State != workspacev1alpha1.ExecutionWorkspaceStateFailed &&
		!acpWorkspaceQuarantined(workspace) && !operatoraccess.Tainted(workspace.Annotations)
}

func acpWorkspaceSpeculativeStartHoldsPool(workspace *workspacev1alpha1.ExecutionWorkspace, now time.Time) (time.Time, bool) {
	if !acpWorkspaceSpeculativeStartAllowed(workspace) {
		return time.Time{}, false
	}
	deadline, _, ok := parseACPWorkspaceSpeculativeStart(workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation])
	if !ok || !now.Before(deadline) {
		return time.Time{}, false
	}
	return deadline, true
}

func acpWorkspaceSpeculativeStartAttachable(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	if _, _, ok := parseACPWorkspaceSpeculativeStart(workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation]); !ok {
		return false
	}
	request, observation, retirement := workspace.Spec.Workload, workspace.Status.Allocation, workspace.Spec.Retirement
	if request == nil {
		return false
	}
	if observation == nil {
		return retirement == nil && request.Sequence == 1 && request.PreviousInstance == nil &&
			workspace.Spec.AttachmentEpoch == 0 && workspace.Status.AttachedEpoch == 0
	}
	if observation.Key != request.Key || observation.Sequence != request.Sequence ||
		(observation.Identity.RequestRevision != "" && observation.Identity.RequestRevision != request.Revision) {
		return false
	}
	if observation.State == workspacev1alpha1.AllocationPending {
		return retirement == nil
	}
	return observation.State == workspacev1alpha1.AllocationStopped && observation.Startup == nil &&
		observation.Identity.Valid() && !acpWorkspaceSuspendCapable(workspace) &&
		(retirement == nil || retirement.Sequence == request.Sequence)
}

func acpWorkspaceSuspendCapable(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	return slices.Contains(workspace.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend) &&
		workspace.Annotations[acpWorkspaceSuspendModeAnnotation] == acpWorkspaceSuspendDataOnly
}

func acpWorkspaceRetainedDataLost(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	request, observation := workspace.Spec.Workload, workspace.Status.Allocation
	return workspace.Spec.Attachment == nil && request != nil && observation != nil && acpWorkspaceSuspendCapable(workspace) &&
		workspace.Status.ObservedGeneration == workspace.Generation &&
		observation.State == workspacev1alpha1.AllocationStopped && observation.Startup == nil && observation.RetainedData == nil &&
		observation.Key == request.Key && observation.Sequence == request.Sequence
}

func acpWorkspaceSpeculativeStartEligible(workspace *workspacev1alpha1.ExecutionWorkspace, task *corev1alpha1.Task) bool {
	if task == nil || task.UID == "" || !acpWorkspaceSpeculativeStartAllowed(workspace) ||
		!acpWorkspaceResumeDemandBelongsToTask(workspace.Annotations[acpWorkspaceResumeRequestedAnnotation], string(task.UID)) ||
		strings.TrimSpace(task.Annotations[acpExecutionWorkspaceUIDAnnotation]) != string(workspace.UID) {
		return false
	}
	if !acpWorkspaceSuspendCapable(workspace) {
		return false
	}
	if _, warm := acpWorkspaceWarmWindowOpen(workspace); warm {
		return false
	}
	if workspace.Annotations[acpWorkspaceResumedLineageAnnotation] == booleanTrueValue {
		return workspace.Spec.Workload != nil
	}
	return workspace.Spec.Workload == nil && workspace.Spec.AttachmentEpoch == 0 && workspace.Status.AttachedEpoch == 0
}

func (r *TaskReconciler) startACPWorkspaceSpeculativeRuntime(
	ctx context.Context,
	task *corev1alpha1.Task,
	plan ACPRuntimePlan,
	allowCreate bool,
	requiredUID types.UID,
) {
	binding := plan.Workspace
	if binding == nil || binding.Class == nil || binding.Class.ControllerName == "" || task == nil {
		return
	}
	logger := logf.FromContext(ctx).V(1)
	key := types.NamespacedName{Namespace: task.Namespace, Name: acpClassWorkspaceName(task, binding)}
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, key, workspace); (err != nil && !apierrors.IsNotFound(err)) || (err == nil && workspace.Spec.Attachment != nil) {
		return
	}
	reader := uncachedReader(r.APIReader, r.Client)
	if err := reader.Get(ctx, key, workspace); err != nil {
		return
	}
	now := time.Now()
	if acpWorkspaceSpeculativeResumePending(workspace, now) {
		_, owner, _ := parseACPWorkspaceSpeculativeStart(workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation])
		if owner != string(task.UID) {
			return
		}
		if _, _, err := r.ensureACPRuntimePoolDemand(
			ctx, task.Namespace, plan, workspace.Name, string(workspace.UID), string(task.UID), allowCreate, requiredUID, false,
		); err != nil {
			logger.Info("speculative resume demand failed; flipping directly", "workspace", workspace.Name, "error", err.Error())
			if flipErr := r.flipACPWorkspaceForColdResume(ctx, workspace, task, binding.Class.EffectiveOnDetach); flipErr != nil {
				logger.Info("cold-resume flip deferred", "workspace", workspace.Name, "error", flipErr.Error())
			}
			return
		}
		acpPreDispatch.mark(task, acpPreDispatchSpeculativePoolActivated)
		return
	}
	if !acpWorkspaceSpeculativeStartEligible(workspace, task) {
		return
	}
	deadline, owner, recorded := parseACPWorkspaceSpeculativeStart(workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation])
	if recorded && owner == string(task.UID) && !now.Before(deadline) {
		return
	}
	_, hasIntent := workspace.Annotations[acpWorkspaceAttachmentIntentAnnotation]
	if !recorded || owner != string(task.UID) || (!hasIntent && workspace.Spec.Workload == nil) {
		if !r.recordACPWorkspaceSpeculativeStart(ctx, reader, workspace, task, binding, plan.PoolName, now) {
			return
		}
	}
	if _, _, err := r.ensureACPRuntimePoolDemand(
		ctx, task.Namespace, plan, workspace.Name, string(workspace.UID), string(task.UID), allowCreate, requiredUID, false,
	); err != nil {
		logger.Info("speculative workspace runtime demand deferred", "workspace", workspace.Name, "error", err.Error())
		return
	}
	acpPreDispatch.mark(task, acpPreDispatchSpeculativePoolActivated)
}

func (r *TaskReconciler) recordACPWorkspaceSpeculativeStart(
	ctx context.Context,
	reader client.Reader,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	binding *ACPRuntimeWorkspaceBinding,
	poolName string,
	now time.Time,
) bool {
	logger := logf.FromContext(ctx).V(1)
	if tainted, err := acpWorkspaceOperatorTainted(ctx, reader, workspace); err != nil || tainted {
		return false
	}
	intent := ""
	if _, staged := workspace.Annotations[acpWorkspaceAttachmentIntentAnnotation]; !staged && workspace.Spec.Workload == nil {
		intent = r.stageACPWorkspaceAttachmentIntent(ctx, workspace, task, binding, poolName)
	}
	recorded := false
	first := true
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := workspace
		if !first {
			current = &workspacev1alpha1.ExecutionWorkspace{}
			if err := reader.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
				return err
			}
			if current.UID != workspace.UID || !acpWorkspaceSpeculativeStartEligible(current, task) {
				return nil
			}
		}
		first = false
		base := current.DeepCopy()
		changed := false
		if _, owner, ok := parseACPWorkspaceSpeculativeStart(current.Annotations[acpWorkspaceSpeculativeStartAnnotation]); !ok || owner != string(task.UID) {
			current.Annotations[acpWorkspaceSpeculativeStartAnnotation] =
				formatACPWorkspaceSpeculativeStart(now.Add(acpWorkspaceSpeculativeStartTTL), task.UID)
			changed = true
		}
		if _, staged := current.Annotations[acpWorkspaceAttachmentIntentAnnotation]; intent != "" && !staged && current.Spec.Workload == nil {
			current.Annotations[acpWorkspaceAttachmentIntentAnnotation] = intent
			changed = true
		}
		if changed {
			if err := r.Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
			acpPreDispatch.mark(task, acpPreDispatchSpeculativeStart)
		}
		recorded = true
		return nil
	})
	if err != nil {
		logger.Info("speculative workspace start not recorded", "workspace", workspace.Name, "error", err.Error())
	}
	return recorded
}

func (r *TaskReconciler) acpWorkspaceAwaitsStagedInstall(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	poolName string,
	now time.Time,
) bool {
	if workspace.Spec.Attachment != nil || workspace.Spec.Workload != nil || poolName == "" {
		return false
	}
	intent, staged := parseACPWorkspaceAttachmentIntent(workspace)
	deadline, owner, marked := parseACPWorkspaceSpeculativeStart(workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation])
	if !staged || intent.TaskUID != task.UID || !marked || owner != string(task.UID) || !now.Before(deadline) {
		return false
	}
	pool := &corev1alpha1.RuntimePool{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: poolName}, pool); err != nil {
		return false
	}
	return pool.DeletionTimestamp.IsZero() && pool.Spec.DesiredReplicas > 0 &&
		pool.Annotations[acpExecutionWorkspaceUIDAnnotation] == string(workspace.UID) &&
		pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded
}

func acpWorkspaceRetainedDataAvailable(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	request, observation := workspace.Spec.Workload, workspace.Status.Allocation
	return request != nil && observation != nil && workspace.Status.ObservedGeneration == workspace.Generation &&
		observation.State == workspacev1alpha1.AllocationStopped && observation.Startup == nil &&
		observation.Key == request.Key && observation.Sequence == request.Sequence &&
		observation.Identity.RequestRevision == request.Revision && observation.Identity.Valid() &&
		observation.RetainedData != nil && observation.RetainedData.Valid() &&
		observation.RetainedData.SourceInstance == observation.Identity
}

func acpWorkspaceSpeculativeResumePending(workspace *workspacev1alpha1.ExecutionWorkspace, now time.Time) bool {
	if workspace == nil || !workspace.DeletionTimestamp.IsZero() ||
		workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended ||
		workspace.Spec.Attachment != nil ||
		workspace.Status.State != workspacev1alpha1.ExecutionWorkspaceStateSuspended ||
		workspace.Annotations[acpWorkspaceResumedLineageAnnotation] != booleanTrueValue ||
		strings.TrimSpace(workspace.Annotations[acpWorkspaceRevocationStartedAnnotation]) != "" ||
		acpWorkspaceQuarantined(workspace) || operatoraccess.Tainted(workspace.Annotations) ||
		!acpWorkspaceSuspendCapable(workspace) || !acpWorkspaceRetainedDataAvailable(workspace) {
		return false
	}
	deadline, owner, ok := parseACPWorkspaceSpeculativeStart(workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation])
	return ok && now.Before(deadline) &&
		acpWorkspaceResumeDemandBelongsToTask(workspace.Annotations[acpWorkspaceResumeRequestedAnnotation], owner)
}

func (r *TaskReconciler) requestACPWorkspaceSpeculativeResume(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	binding *ACPRuntimeWorkspaceBinding,
	poolName string,
	now time.Time,
) error {
	reader := uncachedReader(r.APIReader, r.Client)
	intent := r.stageACPWorkspaceAttachmentIntent(ctx, workspace, task, binding, poolName)
	if intent == "" {
		return nil
	}
	effectiveOnDetach := binding.Class.EffectiveOnDetach
	first := true
	claimed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := workspace
		if !first {
			current = &workspacev1alpha1.ExecutionWorkspace{}
			if err := reader.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
				return err
			}
			if current.UID != workspace.UID || current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended ||
				current.Status.State != workspacev1alpha1.ExecutionWorkspaceStateSuspended || current.Spec.Attachment != nil ||
				acpWorkspaceSpeculativeResumePending(current, now) {
				return nil
			}
		}
		first = false
		base := current.DeepCopy()
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations[acpWorkspaceResumeRequestedAnnotation] = now.UTC().Format(time.RFC3339Nano) + " " + task.Name + " " + string(task.UID)
		current.Annotations[acpWorkspaceDetachActionAnnotation] = effectiveOnDetach
		current.Annotations[acpWorkspaceResumedLineageAnnotation] = booleanTrueValue
		current.Annotations[acpWorkspaceSpeculativeStartAnnotation] = formatACPWorkspaceSpeculativeStart(now.Add(acpWorkspaceSpeculativeStartTTL), task.UID)
		current.Annotations[acpWorkspaceAttachmentIntentAnnotation] = intent
		if err := r.Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		claimed = true
		acpPreDispatch.mark(task, acpPreDispatchSpeculativeStart)
		return nil
	})
	if err != nil || !claimed {
		fresh := &workspacev1alpha1.ExecutionWorkspace{}
		readErr := reader.Get(ctx, client.ObjectKeyFromObject(workspace), fresh)
		if readErr == nil && fresh.UID == workspace.UID {
			if cleanupErr := r.releaseACPWorkspaceStagedAttachmentObjects(ctx, fresh, task); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
			}
		} else if readErr != nil && !apierrors.IsNotFound(readErr) {
			err = errors.Join(err, readErr)
		}
	}
	return err
}

func (r *TaskReconciler) flipACPWorkspaceForColdResume(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	effectiveOnDetach string,
) error {
	base := workspace.DeepCopy()
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredReady
	if workspace.Annotations == nil {
		workspace.Annotations = map[string]string{}
	}
	now := time.Now().UTC()
	workspace.Annotations[acpWorkspaceResumeRequestedAnnotation] =
		now.Format(time.RFC3339Nano) + " " + task.Name + " " + string(task.UID)
	workspace.Annotations[acpWorkspaceDetachActionAnnotation] = effectiveOnDetach
	workspace.Annotations[acpWorkspaceResumedLineageAnnotation] = booleanTrueValue
	if _, owner, ok := parseACPWorkspaceSpeculativeStart(workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation]); !ok || owner != string(task.UID) {
		delete(workspace.Annotations, acpWorkspaceSpeculativeStartAnnotation)
	}
	return r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

const acpWorkspaceAttachmentIntentAnnotation = "acp.workspace.orka.ai/attachment-intent"

type acpWorkspaceAttachmentIntent struct {
	TaskName    string    `json:"taskName"`
	TaskUID     types.UID `json:"taskUID"`
	Epoch       int64     `json:"epoch"`
	TokenSHA256 string    `json:"tokenSHA256"`
	ExpiresAt   time.Time `json:"expiresAt"`
	SecretName  string    `json:"secretName"`
}

func encodeACPWorkspaceAttachmentIntent(attachment *workspacev1alpha1.ExecutionWorkspaceAttachment) (string, error) {
	encoded, err := json.Marshal(acpWorkspaceAttachmentIntent{
		TaskName: attachment.TaskRef.Name, TaskUID: attachment.TaskRef.UID, Epoch: attachment.Epoch,
		TokenSHA256: attachment.TokenSHA256, ExpiresAt: attachment.ExpiresAt.UTC(), SecretName: attachment.TokenSecretRef.Name,
	})
	return string(encoded), err
}

func parseACPWorkspaceAttachmentIntent(workspace *workspacev1alpha1.ExecutionWorkspace) (*acpWorkspaceAttachmentIntent, bool) {
	raw, present := workspace.Annotations[acpWorkspaceAttachmentIntentAnnotation]
	if !present {
		return nil, false
	}
	intent := &acpWorkspaceAttachmentIntent{}
	if err := json.Unmarshal([]byte(raw), intent); err != nil || intent.TaskUID == "" || intent.TaskName == "" ||
		intent.Epoch <= 0 || intent.SecretName != attachmentSecretName(workspace.Name, intent.Epoch) ||
		!strings.HasPrefix(intent.TokenSHA256, "sha256:") || intent.ExpiresAt.IsZero() {
		return nil, false
	}
	return intent, true
}

func (i *acpWorkspaceAttachmentIntent) attachment() *workspacev1alpha1.ExecutionWorkspaceAttachment {
	attachment := &workspacev1alpha1.ExecutionWorkspaceAttachment{
		TaskRef: workspacev1alpha1.ObjectIdentityReference{Name: i.TaskName, UID: i.TaskUID},
		Epoch:   i.Epoch, TokenSHA256: i.TokenSHA256, ExpiresAt: metav1.NewTime(i.ExpiresAt),
	}
	attachment.TokenSecretRef.Name = i.SecretName
	return attachment
}

func validateACPWorkspaceAttachmentIntentTarget(
	ctx context.Context,
	reader client.Reader,
	binding *ACPRuntimeWorkspaceBinding,
	poolName string,
) func(*workspacev1alpha1.ExecutionWorkspace, *corev1alpha1.Task, time.Time) error {
	return func(workspace *workspacev1alpha1.ExecutionWorkspace, task *corev1alpha1.Task, _ time.Time) error {
		if err := verifyACPClassWorkspace(workspace, task, binding, poolName); err != nil {
			return err
		}
		if workspace.Spec.Attachment != nil || strings.TrimSpace(workspace.Annotations[acpWorkspaceRevocationStartedAnnotation]) != "" ||
			workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateFailed || acpWorkspaceQuarantined(workspace) ||
			!acpWorkspaceSuspendCapable(workspace) {
			return fmt.Errorf("workspace %s cannot stage a speculative attachment", workspace.Name)
		}
		fresh := workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredReady &&
			workspace.Spec.AttachmentEpoch == 0 && workspace.Status.AttachedEpoch == 0
		resume := workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended &&
			workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateSuspended && acpWorkspaceRetainedDataAvailable(workspace)
		if !fresh && !resume {
			return fmt.Errorf("workspace %s is neither fresh nor a settled suspension", workspace.Name)
		}
		if operatoraccess.Tainted(workspace.Annotations) {
			return fmt.Errorf("workspace %s: %w", workspace.Name, operatoraccess.ErrTainted)
		}
		return refuseOperatorTaintedACPWorkspaceHandoff(ctx, reader, workspace, task)
	}
}

func (r *TaskReconciler) stageACPWorkspaceAttachmentIntent(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	binding *ACPRuntimeWorkspaceBinding,
	poolName string,
) string {
	if binding == nil || workspace.UID == "" {
		return ""
	}
	reader := uncachedReader(r.APIReader, r.Client)
	manager := WorkspaceAttachmentManager{Client: r.Client, APIReader: r.APIReader, LeaseTTL: acpWorkspaceAttachmentTTL}
	attachment, err := manager.prepareAttachmentIntent(ctx, workspace, task, validateACPWorkspaceAttachmentIntentTarget(ctx, reader, binding, poolName))
	if err != nil {
		logf.FromContext(ctx).V(1).Info("speculative attachment not staged", "workspace", workspace.Name, "error", err.Error())
		return ""
	}
	encoded, err := encodeACPWorkspaceAttachmentIntent(attachment)
	if err != nil {
		return ""
	}
	return encoded
}

func installableACPWorkspaceAttachmentIntent(
	ctx context.Context,
	reader client.Reader,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	resume bool,
	now time.Time,
) (*workspacev1alpha1.ExecutionWorkspaceAttachment, error) {
	intent, ok := parseACPWorkspaceAttachmentIntent(workspace)
	if !ok || workspace.Spec.Attachment != nil || !intent.ExpiresAt.After(now) {
		return nil, nil
	}
	_, owner, marked := parseACPWorkspaceSpeculativeStart(workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation])
	if !marked || owner != string(intent.TaskUID) ||
		!acpWorkspaceResumeDemandBelongsToTask(workspace.Annotations[acpWorkspaceResumeRequestedAnnotation], owner) {
		return nil, nil
	}
	if next, err := nextWorkspaceAttachmentEpoch(workspace); err != nil || next != intent.Epoch {
		return nil, nil
	}
	if !resume && (workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady || !acpWorkspaceSpeculativeStartAllowed(workspace)) {
		return nil, nil
	}
	if !acpWorkspaceSuspendCapable(workspace) || operatoraccess.Tainted(workspace.Annotations) || acpWorkspaceQuarantined(workspace) {
		return nil, nil
	}
	tainted, err := acpWorkspaceOperatorTainted(ctx, reader, workspace)
	if err != nil || tainted {
		return nil, err
	}
	if staged, err := acpWorkspaceStagedAttachmentObjectsHeld(ctx, reader, workspace, intent, now); err != nil || !staged {
		return nil, err
	}
	return intent.attachment(), nil
}

func acpWorkspaceStagedAttachmentObjectsHeld(
	ctx context.Context,
	reader client.Reader,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	intent *acpWorkspaceAttachmentIntent,
	now time.Time,
) (bool, error) {
	epoch := strconv.FormatInt(intent.Epoch, 10)
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: intent.SecretName}, secret); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	secretOwner := metav1.GetControllerOf(secret)
	digest := sha256.Sum256(secret.Data[workspaceAttachmentTokenKey])
	if secretOwner == nil || secretOwner.UID != workspace.UID || secret.Labels[workspaceAttachmentLabel] != string(workspace.UID) ||
		string(secret.Data["workspaceUID"]) != string(workspace.UID) || string(secret.Data[epochField]) != epoch ||
		"sha256:"+hex.EncodeToString(digest[:]) != intent.TokenSHA256 {
		return false, nil
	}
	lease := &coordinationv1.Lease{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: attachmentLeaseName(workspace.Name)}, lease); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	leaseOwner := metav1.GetControllerOf(lease)
	return leaseOwner != nil && leaseOwner.UID == workspace.UID && lease.Spec.HolderIdentity != nil &&
		*lease.Spec.HolderIdentity == string(intent.TaskUID) && !leaseExpired(lease, now) &&
		lease.Annotations[workspaceAttachmentLeaseEpochAnnotation] == epoch, nil
}

func (r *TaskReconciler) releaseACPWorkspaceSpeculativeClaims(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
) (bool, error) {
	if workspace.Spec.Attachment != nil && workspace.Spec.Attachment.TaskRef.UID == task.UID {
		return false, nil
	}
	intent, staged := parseACPWorkspaceAttachmentIntent(workspace)
	ownIntent := staged && intent.TaskUID == task.UID
	_, owner, marked := parseACPWorkspaceSpeculativeStart(workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation])
	pendingResume := workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended && marked && owner == string(task.UID)
	_, malformedIntent := workspace.Annotations[acpWorkspaceAttachmentIntentAnnotation]
	ownMalformed := malformedIntent && !staged && pendingResume
	if ownIntent || pendingResume || ownMalformed {
		base := workspace.DeepCopy()
		if ownIntent || ownMalformed {
			delete(workspace.Annotations, acpWorkspaceAttachmentIntentAnnotation)
		}
		if pendingResume {
			delete(workspace.Annotations, acpWorkspaceSpeculativeStartAnnotation)
		}
		if err := r.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				return true, nil
			}
			return true, err
		}
		return true, r.releaseACPWorkspaceStagedAttachmentObjects(ctx, workspace, task)
	}
	return false, r.releaseACPWorkspaceStagedAttachmentObjects(ctx, workspace, task)
}

func (r *TaskReconciler) releaseACPWorkspaceStagedAttachmentObjects(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
) error {
	if intent, staged := parseACPWorkspaceAttachmentIntent(workspace); staged && intent.TaskUID == task.UID {
		return nil
	}
	next, err := nextWorkspaceAttachmentEpoch(workspace)
	if err != nil {
		return nil
	}
	reader := uncachedReader(r.APIReader, r.Client)
	lease := &coordinationv1.Lease{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: attachmentLeaseName(workspace.Name)}, lease); err != nil {
		return client.IgnoreNotFound(err)
	}
	epoch := strconv.FormatInt(next, 10)
	leaseOwner := metav1.GetControllerOf(lease)
	if leaseOwner == nil || leaseOwner.UID != workspace.UID || lease.Spec.HolderIdentity == nil ||
		*lease.Spec.HolderIdentity != string(task.UID) || lease.Annotations[workspaceAttachmentLeaseEpochAnnotation] != epoch {
		return nil
	}
	now := metav1.NewMicroTime(time.Now().UTC())
	leaseBase := lease.DeepCopy()
	lease.Spec.RenewTime = &now
	if err := r.Patch(ctx, lease, client.MergeFromWithOptions(leaseBase, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			return nil
		}
		return err
	}
	freshWS := &workspacev1alpha1.ExecutionWorkspace{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(workspace), freshWS); err != nil {
		return client.IgnoreNotFound(err)
	}
	if freshWS.UID != workspace.UID || freshWS.Spec.AttachmentEpoch >= next {
		return nil
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: attachmentSecretName(workspace.Name, next)}, secret); err == nil {
		secretOwner := metav1.GetControllerOf(secret)
		if secretOwner != nil && secretOwner.UID == workspace.UID {
			if err := r.Delete(ctx, secret, deleteCurrentObjectPreconditions(secret)...); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	if err := r.Delete(ctx, lease, deleteCurrentObjectPreconditions(lease)...); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
