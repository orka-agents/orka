package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// A relative timer may have elapsed while a backward wall-clock step leaves
// the controller's wire deadline in the future. Shortening only the timer
// models that mismatch without mutating a global clock or the absolute bound.
func TestRuntimeSessionLeaseEarlyCallbackRearmsAbsoluteExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		session, peer := newLeaseTestRuntimeSession(t)
		deadline := time.Now().UTC().Add(2 * time.Second)
		run, err := session.StartPromptWithLeaseDeadline(t.Context(), "clock-step", "sha256:clock-step", []ContentBlock{Text("wait")}, deadline)
		if err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(peer)
		request, err := readTestMessage(reader)
		if err != nil {
			t.Fatal(err)
		}
		<-run.Events
		cancelled := make(chan time.Time, 1)
		peerDone := make(chan error, 1)
		go func() {
			message, err := readTestMessage(reader)
			if err != nil {
				peerDone <- err
				return
			}
			if message.Method != MethodSessionCancel {
				peerDone <- errors.New("expected session/cancel")
				return
			}
			cancelled <- time.Now().UTC()
			peerDone <- writeTestMessage(peer, map[string]any{
				"jsonrpc": "2.0", "id": request.ID,
				"result": map[string]any{"stopReason": StopReasonCancelled},
			})
		}()
		session.mu.Lock()
		active := session.active
		active.lease.Stop()
		session.mu.Unlock()
		callback := leaseTestExpiryCallback(session, active)
		session.mu.Lock()
		active.lease = time.AfterFunc(time.Second, callback)
		session.mu.Unlock()
		time.Sleep(2*time.Second - time.Nanosecond)
		synctest.Wait()
		select {
		case at := <-cancelled:
			t.Fatalf("early timer cancelled at %v before absolute expiry %v", at, deadline)
		default:
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if at := <-cancelled; !at.Equal(deadline) {
			t.Fatalf("rearmed cancellation at %v, want exact expiry %v", at, deadline)
		}
		if err := <-peerDone; err != nil {
			t.Fatal(err)
		}
		if result := <-run.Result; result.Outcome != PromptOutcomeCancelled || !result.SettledAt.Equal(deadline) {
			t.Fatalf("rearmed result = %#v, want cancellation at absolute expiry", result)
		}
	})
}

func TestRuntimeSessionLeaseQueuedCallbackCannotCancelRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		session, active := newLeaseTestActivePrompt(time.Now().UTC().Add(2 * time.Second))
		session.config.CancelGrace = time.Second
		callback := leaseTestExpiryCallback(session, active)
		release := make(chan struct{})
		active.lease = time.AfterFunc(0, func() { <-release; callback() })
		synctest.Wait() // The relative timer fired, but its callback is queued.
		renewed := time.Now().UTC().Add(3 * time.Second)
		if err := session.RenewPromptLeaseUntil(active.id, renewed); err != nil {
			session.finishPrompt(active, PromptResult{Outcome: PromptOutcomeCompleted})
			close(release)
			synctest.Wait()
			t.Fatalf("still-live absolute authority was not renewable: %v", err)
		}
		defer active.lease.Stop()
		close(release)
		synctest.Wait()
		session.mu.Lock()
		defer session.mu.Unlock()
		if active.cancelRequested || !active.leaseDeadline.Equal(renewed) {
			t.Fatal("queued old callback cancelled renewed authority")
		}
	})
}

func TestRuntimeSessionLeaseStaleCallbackCannotCancelReplacement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		session, old := newLeaseTestActivePrompt(time.Now().UTC().Add(time.Second))
		callback := leaseTestExpiryCallback(session, old)
		session.finishPrompt(old, PromptResult{Outcome: PromptOutcomeCompleted})
		_, replacement := newLeaseTestActivePrompt(time.Now().UTC().Add(2 * time.Second))
		session.active = replacement // Even identical IDs cannot revive an old callback.
		callbackDone := make(chan struct{})
		go func() { callback(); close(callbackDone) }()
		synctest.Wait()
		cancelled := replacement.cancelRequested
		session.finishPrompt(replacement, PromptResult{Outcome: PromptOutcomeCompleted})
		<-callbackDone
		if cancelled {
			t.Fatal("old prompt callback cancelled replacement authority")
		}
	})
}

func TestRuntimeSessionLeaseCancellationDecisionFencesRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		session, peer := newLeaseTestRuntimeSession(t)
		deadline := time.Now().UTC().Add(2 * time.Second)
		run, err := session.StartPromptWithLeaseDeadline(t.Context(), "cancel-decision", "sha256:cancel-decision", []ContentBlock{Text("wait")}, deadline)
		if err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(peer)
		request, err := readTestMessage(reader)
		if err != nil {
			t.Fatal(err)
		}
		<-run.Events
		cancelDone := make(chan struct{})
		go func() {
			_, _ = session.CancelPrompt(t.Context(), "cancel-decision")
			close(cancelDone)
		}()
		synctest.Wait() // Cancellation is decided; courtesy cancel is blocked on the pipe.
		err = session.RenewPromptLeaseUntil("cancel-decision", deadline.Add(time.Second))
		if _, ok := errors.AsType[*StalePromptError](err); !ok {
			t.Errorf("renewal after cancellation decision = %v, want stale authority", err)
		}
		message, err := readTestMessage(reader)
		if err != nil || message.Method != MethodSessionCancel {
			t.Fatalf("courtesy cancel = %#v, error = %v", message, err)
		}
		if err := writeTestMessage(peer, map[string]any{
			"jsonrpc": "2.0", "id": request.ID,
			"result": map[string]any{"stopReason": StopReasonCancelled},
		}); err != nil {
			t.Fatal(err)
		}
		<-run.Result
		<-cancelDone
	})
}

func TestRuntimeSessionLeaseFiredTimerCannotRenewExpiredAuthority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		deadline := time.Now().UTC().Add(time.Second)
		session, active := newLeaseTestActivePrompt(deadline)
		release := make(chan struct{})
		active.lease = time.AfterFunc(time.Second, func() { <-release })
		defer close(release)
		time.Sleep(time.Second)
		synctest.Wait()
		err := session.RenewPromptLeaseUntil(active.id, deadline.Add(time.Minute))
		if _, ok := errors.AsType[*StalePromptError](err); !ok || !active.leaseDeadline.Equal(deadline) {
			t.Fatalf("expired fired-timer renewal = %v, deadline = %v", err, active.leaseDeadline)
		}
	})
}

// A session/cancel notification has no prompt identity. Even after the old
// result is published, replacement admission must wait for a delayed write.
func TestRuntimeSessionLeasePendingCancelFencesReplacement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		session, peer := newLeaseTestRuntimeSession(t)
		client := session.process.Client()
		gate := &leaseTestCancelWriter{Writer: client.writer, entered: make(chan struct{}), release: make(chan struct{})}
		client.writer = gate
		released := false
		defer func() {
			if !released {
				close(gate.release)
			}
		}()
		deadline := time.Now().UTC().Add(time.Second)
		run, err := session.StartPromptWithLeaseDeadline(t.Context(), "old", "sha256:old", []ContentBlock{Text("wait")}, deadline)
		if err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(peer)
		request, err := readTestMessage(reader)
		if err != nil {
			t.Fatal(err)
		}
		<-run.Events

		// Delay the real courtesy write before bytes reach the peer. Its
		// writeMu remains held; the independent read loop can still settle.
		time.Sleep(time.Second)
		<-gate.entered
		synctest.Wait()
		session.mu.Lock()
		decided := session.active.cancelRequested
		session.mu.Unlock()
		if !decided {
			t.Fatal("expiry did not commit cancellation")
		}
		if err := writeTestMessage(peer, map[string]any{
			"jsonrpc": "2.0", "id": request.ID,
			"result": map[string]any{"stopReason": StopReasonEndTurn},
		}); err != nil {
			t.Fatal(err)
		}
		if result := <-run.Result; result.Outcome != PromptOutcomeCancelled || !result.Accepted {
			t.Fatalf("old prompt settlement = %#v, want accepted cancellation", result)
		}
		synctest.Wait()
		// The expiry caller's two-grace context ends while the writer is
		// still blocked. Caller cancellation must not release admission.
		time.Sleep(2 * session.config.CancelGrace)
		synctest.Wait()
		if _, err := session.StartPromptWithLeaseDeadline(t.Context(), "new", "sha256:new", []ContentBlock{Text("replacement")}, time.Now().Add(time.Minute)); err == nil {
			t.Fatal("replacement admitted while old session/cancel write was pending")
		} else if err.Error() != "runtime session has a pending prompt cancellation write" {
			t.Fatalf("replacement rejected for an unrelated reason: %v", err)
		}
		close(gate.release)
		released = true
		cancelMessage, err := readTestMessage(reader)
		if err != nil || cancelMessage.Method != MethodSessionCancel {
			t.Fatalf("old courtesy notification = %#v, error = %v", cancelMessage, err)
		}
		synctest.Wait() // The full courtesy write has completed.
		replacement, err := session.StartPromptWithLeaseDeadline(t.Context(), "new", "sha256:new", []ContentBlock{Text("replacement")}, time.Now().Add(time.Minute))
		if err != nil {
			t.Fatalf("replacement rejected after old cancel finished: %v", err)
		}
		newRequest, err := readTestMessage(reader)
		if err != nil || newRequest.Method != MethodSessionPrompt {
			t.Fatalf("replacement request = %#v, error = %v", newRequest, err)
		}
		<-replacement.Events
		// A subsequent real notification is an ordered wire barrier: any
		// leftover old cancellation would be observed before this message.
		barrierDone := make(chan error, 1)
		go func() { barrierDone <- client.Notify(context.Background(), "test/barrier", nil) }()
		message, err := readTestMessage(reader)
		if err != nil || message.Method != "test/barrier" {
			t.Fatalf("old cancel reached replacement: message = %#v, error = %v", message, err)
		}
		if err := <-barrierDone; err != nil {
			t.Fatal(err)
		}
		if err := writeTestMessage(peer, map[string]any{
			"jsonrpc": "2.0", "id": newRequest.ID,
			"result": map[string]any{"stopReason": StopReasonEndTurn},
		}); err != nil {
			t.Fatal(err)
		}
		if result := <-replacement.Result; result.Outcome != PromptOutcomeCompleted || result.StopReason != StopReasonEndTurn {
			t.Fatalf("replacement settlement = %#v, want completed", result)
		}
	})
}

func TestRuntimeSessionLeaseBlockedCancelKeepsGraceBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		session, peer := newLeaseTestRuntimeSession(t)
		client := session.process.Client()
		gate := &leaseTestCancelWriter{Writer: client.writer, entered: make(chan struct{}), release: make(chan struct{})}
		client.writer = gate
		defer close(gate.release)
		run, err := session.StartPromptWithLeaseDeadline(t.Context(), "blocked", "sha256:blocked", []ContentBlock{Text("wait")}, time.Now().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		request, err := readTestMessage(bufio.NewReader(peer))
		if err != nil {
			t.Fatal(err)
		}
		<-run.Events
		started := time.Now()
		ctx, cancel := context.WithTimeout(t.Context(), 2*session.config.CancelGrace)
		defer cancel()
		cancelled := make(chan error, 1)
		go func() {
			_, err := session.CancelPrompt(ctx, "blocked")
			cancelled <- err
		}()
		<-gate.entered
		synctest.Wait()
		time.Sleep(session.config.CancelGrace)
		synctest.Wait() // The nil-process fixture's Stop returns without settlement.
		select {
		case err := <-cancelled:
			t.Fatalf("cancellation returned before its context bound: %v", err)
		default:
		}
		time.Sleep(session.config.CancelGrace)
		synctest.Wait()
		select {
		case err := <-cancelled:
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != 2*session.config.CancelGrace {
				t.Fatalf("bounded cancellation = %v at %v", err, time.Since(started))
			}
		default:
			t.Fatal("blocked courtesy write prevented bounded cancellation return")
		}
		// A response still settles normally while the write is blocked, so
		// neither grace escalation nor transport waiting holds the session lock.
		if err := writeTestMessage(peer, map[string]any{
			"jsonrpc": "2.0", "id": request.ID,
			"result": map[string]any{"stopReason": StopReasonEndTurn},
		}); err != nil {
			t.Fatal(err)
		}
		if result := <-run.Result; result.Outcome != PromptOutcomeCompleted {
			t.Fatalf("blocked-write settlement = %#v, want completed", result)
		}
	})
}

type leaseTestCancelWriter struct {
	io.Writer
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *leaseTestCancelWriter) Write(data []byte) (int, error) {
	var message rpcMessage
	if err := json.Unmarshal(data, &message); err != nil {
		return 0, err
	}
	if message.Method == MethodSessionCancel {
		w.once.Do(func() { close(w.entered) })
		<-w.release
	}
	return w.Writer.Write(data)
}

// Captures the callback's authority when the timer is armed.
func leaseTestExpiryCallback(session *RuntimeSession, active *activePrompt) func() {
	version := active.leaseVersion
	return func() { session.expirePrompt(active, version) }
}
