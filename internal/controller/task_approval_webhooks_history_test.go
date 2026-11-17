/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/taskwebhook"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const approvalHistoryParent = "nightly"

// approvalHistoryParentTask is a scheduled Task that keeps no successful runs.
func approvalHistoryParentTask() *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: approvalHistoryParent, Namespace: defaultNS, UID: "uid-nightly"},
		Spec:       corev1alpha1.TaskSpec{Schedule: "@daily", SuccessfulRunsHistoryLimit: new(int32(0))},
	}
}

// approvalHistoryRun is a finished scheduled run of approvalHistoryParent.
func approvalHistoryRun(name string, uid types.UID, age time.Duration) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: defaultNS, UID: uid,
			CreationTimestamp: metav1.NewTime(approvalWebhookTestNow.Add(-age)),
			Labels:            map[string]string{labels.LabelParentTask: approvalHistoryParent, labels.LabelScheduledRun: scheduledRunLabelValue},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded, Attempts: 1},
	}
}

func withApprovalNotice(task *corev1alpha1.Task, state corev1alpha1.TaskWebhookDeliveryState) *corev1alpha1.Task {
	task.Status.ApprovalWebhooks = &corev1alpha1.TaskApprovalWebhooks{Tracking: true, Notices: []corev1alpha1.TaskApprovalWebhookNotice{{
		TaskWebhookDelivery: corev1alpha1.TaskWebhookDelivery{DeliveryID: "delivery-1", EventType: "task.approval_requested", State: state},
		Transition:          corev1alpha1.TaskApprovalWebhookRequested, StreamSeq: 1, ApprovalID: "approval-1",
	}}}
	return task
}

func historyRunPruned(t *testing.T, c ctrlclient.Client, name string) bool {
	t.Helper()
	task := &corev1alpha1.Task{}
	if err := c.Get(context.Background(), ctrlclient.ObjectKey{Namespace: defaultNS, Name: name}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return true
		}
		t.Fatal(err)
	}
	return !task.DeletionTimestamp.IsZero()
}

func setApprovalNoticeState(t *testing.T, c ctrlclient.Client, name string, state corev1alpha1.TaskWebhookDeliveryState) {
	t.Helper()
	task := &corev1alpha1.Task{}
	if err := c.Get(context.Background(), ctrlclient.ObjectKey{Namespace: defaultNS, Name: name}, task); err != nil {
		t.Fatal(err)
	}
	task.Status.ApprovalWebhooks.Notices[0].State = state
	if err := c.Status().Update(context.Background(), task); err != nil {
		t.Fatal(err)
	}
}

// A run that keeps no history must stay until its queued request and
// resolution notices are sent: deleting it after the request alone drops the
// resolution, which no later reconcile could recover.
func TestScheduledRunHistoryKeepsARunUntilItsApprovalNoticesSettle(t *testing.T) {
	run := approvalHistoryRun(approvalWebhookTestTask, approvalWebhookTestUID, time.Hour)
	run.Finalizers = []string{labels.TaskFinalizer}
	run.Spec.WebhookEvents = bothApprovalEvents
	run.Status.ApprovalWebhooks = &corev1alpha1.TaskApprovalWebhooks{Tracking: true}
	h := newApprovalWebhookHarness(t, nil)
	run.Spec.WebhookURL = h.receiver.server.URL + "/hook"
	h.client = newWebhookTestClient(t, approvalHistoryParentTask(), run,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: defaultNS, UID: "namespace-uid"}})
	h.request(approvalWebhookTestUID, "approval-1")
	h.decide(approvalWebhookTestUID, "approval-1", events.ExecutionEventTypeApprovalApproved)
	reconciler := &TaskReconciler{
		Client: h.client, Scheme: h.client.Scheme(), ExecutionEventStore: h.events,
		WebhookNotifier: newTestWebhookNotifier(t, h.keys, nil),
	}

	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: defaultNS, Name: approvalWebhookTestTask}}
	var sent []string
	for range 6 {
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		sent = sent[:0]
		for _, receipt := range h.receiver.all() {
			sent = append(sent, receipt.header.Get(taskwebhook.HeaderEvent))
		}
		if historyRunPruned(t, h.client, approvalWebhookTestTask) {
			break
		}
	}
	if !slices.Contains(sent, "task.approval_resolved") || !slices.Contains(sent, "task.approval_requested") {
		t.Fatalf("the run was pruned before its approval notices were sent: sent %v", sent)
	}
	if !historyRunPruned(t, h.client, approvalWebhookTestTask) {
		t.Fatalf("the run must be pruned once its notices settle: sent %v", sent)
	}
}

// Parent-triggered pruning respects the hold, and prunes the run on the next
// enforcement once its notice is delivered or retired.
func TestScheduledRunHistoryHoldsPendingApprovalNoticesForTheParent(t *testing.T) {
	for _, settled := range []corev1alpha1.TaskWebhookDeliveryState{corev1alpha1.TaskWebhookDeliveryDelivered, corev1alpha1.TaskWebhookDeliveryFailed} {
		t.Run(string(settled), func(t *testing.T) {
			parent := approvalHistoryParentTask()
			held := withApprovalNotice(approvalHistoryRun("held-run", "uid-held", 2*time.Hour), corev1alpha1.TaskWebhookDeliveryPending)
			plain := approvalHistoryRun("plain-run", "uid-plain", time.Hour)
			r := newUnitReconciler(newTestScheme(), parent, held, plain)
			r.WebhookNotifier = newTestWebhookNotifier(t, newWebhookTestKeys(t), nil)

			if err := r.enforceHistoryLimits(context.Background(), parent); err != nil {
				t.Fatal(err)
			}
			if historyRunPruned(t, r.Client, "held-run") {
				t.Fatal("a run with a pending approval notice must not be pruned")
			}
			if !historyRunPruned(t, r.Client, "plain-run") {
				t.Fatal("a run without pending notices beyond the limit is still pruned")
			}

			setApprovalNoticeState(t, r.Client, "held-run", settled)
			if err := r.enforceHistoryLimits(context.Background(), parent); err != nil {
				t.Fatal(err)
			}
			if !historyRunPruned(t, r.Client, "held-run") {
				t.Fatalf("a run whose notice is %s must be pruned", settled)
			}
		})
	}
}

// A continuation's pending notice holds its whole run, which is pruned
// through the head once the notice settles.
func TestScheduledRunHistoryHoldsAChainWhoseContinuationHasPendingNotices(t *testing.T) {
	parent := approvalHistoryParentTask()
	head := approvalHistoryRun("parked-run", "uid-parked-run", 3*time.Hour)
	head.Status.Wait = &corev1alpha1.TaskWaitStatus{
		State: corev1alpha1.TaskWaitStateResumed, ContinuationTaskName: "parked-run-next", ContinuationTaskUID: "uid-parked-run-next",
	}
	next := withApprovalNotice(approvalHistoryRun("parked-run-next", "uid-parked-run-next", time.Hour), corev1alpha1.TaskWebhookDeliveryPending)
	next.Annotations = map[string]string{
		labels.AnnotationContinuationOfName: "parked-run", labels.AnnotationContinuationOfUID: "uid-parked-run",
	}
	r := newUnitReconciler(newTestScheme(), parent, head, next)
	r.WebhookNotifier = newTestWebhookNotifier(t, newWebhookTestKeys(t), nil)

	if err := r.enforceHistoryLimits(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if historyRunPruned(t, r.Client, "parked-run") || historyRunPruned(t, r.Client, "parked-run-next") {
		t.Fatal("a chain whose continuation has a pending approval notice must not be pruned")
	}

	setApprovalNoticeState(t, r.Client, "parked-run-next", corev1alpha1.TaskWebhookDeliveryDelivered)
	if err := r.enforceHistoryLimits(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if !historyRunPruned(t, r.Client, "parked-run") {
		t.Fatal("the settled chain must be pruned through its head")
	}
}

// A failed read of the run or its chain keeps the run.
func TestApprovalNoticeHoldFailsSafeOnLookupErrors(t *testing.T) {
	head := approvalHistoryRun("parked-run", "uid-parked-run", 3*time.Hour)
	head.Status.Wait = &corev1alpha1.TaskWaitStatus{
		State: corev1alpha1.TaskWaitStateResumed, ContinuationTaskName: "parked-run-next", ContinuationTaskUID: "uid-parked-run-next",
	}
	next := approvalHistoryRun("parked-run-next", "uid-parked-run-next", time.Hour)
	next.Annotations = map[string]string{
		labels.AnnotationContinuationOfName: "parked-run", labels.AnnotationContinuationOfUID: "uid-parked-run",
	}
	for _, failing := range []string{"parked-run", "parked-run-next"} {
		t.Run(failing, func(t *testing.T) {
			r := newUnitReconciler(newTestScheme(), head.DeepCopy(), next.DeepCopy())
			r.WebhookNotifier = newTestWebhookNotifier(t, newWebhookTestKeys(t), nil)
			r.APIReader = interceptor.NewClient(r.Client.(ctrlclient.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c ctrlclient.WithWatch, key ctrlclient.ObjectKey, obj ctrlclient.Object, opts ...ctrlclient.GetOption) error {
					if key.Name == failing {
						return errors.New("apiserver unavailable")
					}
					return c.Get(ctx, key, obj, opts...)
				},
			})
			if !r.approvalNoticesHoldRun(context.Background(), head) {
				t.Fatal("a lookup failure must keep the run")
			}
		})
	}
	r := newUnitReconciler(newTestScheme(), head.DeepCopy(), next.DeepCopy())
	r.WebhookNotifier = newTestWebhookNotifier(t, newWebhookTestKeys(t), nil)
	if r.approvalNoticesHoldRun(context.Background(), head) {
		t.Fatal("a chain without pending approval notices is not held")
	}
}

// appendAfterProjectionStore appends once, right after the at-th approval
// event list returns, as a decision committing between a reconcile's
// projection and its history enforcement would.
type appendAfterProjectionStore struct {
	store.ExecutionEventStore
	at     int
	lists  int
	append func()
}

func (s *appendAfterProjectionStore) ListExecutionEvents(ctx context.Context, filter store.ExecutionEventFilter) ([]store.ExecutionEvent, error) {
	listed, err := s.ExecutionEventStore.ListExecutionEvents(ctx, filter)
	if err == nil && slices.Contains(filter.EventTypes, events.ExecutionEventTypeApprovalRequested) {
		if s.lists++; s.lists == s.at {
			s.append()
		}
	}
	return listed, err
}

// approvalHistoryHarness is a finished run of approvalHistoryParent that
// keeps no history and selects both approval events, served by a real event
// store and receiver.
func approvalHistoryHarness(t *testing.T, run *corev1alpha1.Task) (*approvalWebhookHarness, *TaskReconciler) {
	t.Helper()
	run.Finalizers = []string{labels.TaskFinalizer}
	run.Spec.WebhookEvents = bothApprovalEvents
	if run.Status.ApprovalWebhooks == nil {
		run.Status.ApprovalWebhooks = &corev1alpha1.TaskApprovalWebhooks{Tracking: true}
	}
	h := newApprovalWebhookHarness(t, nil)
	run.Spec.WebhookURL = h.receiver.server.URL + "/hook"
	h.client = newWebhookTestClient(t, approvalHistoryParentTask(), run,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: defaultNS, UID: "namespace-uid"}})
	reconciler := &TaskReconciler{
		Client: h.client, Scheme: h.client.Scheme(), ExecutionEventStore: h.events,
		WebhookNotifier: newTestWebhookNotifier(t, h.keys, nil),
	}
	return h, reconciler
}

// reconcileApprovalHistoryRun reconciles the run until it is pruned, at most
// rounds times, and returns the approval event types the receiver got.
func reconcileApprovalHistoryRun(t *testing.T, h *approvalWebhookHarness, reconciler *TaskReconciler, rounds int) []string {
	t.Helper()
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: defaultNS, Name: approvalWebhookTestTask}}
	for range rounds {
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		if historyRunPruned(t, h.client, approvalWebhookTestTask) {
			break
		}
	}
	var sent []string
	for _, receipt := range h.receiver.all() {
		if event := receipt.header.Get(taskwebhook.HeaderEvent); strings.HasPrefix(event, "task.approval_") {
			sent = append(sent, event)
		}
	}
	return sent
}

// A decision that commits after a run's projection read but before its
// history enforcement is not yet a notice. Pruning then would delete the
// stream and lose the resolution, so the run is held until a later
// reconcile notifies it, once, and is pruned after that. The decision lands
// after the first, then the second, approval read of the run's reconciles;
// the second lands before the enforcement of the reconcile that settles the
// request notice.
func TestScheduledRunHistoryHoldsADecisionAppendedAfterProjection(t *testing.T) {
	for at := 1; at <= 2; at++ {
		t.Run(strconv.Itoa(at), func(t *testing.T) {
			h, reconciler := approvalHistoryHarness(t, approvalHistoryRun(approvalWebhookTestTask, approvalWebhookTestUID, time.Hour))
			h.request(approvalWebhookTestUID, "approval-1")
			racing := &appendAfterProjectionStore{ExecutionEventStore: h.events, at: at, append: func() {
				h.decide(approvalWebhookTestUID, "approval-1", events.ExecutionEventTypeApprovalApproved)
			}}
			reconciler.ExecutionEventStore = racing

			sent := reconcileApprovalHistoryRun(t, h, reconciler, 10)
			if racing.lists < at {
				t.Fatalf("the run was pruned after %d approval reads, before the decision", racing.lists)
			}
			if !slices.Equal(sent, []string{"task.approval_requested", "task.approval_resolved"}) {
				t.Fatalf("approval notices = %v, want the request and its resolution once each", sent)
			}
			if !historyRunPruned(t, h.client, approvalWebhookTestTask) {
				t.Fatal("the run must be pruned once its resolution is notified")
			}
		})
	}
}

// Parent-triggered pruning holds a run whose stream has approval events its
// projection has not reached, and prunes it once the run's own reconcile
// notified them.
func TestScheduledRunHistoryParentHoldsUnprojectedApprovalEvents(t *testing.T) {
	run := approvalHistoryRun(approvalWebhookTestTask, approvalWebhookTestUID, time.Hour)
	h, reconciler := approvalHistoryHarness(t, run)
	h.request(approvalWebhookTestUID, "approval-1")
	if sent := reconcileApprovalHistoryRun(t, h, reconciler, 1); !slices.Equal(sent, []string{"task.approval_requested"}) {
		t.Fatalf("approval notices = %v, want the request", sent)
	}
	if historyRunPruned(t, h.client, approvalWebhookTestTask) {
		t.Fatal("the run was pruned before the decision")
	}
	h.decide(approvalWebhookTestUID, "approval-1", events.ExecutionEventTypeApprovalApproved)

	parent := &corev1alpha1.Task{}
	if err := h.client.Get(context.Background(), ctrlclient.ObjectKey{Namespace: defaultNS, Name: approvalHistoryParent}, parent); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.enforceHistoryLimits(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if historyRunPruned(t, h.client, approvalWebhookTestTask) {
		t.Fatal("the parent pruned a run whose decision was not yet notified")
	}

	sent := reconcileApprovalHistoryRun(t, h, reconciler, 6)
	if !slices.Equal(sent, []string{"task.approval_requested", "task.approval_resolved"}) {
		t.Fatalf("approval notices = %v, want the request and its resolution once each", sent)
	}
	if !historyRunPruned(t, h.client, approvalWebhookTestTask) {
		t.Fatal("the run must be pruned once its resolution is notified")
	}
}

// unprojectedApprovalRun is a finished run that tracks both approval events,
// with its cursor at seq.
func unprojectedApprovalRun(seq int64) *corev1alpha1.Task {
	run := approvalHistoryRun(approvalWebhookTestTask, approvalWebhookTestUID, time.Hour)
	run.Spec.WebhookURL = "https://receiver.example/hook"
	run.Spec.WebhookEvents = bothApprovalEvents
	completed := metav1.NewTime(approvalWebhookTestNow.Add(-time.Minute))
	run.Status.CompletionTime = &completed
	run.Status.ApprovalWebhooks = &corev1alpha1.TaskApprovalWebhooks{Tracking: true, ProjectedSeq: seq}
	return run
}

func approvalHoldReconciler(t *testing.T, run *corev1alpha1.Task, eventStore store.ExecutionEventStore, now time.Time) *TaskReconciler {
	t.Helper()
	r := newUnitReconciler(newTestScheme(), run)
	r.ExecutionEventStore = eventStore
	r.WebhookNotifier = newTestWebhookNotifier(t, newWebhookTestKeys(t), nil)
	r.WebhookNotifier.now = func() time.Time { return now }
	return r
}

// A failed event store read keeps a recently settled run, since it cannot
// tell whether a decision is still to be notified, but not past the approval
// notice window: a store that stays unavailable holds no run indefinitely.
func TestApprovalEventHoldFailsSafeOnStoreErrorsWithinTheWindow(t *testing.T) {
	failing := failingExecutionEventStore{err: errors.New("event store unavailable")}
	run := unprojectedApprovalRun(0)
	if r := approvalHoldReconciler(t, run.DeepCopy(), failing, approvalWebhookTestNow); !r.approvalNoticesHoldRun(context.Background(), run) {
		t.Fatal("an event store failure must keep a run that settled within the window")
	}
	past := run.Status.CompletionTime.Add(approvalWebhookSigningWindow)
	if r := approvalHoldReconciler(t, run.DeepCopy(), failing, past); r.approvalNoticesHoldRun(context.Background(), run) {
		t.Fatal("an event store failure must not keep a run past the window")
	}
	unselected := unprojectedApprovalRun(0)
	unselected.Spec.WebhookEvents = nil
	if r := approvalHoldReconciler(t, unselected.DeepCopy(), failing, approvalWebhookTestNow); r.approvalNoticesHoldRun(context.Background(), unselected) {
		t.Fatal("a run that selects no approval event never reads the store, so a failure cannot hold it")
	}
}

// Only a transition projection would still notify holds a run. Events that
// never become one never advance the cursor, so they must not hold it for
// good.
func TestApprovalEventHoldIgnoresEventsProjectionNeverNotifies(t *testing.T) {
	for _, tc := range []struct {
		name string
		// arrange appends events and returns the run, its cursor set.
		arrange func(h *approvalWebhookHarness) *corev1alpha1.Task
		held    bool
	}{
		{name: "unprojected decision", held: true, arrange: func(h *approvalWebhookHarness) *corev1alpha1.Task {
			request := h.request(approvalWebhookTestUID, "approval-1")
			h.decide(approvalWebhookTestUID, "approval-1", events.ExecutionEventTypeApprovalApproved)
			return unprojectedApprovalRun(request.Seq)
		}},
		{name: "projected", arrange: func(h *approvalWebhookHarness) *corev1alpha1.Task {
			h.request(approvalWebhookTestUID, "approval-1")
			decision := h.decide(approvalWebhookTestUID, "approval-1", events.ExecutionEventTypeApprovalApproved)
			return unprojectedApprovalRun(decision.Seq)
		}},
		{name: "execution update", arrange: func(h *approvalWebhookHarness) *corev1alpha1.Task {
			h.request(approvalWebhookTestUID, "approval-1")
			decision := h.decide(approvalWebhookTestUID, "approval-1", events.ExecutionEventTypeApprovalApproved)
			h.append(&store.ExecutionEvent{Type: events.ExecutionEventTypeApprovalExecutionUpdated, ToolCallID: "approval-1",
				Content: []byte(`{"approvalID":"approval-1","taskUID":"` + string(approvalWebhookTestUID) + `"}`)})
			return unprojectedApprovalRun(decision.Seq)
		}},
		{name: "duplicate request", arrange: func(h *approvalWebhookHarness) *corev1alpha1.Task {
			request := h.request(approvalWebhookTestUID, "approval-1")
			h.request(approvalWebhookTestUID, "approval-1")
			return unprojectedApprovalRun(request.Seq)
		}},
		{name: "another incarnation", arrange: func(h *approvalWebhookHarness) *corev1alpha1.Task {
			h.request("uid-earlier-incarnation", "approval-1")
			return unprojectedApprovalRun(0)
		}},
		{name: "unselected resolution", arrange: func(h *approvalWebhookHarness) *corev1alpha1.Task {
			request := h.request(approvalWebhookTestUID, "approval-1")
			h.decide(approvalWebhookTestUID, "approval-1", events.ExecutionEventTypeApprovalApproved)
			run := unprojectedApprovalRun(request.Seq)
			run.Spec.WebhookEvents = []corev1alpha1.TaskWebhookEvent{corev1alpha1.TaskWebhookEventApprovalRequested}
			return run
		}},
		{name: "untracked", arrange: func(h *approvalWebhookHarness) *corev1alpha1.Task {
			h.request(approvalWebhookTestUID, "approval-1")
			run := unprojectedApprovalRun(0)
			run.Status.ApprovalWebhooks.Tracking = false
			return run
		}},
		{name: "overflowed", arrange: func(h *approvalWebhookHarness) *corev1alpha1.Task {
			h.request(approvalWebhookTestUID, "approval-1")
			run := unprojectedApprovalRun(0)
			run.Status.ApprovalWebhooks.Overflowed = true
			return run
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newApprovalWebhookHarness(t, nil)
			run := tc.arrange(h)
			r := approvalHoldReconciler(t, run.DeepCopy(), h.events, approvalWebhookTestNow)
			if got := r.approvalNoticesHoldRun(context.Background(), run); got != tc.held {
				t.Fatalf("held = %v, want %v", got, tc.held)
			}
		})
	}
}
