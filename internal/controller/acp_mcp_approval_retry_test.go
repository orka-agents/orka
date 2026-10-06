package controller

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

type approvalRetryReadFault struct {
	active atomic.Bool
	reads  chan struct{}
}

func (f *approvalRetryReadFault) readError() error {
	if !f.active.Load() {
		return nil
	}
	select {
	case f.reads <- struct{}{}:
	default:
	}
	return errors.New("simulated approval read outage")
}

type approvalRetryEventStore struct {
	store.TaskDataTransactionStore
	store.DeduplicatingExecutionEventStore
	fault *approvalRetryReadFault
}

func (s approvalRetryEventStore) ListExecutionEvents(ctx context.Context, filter store.ExecutionEventFilter) ([]store.ExecutionEvent, error) {
	if err := s.fault.readError(); err != nil {
		return nil, err
	}
	return s.DeduplicatingExecutionEventStore.ListExecutionEvents(ctx, filter)
}

type approvalRetryEffectStore struct {
	store.ExternalEffectStore
	fault *approvalRetryReadFault
}

func (s approvalRetryEffectStore) GetExternalEffect(ctx context.Context, id string) (*store.ExternalEffect, error) {
	if err := s.fault.readError(); err != nil {
		return nil, err
	}
	return s.ExternalEffectStore.GetExternalEffect(ctx, id)
}

func configureApprovalRetryReadFault(t *testing.T, f *mcpApprovalFixture, source string) *approvalRetryReadFault {
	t.Helper()
	fault := &approvalRetryReadFault{reads: make(chan struct{}, 2)}
	switch source {
	case "decision":
		f.broker.ApprovalEvents = approvalRetryEventStore{DeduplicatingExecutionEventStore: f.events, TaskDataTransactionStore: f.events, fault: fault}
	case "receipt":
		f.broker.Effects = approvalRetryEffectStore{ExternalEffectStore: f.broker.Effects, fault: fault}
	case "secret":
		f.broker.ApprovalSecrets.(*k8sfake.Clientset).PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
			err := fault.readError()
			return err != nil, nil, err
		})
	default:
		t.Fatalf("unknown approval read source %q", source)
	}
	return fault
}

func TestMCPApprovalReadDeadlineBoundsBlockingReads(t *testing.T) {
	for _, test := range []struct {
		name          string
		firstOutage   bool
		cancelParent  bool
		parentTimeout time.Duration
		want          error
	}{
		{name: "first read", want: errACPMCPTaskExpired},
		{name: "retry read", firstOutage: true, want: errACPMCPTaskExpired},
		{name: "parent cancellation", cancelParent: true, want: context.Canceled},
		{name: "parent deadline", parentTimeout: 50 * time.Millisecond, want: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			parentTimeout := time.Second
			if test.parentTimeout != 0 {
				parentTimeout = test.parentTimeout
			}
			ctx, cancel := context.WithTimeout(t.Context(), parentTimeout)
			defer cancel()
			call := &acpMCPApprovalCall{ExpiresAt: time.Now().UTC().Add(150 * time.Millisecond)}
			broker := &ACPMCPBroker{ApprovalPollInterval: time.Millisecond}
			reads := 0
			err := broker.readUnstartedApproval(ctx, call, func(readCtx context.Context) error {
				reads++
				if test.firstOutage && reads == 1 {
					return acpMCPAuthorityReadError(errors.New("simulated read outage"))
				}
				if test.cancelParent {
					cancel()
				}
				<-readCtx.Done()
				return readCtx.Err()
			})
			require.ErrorIs(t, err, test.want)
			wantReads := 1
			if test.firstOutage {
				wantReads++
			}
			require.Equal(t, wantReads, reads, "a cancelled or expired read must never be retried")
			if errors.Is(test.want, errACPMCPTaskExpired) {
				require.NoError(t, ctx.Err(), "the original approval deadline must bound every read before the caller deadline")
			}
		})
	}
}

func TestMCPApprovalStorageReadRecoveryContinuesOriginalCall(t *testing.T) {
	for _, source := range []string{"decision", "receipt"} {
		t.Run(source, func(t *testing.T) {
			f := newMCPApprovalFixture(t)
			fault := configureApprovalRetryReadFault(t, f, source)
			done := f.start(f.request)
			pending := f.pending()
			fault.active.Store(true)
			requireApprovalReadRetrying(t, f, pending.ID, approvals.StatusPending, done, fault.reads)
			f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
			requireApprovalReadRetrying(t, f, pending.ID, approvals.StatusApproved, done, fault.reads)
			fault.active.Store(false)
			result := awaitMCPApprovalResult(t, done)
			require.False(t, result.IsError || result.Replayed)
			require.EqualValues(t, 1, f.count.Load())
			f.reopen()
			replay := awaitMCPApprovalResult(t, f.start(f.request))
			require.True(t, replay.Replayed)
			require.JSONEq(t, string(result.Result), string(replay.Result))
			require.EqualValues(t, 1, f.count.Load())
		})
	}
}

func TestMCPApprovalReadRetriesKeepOriginalExpiry(t *testing.T) {
	for _, source := range []string{"decision", "receipt", "secret"} {
		t.Run(source, func(t *testing.T) {
			f := newMCPApprovalFixture(t)
			f.broker.ApprovalWaitTimeout = time.Second
			fault := configureApprovalRetryReadFault(t, f, source)
			done := f.start(f.request)
			pending := f.pending()
			fault.active.Store(true)
			f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
			requireApprovalReadRetrying(t, f, pending.ID, approvals.StatusApproved, done, fault.reads)
			if source == "receipt" || source == "decision" {
				// Unreadable receipts cannot rule out another execution claim.
				// Unreadable history cannot prove a retained request for the
				// expiry projection. Neither outage may be treated as absence.
				select {
				case response := <-done:
					require.Equal(t, http.StatusServiceUnavailable, response.Code)
				case <-time.After(3 * time.Second):
					t.Fatal("storage outage extended the original approval wait")
				}
			} else {
				result := awaitMCPApprovalResult(t, done)
				require.True(t, result.IsError)
				require.JSONEq(t, string(acpApprovalError(pending.ID, acpApprovalCodeExpired)), string(result.Result))
			}
			require.False(t, time.Now().UTC().Before(*pending.ExpiresAt))
			require.WithinDuration(t, *pending.ExpiresAt, time.Now().UTC(), 2*time.Second)
			require.Zero(t, f.count.Load())
			fault.active.Store(false)
			f.reopen()
			replay := awaitMCPApprovalResult(t, f.start(f.request))
			require.True(t, replay.IsError)
			require.JSONEq(t, string(acpApprovalError(pending.ID, acpApprovalCodeExpired)), string(replay.Result))
			require.Zero(t, f.count.Load(), "read recovery must not extend the original approval deadline")
		})
	}
}

func TestMCPApprovalSecretReadRetriesStopOnCancellation(t *testing.T) {
	for _, cancellation := range []string{"caller", "prompt"} {
		t.Run(cancellation, func(t *testing.T) {
			f := newMCPApprovalFixture(t)
			fault := configureApprovalRetryReadFault(t, f, "secret")
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			done := f.startContext(ctx, f.request)
			pending := f.pending()
			fault.active.Store(true)
			f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
			requireApprovalReadRetrying(t, f, pending.ID, approvals.StatusApproved, done, fault.reads)
			if cancellation == "caller" {
				cancel()
				requireApprovalReadUnavailable(t, f, pending.ID, approvals.StatusApproved, done)
			} else {
				f.active.Store(false)
				result := awaitMCPApprovalResult(t, done)
				require.True(t, result.IsError)
				require.JSONEq(t, string(acpApprovalError(pending.ID, acpApprovalCodeStale)), string(result.Result))
			}
			require.Zero(t, f.count.Load())
		})
	}
}

type approvalAmbiguousClaimStore struct {
	store.ExternalEffectStore
	claims atomic.Int32
}

func (s *approvalAmbiguousClaimStore) TransitionExternalEffect(ctx context.Context, change store.ExternalEffectTransition) (*store.ExternalEffect, error) {
	effect, err := s.ExternalEffectStore.TransitionExternalEffect(ctx, change)
	if err != nil || change.NewState != store.ExternalEffectInFlight {
		return effect, err
	}
	s.claims.Add(1)
	// Even an error classified as unavailable must not retry a mutation that
	// committed but lost its response.
	return nil, acpMCPAuthorityReadError(errors.New("simulated lost claim response"))
}

func TestMCPApprovalAmbiguousClaimIsNotRetried(t *testing.T) {
	f := newMCPApprovalFixture(t)
	effects := &approvalAmbiguousClaimStore{ExternalEffectStore: f.broker.Effects}
	f.broker.Effects = effects
	done := f.start(f.request)
	pending := f.pending()
	f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
	select {
	case response := <-done:
		require.Equal(t, http.StatusServiceUnavailable, response.Code)
	case <-time.After(3 * time.Second):
		t.Fatal("ambiguous claim entered the pending-read retry loop")
	}
	require.EqualValues(t, 1, effects.claims.Load())
	require.Zero(t, f.count.Load())
	identity := store.ExternalEffectIdentity{Kind: acpMCPToolEffectKind, Namespace: f.request.Namespace,
		AggregateID: string(f.request.Authorization.RuntimeSessionUID), OperationID: string(f.request.Metadata.OperationID)}
	id, err := identity.CanonicalID()
	require.NoError(t, err)
	claimed, err := f.events.GetExternalEffect(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, store.ExternalEffectInFlight, claimed.State)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	select {
	case response := <-f.startContext(ctx, f.request):
		require.Equal(t, http.StatusServiceUnavailable, response.Code)
	case <-time.After(3 * time.Second):
		t.Fatal("redelivery did not stop at its caller deadline")
	}
	require.EqualValues(t, 1, effects.claims.Load())
	require.Zero(t, f.count.Load(), "an ambiguous claim must never be executed by a later delivery")
}
