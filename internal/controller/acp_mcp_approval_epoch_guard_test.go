package controller

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

type approvalEpochClaimBarrier struct {
	store.ExternalEffectStore
	claimed chan *store.ExternalEffect
	release <-chan struct{}
}

func (s *approvalEpochClaimBarrier) TransitionExternalEffect(ctx context.Context, transition store.ExternalEffectTransition) (*store.ExternalEffect, error) {
	effect, err := s.ExternalEffectStore.TransitionExternalEffect(ctx, transition)
	if err != nil || transition.ExpectedState != store.ExternalEffectPending || transition.NewState != store.ExternalEffectInFlight {
		return effect, err
	}
	s.claimed <- effect
	select {
	case <-s.release:
		return effect, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestMCPApprovalAuthoritativeEpochLossStopsBeforeExecution(t *testing.T) {
	for _, stage := range []string{"before_claim", "after_claim_before_start"} {
		t.Run(stage, func(t *testing.T) {
			f := newMCPApprovalRecoveryFixture(t)
			registry := f.broker.Prompts.(DurableACPMCPPromptAuthorizer).PromptLeases
			registerApprovalLease(t, registry, f.ctx, f.request)
			callCtx, cancelCall := context.WithCancel(f.ctx)
			defer cancelCall()
			release := make(chan struct{})
			var releaseOnce sync.Once
			resume := func() { releaseOnce.Do(func() { close(release) }) }
			defer resume()
			barrier := &approvalEpochClaimBarrier{ExternalEffectStore: f.control,
				claimed: make(chan *store.ExternalEffect, 1), release: release}
			claimed := stage == "after_claim_before_start"
			if claimed {
				f.broker.Effects = barrier
			}
			done := f.startContext(callCtx, f.request)
			pending := f.pending()
			before, err := f.control.GetExternalEffect(f.ctx, approvalClaimReadEffectID(t, f.mcpApprovalFixture))
			require.NoError(t, err)
			require.Equal(t, store.ExternalEffectPending, before.State)
			if claimed {
				f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
				select {
				case before = <-barrier.claimed:
				case <-f.ctx.Done():
					t.Fatal("approved call did not commit its execution claim")
				}
				require.Equal(t, store.ExternalEffectInFlight, before.State)
			}

			// Change the authoritative epoch and holder while retaining the old
			// broker credentials and live prompt registration. The actual epoch
			// guard, rather than a prompt-watcher cancellation, must stop the call.
			advanceMCPApprovalEpochForRecovery(t, f)
			require.NoError(t, f.broker.Prompts.AuthorizeACPMCPPrompt(f.ctx, f.request))
			if !claimed {
				f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
			}
			resume()
			select {
			case response := <-done:
				// Unlike a readable SQLite-only fixture, the real Kubernetes
				// effect store also fences receipt writes. The obsolete owner
				// must return promptly without claiming it committed a denial.
				require.Equal(t, http.StatusServiceUnavailable, response.Code)
			case <-time.After(3 * time.Second):
				t.Fatal("confirmed epoch loss was retried instead of returning promptly")
			}
			require.Zero(t, f.count.Load())
			after, err := f.control.GetExternalEffect(f.ctx, before.ID)
			require.NoError(t, err)
			require.Equal(t, before, after, "a stale owner cannot claim or settle the durable effect")
			approval, listed := f.approval(t)
			require.Equal(t, approvals.StatusApproved, approval.Status)
			require.Equal(t, "not_started", approval.ExecutionOutcome)
			for _, event := range listed {
				require.NotEqual(t, events.ExecutionEventTypeApprovalExpired, event.Type,
					"confirmed stale authority must not be reported as an expiry")
			}
			requireMCPApprovalEpochRecovery(t, f, pending, before, claimed)
		})
	}
}

func advanceMCPApprovalEpochForRecovery(t *testing.T, f *mcpApprovalRecoveryFixture) {
	t.Helper()
	oldFence := f.fence
	f.stopEpoch()
	epochs, stop := startArchivedRecoveryEpoch(t, f.ctx, f.control, nil, "controller-b")
	f.stopEpoch, f.dispatcher.Epochs = stop, epochs
	var err error
	f.fence, err = epochs.CurrentFence(f.ctx)
	require.NoError(t, err)
	require.Greater(t, f.fence.Epoch, oldFence.Epoch)
	require.NotEqual(t, oldFence.HolderID, f.fence.HolderID)
}

func requireMCPApprovalEpochRecovery(t *testing.T, f *mcpApprovalRecoveryFixture, pending approvals.Approval, before *store.ExternalEffect, claimed bool) {
	t.Helper()
	// Recovery runs with the successor's fence and no remembered authority or
	// access to the executable request. The original handler has already ended.
	f.broker.ApprovalSecrets = nil
	f.broker.Prompts = DurableACPMCPPromptAuthorizer{Attempts: f.control, PromptLeases: &ACPMCPPromptLeaseRegistry{}}
	require.NoError(t, f.reconcile(t))
	after, err := f.control.GetExternalEffect(f.ctx, before.ID)
	require.NoError(t, err)
	approval, listed := f.approval(t)
	require.Equal(t, pending.ID, approval.ID)
	require.Equal(t, pending.Binding, approval.Binding)
	require.Equal(t, approvals.StatusApproved, approval.Status)
	require.Equal(t, before.Attempts, after.Attempts)
	if claimed {
		// The durable claim alone cannot prove the executor was never called.
		// Preserve that uncertainty even though this test observed zero calls.
		require.Equal(t, store.ExternalEffectOutcomeUnknown, after.State)
		require.Equal(t, acpApprovalOutcomeUnknown, approval.ExecutionOutcome)
		require.Equal(t, acpMCPApprovalUnknownReason, approval.ExecutionReason)
		require.Empty(t, after.Response)
	} else {
		require.Equal(t, store.ExternalEffectFailed, after.State)
		require.Equal(t, "not_started", approval.ExecutionOutcome)
		require.Equal(t, acpApprovalCodeStale, approval.ExecutionReason)
		outcome, reason, _, receiptErr := acpMCPApprovalReceiptOutcome(after, pending.ID)
		require.NoError(t, receiptErr)
		require.Equal(t, approval.ExecutionOutcome, outcome)
		require.Equal(t, approval.ExecutionReason, reason)
	}
	require.NoError(t, f.reconcile(t))
	repeated, repeatedEvents := f.approval(t)
	require.Equal(t, approval, repeated)
	require.Len(t, repeatedEvents, len(listed))
	require.Zero(t, f.count.Load())
	require.Zero(t, f.secretReads.Load())
}
