/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
