package acp

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestWaitPromptSettlementPreservesConclusiveResult(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		session, active := newLeaseTestActivePrompt(time.Now().Add(time.Minute))
		session.config.CancelGrace = time.Second
		done := make(chan error, 1)
		go func() { done <- session.WaitPromptSettlement(t.Context(), active.id) }()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("wait returned before settlement: %v", err)
		default:
		}
		if active.cancelRequested {
			t.Fatal("waiting requested another cancellation")
		}
		want := PromptResult{Outcome: PromptOutcomeFailed, Accepted: true, SettledAt: time.Now(),
			Err: &RPCError{Code: -32603, Message: "independent adapter failure"}}
		session.finishPrompt(active, want)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if got := <-active.result; got != want {
			t.Fatalf("joining changed the real result: %#v", got)
		}
	})
}

func TestWaitPromptSettlementIsBoundedAndDoesNotStopTheChild(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		session, active := newLeaseTestActivePrompt(time.Now().Add(time.Minute))
		session.config.CancelGrace = time.Second
		started := time.Now()
		if err := session.WaitPromptSettlement(t.Context(), active.id); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("wait returned %v, want its bounded deadline", err)
		}
		if time.Since(started) != 2*time.Second || active.settled || active.cancelRequested || session.active != active {
			t.Fatal("the response hold exceeded its bound or changed prompt state")
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := session.WaitPromptSettlement(ctx, active.id); !errors.Is(err, context.Canceled) {
			t.Fatalf("downstream disconnect returned %v", err)
		}
	})
}

func TestWaitPromptSettlementNeverJoinsAnotherPrompt(t *testing.T) {
	session, active := newLeaseTestActivePrompt(time.Now().Add(time.Minute))
	if err := session.WaitPromptSettlement(t.Context(), "another-prompt"); err == nil {
		t.Fatal("unknown prompt was treated as settled")
	}
	session.tombstones["previous-prompt"] = PromptTombstone{PromptID: "previous-prompt"}
	if err := session.WaitPromptSettlement(t.Context(), "previous-prompt"); err != nil {
		t.Fatal(err)
	}
	if session.active != active || active.cancelRequested || active.settled {
		t.Fatal("waiting for the previous prompt changed its successor")
	}
}
