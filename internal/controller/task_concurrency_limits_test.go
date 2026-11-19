/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

// limitTestReconciler returns a reconciler whose informer cache still shows
// peer before its admission, while the API server already has it admitted, as
// when another Task's reconcile admitted peer concurrently.
func limitTestReconciler(t *testing.T, task, peer *corev1alpha1.Task, objects ...client.Object) *TaskReconciler {
	t.Helper()
	scheme := newTestScheme()
	cachedPeer := peer.DeepCopy()
	cachedPeer.Status = corev1alpha1.TaskStatus{}
	r := newUnitReconciler(scheme, append([]client.Object{task, cachedPeer}, objects...)...)
	r.APIReader = fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(append([]client.Object{task.DeepCopy(), peer.DeepCopy()}, objectCopies(objects)...)...).Build()
	return r
}

func objectCopies(objects []client.Object) []client.Object {
	copies := make([]client.Object, 0, len(objects))
	for _, object := range objects {
		copies = append(copies, object.DeepCopyObject().(client.Object))
	}
	return copies
}

// limitTestTask is a Pending container Task.
func limitTestTask(name string) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name + "-uid"), Finalizers: []string{labels.TaskFinalizer}},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
}

// Parallel reconciles must not admit past the namespace limit: a Task the
// cache has not yet seen as Pending is counted from the API server.
func TestHandlePendingNamespaceLimitCountsConcurrentlyAdmittedTasks(t *testing.T) {
	task := limitTestTask("checking")
	peer := limitTestTask("admitted-concurrently")
	peer.Status.JobName = "admitted-concurrently-job"
	r := limitTestReconciler(t, task, peer)
	r.MaxTasksPerNamespace = 1

	result, err := r.handlePending(context.Background(), task)
	if err != nil {
		t.Fatalf("handlePending() error = %v", err)
	}
	if result.RequeueAfter != 10*time.Second {
		t.Fatalf("RequeueAfter = %v, want the namespace limit to hold the second Task", result.RequeueAfter)
	}
	assertNoJobsForTask(t, r, task)
}

// The same holds for a parent's maxConcurrentChildren.
func TestCoordinationChildLimitCountsConcurrentlyAdmittedSiblings(t *testing.T) {
	parentAgent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coordinator", Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{Coordination: &corev1alpha1.CoordinationConfig{
			Enabled: true, MaxConcurrentChildren: 1,
		}},
	}
	parent := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default", UID: "parent-uid"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, AgentRef: &corev1alpha1.AgentReference{Name: parentAgent.Name}},
	}
	child := func(name string) *corev1alpha1.Task {
		task := limitTestTask(name)
		task.Labels = map[string]string{labels.LabelParentTask: parent.Name}
		task.Annotations = map[string]string{labels.AnnotationCoordinationDepth: "1"}
		return task
	}
	task, sibling := child("checking-child"), child("admitted-sibling")
	sibling.Status.JobName = "admitted-sibling-job"
	r := limitTestReconciler(t, task, sibling, parentAgent, parent)

	result, err, handled := r.validateCoordinationConstraints(context.Background(), task)
	if err != nil {
		t.Fatalf("validateCoordinationConstraints() error = %v", err)
	}
	if !handled || result.RequeueAfter != 10*time.Second {
		t.Fatalf("handled=%t RequeueAfter=%v, want the child limit to hold the second child", handled, result.RequeueAfter)
	}
}

// Two Pending Tasks waiting under a limit of one used to count each other
// and wait forever. Waiting Tasks no longer count: the first to take the slot
// proceeds and the other waits for it.
func TestHandlePendingNamespaceLimitAdmitsOneOfTwoWaitingTasks(t *testing.T) {
	first := limitTestTask("first-waiting")
	second := limitTestTask("second-waiting")
	r := newUnitReconciler(newTestScheme(), first, second)
	r.APIReader = r.Client
	r.MaxTasksPerNamespace = 1

	result, err := r.handlePending(context.Background(), first.DeepCopy())
	if err != nil {
		t.Fatalf("first handlePending() error = %v", err)
	}
	if result.RequeueAfter == 10*time.Second {
		t.Fatal("the first waiting Task was held although no Task held the slot")
	}
	result, err = r.handlePending(context.Background(), second.DeepCopy())
	if err != nil {
		t.Fatalf("second handlePending() error = %v", err)
	}
	if result.RequeueAfter != 10*time.Second {
		t.Fatalf("second RequeueAfter = %v, want it to wait for the slot", result.RequeueAfter)
	}
	// Only the admitted Task created its Job.
	stored := &corev1alpha1.Task{}
	for _, task := range []*corev1alpha1.Task{first, second} {
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), stored); err != nil {
			t.Fatal(err)
		}
		if admitted := stored.Status.JobName != ""; admitted != (task == first) {
			t.Fatalf("%s JobName = %q, want only the first Task admitted", task.Name, stored.Status.JobName)
		}
	}
}

// A recurring Task's scheduling parent stays Scheduled and runs nothing
// itself, so it never holds a slot: under a limit of one, both its own child
// run and an unrelated Task proceed.
func TestHandlePendingRecurringParentHoldsNoSlot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		labeled bool
	}{{name: "child run", labeled: true}, {name: "unrelated Task"}} {
		t.Run(tc.name, func(t *testing.T) {
			parent := limitTestTask("recurring-parent")
			parent.Spec.Schedule = "*/5 * * * *"
			parent.Status.Phase = corev1alpha1.TaskPhaseScheduled
			waiting := limitTestTask("run")
			if tc.labeled {
				waiting.Labels = map[string]string{labels.LabelParentTask: parent.Name}
			}
			r := newUnitReconciler(newTestScheme(), parent, waiting)
			r.APIReader = r.Client
			r.MaxTasksPerNamespace = 1

			result, err := r.handlePending(context.Background(), waiting.DeepCopy())
			if err != nil {
				t.Fatalf("handlePending() error = %v", err)
			}
			if result.RequeueAfter == 10*time.Second {
				t.Fatal("the recurring parent held the only namespace slot")
			}
			stored := &corev1alpha1.Task{}
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(waiting), stored); err != nil {
				t.Fatal(err)
			}
			if stored.Status.JobName == "" {
				t.Fatal("the waiting Task did not run")
			}
		})
	}
}
