package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

type submittingMCPAttemptStore struct {
	staticPromptAttemptStore
	current atomic.Pointer[store.PromptAttempt]
	reads   chan struct{}
}

func (s *submittingMCPAttemptStore) GetPromptAttempt(context.Context, string) (*store.PromptAttempt, error) {
	value := *s.current.Load()
	if value.ExecutionState == store.PromptExecutionSubmitting {
		select {
		case s.reads <- struct{}{}:
		default:
		}
	}
	return &value, nil
}

func submittingMCPApproval(t *testing.T, f *mcpApprovalFixture) (*submittingMCPAttemptStore, context.CancelFunc) {
	t.Helper()
	attempt := &store.PromptAttempt{
		Key:        mcpPromptLeaseKey(f.request.Namespace, f.request.Metadata),
		SessionUID: string(f.request.Authorization.RuntimeSessionUID), RuntimeInstanceID: string(f.request.Metadata.Fence.RuntimeInstanceID),
		ControllerEpoch: int64(f.request.Metadata.Fence.ControllerEpoch), ExecutionState: store.PromptExecutionSubmitting,
	}
	var err error
	attempt.ID, err = attempt.Key.CanonicalID()
	require.NoError(t, err)
	storage := &submittingMCPAttemptStore{reads: make(chan struct{}, 2)}
	storage.current.Store(attempt)
	leases := &ACPMCPPromptLeaseRegistry{}
	leaseCtx, cancel := context.WithCancel(t.Context())
	lease, err := leases.register(leaseCtx, f.request.Namespace, harnessv2.StartPromptRequest{
		Metadata: f.request.Metadata, Lease: f.request.Lease, MCPAuthorization: f.request.Authorization,
	})
	require.NoError(t, err)
	t.Cleanup(cancel)
	t.Cleanup(lease.release)
	f.broker.Prompts = DurableACPMCPPromptAuthorizer{Attempts: storage, PromptLeases: leases}
	return storage, cancel
}

func waitForMCPAdmissionBarrier(t *testing.T, storage *submittingMCPAttemptStore, done <-chan *httptest.ResponseRecorder) {
	t.Helper()
	for range 2 {
		select {
		case <-storage.reads:
		case response := <-done:
			t.Fatalf("first approval call failed before durable acceptance: HTTP %d", response.Code)
		case <-time.After(3 * time.Second):
			t.Fatal("approval admission did not wait for the durable Accepted event")
		}
	}
}

func requireNoMCPApprovalAdmission(t *testing.T, f *mcpApprovalFixture) {
	t.Helper()
	listed, err := approvals.ListEvents(t.Context(), f.events, f.request.Namespace, "approval-task")
	require.NoError(t, err)
	require.Empty(t, listed, "review must not be published before prompt acceptance")
	secrets, err := f.broker.ApprovalSecrets.CoreV1().Secrets(f.request.Namespace).List(t.Context(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, secrets.Items, "executable input must not be retained before prompt acceptance")
	require.Zero(t, f.count.Load())
}

func TestMCPApprovalFirstCallWaitsForDurableAcceptance(t *testing.T) {
	for _, effect := range []harnessv2.MCPToolEffect{harnessv2.MCPToolEffectReadOnly, harnessv2.MCPToolEffectConsequential} {
		t.Run(string(effect), func(t *testing.T) {
			f := newMCPApprovalFixture(t)
			f.request.Authorization.ToolPolicy.Tools[0].Effect = effect
			var err error
			f.request.Authorization.ToolPolicy.DescriptorDigest, err = harnessv2.CanonicalMCPToolDescriptorDigest(f.request.Authorization.ToolPolicy.Tools)
			require.NoError(t, err)
			f.request.Metadata.RequestDigest, err = harnessv2.CanonicalRequestDigest(f.request)
			require.NoError(t, err)
			storage, _ := submittingMCPApproval(t, f)
			done := f.start(f.request)
			waitForMCPAdmissionBarrier(t, storage, done)
			requireNoMCPApprovalAdmission(t, f)
			// Execution guards must continue rejecting the submitting attempt.
			require.Error(t, f.broker.Prompts.AuthorizeACPMCPPrompt(t.Context(), f.request))
			accepted := *storage.current.Load()
			accepted.ExecutionState = store.PromptExecutionAccepted
			storage.current.Store(&accepted)
			pending := f.pending()
			require.Zero(t, f.count.Load())
			f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
			result := awaitMCPApprovalResult(t, done)
			require.False(t, result.IsError)
			require.False(t, result.Replayed, "the original call must finish without a client retry")
			require.EqualValues(t, 1, f.count.Load())
		})
	}
}

func TestMCPApprovalAdmissionStopsWhenPromptAuthorityEnds(t *testing.T) {
	for _, reason := range []string{"lease cancelled", "attempt settled", "request cancelled", "operation expired", "session changed"} {
		t.Run(reason, func(t *testing.T) {
			f := newMCPApprovalFixture(t)
			if reason == "operation expired" {
				f.request.Metadata.ExpiresAt = time.Now().UTC().Add(350 * time.Millisecond)
				var err error
				f.request.Metadata.RequestDigest, err = harnessv2.CanonicalRequestDigest(f.request)
				require.NoError(t, err)
			}
			storage, cancelLease := submittingMCPApproval(t, f)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := f.startContext(ctx, f.request)
			waitForMCPAdmissionBarrier(t, storage, done)
			switch reason {
			case "lease cancelled":
				cancelLease()
			case "attempt settled":
				settled := *storage.current.Load()
				settled.ExecutionState = store.PromptExecutionSettling
				storage.current.Store(&settled)
			case "request cancelled":
				cancel()
			case "session changed":
				changed := *storage.current.Load()
				changed.SessionUID = "another-session"
				storage.current.Store(&changed)
			}
			select {
			case response := <-done:
				require.Equal(t, http.StatusForbidden, response.Code)
			case <-time.After(3 * time.Second):
				t.Fatal("revoked approval admission did not stop")
			}
			requireNoMCPApprovalAdmission(t, f)
		})
	}
}
