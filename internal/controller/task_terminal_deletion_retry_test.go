package controller

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

type terminalDeletionAppendCounter struct {
	store.ExecutionEventStore
	terminalAppends int
}

func (s *terminalDeletionAppendCounter) AppendExecutionEvent(ctx context.Context, event *store.ExecutionEvent) (*store.ExecutionEvent, error) {
	saved, err := s.ExecutionEventStore.AppendExecutionEvent(ctx, event)
	if err == nil && event.Type == events.ExecutionEventTypeTaskCancelled {
		s.terminalAppends++
	}
	return saved, err
}

type terminalDeletionFinalizerPatchFailure struct {
	client.Client
	fail    bool
	failure error
}

func (c *terminalDeletionFinalizerPatchFailure) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	if _, ok := object.(*corev1alpha1.Task); ok && c.fail {
		c.fail = false
		return c.failure
	}
	return c.Client.Patch(ctx, object, patch, options...)
}

func TestTerminalDeletionRetryDoesNotRepublishErasedEvent(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "review-terminal-retry", UID: types.UID("review-terminal-retry-uid"),
			Finalizers: []string{labels.TaskFinalizer}, DeletionTimestamp: &now,
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseCancelled, Message: "cancelled"},
	}
	r := newUnitReconciler(newTestScheme(), task)
	counter := &terminalDeletionAppendCounter{ExecutionEventStore: r.ExecutionEventStore}
	r.ExecutionEventStore = counter
	failure := errors.New("synthetic finalizer patch transport failure")
	r.Client = &terminalDeletionFinalizerPatchFailure{Client: r.Client, fail: true, failure: failure}
	if err := r.recordTaskLifecycleEvent(ctx, task, events.ExecutionEventTypeTaskCancelled, events.ExecutionEventSeverityWarning, task.Status.Message); err != nil {
		t.Fatal(err)
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}
	if _, err := r.Reconcile(ctx, request); !errors.Is(err, failure) {
		t.Fatalf("first reconcile error = %v, want synthetic finalizer failure", err)
	}
	latest := &corev1alpha1.Task{}
	if err := r.Get(ctx, request.NamespacedName, latest); err != nil || !controllerutil.ContainsFinalizer(latest, labels.TaskFinalizer) {
		t.Fatalf("first retry lost original Task finalizer: %v", err)
	}
	erased, err := counter.ListExecutionEvents(ctx, store.ExecutionEventFilter{
		Namespace: task.Namespace, StreamType: store.ExecutionEventStreamTypeTask, StreamID: task.Name, Limit: 10,
	})
	if err != nil || len(erased) != 0 {
		t.Fatalf("expected successful event erasure before failed finalizer patch: count=%d err=%v", len(erased), err)
	}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("retry reconcile = %v", err)
	}
	if counter.terminalAppends != 1 {
		t.Fatalf("successful terminal event publications = %d, want the original single publication; retry recreated erased history", counter.terminalAppends)
	}
}
