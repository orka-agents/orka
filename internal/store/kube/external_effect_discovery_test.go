package kube

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func TestExternalEffectApprovalDiscoveryCreationPreservesIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		hint string
	}{
		{name: "automatic"},
		{name: "approval", hint: "approval-task-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kubeStore, kubeClient, fence := newTestStoreWithEpoch(t)
			request := approvalDiscoveryRequest(fence)
			request.ApprovalTaskUID = tc.hint
			id, err := request.Identity.CanonicalID()
			require.NoError(t, err)
			effect, err := kubeStore.ReserveExternalEffect(t.Context(), request)
			require.NoError(t, err)
			require.Equal(t, id, effect.ID)
			require.Equal(t, request.Identity, effect.Identity)
			require.Equal(t, request.RequestDigest, effect.RequestDigest)
			require.Equal(t, store.ExternalEffectPending, effect.State)
			object := approvalDiscoveryObject(t, kubeClient, request.Identity)
			require.Equal(t, id, object.Spec.ID)
			require.Equal(t, request.Identity.Kind, object.Spec.Kind)
			require.Equal(t, request.Identity.AggregateID, object.Spec.AggregateID)
			require.Equal(t, tc.hint, object.Spec.ApprovalTaskUID)
			require.Equal(t, tc.hint, object.Labels[corev1alpha1.ControlRecordTaskUIDLabel])
			require.Empty(t, object.OwnerReferences, "discovery must not make receipts subject to Task garbage collection")

			completed := settleApprovalDiscoveryEffect(t, kubeStore, request, effect, store.ExternalEffectSucceeded)
			// Omitting the optional hint must still return the same completed
			// execution and must not remove an existing discovery label.
			request.ApprovalTaskUID = ""
			replayed, err := kubeStore.ReserveExternalEffect(t.Context(), request)
			require.NoError(t, err)
			require.Equal(t, completed, replayed)
			object = approvalDiscoveryObject(t, kubeClient, request.Identity)
			require.Equal(t, tc.hint, object.Spec.ApprovalTaskUID)
			require.Equal(t, tc.hint, object.Labels[corev1alpha1.ControlRecordTaskUIDLabel])
			require.Empty(t, object.OwnerReferences)
		})
	}
}

func TestExternalEffectApprovalDiscoveryBackfillsSettledReceipt(t *testing.T) {
	for _, state := range []store.ExternalEffectState{
		store.ExternalEffectSucceeded, store.ExternalEffectFailed, store.ExternalEffectOutcomeUnknown,
	} {
		t.Run(string(state), func(t *testing.T) {
			kubeStore, kubeClient, fence := newTestStoreWithEpoch(t)
			request := approvalDiscoveryRequest(fence)
			effect, err := kubeStore.ReserveExternalEffect(t.Context(), request)
			require.NoError(t, err)
			completed := settleApprovalDiscoveryEffect(t, kubeStore, request, effect, state)
			before := approvalDiscoveryObject(t, kubeClient, request.Identity)
			require.Empty(t, before.Labels[corev1alpha1.ControlRecordTaskUIDLabel])

			request.ApprovalTaskUID = "approval-task-a"
			replayed, err := kubeStore.ReserveExternalEffect(t.Context(), request)
			require.NoError(t, err)
			require.Equal(t, completed, replayed, "adding discovery metadata must preserve the receipt and domain version")
			after := approvalDiscoveryObject(t, kubeClient, request.Identity)
			require.Equal(t, request.ApprovalTaskUID, after.Labels[corev1alpha1.ControlRecordTaskUIDLabel])
			require.Equal(t, before.Spec, after.Spec)
			require.Equal(t, before.Status, after.Status)
			require.Empty(t, after.OwnerReferences)
			require.NotEqual(t, before.ResourceVersion, after.ResourceVersion)

			replayed, err = kubeStore.ReserveExternalEffect(t.Context(), request)
			require.NoError(t, err)
			require.Equal(t, completed, replayed)
			require.Equal(t, after, approvalDiscoveryObject(t, kubeClient, request.Identity),
				"repeated discovery hints must not rewrite metadata or receipts")
			var discovered corev1alpha1.ExternalEffectList
			require.NoError(t, kubeClient.List(t.Context(), &discovered,
				client.InNamespace(request.Identity.Namespace),
				client.MatchingLabels{corev1alpha1.ControlRecordTaskUIDLabel: request.ApprovalTaskUID}))
			require.Len(t, discovered.Items, 1)
			require.Equal(t, completed.ID, discovered.Items[0].Spec.ID)
		})
	}
}

func TestExternalEffectApprovalDiscoveryRejectsRelabeling(t *testing.T) {
	for _, tc := range []struct {
		name          string
		initialHint   string
		changedDigest bool
	}{
		{name: "missing_label_changed_digest", changedDigest: true},
		{name: "existing_label_changed_digest", initialHint: "approval-task-a", changedDigest: true},
		{name: "conflicting_hint", initialHint: "approval-task-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kubeStore, kubeClient, fence := newTestStoreWithEpoch(t)
			request := approvalDiscoveryRequest(fence)
			request.ApprovalTaskUID = tc.initialHint
			effect, err := kubeStore.ReserveExternalEffect(t.Context(), request)
			require.NoError(t, err)
			completed := settleApprovalDiscoveryEffect(t, kubeStore, request, effect, store.ExternalEffectSucceeded)
			before := approvalDiscoveryObject(t, kubeClient, request.Identity)
			request.ApprovalTaskUID = "approval-task-b"
			if tc.changedDigest {
				request.RequestDigest = testDigest("changed approval action")
			}
			_, err = kubeStore.ReserveExternalEffect(t.Context(), request)
			require.ErrorIs(t, err, store.ErrConflict)
			require.Equal(t, before, approvalDiscoveryObject(t, kubeClient, request.Identity))
			persisted, err := kubeStore.GetExternalEffectByIdentity(t.Context(), request.Identity)
			require.NoError(t, err)
			require.Equal(t, completed, persisted)
		})
	}
}

func TestExternalEffectApprovalDiscoveryRejectsInvalidHint(t *testing.T) {
	for _, tc := range []struct {
		name string
		hint string
	}{
		{name: "slash", hint: "task/uid"},
		{name: "whitespace", hint: " task-uid "},
		{name: "oversize", hint: strings.Repeat("a", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kubeStore, kubeClient, fence := newTestStoreWithEpoch(t)
			request := approvalDiscoveryRequest(fence)
			request.ApprovalTaskUID = tc.hint
			_, err := kubeStore.ReserveExternalEffect(t.Context(), request)
			require.ErrorIs(t, err, store.ErrValidation)
			var effects corev1alpha1.ExternalEffectList
			require.NoError(t, kubeClient.List(t.Context(), &effects))
			require.Empty(t, effects.Items, "an invalid hint must not leave an undiscoverable reserved action")
		})
	}
}

func TestExternalEffectApprovalBindingRejectsRelabeledReplay(t *testing.T) {
	for _, changedHint := range []string{"", "approval-task-b"} {
		t.Run("hint="+changedHint, func(t *testing.T) {
			kubeStore, kubeClient, fence := newTestStoreWithEpoch(t)
			request := approvalDiscoveryRequest(fence)
			request.ApprovalTaskUID = "approval-task-a"
			effect, err := kubeStore.ReserveExternalEffect(t.Context(), request)
			require.NoError(t, err)
			object := approvalDiscoveryObject(t, kubeClient, request.Identity)
			before := object.DeepCopy()
			if changedHint == "" {
				delete(object.Labels, corev1alpha1.ControlRecordTaskUIDLabel)
			} else {
				object.Labels[corev1alpha1.ControlRecordTaskUIDLabel] = changedHint
			}
			require.NoError(t, kubeClient.Patch(t.Context(), object, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})))
			patched := approvalDiscoveryObject(t, kubeClient, request.Identity)
			require.Equal(t, before.Spec, patched.Spec)

			request.ApprovalTaskUID = "approval-task-b"
			_, err = kubeStore.ReserveExternalEffect(t.Context(), request)
			require.ErrorIs(t, err, store.ErrConflict, "mutable discovery metadata cannot replace the reserved Task binding")
			require.Equal(t, patched, approvalDiscoveryObject(t, kubeClient, request.Identity))

			request.ApprovalTaskUID = ""
			replayed, err := kubeStore.ReserveExternalEffect(t.Context(), request)
			require.NoError(t, err, "omission-compatible replay must preserve the bound record")
			require.Equal(t, effect, replayed)
			require.Equal(t, patched, approvalDiscoveryObject(t, kubeClient, request.Identity))
		})
	}
}

func approvalDiscoveryRequest(fence store.ControllerEpochFence) store.ReserveExternalEffectRequest {
	return store.ReserveExternalEffectRequest{
		Identity: store.ExternalEffectIdentity{
			Kind: "acp-mcp-tool", Namespace: "tenant-a", AggregateID: "runtime-session-a", OperationID: "mcp-call-a",
		},
		RequestDigest: testDigest("approval action"), Fence: fence, CreatedAt: testNow,
	}
}

func approvalDiscoveryObject(
	t *testing.T,
	kubeClient client.Client,
	identity store.ExternalEffectIdentity,
) *corev1alpha1.ExternalEffect {
	t.Helper()
	id, err := identity.CanonicalID()
	require.NoError(t, err)
	object := &corev1alpha1.ExternalEffect{}
	require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKey{
		Namespace: identity.Namespace, Name: objectName(externalEffectNamePrefix, id),
	}, object))
	return object
}

func settleApprovalDiscoveryEffect(
	t *testing.T,
	kubeStore *Store,
	request store.ReserveExternalEffectRequest,
	effect *store.ExternalEffect,
	state store.ExternalEffectState,
) *store.ExternalEffect {
	t.Helper()
	expires := testNow.Add(5 * time.Minute)
	claimed, err := kubeStore.TransitionExternalEffect(t.Context(), store.ExternalEffectTransition{
		ID: effect.ID, Fence: request.Fence, ExpectedVersion: effect.Version,
		ExpectedState: store.ExternalEffectPending, NewState: store.ExternalEffectInFlight,
		RequestDigest: request.RequestDigest, LeaseOwner: "approval-owner", LeaseExpiresAt: &expires,
		UpdatedAt: testNow.Add(time.Minute),
	})
	require.NoError(t, err)
	response := json.RawMessage(`{"workOrder":"simulated-1"}`)
	digest := store.CanonicalBytesDigest(response)
	if state == store.ExternalEffectOutcomeUnknown {
		response, digest = nil, ""
	}
	_, err = kubeStore.TransitionExternalEffect(t.Context(), store.ExternalEffectTransition{
		ID: effect.ID, Fence: request.Fence, ExpectedVersion: claimed.Version,
		ExpectedState: store.ExternalEffectInFlight, NewState: state, RequestDigest: request.RequestDigest,
		ExpectedLeaseOwner: claimed.LeaseOwner, Response: response, ResponseDigest: digest,
		UpdatedAt: testNow.Add(2 * time.Minute),
	})
	require.NoError(t, err)
	completed, err := kubeStore.GetExternalEffectByIdentity(t.Context(), request.Identity)
	require.NoError(t, err)
	require.Equal(t, state, completed.State)
	require.Equal(t, digest, completed.ResponseDigest)
	require.Equal(t, response, completed.Response)
	return completed
}
