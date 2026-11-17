/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/taskwebhook"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// blockingApprovalReceiver holds every request until released and then
// answers 503, standing in for a slow, failing receiver. It reports the
// stored Task as it was when each request arrived.
type blockingApprovalReceiver struct {
	server  *httptest.Server
	arrived chan *corev1alpha1.Task
	release chan struct{}
	once    sync.Once
}

// unblock releases every held request, now and later.
func (r *blockingApprovalReceiver) unblock() { r.once.Do(func() { close(r.release) }) }

func newBlockingApprovalReceiver(t *testing.T, read func() *corev1alpha1.Task) *blockingApprovalReceiver {
	t.Helper()
	receiver := &blockingApprovalReceiver{arrived: make(chan *corev1alpha1.Task, 4), release: make(chan struct{})}
	receiver.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		receiver.arrived <- read()
		<-receiver.release
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(receiver.server.Close)
	// Cleanups run last-registered first, so a failing test releases a held
	// request before the server waits for it to finish.
	t.Cleanup(receiver.unblock)
	return receiver
}

// A pending approval notice never holds back the Task's own phase work: the
// phase transition is persisted before the notice is sent, while the receiver
// still hangs, and the reconcile then waits for the notice's retry backoff.
func TestTaskReconcilerAdvancesThePhaseBeforeASlowApprovalReceiver(t *testing.T) {
	task := newApprovalWebhookTask(corev1alpha1.TaskPhasePending, bothApprovalEvents...)
	task.Spec.Type = corev1alpha1.TaskTypeContainer
	task.Finalizers = []string{labels.TaskFinalizer}
	// A recorded outcome makes the Pending phase handler complete the Task.
	task.Status.ExecutionOutcome = &corev1alpha1.TaskWorkloadExecutionOutcome{
		Phase: corev1alpha1.TaskPhaseFailed, Message: "recorded outcome",
	}
	h := newApprovalWebhookHarness(t, task)
	receiver := newBlockingApprovalReceiver(t, func() *corev1alpha1.Task {
		stored := &corev1alpha1.Task{}
		if err := h.client.Get(context.Background(), ctrlclient.ObjectKeyFromObject(task), stored); err != nil {
			return nil
		}
		return stored
	})
	stored := h.task()
	stored.Spec.WebhookURL = receiver.server.URL + "/hook"
	if err := h.client.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	h.request(approvalWebhookTestUID, "approval-1")
	reconciler := &TaskReconciler{
		Client: h.client, Scheme: h.client.Scheme(), ExecutionEventStore: h.events,
		WebhookNotifier: newTestWebhookNotifier(t, h.keys, nil),
	}

	type outcome struct {
		result ctrl.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := reconciler.Reconcile(context.Background(),
			ctrl.Request{NamespacedName: types.NamespacedName{Namespace: defaultNS, Name: approvalWebhookTestTask}})
		done <- outcome{result, err}
	}()

	select {
	case seen := <-receiver.arrived:
		if seen == nil {
			t.Fatal("the Task could not be read at the approval attempt")
		}
		if seen.Status.Phase != corev1alpha1.TaskPhaseFailed {
			t.Fatalf("phase at the approval attempt = %s, want its phase work already persisted as Failed", seen.Status.Phase)
		}
	case got := <-done:
		t.Fatalf("reconcile returned without attempting the approval notice: %+v, %v", got.result, got.err)
	case <-time.After(30 * time.Second):
		t.Fatal("the approval notice was never attempted")
	}
	receiver.unblock()
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}

	notice := h.task().Status.ApprovalWebhooks.Notices[0]
	if notice.State != corev1alpha1.TaskWebhookDeliveryPending || notice.Attempts != 1 || notice.NextAttemptTime == nil {
		t.Fatalf("notice after a failed attempt = %+v, want pending with its retry recorded", notice.TaskWebhookDelivery)
	}
	retry := time.Until(notice.NextAttemptTime.Time)
	if got.result.RequeueAfter <= 0 || got.result.RequeueAfter > retry+time.Second {
		t.Fatalf("requeue = %v, want the notice's retry in %v", got.result.RequeueAfter, retry)
	}
}

// A phase handler that fails does not stop the approval notice of a finished
// Task, and its error is still reported.
func TestTaskReconcilerDeliversApprovalNoticesWhenThePhaseWorkFails(t *testing.T) {
	task := newApprovalWebhookTask(corev1alpha1.TaskPhaseSucceeded, bothApprovalEvents...)
	task.Spec.Type = corev1alpha1.TaskTypeContainer
	task.Finalizers = []string{labels.TaskFinalizer}
	task.Status.JobName = "approval-task-job"
	h := newApprovalWebhookHarness(t, task)
	jobLookupFailed := errors.New("job lookup failed")
	h.client = fake.NewClientBuilder().WithScheme(newTestScheme()).WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(task, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: defaultNS, UID: "namespace-uid"}}).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c ctrlclient.WithWatch, key ctrlclient.ObjectKey, obj ctrlclient.Object, opts ...ctrlclient.GetOption) error {
			if _, ok := obj.(*batchv1.Job); ok {
				return jobLookupFailed
			}
			return c.Get(ctx, key, obj, opts...)
		}}).Build()
	h.request(approvalWebhookTestUID, "approval-1")
	reconciler := &TaskReconciler{
		Client: h.client, Scheme: h.client.Scheme(), ExecutionEventStore: h.events,
		WebhookNotifier: newTestWebhookNotifier(t, h.keys, nil),
	}

	_, err := reconciler.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: defaultNS, Name: approvalWebhookTestTask}})
	if !errors.Is(err, jobLookupFailed) {
		t.Fatalf("reconcile error = %v, want the phase error", err)
	}
	if got := eventTypes(h.receipts()); !slices.Equal(got, []string{"task.approval_requested"}) {
		t.Fatalf("approval notices = %v, want the request sent despite the phase error", got)
	}
	if notice := h.task().Status.ApprovalWebhooks.Notices[0]; notice.State != corev1alpha1.TaskWebhookDeliveryDelivered {
		t.Fatalf("notice = %+v, want Delivered", notice.TaskWebhookDelivery)
	}
}

// Once its last pending notice settles, the Task is requeued promptly, so the
// phase work that ran before the attempt runs again.
func TestTaskReconcilerRequeuesWhenTheLastApprovalNoticeSettles(t *testing.T) {
	task := newApprovalWebhookTask(corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskWebhookEventApprovalRequested)
	task.Spec.Type = corev1alpha1.TaskTypeContainer
	task.Finalizers = []string{labels.TaskFinalizer}
	h := newApprovalWebhookHarness(t, task)
	h.request(approvalWebhookTestUID, "approval-1")
	reconciler := &TaskReconciler{
		Client: h.client, Scheme: h.client.Scheme(), ExecutionEventStore: h.events,
		WebhookNotifier: newTestWebhookNotifier(t, h.keys, nil),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: defaultNS, Name: approvalWebhookTestTask}}

	settled := false
	for range 4 {
		result, err := reconciler.Reconcile(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if notices := h.task().Status.ApprovalWebhooks.Notices; len(notices) == 1 && notices[0].State == corev1alpha1.TaskWebhookDeliveryDelivered {
			if result.RequeueAfter <= 0 || result.RequeueAfter > approvalWebhookSettledRequeue {
				t.Fatalf("requeue after the last notice settled = %v, want at most %v", result.RequeueAfter, approvalWebhookSettledRequeue)
			}
			settled = true
			break
		}
	}
	if !settled {
		t.Fatalf("the approval notice never settled: %v", eventTypes(h.receipts()))
	}
}

// A parked turn that Succeeded still has its sign-off notices delivered by
// the Task reconciler, after its phase work, while its terminal notice stays
// deferred.
func TestTaskReconcilerDeliversParkedSignOffNoticesAfterThePhaseWork(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindApproval, corev1alpha1.TaskPhaseSucceeded)
	task.Spec.WebhookEvents = []corev1alpha1.TaskWebhookEvent{corev1alpha1.TaskWebhookEventApprovalRequested}
	task.Status.ApprovalWebhooks = &corev1alpha1.TaskApprovalWebhooks{Tracking: true}
	h := newParkedWebhookHarness(t, task)
	h.settle("parked")
	if state := h.task("parked").Status.Wait.State; state != corev1alpha1.TaskWaitStateWaiting {
		t.Fatalf("wait state = %s, want Waiting", state)
	}
	reconciler := &TaskReconciler{
		Client: h.client, APIReader: h.client, Scheme: h.client.Scheme(), ExecutionEventStore: h.events,
		WebhookNotifier: newTestWebhookNotifier(t, h.keys, nil),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: taskWaitTestNamespace, Name: "parked"}}
	for range 4 {
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}

	receipts := h.receiver.all()
	sent := make([]string, 0, len(receipts))
	for _, receipt := range receipts {
		sent = append(sent, receipt.header.Get(taskwebhook.HeaderEvent))
	}
	if !slices.Equal(sent, []string{"task.approval_requested"}) {
		t.Fatalf("sent = %v, want only the sign-off request", sent)
	}
	stored := h.task("parked")
	if stored.Status.WebhookDelivered || stored.Status.WebhookDelivery != nil {
		t.Fatalf("parked Task recorded a terminal notice: %+v", stored.Status.WebhookDelivery)
	}
	if stored.Status.ApprovalWebhooks.Notices[0].State != corev1alpha1.TaskWebhookDeliveryDelivered {
		t.Fatalf("notice = %+v, want Delivered", stored.Status.ApprovalWebhooks.Notices[0].TaskWebhookDelivery)
	}
}
