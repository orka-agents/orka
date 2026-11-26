/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func TestACPOutboxScheduleNext(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	idle := 10 * time.Second
	var schedule acpOutboxSchedule
	if got := schedule.next(now, acpOutboxPass{claimedAt: now}, nil, idle); !got.Equal(now.Add(idle)) {
		t.Fatalf("idle pass: next = %s, want the idle interval", got)
	}
	if got := schedule.next(now, acpOutboxPass{claimedAt: now}, errors.New("unavailable"), idle); !got.Equal(now) {
		t.Fatalf("failed pass: next = %s, want the next tick", got)
	}
	full := acpOutboxPass{claimedAt: now, claimed: make([]string, defaultACPOutboxLimit)}
	if got := schedule.next(now, full, nil, idle); !got.Equal(now) {
		t.Fatalf("full claim: next = %s, want the next tick", got)
	}
}

// An enqueue can wake a pass before a scheduled retry is due. That pass
// claims nothing and must keep the retry's deadline rather than fall back to
// the idle interval.
func TestACPOutboxScheduleKeepsARetryAcrossAnEarlyPass(t *testing.T) {
	start := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	idle := 10 * time.Second
	var schedule acpOutboxSchedule
	first := acpOutboxPass{claimedAt: start, claimed: []string{"a"}, retries: map[string]time.Time{"a": start.Add(2 * time.Second)}}
	schedule.observe(first, nil)
	if got := schedule.next(start, first, nil, idle); !got.Equal(start.Add(2 * time.Second)) {
		t.Fatalf("after scheduling a retry: next = %s, want the retry", got)
	}
	early := acpOutboxPass{claimedAt: start.Add(500 * time.Millisecond)}
	schedule.observe(early, nil)
	if got := schedule.next(early.claimedAt, early, nil, idle); !got.Equal(start.Add(2 * time.Second)) {
		t.Fatalf("after an early enqueue pass: next = %s, want the outstanding retry %s", got, start.Add(2*time.Second))
	}
}

// A batch can schedule several retries. When the earliest is claimed and
// delivered, the later deadline is still outstanding.
func TestACPOutboxScheduleKeepsLaterRetriesOfABatch(t *testing.T) {
	start := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	idle := 10 * time.Second
	var schedule acpOutboxSchedule
	batch := acpOutboxPass{claimedAt: start, claimed: []string{"a", "b"}, retries: map[string]time.Time{
		"a": start.Add(time.Second), "b": start.Add(4 * time.Second),
	}}
	schedule.observe(batch, nil)
	retryA := acpOutboxPass{claimedAt: start.Add(time.Second), claimed: []string{"a"}} // delivered
	schedule.observe(retryA, nil)
	if got := schedule.next(retryA.claimedAt, retryA, nil, idle); !got.Equal(start.Add(4 * time.Second)) {
		t.Fatalf("after the earliest retry succeeded: next = %s, want b's retry %s", got, start.Add(4*time.Second))
	}
	retryB := acpOutboxPass{claimedAt: start.Add(4 * time.Second), claimed: []string{"b"}} // delivered
	schedule.observe(retryB, nil)
	if got := schedule.next(retryB.claimedAt, retryB, nil, idle); !got.Equal(retryB.claimedAt.Add(idle)) {
		t.Fatalf("after every retry settled: next = %s, want the idle interval", got)
	}
	// A due deadline a non-full pass did not return belongs to a projection
	// that is no longer pending, and is dropped.
	schedule.observe(acpOutboxPass{claimedAt: start, retries: map[string]time.Time{"gone": start.Add(time.Second)}}, nil)
	late := acpOutboxPass{claimedAt: start.Add(2 * time.Second)}
	schedule.observe(late, nil)
	if got := schedule.next(late.claimedAt, late, nil, idle); !got.Equal(late.claimedAt.Add(idle)) {
		t.Fatalf("stale deadline kept the projector busy: next = %s", got)
	}
}

// countingOutboxStore records claims; it never has a projection to deliver.
type countingOutboxStore struct {
	store.OutboxProjectionStore
	mu     sync.Mutex
	claims int
}

func (s *countingOutboxStore) ClaimOutboxProjections(context.Context, store.ClaimOutboxProjectionsRequest) ([]store.OutboxProjection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims++
	return nil, nil
}

func (s *countingOutboxStore) claimCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claims
}

// Every claim takes the controller-epoch mutation interlock, so an idle
// projector claims once at start and then only on the idle interval, while an
// enqueue wakes it to claim at once.
func TestACPOutboxProjectorClaimsOnEnqueueNotEveryTick(t *testing.T) {
	epochs, _ := settledDeliveryTestEpochs(t)
	claims := &countingOutboxStore{}
	projector := &ACPOutboxProjector{
		Client: newUnitReconciler(newTestScheme()).Client, Store: claims, Epochs: epochs, WorkerID: "schedule-test",
		Interval: 20 * time.Millisecond, IdleClaimInterval: time.Hour,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- projector.Start(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	waitForClaims := func(step string, want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for claims.claimCount() < want {
			if time.Now().After(deadline) {
				t.Fatalf("%s: claims = %d, want %d", step, claims.claimCount(), want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitForClaims("start", 1)
	time.Sleep(200 * time.Millisecond) // ten ticks
	if got := claims.claimCount(); got != 1 {
		t.Fatalf("an idle projector claimed %d times over ten ticks, want once", got)
	}
	wakeACPOutboxProjectors()
	waitForClaims("enqueue wakeup", 2)
}

// enqueueOutboxStore accepts one new projection.
type enqueueOutboxStore struct {
	store.OutboxProjectionStore
}

func (enqueueOutboxStore) GetOutboxProjection(context.Context, string) (*store.OutboxProjection, error) {
	return nil, store.ErrNotFound
}

func (enqueueOutboxStore) EnqueueOutboxProjection(_ context.Context, projection *store.OutboxProjection, _ store.ControllerEpochFence) (*store.OutboxProjection, error) {
	return projection, nil
}

func TestEnqueuedTerminalProjectionWakesProjectors(t *testing.T) {
	wake, unsubscribe := subscribeACPOutboxWakeups()
	defer unsubscribe()
	task := settledDeliveryTestTask(1)
	if err := enqueueDurableTaskTerminalProjection(context.Background(), enqueueOutboxStore{},
		store.ControllerEpochFence{Name: "test", Epoch: 1, HolderID: "holder"}, &task, taskTerminalProjection{
			Namespace: task.Namespace, Task: task.Name, TaskUID: string(task.UID), Attempt: 1,
		}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("an enqueued terminal projection did not wake the projector")
	}
}

// A Session turn's finalization, and a resumed finalization, activate its
// terminal projection through the store; both wake the projector.
func TestSessionTurnFinalizationWakesProjectors(t *testing.T) {
	ctx := context.Background()
	s, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "finalize-wake.db"))
	defer closeStore()
	continuity := newACPSessionTestContinuity(t, s, ACPBootstrapLimits{})
	control := ensureACPSessionForTest(t, continuity, fence, "finalize-wake")
	turn, attempt := openACPSessionTurnForTest(t, continuity, s, fence, control, "task-finalize-wake", "prompt-finalize-wake", "work")
	completeACPAttemptExecutionForTest(t, s, fence, attempt, true)

	wake, unsubscribe := subscribeACPOutboxWakeups()
	defer unsubscribe()
	expectWake := func(step string) {
		t.Helper()
		select {
		case <-wake:
		default:
			t.Fatalf("%s did not wake the projector", step)
		}
	}
	finalized, err := continuity.FinalizeOutcomeUnknown(ctx, ACPFinalizeOutcomeUnknownRequest{
		SessionTurn: *turn, Fence: fence, Reason: "ambiguous prompt acceptance",
		Projection: acpSessionProjectionForTest("finalize-wake", "OutcomeUnknown"),
	})
	if err != nil {
		t.Fatal(err)
	}
	expectWake("turn finalization")

	resumed := *turn
	resumed.Turn = finalized.Turn
	if _, err := continuity.ResumeSessionTurnFinalization(ctx, ACPResumeSessionTurnFinalizationRequest{
		SessionTurn: resumed, Fence: fence,
	}); err != nil {
		t.Fatal(err)
	}
	expectWake("resumed turn finalization")
}

// partialBatchOutboxStore claims two projections that cannot be delivered.
// Completing the first commits its retry; completing the second fails.
type partialBatchOutboxStore struct {
	store.OutboxProjectionStore
	claimed bool
}

func (s *partialBatchOutboxStore) ClaimOutboxProjections(context.Context, store.ClaimOutboxProjectionsRequest) ([]store.OutboxProjection, error) {
	if s.claimed {
		return nil, nil
	}
	s.claimed = true
	return []store.OutboxProjection{
		{ID: "first", ProjectionKind: "unsupported", Attempts: 1, Version: 1},
		{ID: "second", ProjectionKind: "unsupported", Attempts: 1, Version: 1},
	}, nil
}

func (s *partialBatchOutboxStore) CompleteOutboxProjection(_ context.Context, request store.CompleteOutboxProjectionRequest) (*store.OutboxProjection, error) {
	if request.ID == "second" {
		return nil, errors.New("store unavailable")
	}
	return &store.OutboxProjection{ID: request.ID, State: request.NewState, AvailableAt: request.AvailableAt}, nil
}

// A batch that fails after committing a retry still reports that retry, and
// the empty claim that follows keeps its deadline rather than falling back
// to the idle interval.
func TestACPOutboxKeepsRetriesCommittedBeforeABatchFails(t *testing.T) {
	epochs, _ := settledDeliveryTestEpochs(t)
	projector := &ACPOutboxProjector{
		Client: newUnitReconciler(newTestScheme()).Client, Store: &partialBatchOutboxStore{}, Epochs: epochs,
		WorkerID: "partial-batch", MaxAttempts: 10,
	}
	ctx := context.Background()
	var schedule acpOutboxSchedule
	idle := time.Hour

	failed, err := projector.projectPass(ctx)
	if err == nil {
		t.Fatal("the batch's second completion did not fail")
	}
	due, ok := failed.retries["first"]
	if !ok || len(failed.retries) != 1 {
		t.Fatalf("retries = %v, want only the committed retry of the first projection", failed.retries)
	}
	schedule.observe(failed, err)

	empty, err := projector.projectPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	schedule.observe(empty, nil)
	if got := schedule.next(empty.claimedAt, empty, nil, idle); !got.Equal(due) {
		t.Fatalf("after the empty claim: next = %s, want the committed retry %s", got, due)
	}
}

func TestACPOutboxScheduleKeepsDeadlinesAcrossAFailedPass(t *testing.T) {
	start := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	var schedule acpOutboxSchedule
	schedule.observe(acpOutboxPass{claimedAt: start, claimed: []string{"a"}, retries: map[string]time.Time{"a": start.Add(4 * time.Second)}}, nil)
	// A failed pass after a's deadline settles and prunes nothing.
	schedule.observe(acpOutboxPass{claimedAt: start.Add(5 * time.Second), retries: map[string]time.Time{"b": start.Add(6 * time.Second)}}, errors.New("unavailable"))
	if len(schedule.retries) != 2 {
		t.Fatalf("retries = %v, want both deadlines kept across the failed pass", schedule.retries)
	}
}
