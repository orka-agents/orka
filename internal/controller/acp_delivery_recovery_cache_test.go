/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

type countingAttemptStore struct {
	store.DurableControlStore
	mu      sync.Mutex
	attempt store.PromptAttempt
	reads   int
}

func (s *countingAttemptStore) GetPromptAttempt(context.Context, string) (*store.PromptAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	attempt := s.attempt
	return &attempt, nil
}

func (s *countingAttemptStore) setDelivery(state store.PromptDeliveryState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempt.DeliveryState = state
}

func (s *countingAttemptStore) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func settledDeliveryTestEpochs(t *testing.T) (*ControllerEpochManager, int64) {
	t.Helper()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "epochs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	epochs := NewControllerEpochManager(sqlite.NewStore(db, "test"), "delivery-recovery-cache")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- epochs.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	fence, err := epochs.CurrentFence(waitCtx)
	if err != nil {
		t.Fatal(err)
	}
	return epochs, fence.Epoch
}

func settledDeliveryTestTask(epoch int64) corev1alpha1.Task {
	now := metav1.Now()
	return corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "settled", UID: types.UID("66666666-6666-6666-6666-666666666666"), ResourceVersion: "10"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{
			Phase:                 corev1alpha1.TaskPhaseSucceeded,
			AgentExecutionBinding: &corev1alpha1.AgentExecutionBinding{ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV2},
			Execution: &corev1alpha1.TaskExecutionStatus{
				State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
				Attempt: 1, PromptID: "prompt-1", ControllerEpoch: epoch, LastTransitionTime: &now,
			},
			Delivery: &corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStateReadValidated, LastTransitionTime: &now},
		},
	}
}

func TestDeliveryRecoveryReadsASettledTaskOnlyWhenItChanges(t *testing.T) {
	epochs, epoch := settledDeliveryTestEpochs(t)
	attempts := &countingAttemptStore{attempt: store.PromptAttempt{
		ExecutionState: store.PromptExecutionSucceeded, DeliveryState: store.PromptDeliveryReadValidated,
	}}
	dispatcher := &ACPDispatcher{Store: attempts, Epochs: epochs, sem: make(chan struct{}, 1)}
	tasks := []corev1alpha1.Task{settledDeliveryTestTask(epoch)}
	ctx := context.Background()

	for range 3 {
		if err := dispatcher.scheduleACPDeliveryRecoveries(ctx, tasks); err != nil {
			t.Fatal(err)
		}
	}
	if got := attempts.readCount(); got != 1 {
		t.Fatalf("store reads over three scans of an unchanged settled Task = %d, want 1", got)
	}

	tasks[0].ResourceVersion = "11"
	if err := dispatcher.scheduleACPDeliveryRecoveries(ctx, tasks); err != nil {
		t.Fatal(err)
	}
	if got := attempts.readCount(); got != 2 {
		t.Fatalf("store reads after the Task changed = %d, want 2", got)
	}

	dispatcher.mu.Lock()
	dispatcher.settledDeliveryRecoveries.put(tasks[0].UID, acpSettledDeliveryRecovery{resourceVersion: "11", epoch: epoch - 1})
	dispatcher.mu.Unlock()
	if err := dispatcher.scheduleACPDeliveryRecoveries(ctx, tasks); err != nil {
		t.Fatal(err)
	}
	if got := attempts.readCount(); got != 3 {
		t.Fatalf("store reads under a different controller epoch = %d, want 3", got)
	}

	if err := dispatcher.scheduleACPDeliveryRecoveries(ctx, nil); err != nil {
		t.Fatal(err)
	}
	dispatcher.mu.Lock()
	remaining := dispatcher.settledDeliveryRecoveries.len()
	dispatcher.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("%d verdicts remain for Tasks no longer listed", remaining)
	}
}

func TestDeliveryRecoveryVerdictCacheIsBounded(t *testing.T) {
	epochs, epoch := settledDeliveryTestEpochs(t)
	attempts := &countingAttemptStore{attempt: store.PromptAttempt{
		ExecutionState: store.PromptExecutionSucceeded, DeliveryState: store.PromptDeliveryReadValidated,
	}}
	const limit = 4
	dispatcher := &ACPDispatcher{Store: attempts, Epochs: epochs, sem: make(chan struct{}, 1)}
	dispatcher.settledDeliveryRecoveries.limit = limit
	ctx := context.Background()
	tasks := make([]corev1alpha1.Task, 3*limit)
	for i := range tasks {
		tasks[i] = settledDeliveryTestTask(epoch)
		tasks[i].Name = fmt.Sprintf("settled-%d", i)
		tasks[i].UID = types.UID(fmt.Sprintf("settled-uid-%d", i))
	}
	cached := func() int {
		dispatcher.mu.Lock()
		defer dispatcher.mu.Unlock()
		return dispatcher.settledDeliveryRecoveries.len()
	}

	if err := dispatcher.scheduleACPDeliveryRecoveries(ctx, tasks); err != nil {
		t.Fatal(err)
	}
	if got := cached(); got != limit {
		t.Fatalf("cached verdicts after one scan of %d settled Tasks = %d, want the limit %d", len(tasks), got, limit)
	}
	if got := attempts.readCount(); got != len(tasks) {
		t.Fatalf("store reads on the first scan = %d, want %d", got, len(tasks))
	}
	for scan := range 3 {
		before := attempts.readCount()
		if err := dispatcher.scheduleACPDeliveryRecoveries(ctx, tasks); err != nil {
			t.Fatal(err)
		}
		if got := cached(); got != limit {
			t.Fatalf("scan %d: cached verdicts = %d, want %d", scan, got, limit)
		}
		if reads := attempts.readCount() - before; reads < len(tasks)-limit || reads > len(tasks) {
			t.Fatalf("scan %d: store reads = %d, want between %d and %d", scan, reads, len(tasks)-limit, len(tasks))
		}
	}
	dispatcher.mu.Lock()
	kept := make([]corev1alpha1.Task, 0, limit)
	for i := range tasks {
		if _, ok := dispatcher.settledDeliveryRecoveries.entries[tasks[i].UID]; ok {
			kept = append(kept, tasks[i])
		}
	}
	dispatcher.mu.Unlock()
	before := attempts.readCount()
	if err := dispatcher.scheduleACPDeliveryRecoveries(ctx, kept); err != nil {
		t.Fatal(err)
	}
	if reads := attempts.readCount() - before; len(kept) != limit || reads != 0 {
		t.Fatalf("kept=%d store reads=%d, want %d kept Tasks skipped without reads", len(kept), reads, limit)
	}
	dispatcher.sem = make(chan struct{})
	attempts.setDelivery(store.PromptDeliveryValidating)
	evicted := tasks[0]
	evicted.ResourceVersion = "12"
	before = attempts.readCount()
	if err := dispatcher.scheduleACPDeliveryRecoveries(ctx, []corev1alpha1.Task{evicted}); err != nil {
		t.Fatal(err)
	}
	if reads := attempts.readCount() - before; reads != 1 {
		t.Fatalf("store reads for a changed evicted Task = %d, want 1", reads)
	}
}

func TestACPSettledDeliveryRecoveryCacheEvictsLeastRecentlyUsed(t *testing.T) {
	cache := acpSettledDeliveryRecoveryCache{limit: 2}
	cache.put("a", acpSettledDeliveryRecovery{resourceVersion: "1"})
	cache.put("b", acpSettledDeliveryRecovery{resourceVersion: "1"})
	if _, ok := cache.get("a"); !ok {
		t.Fatal("a was not kept")
	}
	cache.put("c", acpSettledDeliveryRecovery{resourceVersion: "1"})
	if _, ok := cache.get("b"); ok {
		t.Fatal("b, the least recently used, was kept past the limit")
	}
	for _, uid := range []types.UID{"a", "c"} {
		if _, ok := cache.get(uid); !ok {
			t.Fatalf("%s was evicted", uid)
		}
	}
	cache.put("a", acpSettledDeliveryRecovery{resourceVersion: "2"})
	if verdict, _ := cache.get("a"); verdict.resourceVersion != "2" || cache.len() != 2 {
		t.Fatalf("updated a = %+v, len %d", verdict, cache.len())
	}
	cache.remove("a")
	if _, ok := cache.get("a"); ok || cache.len() != 1 {
		t.Fatalf("removed a still cached, len %d", cache.len())
	}
}

func TestDeliveryRecoveryKeepsReadingAttemptsThatCanStillChange(t *testing.T) {
	epochs, epoch := settledDeliveryTestEpochs(t)
	attempts := &countingAttemptStore{attempt: store.PromptAttempt{
		ExecutionState: store.PromptExecutionSucceeded, DeliveryState: store.PromptDeliveryNotRequested,
	}}
	dispatcher := &ACPDispatcher{Store: attempts, Epochs: epochs, sem: make(chan struct{})}
	tasks := []corev1alpha1.Task{settledDeliveryTestTask(epoch)}
	ctx := context.Background()
	for range 2 {
		if err := dispatcher.scheduleACPDeliveryRecoveries(ctx, tasks); err != nil {
			t.Fatal(err)
		}
	}
	if got := attempts.readCount(); got != 2 {
		t.Fatalf("store reads of a still-changeable attempt over two scans = %d, want 2", got)
	}
	attempts.setDelivery(store.PromptDeliveryValidating)
	if err := dispatcher.scheduleACPDeliveryRecoveries(ctx, tasks); err != nil {
		t.Fatal(err)
	}
	if got := attempts.readCount(); got != 3 {
		t.Fatalf("store reads after the durable delivery moved = %d, want 3", got)
	}
}

func TestACPDeliveryRecoveryVerdictSettled(t *testing.T) {
	settled := settledDeliveryTestTask(1)
	finalizing := settledDeliveryTestTask(1)
	finalizing.Status.Phase = corev1alpha1.TaskPhaseFinalizing
	deleting := settledDeliveryTestTask(1)
	deleting.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	for _, tc := range []struct {
		name           string
		task           *corev1alpha1.Task
		execution      store.PromptExecutionState
		delivery       store.PromptDeliveryState
		cleanupPending bool
		want           bool
	}{
		{name: "succeeded with final delivery", task: &settled, execution: store.PromptExecutionSucceeded, delivery: store.PromptDeliveryVerifiedExact, want: true},
		{name: "succeeded before delivery was requested", task: &settled, execution: store.PromptExecutionSucceeded, delivery: store.PromptDeliveryNotRequested},
		{name: "succeeded while publishing", task: &settled, execution: store.PromptExecutionSucceeded, delivery: store.PromptDeliveryPublishing},
		{name: "failed with cleanup complete", task: &settled, execution: store.PromptExecutionFailed, delivery: store.PromptDeliveryNotRequested, want: true},
		{name: "failed with cleanup pending", task: &settled, execution: store.PromptExecutionFailed, delivery: store.PromptDeliveryNotRequested, cleanupPending: true},
		{name: "outcome unknown with final delivery", task: &settled, execution: store.PromptExecutionOutcomeUnknown, delivery: store.PromptDeliveryPublicationOutcomeUnknown, cleanupPending: true, want: true},
		{name: "running attempt", task: &settled, execution: store.PromptExecutionRunning, delivery: store.PromptDeliveryNotRequested},
		{name: "finalizing Task", task: &finalizing, execution: store.PromptExecutionSucceeded, delivery: store.PromptDeliveryVerifiedExact},
		{name: "deleting Task", task: &deleting, execution: store.PromptExecutionSucceeded, delivery: store.PromptDeliveryVerifiedExact},
	} {
		attempt := &store.PromptAttempt{ExecutionState: tc.execution, DeliveryState: tc.delivery}
		if got := acpDeliveryRecoveryVerdictSettled(tc.task, attempt, tc.cleanupPending); got != tc.want {
			t.Errorf("%s: settled = %t, want %t", tc.name, got, tc.want)
		}
	}
}

func TestEffectiveRuntimePoolMaxConcurrentReconciles(t *testing.T) {
	limits := func(configured int) (int, error) {
		return effectiveMaxConcurrentReconciles("runtimepool", configured,
			DefaultRuntimePoolMaxConcurrentReconciles, MaxRuntimePoolMaxConcurrentReconciles)
	}
	if got, err := limits(0); err != nil || got != DefaultRuntimePoolMaxConcurrentReconciles {
		t.Fatalf("unset = %d, %v; want the default %d", got, err, DefaultRuntimePoolMaxConcurrentReconciles)
	}
	if DefaultRuntimePoolMaxConcurrentReconciles < 2 {
		t.Fatalf("default RuntimePool concurrency = %d; independent pools would publish one at a time", DefaultRuntimePoolMaxConcurrentReconciles)
	}
	for _, invalid := range []int{-1, MaxRuntimePoolMaxConcurrentReconciles + 1} {
		if _, err := limits(invalid); err == nil {
			t.Fatalf("%d concurrent RuntimePool reconciles was accepted", invalid)
		}
	}
}
