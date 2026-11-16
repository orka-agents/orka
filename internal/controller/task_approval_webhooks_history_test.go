/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/labels"
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
