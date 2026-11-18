/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

type steppingClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *steppingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *steppingClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func preDispatchTestTask(uid string, created time.Time) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "task-" + uid, UID: types.UID(uid), CreationTimestamp: metav1.NewTime(created),
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
	}
}

func preDispatchValues(values []any) map[string]any {
	out := make(map[string]any, len(values)/2)
	for i := 0; i+1 < len(values); i += 2 {
		out[values[i].(string)] = values[i+1]
	}
	return out
}

// Each milestone keeps its first time and is reported as the step since the
// previous one; the report removes the record so a Task logs once.
func TestACPPreDispatchTimingsReportsEachStepOnce(t *testing.T) {
	created := time.Date(2026, 11, 18, 18, 29, 17, 0, time.UTC)
	clock := &steppingClock{now: created.Add(300 * time.Millisecond)}
	timings := newACPPreDispatchTimings(clock.Now)
	task := preDispatchTestTask("uid-1", created)

	timings.mark(task, acpPreDispatchObserved)
	clock.Advance(500 * time.Millisecond)
	timings.mark(task, acpPreDispatchStatusInitialized)
	clock.Advance(time.Second)
	timings.mark(task, acpPreDispatchExecutionBound)
	clock.Advance(4 * time.Second)
	timings.mark(task, acpPreDispatchQueued)
	clock.Advance(time.Second)
	// A later pass reaching an earlier milestone again keeps the first time.
	timings.mark(task, acpPreDispatchStatusInitialized)
	timings.mark(task, acpPreDispatchScanned)
	clock.Advance(200 * time.Millisecond)
	timings.mark(task, acpPreDispatchReserved)
	clock.Advance(100 * time.Millisecond)

	finished := timings.finish(task)
	if finished == nil {
		t.Fatal("finish returned no record")
	}
	got := preDispatchValues(finished.keysAndValues())
	want := map[string]any{
		"sinceTaskCreatedMs":  int64(7100),
		"observedMs":          int64(300),
		"statusInitializedMs": int64(500),
		"executionBoundMs":    int64(1000),
		"dispatchQueuedMs":    int64(4000),
		"dispatchScannedMs":   int64(1000),
		"reservedMs":          int64(200),
		"startedMs":           int64(100),
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("timings = %v, want %v", got, want)
	}
	if again := timings.finish(task); again != nil {
		t.Fatalf("a second finish returned %+v, want the record removed", again)
	}
}

func TestACPPreDispatchTimingsIgnoresNonAgentTasksAndBoundsRecords(t *testing.T) {
	created := time.Date(2026, 11, 18, 18, 0, 0, 0, time.UTC)
	clock := &steppingClock{now: created}
	timings := newACPPreDispatchTimings(clock.Now)
	container := preDispatchTestTask("container", created)
	container.Spec.Type = corev1alpha1.TaskTypeContainer
	timings.mark(container, acpPreDispatchObserved)
	timings.mark(&corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent}}, acpPreDispatchObserved)
	timings.mark(nil, acpPreDispatchObserved)
	if len(timings.records) != 0 {
		t.Fatalf("records = %d, want only agent Tasks with a UID tracked", len(timings.records))
	}

	for i := range acpPreDispatchMaxRecords {
		timings.mark(preDispatchTestTask(fmt.Sprintf("stale-%d", i), created), acpPreDispatchObserved)
	}
	fresh := preDispatchTestTask("fresh", created)
	timings.mark(fresh, acpPreDispatchObserved)
	if _, ok := timings.records[fresh.UID]; ok {
		t.Fatal("a full table admitted a new record before its entries expired")
	}
	clock.Advance(acpPreDispatchRetention)
	timings.mark(fresh, acpPreDispatchObserved)
	if _, ok := timings.records[fresh.UID]; !ok || len(timings.records) != 1 {
		t.Fatalf("expired records were not pruned: %d records remain", len(timings.records))
	}
}

func TestACPPreDispatchTimingsIsSafeForConcurrentReconciles(t *testing.T) {
	timings := newACPPreDispatchTimings(time.Now)
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Go(func() {
			for i := range 200 {
				task := preDispatchTestTask(fmt.Sprintf("uid-%d", i%16), time.Now())
				timings.mark(task, acpPreDispatchMilestone((worker+i)%int(acpPreDispatchStarted)))
				if i%7 == 0 {
					timings.report(context.Background(), task)
				}
			}
		})
	}
	wg.Wait()
}

func TestACPDispatchPoolRetirementDue(t *testing.T) {
	now := time.Date(2026, 11, 18, 18, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		last time.Time
		want bool
	}{
		{name: "first scan", want: true},
		{name: "wakeup just after a retiring scan", last: now.Add(-200 * time.Millisecond), want: false},
		{name: "tick shortly after a wakeup scan", last: now.Add(-800 * time.Millisecond), want: true},
		{name: "a full interval later", last: now.Add(-time.Second), want: true},
	} {
		if got := acpDispatchPoolRetirementDue(tc.last, now, time.Second); got != tc.want {
			t.Errorf("%s: retirement due = %t, want %t", tc.name, got, tc.want)
		}
	}
}

// A wakeup scan still publishes coordinator capacity, which retirement and
// scale-down read as a guard, but leaves idle and stopped pools to the next
// retiring scan; a retiring scan reaps both.
func TestReapIdlePoolsWithoutRetirementRefreshesCapacityOnly(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	oldDemand := now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	idle := &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "idle", UID: "idle-uid",
			Annotations: map[string]string{acpRuntimeLastDemandAnnotation: oldDemand},
		},
		Spec:   corev1alpha1.RuntimePoolSpec{DesiredReplicas: 1},
		Status: corev1alpha1.RuntimePoolStatus{Capacity: corev1alpha1.RuntimePoolCapacityStatus{QueuedTasks: 2}},
	}
	stopped := &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "stopped", UID: "stopped-uid",
			Annotations: map[string]string{acpRuntimeLastDemandAnnotation: oldDemand},
		},
		Spec: corev1alpha1.RuntimePoolSpec{
			ExecutionWorkspace: &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{Provider: corev1alpha1.WorkspaceProviderSubstrate},
		},
		Status: corev1alpha1.RuntimePoolStatus{Lifecycle: corev1alpha1.RuntimePoolLifecycleStopped},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RuntimePool{}).WithObjects(idle, stopped).Build()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "retirement.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	epochs := NewControllerEpochManager(sqlite.NewStore(db, "test"), "retirement-controller")
	epochCtx, cancelEpoch := context.WithCancel(context.Background())
	epochDone := make(chan error, 1)
	go func() { epochDone <- epochs.Start(epochCtx) }()
	defer func() {
		cancelEpoch()
		<-epochDone
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := epochs.CurrentFence(ctx); err != nil {
		t.Fatal(err)
	}
	dispatcher := &ACPDispatcher{Client: kubeClient, Epochs: epochs, IdlePoolTTL: time.Minute}

	if err := dispatcher.reapIdlePoolsWithRetirement(ctx, nil, false); err != nil {
		t.Fatal(err)
	}
	got := &corev1alpha1.RuntimePool{}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(idle), got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.DesiredReplicas != 1 {
		t.Fatalf("a wakeup scan scaled the idle pool to %d", got.Spec.DesiredReplicas)
	}
	if got.Status.Capacity.QueuedTasks != 0 {
		t.Fatalf("queued capacity = %d, want the wakeup scan to publish 0", got.Status.Capacity.QueuedTasks)
	}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(stopped), &corev1alpha1.RuntimePool{}); err != nil {
		t.Fatalf("a wakeup scan deleted the stopped pool: %v", err)
	}

	if err := dispatcher.reapIdlePoolsWithRetirement(ctx, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(idle), got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.DesiredReplicas != 0 {
		t.Fatalf("a retiring scan left the idle pool at %d replicas", got.Spec.DesiredReplicas)
	}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(stopped), &corev1alpha1.RuntimePool{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a retiring scan kept the stopped pool: %v", err)
	}
}

func TestEffectiveTaskMaxConcurrentReconciles(t *testing.T) {
	for _, tc := range []struct {
		configured int
		want       int
		wantErr    bool
	}{
		{configured: 0, want: DefaultTaskMaxConcurrentReconciles},
		{configured: 1, want: 1},
		{configured: MaxTaskMaxConcurrentReconciles, want: MaxTaskMaxConcurrentReconciles},
		{configured: -1, wantErr: true},
		{configured: MaxTaskMaxConcurrentReconciles + 1, wantErr: true},
	} {
		got, err := effectiveTaskMaxConcurrentReconciles(tc.configured)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("effectiveTaskMaxConcurrentReconciles(%d) = %d, %v; want %d, error %t", tc.configured, got, err, tc.want, tc.wantErr)
		}
	}
	if DefaultTaskMaxConcurrentReconciles != 1 {
		t.Fatalf("default Task concurrency = %d, want serial reconciliation", DefaultTaskMaxConcurrentReconciles)
	}
}

// Concurrent reconciles of two Tasks in one namespace ensure the same trusted
// Service grant; the reconcile that loses the create accepts the identical
// object instead of failing, and still refuses a foreign one.
func TestTrustedServiceGrantCreateAcceptsConcurrentIdenticalCreate(t *testing.T) {
	scheme := newTestScheme()
	desiredRole := func() *rbacv1.Role {
		return &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Namespace: "gateway", Name: "orka-outbound-service-x", Labels: map[string]string{"a": "b"}},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"services"}, ResourceNames: []string{"gw"}, Verbs: []string{"get"}}},
		}
	}
	desiredBinding := func() *rbacv1.RoleBinding {
		return &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Namespace: "gateway", Name: "orka-outbound-service-x", Labels: map[string]string{"a": "b"}},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: rbacRoleKind, Name: "orka-outbound-service-x"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "worker", Namespace: "tasks"}},
		}
	}
	// The other reconcile's objects exist, but this reconcile's first read
	// missed them.
	raced := func(objects ...client.Object) client.Client {
		missed := map[string]bool{}
		return interceptor.NewClient(fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), interceptor.Funcs{
			Get: func(ctx context.Context, delegate client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
				kind := fmt.Sprintf("%T", object)
				if !missed[kind] {
					missed[kind] = true
					return apierrors.NewNotFound(rbacv1.Resource("roles"), key.Name)
				}
				return delegate.Get(ctx, key, object, options...)
			},
		})
	}

	r := &TaskReconciler{Client: raced(desiredRole(), desiredBinding())}
	if err := r.createOrUpdateTrustedServiceRole(context.Background(), desiredRole()); err != nil {
		t.Fatalf("identical concurrent Role create: %v", err)
	}
	if err := r.createOrUpdateTrustedServiceRoleBinding(context.Background(), desiredBinding()); err != nil {
		t.Fatalf("identical concurrent RoleBinding create: %v", err)
	}

	foreign := desiredRole()
	foreign.Rules[0].Verbs = []string{"get", "list"}
	r = &TaskReconciler{Client: raced(foreign)}
	if err := r.createOrUpdateTrustedServiceRole(context.Background(), desiredRole()); err == nil {
		t.Fatal("a concurrently created Role with other permissions was accepted")
	}
}

// The Queued status write is watch-visible before it returns, so a dispatcher
// scan, and even the start report, can run while the queue pass is still in
// the write. The queued milestone must already be recorded then, ahead of
// scanned, and the queue pass must leave no record behind for the reported
// Task.
func TestQueueACPRuntimeTaskRecordsQueuedBeforeTheStatusWriteIsVisible(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "task", UID: types.UID("33333333-3333-3333-3333-333333333333"), Generation: 2},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "implement", AgentRuntime: &corev1alpha1.AgentRuntimeSpec{},
			Workspace: &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentRead, GitRepo: "https://github.com/orka-agents/orka.git"},
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "agent", UID: types.UID("44444444-4444-4444-4444-444444444444"), Generation: 3},
		Spec: corev1alpha1.AgentSpec{Model: &corev1alpha1.ModelConfig{Name: acpTestModel}, Runtime: &corev1alpha1.AgentCLIRuntime{
			Type:            corev1alpha1.AgentRuntimeCodex,
			ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2),
		}},
	}
	var reported *acpPreDispatchRecord
	interleaved := false
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}, &corev1alpha1.RuntimePool{}).WithObjects(task).Build()
	kubeClient := interceptor.NewClient(base, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, delegate client.Client, subResource string, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
			if err := delegate.SubResource(subResource).Patch(ctx, object, patch, options...); err != nil {
				return err
			}
			written, ok := object.(*corev1alpha1.Task)
			if !ok || interleaved || written.Status.Execution == nil ||
				written.Status.Execution.State != corev1alpha1.TaskExecutionStateQueued {
				return nil
			}
			// The committed Queued status reaches the dispatcher before the
			// write returns: it scans, reserves, and starts the Task.
			interleaved = true
			visible := written.DeepCopy()
			acpPreDispatch.mark(visible, acpPreDispatchScanned)
			acpPreDispatch.mark(visible, acpPreDispatchReserved)
			reported = acpPreDispatch.finish(visible)
			return nil
		},
	})
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	controlStore := sqlite.NewStore(db, "test")
	epochs := NewControllerEpochManager(controlStore, "controller-test")
	epochCtx, cancelEpoch := context.WithCancel(context.Background())
	epochDone := make(chan error, 1)
	go func() { epochDone <- epochs.Start(epochCtx) }()
	defer func() {
		cancelEpoch()
		<-epochDone
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := epochs.CurrentFence(ctx); err != nil {
		t.Fatal(err)
	}
	reconciler := &TaskReconciler{
		Client: kubeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(10), DurableControlStore: controlStore,
		ControllerEpochManager: epochs, ACPRuntimeNamespace: acpTestRuntimeNamespace,
		ACPRuntimeImages: ACPRuntimeImages{Codex: "docker.io/example/codex@sha256:" + strings.Repeat("a", 64)},
	}
	bound := bindACPQueueTaskForTest(t, ctx, reconciler, task, agent)
	// The Task's first reconcile observed it before any execution.
	acpPreDispatch.mark(bound, acpPreDispatchObserved)
	if _, err := reconciler.queueACPRuntimeTask(ctx, bound, agent); err != nil {
		t.Fatal(err)
	}
	if !interleaved || reported == nil {
		t.Fatalf("the dispatcher never observed the Queued write (interleaved=%t)", interleaved)
	}
	queued, scanned := reported.marks[acpPreDispatchQueued], reported.marks[acpPreDispatchScanned]
	if queued.IsZero() {
		t.Fatal("the start report ran without the queued milestone")
	}
	poolReady, persisted := reported.marks[acpPreDispatchRuntimePoolReady], reported.marks[acpPreDispatchAttemptPersisted]
	if poolReady.IsZero() || persisted.IsZero() || persisted.Before(poolReady) || queued.Before(persisted) {
		t.Fatalf("queue sub-milestones out of order: runtimePoolReady %s, promptAttemptPersisted %s, queued %s", poolReady, persisted, queued)
	}
	if scanned.Before(queued) {
		t.Fatalf("scanned at %s precedes queued at %s", scanned, queued)
	}
	if _, ok := preDispatchValues(reported.keysAndValues())["dispatchScannedMs"]; !ok {
		t.Fatalf("the report omits the queue-to-scan step: %v", reported.keysAndValues())
	}
	acpPreDispatch.mu.Lock()
	_, orphaned := acpPreDispatch.records[task.UID]
	acpPreDispatch.mu.Unlock()
	if orphaned {
		t.Fatal("the queue pass recreated a record after its Task was reported")
	}
}

// A Task's record is created only before it has an execution. Marks from
// later stages that run after its start reported, such as a dispatcher scan
// of the still-Reserved Task or a queue pass of the executing Task, update
// nothing and never recreate a record that nothing would report.
func TestACPPreDispatchTimingsLateMarksNeverRecreateAReportedRecord(t *testing.T) {
	created := time.Date(2026, 11, 18, 18, 0, 0, 0, time.UTC)
	clock := &steppingClock{now: created}
	timings := newACPPreDispatchTimings(clock.Now)
	task := preDispatchTestTask("reported", created)
	timings.mark(task, acpPreDispatchObserved)
	timings.markQueueWriteIssued(task)
	if timings.finish(task) == nil {
		t.Fatal("finish returned no record")
	}
	for milestone := acpPreDispatchExecutionBound; milestone < acpPreDispatchMilestoneCount; milestone++ {
		timings.mark(task, milestone)
	}
	timings.markQueueWriteIssued(task)
	timings.markStarted(task)
	if len(timings.records) != 0 {
		t.Fatalf("late marks recreated %d records after the Task reported", len(timings.records))
	}
	// A Task never observed before execution, such as one queued before this
	// process started, is not tracked either.
	unobserved := preDispatchTestTask("unobserved", created)
	timings.mark(unobserved, acpPreDispatchScanned)
	timings.mark(unobserved, acpPreDispatchReserved)
	if len(timings.records) != 0 || timings.finish(unobserved) != nil {
		t.Fatal("a dispatcher mark created a record for an unobserved Task")
	}
}

// Each reserved execution attempt records its own start; an attempt that
// requeues before its turn leaves the record for the attempt that runs.
func TestACPPreDispatchTimingsStartedIsTheLatestAttempt(t *testing.T) {
	created := time.Date(2026, 11, 18, 18, 0, 0, 0, time.UTC)
	clock := &steppingClock{now: created.Add(time.Second)}
	timings := newACPPreDispatchTimings(clock.Now)
	task := preDispatchTestTask("requeued", created)
	timings.mark(task, acpPreDispatchObserved)
	timings.mark(task, acpPreDispatchReserved)
	timings.markStarted(task)
	clock.Advance(3 * time.Second)
	timings.mark(task, acpPreDispatchReserved)
	timings.markStarted(task)
	finished := timings.finish(task)
	if finished == nil {
		t.Fatal("finish returned no record")
	}
	got := preDispatchValues(finished.keysAndValues())
	if got["sinceTaskCreatedMs"] != int64(4000) || got["startedMs"] != int64(3000) {
		t.Fatalf("timings = %v, want the second attempt's start", got)
	}
}

// An execution that exits before its turn begins, here an unverifiable
// binding, records its start but neither consumes nor reports the Task's
// milestones; the attempt that reaches its turn reports them.
func TestExecuteReservedTaskEarlyExitKeepsPreDispatchMilestones(t *testing.T) {
	task := preDispatchTestTask("55555555-5555-5555-5555-555555555555", time.Now())
	acpPreDispatch.mark(task, acpPreDispatchObserved)
	acpPreDispatch.markQueueWriteIssued(task)
	t.Cleanup(func() { acpPreDispatch.finish(task) })

	if err := (&ACPDispatcher{}).executeReservedTask(context.Background(), task.DeepCopy(), acpDispatchTarget{}); err == nil {
		t.Fatal("execution without an execution snapshot store unexpectedly proceeded")
	}
	acpPreDispatch.mu.Lock()
	remaining := acpPreDispatch.records[task.UID]
	var started, queued time.Time
	if remaining != nil {
		started, queued = remaining.marks[acpPreDispatchStarted], remaining.marks[acpPreDispatchQueued]
	}
	acpPreDispatch.mu.Unlock()
	if remaining == nil {
		t.Fatal("an execution that requeued before its turn consumed the pre-dispatch record")
	}
	if started.IsZero() || queued.IsZero() {
		t.Fatalf("record after the early exit: queued %s, started %s; want both kept", queued, started)
	}
}
