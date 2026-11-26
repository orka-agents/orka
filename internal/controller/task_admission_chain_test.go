/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

func admissionChainTask(name string, taskType corev1alpha1.TaskType) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name + "-uid")},
		Spec:       corev1alpha1.TaskSpec{Type: taskType, Prompt: "work"},
	}
}

func TestReconcileChainsAgentTaskAdmissionInOnePass(t *testing.T) {
	task := admissionChainTask("chained", corev1alpha1.TaskTypeAgent)
	r := newUnitReconciler(newTestScheme(), task, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "ns-uid"}})
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}); err != nil {
		t.Fatal(err)
	}
	stored := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), stored); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(stored, labels.TaskFinalizer) {
		t.Fatal("the first pass did not add the finalizer")
	}
	if stored.Status.Phase == "" || stored.Status.Phase == corev1alpha1.TaskPhasePending {
		t.Fatalf("phase = %q, want the pass to continue past Pending into admission", stored.Status.Phase)
	}
}

func TestReconcileKeepsOneStepPerPassForJobTasks(t *testing.T) {
	task := admissionChainTask("container", corev1alpha1.TaskTypeContainer)
	r := newUnitReconciler(newTestScheme(), task, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "ns-uid"}})
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)})
	if err != nil {
		t.Fatal(err)
	}
	stored := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), stored); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(stored, labels.TaskFinalizer) || stored.Status.Phase != "" || result.RequeueAfter != time.Second {
		t.Fatalf("finalizer=%t phase=%q requeue=%v, want only the finalizer and a 1s requeue",
			controllerutil.ContainsFinalizer(stored, labels.TaskFinalizer), stored.Status.Phase, result.RequeueAfter)
	}
}

func TestInitializePendingStatusFallsBackOnConflict(t *testing.T) {
	task := admissionChainTask("conflicting", corev1alpha1.TaskTypeAgent)
	r := newUnitReconciler(newTestScheme(), task)
	conflicted := false
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, delegate client.Client, subResource string, object client.Object, options ...client.SubResourceUpdateOption) error {
			if !conflicted {
				conflicted = true
				return apierrors.NewConflict(schema.GroupResource{Group: "core.orka.ai", Resource: "tasks"}, object.GetName(), nil)
			}
			return delegate.SubResource(subResource).Update(ctx, object, options...)
		},
	})
	current := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	if err := r.initializePendingStatus(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if !conflicted || current.Status.Phase != corev1alpha1.TaskPhasePending {
		t.Fatalf("conflicted=%t phase=%q, want the retry to write Pending", conflicted, current.Status.Phase)
	}
}
