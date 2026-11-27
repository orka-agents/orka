/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

func TestTaskAdmissionSlotsAdmitEveryContender(t *testing.T) {
	for _, scopeKind := range []string{"namespace", "children"} {
		for _, parallel := range []bool{false, true} {
			for _, size := range []struct{ contenders, limit int }{{2, 1}, {7, 3}} {
				name := fmt.Sprintf("%s/parallel=%t/%d-for-%d", scopeKind, parallel, size.contenders, size.limit)
				t.Run(name, func(t *testing.T) {
					runAdmissionContenders(t, scopeKind, parallel, size.contenders, int32(size.limit))
				})
			}
		}
	}
}

func runAdmissionContenders(t *testing.T, scopeKind string, parallel bool, contenders int, limit int32) {
	t.Helper()
	ctx := context.Background()
	const namespace = "default"
	parent := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "coordinator", UID: "coordinator-uid"}}
	objects := make([]client.Object, 0, 1+contenders)
	objects = append(objects, parent)
	for i := range contenders {
		task := &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace, Name: fmt.Sprintf("contender-%d", i), UID: types.UID(fmt.Sprintf("contender-%d-uid", i)),
				Labels: map[string]string{labels.LabelParentTask: parent.Name},
			},
			Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
			Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
		}
		objects = append(objects, task)
	}
	r := newUnitReconciler(newTestScheme(), objects...)
	r.APIReader = r.Client
	r.ControllerNamespace = "orka-system"

	scopeFor := func() taskAdmissionScope {
		if scopeKind == "children" {
			return r.childrenAdmissionScope(parent, limit)
		}
		return r.namespaceAdmissionScope(namespace, limit)
	}
	setPhase := func(name string, phase corev1alpha1.TaskPhase) {
		t.Helper()
		task := &corev1alpha1.Task{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, task); err != nil {
			t.Fatal(err)
		}
		task.Status.Phase = phase
		if err := r.Status().Update(ctx, task); err != nil {
			t.Fatal(err)
		}
	}

	ran := map[string]bool{}
	var running []string
	for round := 0; len(ran) < contenders; round++ {
		if round > 4*contenders {
			t.Fatalf("contenders stopped making progress: %d of %d ran", len(ran), contenders)
		}
		var waiting []*corev1alpha1.Task
		for i := range contenders {
			name := fmt.Sprintf("contender-%d", i)
			if !ran[name] {
				task := &corev1alpha1.Task{}
				if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, task); err != nil {
					t.Fatal(err)
				}
				waiting = append(waiting, task)
			}
		}
		granted := make([]bool, len(waiting))
		attempt := func(i int) {
			ok, err := r.acquireTaskAdmissionSlot(ctx, waiting[i], scopeFor())
			if err != nil {
				t.Error(err)
			}
			granted[i] = ok
		}
		if parallel {
			var wg sync.WaitGroup
			for i := range waiting {
				wg.Go(func() { attempt(i) })
			}
			wg.Wait()
		} else {
			for i := range waiting {
				attempt(i)
			}
		}
		admitted := 0
		for i, ok := range granted {
			if ok {
				admitted++
				ran[waiting[i].Name] = true
				running = append(running, waiting[i].Name)
				setPhase(waiting[i].Name, corev1alpha1.TaskPhaseRunning)
			}
		}
		if int32(len(running)) > limit { //nolint:gosec // test sizes are tiny
			t.Fatalf("round %d: %d Tasks hold slots, limit %d", round, len(running), limit)
		}
		if admitted == 0 && int32(len(running)) < limit { //nolint:gosec // test sizes are tiny
			t.Fatalf("round %d: a slot was free but no waiting Task took it", round)
		}
		if len(running) > 0 {
			setPhase(running[0], corev1alpha1.TaskPhaseSucceeded)
			running = running[1:]
		}
	}
}

func TestTaskAdmissionSlotIsKeptByItsHolderAndFreedOnCompletion(t *testing.T) {
	ctx := context.Background()
	first := limitTestTask("first")
	second := limitTestTask("second")
	r := newUnitReconciler(newTestScheme(), first, second)
	r.APIReader = r.Client
	scope := func() taskAdmissionScope { return r.namespaceAdmissionScope("default", 1) }
	for _, step := range []struct {
		task *corev1alpha1.Task
		want bool
	}{{first, true}, {second, false}, {first, true}} {
		if got, err := r.acquireTaskAdmissionSlot(ctx, step.task, scope()); err != nil || got != step.want {
			t.Fatalf("%s: granted=%t err=%v, want %t", step.task.Name, got, err, step.want)
		}
	}
	done := first.DeepCopy()
	if err := r.Get(ctx, client.ObjectKeyFromObject(first), done); err != nil {
		t.Fatal(err)
	}
	done.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	if err := r.Status().Update(ctx, done); err != nil {
		t.Fatal(err)
	}
	if got, err := r.acquireTaskAdmissionSlot(ctx, second, scope()); err != nil || !got {
		t.Fatalf("second after first finished: granted=%t err=%v, want the freed slot", got, err)
	}
}

func TestTaskAdmissionHonorsAConcurrentlyRecordedGrant(t *testing.T) {
	ctx := context.Background()
	checking := limitTestTask("checking")
	r := newUnitReconciler(newTestScheme(), checking)
	r.APIReader = r.Client
	scope := r.namespaceAdmissionScope("default", 1)
	newcomer := limitTestTask("newcomer")
	if err := r.Create(ctx, newcomer); err != nil {
		t.Fatal(err)
	}
	if granted, err := r.acquireTaskAdmissionSlot(ctx, newcomer, scope); err != nil || !granted {
		t.Fatalf("newcomer granted=%t err=%v, want the free slot", granted, err)
	}
	if granted, err := r.acquireTaskAdmissionSlot(ctx, checking, scope); err != nil || granted {
		t.Fatalf("checking granted=%t err=%v, want the newcomer's grant to hold the only slot", granted, err)
	}
}

func TestReapNamespaceAdmissionLedgers(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	ledger := func(namespace string, renewed time.Time, entries ...types.UID) *coordinationv1.Lease {
		encoded, err := json.Marshal(entries)
		if err != nil {
			t.Fatal(err)
		}
		renewTime := metav1.NewMicroTime(renewed)
		return &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "orka-system", Name: taskAdmissionNamespaceLedger + taskAdmissionLedgerDigest(namespace),
				Labels: map[string]string{taskAdmissionLedgerKindLabel: taskAdmissionLedgerKindNamespace},
				Annotations: map[string]string{
					taskAdmissionLedgerNamespaceAnnotation: namespace, taskAdmissionLedgerAnnotation: string(encoded),
				},
			},
			Spec: coordinationv1.LeaseSpec{RenewTime: &renewTime},
		}
	}
	live := limitTestTask("live")
	live.Namespace = "busy"
	finished := limitTestTask("finished")
	finished.Namespace = "idle"
	finished.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	objects := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "busy"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "idle"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "fresh"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "raced"}},
		live, finished,
		ledger("gone", now, "gone-uid"),
		ledger("busy", now.Add(-2*time.Hour), live.UID),
		ledger("idle", now.Add(-2*time.Hour), finished.UID),
		ledger("fresh", now.Add(-time.Minute)),
		ledger("raced", now.Add(-2*time.Hour)),
	}
	r := newUnitReconciler(newTestScheme(), objects...)
	base := r.Client.(client.WithWatch)
	r.Client = interceptor.NewClient(base, interceptor.Funcs{
		Delete: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.DeleteOption) error {
			if object.GetName() == taskAdmissionNamespaceLedger+taskAdmissionLedgerDigest("raced") {
				current := &coordinationv1.Lease{}
				if err := delegate.Get(ctx, client.ObjectKeyFromObject(object), current); err != nil {
					return err
				}
				current.Annotations[taskAdmissionLedgerAnnotation] = `["new-grant"]`
				if err := delegate.Update(ctx, current); err != nil {
					return err
				}
			}
			return delegate.Delete(ctx, object, options...)
		},
	})
	r.APIReader = base
	r.ControllerNamespace = "orka-system"
	if err := r.reapTaskAdmissionLedgers(ctx, now); err != nil {
		t.Fatal(err)
	}
	for namespace, wantKept := range map[string]bool{"gone": false, "busy": true, "idle": false, "fresh": true, "raced": true} {
		err := base.Get(ctx, types.NamespacedName{Namespace: "orka-system", Name: taskAdmissionNamespaceLedger + taskAdmissionLedgerDigest(namespace)}, &coordinationv1.Lease{})
		if kept := err == nil; kept != wantKept {
			t.Errorf("%s ledger kept=%t (err %v), want %t", namespace, kept, err, wantKept)
		}
	}
}

func TestTaskAdmissionLedgerSizeIsBoundedByTheSlotCap(t *testing.T) {
	entries := make([]types.UID, MaxTaskAdmissionSlots)
	for i := range entries {
		entries[i] = types.UID(fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i))
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	const annotationLimit = 256 * 1024
	if len(encoded) > annotationLimit*3/4 {
		t.Fatalf("a full ledger encodes to %d bytes, too close to the %d-byte annotation limit", len(encoded), annotationLimit)
	}
	for _, tc := range []struct{ configured, want int32 }{
		{1, 1}, {MaxTaskAdmissionSlots, MaxTaskAdmissionSlots}, {MaxTaskAdmissionSlots + 1, MaxTaskAdmissionSlots}, {1 << 30, MaxTaskAdmissionSlots},
	} {
		if got := effectiveTaskAdmissionLimit(tc.configured); got != tc.want {
			t.Errorf("effectiveTaskAdmissionLimit(%d) = %d, want %d", tc.configured, got, tc.want)
		}
	}
}

func TestTaskAdmissionClampsALimitAboveTheCap(t *testing.T) {
	ctx := context.Background()
	holders := make([]corev1alpha1.Task, MaxTaskAdmissionSlots)
	for i := range holders {
		holders[i] = corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: fmt.Sprintf("holder-%d", i), UID: types.UID(fmt.Sprintf("holder-%d", i))},
			Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning}}
	}
	contender := limitTestTask("contender")
	r := newUnitReconciler(newTestScheme(), contender)
	r.APIReader = r.Client
	scope := r.namespaceAdmissionScope("default", 1<<30)
	scope.list = func(context.Context) ([]corev1alpha1.Task, error) { return append(holders, *contender), nil }
	if granted, err := r.acquireTaskAdmissionSlot(ctx, contender, scope); err != nil || granted {
		t.Fatalf("granted=%t err=%v with %d holders, want the capped limit to hold", granted, err, MaxTaskAdmissionSlots)
	}
}

func writeAdmissionLedger(t *testing.T, c client.Client, ledger types.NamespacedName, grants ...types.UID) {
	t.Helper()
	encoded, err := json.Marshal(grants)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Create(context.Background(), &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Namespace: ledger.Namespace, Name: ledger.Name,
		Annotations: map[string]string{taskAdmissionLedgerAnnotation: string(encoded)},
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestForgedAdmissionGrantsNeverRaiseCapacity(t *testing.T) {
	ctx := context.Background()
	parent := limitTestTask("coordinator")
	running := limitTestTask("running")
	running.Labels = map[string]string{labels.LabelParentTask: parent.Name}
	running.Status.Phase = corev1alpha1.TaskPhaseRunning
	forger := limitTestTask("forger")
	forger.Labels = map[string]string{labels.LabelParentTask: parent.Name}
	finished := limitTestTask("finished")
	finished.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	recurring := limitTestTask("recurring")
	recurring.Spec.Schedule = "*/5 * * * *"
	recurring.Status.Phase = corev1alpha1.TaskPhaseScheduled

	for _, kind := range []string{"namespace", "children"} {
		t.Run(kind+" limit full", func(t *testing.T) {
			r := newUnitReconciler(newTestScheme(), parent, running, forger)
			r.APIReader = r.Client
			scope := r.namespaceAdmissionScope("default", 1)
			if kind == "children" {
				scope = r.childrenAdmissionScope(parent, 1)
			}
			writeAdmissionLedger(t, r.Client, scope.ledger, forger.UID)
			if granted, err := r.acquireTaskAdmissionSlot(ctx, forger, scope); err != nil || granted {
				t.Fatalf("granted=%t err=%v, want a self-written grant refused while the slot is held", granted, err)
			}
		})
	}

	t.Run("grants for no eligible Task", func(t *testing.T) {
		r := newUnitReconciler(newTestScheme(), forger, finished, recurring)
		r.APIReader = r.Client
		scope := r.namespaceAdmissionScope("default", 1)
		writeAdmissionLedger(t, r.Client, scope.ledger, "no-such-task", finished.UID, recurring.UID)
		if granted, err := r.acquireTaskAdmissionSlot(ctx, forger, scope); err != nil || !granted {
			t.Fatalf("granted=%t err=%v, want the free slot despite grants naming no eligible Task", granted, err)
		}
		stored := &coordinationv1.Lease{}
		if err := r.Get(ctx, scope.ledger, stored); err != nil {
			t.Fatal(err)
		}
		if got := stored.Annotations[taskAdmissionLedgerAnnotation]; got != `["`+string(forger.UID)+`"]` {
			t.Fatalf("ledger = %s, want only the real grant kept", got)
		}
	})
}

func TestAdmissionLedgersLiveOnlyInTheControllerNamespace(t *testing.T) {
	parent := limitTestTask("coordinator")
	r := newUnitReconciler(newTestScheme(), parent)
	if scope := r.namespaceAdmissionScope("default", 1); scope.ledger.Namespace != "orka-system" {
		t.Fatalf("namespace ledger in %q, want the controller namespace", scope.ledger.Namespace)
	}
	if scope := r.childrenAdmissionScope(parent, 1); scope.ledger.Namespace != "orka-system" {
		t.Fatalf("child ledger in %q, want the controller namespace", scope.ledger.Namespace)
	}
	r.ControllerNamespace = ""
	for _, scope := range []taskAdmissionScope{r.namespaceAdmissionScope("default", 1), r.childrenAdmissionScope(parent, 1)} {
		granted, err := r.acquireTaskAdmissionSlot(context.Background(), parent, scope)
		if granted || !errors.Is(err, errTaskAdmissionLedgerUnavailable) {
			t.Fatalf("granted=%t err=%v, want admission to fail closed without a controller namespace", granted, err)
		}
	}
}

func TestReapChildAdmissionLedgers(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	live := limitTestTask("live-parent")
	child := limitTestTask("child")
	child.Labels = map[string]string{labels.LabelParentTask: live.Name}
	gone := limitTestTask("gone-parent")
	r := newUnitReconciler(newTestScheme(), live, child)
	r.APIReader = r.Client
	for _, tc := range []struct {
		parent *corev1alpha1.Task
		grants []types.UID
	}{{live, []types.UID{child.UID}}, {gone, []types.UID{"old-child"}}} {
		scope := r.childrenAdmissionScope(tc.parent, 1)
		encoded, err := json.Marshal(tc.grants)
		if err != nil {
			t.Fatal(err)
		}
		annotations := map[string]string{taskAdmissionLedgerAnnotation: string(encoded)}
		maps.Copy(annotations, scope.annotations)
		renewed := metav1.NewMicroTime(now)
		if err := r.Create(ctx, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Namespace: scope.ledger.Namespace, Name: scope.ledger.Name, Labels: scope.labels, Annotations: annotations},
			Spec:       coordinationv1.LeaseSpec{RenewTime: &renewed},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.reapTaskAdmissionLedgers(ctx, now); err != nil {
		t.Fatal(err)
	}
	for parent, wantKept := range map[*corev1alpha1.Task]bool{live: true, gone: false} {
		err := r.Get(ctx, r.childrenAdmissionScope(parent, 1).ledger, &coordinationv1.Lease{})
		if kept := err == nil; kept != wantKept {
			t.Errorf("%s child ledger kept=%t (err %v), want %t", parent.Name, kept, err, wantKept)
		}
	}
}

func TestChildAdmissionCountsOnlyThisParentIncarnation(t *testing.T) {
	ctx := context.Background()
	parent := limitTestTask("coordinator")
	parent.UID = "coordinator-new"
	child := func(name string, ownerUID types.UID, phase corev1alpha1.TaskPhase) *corev1alpha1.Task {
		task := limitTestTask(name)
		task.Labels = map[string]string{labels.LabelParentTask: parent.Name}
		task.Status.Phase = phase
		if ownerUID != "" {
			isController := true
			task.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: parent.Name, UID: ownerUID, Controller: &isController,
			}}
		}
		return task
	}
	waiting := child("new-child", parent.UID, corev1alpha1.TaskPhasePending)
	for _, tc := range []struct {
		name        string
		running     *corev1alpha1.Task
		wantGranted bool
	}{
		{name: "old parent's child running", running: child("old-child", "coordinator-old", corev1alpha1.TaskPhaseRunning), wantGranted: true},
		{name: "new parent's child running", running: child("sibling", parent.UID, corev1alpha1.TaskPhaseRunning)},
		{name: "unpinned child running", running: child("unpinned", "", corev1alpha1.TaskPhaseRunning)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newUnitReconciler(newTestScheme(), parent, tc.running, waiting)
			r.APIReader = r.Client
			granted, err := r.acquireTaskAdmissionSlot(ctx, waiting, r.childrenAdmissionScope(parent, 1))
			if err != nil || granted != tc.wantGranted {
				t.Fatalf("granted=%t err=%v, want %t", granted, err, tc.wantGranted)
			}
		})
	}
	oldParent := parent.DeepCopy()
	oldParent.UID = "coordinator-old"
	r := newUnitReconciler(newTestScheme())
	if r.childrenAdmissionScope(oldParent, 1).ledger == r.childrenAdmissionScope(parent, 1).ledger {
		t.Fatal("the old and new parent share a child ledger")
	}
}

func TestAdmissionNeverGrantsATaskItsScopeDoesNotList(t *testing.T) {
	ctx := context.Background()
	parent := limitTestTask("coordinator")
	parent.UID = "coordinator-new"
	child := func(name string, ownerUID types.UID) *corev1alpha1.Task {
		task := limitTestTask(name)
		task.Labels = map[string]string{labels.LabelParentTask: parent.Name}
		isController := true
		task.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: parent.Name, UID: ownerUID, Controller: &isController,
		}}
		return task
	}
	annotated := limitTestTask("old-annotated")
	annotated.Labels = map[string]string{labels.LabelParentTask: parent.Name}
	annotated.Annotations = map[string]string{labels.AnnotationParentTaskUID: "coordinator-old"}
	stale := []*corev1alpha1.Task{child("old-a", "coordinator-old"), child("old-b", "coordinator-old"), child("old-c", "coordinator-old"), annotated}
	first, second := child("new-a", parent.UID), child("new-b", parent.UID)
	objects := make([]client.Object, 0, 3+len(stale))
	objects = append(objects, parent, first, second)
	for _, task := range stale {
		objects = append(objects, task)
	}
	r := newUnitReconciler(newTestScheme(), objects...)
	r.APIReader = r.Client
	scope := r.childrenAdmissionScope(parent, 1)

	for range 2 {
		for _, task := range stale {
			if granted, err := r.acquireTaskAdmissionSlot(ctx, task, scope); err != nil || granted {
				t.Fatalf("%s pinned to the replaced parent: granted=%t err=%v, want no grant", task.Name, granted, err)
			}
		}
	}
	lease := &coordinationv1.Lease{}
	if err := r.Get(ctx, scope.ledger, lease); !apierrors.IsNotFound(err) {
		t.Fatalf("the new parent's ledger was written for children it does not list: %v %v", err, lease.Annotations)
	}

	if granted, err := r.acquireTaskAdmissionSlot(ctx, first, scope); err != nil || !granted {
		t.Fatalf("the new parent's first child: granted=%t err=%v, want it admitted", granted, err)
	}
	if granted, err := r.acquireTaskAdmissionSlot(ctx, second, scope); err != nil || granted {
		t.Fatalf("the new parent's second child: granted=%t err=%v, want the limit of one to hold it", granted, err)
	}
	for _, task := range stale {
		if granted, err := r.acquireTaskAdmissionSlot(ctx, task, scope); err != nil || granted {
			t.Fatalf("%s pinned to the replaced parent: granted=%t err=%v once a slot is held", task.Name, granted, err)
		}
	}
	if err := r.Get(ctx, scope.ledger, lease); err != nil {
		t.Fatal(err)
	}
	grants, err := decodeTaskAdmissionLedger(lease.Annotations[taskAdmissionLedgerAnnotation])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(grants, []types.UID{first.UID}) {
		t.Fatalf("ledger grants = %v, want only %s", grants, first.UID)
	}

	gone := limitTestTask("deleted")
	if granted, err := r.acquireTaskAdmissionSlot(ctx, gone, r.namespaceAdmissionScope("default", 10)); err != nil || granted {
		t.Fatalf("a Task the namespace does not list: granted=%t err=%v, want no grant", granted, err)
	}
}

func TestLoweredLimitElectsOneRetryingHolderAtATime(t *testing.T) {
	for _, scopeKind := range []string{taskAdmissionLedgerKindNamespace, taskAdmissionLedgerKindChildren} {
		t.Run(scopeKind, func(t *testing.T) {
			ctx := context.Background()
			parent := limitTestTask("coordinator")
			created := time.Now().Add(-time.Hour)
			task := func(name string, age time.Duration, retrying bool) *corev1alpha1.Task {
				task := limitTestTask(name)
				if retrying {
					task = limitTestRetryingTask(name)
				}
				task.CreationTimestamp = metav1.NewTime(created.Add(-age))
				if scopeKind == taskAdmissionLedgerKindChildren {
					task.Labels = map[string]string{labels.LabelParentTask: parent.Name}
				}
				return task
			}
			younger, older := task("a-younger", time.Minute, true), task("b-older", 2*time.Minute, true)
			pruned, fresh := task("c-pruned", 0, true), task("d-fresh", 0, false)
			r := newUnitReconciler(newTestScheme(), parent, younger, older, pruned, fresh)
			r.APIReader = r.Client
			scope := r.namespaceAdmissionScope("default", 1)
			if scopeKind == taskAdmissionLedgerKindChildren {
				scope = r.childrenAdmissionScope(parent, 1)
			}
			writeAdmissionLedger(t, r.Client, scope.ledger, younger.UID, older.UID)
			acquire := func(task *corev1alpha1.Task, want bool, when string) {
				t.Helper()
				if granted, err := r.acquireTaskAdmissionSlot(ctx, task, scope); err != nil || granted != want {
					t.Fatalf("%s: %s granted=%t err=%v, want %t", when, task.Name, granted, err, want)
				}
			}
			finish := func(task *corev1alpha1.Task) {
				t.Helper()
				current := &corev1alpha1.Task{}
				if err := r.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
					t.Fatal(err)
				}
				current.Status.Phase = corev1alpha1.TaskPhaseSucceeded
				if err := r.Status().Update(ctx, current); err != nil {
					t.Fatal(err)
				}
			}

			for range 2 {
				acquire(younger, false, "limit lowered")
				acquire(older, true, "limit lowered")
				acquire(pruned, false, "limit lowered")
				acquire(fresh, false, "limit lowered")
			}
			lease := &coordinationv1.Lease{}
			if err := r.Get(ctx, scope.ledger, lease); err != nil {
				t.Fatal(err)
			}
			if grants, err := decodeTaskAdmissionLedger(lease.Annotations[taskAdmissionLedgerAnnotation]); err != nil ||
				!slices.Equal(grants, []types.UID{younger.UID, older.UID}) {
				t.Fatalf("ledger grants = %v (%v), want both retrying holders kept", grants, err)
			}

			finish(older)
			acquire(pruned, false, "after the oldest finished")
			acquire(fresh, false, "after the oldest finished")
			acquire(younger, true, "after the oldest finished")

			finish(younger)
			acquire(fresh, false, "after both granted holders finished")
			acquire(pruned, true, "after both granted holders finished")

			finish(pruned)
			acquire(fresh, true, "after every retrying Task finished")
		})
	}
}

func TestLoweredLimitRetryingHolderWaitsForRunningHolders(t *testing.T) {
	ctx := context.Background()
	running := func(name string) *corev1alpha1.Task {
		task := limitTestTask(name)
		task.Status.Phase = corev1alpha1.TaskPhaseRunning
		task.Status.JobName = name + "-job"
		return task
	}
	first, second, retrying := running("first-running"), running("second-running"), limitTestRetryingTask("retrying")
	r := newUnitReconciler(newTestScheme(), first, second, retrying)
	r.APIReader = r.Client
	scope := r.namespaceAdmissionScope("default", 1)
	writeAdmissionLedger(t, r.Client, scope.ledger, first.UID, second.UID, retrying.UID)
	finish := func(task *corev1alpha1.Task) {
		t.Helper()
		current := &corev1alpha1.Task{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
			t.Fatal(err)
		}
		current.Status.Phase = corev1alpha1.TaskPhaseSucceeded
		if err := r.Status().Update(ctx, current); err != nil {
			t.Fatal(err)
		}
	}

	if granted, err := r.acquireTaskAdmissionSlot(ctx, retrying, scope); err != nil || granted {
		t.Fatalf("two running holders over a limit of one: granted=%t err=%v, want the retrying Task held", granted, err)
	}
	lease := &coordinationv1.Lease{}
	if err := r.Get(ctx, scope.ledger, lease); err != nil {
		t.Fatal(err)
	}
	if grants, err := decodeTaskAdmissionLedger(lease.Annotations[taskAdmissionLedgerAnnotation]); err != nil ||
		!slices.Contains(grants, first.UID) || !slices.Contains(grants, second.UID) {
		t.Fatalf("ledger grants = %v (%v), want the running holders' grants kept", grants, err)
	}
	finish(first)
	if granted, err := r.acquireTaskAdmissionSlot(ctx, retrying, scope); err != nil || granted {
		t.Fatalf("one running holder at a limit of one: granted=%t err=%v, want the retrying Task held", granted, err)
	}
	finish(second)
	if granted, err := r.acquireTaskAdmissionSlot(ctx, retrying, scope); err != nil || !granted {
		t.Fatalf("no running holder: granted=%t err=%v, want the retrying Task admitted", granted, err)
	}
}

func TestAdmissionCountsTasksRunningWithoutAGrant(t *testing.T) {
	ctx := context.Background()
	ownedJob := func(owner *corev1alpha1.Task, mutate func(*batchv1.Job)) *batchv1.Job {
		isController := true
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Namespace: owner.Namespace, Name: buildTaskJobName(owner),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: owner.Name, UID: owner.UID, Controller: &isController,
			}},
		}}
		if mutate != nil {
			mutate(job)
		}
		return job
	}
	for _, tc := range []struct {
		name     string
		holder   func() (*corev1alpha1.Task, []client.Object)
		wantHeld bool
	}{
		{name: "running on a recorded Job", wantHeld: true, holder: func() (*corev1alpha1.Task, []client.Object) {
			task := limitTestTask("recorded")
			task.Status.Phase = corev1alpha1.TaskPhaseRunning
			task.Status.JobName = buildTaskJobName(task)
			return task, []client.Object{ownedJob(task, nil)}
		}},
		{name: "Job created but not recorded", wantHeld: true, holder: func() (*corev1alpha1.Task, []client.Object) {
			task := limitTestTask("unrecorded")
			return task, []client.Object{ownedJob(task, nil)}
		}},
		{name: "unrecorded Job already finished", holder: func() (*corev1alpha1.Task, []client.Object) {
			task := limitTestTask("finished-job")
			return task, []client.Object{ownedJob(task, func(job *batchv1.Job) {
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
			})}
		}},
		{name: "Job deleted externally, retry pending", wantHeld: true, holder: func() (*corev1alpha1.Task, []client.Object) {
			return limitTestRetryingTask("job-deleted"), nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holder, extra := tc.holder()
			waiting := limitTestTask("waiting")
			objects := append([]client.Object{holder, waiting}, extra...)
			r := newUnitReconciler(newTestScheme(), objects...)
			r.APIReader = r.Client
			scope := r.namespaceAdmissionScope("default", 1)
			granted, err := r.acquireTaskAdmissionSlot(ctx, waiting, scope)
			if err != nil || granted == tc.wantHeld {
				t.Fatalf("waiting Task granted=%t err=%v, want held=%t", granted, err, tc.wantHeld)
			}
			if !tc.wantHeld {
				return
			}
			current := &corev1alpha1.Task{}
			if err := r.Get(ctx, client.ObjectKeyFromObject(holder), current); err != nil {
				t.Fatal(err)
			}
			current.Status.Phase = corev1alpha1.TaskPhaseSucceeded
			if err := r.Status().Update(ctx, current); err != nil {
				t.Fatal(err)
			}
			if granted, err := r.acquireTaskAdmissionSlot(ctx, waiting, scope); err != nil || !granted {
				t.Fatalf("after the holder finished: granted=%t err=%v, want admitted", granted, err)
			}
		})
	}
}

func TestAdmissionCountsATaskThatStartsBetweenTheReadAndTheWrite(t *testing.T) {
	ctx := context.Background()
	racer, waiting := limitTestTask("racer"), limitTestTask("waiting")
	r := newUnitReconciler(newTestScheme(), racer, waiting)
	scope := r.namespaceAdmissionScope("default", 1)
	writeAdmissionLedger(t, r.Client, scope.ledger)
	base := r.Client.(client.WithWatch)
	raced := false
	r.Client = interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, delegate client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
			if _, tasks := list.(*corev1alpha1.TaskList); tasks && !raced {
				raced = true
				lease := &coordinationv1.Lease{}
				if err := delegate.Get(ctx, scope.ledger, lease); err != nil {
					return err
				}
				lease.Annotations[taskAdmissionLedgerAnnotation] = `["` + string(racer.UID) + `"]`
				if err := delegate.Update(ctx, lease); err != nil {
					return err
				}
				current := &corev1alpha1.Task{}
				if err := delegate.Get(ctx, client.ObjectKeyFromObject(racer), current); err != nil {
					return err
				}
				current.Status.Phase = corev1alpha1.TaskPhaseRunning
				current.Status.JobName = buildTaskJobName(current)
				if err := delegate.Status().Update(ctx, current); err != nil {
					return err
				}
			}
			return delegate.List(ctx, list, options...)
		},
	})
	r.APIReader = r.Client
	granted, err := r.acquireTaskAdmissionSlot(ctx, waiting, scope)
	if !raced || err != nil || granted {
		t.Fatalf("raced=%t granted=%t err=%v, want the waiting Task held by the Task that started", raced, granted, err)
	}
}
