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

type taskStatusWriter interface {
	Update(ctx context.Context, obj ctrlclient.Object, opts ...ctrlclient.SubResourceUpdateOption) error
}

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

func terminalNoticeStillCurrent(task *corev1alpha1.Task, notice webhookNotice) bool {
	eventType, disposition := taskWebhookEvent(task)
	if disposition != webhookNoticeDue || eventType != notice.EventType {
		return false
	}
	rebuilt, err := buildWebhookNotice(task, notice.DeliveryID, notice.EventType, notice.OccurredAt)
	return err == nil && rebuilt.Digest == notice.Digest
}

func terminalNoticeOutstanding(task *corev1alpha1.Task, now time.Time) (bool, time.Duration) {
	if task.Spec.WebhookURL == "" || task.Status.WebhookDelivered {
		return false, 0
	}
	delivery := task.Status.WebhookDelivery
	if delivery != nil && delivery.State != corev1alpha1.TaskWebhookDeliveryPending {
		return false, 0
	}
	if delivery != nil && delivery.NextAttemptTime != nil && now.Before(delivery.NextAttemptTime.Time) {
		return true, delivery.NextAttemptTime.Sub(now)
	}
	switch _, disposition := taskWebhookEvent(task); disposition {
	case webhookNoticeDeferred:
		return false, 0
	case webhookNoticeContinued:
		return delivery != nil, 0
	}
	return true, 0
}

type webhookRecordRef func(task *corev1alpha1.Task) *corev1alpha1.TaskWebhookDelivery

func terminalWebhookRecord(task *corev1alpha1.Task) *corev1alpha1.TaskWebhookDelivery {
	return task.Status.WebhookDelivery
}

type webhookRetryPolicy struct {
	maxAttempts        int32
	maxBackoff         time.Duration
	signingUnavailable func(ctx context.Context, task *corev1alpha1.Task, now time.Time) (bool, time.Duration)
}

func (w *WebhookNotifier) terminalRetry() webhookRetryPolicy {
	return webhookRetryPolicy{maxAttempts: w.maxAttempts, maxBackoff: w.maxBackoff}
}

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

	leaseCtx, cancelLease := context.WithTimeout(ctx, w.attemptLease)
	defer cancelLease()
	now := w.now()
	attempt := delivery.Attempts + 1
	claimedAt := metav1.NewTime(now)
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
		return 0, errors.New("webhook notice record is missing from Task status after its claim")
	}
	switch {
	case result.Err == nil:
	case nextAttempt == nil:
		log.Info("webhook delivery exhausted its attempts", "deliveryID", notice.DeliveryID, "attempts", attempt, "reason", boundedWebhookError(result.Err))
	default:
		log.Info("webhook delivery attempt failed", "deliveryID", notice.DeliveryID, "attempt", attempt, "retryIn", requeue, "reason", boundedWebhookError(result.Err))
	}
	sentTo := task.Spec.WebhookURL
	if err := recordWebhookAttemptResult(ctx, status, task, func(current *corev1alpha1.Task) bool {
		return current.Spec.WebhookURL == sentTo && stillCurrent(current) && recordResult(current)
	}); err != nil {
		return 0, err
	}
	return requeue, nil
}

func holdsWebhookAttemptClaim(delivery *corev1alpha1.TaskWebhookDelivery, attempt int32, lease *metav1.Time) bool {
	return delivery != nil &&
		delivery.State == corev1alpha1.TaskWebhookDeliveryPending &&
		delivery.Attempts == attempt &&
		delivery.NextAttemptTime != nil && delivery.NextAttemptTime.Equal(lease)
}

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
	occurredAt := now.UTC().Truncate(time.Second)
	notice, err := buildWebhookNotice(task, w.newID(), eventType, occurredAt)
	return notice, true, err
}

type webhookNoticeDisposition int

const (
	webhookNoticeDue webhookNoticeDisposition = iota
	webhookNoticeDeferred
	webhookNoticeContinued
)

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

func ceilToSecond(t time.Time) time.Time {
	if truncated := t.Truncate(time.Second); !truncated.Equal(t) {
		return truncated.Add(time.Second)
	}
	return t
}
