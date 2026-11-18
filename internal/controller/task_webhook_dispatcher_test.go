/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// attachTestWebhookDispatcher gives r the delivery runnable SetupWithManager
// would register, without starting its workers. Tests run its queued passes
// with drain.
func attachTestWebhookDispatcher(t *testing.T, r *TaskReconciler) *taskWebhookDispatcher {
	t.Helper()
	r.webhookDeliveries = newTaskWebhookDispatcher(r.WebhookNotifier, uncachedReader(r.APIReader, r.Client), r.Status(), 1)
	t.Cleanup(r.webhookDeliveries.queue.ShutDown)
	return r.webhookDeliveries
}

// drain runs, in the caller's goroutine, every queued delivery pass that is
// due now. Passes the dispatcher scheduled for later are left queued.
func (d *taskWebhookDispatcher) drain(ctx context.Context) {
	for d.queue.Len() > 0 {
		d.processNext(ctx)
	}
}

// startTestWebhookDispatcher gives r a running delivery runnable with the
// given number of workers, stopped when the test ends.
func startTestWebhookDispatcher(t *testing.T, r *TaskReconciler, workers int) {
	t.Helper()
	r.webhookDeliveries = newTaskWebhookDispatcher(r.WebhookNotifier, uncachedReader(r.APIReader, r.Client), r.Status(), workers)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = r.webhookDeliveries.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
}

// hangingWebhookReceiver accepts every request and answers none until the
// test ends, like a receiver that hangs past the request timeout.
type hangingWebhookReceiver struct {
	server  *httptest.Server
	arrived chan struct{}
	release chan struct{}
	once    sync.Once
}

func newHangingWebhookReceiver(t *testing.T) *hangingWebhookReceiver {
	t.Helper()
	receiver := &hangingWebhookReceiver{arrived: make(chan struct{}, 16), release: make(chan struct{})}
	receiver.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receiver.arrived <- struct{}{}
		select {
		case <-receiver.release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(receiver.server.Close)
	// Cleanups run last-registered first: release held requests before the
	// server waits for them and before the dispatcher stops.
	t.Cleanup(func() { receiver.once.Do(func() { close(receiver.release) }) })
	return receiver
}

func completedWebhookTask(name, webhookURL string) *corev1alpha1.Task {
	completion := metav1.NewTime(time.Now().Add(-time.Minute))
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: defaultNS, UID: types.UID(name + "-uid"),
			Finalizers: []string{labels.TaskFinalizer},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer, WebhookURL: webhookURL},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded, StartTime: &completion, CompletionTime: &completion,
		},
	}
}

// A receiver that hangs must not hold the Task reconcile worker. Before the
// delivery runnable, the reconciler sent each attempt inline, so every Task
// waited behind a hanging receiver's request timeout. Here the reconcile of
// the slow Task, of another Task with a webhook and of a Task without one all
// return while the slow receiver still holds its request, and the other
// Task's notice is delivered meanwhile.
func TestHangingWebhookReceiverDoesNotBlockOtherTasks(t *testing.T) {
	hanging := newHangingWebhookReceiver(t)
	fast := newWebhookReceiver(t, http.StatusOK)
	slowTask := completedWebhookTask("slow", hanging.server.URL+"/hook")
	fastTask := completedWebhookTask("fast", fast.server.URL+"/hook")
	plainTask := completedWebhookTask("plain", "")
	r := newUnitReconciler(newTestScheme(), slowTask, fastTask, plainTask,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: defaultNS, UID: "namespace-uid"}})
	r.WebhookNotifier = newTestWebhookNotifier(t, newWebhookTestKeys(t), nil)
	startTestWebhookDispatcher(t, r, 2)

	reconcile := func(task *corev1alpha1.Task) ctrl.Result {
		t.Helper()
		started := time.Now()
		result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: ctrlclient.ObjectKeyFromObject(task)})
		if err != nil {
			t.Fatalf("Reconcile(%s) error = %v", task.Name, err)
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Fatalf("Reconcile(%s) took %v while a webhook receiver hangs", task.Name, elapsed)
		}
		return result
	}
	stored := func(task *corev1alpha1.Task) *corev1alpha1.Task {
		t.Helper()
		current := &corev1alpha1.Task{}
		if err := r.Get(context.Background(), ctrlclient.ObjectKeyFromObject(task), current); err != nil {
			t.Fatal(err)
		}
		return current
	}

	// Reconcile the slow Task until its notice is handed off and the
	// receiver holds the attempt.
	deadline := time.After(20 * time.Second)
	for arrived := false; !arrived; {
		reconcile(slowTask)
		select {
		case <-hanging.arrived:
			arrived = true
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatal("the slow Task's notice was never attempted")
		}
	}

	// While it hangs, other Tasks reconcile and the other notice is sent.
	reconcile(plainTask)
	for delivered := false; !delivered; {
		reconcile(fastTask)
		delivered = stored(fastTask).Status.WebhookDelivered
		if !delivered {
			select {
			case <-time.After(50 * time.Millisecond):
			case <-deadline:
				t.Fatalf("the other Task's notice was not delivered while a receiver hangs; receipts = %d", len(fast.all()))
			}
		}
	}
	if got := len(fast.all()); got != 1 {
		t.Fatalf("fast receiver receipts = %d, want 1", got)
	}

	// The slow Task's own reconcile returns too, waiting out the persisted
	// lease of the attempt in flight, which is not repeated.
	slow := stored(slowTask)
	delivery := slow.Status.WebhookDelivery
	if delivery == nil || delivery.State != corev1alpha1.TaskWebhookDeliveryPending || delivery.Attempts != 1 || delivery.NextAttemptTime == nil {
		t.Fatalf("slow delivery while its attempt hangs = %+v, want one claimed attempt", delivery)
	}
	if result := reconcile(slowTask); result.RequeueAfter <= 0 || result.RequeueAfter > time.Until(delivery.NextAttemptTime.Time)+time.Second {
		t.Fatalf("slow Task requeue while its attempt hangs = %v, want the lease ending at %v", result.RequeueAfter, delivery.NextAttemptTime)
	}
	select {
	case <-hanging.arrived:
		t.Fatal("the slow notice was attempted twice under one lease")
	default:
	}
}

// concurrentWriteHarness sends one terminal notice while another writer
// changes the Task between the attempt's claim and its result.
type concurrentWriteHarness struct {
	t        *testing.T
	r        *TaskReconciler
	task     *corev1alpha1.Task
	clock    *webhookTestClock
	receiver *webhookReceiver
	moved    *webhookReceiver
}

func newConcurrentWriteHarness(t *testing.T) *concurrentWriteHarness {
	t.Helper()
	h := &concurrentWriteHarness{
		t: t, receiver: newWebhookReceiver(t, http.StatusOK), moved: newWebhookReceiver(t, http.StatusOK),
		clock: &webhookTestClock{now: time.Now().Truncate(time.Second)},
	}
	h.task = completedWebhookTask("concurrent", h.receiver.server.URL+"/hook")
	h.r = newUnitReconciler(newTestScheme(), h.task)
	h.r.WebhookNotifier = newTestWebhookNotifier(t, newWebhookTestKeys(t), h.clock)
	return h
}

func (h *concurrentWriteHarness) read() *corev1alpha1.Task {
	h.t.Helper()
	current := &corev1alpha1.Task{}
	if err := h.r.Get(context.Background(), ctrlclient.ObjectKeyFromObject(h.task), current); err != nil {
		h.t.Fatal(err)
	}
	return current
}

// attemptDuring runs one attempt, with write committed after its claim and
// before its result, and returns the Task afterwards.
func (h *concurrentWriteHarness) attemptDuring(write func(context.Context, *corev1alpha1.Task) error) *corev1alpha1.Task {
	h.t.Helper()
	status := &conflictOnceTaskStatus{rereadingTaskStatus: rereadingTaskStatus{writer: h.r.Status(), reader: h.r.Client}}
	status.beforeResult = func(ctx context.Context) {
		if err := write(ctx, h.read()); err != nil {
			h.t.Fatal(err)
		}
	}
	if _, err := h.r.WebhookNotifier.reconcileDelivery(context.Background(), status, h.read()); err != nil {
		h.t.Fatalf("reconcileDelivery() error = %v", err)
	}
	if !status.conflicted {
		h.t.Fatal("the result write never met the concurrent status write")
	}
	current := h.read()
	if delivery := current.Status.WebhookDelivery; len(h.receiver.all()) != 1 || delivery == nil || delivery.Attempts != 1 {
		h.t.Fatalf("after one attempt: receipts = %d, delivery = %+v", len(h.receiver.all()), delivery)
	}
	return current
}

// afterLease checks that the stale attempt left its claim pending, then
// runs the next pass once the claim's lease ends.
func (h *concurrentWriteHarness) afterLease(stale *corev1alpha1.Task) *corev1alpha1.Task {
	h.t.Helper()
	first := stale.Status.WebhookDelivery
	if stale.Status.WebhookDelivered || first.State != corev1alpha1.TaskWebhookDeliveryPending || first.NextAttemptTime == nil {
		h.t.Fatalf("stale notice = delivered %v, %+v; want its claim left pending", stale.Status.WebhookDelivered, first)
	}
	h.clock.now = first.NextAttemptTime.Time
	if _, err := h.r.WebhookNotifier.reconcileDelivery(context.Background(), h.r.Status(), h.read()); err != nil {
		h.t.Fatalf("reconcileDelivery() after the lease error = %v", err)
	}
	return h.read()
}

// An attempt's result survives a status write that another writer, such as
// the Task reconciler, committed while the request was in flight, as long as
// the Task still renders the notice that was sent.
func TestWebhookAttemptResultSurvivesAnUnrelatedStatusWrite(t *testing.T) {
	h := newConcurrentWriteHarness(t)
	current := h.attemptDuring(func(ctx context.Context, task *corev1alpha1.Task) error {
		task.Status.Conditions = append(task.Status.Conditions, metav1.Condition{
			Type: "Unrelated", Status: metav1.ConditionTrue, Reason: "Test", LastTransitionTime: metav1.Now(),
		})
		return h.r.Status().Update(ctx, task)
	})
	delivery := current.Status.WebhookDelivery
	if !current.Status.WebhookDelivered || delivery.State != corev1alpha1.TaskWebhookDeliveryDelivered || len(current.Status.Conditions) != 1 {
		t.Fatalf("status = delivered %v, %+v, conditions %v; want the result and the other write kept",
			current.Status.WebhookDelivered, delivery, current.Status.Conditions)
	}
}

// When the concurrent write changed the Task's terminal state, the sent
// notice is stale: its result is not recorded, and once the claim's lease
// ends the notice is superseded by one that carries the newer state, as ADR
// 0042 requires.
func TestWebhookAttemptResultIsNotRecordedForAChangedTerminalState(t *testing.T) {
	h := newConcurrentWriteHarness(t)
	stale := h.attemptDuring(func(ctx context.Context, task *corev1alpha1.Task) error {
		task.Status.Message = "updated by the reconciler"
		return h.r.Status().Update(ctx, task)
	})
	current := h.afterLease(stale)
	first, second := stale.Status.WebhookDelivery, current.Status.WebhookDelivery
	if !current.Status.WebhookDelivered || second.DeliveryID == first.DeliveryID || second.Superseded != 1 ||
		second.State != corev1alpha1.TaskWebhookDeliveryDelivered || len(h.receiver.all()) != 2 {
		t.Fatalf("after the lease: delivered %v, %+v, receipts %d; want a superseding notice with the newer state",
			current.Status.WebhookDelivered, second, len(h.receiver.all()))
	}
	var payload WebhookPayload
	if err := json.Unmarshal(h.receiver.all()[1].body, &payload); err != nil || payload.Message != "updated by the reconciler" {
		t.Fatalf("superseding payload = %+v, %v; want the newer message", payload, err)
	}
}

// A Task whose spec is still mutable may move its receiver while an attempt
// is in flight. The body does not cover the URL, so the old receiver's
// success is not recorded, and the same notice is retried at the new one.
func TestWebhookAttemptResultIsNotRecordedForAMovedReceiver(t *testing.T) {
	h := newConcurrentWriteHarness(t)
	stale := h.attemptDuring(func(ctx context.Context, task *corev1alpha1.Task) error {
		task.Spec.WebhookURL = h.moved.server.URL + "/hook"
		return h.r.Update(ctx, task)
	})
	current := h.afterLease(stale)
	first, second := stale.Status.WebhookDelivery, current.Status.WebhookDelivery
	if !current.Status.WebhookDelivered || second.DeliveryID != first.DeliveryID || second.Attempts != 2 ||
		len(h.receiver.all()) != 1 || len(h.moved.all()) != 1 {
		t.Fatalf("after the lease: delivered %v, %+v, receipts old %d new %d; want the notice retried at the new receiver",
			current.Status.WebhookDelivered, second, len(h.receiver.all()), len(h.moved.all()))
	}
}

// conflictOnceTaskStatus runs beforeResult just before the second status
// write, which records the first attempt's result after its claim.
type conflictOnceTaskStatus struct {
	rereadingTaskStatus
	writes       int
	beforeResult func(context.Context)
	conflicted   bool
}

func (s *conflictOnceTaskStatus) Update(ctx context.Context, obj ctrlclient.Object, opts ...ctrlclient.SubResourceUpdateOption) error {
	s.writes++
	if s.writes == 2 {
		s.beforeResult(ctx)
	}
	err := s.rereadingTaskStatus.Update(ctx, obj, opts...)
	if apierrors.IsConflict(err) {
		s.conflicted = true
	}
	return err
}

// reconcileThenDeliver runs one Task reconcile and then the webhook delivery
// passes it queued, as the manager's delivery workers would.
func reconcileThenDeliver(ctx context.Context, r *TaskReconciler, request ctrl.Request) (ctrl.Result, error) {
	result, err := r.Reconcile(ctx, request)
	if r.webhookDeliveries != nil {
		r.webhookDeliveries.drain(ctx)
	}
	return result, err
}

// A Task deleted while one of its approval notices is in flight stays behind
// its finalizer. The pass must not go on to send its terminal notice once
// recording the approval result has observed the deletion.
func TestWebhookDeliveryStopsAfterObservingDeletion(t *testing.T) {
	var r *TaskReconciler
	var task *corev1alpha1.Task
	var receipts []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		receipts = append(receipts, req.Header.Get("X-Orka-Event"))
		first := len(receipts) == 1
		mu.Unlock()
		if first {
			if err := r.Delete(context.Background(), task.DeepCopy()); err != nil {
				t.Errorf("delete Task during the approval attempt: %v", err)
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	task = completedWebhookTask("deleted", server.URL+"/hook")
	task.Spec.WebhookEvents = []corev1alpha1.TaskWebhookEvent{corev1alpha1.TaskWebhookEventApprovalRequested}
	now := time.Now().Truncate(time.Second)
	notice := corev1alpha1.TaskApprovalWebhookNotice{
		Transition: corev1alpha1.TaskApprovalWebhookRequested, StreamSeq: 1, ApprovalID: "approval-1",
	}
	if err := sealApprovalNotice(task, &notice, now, now); err != nil {
		t.Fatal(err)
	}
	task.Status.ApprovalWebhooks = &corev1alpha1.TaskApprovalWebhooks{
		Tracking: true, ProjectedSeq: 1, Notices: []corev1alpha1.TaskApprovalWebhookNotice{notice},
	}
	r = newUnitReconciler(newTestScheme(), task)
	r.WebhookNotifier = newTestWebhookNotifier(t, newWebhookTestKeys(t), nil)
	deliveries := attachTestWebhookDispatcher(t, r)

	stored := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), ctrlclient.ObjectKeyFromObject(task), stored); err != nil {
		t.Fatal(err)
	}
	deliveries.enqueue(stored, true)
	deliveries.drain(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(receipts) != 1 || receipts[0] != string(corev1alpha1.TaskWebhookEventApprovalRequested) {
		t.Fatalf("receipts = %v, want only the approval notice in flight at deletion", receipts)
	}
	if err := r.Get(context.Background(), ctrlclient.ObjectKeyFromObject(task), stored); err != nil {
		t.Fatal(err)
	}
	if stored.DeletionTimestamp.IsZero() || stored.Status.WebhookDelivery != nil {
		t.Fatalf("deleted Task: deletionTimestamp %v, terminal delivery %+v; want no terminal attempt", stored.DeletionTimestamp, stored.Status.WebhookDelivery)
	}
}
