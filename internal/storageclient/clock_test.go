package storageclient

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store/sqlite"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// skewedClockStore reports a provider clock offset from the real one.
type skewedClockStore struct {
	*sqlite.Store

	mu       sync.Mutex
	offset   time.Duration
	reported bool
	failure  error
}

func (s *skewedClockStore) set(offset time.Duration, reported bool, failure error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offset, s.reported, s.failure = offset, reported, failure
}

func (s *skewedClockStore) Describe(ctx context.Context) (storagev1.ProviderInfo, error) {
	info, err := s.Store.Describe(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure != nil {
		return storagev1.ProviderInfo{}, s.failure
	}
	info.ObservedAt = time.Time{}
	if s.reported {
		info.ObservedAt = time.Now().UTC().Add(s.offset)
	}
	return info, err
}

func newSkewedClockClient(t *testing.T) (*Client, *skewedClockStore) {
	t.Helper()
	backend, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	datasetID := advanceTestDataset(t, backend, testFence)
	skewed := &skewedClockStore{Store: backend}
	client, err := New(Options{
		State: skewed, Artifacts: skewed, Embedded: skewed,
		Fences: staticFences{datasetID: datasetID, fence: testFence},
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, skewed
}

// A dataset whose clock is beyond MaxClockSkew refuses new writes with
// ErrNotReady until a later check finds it back within bounds. A provider
// that does not report its clock is not checked.
func TestClockCheckGatesWrites(t *testing.T) {
	client, skewed := newSkewedClockClient(t)
	ctx := t.Context()

	if err := client.CheckClocks(ctx); err != nil {
		t.Fatalf("a provider that reports no clock: %v", err)
	}
	skewed.set(5*time.Second, true, nil)
	if err := client.CheckClocks(ctx); err != nil {
		t.Fatalf("a clock within bounds: %v", err)
	}
	if err := client.SaveResult(ctx, "tenant", "task", []byte("first")); err != nil {
		t.Fatal(err)
	}

	for _, offset := range []time.Duration{storagev1.MaxClockSkew + time.Minute, -storagev1.MaxClockSkew - time.Minute} {
		skewed.set(offset, true, nil)
		if err := client.CheckClocks(ctx); !errors.Is(err, storagev1.ErrNotReady) {
			t.Fatalf("offset %s: check = %v, want ErrNotReady", offset, err)
		}
		err := client.SaveResult(ctx, "tenant", "task", []byte("refused"))
		if !errors.Is(err, storagev1.ErrNotReady) || !errors.Is(err, storagev1.ErrClockSkew) {
			t.Fatalf("offset %s: write = %v, want ErrNotReady for clock skew", offset, err)
		}
		if _, err := client.GetResult(ctx, "tenant", "task"); err != nil {
			t.Fatalf("offset %s: reads keep working: %v", offset, err)
		}
	}
	if data, _ := client.GetResult(ctx, "tenant", "task"); string(data) != "first" {
		t.Fatalf("result = %q, want the refused writes to change nothing", data)
	}

	skewed.set(0, false, errors.New("describe failed"))
	if err := client.CheckClocks(ctx); err == nil {
		t.Fatal("a failed check reported no error")
	}
	if err := client.SaveResult(ctx, "tenant", "task", []byte("still refused")); !errors.Is(err, storagev1.ErrNotReady) {
		t.Fatalf("a failed check lifted the refusal: %v", err)
	}

	skewed.set(time.Second, true, nil)
	if err := client.CheckClocks(ctx); err != nil {
		t.Fatalf("a clock back within bounds: %v", err)
	}
	if err := client.SaveResult(ctx, "tenant", "task", []byte("second")); err != nil {
		t.Fatalf("writes resume: %v", err)
	}
}

// The offset is estimated against the midpoint of the round trip, and only a
// skew beyond the limit even at the edge of that uncertainty counts.
func TestEstimateClockSkew(t *testing.T) {
	sent := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		roundTrip  time.Duration
		observedAt time.Time
		offset     time.Duration
		beyond     bool
	}{
		{"in step", 2 * time.Second, sent.Add(time.Second), 0, false},
		{"ahead", 2 * time.Second, sent.Add(time.Minute), 59 * time.Second, true},
		{"behind", 2 * time.Second, sent.Add(-time.Minute), -61 * time.Second, true},
		{"within the round trip", 10 * time.Second, sent.Add(40 * time.Second), 35 * time.Second, false},
		{"just beyond", 0, sent.Add(storagev1.MaxClockSkew + time.Nanosecond), storagev1.MaxClockSkew + time.Nanosecond, true},
		{"at the limit", 0, sent.Add(storagev1.MaxClockSkew), storagev1.MaxClockSkew, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			skew := estimateClockSkew(sent, sent.Add(tc.roundTrip), tc.observedAt)
			if skew.offset != tc.offset || skew.uncertainty != tc.roundTrip/2 {
				t.Fatalf("skew = %s, want %s within %s", skew, tc.offset, tc.roundTrip/2)
			}
			if got := skew.beyond(storagev1.MaxClockSkew); got != tc.beyond {
				t.Fatalf("beyond = %v, want %v", got, tc.beyond)
			}
		})
	}
}

// The monitor checks at once and then on its interval, until it stops.
func TestClockMonitorChecksUntilStopped(t *testing.T) {
	client, skewed := newSkewedClockClient(t)
	skewed.set(storagev1.MaxClockSkew+time.Minute, true, nil)
	monitor := &ClockMonitor{Client: client, Interval: 20 * time.Millisecond}
	if !monitor.NeedLeaderElection() {
		t.Fatal("the clock monitor must run only on the leader")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- monitor.Start(ctx) }()

	waitFor := func(what string, ready func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ready() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor("the first check to refuse writes", func() bool {
		return errors.Is(client.SaveResult(t.Context(), "tenant", "task", nil), storagev1.ErrNotReady)
	})
	skewed.set(0, true, nil)
	waitFor("a later check to lift the refusal", func() bool {
		return client.SaveResult(t.Context(), "tenant", "task", []byte("ok")) == nil
	})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the monitor did not stop")
	}
}
