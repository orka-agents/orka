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
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

// Contenders for k slots all run eventually, at most k at a time, whether
// they are admitted one at a time or race in parallel, for the namespace limit
// and for a coordinator's child limit. A Pending Task waiting for a slot never
// blocks another, so contenders that outnumber the slots still make progress.
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
		// The longest-running Task finishes and frees its slot.
		if len(running) > 0 {
			setPhase(running[0], corev1alpha1.TaskPhaseSucceeded)
			running = running[1:]
		}
	}
}

// A granted Task keeps its slot on later passes, even before its own state
// shows it admitted, and a holder that finishes frees its slot.
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

// A grant recorded for a Task this reconcile has not seen admitted, such as
// one created and granted moments ago by a concurrent reconcile, is a holder:
// the Tasks are listed after each ledger read, so its grant is never mistaken
// for a finished Task's and pruned.
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

// Namespace ledgers kept in the controller namespace are reaped when their
// namespace is gone, or when no grant is live and none was made for the stale
// interval, never while a grant is live, and never once a grant changed them.
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
				// A grant lands between the reaper's read and its delete.
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

// A ledger never holds more grants than its limit, and every limit is capped
// at MaxTaskAdmissionSlots, so a full ledger of UUID-shaped UIDs stays far
// below the 256KiB Kubernetes allows for all of an object's annotations.
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

// A limit above the cap admits at most MaxTaskAdmissionSlots holders.
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

// writeAdmissionLedger stores grants in a ledger as a forger with write
// access to it would.
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

// A grant in the ledger is not enough to be admitted. A Task that writes its
// own UID into a full namespace or child ledger is still refused, and grants
// naming no live, schedule-free Task of the scope take no capacity.
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

// Ledgers live only in the controller namespace, out of Task authors' reach;
// without one, a limit admits nothing rather than trusting a ledger they
// could write.
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

// Child ledgers are reaped once their coordinator is gone and no child grant
// is live, and kept while a child grant is live.
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

// A coordinator recreated under the same name while a child of the old one is
// still running: the child limit counts only the children pinned to the
// current parent's UID, so the old child does not take the new parent's slot,
// a child pinned to the new parent does, and an unpinned child is counted to
// be safe. Each incarnation has its own ledger.
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
