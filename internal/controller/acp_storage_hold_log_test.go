package controller

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr/funcr"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// recoveryLog records the dispatcher's log lines.
type recoveryLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *recoveryLog) context(ctx context.Context) context.Context {
	return logf.IntoContext(ctx, funcr.New(func(prefix, args string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.lines = append(l.lines, prefix+" "+args)
	}, funcr.Options{}))
}

func (l *recoveryLog) count(message string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.Contains(line, message) {
			n++
		}
	}
	return n
}

// The scheduled stale recovery that runs every dispatch interval reports a
// held Task once, when the hold is placed, rather than on every pass.
func TestScheduledRecoveryDoesNotLogAnUnchangedHoldEveryPass(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task, _, _ := settlingTaskWithReceipt(t, fixture)
	loseResultReceipt(t, fixture, task)
	fixture.dispatcher.active = map[types.UID]struct{}{}
	log := &recoveryLog{}
	ctx := log.context(fixture.ctx)

	for range 3 {
		latest := &corev1alpha1.Task{}
		if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), latest); err != nil {
			t.Fatal(err)
		}
		fixture.dispatcher.scheduleStaleAttemptRecoveries(ctx, []corev1alpha1.Task{*latest}, fixture.fence)
		// The scheduled pass holds the mutex until it finishes.
		fixture.dispatcher.staleRecoveryMu.Lock()
		fixture.dispatcher.staleRecoveryMu.Unlock() //nolint:staticcheck // Waits for the scheduled pass.
	}

	requireTaskStorageCondition(t, fixture, task, "True", corev1alpha1.StorageRecoveryMissingRecordReason)
	if held := log.count("ACP Task is held for storage recovery"); held != 1 {
		t.Fatalf("hold placements logged = %d, want 1", held)
	}
	if blocked := log.count("ACP Task remains blocked on epoch recovery"); blocked != 0 {
		t.Fatalf("an unchanged hold was logged as blocked recovery %d times", blocked)
	}
}
