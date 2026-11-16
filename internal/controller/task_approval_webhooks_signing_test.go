/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// requestedNotice returns the stored notice of approvalID's request.
func (h *approvalWebhookHarness) requestedNotice(approvalID string) corev1alpha1.TaskApprovalWebhookNotice {
	h.t.Helper()
	id := approvalWebhookDeliveryID(string(approvalWebhookTestUID), approvalID, corev1alpha1.TaskApprovalWebhookRequested)
	for _, notice := range h.task().Status.ApprovalWebhooks.Notices {
		if notice.DeliveryID == id {
			return notice
		}
	}
	h.t.Fatalf("no notice for %s", approvalID)
	return corev1alpha1.TaskApprovalWebhookNotice{}
}

func deadlineIs(deadline *metav1.Time, want time.Time) bool {
	return deadline != nil && deadline.Time.Equal(want)
}

func countPendingApprovalNotices(state *corev1alpha1.TaskApprovalWebhooks) int {
	pending := 0
	for _, notice := range state.Notices {
		if notice.State == corev1alpha1.TaskWebhookDeliveryPending {
			pending++
		}
	}
	return pending
}

// A notice that cannot be signed within its persisted window is retired
// unsent, without consuming an attempt, so the next notice is sent once
// signing returns. Every reconcile uses a fresh notifier, so the deadline
// comes from status alone, as after a controller restart.
func TestApprovalWebhooksSigningOutageRetiresStaleNoticeUnsent(t *testing.T) {
	h := newApprovalWebhookHarness(t, newApprovalWebhookTask(corev1alpha1.TaskPhaseRunning, corev1alpha1.TaskWebhookEventApprovalRequested))
	h.signingUnavailable = true
	queued := h.now
	h.request(approvalWebhookTestUID, "approval-1")
	if requeue := h.reconcile(); requeue <= 0 || requeue > webhookSigningRetryInterval {
		t.Fatalf("requeue = %v while signing is unavailable", requeue)
	}
	first := h.requestedNotice("approval-1")
	if first.State != corev1alpha1.TaskWebhookDeliveryPending || first.Attempts != 0 ||
		!deadlineIs(first.SigningDeadline, queued.Add(approvalWebhookSigningWindow)) ||
		first.LastError != "webhook signing key is unavailable" {
		t.Fatalf("first notice = %+v, want pending with a persisted signing deadline", first)
	}

	h.now = queued.Add(time.Minute)
	h.request(approvalWebhookTestUID, "approval-2")
	h.reconcile()
	if second := h.requestedNotice("approval-2"); second.State != corev1alpha1.TaskWebhookDeliveryPending ||
		!deadlineIs(second.SigningDeadline, h.now.Add(approvalWebhookSigningWindow)) {
		t.Fatalf("second notice = %+v", second)
	}

	// The requeue never sleeps past the head's deadline.
	h.now = queued.Add(approvalWebhookSigningWindow - 10*time.Second)
	if requeue := h.reconcile(); requeue != 10*time.Second {
		t.Fatalf("requeue = %v, want the time left before the signing deadline", requeue)
	}

	h.now = queued.Add(approvalWebhookSigningWindow)
	if requeue := h.reconcile(); requeue <= 0 {
		t.Fatalf("requeue = %v, want the next notice to be retried", requeue)
	}
	first = h.requestedNotice("approval-1")
	if first.State != corev1alpha1.TaskWebhookDeliveryFailed || first.Attempts != 0 || first.LastAttemptTime != nil ||
		first.NextAttemptTime != nil || first.LastError != approvalWebhookSigningExpired {
		t.Fatalf("stale notice = %+v, want retired unsent", first)
	}
	if second := h.requestedNotice("approval-2"); second.State != corev1alpha1.TaskWebhookDeliveryPending || second.Attempts != 0 {
		t.Fatalf("second notice = %+v, want still pending inside its window", second)
	}
	if got := len(h.receiver.all()); got != 0 {
		t.Fatalf("receipts = %d while signing is unavailable", got)
	}

	// Signing returns: the next notice proceeds, and the retired one is never
	// regenerated or sent.
	h.signingUnavailable = false
	for range 3 {
		h.reconcile()
	}
	receipts := h.receipts()
	if len(receipts) != 1 || receipts[0].payload.ApprovalID != "approval-2" {
		t.Fatalf("receipts = %v, want only the second notice", eventTypes(receipts))
	}
	state := h.task().Status.ApprovalWebhooks
	if len(state.Notices) != 2 || state.Overflowed || h.requestedNotice("approval-1").State != corev1alpha1.TaskWebhookDeliveryFailed ||
		h.requestedNotice("approval-2").State != corev1alpha1.TaskWebhookDeliveryDelivered {
		t.Fatalf("state = %+v", state)
	}
}

// Every pending notice waits on the same keys, so a pass retires all that
// are past their deadline at once, including after a controller restart that
// finds many of them stale.
func TestApprovalWebhooksSigningOutageRetiresEveryStaleNoticeInOnePass(t *testing.T) {
	h := newApprovalWebhookHarness(t, newApprovalWebhookTask(corev1alpha1.TaskPhaseRunning, bothApprovalEvents...))
	h.signingUnavailable = true
	for i := range 4 {
		h.request(approvalWebhookTestUID, fmt.Sprintf("approval-%d", i))
	}
	h.reconcile()
	h.now = h.now.Add(time.Hour)
	h.reconcile()
	state := h.task().Status.ApprovalWebhooks
	if len(state.Notices) != 4 || countPendingApprovalNotices(state) != 0 {
		t.Fatalf("state = %+v, want every stale notice retired", state)
	}
	for _, notice := range state.Notices {
		if notice.LastError != approvalWebhookSigningExpired || notice.Attempts != 0 {
			t.Fatalf("notice = %+v", notice)
		}
	}
	if requeue := h.reconcile(); requeue != 0 || len(h.receiver.all()) != 0 {
		t.Fatalf("requeue = %v with %d receipts after retirement", requeue, len(h.receiver.all()))
	}
}

// A sustained signing outage with steady approvals never fills the queue:
// retired notices free their slots. The cutoff stays reachable only by the
// documented rule, 15 notices pending inside one window.
func TestApprovalWebhooksSigningOutageDoesNotLatchOverflow(t *testing.T) {
	h := newApprovalWebhookHarness(t, newApprovalWebhookTask(corev1alpha1.TaskPhaseRunning, corev1alpha1.TaskWebhookEventApprovalRequested))
	h.signingUnavailable = true
	limit := corev1alpha1.MaxTaskApprovalWebhookNotices - 1
	spacing := 30 * time.Second
	perWindow := int(approvalWebhookSigningWindow / spacing)
	if perWindow >= limit {
		t.Fatalf("test spacing admits %d notices per window, want fewer than %d", perWindow, limit)
	}
	total := 4 * limit
	for i := range total {
		h.request(approvalWebhookTestUID, fmt.Sprintf("approval-%02d", i))
		h.reconcile()
		state := h.task().Status.ApprovalWebhooks
		if state.Overflowed || countPendingApprovalNotices(state) > perWindow+1 {
			t.Fatalf("after %d approvals: %d pending, overflowed=%v", i+1, countPendingApprovalNotices(state), state.Overflowed)
		}
		h.now = h.now.Add(spacing)
	}
	if got := len(h.receiver.all()); got != 0 {
		t.Fatalf("receipts = %d while signing is unavailable", got)
	}

	// Signing returns: the notices still inside their window are sent in
	// stream order, and nothing retired is sent.
	h.signingUnavailable = false
	for range 2 * corev1alpha1.MaxTaskApprovalWebhookNotices {
		h.reconcile()
	}
	state := h.task().Status.ApprovalWebhooks
	receipts := h.receipts()
	if state.Overflowed || countPendingApprovalNotices(state) != 0 || len(receipts) == 0 || len(receipts) > perWindow+1 {
		t.Fatalf("after recovery: %d receipts, %d pending, overflowed=%v", len(receipts), countPendingApprovalNotices(state), state.Overflowed)
	}
	for i := 1; i < len(receipts); i++ {
		if receipts[i].payload.StreamSeq <= receipts[i-1].payload.StreamSeq {
			t.Fatalf("receipts out of stream order: %d then %d", receipts[i-1].payload.StreamSeq, receipts[i].payload.StreamSeq)
		}
	}
	if last := receipts[len(receipts)-1].payload.ApprovalID; last != fmt.Sprintf("approval-%02d", total-1) {
		t.Fatalf("last receipt = %s, want the newest approval", last)
	}

	// The documented rule still holds: a burst that leaves 15 notices pending
	// inside one window overflows, whatever keeps them pending.
	h.signingUnavailable = true
	for i := range limit + 1 {
		h.request(approvalWebhookTestUID, fmt.Sprintf("burst-%02d", i))
	}
	h.reconcile()
	if state := h.task().Status.ApprovalWebhooks; !state.Overflowed || state.Omitted != 1 {
		t.Fatalf("burst state: overflowed=%v omitted=%d", state.Overflowed, state.Omitted)
	}
}

// A notice recorded without a signing deadline, by a controller from before
// the field existed, gets a persisted one starting from the first pass that
// finds signing unavailable, rather than being retired at once or waiting
// forever.
func TestApprovalWebhooksSigningDeadlineIsBackfilledOnce(t *testing.T) {
	h := newApprovalWebhookHarness(t, newApprovalWebhookTask(corev1alpha1.TaskPhaseRunning, corev1alpha1.TaskWebhookEventApprovalRequested))
	h.request(approvalWebhookTestUID, "approval-1")
	h.signingUnavailable = true
	h.reconcile()
	task := h.task()
	task.Status.ApprovalWebhooks.Notices[0].SigningDeadline = nil
	if err := h.client.Status().Update(context.Background(), task); err != nil {
		t.Fatal(err)
	}

	h.now = h.now.Add(time.Hour)
	backfilledAt := h.now
	h.reconcile()
	notice := h.requestedNotice("approval-1")
	if notice.State != corev1alpha1.TaskWebhookDeliveryPending || !deadlineIs(notice.SigningDeadline, backfilledAt.Add(approvalWebhookSigningWindow)) {
		t.Fatalf("notice = %+v, want a backfilled deadline from now", notice)
	}
	h.now = backfilledAt.Add(approvalWebhookSigningWindow - time.Second)
	h.reconcile()
	if notice := h.requestedNotice("approval-1"); notice.State != corev1alpha1.TaskWebhookDeliveryPending ||
		!deadlineIs(notice.SigningDeadline, backfilledAt.Add(approvalWebhookSigningWindow)) {
		t.Fatalf("notice = %+v, want the backfilled deadline kept", notice)
	}
	h.now = backfilledAt.Add(approvalWebhookSigningWindow)
	h.reconcile()
	if notice := h.requestedNotice("approval-1"); notice.State != corev1alpha1.TaskWebhookDeliveryFailed || notice.LastError != approvalWebhookSigningExpired {
		t.Fatalf("notice = %+v, want retired at the backfilled deadline", notice)
	}
}

// Terminal notices keep waiting for signing without consuming attempts,
// however long the outage lasts, while the Task's approval notices are
// retired around them.
func TestApprovalWebhooksSigningOutageLeavesTerminalNoticePending(t *testing.T) {
	h := newApprovalWebhookHarness(t, newApprovalWebhookTask(corev1alpha1.TaskPhaseSucceeded, bothApprovalEvents...))
	h.signingUnavailable = true
	h.request(approvalWebhookTestUID, "approval-1")
	for range 6 {
		h.reconcile()
		requeue, err := h.notifier().reconcileDelivery(context.Background(), h.client.Status(), h.task())
		if err != nil || requeue != webhookSigningRetryInterval {
			t.Fatalf("reconcileDelivery() = %v, %v", requeue, err)
		}
		h.now = h.now.Add(time.Hour)
	}
	stored := h.task()
	terminal := stored.Status.WebhookDelivery
	if stored.Status.WebhookDelivered || terminal == nil || terminal.State != corev1alpha1.TaskWebhookDeliveryPending ||
		terminal.Attempts != 0 || terminal.LastError != "webhook signing key is unavailable" {
		t.Fatalf("terminal delivery = %+v, want pending with no attempt consumed", terminal)
	}
	if notice := stored.Status.ApprovalWebhooks.Notices[0]; notice.State != corev1alpha1.TaskWebhookDeliveryFailed {
		t.Fatalf("approval notice = %+v, want retired", notice)
	}

	h.signingUnavailable = false
	if _, err := h.notifier().reconcileDelivery(context.Background(), h.client.Status(), h.task()); err != nil {
		t.Fatal(err)
	}
	if stored := h.task(); !stored.Status.WebhookDelivered || stored.Status.WebhookDelivery.Attempts != 1 || len(h.receiver.all()) != 1 {
		t.Fatalf("terminal delivery = %+v with %d receipts after signing returned", stored.Status.WebhookDelivery, len(h.receiver.all()))
	}
}
