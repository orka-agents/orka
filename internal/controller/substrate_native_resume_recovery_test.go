package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"github.com/orka-agents/orka/internal/workspace"
)

type nativeResumeRejectionAPI struct {
	*nativeRuntimeTestAPI
	rejection error
	calls     int
}

func (a *nativeResumeRejectionAPI) ResumeActor(ctx context.Context, req *ateapipb.ResumeActorRequest, opts ...grpc.CallOption) (*ateapipb.ResumeActorResponse, error) {
	a.calls++
	if a.rejection != nil {
		return nil, a.rejection
	}
	return a.nativeRuntimeTestAPI.ResumeActor(ctx, req, opts...)
}

func TestNativeSubstrateBootRecoversOnlyDefinitiveAuthenticationRejection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{name: "expired credential", err: status.Error(codes.Unauthenticated, "invalid bearer token"), retry: true},
		{name: "missing credential", err: status.Error(codes.Unauthenticated, "missing bearer token"), retry: true},
		{name: "untrusted issuer", err: status.Error(codes.Unauthenticated, `token issuer "untrusted" not trusted`), retry: true},
		{name: "worker credential", err: fmt.Errorf("while creating workload from spec: %w", status.Error(codes.Unauthenticated, "invalid bearer token"))},
		{name: "unavailable", err: status.Error(codes.Unavailable, "boot outcome unknown")},
		{name: "deadline", err: status.Error(codes.DeadlineExceeded, "boot outcome unknown")},
		{name: "permission denied", err: status.Error(codes.PermissionDenied, "worker refused request")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newNativeRuntimeTestHarness(t)
			api := &nativeResumeRejectionAPI{nativeRuntimeTestAPI: h.api, rejection: tc.err}
			h.r.SubstrateNativeClientFactory = func(SubstrateConfig) (*workspace.SubstrateNativeClient, error) {
				return &workspace.SubstrateNativeClient{Control: api}, nil
			}
			h.until(t, func(_ *corev1alpha1.RuntimePool, _ *substrateNativeState) bool { return api.calls == 1 })
			record := h.record(t)
			require.Equal(t, !tc.retry, record.Attempt.BootRequested)
			require.Empty(t, h.seeds, "a rejected boot must not receive runtime credentials")
			attemptUID := record.Attempt.UID
			api.rejection = nil
			// Recovery uses the saved journal, not an in-memory retry flag.
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
			if tc.retry {
				h.until(t, nativeTestServing)
				require.Equal(t, 2, api.calls)
				require.Equal(t, 1, h.api.resumes)
				require.Equal(t, attemptUID, h.record(t).Attempt.UID)
				return
			}
			for range 4 {
				h.step(t)
			}
			require.Equal(t, 1, api.calls, "an ambiguous boot must not be replayed after a negative Actor read")
			require.Empty(t, h.seeds)
		})
	}
}

type nativeConsentPatchFailureClient struct {
	client.Client
	err error
}

func (c *nativeConsentPatchFailureClient) Patch(
	ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption,
) error {
	if pool, ok := obj.(*corev1alpha1.RuntimePool); ok && pool.Annotations[substrateNativeCheckpointConsent] == "" {
		return c.err
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func TestNativeSubstrateResumeRetriesConsentClearAfterJournalSave(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	h.until(t, nativeTestServing)
	h.api.data[h.record(t).Attempt.Name] = "resume recovery proof"
	substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
	h.until(t, nativeTestSuspended)
	h.step(t)
	pool := runtimePoolTestGetPool(t, h.r, h.pool)
	if pool.Annotations[substrateNativeCheckpointConsent] == "" {
		t.Fatal("stopped runtime lost its checkpoint consent")
	}
	beforeCreates := h.api.creates
	baseClient := h.r.Client
	injected := errors.New("injected consent removal failure")
	h.r.Client = &nativeConsentPatchFailureClient{Client: baseClient, err: injected}
	substrateSuspendTestPoolIntent(t, h.r, h.pool, false)
	var reconcileErr error
	for range 20 {
		_, reconcileErr = h.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(h.pool)})
		if reconcileErr != nil {
			break
		}
	}
	if !errors.Is(reconcileErr, injected) {
		t.Fatalf("resume did not reach the consent write failure: %v", reconcileErr)
	}
	record := h.record(t)
	pool = runtimePoolTestGetPool(t, h.r, h.pool)
	if record.Attempt == nil || pool.Annotations[substrateNativeCheckpointConsent] == "" ||
		pool.Status.AdmissionState == corev1alpha1.RuntimePoolAdmissionAccepting || h.api.creates != beforeCreates {
		t.Fatal("failed consent removal lost the journal barrier or admitted a new runtime")
	}
	attemptName := record.Attempt.Name
	// A new reconciler has only the durable journal and the surviving annotation.
	old := h.r
	h.r = &RuntimePoolReconciler{
		Client: baseClient, APIReader: old.APIReader, Scheme: old.Scheme,
		RuntimeNamespace: old.RuntimeNamespace, ControllerNamespace: old.ControllerNamespace,
		ControllerAPIURL: old.ControllerAPIURL, ControllerAPIPort: old.ControllerAPIPort,
		ControllerEpoch: old.ControllerEpoch, AllowedImages: old.AllowedImages,
		WorkspaceArtifactMaxBytes: old.WorkspaceArtifactMaxBytes,
		ProviderProxy:             old.ProviderProxy, SubstrateEnabled: old.SubstrateEnabled,
		SubstrateConfig: old.SubstrateConfig, SubstrateNativeClientFactory: old.SubstrateNativeClientFactory,
		SubstrateCredentialSeeder: old.SubstrateCredentialSeeder, SupervisorClient: old.SupervisorClient,
		Rand: old.Rand, Now: old.Now,
	}
	h.until(t, nativeTestServing)
	pool = runtimePoolTestGetPool(t, h.r, h.pool)
	if !runtimePoolWorkspaceResumeSettled(&pool, false) || h.record(t).Attempt.Name != attemptName ||
		h.api.creates != beforeCreates+1 || h.api.data[attemptName] != "resume recovery proof" {
		t.Fatal("resumed workspace did not settle with its original attempt and preserved data")
	}
}
