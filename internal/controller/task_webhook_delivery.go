/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/taskwait"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// taskStatusWriter persists Task status with optimistic concurrency.
type taskStatusWriter interface {
	Update(ctx context.Context, obj ctrlclient.Object, opts ...ctrlclient.SubResourceUpdateOption) error
}

// reconcileDelivery advances the Task's durable webhook notice by at most one
// attempt and returns how long to wait before calling again (zero when the
// notice is settled).
//
// The notice lives in Task.status.webhookDelivery, so it survives controller
// restarts and leader changes. Every attempt is claimed by persisting the
// incremented attempt count and a lease (NextAttemptTime) before any request
// is sent; the status write carries the Task's resourceVersion, so of two
// controllers racing on the same Task only one claim succeeds. A controller
// that crashes mid-attempt loses that attempt, and the next one waits for the
// lease to expire. Receivers deduplicate on the stable delivery ID.
func (w *WebhookNotifier) reconcileDelivery(ctx context.Context, status taskStatusWriter, task *corev1alpha1.Task) (time.Duration, error) {
	log := logf.FromContext(ctx)
	now := w.now()
	outstanding, wait := terminalNoticeOutstanding(task, now)
	if !outstanding || wait > 0 {
		return wait, nil
	}
	delivery := task.Status.WebhookDelivery
	eventType, disposition := taskWebhookEvent(task)
	if disposition == webhookNoticeContinued {
		// The continuation inherited spec.webhookURL and reports the chain's
		// outcome. A notice recorded before this rule existed is retired
		// rather than left pending forever.
		delivery.State = corev1alpha1.TaskWebhookDeliveryFailed
		delivery.NextAttemptTime = nil
		delivery.LastError = boundedWebhookError(errors.New("the parked turn resumed as a continuation Task, which sends the notice for the chain"))
		task.Status.WebhookDelivery = delivery
		log.Info("retiring pending webhook notice of a resumed parked turn", "deliveryID", delivery.DeliveryID)
		return 0, status.Update(ctx, task)
	}

	notice, superseded, err := w.currentNotice(task, delivery, eventType, now)
	if err != nil {
		return 0, err
	}
	if superseded {
		previous := int32(0)
		if delivery != nil {
			previous = delivery.Superseded + 1
			log.Info("superseding pending webhook notice with newer Task state", "previousDeliveryID", delivery.DeliveryID, "deliveryID", notice.DeliveryID)
		}
		delivery = &corev1alpha1.TaskWebhookDelivery{
			DeliveryID:    notice.DeliveryID,
			EventType:     notice.EventType,
			OccurredAt:    metav1.NewTime(notice.OccurredAt),
			PayloadSHA256: notice.Digest,
			State:         corev1alpha1.TaskWebhookDeliveryPending,
			Superseded:    previous,
		}
	}

	task.Status.WebhookDelivery = delivery
	sent := notice
	return w.attemptNotice(ctx, status, task, terminalWebhookRecord, notice, superseded, w.terminalRetry(),
		func(current *corev1alpha1.Task) bool { return terminalNoticeStillCurrent(current, sent) },
		func(task *corev1alpha1.Task) {
			task.Status.WebhookDelivered = true
		})
}

// terminalNoticeStillCurrent reports whether task's current state still
// owes exactly notice: the same event, due now, with a byte-identical body.
// Otherwise the pending notice is superseded before its next attempt.
func terminalNoticeStillCurrent(task *corev1alpha1.Task, notice webhookNotice) bool {
	eventType, disposition := taskWebhookEvent(task)
	if disposition != webhookNoticeDue || eventType != notice.EventType {
		return false
	}
	rebuilt, err := buildWebhookNotice(task, notice.DeliveryID, notice.EventType, notice.OccurredAt)
	return err == nil && rebuilt.Digest == notice.Digest
}

// terminalNoticeOutstanding reports whether the Task's terminal notice still
// needs delivery work and, while the lease of an in-flight attempt or the
// backoff of a failed one runs, how long until it may be attempted again.
// reconcileDelivery and the Task reconciler share it, so the reconciler holds
// back the Task's completion work exactly while delivery has work left.
func terminalNoticeOutstanding(task *corev1alpha1.Task, now time.Time) (bool, time.Duration) {
	if task.Spec.WebhookURL == "" || task.Status.WebhookDelivered {
		return false, 0
	}
	delivery := task.Status.WebhookDelivery
	if delivery != nil && delivery.State != corev1alpha1.TaskWebhookDeliveryPending {
		return false, 0
	}
	// Honor the lease and the backoff before anything else, so newer Task
	// state never accelerates retries.
	if delivery != nil && delivery.NextAttemptTime != nil && now.Before(delivery.NextAttemptTime.Time) {
		return true, delivery.NextAttemptTime.Sub(now)
	}
	switch _, disposition := taskWebhookEvent(task); disposition {
	case webhookNoticeDeferred:
		// The turn parked and its wait can still resume the work. The wait's
		// own status transitions reconcile this Task again, so nothing is
		// written and nothing is requeued.
		return false, 0
	case webhookNoticeContinued:
		// Only a notice recorded before the continuation rule existed is
		// left to retire.
		return delivery != nil, 0
	}
	return true, 0
}

// webhookRecordRef returns the live delivery record of one notice in task's
// status, or nil when it is gone. Every status write replaces the decoded
// status, so the record is looked up again after each one.
type webhookRecordRef func(task *corev1alpha1.Task) *corev1alpha1.TaskWebhookDelivery

func terminalWebhookRecord(task *corev1alpha1.Task) *corev1alpha1.TaskWebhookDelivery {
	return task.Status.WebhookDelivery
}

// webhookRetryPolicy bounds the attempts of one notice.
type webhookRetryPolicy struct {
	maxAttempts int32
	maxBackoff  time.Duration
	// signingUnavailable, when set, runs on the Task while signing keys are
	// unavailable, before the notice is kept pending. It may retire notices
	// whose wait for keys has ended, including this one, and reports whether
	// it changed status and how long until the next such retirement (zero
	// for none). Without it, as for terminal notices, a notice waits until
	// signing is fixed.
	signingUnavailable func(ctx context.Context, task *corev1alpha1.Task, now time.Time) (bool, time.Duration)
}

func (w *WebhookNotifier) terminalRetry() webhookRetryPolicy {
	return webhookRetryPolicy{maxAttempts: w.maxAttempts, maxBackoff: w.maxBackoff}
}

// attemptNotice advances one pending notice, already placed in task status,
// by at most one attempt. It applies the destination policy, the attempt
// bound, and the signing requirement, claims the attempt durably under a
// lease, sends the signed notice, and records the result. fresh means the
// record was just created and holds no attempt yet. stillCurrent reports
// whether a re-read Task still renders exactly notice; a result is recorded
// on a re-read Task only when it does and still names the same webhook URL.
// delivered runs on the Task before the success is persisted.
//
//nolint:gocyclo // Every refusal before the send stays inline so none can be skipped.
func (w *WebhookNotifier) attemptNotice(
	ctx context.Context,
	status taskStatusWriter,
	task *corev1alpha1.Task,
	record webhookRecordRef,
	notice webhookNotice,
	fresh bool,
	retry webhookRetryPolicy,
	stillCurrent func(*corev1alpha1.Task) bool,
	delivered func(*corev1alpha1.Task),
) (time.Duration, error) {
	log := logf.FromContext(ctx)
	delivery := record(task)
	if delivery == nil {
		return 0, errors.New("webhook notice record is missing from Task status")
	}
	if _, _, policyErr := validateWebhookURLPolicy(task.Spec.WebhookURL, task.Namespace, w.policy); policyErr != nil {
		delivery.State = corev1alpha1.TaskWebhookDeliveryFailed
		delivery.NextAttemptTime = nil
		delivery.LastError = boundedWebhookError(policyErr)
		log.Info("webhook destination rejected by policy", "deliveryID", delivery.DeliveryID, "reason", delivery.LastError)
		return 0, status.Update(ctx, task)
	}

	// An attempt whose result was never recorded (a restart or a failed status
	// write) still counts. Never claim past the bound.
	if !fresh && delivery.Attempts >= retry.maxAttempts {
		delivery.State = corev1alpha1.TaskWebhookDeliveryFailed
		delivery.NextAttemptTime = nil
		if delivery.LastError == "" {
			delivery.LastError = "webhook delivery attempt limit reached"
		}
		log.Info("webhook delivery exhausted its attempts", "deliveryID", delivery.DeliveryID, "attempts", delivery.Attempts)
		return 0, status.Update(ctx, task)
	}

	if w.keys == nil {
		// Signing is disabled (no signing Secret is configured). Nothing is
		// ever sent unsigned, so the notice fails terminally instead of
		// requeueing a finished Task forever.
		delivery.State = corev1alpha1.TaskWebhookDeliveryFailed
		delivery.NextAttemptTime = nil
		delivery.LastError = boundedWebhookError(errors.New("webhook signing is disabled"))
		log.Info("webhook delivery skipped: signing is disabled", "deliveryID", delivery.DeliveryID)
		return 0, status.Update(ctx, task)
	}
	keys, err := w.keys.SigningKeys(ctx)
	if err != nil {
		return w.recordSigningUnavailable(ctx, status, task, delivery, fresh, retry, err)
	}

	// Claim the attempt durably before sending. Loading keys can block on
	// Secret reads, so the lease starts from a fresh clock reading. The claim
	// write and the send run under a timer started no later than that
	// reading, so this replica cannot begin a POST after its persisted lease
	// lets another replica claim the next attempt.
	leaseCtx, cancelLease := context.WithTimeout(ctx, w.attemptLease)
	defer cancelLease()
	now := w.now()
	attempt := delivery.Attempts + 1
	claimedAt := metav1.NewTime(now)
	// Status times are serialized at whole seconds. Round the persisted lease
	// up so it never ends before leaseCtx: truncation would let another
	// replica claim the next attempt while this one can still send.
	lease := metav1.NewTime(ceilToSecond(now.Add(w.attemptLease)))
	delivery.Attempts = attempt
	delivery.LastAttemptTime = &claimedAt
	delivery.NextAttemptTime = &lease
	if err := status.Update(leaseCtx, task); err != nil {
		return 0, err
	}

	result := w.deliver(leaseCtx, task, notice, attempt, keys.Current)

	requeue := time.Duration(0)
	var nextAttempt *metav1.Time
	if result.Err != nil && attempt < retry.maxAttempts {
		requeue = w.backoffUpTo(attempt, retry.maxBackoff)
		next := metav1.NewTime(w.now().Add(requeue))
		nextAttempt = &next
	}
	// recordResult applies the result to a record that still holds this
	// attempt's claim. It reports false when the claim is gone.
	recordResult := func(task *corev1alpha1.Task) bool {
		delivery := record(task)
		if !holdsWebhookAttemptClaim(delivery, attempt, &lease) {
			return false
		}
		delivery.LastStatusCode = result.StatusCode
		switch {
		case result.Err == nil:
			delivery.State = corev1alpha1.TaskWebhookDeliveryDelivered
			delivery.NextAttemptTime = nil
			delivery.LastError = ""
			if delivered != nil {
				delivered(task)
			}
		case nextAttempt == nil:
			delivery.State = corev1alpha1.TaskWebhookDeliveryFailed
			delivery.NextAttemptTime = nil
			delivery.LastError = boundedWebhookError(result.Err)
		default:
			delivery.NextAttemptTime = nextAttempt.DeepCopy()
			delivery.LastError = boundedWebhookError(result.Err)
		}
		return true
	}
	if !recordResult(task) {
		// The claim persisted, so its lease still bounds the next attempt.
		return 0, errors.New("webhook notice record is missing from Task status after its claim")
	}
	switch {
	case result.Err == nil:
	case nextAttempt == nil:
		log.Info("webhook delivery exhausted its attempts", "deliveryID", notice.DeliveryID, "attempts", attempt, "reason", boundedWebhookError(result.Err))
	default:
		log.Info("webhook delivery attempt failed", "deliveryID", notice.DeliveryID, "attempt", attempt, "retryIn", requeue, "reason", boundedWebhookError(result.Err))
	}
	// Neither body covers the destination, so a re-read Task must also still
	// name the receiver this attempt was sent to.
	sentTo := task.Spec.WebhookURL
	if err := recordWebhookAttemptResult(ctx, status, task, func(current *corev1alpha1.Task) bool {
		return current.Spec.WebhookURL == sentTo && stillCurrent(current) && recordResult(current)
	}); err != nil {
		// The claimed lease still bounds the next attempt.
		return 0, err
	}
	return requeue, nil
}

// holdsWebhookAttemptClaim reports whether delivery still records the claim
// of attempt under lease: no result was recorded for it and no later claim
// replaced it.
func holdsWebhookAttemptClaim(delivery *corev1alpha1.TaskWebhookDelivery, attempt int32, lease *metav1.Time) bool {
	return delivery != nil &&
		delivery.State == corev1alpha1.TaskWebhookDeliveryPending &&
		delivery.Attempts == attempt &&
		delivery.NextAttemptTime != nil && delivery.NextAttemptTime.Equal(lease)
}

// recordWebhookAttemptResult persists an attempt's result, already applied
// to task. Delivery runs beside the Task reconciler and other controllers
// that write Task status, so a conflicting write is expected. When the
// writer can re-read the Task, rerecord applies the result again to the
// current Task, and reports false, recording nothing, unless that Task still
// names the same receiver, renders the sent notice, and its record still
// holds this attempt's claim. A
// claim that was replaced or settled is not this attempt's to record, and a
// notice whose Task state changed must be superseded, as ADR 0042 requires,
// rather than settled by a send of the older body. A result that is not
// recorded leaves the claim's lease to bound the next attempt, as a crash
// does.
func recordWebhookAttemptResult(
	ctx context.Context,
	status taskStatusWriter,
	task *corev1alpha1.Task,
	rerecord func(*corev1alpha1.Task) bool,
) error {
	err := status.Update(ctx, task)
	rereader, ok := status.(taskStatusRereader)
	for retries := 0; ok && apierrors.IsConflict(err) && retries < webhookResultRecordRetries; retries++ {
		if err := rereader.reread(ctx, task); err != nil {
			return err
		}
		if !rerecord(task) {
			logf.FromContext(ctx).Info("webhook attempt result not recorded: its claim or its notice is no longer current")
			return nil
		}
		err = status.Update(ctx, task)
	}
	return err
}

// currentNotice rebuilds the pending notice for the Task's current state. When
// that state no longer matches the recorded digest, or the event changed, the
// pending notice is superseded by a fresh one with a new delivery ID.
func (w *WebhookNotifier) currentNotice(
	task *corev1alpha1.Task,
	delivery *corev1alpha1.TaskWebhookDelivery,
	eventType string,
	now time.Time,
) (webhookNotice, bool, error) {
	if delivery != nil && delivery.DeliveryID != "" && delivery.EventType == eventType {
		notice, err := buildWebhookNotice(task, delivery.DeliveryID, delivery.EventType, delivery.OccurredAt.Time)
		if err != nil {
			return webhookNotice{}, false, err
		}
		if notice.Digest == delivery.PayloadSHA256 {
			return notice, false, nil
		}
	}
	// Status times have second precision; keep OccurredAt identical to what a
	// round trip through the API server returns.
	occurredAt := now.UTC().Truncate(time.Second)
	notice, err := buildWebhookNotice(task, w.newID(), eventType, occurredAt)
	return notice, true, err
}

// webhookNoticeDisposition says whether a terminal Task owes its webhook a
// notice now.
type webhookNoticeDisposition int

const (
	// webhookNoticeDue means the notice is owed now.
	webhookNoticeDue webhookNoticeDisposition = iota
	// webhookNoticeDeferred means the Task's turn parked and its wait can
	// still resume the work, so no outcome is known yet.
	webhookNoticeDeferred
	// webhookNoticeContinued means the wait resumed into a continuation Task.
	// The continuation copies spec.webhookURL and reports instead.
	webhookNoticeContinued
)

// taskWebhookEvent returns the event a terminal Task's notice reports.
//
// A parked Task's phase is the outcome of its own turn (ADR 0041), not of the
// work, which continues through a controller-created continuation Task. Its
// notice therefore follows status.wait: it is deferred while the wait can
// still resume, owed by the continuation once the wait resumes, and, when the
// wait ends without resuming, reports why the work stopped: a cancelled wait
// is task.cancelled, and an expired or abandoned one is task.failed, as a
// Task that runs past its deadline is Failed. A turn that itself failed or was
// cancelled reports its phase, and a wait left by an earlier attempt that a
// later, unparked attempt superseded does not decide the event.
func taskWebhookEvent(task *corev1alpha1.Task) (string, webhookNoticeDisposition) {
	phaseEvent := webhookEventType(task.Status.Phase)
	wait := task.Status.Wait
	switch {
	case wait == nil:
		return phaseEvent, webhookNoticeDue
	case taskwait.Active(wait):
		return "", webhookNoticeDeferred
	case wait.State == corev1alpha1.TaskWaitStateResumed:
		return "", webhookNoticeContinued
	case task.Status.Phase != corev1alpha1.TaskPhaseSucceeded:
		return phaseEvent, webhookNoticeDue
	}
	execution := task.Status.Execution
	if execution == nil || execution.Attempt != wait.Attempt || execution.PromptID != wait.PromptID {
		return phaseEvent, webhookNoticeDue
	}
	if wait.State == corev1alpha1.TaskWaitStateCancelled {
		return webhookEventType(corev1alpha1.TaskPhaseCancelled), webhookNoticeDue
	}
	return webhookEventType(corev1alpha1.TaskPhaseFailed), webhookNoticeDue
}

// recordSigningUnavailable keeps the notice, already placed in task status,
// pending without consuming an attempt: nothing is ever sent unsigned, and a
// configuration fix resumes delivery. A retry policy with a signing hook may
// instead retire the notice unsent; it then reports zero so the caller moves
// on to the next notice.
func (w *WebhookNotifier) recordSigningUnavailable(
	ctx context.Context,
	status taskStatusWriter,
	task *corev1alpha1.Task,
	delivery *corev1alpha1.TaskWebhookDelivery,
	changed bool,
	retry webhookRetryPolicy,
	cause error,
) (time.Duration, error) {
	logf.FromContext(ctx).Error(cause, "webhook signing key unavailable; delivery deferred", "deliveryID", delivery.DeliveryID)
	requeue := webhookSigningRetryInterval
	if retry.signingUnavailable != nil {
		expired, next := retry.signingUnavailable(ctx, task, w.now())
		changed = changed || expired
		requeue = minPositiveDuration(requeue, next)
	}
	if delivery.State != corev1alpha1.TaskWebhookDeliveryPending {
		return 0, status.Update(ctx, task)
	}
	message := boundedWebhookError(errors.New("webhook signing key is unavailable"))
	if changed || delivery.LastError != message {
		delivery.LastError = message
		if err := status.Update(ctx, task); err != nil {
			return 0, err
		}
	}
	return requeue, nil
}

// ceilToSecond rounds t up to the next whole second.
func ceilToSecond(t time.Time) time.Time {
	if truncated := t.Truncate(time.Second); !truncated.Equal(t) {
		return truncated.Add(time.Second)
	}
	return t
}
