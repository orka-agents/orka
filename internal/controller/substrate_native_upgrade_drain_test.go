package controller

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func TestNativeSubstrateUpgradeDrainRequiresCompletedFailedCleanup(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	h.r.ControllerNamespace = "native-upgrade-control"
	h.until(t, nativeTestServing)
	substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
	h.until(t, nativeTestSuspended)
	digest := h.record(t).Checkpoint.Digest
	substrateSuspendTestPoolIntent(t, h.r, h.pool, false)
	h.until(t, nativeTestServing)
	lost := h.record(t).Attempt
	delete(h.api.actors, lost.Name)
	h.step(t)

	coordinator := &ACPUpgradeDrainCoordinator{
		Client: h.r.Client, APIReader: h.r.Client, ControllerNamespace: h.r.ControllerNamespace,
		Options: ACPUpgradeDrainOptions{WatchNamespace: h.pool.Namespace},
		Barriers: ACPUpgradeDrainBarrierObserverFunc(func(context.Context) (ACPUpgradeDrainBarrierSnapshot, error) {
			return ACPUpgradeDrainBarrierSnapshot{}, nil
		}),
	}
	require.NoError(t, coordinator.setRuntimePoolDesiredReplicasZero(t.Context(), client.ObjectKeyFromObject(h.pool)))
	pool := runtimePoolTestGetPool(t, h.r, h.pool)
	fence := store.ControllerEpochFence{Epoch: pool.Status.ControllerEpoch}
	_, err := coordinator.reconcileDrainPass(t.Context(), fence)
	require.ErrorContains(t, err, "cleanup is incomplete", "Degraded status alone cannot prove workload absence")
	h.until(t, func(pool *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
		return record.Phase == substrateNativeFailed && record.Attempt == nil && pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleDegraded
	})
	snapshot, err := coordinator.reconcileDrainPass(t.Context(), fence)
	require.NoError(t, err)
	require.Equal(t, 1, snapshot.ObservedPools)
	require.True(t, snapshot.Quiescent())
	require.Empty(t, h.api.actors)
	require.False(t, h.api.deleteWithLivePod)
	require.NotEmpty(t, h.record(t).Failure)
	require.Equal(t, digest, h.record(t).Checkpoint.Digest)
	pool = runtimePoolTestGetPool(t, h.r, h.pool)
	require.Equal(t, corev1alpha1.RuntimePoolLifecycleDegraded, pool.Status.Lifecycle)
	cm, _, err := h.r.readNativeSubstrateState(t.Context(), &pool)
	require.NoError(t, err)

	for _, mismatch := range []string{
		"pending attempt", "wrong pool UID", "wrong journal owner", "wrong atespace", "missing journal",
		"unavailable journal", "wrong controller namespace", "stale generation", "new demand", "live replicas",
		"open admission", "missing journal marker", "incomplete cleanup phase",
	} {
		t.Run(mismatch, func(t *testing.T) {
			candidate, journal, record := pool.DeepCopy(), cm.DeepCopy(), h.record(t)
			switch mismatch {
			case "pending attempt":
				record.Attempt = lost
			case "wrong pool UID":
				record.PoolUID = "another-pool"
			case "wrong journal owner":
				journal.Labels[runtimePoolUIDLabel] = "another-pool"
			case "wrong atespace":
				record.Atespace = "another-atespace"
			case "stale generation":
				candidate.Status.ObservedGeneration--
			case "new demand":
				candidate.Spec.DesiredReplicas = 1
			case "live replicas":
				candidate.Status.CurrentReplicas = 1
			case "open admission":
				candidate.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionAccepting
			case "missing journal marker":
				delete(candidate.Annotations, substrateNativeJournalAnnotation)
			case "incomplete cleanup phase":
				record.Phase = substrateNativeStopping
			}
			encoded, err := json.Marshal(record)
			require.NoError(t, err)
			journal.Data[substrateNativeStateKey] = string(encoded)
			kube := fake.NewClientBuilder().WithScheme(h.r.Scheme).WithObjects(journal).Build()
			observer := &ACPUpgradeDrainCoordinator{Client: kube, APIReader: kube, ControllerNamespace: h.r.ControllerNamespace}
			switch mismatch {
			case "missing journal":
				require.NoError(t, kube.Delete(t.Context(), journal))
			case "unavailable journal":
				observer.APIReader = recoveryJournalUnavailableReader{Reader: kube}
			case "wrong controller namespace":
				observer.ControllerNamespace = "different-control"
			}
			err = observer.observeAndDrainRuntimePool(t.Context(), fence, candidate, &ACPUpgradeDrainSnapshot{})
			require.Error(t, err, "unverified failed cleanup cannot complete a planned handoff")
			// Observing drain never edits the retained failure or checkpoint.
			if mismatch != "missing journal" {
				observed := &corev1.ConfigMap{}
				require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(journal), observed))
				require.Equal(t, journal.Data, observed.Data)
			}
		})
	}
}

func TestNativeSubstrateUpgradeDrainPreservesPendingDetachCheckpoint(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	h.until(t, nativeTestServing)
	source := h.record(t).Attempt
	h.api.data[source.Name] = "workspace change before controller restart"

	pool := runtimePoolTestGetPool(t, h.r, h.pool)
	linked := &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: pool.Namespace, Name: "pending-upgrade-suspend", UID: "pending-upgrade-uid",
			Annotations: map[string]string{
				acpWorkspaceDetachActionAnnotation:  string(workspacev1alpha1.WorkspaceOnDetachSuspend),
				acpExecutionWorkspacePoolAnnotation: pool.Name,
			},
		},
		Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
			Mode:         workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredReady,
		},
	}
	require.NoError(t, h.r.Create(t.Context(), linked))
	pool.Labels[acpExecutionWorkspaceLinkLabel] = linked.Name
	pool.Annotations[acpExecutionWorkspaceUIDAnnotation] = string(linked.UID)
	require.NoError(t, h.r.Update(t.Context(), &pool))

	// The real restart hook lowers replicas before Task settlement records
	// DesiredState=Suspended. Its frozen detach action must preserve the data
	// while the coordinator drains the supervisor and settlement catches up.
	coordinator := &ACPUpgradeDrainCoordinator{Client: h.r.Client}
	require.NoError(t, coordinator.setRuntimePoolDesiredReplicasZero(t.Context(), client.ObjectKeyFromObject(&pool)))
	for range 16 {
		h.step(t)
	}
	require.Contains(t, h.api.actors, source.Name, "upgrade drain deleted the Actor before its requested checkpoint")
	require.Zero(t, h.api.deletes)
	require.Equal(t, "workspace change before controller restart", h.api.data[source.Name])
	pool = runtimePoolTestGetPool(t, h.r, h.pool)
	require.NotNil(t, pool.Status.ActiveInstance, "planned drain must still be able to authenticate the live supervisor")
	require.EqualValues(t, 1, pool.Status.CurrentReplicas)
	require.Equal(t, corev1alpha1.RuntimePoolAdmissionDraining, pool.Status.AdmissionState)

	// The next controller recovers the Task outcome, then the workspace
	// adapter persists suspension against the still-owned runtime.
	h.r.ControllerEpoch++
	require.NoError(t, h.r.Get(t.Context(), client.ObjectKeyFromObject(linked), linked))
	linked.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	require.NoError(t, h.r.Update(t.Context(), linked))
	adapter := &ACPExecutionWorkspaceAdapterReconciler{Client: h.r.Client, APIReader: h.r.APIReader}
	_, err := adapter.reconcileSuspension(t.Context(), linked)
	require.NoError(t, err)
	h.until(t, nativeTestSuspended)
	saved := h.record(t).Checkpoint
	require.Equal(t, source.UID, saved.SourceUID)
	require.Equal(t, "workspace change before controller restart", h.api.tagData[saved.Name])
	require.Empty(t, h.api.actors)
	require.False(t, h.api.deleteWithLivePod)

	require.NoError(t, h.r.Get(t.Context(), client.ObjectKeyFromObject(linked), linked))
	_, err = adapter.reconcileSuspension(t.Context(), linked)
	require.NoError(t, err)
	require.NoError(t, h.r.Get(t.Context(), client.ObjectKeyFromObject(linked), linked))
	require.Equal(t, workspacev1alpha1.ExecutionWorkspaceStateSuspended, linked.Status.State)
}

func TestLinkedWorkspaceFrozenSuspendHoldRequiresLiveExactWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(*workspacev1alpha1.ExecutionWorkspace)
		hold     bool
		deleting bool
	}{
		{name: "pending settlement", hold: true},
		{name: "delete action", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachDelete)
		}},
		{name: "replacement workspace", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.UID = "replacement-uid"
		}},
		{name: "failed workspace", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
		}},
		{name: "delete requested", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
		}},
		{name: "deleting workspace", deleting: true, mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Finalizers = []string{"test.orka.ai/finalizer"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newNativeRuntimeTestHarness(t)
			pool := runtimePoolTestGetPool(t, h.r, h.pool)
			pool.Labels[acpExecutionWorkspaceLinkLabel] = "frozen-suspend"
			if pool.Annotations == nil {
				pool.Annotations = map[string]string{}
			}
			pool.Annotations[acpExecutionWorkspaceUIDAnnotation] = "original-uid"
			linked := &workspacev1alpha1.ExecutionWorkspace{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: pool.Namespace, Name: "frozen-suspend", UID: "original-uid",
					Annotations: map[string]string{acpWorkspaceDetachActionAnnotation: string(workspacev1alpha1.WorkspaceOnDetachSuspend)},
				},
				Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
					Mode:         workspacev1alpha1.ExecutionWorkspaceModeInteractive,
					DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredReady,
				},
			}
			if tc.mutate != nil {
				tc.mutate(linked)
			}
			require.NoError(t, h.r.Create(t.Context(), linked))
			if tc.deleting {
				require.NoError(t, h.r.Delete(t.Context(), linked))
			}
			hold, err := h.r.linkedWorkspaceSuspendIntentPending(t.Context(), &pool)
			require.NoError(t, err)
			require.Equal(t, tc.hold, hold)
		})
	}
}
