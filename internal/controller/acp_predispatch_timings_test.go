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

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

	record := timings.finish(task)
	if record == nil {
		t.Fatal("finish returned no record")
	}
	got := preDispatchValues(record.keysAndValues())
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
