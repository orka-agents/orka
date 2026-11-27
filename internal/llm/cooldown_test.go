package llm

import (
	"sync"
	"testing"
	"time"
)

func TestCooldownTracker_MarkAndCheck(t *testing.T) {
	tracker := NewCooldownTracker()

	if tracker.IsCoolingDown("test") {
		t.Error("expected not cooling down before mark")
	}

	tracker.MarkCooldown("test")

	if !tracker.IsCoolingDown("test") {
		t.Error("expected cooling down after mark")
	}
}

func TestCooldownTracker_Reset(t *testing.T) {
	tracker := NewCooldownTracker()

	tracker.MarkCooldown("test")
	if !tracker.IsCoolingDown("test") {
		t.Error("expected cooling down after mark")
	}

	tracker.Reset("test")
	if tracker.IsCoolingDown("test") {
		t.Error("expected not cooling down after reset")
	}
}

func TestCooldownTracker_CooldownRemaining(t *testing.T) {
	tracker := NewCooldownTracker()

	if d := tracker.CooldownRemaining("test"); d != 0 {
		t.Errorf("expected 0 remaining, got %v", d)
	}

	tracker.MarkCooldown("test")
	remaining := tracker.CooldownRemaining("test")

	if remaining < 50*time.Second || remaining > 65*time.Second {
		t.Errorf("expected ~1m remaining, got %v", remaining)
	}
}

func TestCooldownTracker_ExponentialBackoff(t *testing.T) {
	tracker := NewCooldownTracker()

	tracker.MarkCooldown("test")
	r1 := tracker.CooldownRemaining("test")
	if r1 < 50*time.Second || r1 > 65*time.Second {
		t.Errorf("expected ~1m after 1st mark, got %v", r1)
	}

	tracker.MarkCooldown("test")
	r2 := tracker.CooldownRemaining("test")
	if r2 < 4*time.Minute || r2 > 6*time.Minute {
		t.Errorf("expected ~5m after 2nd mark, got %v", r2)
	}

	tracker.MarkCooldown("test")
	r3 := tracker.CooldownRemaining("test")
	if r3 < 20*time.Minute || r3 > 30*time.Minute {
		t.Errorf("expected ~25m after 3rd mark, got %v", r3)
	}
}

func TestCooldownTracker_CappedAt1Hour(t *testing.T) {
	tracker := NewCooldownTracker()

	for range 10 {
		tracker.MarkCooldown("test")
	}

	remaining := tracker.CooldownRemaining("test")
	if remaining > time.Hour+time.Second {
		t.Errorf("expected capped at 1h, got %v", remaining)
	}
}

func TestCooldownTracker_IndependentProviders(t *testing.T) {
	tracker := NewCooldownTracker()

	tracker.MarkCooldown("provider-a")

	if tracker.IsCoolingDown("provider-b") {
		t.Error("provider-b should not be affected by provider-a's cooldown")
	}
	if !tracker.IsCoolingDown("provider-a") {
		t.Error("provider-a should be cooling down")
	}
}

func TestCooldownTracker_ConcurrentAccess(t *testing.T) {
	tracker := NewCooldownTracker()
	var wg sync.WaitGroup

	for range 100 {
		wg.Go(func() {
			tracker.MarkCooldown("concurrent-test")
			tracker.IsCoolingDown("concurrent-test")
			tracker.CooldownRemaining("concurrent-test")
		})
	}

	wg.Wait()

	if !tracker.IsCoolingDown("concurrent-test") {
		t.Error("expected cooling down after concurrent marks")
	}
}

func TestCooldownTracker_ResetNonExistent(t *testing.T) {
	tracker := NewCooldownTracker()
	tracker.Reset("nonexistent")
}
