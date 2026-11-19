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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

// limitTestReconciler returns a reconciler whose informer cache still shows
// peer before its Pending write, while the API server already has it Pending,
// as when another Task's reconcile admitted peer concurrently.
func limitTestReconciler(t *testing.T, task, peer *corev1alpha1.Task, objects ...client.Object) *TaskReconciler {
	t.Helper()
	scheme := newTestScheme()
	cachedPeer := peer.DeepCopy()
	cachedPeer.Status.Phase = ""
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

func limitTestTask(name string, phase corev1alpha1.TaskPhase) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Finalizers: []string{labels.TaskFinalizer}},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status:     corev1alpha1.TaskStatus{Phase: phase},
	}
}

// Parallel reconciles must not admit past the namespace limit: a Task the
// cache has not yet seen as Pending is counted from the API server.
func TestHandlePendingNamespaceLimitCountsConcurrentlyAdmittedTasks(t *testing.T) {
	task := limitTestTask("checking", corev1alpha1.TaskPhasePending)
	peer := limitTestTask("admitted-concurrently", corev1alpha1.TaskPhasePending)
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
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, AgentRef: &corev1alpha1.AgentReference{Name: parentAgent.Name}},
	}
	child := func(name string) *corev1alpha1.Task {
		task := limitTestTask(name, corev1alpha1.TaskPhasePending)
		task.Labels = map[string]string{labels.LabelParentTask: parent.Name}
		task.Annotations = map[string]string{labels.AnnotationCoordinationDepth: "1"}
		return task
	}
	task, sibling := child("checking-child"), child("admitted-sibling")
	r := limitTestReconciler(t, task, sibling, parentAgent, parent)

	result, err, handled := r.validateCoordinationConstraints(context.Background(), task)
	if err != nil {
		t.Fatalf("validateCoordinationConstraints() error = %v", err)
	}
	if !handled || result.RequeueAfter != 10*time.Second {
		t.Fatalf("handled=%t RequeueAfter=%v, want the child limit to hold the second child", handled, result.RequeueAfter)
	}
}
