package acp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"testing/synctest"
	"time"
)

// The proxy can revoke provider access exactly at expiry before the runtime's
// lease callback runs. A conclusive adapter error must not win that race and
// erase the authoritative lease cancellation.
func TestRuntimeSessionLeaseExpiryWinsDelayedWatchdog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		session, peer := newLeaseTestRuntimeSession(t)
		run, err := session.StartPromptWithLease(t.Context(), "prompt-expiry", "sha256:expiry", []ContentBlock{Text("wait")}, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		request, err := readTestMessage(bufio.NewReader(peer))
		if err != nil {
			t.Fatal(err)
		}
		if event := <-run.Events; event.Type != PromptEventAccepted {
			t.Fatalf("first event = %q, want accepted", event.Type)
		}
		// Stop only the callback, modeling an arbitrarily delayed timer
		// goroutine without changing the controller's lease deadline.
		session.mu.Lock()
		stopped := session.active.lease.Stop()
		session.mu.Unlock()
		if !stopped {
			t.Fatal("lease callback ran before the test held it")
		}
		time.Sleep(2 * time.Second)
		if err := writeTestMessage(peer, map[string]any{
			"jsonrpc": "2.0", "id": request.ID,
			"error": &RPCError{Code: -32603, Message: "provider request closed"},
		}); err != nil {
			t.Fatal(err)
		}
		result := <-run.Result
		if result.Outcome != PromptOutcomeCancelled || result.StopReason != StopReasonCancelled || !result.Accepted {
			t.Fatalf("expired prompt = %#v, want accepted cancellation", result)
		}
		tombstone, ok := session.Tombstone("prompt-expiry")
		if !ok || tombstone.Result != result {
			t.Fatalf("tombstone = %#v, want the same cancellation", tombstone)
		}
	})
}

func TestRuntimeSessionLeaseSettlementBoundaries(t *testing.T) {
	deadline := time.Now().Add(-time.Second)
	rpcErr := &RPCError{Code: -32603, Message: "adapter rejected the prompt"}
	tests := []struct {
		name       string
		receivedAt time.Time
		accepted   bool
		outcome    PromptOutcome
		stopReason StopReason
		err        error
		want       PromptOutcome
	}{
		{"failure before expiry", deadline.Add(-time.Nanosecond), true, PromptOutcomeFailed, "", rpcErr, PromptOutcomeFailed},
		{"completion before expiry", deadline.Add(-time.Nanosecond), true, PromptOutcomeCompleted, StopReasonEndTurn, nil, PromptOutcomeCompleted},
		{"failure at expiry", deadline, true, PromptOutcomeFailed, "", rpcErr, PromptOutcomeCancelled},
		{"failure after expiry", deadline.Add(time.Nanosecond), true, PromptOutcomeFailed, "", rpcErr, PromptOutcomeCancelled},
		{"completion after expiry", deadline.Add(time.Nanosecond), true, PromptOutcomeCompleted, StopReasonEndTurn, nil, PromptOutcomeCancelled},
		{"refusal after expiry", deadline.Add(time.Nanosecond), true, PromptOutcomeFailed, StopReasonRefusal, nil, PromptOutcomeCancelled},
		{"transport loss after expiry", deadline.Add(time.Nanosecond), true, PromptOutcomeOutcomeUnknown, "", io.EOF, PromptOutcomeOutcomeUnknown},
		{"failure before acceptance", deadline.Add(time.Nanosecond), false, PromptOutcomeFailed, "", context.DeadlineExceeded, PromptOutcomeFailed},
		{"missing receipt time", time.Time{}, true, PromptOutcomeFailed, "", rpcErr, PromptOutcomeFailed},
		{"missing outcome", deadline.Add(time.Nanosecond), true, "", "", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session, active := newLeaseTestActivePrompt(deadline)
			original := PromptResult{Outcome: tt.outcome, StopReason: tt.stopReason, Err: tt.err, Accepted: tt.accepted, SettledAt: tt.receivedAt}
			session.finishPrompt(active, original)
			got := <-active.result
			if got.Outcome != tt.want || got.Accepted != original.Accepted || !got.SettledAt.Equal(tt.receivedAt) {
				t.Fatalf("result = %#v, want outcome %q with the original acceptance and receipt", got, tt.want)
			}
			if tt.want == PromptOutcomeCancelled {
				if got.StopReason != StopReasonCancelled || got.Err != nil {
					t.Fatalf("cancellation = %#v", got)
				}
			} else if got != original {
				t.Fatalf("unexpired or unproven settlement changed: got %#v, want %#v", got, original)
			}
		})
	}
}

func TestRuntimeSessionLeasePreservesReceiptBeforeLockWait(t *testing.T) {
	deadline := time.Now().Add(-time.Second)
	session, active := newLeaseTestActivePrompt(deadline)
	original := PromptResult{
		Outcome: PromptOutcomeFailed, Accepted: true, SettledAt: deadline.Add(-time.Nanosecond),
		Err: &RPCError{Code: -32603, Message: "provider failed before expiry"},
	}
	// The response arrived before expiry, but settlement cannot acquire the
	// session lock until after the deadline. No wall-clock sleep is required.
	session.mu.Lock()
	entered := make(chan struct{})
	go func() {
		close(entered)
		session.finishPrompt(active, original)
	}()
	<-entered
	session.mu.Unlock()
	if got := <-active.result; got != original {
		t.Fatalf("receipt changed after lock wait: got %#v, want %#v", got, original)
	}
}

func TestRuntimeSessionLeaseExpiryPreservesEventOverflow(t *testing.T) {
	deadline := time.Now().Add(-time.Second)
	session, active := newLeaseTestActivePrompt(deadline)
	active.overflowed = true
	session.finishPrompt(active, PromptResult{
		Outcome: PromptOutcomeCompleted, StopReason: StopReasonEndTurn,
		Accepted: true, SettledAt: deadline.Add(time.Nanosecond),
	})
	got := <-active.result
	if got.Outcome != PromptOutcomeFailed || !errors.Is(got.Err, ErrPromptEventBufferOverflow) {
		t.Fatalf("lost events were hidden by expiry: %#v", got)
	}
}

func TestRuntimeSessionLeaseDeadlineRejectsExpiredAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		session, _ := newLeaseTestRuntimeSession(t)
		deadline := time.Now().Add(time.Second)
		time.Sleep(2 * time.Second)
		if _, err := session.StartPromptWithLeaseDeadline(t.Context(), "expired", "sha256:expired", []ContentBlock{Text("wait")}, deadline); err == nil {
			t.Fatal("expired absolute lease admitted a prompt")
		}
		if session.active != nil {
			t.Fatal("expired admission created prompt state")
		}
	})
}

func TestRuntimeSessionLeaseRenewalCannotReviveExpiredAuthority(t *testing.T) {
	for _, elapsed := range []time.Duration{time.Second, 2 * time.Second} {
		t.Run(elapsed.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				deadline := time.Now().Add(time.Second)
				session, active := newLeaseTestActivePrompt(deadline)
				// The timer is deliberately late: its Stop still succeeds even
				// after the exact authority deadline has passed.
				active.lease = time.AfterFunc(time.Hour, func() {})
				defer active.lease.Stop()
				time.Sleep(elapsed)
				err := session.RenewPromptLeaseUntil(active.id, time.Now().Add(time.Minute))
				var stale *StalePromptError
				if !errors.As(err, &stale) || !active.leaseDeadline.Equal(deadline) {
					t.Fatalf("expired renewal = %v, deadline = %v", err, active.leaseDeadline)
				}
			})
		})
	}
}

func TestRuntimeSessionLeaseRenewalControlsSettlementDeadline(t *testing.T) {
	for _, afterRenewed := range []bool{false, true} {
		name := "before renewed expiry"
		if afterRenewed {
			name = "at renewed expiry"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				deadline := time.Now().Add(time.Second)
				session, active := newLeaseTestActivePrompt(deadline)
				active.lease = time.AfterFunc(time.Hour, func() {})
				defer active.lease.Stop()
				renewed := deadline.Add(time.Second)
				if err := session.RenewPromptLeaseUntil(active.id, renewed); err != nil {
					t.Fatal(err)
				}
				if !active.leaseDeadline.Equal(renewed) {
					t.Fatal("renewal did not preserve the exact deadline")
				}
				received := deadline.Add(time.Nanosecond)
				want := PromptOutcomeFailed
				if afterRenewed {
					received, want = renewed, PromptOutcomeCancelled
				}
				session.finishPrompt(active, PromptResult{
					Outcome: PromptOutcomeFailed, Accepted: true, SettledAt: received,
					Err: &RPCError{Code: -32603, Message: "provider request closed"},
				})
				if got := <-active.result; got.Outcome != want {
					t.Fatalf("renewed settlement = %#v, want %q", got, want)
				}
			})
		})
	}
}

func TestRuntimeSessionLeaseRenewalMovesWatchdogToExactDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		session, peer := newLeaseTestRuntimeSession(t)
		deadline := time.Now().Add(time.Second)
		run, err := session.StartPromptWithLeaseDeadline(t.Context(), "renewed", "sha256:renewed", []ContentBlock{Text("wait")}, deadline)
		if err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(peer)
		request, err := readTestMessage(reader)
		if err != nil {
			t.Fatal(err)
		}
		<-run.Events
		cancelled := make(chan rpcMessage, 1)
		peerDone := make(chan error, 1)
		go func() {
			message, readErr := readTestMessage(reader)
			if readErr != nil {
				peerDone <- readErr
				return
			}
			cancelled <- message
			peerDone <- writeTestMessage(peer, map[string]any{
				"jsonrpc": "2.0", "id": request.ID,
				"result": map[string]any{"stopReason": StopReasonCancelled},
			})
		}()
		time.Sleep(500 * time.Millisecond)
		renewed := deadline.Add(time.Second)
		if err := session.RenewPromptLeaseUntil("renewed", renewed); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		select {
		case <-cancelled:
			t.Fatal("the original deadline cancelled a renewed prompt")
		default:
		}
		time.Sleep(time.Second)
		if message := <-cancelled; message.Method != MethodSessionCancel {
			t.Fatalf("watchdog sent %q, want session/cancel", message.Method)
		}
		if err := <-peerDone; err != nil {
			t.Fatal(err)
		}
		if result := <-run.Result; result.Outcome != PromptOutcomeCancelled || !result.SettledAt.Equal(renewed) {
			t.Fatalf("watchdog result = %#v, want cancellation at %v", result, renewed)
		}
	})
}

func newLeaseTestActivePrompt(deadline time.Time) (*RuntimeSession, *activePrompt) {
	active := &activePrompt{
		id: "prompt-lease", requestDigest: "sha256:lease", leaseDeadline: deadline,
		events: make(chan PromptEvent, 1), result: make(chan PromptResult, 1), done: make(chan struct{}),
		permissions: make(map[string]*pendingPermission),
	}
	return &RuntimeSession{active: active, tombstones: make(map[string]PromptTombstone)}, active
}

func newLeaseTestRuntimeSession(t *testing.T) (*RuntimeSession, net.Conn) {
	t.Helper()
	clientConn, peer := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = peer.Close()
	})
	client := NewClient(clientConn, clientConn, Options{})
	return &RuntimeSession{
		providerSessionID: "lease-provider-session",
		process:           &Process{client: client},
		config: RuntimeSessionConfig{
			CancelGrace:       time.Second,
			MaxBufferedEvents: 16,
		},
		tombstones: make(map[string]PromptTombstone),
	}, peer
}
