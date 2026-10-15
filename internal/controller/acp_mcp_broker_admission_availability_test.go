package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

type mcpAdmissionAvailabilityStore struct {
	store.PromptAttemptStore
	read     func(context.Context, string) (*store.PromptAttempt, error)
	calls    atomic.Int32
	failures chan struct{}
}

func (s *mcpAdmissionAvailabilityStore) GetPromptAttempt(ctx context.Context, id string) (*store.PromptAttempt, error) {
	s.calls.Add(1)
	attempt, err := s.read(ctx, id)
	if err != nil {
		select {
		case s.failures <- struct{}{}:
		default:
		}
	}
	return attempt, err
}

func interceptMCPAdmissionReads(f *mcpApprovalFixture, read func(context.Context, string) (*store.PromptAttempt, error)) *mcpAdmissionAvailabilityStore {
	authorizer := f.broker.Prompts.(DurableACPMCPPromptAuthorizer)
	storage := &mcpAdmissionAvailabilityStore{PromptAttemptStore: authorizer.Attempts, read: read, failures: make(chan struct{}, 2)}
	authorizer.Attempts = storage
	f.broker.Prompts = authorizer
	return storage
}

func waitForMCPAdmissionReadOutage(t *testing.T, storage *mcpAdmissionAvailabilityStore, done <-chan *httptest.ResponseRecorder) {
	t.Helper()
	for range 2 {
		select {
		case <-storage.failures:
		case response := <-done:
			t.Fatalf("original approval call ended during the read outage: HTTP %d", response.Code)
		case <-time.After(3 * time.Second):
			t.Fatal("approval admission did not retry the failed durable read")
		}
	}
	select {
	case response := <-done:
		t.Fatalf("original approval call ended before read recovery: HTTP %d", response.Code)
	default:
	}
}

func requireMCPAdmissionDenied(t *testing.T, f *mcpApprovalFixture, done <-chan *httptest.ResponseRecorder) {
	t.Helper()
	select {
	case response := <-done:
		require.Equal(t, http.StatusForbidden, response.Code)
		require.NotContains(t, response.Body.String(), "simulated")
	case <-time.After(3 * time.Second):
		t.Fatal("approval admission did not stop after authority ended")
	}
	requireNoMCPApprovalAdmission(t, f)
}

func TestMCPApprovalAdmissionSurvivesTransientPromptReads(t *testing.T) {
	for _, state := range []store.PromptExecutionState{store.PromptExecutionSubmitting, store.PromptExecutionAccepted} {
		for _, failure := range []struct {
			name string
			err  error
		}{
			{"unavailable", errors.New("simulated durable read outage")},
			{"timeout", apierrors.NewTimeoutError("simulated durable read timeout", 1)},
		} {
			t.Run(string(state)+"/"+failure.name, func(t *testing.T) {
				f := newMCPApprovalFixture(t)
				attempts, _ := submittingMCPApproval(t, f)
				initial := *attempts.current.Load()
				initial.ExecutionState = state
				attempts.current.Store(&initial)
				var unavailable atomic.Bool
				unavailable.Store(true)
				reads := interceptMCPAdmissionReads(f, func(ctx context.Context, id string) (*store.PromptAttempt, error) {
					if unavailable.Load() {
						return nil, failure.err
					}
					return attempts.GetPromptAttempt(ctx, id)
				})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := f.startContext(ctx, f.request)
				waitForMCPAdmissionReadOutage(t, reads, done)
				requireNoMCPApprovalAdmission(t, f)
				unavailable.Store(false)
				if state == store.PromptExecutionSubmitting {
					waitForMCPAdmissionBarrier(t, attempts, done)
					requireNoMCPApprovalAdmission(t, f)
					accepted := initial
					accepted.ExecutionState = store.PromptExecutionAccepted
					attempts.current.Store(&accepted)
				}
				pending := f.pending()
				require.Zero(t, f.count.Load())
				f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
				result := awaitMCPApprovalResult(t, done)
				require.False(t, result.IsError)
				require.False(t, result.Replayed, "the same original HTTP call must complete without redelivery")
				require.Equal(t, f.request.Call.CallID, result.CallID)
				require.EqualValues(t, 1, f.count.Load())
				listed, err := approvals.ListEvents(t.Context(), f.events, f.request.Namespace, "approval-task")
				require.NoError(t, err)
				values := approvals.Derive(listed, time.Time{})
				require.Len(t, values, 1)
				require.Equal(t, pending.ID, values[0].ID)
				require.Equal(t, "succeeded", values[0].ExecutionOutcome)
				requested := 0
				for _, event := range listed {
					if event.Type == events.ExecutionEventTypeApprovalRequested {
						requested++
					}
				}
				require.Equal(t, 1, requested)
				secrets, err := f.broker.ApprovalSecrets.CoreV1().Secrets(f.request.Namespace).List(t.Context(), metav1.ListOptions{})
				require.NoError(t, err)
				require.Len(t, secrets.Items, 1, "only the admitted original call may retain executable input")
			})
		}
	}
}

func TestMCPApprovalAdmissionOutageStopsOnAuthorityEnd(t *testing.T) {
	for _, reason := range []string{"operation deadline", "request cancellation", "local lease cancellation"} {
		t.Run(reason, func(t *testing.T) {
			f := newMCPApprovalFixture(t)
			if reason == "operation deadline" {
				f.request.Metadata.ExpiresAt = time.Now().UTC().Add(time.Second)
				var err error
				f.request.Metadata.RequestDigest, err = harnessv2.CanonicalRequestDigest(f.request)
				require.NoError(t, err)
			}
			_, cancelLease := submittingMCPApproval(t, f)
			reads := interceptMCPAdmissionReads(f, func(context.Context, string) (*store.PromptAttempt, error) {
				return nil, errors.New("simulated continuing read outage")
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := f.startContext(ctx, f.request)
			waitForMCPAdmissionReadOutage(t, reads, done)
			requireNoMCPApprovalAdmission(t, f)
			stoppedAt := time.Now()
			switch reason {
			case "request cancellation":
				cancel()
			case "local lease cancellation":
				cancelLease()
			}
			requireMCPAdmissionDenied(t, f, done)
			require.Less(t, time.Since(stoppedAt), 2*time.Second)
			if reason == "operation deadline" {
				require.False(t, time.Now().Before(f.request.Metadata.ExpiresAt), "outage must remain parked until the original deadline")
			}
			if reason == "local lease cancellation" {
				require.NoError(t, ctx.Err(), "local revocation must stop a still-connected request")
				require.True(t, time.Now().Before(f.request.Metadata.ExpiresAt))
			}
		})
	}
}

func TestMCPApprovalAdmissionBoundsBlockedPromptRead(t *testing.T) {
	for _, reason := range []string{"operation deadline", "request cancellation"} {
		t.Run(reason, func(t *testing.T) {
			f := newMCPApprovalFixture(t)
			f.request.Metadata.ExpiresAt = time.Now().UTC().Add(time.Second)
			var err error
			f.request.Metadata.RequestDigest, err = harnessv2.CanonicalRequestDigest(f.request)
			require.NoError(t, err)
			submittingMCPApproval(t, f)
			entered := make(chan time.Time, 1)
			exited := make(chan error, 1)
			reads := interceptMCPAdmissionReads(f, func(ctx context.Context, _ string) (*store.PromptAttempt, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				deadline, _ := ctx.Deadline()
				select {
				case entered <- deadline:
				default:
				}
				<-ctx.Done()
				select {
				case exited <- ctx.Err():
				default:
				}
				return nil, ctx.Err()
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := f.startContext(ctx, f.request)
			select {
			case deadline := <-entered:
				require.True(t, deadline.Equal(f.request.Metadata.ExpiresAt), "durable I/O must inherit the original operation deadline")
			case response := <-done:
				t.Fatalf("call ended before entering the blocked read: HTTP %d", response.Code)
			case <-time.After(3 * time.Second):
				t.Fatal("admission did not enter the durable read")
			}
			requireNoMCPApprovalAdmission(t, f)
			want := context.DeadlineExceeded
			if reason == "request cancellation" {
				want = context.Canceled
				cancel()
			}
			requireMCPAdmissionDenied(t, f, done)
			require.ErrorIs(t, <-exited, want)
			require.EqualValues(t, 1, reads.calls.Load(), "cancelled durable I/O must not be retried")
		})
	}
}

func TestMCPApprovalAdmissionDoesNotRetryDefinitiveRejection(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		change func(*store.PromptAttempt)
	}{
		{name: "not found", err: store.ErrNotFound},
		{name: "Kubernetes not found", err: apierrors.NewNotFound(schema.GroupResource{Resource: "promptattempts"}, "missing")},
		{name: "conflict", err: store.ConflictErrorf("simulated authoritative conflict")},
		{name: "validation", err: store.ValidationErrorf("simulated invalid record")},
		{name: "session changed", change: func(attempt *store.PromptAttempt) { attempt.SessionUID = "another-session" }},
		{name: "instance changed", change: func(attempt *store.PromptAttempt) { attempt.RuntimeInstanceID = "another-instance" }},
		{name: "epoch changed", change: func(attempt *store.PromptAttempt) { attempt.ControllerEpoch++ }},
		{name: "settling", change: func(attempt *store.PromptAttempt) { attempt.ExecutionState = store.PromptExecutionSettling }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newMCPApprovalFixture(t)
			attempts, _ := submittingMCPApproval(t, f)
			attempt := *attempts.current.Load()
			attempt.ExecutionState = store.PromptExecutionAccepted
			if test.change != nil {
				test.change(&attempt)
			}
			attempts.current.Store(&attempt)
			reads := interceptMCPAdmissionReads(f, func(ctx context.Context, id string) (*store.PromptAttempt, error) {
				if test.err != nil {
					return nil, test.err
				}
				return attempts.GetPromptAttempt(ctx, id)
			})
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			requireMCPAdmissionDenied(t, f, f.startContext(ctx, f.request))
			require.EqualValues(t, 1, reads.calls.Load(), "a definitive rejection must return without a retry")
			require.NoError(t, ctx.Err())
		})
	}
}

func TestMCPNonApprovalAdmissionDoesNotRetryReadOutage(t *testing.T) {
	for _, effect := range []harnessv2.MCPToolEffect{harnessv2.MCPToolEffectReadOnly, harnessv2.MCPToolEffectConsequential} {
		t.Run(string(effect), func(t *testing.T) {
			f := newMCPApprovalFixture(t)
			f.request.Authorization.ApprovalPolicy.RequiredTools = nil
			f.request.Authorization.ToolPolicy.Tools[0].Effect = effect
			var err error
			f.request.Authorization.ApprovalPolicyDigest, err = harnessv2.CanonicalMCPApprovalPolicyDigest(f.request.Authorization.ApprovalPolicy)
			require.NoError(t, err)
			f.profile.ApprovalPolicyDigest = f.request.Authorization.ApprovalPolicyDigest
			f.request.Authorization.ToolPolicy.DescriptorDigest, err = harnessv2.CanonicalMCPToolDescriptorDigest(f.request.Authorization.ToolPolicy.Tools)
			require.NoError(t, err)
			f.request.Metadata.RequestDigest, err = harnessv2.CanonicalRequestDigest(f.request)
			require.NoError(t, err)
			submittingMCPApproval(t, f)
			reads := interceptMCPAdmissionReads(f, func(context.Context, string) (*store.PromptAttempt, error) {
				return nil, errors.New("simulated automatic-call read outage")
			})
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			requireMCPAdmissionDenied(t, f, f.startContext(ctx, f.request))
			require.EqualValues(t, 1, reads.calls.Load())
			require.NoError(t, ctx.Err())
		})
	}
}

func TestMCPApprovalAdmissionRevokedLeaseSkipsDurableRead(t *testing.T) {
	for _, reason := range []string{"cancelled", "released", "expired"} {
		t.Run(reason, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				request := approvalLeaseCall(t)
				if reason == "expired" {
					request.Authorization.ExpiresAt = time.Now().UTC().Add(time.Second)
					request.Metadata.ExpiresAt = request.Authorization.ExpiresAt
				}
				leases := &ACPMCPPromptLeaseRegistry{}
				leaseCtx, cancel := context.WithCancel(t.Context())
				defer cancel()
				lease := registerApprovalLease(t, leases, leaseCtx, request)
				reads := &mcpAdmissionAvailabilityStore{read: func(context.Context, string) (*store.PromptAttempt, error) {
					return nil, errors.New("simulated continuing read outage")
				}}
				authorizer := DurableACPMCPPromptAuthorizer{Attempts: reads, PromptLeases: leases}
				require.ErrorIs(t, authorizer.AuthorizeACPMCPPrompt(t.Context(), request), errACPMCPAuthorityUnavailable)
				require.EqualValues(t, 1, reads.calls.Load())
				switch reason {
				case "cancelled":
					cancel()
				case "released":
					lease.release()
				case "expired":
					time.Sleep(time.Second)
				}
				for range 2 {
					require.ErrorIs(t, authorizer.AuthorizeACPMCPPrompt(t.Context(), request), errACPMCPPromptLeaseInactive)
				}
				require.EqualValues(t, 1, reads.calls.Load(), "revocation must be checked before any further backend read")
			})
		})
	}
}

func TestMCPApprovalAdmissionRechecksLeaseAfterRead(t *testing.T) {
	f := newMCPApprovalFixture(t)
	attempts, cancelLease := submittingMCPApproval(t, f)
	attempt := *attempts.current.Load()
	attempt.ExecutionState = store.PromptExecutionAccepted
	attempts.current.Store(&attempt)
	reads := interceptMCPAdmissionReads(f, func(ctx context.Context, id string) (*store.PromptAttempt, error) {
		cancelLease()
		return attempts.GetPromptAttempt(ctx, id)
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	requireMCPAdmissionDenied(t, f, f.startContext(ctx, f.request))
	require.EqualValues(t, 1, reads.calls.Load(), "a successful read must not hide concurrent local revocation")
	require.NoError(t, ctx.Err())
}
