package controller

import (
	"context"
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestNativeSubstrateCheckpointMissingSourceFailsBeforeAcquisition(t *testing.T) {
	for _, stage := range []string{"before selection", "before acquisition"} {
		for _, failure := range []string{"source deleted", "transient read failure"} {
			t.Run(stage+"/"+failure, func(t *testing.T) {
				h := newNativeRuntimeTestHarness(t)
				ws := nativeCheckpointWorkspace(t, h, "source")
				h.until(t, nativeTestServing)
				substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
				h.until(t, nativeTestSuspended)
				ws.Status.State = workspacev1alpha1.ExecutionWorkspaceStateSuspended
				require.NoError(t, h.r.Status().Update(t.Context(), ws))
				cp := &workspacev1alpha1.ExecutionWorkspaceCheckpoint{
					ObjectMeta: metav1.ObjectMeta{Namespace: ws.Namespace, Name: "save-source", UID: "checkpoint-uid", Generation: 1,
						Finalizers: []string{substratePublicCheckpointFinalizer}},
					Spec: workspacev1alpha1.ExecutionWorkspaceCheckpointSpec{
						WorkspaceRef: workspacev1alpha1.ObjectIdentityReference{Name: ws.Name, UID: ws.UID},
					},
				}
				require.NoError(t, h.r.Create(t.Context(), cp))
				r := &SubstrateCheckpointReconciler{RuntimePools: h.r, CheckpointAPIInstalled: true}
				req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cp)}
				if stage == "before acquisition" {
					_, err := r.Reconcile(t.Context(), req)
					require.NoError(t, err)
					require.NoError(t, h.r.Get(t.Context(), req.NamespacedName, cp))
					require.NotEmpty(t, cp.Status.Digest)
				}
				wantPhase, wantReason := "Failed", "SourceMissing"
				if failure == "source deleted" {
					require.NoError(t, h.r.Delete(t.Context(), ws))
				} else {
					wantPhase, wantReason = "Pending", "SourceUnavailable"
					h.r.APIReader = checkpointSourceUnavailableReader{Reader: h.r.Client}
				}
				_, err := r.Reconcile(t.Context(), req)
				require.NoError(t, err)
				require.NoError(t, h.r.Get(t.Context(), req.NamespacedName, cp))
				require.Equal(t, wantPhase, cp.Status.Phase)
				condition := meta.FindStatusCondition(cp.Status.Conditions, substrateNativeReady)
				require.NotNil(t, condition)
				require.Equal(t, wantReason, condition.Reason)
				_, artifact, err := h.r.readSubstrateCheckpointArtifact(t.Context(), h.record(t).Checkpoint.Digest)
				require.NoError(t, err)
				require.NotNil(t, artifact)
				require.False(t, artifact.Owners[substratePublicCheckpointOwner(cp)])
				if failure == "transient read failure" {
					h.r.APIReader = h.r.Client
					for range 3 {
						_, err := r.Reconcile(t.Context(), req)
						require.NoError(t, err)
					}
					require.NoError(t, h.r.Get(t.Context(), req.NamespacedName, cp))
					require.Equal(t, substrateNativeReady, cp.Status.Phase)
				}
			})
		}
	}
}

type checkpointSourceUnavailableReader struct{ client.Reader }

func (r checkpointSourceUnavailableReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, source := obj.(*workspacev1alpha1.ExecutionWorkspace); source {
		return apierrors.NewServiceUnavailable("injected source read failure")
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}
