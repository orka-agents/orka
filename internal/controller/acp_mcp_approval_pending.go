package controller

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

func mcpApprovalPendingFromOlderEpoch(effect *store.ExternalEffect, fence store.ControllerEpochFence) bool {
	return effect.Identity.Kind == acpMCPToolEffectKind && effect.State == store.ExternalEffectPending &&
		effect.ControllerEpochName == fence.Name && effect.ControllerEpoch > 0 && effect.ControllerEpoch < fence.Epoch
}

// acpMCPAbandonedPendingReceipt binds a pre-execution denial to its reservation
// when a crash prevented the approval request event from being saved. Recovery
// must first prove an older epoch or a terminal/deleting Task with the same run.
func acpMCPAbandonedPendingReceipt(effect *store.ExternalEffect) json.RawMessage {
	result, _ := harnessv2.CanonicalValue(struct {
		IsError          bool   `json:"isError"`
		Code             string `json:"code"`
		Error            string `json:"error"`
		ExternalEffectID string `json:"externalEffectID"`
		RequestDigest    string `json:"requestDigest"`
	}{
		IsError: true, Code: acpApprovalCodeStale, Error: acpApprovalStaleMessage,
		ExternalEffectID: effect.ID, RequestDigest: effect.RequestDigest,
	})
	return result
}

func (d *ACPDispatcher) failMCPApprovalPending(ctx context.Context, fence store.ControllerEpochFence, effect *store.ExternalEffect, result json.RawMessage) (*store.ExternalEffect, error) {
	id, err := effect.Identity.CanonicalID()
	if err != nil || effect.ID != id || effect.Identity.Kind != acpMCPToolEffectKind || effect.State != store.ExternalEffectPending {
		return nil, store.ErrConflict
	}
	// The Pending/version/digest CAS is the proof that execution never began.
	// A concurrent claim wins rather than being overwritten as an unstarted call.
	return d.Store.TransitionExternalEffect(ctx, store.ExternalEffectTransition{
		ID: effect.ID, Fence: fence, ExpectedVersion: effect.Version,
		ExpectedState: store.ExternalEffectPending, NewState: store.ExternalEffectFailed,
		RequestDigest: effect.RequestDigest, Response: result, ResponseDigest: store.CanonicalBytesDigest(result),
		UpdatedAt: time.Now().UTC(),
	})
}

func (d *ACPDispatcher) mcpApprovalUnboundPendingAbandoned(ctx context.Context, fence store.ControllerEpochFence, task *corev1alpha1.Task, effect *store.ExternalEffect) (bool, error) {
	if effect.Identity.Kind != acpMCPToolEffectKind || effect.State != store.ExternalEffectPending ||
		effect.ControllerEpochName != fence.Name || effect.ControllerEpoch <= 0 || effect.ControllerEpoch > fence.Epoch {
		return false, nil
	}
	if mcpApprovalPendingFromOlderEpoch(effect, fence) {
		return true, nil
	}
	current, err := d.mcpApprovalPendingTask(ctx, task)
	if err != nil || current == nil {
		return false, err
	}
	// The discovery hint alone cannot revoke a call. A fresh Task read must
	// bind this reservation's runtime session and epoch to a run that the broker
	// also refuses. This covers a lost request event followed by Task cleanup
	// without requiring a controller restart or executable Secret access.
	execution := current.Status.Execution
	if execution == nil || execution.RuntimeSessionUID == "" || execution.RuntimeSessionUID != effect.Identity.AggregateID ||
		execution.ControllerEpoch != effect.ControllerEpoch {
		return false, nil
	}
	return !current.DeletionTimestamp.IsZero() || current.Status.Phase == corev1alpha1.TaskPhaseCancelled ||
		current.Status.Phase == corev1alpha1.TaskPhaseSucceeded || current.Status.Phase == corev1alpha1.TaskPhaseFailed, nil
}

func (d *ACPDispatcher) acpMCPApprovalPendingDenial(
	ctx context.Context,
	fence store.ControllerEpochFence,
	task *corev1alpha1.Task,
	approval approvals.Approval,
	effect *store.ExternalEffect,
	now time.Time,
) (string, error) {
	binding := approval.Binding
	if effect.State != store.ExternalEffectPending || effect.ControllerEpochName != fence.Name ||
		effect.ControllerEpoch <= 0 || effect.ControllerEpoch > fence.Epoch || binding == nil ||
		binding.ControllerEpoch != uint64(effect.ControllerEpoch) {
		return "", nil
	}
	if mcpApprovalPendingFromOlderEpoch(effect, fence) {
		return acpApprovalCodeStale, nil
	}
	switch approval.Status {
	case approvals.StatusDeclined:
		return acpApprovalCodeDeclined, nil
	case approvals.StatusCancelled:
		return acpApprovalCodeCancelled, nil
	case approvals.StatusExpired:
		return acpApprovalCodeExpired, nil
	}
	if approval.ExpiresAt != nil && !now.Before(*approval.ExpiresAt) {
		return acpApprovalCodeExpired, nil
	}

	current, err := d.mcpApprovalPendingTask(ctx, task)
	if err != nil || current == nil {
		return "", err
	}
	if !current.DeletionTimestamp.IsZero() || current.Status.Phase == corev1alpha1.TaskPhaseCancelled {
		return acpApprovalCodeCancelled, nil
	}
	key := store.PromptAttemptKey{
		Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: int64(binding.TaskAttempt), PromptID: binding.PromptID,
	}
	id, err := key.CanonicalID()
	if err != nil {
		return "", err
	}
	attempt, err := d.Store.GetPromptAttempt(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	if attempt.ID != id || attempt.Key != key || attempt.SessionUID != binding.RuntimeSessionUID ||
		attempt.RuntimeInstanceID != binding.RuntimeInstanceID || attempt.ControllerEpochName != fence.Name ||
		attempt.ControllerEpoch != effect.ControllerEpoch {
		return "", nil
	}
	if store.IsTerminalPromptExecutionState(attempt.ExecutionState) {
		return acpApprovalCodeStale, nil
	}
	// A transport disconnect, missing local lease, or temporary read failure
	// does not by itself revoke a same-epoch review that can still reconnect.
	return "", nil
}

func (d *ACPDispatcher) mcpApprovalPendingTask(ctx context.Context, task *corev1alpha1.Task) (*corev1alpha1.Task, error) {
	reader := d.APIReader
	if reader == nil {
		reader = d.Client
	}
	current := &corev1alpha1.Task{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if current.UID != task.UID {
		return nil, nil
	}
	return current, nil
}
