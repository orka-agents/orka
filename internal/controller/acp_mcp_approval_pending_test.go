package controller

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	storekube "github.com/orka-agents/orka/internal/store/kube"
)

func seedMCPApprovalPending(t *testing.T, f *mcpApprovalRecoveryFixture, requested bool) (*acpMCPApprovalCall, *store.ExternalEffect) {
	t.Helper()
	descriptor, err := f.request.ValidateAt(time.Now().UTC())
	require.NoError(t, err)
	credentials, err := f.broker.Credentials.ResolveACPMCPBrokerCredentials(f.ctx, f.request)
	require.NoError(t, err)
	call, _, err := f.broker.persistApprovalCall(f.ctx, f.request, descriptor, credentials.Task)
	require.NoError(t, err)
	effect, err := f.control.ReserveExternalEffect(f.ctx, store.ReserveExternalEffectRequest{
		Identity: store.ExternalEffectIdentity{
			Kind: acpMCPToolEffectKind, Namespace: f.request.Namespace,
			AggregateID: string(f.request.Authorization.RuntimeSessionUID), OperationID: string(f.request.Metadata.OperationID),
		},
		RequestDigest: call.RequestDigest, Fence: f.fence, CreatedAt: call.CreatedAt, ApprovalTaskUID: call.Task.UID,
	})
	require.NoError(t, err)
	if requested {
		require.NoError(t, f.broker.requestToolApproval(f.ctx, call))
	}
	return call, effect
}

func requireMCPApprovalPendingDenied(t *testing.T, f *mcpApprovalRecoveryFixture, call *acpMCPApprovalCall, original *store.ExternalEffect, code string) *store.ExternalEffect {
	t.Helper()
	persisted, err := f.control.GetExternalEffectByIdentity(f.ctx, original.Identity)
	require.NoError(t, err)
	require.Equal(t, store.ExternalEffectFailed, persisted.State)
	require.Equal(t, original.Version+1, persisted.Version)
	require.Equal(t, original.RequestDigest, persisted.RequestDigest)
	require.JSONEq(t, string(acpApprovalError(call.ID, code)), string(persisted.Response))
	outcome, reason, _, err := acpMCPApprovalReceiptOutcome(persisted, call.ID)
	require.NoError(t, err)
	require.Equal(t, "not_started", outcome)
	require.Equal(t, code, reason)
	approval, _ := f.approval(t)
	require.Equal(t, call.ID, approval.ID)
	require.Equal(t, approvalCallBinding(call), approval.Binding)
	require.Equal(t, "not_started", approval.ExecutionOutcome)
	require.Equal(t, code, approval.ExecutionReason)
	require.Zero(t, f.count.Load())
	require.Zero(t, f.secretReads.Load(), "recovery must not read executable approval Secrets")
	return persisted
}

func TestMCPApprovalPendingRecoveryAfterRestartClearsTaskEffectBarrier(t *testing.T) {
	f := newMCPApprovalRecoveryFixture(t)
	call, effect := seedMCPApprovalPending(t, f, true)
	cleanupTask := f.task.DeepCopy()
	cleanupTask.Status.Execution = &corev1alpha1.TaskExecutionStatus{RuntimeSessionUID: effect.Identity.AggregateID}
	reconciler := &TaskReconciler{Client: f.kube}
	unsettled, err := reconciler.acpTaskHasUnsettledExternalEffects(f.ctx, cleanupTask, "")
	require.NoError(t, err)
	require.True(t, unsettled)

	f.restart(t)
	require.NoError(t, f.reconcile(t))
	persisted := requireMCPApprovalPendingDenied(t, f, call, effect, acpApprovalCodeStale)
	unsettled, err = reconciler.acpTaskHasUnsettledExternalEffects(f.ctx, cleanupTask, "")
	require.NoError(t, err)
	require.False(t, unsettled, "the stale approval must no longer block Task cleanup")

	_, before := f.approval(t)
	require.NoError(t, f.reconcile(t))
	_, after := f.approval(t)
	require.Equal(t, before, after)
	current, err := f.control.GetExternalEffectByIdentity(f.ctx, effect.Identity)
	require.NoError(t, err)
	require.Equal(t, persisted, current)
}

func TestMCPApprovalPendingRecoveryBeforeRequestedEventRepairsLateProjection(t *testing.T) {
	f := newMCPApprovalRecoveryFixture(t)
	call, effect := seedMCPApprovalPending(t, f, false)
	f.restart(t)
	require.NoError(t, f.reconcile(t))
	persisted, err := f.control.GetExternalEffectByIdentity(f.ctx, effect.Identity)
	require.NoError(t, err)
	require.Equal(t, store.ExternalEffectFailed, persisted.State)
	require.Equal(t, effect.Version+1, persisted.Version)
	var receipt map[string]any
	require.NoError(t, json.Unmarshal(persisted.Response, &receipt))
	require.Equal(t, true, receipt["isError"])
	require.Equal(t, acpApprovalCodeStale, receipt["code"])
	require.Equal(t, "The original task or tool authority is no longer valid.", receipt["error"])
	require.Equal(t, effect.ID, receipt["externalEffectID"])
	require.Equal(t, effect.RequestDigest, receipt["requestDigest"])
	require.NotContains(t, receipt, "approvalID", "the absent approval identity must not be invented")
	outcome, reason, _, err := acpMCPApprovalReceiptOutcome(persisted, call.ID)
	require.NoError(t, err)
	require.Equal(t, "not_started", outcome)
	require.Equal(t, acpApprovalCodeStale, reason)
	listed, err := approvals.ListEvents(f.ctx, f.events, f.task.Namespace, f.task.Name)
	require.NoError(t, err)
	require.Empty(t, listed, "recovery must not fabricate a requested review")

	// The old writer can append after recovery has already sealed the effect.
	require.NoError(t, f.broker.requestToolApproval(f.ctx, call))
	require.NoError(t, f.reconcile(t))
	approval, before := f.approval(t)
	require.Equal(t, "not_started", approval.ExecutionOutcome)
	require.Equal(t, acpApprovalCodeStale, approval.ExecutionReason)
	require.Equal(t, approvalCallBinding(call), approval.Binding)
	require.NoError(t, f.reconcile(t))
	_, after := f.approval(t)
	require.Equal(t, before, after)
	current, err := f.control.GetExternalEffectByIdentity(f.ctx, effect.Identity)
	require.NoError(t, err)
	require.Equal(t, persisted, current, "a delayed review must retain the original terminal receipt")
	require.Zero(t, f.count.Load())
	require.Zero(t, f.secretReads.Load())
}

func TestMCPApprovalPendingRecoveryPreservesLiveCurrentEpoch(t *testing.T) {
	for _, requested := range []bool{false, true} {
		name := "before_requested_event"
		if requested {
			name = "with_requested_event"
		}
		t.Run(name, func(t *testing.T) {
			f := newMCPApprovalRecoveryFixture(t)
			_, effect := seedMCPApprovalPending(t, f, requested)
			authorizer := f.broker.Prompts.(DurableACPMCPPromptAuthorizer)
			registerApprovalLease(t, authorizer.PromptLeases, f.ctx, f.request)
			require.NoError(t, authorizer.AuthorizeACPMCPPrompt(f.ctx, f.request))
			before, err := approvals.ListEvents(f.ctx, f.events, f.task.Namespace, f.task.Name)
			require.NoError(t, err)
			for range 2 {
				require.NoError(t, f.reconcile(t))
			}
			current, err := f.control.GetExternalEffectByIdentity(f.ctx, effect.Identity)
			require.NoError(t, err)
			require.Equal(t, effect, current)
			after, err := approvals.ListEvents(f.ctx, f.events, f.task.Namespace, f.task.Name)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.NoError(t, authorizer.AuthorizeACPMCPPrompt(f.ctx, f.request))
			require.Zero(t, f.count.Load())
			require.Zero(t, f.secretReads.Load())
		})
	}
}

func TestMCPApprovalPendingRecoveryPreservesFutureAndForeignEpochs(t *testing.T) {
	for _, name := range []string{"future_epoch", "foreign_epoch"} {
		t.Run(name, func(t *testing.T) {
			f := newMCPApprovalRecoveryFixture(t)
			_, effect := seedMCPApprovalPending(t, f, true)
			_, before := f.approval(t)
			var listed corev1alpha1.ExternalEffectList
			require.NoError(t, f.kube.List(f.ctx, &listed))
			require.Len(t, listed.Items, 1)
			object := &listed.Items[0]
			if name == "future_epoch" {
				object.Status.ControllerEpoch = f.fence.Epoch + 1
			} else {
				object.Status.ControllerEpochName = f.fence.Name + "-other"
			}
			require.NoError(t, f.kube.Status().Update(f.ctx, object))
			original, err := f.control.GetExternalEffectByIdentity(f.ctx, effect.Identity)
			require.NoError(t, err)
			require.NoError(t, f.reconcile(t))
			current, err := f.control.GetExternalEffectByIdentity(f.ctx, effect.Identity)
			require.NoError(t, err)
			require.Equal(t, original, current, "this controller cannot settle a different controller fence's Pending effect")
			_, after := f.approval(t)
			require.Equal(t, before, after)
			require.Zero(t, f.count.Load())
			require.Zero(t, f.secretReads.Load())
		})
	}
}

func TestMCPApprovalPendingRecoveryWithoutEventRechecksTerminalTask(t *testing.T) {
	for _, name := range []string{"cancelled", "succeeded", "failed", "deleting", "running", "wrong_session", "wrong_epoch"} {
		t.Run(name, func(t *testing.T) {
			f := newMCPApprovalRecoveryFixture(t)
			call, effect := seedMCPApprovalPending(t, f, false)
			// Prime a live scan before only the Task's state changes. There is
			// no approval event or effect mutation to invalidate a history cache.
			require.NoError(t, f.reconcile(t))
			currentTask := &corev1alpha1.Task{}
			require.NoError(t, f.kube.Get(f.ctx, client.ObjectKeyFromObject(f.task), currentTask))
			currentTask.Status.Execution = &corev1alpha1.TaskExecutionStatus{
				RuntimeSessionUID: effect.Identity.AggregateID, ControllerEpoch: effect.ControllerEpoch,
			}
			currentTask.Status.Phase = corev1alpha1.TaskPhaseCancelled
			denied := true
			switch name {
			case "succeeded":
				currentTask.Status.Phase = corev1alpha1.TaskPhaseSucceeded
			case "failed":
				currentTask.Status.Phase = corev1alpha1.TaskPhaseFailed
			case "deleting":
				currentTask.Status.Phase = corev1alpha1.TaskPhaseRunning
				currentTask.Finalizers = []string{"test.orka.ai/pending-effect"}
			case "running":
				currentTask.Status.Phase = corev1alpha1.TaskPhaseRunning
				denied = false
			case "wrong_session":
				currentTask.Status.Execution.RuntimeSessionUID = "another-runtime-session"
				denied = false
			case "wrong_epoch":
				currentTask.Status.Execution.ControllerEpoch++
				denied = false
			}
			require.NoError(t, f.kube.Update(f.ctx, currentTask))
			if name == "deleting" {
				require.NoError(t, f.kube.Delete(f.ctx, currentTask))
			}
			require.NoError(t, f.reconcile(t))
			persisted, err := f.control.GetExternalEffectByIdentity(f.ctx, effect.Identity)
			require.NoError(t, err)
			listed, err := approvals.ListEvents(f.ctx, f.events, f.task.Namespace, f.task.Name)
			require.NoError(t, err)
			require.Empty(t, listed, "a receipt cannot invent the missing review")
			if denied {
				require.Equal(t, store.ExternalEffectFailed, persisted.State)
				require.Equal(t, effect.Version+1, persisted.Version)
				require.JSONEq(t, string(acpMCPAbandonedPendingReceipt(effect)), string(persisted.Response))
				outcome, reason, _, err := acpMCPApprovalReceiptOutcome(persisted, call.ID)
				require.NoError(t, err)
				require.Equal(t, "not_started", outcome)
				require.Equal(t, acpApprovalCodeStale, reason)
				unsettled, err := (&TaskReconciler{Client: f.kube}).acpTaskHasUnsettledExternalEffects(f.ctx, currentTask, "")
				require.NoError(t, err)
				require.False(t, unsettled)
			} else {
				require.Equal(t, effect, persisted)
			}
			require.Zero(t, f.count.Load())
			require.Zero(t, f.secretReads.Load())
		})
	}
}

func TestMCPApprovalPendingRecoveryRechecksExpiryWithoutNewEvents(t *testing.T) {
	f := newMCPApprovalRecoveryFixture(t)
	f.broker.ApprovalWaitTimeout = 2 * time.Second
	call, effect := seedMCPApprovalPending(t, f, true)
	for range 2 {
		require.NoError(t, f.reconcile(t))
	}
	current, err := f.control.GetExternalEffectByIdentity(f.ctx, effect.Identity)
	require.NoError(t, err)
	require.Equal(t, effect, current)
	require.True(t, time.Now().UTC().Before(call.ExpiresAt), "prime recovery while the original review is still live")

	// Only the immutable deadline passes; effect versions and event sequence
	// stay unchanged, so a cached live scan must not suppress this settlement.
	time.Sleep(time.Until(call.ExpiresAt))
	require.NoError(t, f.reconcile(t))
	requireMCPApprovalPendingDenied(t, f, call, effect, acpApprovalCodeExpired)
}

func TestMCPApprovalPendingRecoveryRechecksExactTerminalPrompt(t *testing.T) {
	f := newMCPApprovalRecoveryFixture(t)
	call, effect := seedMCPApprovalPending(t, f, true)
	otherKey := mcpPromptLeaseKey(f.request.Namespace, f.request.Metadata)
	otherKey.PromptID += "-other"
	other, err := f.control.CreatePromptAttempt(f.ctx, &store.PromptAttempt{
		Key: otherKey, SessionUID: effect.Identity.AggregateID,
		RequestDigest: testControllerMCPDigest("other prompt"), BindingDigest: testControllerMCPDigest("binding"), SnapshotDigest: testControllerMCPDigest("snapshot"),
	}, f.fence)
	require.NoError(t, err)
	_, err = f.control.TransitionPromptAttemptExecution(f.ctx, store.PromptAttemptExecutionTransition{
		ID: other.ID, Fence: f.fence, ExpectedVersion: other.Version, ExpectedState: other.ExecutionState,
		NewState: store.PromptExecutionFailed, OperationID: "fail-other-prompt", OperationDigest: testControllerMCPDigest("fail other prompt"),
	})
	require.NoError(t, err)
	for range 2 {
		require.NoError(t, f.reconcile(t))
	}
	current, err := f.control.GetExternalEffectByIdentity(f.ctx, effect.Identity)
	require.NoError(t, err)
	require.Equal(t, effect, current, "a different terminal prompt must not end the live review")
	seq, err := f.events.GetLatestExecutionEventSeq(f.ctx, f.task.Namespace, events.ExecutionEventStreamTypeTask, f.task.Name)
	require.NoError(t, err)

	id, err := mcpPromptLeaseKey(f.request.Namespace, f.request.Metadata).CanonicalID()
	require.NoError(t, err)
	attempt, err := f.control.GetPromptAttempt(f.ctx, id)
	require.NoError(t, err)
	for _, state := range []store.PromptExecutionState{store.PromptExecutionSettling, store.PromptExecutionSucceeded} {
		attempt, err = f.control.TransitionPromptAttemptExecution(f.ctx, store.PromptAttemptExecutionTransition{
			ID: attempt.ID, Fence: f.fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState,
			NewState: state, OperationID: "pending-recovery-" + string(state), OperationDigest: testControllerMCPDigest(string(state)),
		})
		require.NoError(t, err)
	}
	currentSeq, err := f.events.GetLatestExecutionEventSeq(f.ctx, f.task.Namespace, events.ExecutionEventStreamTypeTask, f.task.Name)
	require.NoError(t, err)
	require.Equal(t, seq, currentSeq, "only the exact durable prompt changed between recovery scans")
	require.NoError(t, f.reconcile(t))
	requireMCPApprovalPendingDenied(t, f, call, effect, acpApprovalCodeStale)
}

type mcpApprovalPendingPromptReadStore struct {
	*storekube.Store
	read func(context.Context, string) (*store.PromptAttempt, error)
}

func (s *mcpApprovalPendingPromptReadStore) GetPromptAttempt(ctx context.Context, id string) (*store.PromptAttempt, error) {
	return s.read(ctx, id)
}

func TestMCPApprovalPendingRecoveryPreservesReviewWithoutExactPromptEvidence(t *testing.T) {
	for _, name := range []string{"mismatched_prompt_binding", "transient_prompt_read"} {
		t.Run(name, func(t *testing.T) {
			f := newMCPApprovalRecoveryFixture(t)
			_, effect := seedMCPApprovalPending(t, f, true)
			_, before := f.approval(t)
			readErr := errors.New("injected prompt read outage")
			var reads atomic.Int32
			f.dispatcher.Store = &mcpApprovalPendingPromptReadStore{Store: f.control, read: func(ctx context.Context, id string) (*store.PromptAttempt, error) {
				reads.Add(1)
				if name == "transient_prompt_read" {
					return nil, readErr
				}
				attempt, err := f.control.GetPromptAttempt(ctx, id)
				if err != nil {
					return nil, err
				}
				attempt.ExecutionState = store.PromptExecutionSucceeded
				attempt.SessionUID += "-different"
				return attempt, nil
			}}
			err := f.reconcile(t)
			if name == "transient_prompt_read" {
				require.ErrorIs(t, err, readErr)
			} else {
				require.NoError(t, err)
			}
			require.Positive(t, reads.Load())
			current, err := f.control.GetExternalEffectByIdentity(f.ctx, effect.Identity)
			require.NoError(t, err)
			require.Equal(t, effect, current)
			_, after := f.approval(t)
			require.Equal(t, before, after)
			f.dispatcher.Store = f.control
			require.NoError(t, f.reconcile(t))
			current, err = f.control.GetExternalEffectByIdentity(f.ctx, effect.Identity)
			require.NoError(t, err)
			require.Equal(t, effect, current)
			require.Zero(t, f.count.Load())
			require.Zero(t, f.secretReads.Load())
		})
	}
}

type mcpApprovalPendingClaimStore struct {
	*storekube.Store
	beforeFailure func(context.Context, store.ExternalEffectTransition) error
}

func (s *mcpApprovalPendingClaimStore) TransitionExternalEffect(ctx context.Context, transition store.ExternalEffectTransition) (*store.ExternalEffect, error) {
	if transition.ExpectedState == store.ExternalEffectPending && transition.NewState == store.ExternalEffectFailed {
		if err := s.beforeFailure(ctx, transition); err != nil {
			return nil, err
		}
	}
	return s.Store.TransitionExternalEffect(ctx, transition)
}

func TestMCPApprovalPendingRecoveryDoesNotOverwriteConcurrentExecutionClaim(t *testing.T) {
	f := newMCPApprovalRecoveryFixture(t)
	call, effect := seedMCPApprovalPending(t, f, true)
	f.decide(call.ID, events.ExecutionEventTypeApprovalApproved)
	beforeApproval, beforeEvents := f.approval(t)
	f.restart(t)
	var injected atomic.Bool
	var claimed *store.ExternalEffect
	f.dispatcher.Store = &mcpApprovalPendingClaimStore{Store: f.control, beforeFailure: func(ctx context.Context, transition store.ExternalEffectTransition) error {
		if !injected.CompareAndSwap(false, true) {
			return nil
		}
		expires := time.Now().UTC().Add(5 * time.Minute)
		var err error
		claimed, err = f.control.TransitionExternalEffect(ctx, store.ExternalEffectTransition{
			ID: effect.ID, Fence: transition.Fence, ExpectedVersion: effect.Version,
			ExpectedState: store.ExternalEffectPending, NewState: store.ExternalEffectInFlight,
			RequestDigest: effect.RequestDigest, LeaseOwner: "concurrent-execution", LeaseExpiresAt: &expires,
		})
		return err
	}}
	for range 2 {
		require.NoError(t, f.reconcile(t))
	}
	require.True(t, injected.Load(), "the execution claim must win immediately before the recovery CAS")
	require.NotNil(t, claimed)
	current, err := f.control.GetExternalEffectByIdentity(f.ctx, effect.Identity)
	require.NoError(t, err)
	require.Equal(t, claimed, current)
	require.Equal(t, store.ExternalEffectInFlight, current.State)
	approval, afterEvents := f.approval(t)
	require.Equal(t, beforeApproval, approval)
	require.Equal(t, beforeEvents, afterEvents, "recovery cannot append a denial after the execution claim wins")
	require.Zero(t, f.count.Load())
	require.Zero(t, f.secretReads.Load())
}

func TestMCPApprovalAbandonedPendingReceiptValidatesExactEffectBinding(t *testing.T) {
	identity := store.ExternalEffectIdentity{
		Kind: acpMCPToolEffectKind, Namespace: "default", AggregateID: "runtime-session", OperationID: "tool-operation",
	}
	id, err := identity.CanonicalID()
	require.NoError(t, err)
	base := store.ExternalEffect{ID: id, Identity: identity, RequestDigest: testControllerMCPDigest("original request"), State: store.ExternalEffectFailed}
	base.Response = acpMCPAbandonedPendingReceipt(&base)
	base.ResponseDigest = store.CanonicalBytesDigest(base.Response)
	outcome, reason, _, err := acpMCPApprovalReceiptOutcome(&base, "late-requested-approval")
	require.NoError(t, err)
	require.Equal(t, "not_started", outcome)
	require.Equal(t, acpApprovalCodeStale, reason)

	for _, name := range []string{
		"wrong_effect_id", "wrong_request_digest", "wrong_nonempty_approval_id", "malformed_identity", "other_effect_kind",
		"mixed_matching_approval_id", "wrong_stale_message",
		acpApprovalCodeDeclined, acpApprovalCodeExpired, acpApprovalCodeCancelled, acpApprovalCodeUnknown, "tool_execution_failed",
	} {
		t.Run(name, func(t *testing.T) {
			effect := base
			var receipt map[string]any
			require.NoError(t, json.Unmarshal(base.Response, &receipt))
			switch name {
			case "wrong_effect_id":
				receipt["externalEffectID"] = store.CanonicalControlID("external-effect", "different effect")
			case "wrong_request_digest":
				receipt["requestDigest"] = testControllerMCPDigest("different request")
			case "wrong_nonempty_approval_id":
				receipt["approvalID"] = "another-approval"
			case "mixed_matching_approval_id":
				receipt["approvalID"] = "late-requested-approval"
			case "wrong_stale_message":
				receipt["error"] = "Unverified tool output"
			case "malformed_identity":
				effect.Identity.OperationID = ""
			case "other_effect_kind":
				effect.Identity.Kind = "workspace.publish"
				effect.ID, err = effect.Identity.CanonicalID()
				require.NoError(t, err)
				receipt["externalEffectID"] = effect.ID
			default:
				receipt["code"] = name
			}
			// Keep the response digest valid so rejection exercises the
			// receipt's identity and denial checks, not merely its checksum.
			effect.Response, err = harnessv2.CanonicalValue(receipt)
			require.NoError(t, err)
			effect.ResponseDigest = store.CanonicalBytesDigest(effect.Response)
			_, _, result, err := acpMCPApprovalReceiptOutcome(&effect, "late-requested-approval")
			require.Error(t, err)
			require.Empty(t, result)
		})
	}
}
