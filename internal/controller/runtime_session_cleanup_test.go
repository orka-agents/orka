package controller

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/orka/internal/harness"
)

type fakeRuntimeSessionCleanupStore struct {
	called  atomic.Int32
	now     time.Time
	deleted []harness.RuntimeSession
	err     error
}

func (f *fakeRuntimeSessionCleanupStore) CreateRuntimeSession(context.Context, *harness.RuntimeSession) error {
	return nil
}
func (f *fakeRuntimeSessionCleanupStore) GetRuntimeSession(context.Context, string, harness.RuntimeSessionID) (*harness.RuntimeSession, error) {
	return nil, nil
}
func (f *fakeRuntimeSessionCleanupStore) ListRuntimeSessions(context.Context, harness.RuntimeSessionFilter) ([]harness.RuntimeSession, string, error) {
	return nil, "", nil
}
func (f *fakeRuntimeSessionCleanupStore) ClaimRuntimeSession(context.Context, string, string, harness.ProviderKind, string, string, bool, time.Time) (*harness.RuntimeSession, error) {
	return nil, nil
}
func (f *fakeRuntimeSessionCleanupStore) MarkRuntimeSessionActiveTurn(context.Context, string, harness.RuntimeSessionID, string, time.Time) (*harness.RuntimeSession, error) {
	return nil, nil
}
func (f *fakeRuntimeSessionCleanupStore) MarkRuntimeSessionIdle(context.Context, string, harness.RuntimeSessionID, time.Time) (*harness.RuntimeSession, error) {
	return nil, nil
}
func (f *fakeRuntimeSessionCleanupStore) RetainRuntimeSession(context.Context, string, harness.RuntimeSessionID, time.Time) (*harness.RuntimeSession, error) {
	return nil, nil
}
func (f *fakeRuntimeSessionCleanupStore) ReleaseRuntimeSession(context.Context, string, harness.RuntimeSessionID, harness.RuntimeCleanupPolicy, time.Time) (*harness.RuntimeSession, error) {
	return nil, nil
}
func (f *fakeRuntimeSessionCleanupStore) DeleteRuntimeSession(context.Context, string, harness.RuntimeSessionID, time.Time) (*harness.RuntimeSession, error) {
	return nil, nil
}
func (f *fakeRuntimeSessionCleanupStore) PruneDeletedRuntimeSession(context.Context, string, harness.RuntimeSessionID) error {
	return nil
}
func (f *fakeRuntimeSessionCleanupStore) MarkRuntimeSessionFailed(context.Context, string, harness.RuntimeSessionID, harness.RuntimeSessionState, string, time.Time) (*harness.RuntimeSession, error) {
	return nil, nil
}
func (f *fakeRuntimeSessionCleanupStore) CleanupIdleRuntimeSessions(_ context.Context, now time.Time) ([]harness.RuntimeSession, error) {
	f.called.Add(1)
	f.now = now
	return f.deleted, f.err
}

func TestRuntimeSessionCleanupLoopRunOnce(t *testing.T) {
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	store := &fakeRuntimeSessionCleanupStore{deleted: []harness.RuntimeSession{{ID: "runtime-a"}}}
	loop := RuntimeSessionCleanupLoop{Store: store, Now: func() time.Time { return now }}

	loop.RunOnce(context.Background())

	if store.called.Load() != 1 || !store.now.Equal(now) {
		t.Fatalf("cleanup called=%d now=%s, want once at %s", store.called.Load(), store.now, now)
	}
}

func TestRuntimeSessionCleanupLoopStartRunsUntilContextCancelled(t *testing.T) {
	store := &fakeRuntimeSessionCleanupStore{}
	loop := RuntimeSessionCleanupLoop{Store: store, Interval: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.Start(ctx) }()
	deadline := time.After(time.Second)
	for store.called.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("cleanup calls = %d, want at least 2", store.called.Load())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start() error = %v", err)
	}
}

func TestRuntimeSessionCleanupLoopRunOnceLogsAndContinuesOnStoreError(t *testing.T) {
	store := &fakeRuntimeSessionCleanupStore{err: errors.New("cleanup unavailable")}
	loop := RuntimeSessionCleanupLoop{Store: store}
	loop.RunOnce(context.Background())
	if store.called.Load() != 1 {
		t.Fatalf("cleanup called=%d, want 1", store.called.Load())
	}
}
