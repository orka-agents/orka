/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/taskwait"
	"github.com/orka-agents/orka/internal/taskwebhook"
)

// parkedWebhookHarness drives a parked Task's wait with the real wait
// reconciler and its webhook with the real notifier, over one fake API.
type parkedWebhookHarness struct {
	*taskWaitHarness
	receiver *webhookReceiver
	keys     taskwebhook.KeySet
}

func newParkedWebhookHarness(t *testing.T, task *corev1alpha1.Task) *parkedWebhookHarness {
	t.Helper()
	receiver := newWebhookReceiver(t, http.StatusOK)
	task.Spec.WebhookURL = receiver.server.URL + "/hook"
	return &parkedWebhookHarness{taskWaitHarness: newTaskWaitHarness(t, task), receiver: receiver, keys: newWebhookTestKeys(t)}
}

// deliver runs one webhook reconcile for name with a fresh notifier, as a
// restarted controller would: all delivery state comes from Task status.
func (h *parkedWebhookHarness) deliver(name string) {
	h.t.Helper()
	notifier := newTestWebhookNotifier(h.t, h.keys, nil)
	requeue, err := notifier.reconcileDelivery(context.Background(), h.client.Status(), h.task(name))
	if err != nil || requeue != 0 {
		h.t.Fatalf("reconcileDelivery(%s) = %v, %v; want 0, nil", name, requeue, err)
	}
}

// assertOneNotice checks that exactly one signed notice was received for
// task, with the expected event, and returns its payload.
func (h *parkedWebhookHarness) assertOneNotice(task *corev1alpha1.Task, eventType string) WebhookPayload {
	h.t.Helper()
	receipts := h.receiver.all()
	if len(receipts) != 1 {
		h.t.Fatalf("receipts = %d, want exactly 1", len(receipts))
	}
	claims := verifyReceipt(h.t, h.keys, task.Spec.WebhookURL, receipts[0], time.Now())
	var payload WebhookPayload
	if err := json.Unmarshal(receipts[0].body, &payload); err != nil {
		h.t.Fatal(err)
	}
	if claims.EventType != eventType || payload.EventType != eventType || receipts[0].header.Get(taskwebhook.HeaderEvent) != eventType {
		h.t.Fatalf("event = claim %q, body %q, header %q; want %q",
			claims.EventType, payload.EventType, receipts[0].header.Get(taskwebhook.HeaderEvent), eventType)
	}
	if claims.Task.UID != string(task.UID) || payload.TaskName != task.Name {
		h.t.Fatalf("notice names task %s (%s), want %s (%s)", payload.TaskName, claims.Task.UID, task.Name, task.UID)
	}
	stored := h.task(task.Name)
	if !stored.Status.WebhookDelivered || stored.Status.WebhookDelivery == nil ||
		stored.Status.WebhookDelivery.EventType != eventType || stored.Status.WebhookDelivery.DeliveryID != claims.DeliveryID {
		h.t.Fatalf("stored delivery = %#v", stored.Status.WebhookDelivery)
	}
	return payload
}

// assertParkedNothingSent checks that the parked Task neither sent nor
// recorded a notice.
func (h *parkedWebhookHarness) assertParkedNothingSent() {
	h.t.Helper()
	if got := len(h.receiver.all()); got != 0 {
		h.t.Fatalf("receipts = %d, want none while the parked work continues", got)
	}
	if stored := h.task("parked"); stored.Status.WebhookDelivered || stored.Status.WebhookDelivery != nil {
		h.t.Fatalf("parked recorded a notice: delivered=%v %#v", stored.Status.WebhookDelivered, stored.Status.WebhookDelivery)
	}
}

func TestParkedTurnSendsNoSucceededNoticeWhileItsWaitIsArmed(t *testing.T) {
	h := newParkedWebhookHarness(t, parkedTestTask(corev1alpha1.TaskWaitKindApproval, corev1alpha1.TaskPhaseSucceeded))

	// The turn settled Succeeded while its wait is still Requested.
	h.deliver("parked")
	h.assertParkedNothingSent()

	h.settle("parked")
	if got := h.task("parked").Status.Wait.State; got != corev1alpha1.TaskWaitStateWaiting {
		t.Fatalf("wait state = %s, want Waiting", got)
	}
	h.deliver("parked")
	h.assertParkedNothingSent()
}

func TestParkedTurnContinuationReportsTheChainOutcome(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	resumeAt := metav1.NewTime(taskWaitTestNow.Add(10 * time.Minute))
	task.Status.Wait.ResumeAt = &resumeAt
	h := newParkedWebhookHarness(t, task)

	h.settle("parked")
	h.deliver("parked")
	h.r.Now = func() time.Time { return taskWaitTestNow.Add(11 * time.Minute) }
	h.settle("parked")
	parked := h.task("parked")
	if parked.Status.Wait.State != corev1alpha1.TaskWaitStateResumed {
		t.Fatalf("wait state = %s, want Resumed", parked.Status.Wait.State)
	}
	// The resumed parked Task owes no notice of its own, across restarts.
	h.deliver("parked")
	h.deliver("parked")
	h.assertParkedNothingSent()

	continuation := h.task(parked.Status.Wait.ContinuationTaskName)
	if continuation.Spec.WebhookURL != parked.Spec.WebhookURL {
		t.Fatalf("continuation webhookURL = %q, want the parked Task's %q", continuation.Spec.WebhookURL, parked.Spec.WebhookURL)
	}
	completion := metav1.NewTime(taskWaitTestNow.Add(20 * time.Minute))
	continuation.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	continuation.Status.Message = "released v2.4.0"
	continuation.Status.CompletionTime = &completion
	if err := h.client.Status().Update(context.Background(), continuation); err != nil {
		t.Fatal(err)
	}
	h.deliver(continuation.Name)
	h.deliver(continuation.Name)
	h.deliver("parked")
	payload := h.assertOneNotice(continuation, "task.succeeded")
	if payload.Phase != corev1alpha1.TaskPhaseSucceeded || payload.Message != "released v2.4.0" {
		t.Fatalf("tail payload = %#v", payload)
	}
}

func TestParkedTurnWhoseWaitExpiresDeliversOneFailedNoticeAcrossRestarts(t *testing.T) {
	task := parkedTestTask(corev1alpha1.TaskWaitKindApproval, corev1alpha1.TaskPhaseSucceeded)
	task.Spec.Timeout = &metav1.Duration{Duration: 10*time.Minute + 30*time.Second}
	h := newParkedWebhookHarness(t, task)

	h.settle("parked")
	h.deliver("parked")
	h.assertParkedNothingSent()

	// A restarted wait reconciler expires the wait; a restarted notifier sends.
	h.r = &TaskWaitReconciler{
		Client: h.client, APIReader: h.client, ExecutionEvents: h.events, TaskProvenanceProtected: true,
		Now: func() time.Time { return taskWaitTestNow.Add(time.Minute) },
	}
	h.settle("parked")
	parked := h.task("parked")
	if parked.Status.Wait.State != corev1alpha1.TaskWaitStateExpired {
		t.Fatalf("wait state = %s, want Expired", parked.Status.Wait.State)
	}
	h.deliver("parked")
	h.deliver("parked")
	h.settle("parked")
	h.deliver("parked")
	payload := h.assertOneNotice(parked, "task.failed")
	if payload.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Fatalf("payload phase = %s, want the turn's own Succeeded phase", payload.Phase)
	}
}

func TestParkedTurnWhoseWaitIsCancelledDeliversOneCancelledNotice(t *testing.T) {
	h := newParkedWebhookHarness(t, parkedTestTask(corev1alpha1.TaskWaitKindApproval, corev1alpha1.TaskPhaseSucceeded))
	h.settle("parked")
	h.deliver("parked")
	h.assertParkedNothingSent()

	current := h.task("parked")
	current.Annotations[labels.AnnotationCancelWait] = "no longer needed"
	if err := h.client.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	h.settle("parked")
	if got := h.task("parked").Status.Wait.State; got != corev1alpha1.TaskWaitStateCancelled {
		t.Fatalf("wait state = %s, want Cancelled", got)
	}
	h.deliver("parked")
	h.deliver("parked")
	h.assertOneNotice(h.task("parked"), "task.cancelled")
}

func TestParkedTurnThatFailsDeliversItsOwnPhase(t *testing.T) {
	h := newParkedWebhookHarness(t, parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseFailed))
	h.deliver("parked")
	h.assertParkedNothingSent()
	h.settle("parked")
	if got := h.task("parked").Status.Wait.State; got != corev1alpha1.TaskWaitStateAbandoned {
		t.Fatalf("wait state = %s, want Abandoned", got)
	}
	h.deliver("parked")
	h.deliver("parked")
	h.assertOneNotice(h.task("parked"), "task.failed")
}

func TestTaskWebhookEventFollowsTheParkedWait(t *testing.T) {
	t.Parallel()
	succeeded, failed, cancelled := corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled
	cases := []struct {
		name        string
		phase       corev1alpha1.TaskPhase
		state       corev1alpha1.TaskWaitState
		noWait      bool
		superseded  bool
		want        string
		disposition webhookNoticeDisposition
	}{
		{name: "no wait", phase: succeeded, noWait: true, want: "task.succeeded"},
		{name: "no wait failed", phase: failed, noWait: true, want: "task.failed"},
		{name: "requested", phase: succeeded, state: corev1alpha1.TaskWaitStateRequested, disposition: webhookNoticeDeferred},
		{name: "waiting", phase: succeeded, state: corev1alpha1.TaskWaitStateWaiting, disposition: webhookNoticeDeferred},
		{name: "resuming", phase: succeeded, state: corev1alpha1.TaskWaitStateResuming, disposition: webhookNoticeDeferred},
		{name: "requested by a failed turn", phase: failed, state: corev1alpha1.TaskWaitStateRequested, disposition: webhookNoticeDeferred},
		{name: "resumed", phase: succeeded, state: corev1alpha1.TaskWaitStateResumed, disposition: webhookNoticeContinued},
		{name: "expired", phase: succeeded, state: corev1alpha1.TaskWaitStateExpired, want: "task.failed"},
		{name: "abandoned", phase: succeeded, state: corev1alpha1.TaskWaitStateAbandoned, want: "task.failed"},
		{name: "cancelled", phase: succeeded, state: corev1alpha1.TaskWaitStateCancelled, want: "task.cancelled"},
		{name: "abandoned by a failed turn", phase: failed, state: corev1alpha1.TaskWaitStateAbandoned, want: "task.failed"},
		{name: "abandoned by a cancelled turn", phase: cancelled, state: corev1alpha1.TaskWaitStateAbandoned, want: "task.cancelled"},
		{name: "cancelled while a failed turn ran", phase: failed, state: corev1alpha1.TaskWaitStateCancelled, want: "task.failed"},
		{name: "superseded by an unparked attempt", phase: succeeded, state: corev1alpha1.TaskWaitStateAbandoned, superseded: true, want: "task.succeeded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			task := parkedTestTask(corev1alpha1.TaskWaitKindTimer, tc.phase)
			task.Status.Wait.State = tc.state
			if tc.noWait {
				task.Status.Wait = nil
			}
			if tc.superseded {
				task.Status.Execution.Attempt, task.Status.Execution.PromptID = 2, "prompt-uid-parked-2"
			}
			event, disposition := taskWebhookEvent(task)
			if event != tc.want || disposition != tc.disposition {
				t.Fatalf("taskWebhookEvent() = %q, %d; want %q, %d", event, disposition, tc.want, tc.disposition)
			}
		})
	}
}

func TestParkedTurnPendingNoticeFromAnEarlierControllerIsNeverSentAsSucceeded(t *testing.T) {
	t.Parallel()
	for _, final := range []corev1alpha1.TaskWaitState{corev1alpha1.TaskWaitStateExpired, corev1alpha1.TaskWaitStateResumed} {
		t.Run(string(final), func(t *testing.T) {
			t.Parallel()
			receiver := newWebhookReceiver(t, http.StatusOK)
			task := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
			task.Spec.WebhookURL = receiver.server.URL
			task.Status.Wait.State = corev1alpha1.TaskWaitStateWaiting
			legacy, err := buildWebhookNotice(task, "legacy-delivery", "task.succeeded", taskWaitTestNow)
			if err != nil {
				t.Fatal(err)
			}
			task.Status.WebhookDelivery = &corev1alpha1.TaskWebhookDelivery{
				DeliveryID: legacy.DeliveryID, EventType: legacy.EventType, OccurredAt: metav1.NewTime(taskWaitTestNow),
				PayloadSHA256: legacy.Digest, State: corev1alpha1.TaskWebhookDeliveryPending, Attempts: 1,
				LastError: "webhook returned non-2xx status: 503",
			}
			client := newWebhookTestClient(t, task)
			keys := newWebhookTestKeys(t)
			notifier := newTestWebhookNotifier(t, keys, nil)

			if _, err := notifier.reconcileDelivery(context.Background(), client.Status(), reloadWebhookTask(t, client, task)); err != nil {
				t.Fatal(err)
			}
			if got := len(receiver.all()); got != 0 {
				t.Fatalf("receipts while waiting = %d, want 0", got)
			}

			current := reloadWebhookTask(t, client, task)
			current.Status.Wait.State = final
			if final == corev1alpha1.TaskWaitStateResumed {
				current.Status.Wait.ContinuationTaskName = taskwait.ContinuationTaskName(current)
				current.Status.Wait.ContinuationTaskUID = "uid-cont"
			}
			if err := client.Status().Update(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err := notifier.reconcileDelivery(context.Background(), client.Status(), reloadWebhookTask(t, client, task)); err != nil {
					t.Fatal(err)
				}
			}
			stored := reloadWebhookTask(t, client, task).Status.WebhookDelivery
			receipts := receiver.all()
			if final == corev1alpha1.TaskWaitStateResumed {
				if len(receipts) != 0 || stored.DeliveryID != "legacy-delivery" || stored.State != corev1alpha1.TaskWebhookDeliveryFailed || stored.NextAttemptTime != nil {
					t.Fatalf("resumed: receipts = %d, delivery = %#v; want the legacy notice retired unsent", len(receipts), stored)
				}
				return
			}
			if len(receipts) != 1 || stored.DeliveryID == "legacy-delivery" || stored.EventType != "task.failed" ||
				stored.Superseded != 1 || stored.State != corev1alpha1.TaskWebhookDeliveryDelivered {
				t.Fatalf("expired: receipts = %d, delivery = %#v; want one superseding task.failed notice", len(receipts), stored)
			}
			if claims := verifyReceipt(t, keys, task.Spec.WebhookURL, receipts[0], time.Now()); claims.EventType != "task.failed" {
				t.Fatalf("event claim = %q, want task.failed", claims.EventType)
			}
		})
	}
}

func TestHandleCompletedDefersAParkedTurnNoticeWithoutBlocking(t *testing.T) {
	receiver := newWebhookReceiver(t, http.StatusOK)
	task := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	task.Spec.WebhookURL = receiver.server.URL
	task.Status.Wait.State = corev1alpha1.TaskWaitStateWaiting
	r := newUnitReconciler(newTestScheme(), task)
	r.WebhookNotifier = newTestWebhookNotifier(t, newWebhookTestKeys(t), nil)

	current := &corev1alpha1.Task{}
	for range 3 {
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), current); err != nil {
			t.Fatal(err)
		}
		result, err := r.handleCompleted(context.Background(), current)
		if err != nil {
			t.Fatalf("handleCompleted() error = %v", err)
		}
		if result.RequeueAfter > time.Second {
			t.Fatalf("handleCompleted() requeue = %v; a deferred notice must not poll", result.RequeueAfter)
		}
	}
	if got := len(receiver.all()); got != 0 {
		t.Fatalf("receipts = %d, want none while the wait is armed", got)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	if current.Status.WebhookDelivered || current.Status.WebhookDelivery != nil {
		t.Fatalf("deferred notice recorded %#v", current.Status.WebhookDelivery)
	}
}
