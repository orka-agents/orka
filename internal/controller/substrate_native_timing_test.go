package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"github.com/orka-agents/orka/internal/workspace"
)

type nativeLifecycleClockAPI struct {
	*nativeRuntimeTestAPI
	now                                 time.Time
	resumeDelay, suspendDelay, tagDelay time.Duration
}

func nativeRuntimeWithClock(h *nativeRuntimeTestHarness) *nativeLifecycleClockAPI {
	api := &nativeLifecycleClockAPI{nativeRuntimeTestAPI: h.api, now: h.r.now().Truncate(time.Second)}
	h.r.Now = func() time.Time { return api.now }
	h.r.SubstrateNativeClientFactory = func(SubstrateConfig) (*workspace.SubstrateNativeClient, error) {
		return &workspace.SubstrateNativeClient{Control: api}, nil
	}
	return api
}

func (a *nativeLifecycleClockAPI) ResumeActor(ctx context.Context, req *ateapipb.ResumeActorRequest, opts ...grpc.CallOption) (*ateapipb.ResumeActorResponse, error) {
	a.now = a.now.Add(a.resumeDelay)
	return a.nativeRuntimeTestAPI.ResumeActor(ctx, req, opts...)
}

func (a *nativeLifecycleClockAPI) SuspendActor(ctx context.Context, req *ateapipb.SuspendActorRequest, opts ...grpc.CallOption) (*ateapipb.SuspendActorResponse, error) {
	a.now = a.now.Add(a.suspendDelay)
	return a.nativeRuntimeTestAPI.SuspendActor(ctx, req, opts...)
}

func (a *nativeLifecycleClockAPI) CreateTag(ctx context.Context, req *ateapipb.CreateTagRequest, opts ...grpc.CallOption) (*ateapipb.Tag, error) {
	a.now = a.now.Add(a.tagDelay)
	return a.nativeRuntimeTestAPI.CreateTag(ctx, req, opts...)
}

func TestNativeSubstrateBootRecoveryWindowStartsAtBoot(t *testing.T) {
	for _, tc := range []struct {
		name         string
		restore      bool
		claimTimeout time.Duration
	}{
		{name: "cold boot"},
		{name: "checkpoint continuation", restore: true},
		{name: "short claim timeout", claimTimeout: 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newNativeRuntimeTestHarness(t)
			api := nativeRuntimeWithClock(h)
			h.r.SubstrateConfig.ClaimTimeout = tc.claimTimeout
			if tc.restore {
				h.until(t, nativeTestServing)
				h.api.data[h.record(t).Attempt.Name] = "preserved data"
				substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
				h.until(t, nativeTestSuspended)
				substrateSuspendTestPoolIntent(t, h.r, h.pool, false)
			}
			h.until(t, func(_ *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
				return record != nil && record.Attempt != nil && record.Attempt.UID != "" && !record.Attempt.BootRequested
			})
			// Setup and a valid five-minute RPC have separate recovery budgets.
			api.now = api.now.Add(2 * time.Minute)
			api.resumeDelay = 4*time.Minute + 50*time.Second
			before := h.api.resumes
			h.until(t, func(_ *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
				return record.Attempt.BootRequested
			})
			api.now = api.now.Add(20 * time.Second)
			h.until(t, nativeTestServing)
			require.Equal(t, before+1, h.api.resumes)
			if tc.restore {
				require.Equal(t, "preserved data", h.api.data[h.record(t).Attempt.Name])
			}
		})
	}
}

func TestNativeSubstrateSetupRecoveryStillExpires(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	api := nativeRuntimeWithClock(h)
	h.until(t, func(_ *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
		return record != nil && record.Attempt != nil
	})
	intent := h.record(t).Attempt.Name
	api.now = api.now.Add(7 * time.Minute)
	h.step(t)
	require.Equal(t, substrateNativeFailed, h.record(t).Phase)
	require.Equal(t, intent, h.record(t).Attempt.Name)
	require.Zero(t, h.api.creates)
	require.Zero(t, h.api.resumes)
}

func TestNativeSubstrateLegacyBootDeadlineRemainsBounded(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	api := nativeRuntimeWithClock(h)
	h.until(t, func(_ *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
		return record != nil && record.Attempt != nil && record.Attempt.BootRequested
	})
	pool := runtimePoolTestGetPool(t, h.r, h.pool)
	cm, record, err := h.r.readNativeSubstrateState(t.Context(), &pool)
	require.NoError(t, err)
	record.Attempt.BootStartedAt = metav1.Time{}
	require.NoError(t, h.r.saveNativeSubstrateState(t.Context(), cm, record))
	api.now = api.now.Add(7 * time.Minute)
	h.step(t)
	require.Equal(t, substrateNativeFailed, h.record(t).Phase)
	require.Equal(t, 1, h.api.resumes)
	require.Empty(t, h.seeds)
}

func TestNativeSubstrateCheckpointRecoveryWindows(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		setup, suspend, capture time.Duration
		lostTag                 bool
		claimTimeout            time.Duration
	}{
		{name: "slow drain and suspension", setup: 2 * time.Minute, suspend: 4*time.Minute + 50*time.Second},
		{name: "slow suspension and lost tag response", suspend: 4*time.Minute + 50*time.Second, capture: 4*time.Minute + 50*time.Second, lostTag: true},
		{name: "short claim timeout and lost tag response", suspend: 4*time.Minute + 50*time.Second, capture: 4*time.Minute + 50*time.Second, lostTag: true, claimTimeout: 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newNativeRuntimeTestHarness(t)
			api := nativeRuntimeWithClock(h)
			h.r.SubstrateConfig.ClaimTimeout = tc.claimTimeout
			h.until(t, nativeTestServing)
			h.api.data[h.record(t).Attempt.Name] = "slow checkpoint data"
			substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
			h.until(t, func(_ *corev1alpha1.RuntimePool, record *substrateNativeState) bool { return record.Pending != nil })
			api.now = api.now.Add(tc.setup)
			api.suspendDelay, api.tagDelay, h.api.lostTag = tc.suspend, tc.capture, tc.lostTag
			if tc.lostTag {
				h.until(t, func(_ *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
					return record.Pending != nil && record.Pending.TagIssued
				})
				api.now = api.now.Add(20 * time.Second)
			}
			h.until(t, nativeTestSuspended)
			require.Equal(t, 1, h.api.suspends, "suspension must not be replayed")
			require.Len(t, h.api.tags, 1)
			require.Equal(t, "slow checkpoint data", h.api.tagData[h.record(t).Checkpoint.Name])
			require.False(t, h.api.deleteWithLivePod)
		})
	}
}

func TestNativeSubstrateCheckpointCompletionTime(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	api := nativeRuntimeWithClock(h)
	ws := nativeCheckpointWorkspace(t, h, "completion-time")
	h.until(t, nativeTestServing)
	startedAt := api.now
	api.suspendDelay, api.tagDelay = time.Minute, time.Minute
	substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
	h.until(t, nativeTestSuspended)
	cp := nativeExportCheckpoint(t, h, ws, false)
	require.NotNil(t, cp.Status.CreatedAt)
	require.Equal(t, startedAt.Add(2*time.Minute).UTC(), cp.Status.CreatedAt.UTC())
	api.now = api.now.Add(time.Minute)
	h.step(t)
	require.Equal(t, cp.Status.CreatedAt.Time, h.record(t).Checkpoint.CreatedAt.Time)
}

type nativeCheckpointCommitFailureClient struct {
	client.Client
	failure error
	failed  bool
}

func (c *nativeCheckpointCommitFailureClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if cm, ok := obj.(*corev1.ConfigMap); ok && !c.failed && cm.Data[substrateNativeStateKey] != "" {
		var record substrateNativeState
		if err := json.Unmarshal([]byte(cm.Data[substrateNativeStateKey]), &record); err != nil {
			return err
		}
		if record.Checkpoint != nil && record.Pending == nil {
			c.failed = true
			return c.failure
		}
	}
	return c.Client.Update(ctx, obj, opts...)
}

func TestNativeSubstrateCheckpointCatalogRecoveryKeepsCompletionTime(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	api := nativeRuntimeWithClock(h)
	h.until(t, nativeTestServing)
	api.suspendDelay, api.tagDelay = time.Minute, time.Minute
	substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
	h.until(t, func(_ *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
		return record.Pending != nil && record.Pending.SuspendIssued
	})
	failure := errors.New("checkpoint journal write failed")
	wrapped := &nativeCheckpointCommitFailureClient{Client: h.r.Client, failure: failure}
	h.r.Client = wrapped
	for range 10 {
		_, err := h.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(h.pool)})
		if wrapped.failed {
			require.ErrorIs(t, err, failure)
			break
		}
		require.NoError(t, err)
	}
	require.True(t, wrapped.failed)
	require.NotNil(t, h.record(t).Pending)
	require.Nil(t, h.record(t).Checkpoint)
	catalogs, err := h.r.listSubstrateCheckpointArtifacts(t.Context())
	require.NoError(t, err)
	require.Len(t, catalogs, 1)
	var saved substrateCheckpointArtifact
	require.NoError(t, json.Unmarshal([]byte(catalogs[0].Data[substrateCatalogKey]), &saved))
	require.Equal(t, api.now.UTC(), saved.Checkpoint.CreatedAt.UTC())
	api.now = api.now.Add(time.Minute)
	h.until(t, nativeTestSuspended)
	require.Equal(t, saved.Checkpoint, *h.record(t).Checkpoint)
}

func TestNativeSubstrateCheckpointRecoveryStillExpires(t *testing.T) {
	for _, phase := range []string{"suspend", "tag copy"} {
		t.Run(phase, func(t *testing.T) {
			h := newNativeRuntimeTestHarness(t)
			api := nativeRuntimeWithClock(h)
			h.until(t, nativeTestServing)
			if phase == "suspend" {
				rejected := &nativeSuspendRejectionAPI{nativeRuntimeTestAPI: h.api, rejection: status.Error(codes.Unavailable, "suspension outcome unknown")}
				h.r.SubstrateNativeClientFactory = func(SubstrateConfig) (*workspace.SubstrateNativeClient, error) {
					return &workspace.SubstrateNativeClient{Control: rejected}, nil
				}
			} else {
				h.api.afterTag = func(*ateapipb.Actor) {
					for _, tag := range h.api.tags {
						tag.Status.Snapshot = nil
					}
				}
			}
			substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
			h.until(t, func(_ *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
				return record.Pending != nil && (phase == "suspend" && record.Pending.SuspendIssued || phase == "tag copy" && record.Pending.TagIssued)
			})
			before := h.record(t).Pending.Name
			api.now = api.now.Add(7 * time.Minute)
			h.step(t)
			record := h.record(t)
			require.Equal(t, substrateNativeFailed, record.Phase)
			require.Equal(t, before, record.Pending.Name)
			require.Nil(t, record.Checkpoint)
			pool := runtimePoolTestGetPool(t, h.r, h.pool)
			require.False(t, runtimePoolWorkspaceSuspendConsentRecorded(&pool))
		})
	}
}
