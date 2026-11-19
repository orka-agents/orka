/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func TestNextACPOutboxClaim(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	idle := 10 * time.Second
	for _, tc := range []struct {
		name string
		pass acpOutboxPass
		err  error
		want time.Time
	}{
		{name: "idle pass waits for the idle interval", want: now.Add(idle)},
		{name: "a failed pass retries on the next tick", err: errors.New("unavailable"), want: now},
		{name: "a full claim may have left more due", pass: acpOutboxPass{claimed: defaultACPOutboxLimit}, want: now},
		{name: "a scheduled retry comes due first", pass: acpOutboxPass{claimed: 1, nextRetry: now.Add(2 * time.Second)}, want: now.Add(2 * time.Second)},
		{name: "a retry after the idle interval waits for the interval", pass: acpOutboxPass{claimed: 1, nextRetry: now.Add(time.Minute)}, want: now.Add(idle)},
	} {
		if got := nextACPOutboxClaim(now, tc.pass, tc.err, idle); !got.Equal(tc.want) {
			t.Errorf("%s: next claim = %s, want %s", tc.name, got, tc.want)
		}
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
