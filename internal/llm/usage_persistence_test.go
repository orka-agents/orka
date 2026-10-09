package llm

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func TestFinishUsageAllowsBoundedRestartRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithCancel(t.Context())
		cancel() // Final accounting must still settle after task cancellation.
		calls := 0
		err := finishUsage(parent, func(ctx context.Context, _ store.UsageObservation) error {
			calls++
			select {
			case <-time.After(6 * time.Second):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}, store.UsageObservation{ID: "call/finish"})
		if err != nil || calls != 1 {
			t.Fatalf("finishUsage error=%v calls=%d, want one recovered accounting write", err, calls)
		}
	})
}

func TestFinishUsageStillFailsClosedAtBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		err := finishUsage(t.Context(), func(ctx context.Context, _ store.UsageObservation) error {
			<-ctx.Done()
			return ctx.Err()
		}, store.UsageObservation{ID: "call/finish"})
		if !IsUsagePersistenceError(err) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want fail-closed accounting deadline", err)
		}
		if elapsed := time.Since(start); elapsed != 30*time.Second {
			t.Fatalf("elapsed = %v, want bounded 30s settlement", elapsed)
		}
	})
}
