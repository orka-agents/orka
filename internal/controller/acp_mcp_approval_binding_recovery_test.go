package controller

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

func TestMCPApprovalBindingRecoveryIgnoresPatchedDiscoveryHint(t *testing.T) {
	for _, test := range []struct {
		name    string
		state   store.ExternalEffectState
		prior   string
		outcome string
	}{
		{"pending", store.ExternalEffectPending, "", "not_started"},
		{"completed", store.ExternalEffectSucceeded, "running", "succeeded"},
		{"denied", store.ExternalEffectFailed, "", "not_started"},
		{"unknown", store.ExternalEffectOutcomeUnknown, "running", "unknown"},
	} {
		for _, hint := range []string{"", "another-task-uid"} {
			t.Run(test.name+"/hint="+hint, func(t *testing.T) {
				f := newMCPApprovalRecoveryFixture(t)
				other := createMCPApprovalRecoveryOtherTask(t, f)
				executed := test.prior == "running"
				call, effect := f.seed(t, test.state, test.prior, executed, json.RawMessage(`{"workOrder":"simulated-1"}`))
				before, beforeEvents := f.approval(t)
				object := patchMCPApprovalRecoveryHint(t, f, effect.ID, hint)
				require.Equal(t, string(f.task.UID), object.Spec.ApprovalTaskUID)
				f.restart(t)
				count := f.count.Load()

				require.NoError(t, f.reconcile(t))
				after, listed := f.approval(t)
				require.Equal(t, before.ID, after.ID)
				require.Equal(t, before.Binding, after.Binding)
				require.Equal(t, string(f.task.UID), after.TaskUID)
				require.Equal(t, test.outcome, after.ExecutionOutcome)
				require.Len(t, listed, len(beforeEvents)+1, "project the original Task's execution outcome once")
				persisted, err := f.control.GetExternalEffectByIdentity(f.ctx, effect.Identity)
				require.NoError(t, err)
				if test.state == store.ExternalEffectPending {
					require.Equal(t, store.ExternalEffectFailed, persisted.State)
					require.Equal(t, effect.Version+1, persisted.Version)
					require.Zero(t, persisted.Attempts)
					outcome, reason, _, receiptErr := acpMCPApprovalReceiptOutcome(persisted, call.ID)
					require.NoError(t, receiptErr)
					require.Equal(t, "not_started", outcome)
					require.Equal(t, acpApprovalCodeStale, reason)
					require.Equal(t, reason, after.ExecutionReason)
				} else {
					require.Equal(t, effect, persisted, "a terminal receipt must remain byte-for-byte unchanged")
				}
				assertMCPApprovalRecoveryBindingUnchanged(t, f, object, persisted)
				require.NoError(t, f.reconcile(t))
				_, again := f.approval(t)
				require.Equal(t, listed, again, "repeat reconciliation must not append or rewrite the outcome")
				otherEvents, err := approvals.ListEvents(f.ctx, f.events, other.Namespace, other.Name)
				require.NoError(t, err)
				require.Empty(t, otherEvents, "a forged label must not attach the bound effect to another Task")
				require.Equal(t, count, f.count.Load(), "recovery must not execute or repeat the tool")
				require.Zero(t, f.secretReads.Load(), "recovery must not read executable Secrets")
			})
		}
	}
}

func TestMCPApprovalBindingRecoveryProjectsExpiredLeaseAfterLabelPatch(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "current_epoch"
		if restart {
			name = "successor_epoch"
		}
		for _, hint := range []string{"", "another-task-uid"} {
			t.Run(name+"/hint="+hint, func(t *testing.T) {
				f := newMCPApprovalRecoveryFixture(t)
				createMCPApprovalRecoveryOtherTask(t, f)
				call, effect := seedMCPApprovalRecoveryExpiredLease(t, f)
				object := patchMCPApprovalRecoveryHint(t, f, effect.ID, hint)
				_, before := f.approval(t)
				require.True(t, time.Now().After(effect.LeaseExpiresAt.Add(acpExternalEffectReconcileGrace)))
				if restart {
					f.restart(t)
				} else {
					f.broker.ApprovalSecrets = nil
				}

				// One scan must project the receipt returned by the expired-lease
				// CAS, rather than retaining the pre-transition candidate version.
				require.NoError(t, f.reconcile(t))
				after, listed := f.approval(t)
				require.Equal(t, call.ID, after.ID)
				require.Equal(t, "unknown", after.ExecutionOutcome)
				require.Equal(t, acpMCPApprovalUnknownReason, after.ExecutionReason)
				require.Len(t, listed, len(before)+1)
				persisted, err := f.control.GetExternalEffectByIdentity(f.ctx, effect.Identity)
				require.NoError(t, err)
				require.Equal(t, store.ExternalEffectOutcomeUnknown, persisted.State)
				require.Equal(t, effect.Version+1, persisted.Version)
				require.EqualValues(t, 1, persisted.Attempts)
				require.Equal(t, f.fence.Epoch, persisted.ControllerEpoch)
				require.Empty(t, persisted.LeaseOwner)
				require.Nil(t, persisted.LeaseExpiresAt)
				require.Empty(t, persisted.Response, "an interrupted execution has no fabricated result")
				assertMCPApprovalRecoveryBindingUnchanged(t, f, object, persisted)
				require.NoError(t, f.reconcile(t))
				_, again := f.approval(t)
				require.Equal(t, listed, again)
				require.EqualValues(t, 1, f.count.Load(), "the original execution must never be repeated")
				require.Zero(t, f.secretReads.Load())
			})
		}
	}
}

func TestMCPApprovalBindingRecoveryPreservesLegacyHintBehavior(t *testing.T) {
	for _, hint := range []string{"own", "", "another-task-uid"} {
		t.Run("hint="+hint, func(t *testing.T) {
			f := newMCPApprovalRecoveryFixture(t)
			createMCPApprovalRecoveryOtherTask(t, f)
			descriptor, err := f.request.ValidateAt(time.Now().UTC())
			require.NoError(t, err)
			digest, err := acpMCPApprovalRequestDigest(f.request, descriptor)
			require.NoError(t, err)
			// A pre-upgrade reservation has no immutable binding. The broker
			// may later add the discovery label without changing that spec.
			_, err = f.control.ReserveExternalEffect(f.ctx, store.ReserveExternalEffectRequest{
				Identity: mcpApprovalRecoveryEffectIdentity(f.request), RequestDigest: digest, Fence: f.fence,
			})
			require.NoError(t, err)
			_, effect := f.seed(t, store.ExternalEffectSucceeded, "running", true, json.RawMessage(`{"workOrder":"simulated-1"}`))
			if hint == "own" {
				hint = string(f.task.UID)
			}
			object := patchMCPApprovalRecoveryHint(t, f, effect.ID, hint)
			require.Empty(t, object.Spec.ApprovalTaskUID)
			before, beforeEvents := f.approval(t)
			f.restart(t)
			require.NoError(t, f.reconcile(t))
			after, listed := f.approval(t)
			if hint == string(f.task.UID) {
				require.Equal(t, "succeeded", after.ExecutionOutcome)
				require.Len(t, listed, len(beforeEvents)+1)
			} else {
				require.Equal(t, before, after, "legacy discovery still requires the original Task hint")
				require.Equal(t, beforeEvents, listed)
			}
			persisted, err := f.control.GetExternalEffectByIdentity(f.ctx, effect.Identity)
			require.NoError(t, err)
			require.Equal(t, effect, persisted)
			assertMCPApprovalRecoveryBindingUnchanged(t, f, object, persisted)
			require.NoError(t, f.reconcile(t))
			_, again := f.approval(t)
			require.Equal(t, listed, again)
			require.EqualValues(t, 1, f.count.Load())
			require.Zero(t, f.secretReads.Load())
		})
	}
}

func TestMCPApprovalBindingRecoveryDoesNotBorrowAnotherTasksEffect(t *testing.T) {
	for _, state := range []store.ExternalEffectState{store.ExternalEffectPending, store.ExternalEffectSucceeded} {
		t.Run(string(state), func(t *testing.T) {
			f := newMCPApprovalRecoveryFixture(t)
			other := createMCPApprovalRecoveryOtherTask(t, f)
			_, effect := f.seed(t, state, "", state == store.ExternalEffectSucceeded, json.RawMessage(`{"workOrder":"simulated-1"}`))
			object := patchMCPApprovalRecoveryHint(t, f, effect.ID, string(other.UID))
			_, before := f.approval(t)
			f.restart(t)
			observed := &approvalRecoveryEventStore{DeduplicatingExecutionEventStore: f.events}
			f.dispatcher.EventStore = observed
			count := f.count.Load()

			// The immutable owner is absent from this reconciliation snapshot.
			// Another Task sharing its session cannot inherit either its pending
			// reservation or its receipt through the mutable discovery hint.
			for range 2 {
				require.NoError(t, f.dispatcher.reconcileExpiredExternalEffects(f.ctx, []corev1alpha1.Task{*other}))
			}
			persisted, err := f.control.GetExternalEffectByIdentity(f.ctx, effect.Identity)
			require.NoError(t, err)
			require.Equal(t, effect, persisted, "a different Task must not settle the absent owner's effect")
			assertMCPApprovalRecoveryBindingUnchanged(t, f, object, persisted)
			_, after := f.approval(t)
			require.Equal(t, before, after)
			otherEvents, err := approvals.ListEvents(f.ctx, f.events, other.Namespace, other.Name)
			require.NoError(t, err)
			require.Empty(t, otherEvents)
			require.Zero(t, observed.lists.Load(), "a forged hint must not select the other Task's history")
			require.Zero(t, observed.sequenceBatches.Load())
			require.Zero(t, observed.appends.Load())
			require.Zero(t, f.secretReads.Load())
			require.Equal(t, count, f.count.Load())
		})
	}
}

func createMCPApprovalRecoveryOtherTask(t *testing.T, f *mcpApprovalRecoveryFixture) *corev1alpha1.Task {
	t.Helper()
	other := f.task.DeepCopy()
	other.Name, other.UID, other.ResourceVersion = "another-task", "another-task-uid", ""
	other.Status.Execution = &corev1alpha1.TaskExecutionStatus{RuntimeSessionUID: string(f.request.Authorization.RuntimeSessionUID)}
	require.NoError(t, f.kube.Create(f.ctx, other))
	return other
}

func mcpApprovalRecoveryEffectIdentity(request harnessv2.MCPBrokerCallRequest) store.ExternalEffectIdentity {
	return store.ExternalEffectIdentity{
		Kind: acpMCPToolEffectKind, Namespace: request.Namespace,
		AggregateID: string(request.Authorization.RuntimeSessionUID), OperationID: string(request.Metadata.OperationID),
	}
}

func patchMCPApprovalRecoveryHint(t *testing.T, f *mcpApprovalRecoveryFixture, id, hint string) *corev1alpha1.ExternalEffect {
	t.Helper()
	var effects corev1alpha1.ExternalEffectList
	require.NoError(t, f.kube.List(f.ctx, &effects, client.InNamespace(f.task.Namespace)))
	require.Len(t, effects.Items, 1)
	object := effects.Items[0].DeepCopy()
	require.Equal(t, id, object.Spec.ID)
	before := object.DeepCopy()
	if hint == "" {
		delete(object.Labels, corev1alpha1.ControlRecordTaskUIDLabel)
	} else {
		object.Labels[corev1alpha1.ControlRecordTaskUIDLabel] = hint
	}
	require.NoError(t, f.kube.Patch(f.ctx, object, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})))
	require.NoError(t, f.kube.Get(f.ctx, client.ObjectKeyFromObject(object), object))
	if before.Labels[corev1alpha1.ControlRecordTaskUIDLabel] != hint {
		require.NotEqual(t, before.ResourceVersion, object.ResourceVersion, "the metadata change must be persisted")
	}
	require.Equal(t, hint, object.Labels[corev1alpha1.ControlRecordTaskUIDLabel])
	require.Equal(t, before.UID, object.UID)
	require.Equal(t, before.Spec, object.Spec)
	require.Equal(t, before.Status, object.Status, "a metadata patch must not settle the effect")
	return object
}

func assertMCPApprovalRecoveryBindingUnchanged(t *testing.T, f *mcpApprovalRecoveryFixture, before *corev1alpha1.ExternalEffect, effect *store.ExternalEffect) {
	t.Helper()
	current := &corev1alpha1.ExternalEffect{}
	require.NoError(t, f.kube.Get(f.ctx, client.ObjectKeyFromObject(before), current))
	require.Equal(t, before.UID, current.UID)
	require.Equal(t, before.Spec, current.Spec)
	require.Equal(t, before.Labels, current.Labels, "recovery must not repair or trust the forged hint")
	require.Equal(t, before.Spec.ID, effect.ID)
	require.Equal(t, before.Spec.RequestDigest, effect.RequestDigest)
	require.Equal(t, mcpApprovalRecoveryEffectIdentity(f.request), effect.Identity)
}

func seedMCPApprovalRecoveryExpiredLease(t *testing.T, f *mcpApprovalRecoveryFixture) (*acpMCPApprovalCall, *store.ExternalEffect) {
	t.Helper()
	descriptor, err := f.request.ValidateAt(time.Now().UTC())
	require.NoError(t, err)
	digest, err := acpMCPApprovalRequestDigest(f.request, descriptor)
	require.NoError(t, err)
	// Seed coherent historical times through the real store API. No status
	// patch or sleep fabricates expiry, and recovery has no executable Secret.
	created := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)
	call := &acpMCPApprovalCall{
		ID: acpMCPApprovalIdentity(f.request), RequestDigest: digest, Request: f.request, Descriptor: descriptor,
		Task:      ACPMCPAuthenticatedTask{Name: f.task.Name, Namespace: f.task.Namespace, UID: string(f.task.UID)},
		CreatedAt: created, ExpiresAt: created.Add(harnessv2.MCPApprovalWaitTimeout),
	}
	effect, err := f.control.ReserveExternalEffect(f.ctx, store.ReserveExternalEffectRequest{
		Identity: mcpApprovalRecoveryEffectIdentity(f.request), RequestDigest: digest,
		Fence: f.fence, CreatedAt: created, ApprovalTaskUID: string(f.task.UID),
	})
	require.NoError(t, err)
	require.NoError(t, f.broker.requestToolApproval(f.ctx, call))
	f.decide(call.ID, events.ExecutionEventTypeApprovalApproved)
	expires := created.Add(time.Minute)
	effect, err = f.control.TransitionExternalEffect(f.ctx, store.ExternalEffectTransition{
		ID: effect.ID, Fence: f.fence, ExpectedVersion: effect.Version,
		ExpectedState: store.ExternalEffectPending, NewState: store.ExternalEffectInFlight,
		RequestDigest: digest, LeaseOwner: "expired-original-owner", LeaseExpiresAt: &expires,
		UpdatedAt: created.Add(time.Second),
	})
	require.NoError(t, err)
	require.NoError(t, f.broker.approvalOutcome(f.ctx, call, "running", "Approved action started", nil))
	_, err = f.broker.Executor.ExecuteACPMCPTool(f.ctx, f.request, descriptor)
	require.NoError(t, err)
	require.EqualValues(t, 1, effect.Attempts)
	return call, effect
}
