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
	if task.Spec.WebhookURL == "" || task.Status.WebhookDelivered {
		return 0, nil
	}
	delivery := task.Status.WebhookDelivery
	if delivery != nil && delivery.State != corev1alpha1.TaskWebhookDeliveryPending {
		return 0, nil
	}
	now := w.now()
	// Honor the lease of an in-flight attempt and the backoff of a failed one
	// before anything else, so newer Task state never accelerates retries.
	if delivery != nil && delivery.NextAttemptTime != nil && now.Before(delivery.NextAttemptTime.Time) {
		return delivery.NextAttemptTime.Sub(now), nil
	}

	notice, superseded, err := w.currentNotice(task, delivery, now)
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

	if _, _, policyErr := validateWebhookURLPolicy(task.Spec.WebhookURL, task.Namespace, w.policy); policyErr != nil {
		delivery.State = corev1alpha1.TaskWebhookDeliveryFailed
		delivery.NextAttemptTime = nil
		delivery.LastError = boundedWebhookError(policyErr)
		task.Status.WebhookDelivery = delivery
		log.Info("webhook destination rejected by policy", "deliveryID", delivery.DeliveryID, "reason", delivery.LastError)
		return 0, status.Update(ctx, task)
	}

	// An attempt whose result was never recorded (a restart or a failed status
	// write) still counts. Never claim past the bound.
	if !superseded && delivery.Attempts >= w.maxAttempts {
		delivery.State = corev1alpha1.TaskWebhookDeliveryFailed
		delivery.NextAttemptTime = nil
		if delivery.LastError == "" {
			delivery.LastError = "webhook delivery attempt limit reached"
		}
		task.Status.WebhookDelivery = delivery
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
		task.Status.WebhookDelivery = delivery
		logf.FromContext(ctx).Info("webhook delivery skipped: signing is disabled", "deliveryID", delivery.DeliveryID)
		return 0, status.Update(ctx, task)
	}
	keys, err := w.keys.SigningKeys(ctx)
	if err != nil {
		return w.recordSigningUnavailable(ctx, status, task, delivery, superseded, err)
	}

	// Claim the attempt durably before sending. Loading keys can block on
	// Secret reads, so the lease starts from a fresh clock reading. The claim
	// write and the send run under a timer started no later than that
	// reading, so this replica cannot begin a POST after its persisted lease
	// lets another replica claim the next attempt.
	leaseCtx, cancelLease := context.WithTimeout(ctx, w.attemptLease)
	defer cancelLease()
	now = w.now()
	attempt := delivery.Attempts + 1
	claimedAt := metav1.NewTime(now)
	lease := metav1.NewTime(now.Add(w.attemptLease))
	delivery.Attempts = attempt
	delivery.LastAttemptTime = &claimedAt
	delivery.NextAttemptTime = &lease
	task.Status.WebhookDelivery = delivery
	if err := status.Update(leaseCtx, task); err != nil {
		return 0, err
	}

	result := w.deliver(leaseCtx, task, notice, attempt, keys.Current)

	delivery = task.Status.WebhookDelivery
	delivery.LastStatusCode = result.StatusCode
	requeue := time.Duration(0)
	if result.Err == nil {
		delivery.State = corev1alpha1.TaskWebhookDeliveryDelivered
		delivery.NextAttemptTime = nil
		delivery.LastError = ""
		task.Status.WebhookDelivered = true
	} else {
		delivery.LastError = boundedWebhookError(result.Err)
		if attempt >= w.maxAttempts {
			delivery.State = corev1alpha1.TaskWebhookDeliveryFailed
			delivery.NextAttemptTime = nil
			log.Info("webhook delivery exhausted its attempts", "deliveryID", delivery.DeliveryID, "attempts", attempt, "reason", delivery.LastError)
		} else {
			requeue = w.backoff(attempt)
			next := metav1.NewTime(w.now().Add(requeue))
			delivery.NextAttemptTime = &next
			log.Info("webhook delivery attempt failed", "deliveryID", delivery.DeliveryID, "attempt", attempt, "retryIn", requeue, "reason", delivery.LastError)
		}
	}
	if err := status.Update(ctx, task); err != nil {
		// The claimed lease still bounds the next attempt.
		return 0, err
	}
	return requeue, nil
}

// currentNotice rebuilds the pending notice for the Task's current state. When
// that state no longer matches the recorded digest, or the event changed, the
// pending notice is superseded by a fresh one with a new delivery ID.
func (w *WebhookNotifier) currentNotice(task *corev1alpha1.Task, delivery *corev1alpha1.TaskWebhookDelivery, now time.Time) (webhookNotice, bool, error) {
	eventType := webhookEventType(task.Status.Phase)
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

// recordSigningUnavailable keeps the notice pending without consuming an
// attempt: nothing is ever sent unsigned, and a configuration fix resumes
// delivery.
func (w *WebhookNotifier) recordSigningUnavailable(
	ctx context.Context,
	status taskStatusWriter,
	task *corev1alpha1.Task,
	delivery *corev1alpha1.TaskWebhookDelivery,
	changed bool,
	cause error,
) (time.Duration, error) {
	logf.FromContext(ctx).Error(cause, "webhook signing key unavailable; delivery deferred", "deliveryID", delivery.DeliveryID)
	message := boundedWebhookError(errors.New("webhook signing key is unavailable"))
	if changed || delivery.LastError != message || task.Status.WebhookDelivery == nil {
		delivery.LastError = message
		task.Status.WebhookDelivery = delivery
		if err := status.Update(ctx, task); err != nil {
			return 0, err
		}
	}
	return webhookSigningRetryInterval, nil
}
