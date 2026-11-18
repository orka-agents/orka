package controller

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	v2conformance "github.com/orka-agents/orka/internal/harness/v2/conformance"
)

func probeBudgetPrevious() (*corev1alpha1.AgentRuntimeObservedCapabilities, harnessv2.Fence) {
	fence := harnessv2.Fence{RuntimeInstanceID: "runtime", SupervisorBootID: "boot", ControllerEpoch: 1,
		RuntimePoolUID: "pool", RuntimePoolGeneration: 1, RuntimeProfileDigest: "profile", ProfileDigestSchemaVersion: 1}
	previous := &corev1alpha1.AgentRuntimeObservedCapabilities{RuntimeInstanceID: string(fence.RuntimeInstanceID),
		SupervisorBootID: string(fence.SupervisorBootID), ControllerEpoch: int64(fence.ControllerEpoch),
		RuntimePoolUID: string(fence.RuntimePoolUID), RuntimePoolGeneration: int64(fence.RuntimePoolGeneration),
		RuntimeProfileDigest: string(fence.RuntimeProfileDigest), ProfileDigestSchemaVersion: int32(fence.ProfileDigestSchemaVersion)}
	return previous, fence
}

func TestAgentRuntimeV2ProbeBudgets(t *testing.T) {
	for _, test := range []struct {
		name    string
		deep    bool
		upgrade bool
	}{
		{name: "shallow"},
		{name: "deep", deep: true},
		{name: "identity change near shallow deadline", upgrade: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				previous, fence := probeBudgetPrevious()
				target := v2conformance.Target{ControlTimeout: agentRuntimeProbeTimeout, ProbeLifecycle: test.deep,
					ExpectedRuntimeInstanceID: fence.RuntimeInstanceID, ExpectedControllerEpoch: fence.ControllerEpoch}
				var contexts []context.Context
				result := checkAgentRuntimeV2Conformance(t.Context(), target, previous, func(ctx context.Context, got v2conformance.Target) v2conformance.Result {
					deep := test.deep || len(contexts) == 1
					want := 60 * time.Second
					if deep {
						want = 180 * time.Second
					}
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) != want || got.ProbeTimeout != want || got.ProbeLifecycle != deep ||
						got.ControlTimeout != 60*time.Second || got.ExpectedRuntimeInstanceID != target.ExpectedRuntimeInstanceID ||
						got.ExpectedControllerEpoch != target.ExpectedControllerEpoch {
						t.Fatal("probe changed control authority or used the wrong lifecycle deadline")
					}
					if len(contexts) == 1 && contexts[0].Err() != context.Canceled {
						t.Fatal("shallow probe context was not closed before the deep check")
					}
					contexts = append(contexts, ctx)
					if test.upgrade && !deep {
						time.Sleep(59 * time.Second)
						fence.SupervisorBootID = "new-boot"
					}
					if deep {
						time.Sleep(61 * time.Second)
						if ctx.Err() != nil {
							t.Fatal("deep check inherited the shallow or ordinary-control deadline")
						}
					}
					return v2conformance.Result{Passed: true, ObservedStatus: &harnessv2.StatusResponse{Fence: fence}}
				})
				wantCalls := 1
				if test.upgrade {
					wantCalls++
				}
				if !result.Passed || len(contexts) != wantCalls {
					t.Fatal("probe was skipped or repeated")
				}
				for _, ctx := range contexts {
					if ctx.Err() != context.Canceled {
						t.Fatal("completed probe context remains active")
					}
				}
			})
		})
	}
}

func TestAgentRuntimeV2ProbeUpgradeHonorsCallerDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		previous, fence := probeBudgetPrevious()
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Second)
		defer cancel()
		deadline, _ := ctx.Deadline()
		calls := 0
		result := checkAgentRuntimeV2Conformance(ctx, v2conformance.Target{ControlTimeout: agentRuntimeProbeTimeout}, previous,
			func(probeCtx context.Context, got v2conformance.Target) v2conformance.Result {
				calls++
				if calls == 1 {
					time.Sleep(59 * time.Second)
					fence.SupervisorBootID = "new-boot"
					return v2conformance.Result{Passed: true, ObservedStatus: &harnessv2.StatusResponse{Fence: fence}}
				}
				actual, ok := probeCtx.Deadline()
				if !ok || !actual.Equal(deadline) || got.ProbeTimeout != 180*time.Second {
					t.Fatal("deep upgrade escaped its caller deadline")
				}
				<-probeCtx.Done()
				return v2conformance.Result{}
			})
		if result.Passed || calls != 2 || ctx.Err() != context.DeadlineExceeded {
			t.Fatal("expired caller authorized another conformance attempt")
		}
	})
}

func TestAgentRuntimeV2FailedShallowProbeNeverUpgrades(t *testing.T) {
	previous, fence := probeBudgetPrevious()
	fence.SupervisorBootID = "new-boot"
	calls := 0
	result := checkAgentRuntimeV2Conformance(t.Context(), v2conformance.Target{ControlTimeout: agentRuntimeProbeTimeout}, previous,
		func(context.Context, v2conformance.Target) v2conformance.Result {
			calls++
			return v2conformance.Result{ObservedStatus: &harnessv2.StatusResponse{Fence: fence}}
		})
	if result.Passed || calls != 1 {
		t.Fatal("failed shallow observation was retried as deep conformance")
	}
}
