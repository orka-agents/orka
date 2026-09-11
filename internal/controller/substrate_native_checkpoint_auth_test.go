package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"github.com/orka-agents/orka/internal/workspace"
)

type nativeSuspendRejectionAPI struct {
	*nativeRuntimeTestAPI
	rejection error
	calls     int
}

func (a *nativeSuspendRejectionAPI) SuspendActor(ctx context.Context, req *ateapipb.SuspendActorRequest, opts ...grpc.CallOption) (*ateapipb.SuspendActorResponse, error) {
	a.calls++
	if a.rejection != nil {
		return nil, a.rejection
	}
	return a.nativeRuntimeTestAPI.SuspendActor(ctx, req, opts...)
}

func TestNativeSubstrateCheckpointRecoversOnlyDefinitiveAuthenticationRejection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{name: "expired credential", err: status.Error(codes.Unauthenticated, "invalid bearer token"), retry: true},
		{name: "missing credential", err: status.Error(codes.Unauthenticated, "missing bearer token"), retry: true},
		{name: "untrusted issuer", err: status.Error(codes.Unauthenticated, `token issuer "untrusted" not trusted`), retry: true},
		{name: "worker credential", err: fmt.Errorf("while checkpointing workload: %w", status.Error(codes.Unauthenticated, "invalid bearer token"))},
		{name: "unrecognized authentication", err: status.Error(codes.Unauthenticated, "worker rejected authentication")},
		{name: "unavailable", err: status.Error(codes.Unavailable, "suspension outcome unknown")},
		{name: "deadline", err: status.Error(codes.DeadlineExceeded, "suspension outcome unknown")},
		{name: "permission denied", err: status.Error(codes.PermissionDenied, "worker refused request")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newNativeRuntimeTestHarness(t)
			h.until(t, nativeTestServing)
			source := h.record(t).Attempt
			h.api.data[source.Name] = "checkpoint credential recovery proof"
			api := &nativeSuspendRejectionAPI{nativeRuntimeTestAPI: h.api, rejection: tc.err}
			h.r.SubstrateNativeClientFactory = func(SubstrateConfig) (*workspace.SubstrateNativeClient, error) {
				return &workspace.SubstrateNativeClient{Control: api}, nil
			}
			substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
			h.until(t, func(_ *corev1alpha1.RuntimePool, _ *substrateNativeState) bool { return api.calls == 1 })
			rejected := h.record(t)
			require.Equal(t, !tc.retry, rejected.Pending.SuspendIssued)
			require.Nil(t, rejected.Checkpoint)
			require.Zero(t, h.api.suspends)
			// Retry decisions must survive a controller restart and credential repair.
			old := h.r
			h.r = &RuntimePoolReconciler{
				Client: old.Client, APIReader: old.APIReader, Scheme: old.Scheme,
				RuntimeNamespace: old.RuntimeNamespace, ControllerNamespace: old.ControllerNamespace,
				ControllerAPIURL: old.ControllerAPIURL, ControllerAPIPort: old.ControllerAPIPort,
				ControllerEpoch: old.ControllerEpoch, AllowedImages: old.AllowedImages,
				WorkspaceArtifactMaxBytes: old.WorkspaceArtifactMaxBytes,
				ProviderProxy:             old.ProviderProxy, SubstrateEnabled: old.SubstrateEnabled,
				SubstrateConfig: old.SubstrateConfig, SubstrateNativeClientFactory: old.SubstrateNativeClientFactory,
				SubstrateCredentialSeeder: old.SubstrateCredentialSeeder, SupervisorClient: old.SupervisorClient,
				Rand: old.Rand, Now: old.Now,
			}
			api.rejection = nil
			if tc.retry {
				h.until(t, nativeTestSuspended)
				saved := h.record(t).Checkpoint
				require.Equal(t, 2, api.calls)
				require.Equal(t, 1, h.api.suspends)
				require.Equal(t, rejected.Pending.Name, saved.Name)
				require.Equal(t, source.UID, saved.SourceUID)
				require.Equal(t, "checkpoint credential recovery proof", h.api.tagData[saved.Name])
				require.Empty(t, h.api.actors)
				require.False(t, h.api.deleteWithLivePod)
				return
			}
			for range 4 {
				h.step(t)
			}
			require.Equal(t, 1, api.calls, "an ambiguous suspension must not be replayed after a still-running Actor read")
			require.Nil(t, h.record(t).Checkpoint)
			require.Equal(t, source.UID, h.record(t).Attempt.UID)
			pool := runtimePoolTestGetPool(t, h.r, h.pool)
			require.False(t, runtimePoolWorkspaceSuspendConsentRecorded(&pool))
		})
	}
}
