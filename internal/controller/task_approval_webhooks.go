/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/taskwait"
	"github.com/orka-agents/orka/internal/taskwebhook"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	ApprovalWebhookSchemaVersion = "orka.ai/task-approval-webhook/v1"

	approvalWebhookEventOverflow = "task.approval_overflow"

	defaultApprovalWebhookMaxAttempts = 5
	defaultApprovalWebhookMaxBackoff  = time.Minute

	approvalWebhookSigningWindow    = 5 * time.Minute
	approvalWebhookSigningExpired   = "webhook signing key unavailable past the approval notice window"
	approvalWebhookDeselectedReason = "the event is no longer selected in spec.webhookEvents"

	approvalWebhookAttemptsPerPass = 1
	approvalWebhookReadRetry       = 30 * time.Second
	approvalExpiryGrace            = 5 * time.Second
	approvalExpiryReason           = "approval expired"
)

var approvalWebhookNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://orka.ai/adr/0053/approval-webhook-delivery"))

var (
	approvalWebhookIDPattern       = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,256}$`)
	approvalWebhookToolPattern     = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,128}$`)
	approvalWebhookSeverityPattern = regexp.MustCompile(`^[a-z]{1,16}$`)
)

type ApprovalWebhookPayload struct {
	SchemaVersion string `json:"schemaVersion"`
	DeliveryID    string `json:"deliveryId"`
	EventType     string `json:"eventType"`
	OccurredAt    string `json:"occurredAt"`
	TaskUID       string `json:"taskUID"`
	TaskName      string `json:"taskName"`
	TaskNamespace string `json:"taskNamespace"`
	StreamSeq     int64  `json:"streamSeq"`
	ApprovalID    string `json:"approvalId,omitempty"`
	TargetTool    string `json:"targetTool,omitempty"`
	Severity      string `json:"severity,omitempty"`
	ExpiresAt     string `json:"expiresAt,omitempty"`
	Resolution    string `json:"resolution,omitempty"`
	Parked        bool   `json:"parked"`
}

func approvalWebhookEventType(transition corev1alpha1.TaskApprovalWebhookTransition) string {
	switch transition {
	case corev1alpha1.TaskApprovalWebhookRequested:
		return string(corev1alpha1.TaskWebhookEventApprovalRequested)
	case corev1alpha1.TaskApprovalWebhookResolved:
		return string(corev1alpha1.TaskWebhookEventApprovalResolved)
	default:
		return approvalWebhookEventOverflow
	}
}

type approvalWebhookSelection map[string]bool

func selectedApprovalWebhookEvents(task *corev1alpha1.Task) approvalWebhookSelection {
	selected := approvalWebhookSelection{}
	if strings.TrimSpace(task.Spec.WebhookURL) == "" {
		return selected
	}
	for _, event := range task.Spec.WebhookEvents {
		switch event {
		case corev1alpha1.TaskWebhookEventApprovalRequested, corev1alpha1.TaskWebhookEventApprovalResolved:
			selected[string(event)] = true
		}
	}
	return selected
}

func (s approvalWebhookSelection) allows(transition corev1alpha1.TaskApprovalWebhookTransition) bool {
	if transition == corev1alpha1.TaskApprovalWebhookOverflow {
		return len(s) > 0
	}
	return s[approvalWebhookEventType(transition)]
}

func approvalWebhookDeliveryID(taskUID, approvalID string, transition corev1alpha1.TaskApprovalWebhookTransition) string {
	name := strings.Join([]string{taskUID, approvalID, string(transition)}, "\x00")
	return uuid.NewSHA1(approvalWebhookNamespace, []byte(name)).String()
}

type approvalTransition struct {
	seq        int64
	transition corev1alpha1.TaskApprovalWebhookTransition
	approval   approvals.Approval
	occurredAt time.Time
}

func taskApprovals(listed []store.ExecutionEvent, taskUID string) []approvals.Approval {
	derived := approvals.Derive(approvals.FilterEventsForTaskUID(listed, taskUID), time.Time{})
	own := derived[:0]
	for _, approval := range derived {
		if approval.TaskUID == taskUID && approval.RequestSeq > 0 {
			own = append(own, approval)
		}
	}
	return own
}

func approvalWebhookTransitions(own []approvals.Approval) []approvalTransition {
	transitions := make([]approvalTransition, 0, 2*len(own))
	for _, approval := range own {
		transitions = append(transitions, approvalTransition{
			seq: approval.RequestSeq, transition: corev1alpha1.TaskApprovalWebhookRequested,
			approval: approval, occurredAt: approval.CreatedAt,
		})
		if approval.Status != approvals.StatusPending && approval.DecisionSeq > 0 && approval.DecisionTime != nil {
			transitions = append(transitions, approvalTransition{
				seq: approval.DecisionSeq, transition: corev1alpha1.TaskApprovalWebhookResolved,
				approval: approval, occurredAt: *approval.DecisionTime,
			})
		}
	}
	sort.SliceStable(transitions, func(i, j int) bool { return transitions[i].seq < transitions[j].seq })
	return transitions
}

func approvalIsParked(approval approvals.Approval) bool {
	return approval.Binding == nil && approval.Action == taskwait.ParkedApprovalAction
}

func approvalExpiryEnforced(approval approvals.Approval) bool {
	return approval.ExpiresAt != nil && (approval.Binding != nil || approvalIsParked(approval))
}

func newApprovalNotice(task *corev1alpha1.Task, t approvalTransition, now time.Time) (corev1alpha1.TaskApprovalWebhookNotice, bool, error) {
	approval := t.approval
	if !approvalWebhookIDPattern.MatchString(approval.ID) {
		return corev1alpha1.TaskApprovalWebhookNotice{}, false, nil
	}
	notice := corev1alpha1.TaskApprovalWebhookNotice{
		Transition: t.transition,
		StreamSeq:  t.seq,
		ApprovalID: approval.ID,
		Parked:     approvalIsParked(approval),
	}
	if approvalWebhookToolPattern.MatchString(approval.TargetTool) {
		notice.TargetTool = approval.TargetTool
	}
	if approvalWebhookSeverityPattern.MatchString(approval.Severity) {
		notice.Severity = approval.Severity
	}
	if approvalExpiryEnforced(approval) {
		expiresAt := metav1.NewTime(approval.ExpiresAt.UTC().Truncate(time.Second))
		notice.ExpiresAt = &expiresAt
	}
	if t.transition == corev1alpha1.TaskApprovalWebhookResolved {
		switch approval.Status {
		case approvals.StatusApproved, approvals.StatusDeclined, approvals.StatusExpired, approvals.StatusCancelled:
			notice.Resolution = approval.Status
		default:
			return corev1alpha1.TaskApprovalWebhookNotice{}, false, nil
		}
	}
	err := sealApprovalNotice(task, &notice, t.occurredAt, now)
	return notice, err == nil, err
}

func newApprovalOverflowNotice(task *corev1alpha1.Task, firstOmittedSeq int64, now time.Time) (corev1alpha1.TaskApprovalWebhookNotice, error) {
	notice := corev1alpha1.TaskApprovalWebhookNotice{
		Transition: corev1alpha1.TaskApprovalWebhookOverflow,
		StreamSeq:  firstOmittedSeq,
	}
	return notice, sealApprovalNotice(task, &notice, now, now)
}

func sealApprovalNotice(task *corev1alpha1.Task, notice *corev1alpha1.TaskApprovalWebhookNotice, occurredAt, queuedAt time.Time) error {
	notice.DeliveryID = approvalWebhookDeliveryID(string(task.UID), notice.ApprovalID, notice.Transition)
	notice.EventType = approvalWebhookEventType(notice.Transition)
	notice.OccurredAt = metav1.NewTime(occurredAt.UTC().Truncate(time.Second))
	notice.State = corev1alpha1.TaskWebhookDeliveryPending
	notice.SigningDeadline = approvalSigningDeadline(queuedAt)
	rendered, err := renderApprovalNotice(task, notice)
	if err != nil {
		return err
	}
	notice.PayloadSHA256 = rendered.Digest
	return nil
}

func renderApprovalNotice(task *corev1alpha1.Task, notice *corev1alpha1.TaskApprovalWebhookNotice) (webhookNotice, error) {
	payload := ApprovalWebhookPayload{
		SchemaVersion: ApprovalWebhookSchemaVersion,
		DeliveryID:    notice.DeliveryID,
		EventType:     notice.EventType,
		OccurredAt:    notice.OccurredAt.UTC().Format(time.RFC3339),
		TaskUID:       string(task.UID),
		TaskName:      task.Name,
		TaskNamespace: task.Namespace,
		StreamSeq:     notice.StreamSeq,
		ApprovalID:    notice.ApprovalID,
		TargetTool:    notice.TargetTool,
		Severity:      notice.Severity,
		Resolution:    notice.Resolution,
		Parked:        notice.Parked,
	}
	if notice.ExpiresAt != nil {
		payload.ExpiresAt = notice.ExpiresAt.UTC().Format(time.RFC3339)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return webhookNotice{}, fmt.Errorf("failed to marshal approval webhook payload: %w", err)
	}
	return webhookNotice{
		DeliveryID: notice.DeliveryID,
		EventType:  notice.EventType,
		OccurredAt: notice.OccurredAt.Time,
		Body:       body,
		Digest:     taskwebhook.PayloadDigest(body),
	}, nil
}

func projectApprovalNotices(
	ctx context.Context,
	task *corev1alpha1.Task,
	transitions []approvalTransition,
	selected approvalWebhookSelection,
	now time.Time,
) (bool, error) {
	state := task.Status.ApprovalWebhooks
	if state == nil {
		state = &corev1alpha1.TaskApprovalWebhooks{}
	}
	limit := corev1alpha1.MaxTaskApprovalWebhookNotices - 1
	changed := false
	for _, t := range transitions {
		if t.seq <= state.ProjectedSeq {
			continue
		}
		state.ProjectedSeq = t.seq
		changed = true
		if !selected.allows(t.transition) {
			continue
		}
		if state.Overflowed {
			state.Omitted++
			continue
		}
		notice, ok, err := newApprovalNotice(task, t, now)
		if err != nil {
			return false, err
		}
		if !ok {
			logf.FromContext(ctx).Info("approval transition not notified: its identifiers are not safe to send", "streamSeq", t.seq)
			continue
		}
		if slices.ContainsFunc(state.Notices, func(existing corev1alpha1.TaskApprovalWebhookNotice) bool {
			return existing.DeliveryID == notice.DeliveryID
		}) {
			continue
		}
		pruneSettledApprovalNotices(state, limit)
		if len(state.Notices) >= limit {
			overflow, err := newApprovalOverflowNotice(task, t.seq, now)
			if err != nil {
				return false, err
			}
			state.Overflowed = true
			state.Omitted++
			state.Notices = append(state.Notices, overflow)
			logf.FromContext(ctx).Info("approval webhook queue overflowed; later approval transitions are not notified", "streamSeq", t.seq)
			continue
		}
		state.Notices = append(state.Notices, notice)
	}
	if changed {
		task.Status.ApprovalWebhooks = state
	}
	return changed, nil
}

func pruneSettledApprovalNotices(state *corev1alpha1.TaskApprovalWebhooks, limit int) {
	for len(state.Notices) >= limit {
		index := slices.IndexFunc(state.Notices, func(notice corev1alpha1.TaskApprovalWebhookNotice) bool {
			return notice.State != corev1alpha1.TaskWebhookDeliveryPending
		})
		if index < 0 {
			return
		}
		state.Notices = slices.Delete(state.Notices, index, index+1)
		state.Pruned++
	}
}

func recordDueApprovalExpiries(
	ctx context.Context,
	eventStore store.ExecutionEventStore,
	task *corev1alpha1.Task,
	own []approvals.Approval,
	now time.Time,
) (bool, time.Duration, error) {
	attempted := false
	next := time.Duration(0)
	for _, approval := range own {
		if approval.Status != approvals.StatusPending || approval.Binding == nil || approval.ExpiresAt == nil {
			continue
		}
		due := approval.ExpiresAt.Add(approvalExpiryGrace)
		if now.Before(due) {
			if wait := due.Sub(now); next == 0 || wait < next {
				next = wait
			}
			continue
		}
		attempted = true
		_, err := withRetainedMCPApproval(ctx, eventStore, task.Namespace, task.Name, mcpApprovalNamespaceFence, approval,
			func(txCtx context.Context, current approvals.Approval, listed []store.ExecutionEvent) (*store.ExecutionEvent, error) {
				if current.Status != approvals.StatusPending {
					return nil, nil
				}
				event, err := approvalExpiryEvent(task, listed, current)
				if err != nil {
					return nil, err
				}
				appended, err := eventStore.AppendExecutionEvent(txCtx, event)
				if errors.Is(err, store.ErrConflict) {
					return nil, nil
				}
				return appended, err
			})
		if err != nil {
			return attempted, next, fmt.Errorf("record approval expiry: %w", err)
		}
	}
	return attempted, next, nil
}

func approvalExpiryEvent(task *corev1alpha1.Task, listed []store.ExecutionEvent, approval approvals.Approval) (*store.ExecutionEvent, error) {
	var request store.ExecutionEvent
	for _, event := range listed {
		if event.Seq == approval.RequestSeq {
			request = event
			break
		}
	}
	content, err := json.Marshal(map[string]string{
		"approvalID": approval.ID,
		"taskUID":    string(task.UID),
		"reason":     approvalExpiryReason,
		"actor":      "orka-controller",
	})
	if err != nil {
		return nil, err
	}
	return &store.ExecutionEvent{
		Namespace: task.Namespace, StreamType: events.ExecutionEventStreamTypeTask, StreamID: task.Name,
		TaskName: task.Name, SessionName: request.SessionName, AgentName: request.AgentName,
		Type: events.ExecutionEventTypeApprovalExpired, Severity: events.ExecutionEventSeverityInfo,
		ToolName: request.ToolName, ToolCallID: approval.ID, Summary: approvalExpiryReason, Content: content,
	}, nil
}

func (w *WebhookNotifier) prepareApprovalNotices(
	ctx context.Context,
	status taskStatusWriter,
	eventStore store.ExecutionEventStore,
	task *corev1alpha1.Task,
) (time.Duration, error) {
	selected := selectedApprovalWebhookEvents(task)
	retired := retireDeselectedApprovalNotices(ctx, task.Status.ApprovalWebhooks, selected)
	if len(selected) == 0 {
		if state := task.Status.ApprovalWebhooks; state != nil && state.Tracking {
			state.Tracking = false
			retired = true
		}
		if retired {
			if err := status.Update(ctx, task); err != nil {
				return 0, err
			}
		}
		return 0, nil
	}
	if retired {
		if err := status.Update(ctx, task); err != nil {
			return 0, err
		}
	}
	if eventStore == nil || task.UID == "" {
		return 0, nil
	}
	if err := w.retireUnsignableApprovalNotices(ctx, status, task); err != nil {
		return 0, err
	}
	requeue := time.Duration(0)
	projected, err := w.projectTaskApprovalEvents(ctx, eventStore, task, selected)
	switch {
	case err != nil:
		logf.FromContext(ctx).Error(err, "failed to project approval webhook notices")
		requeue = approvalWebhookReadRetry
	case projected.changed:
		if err := status.Update(ctx, task); err != nil {
			return 0, err
		}
	}
	return minPositiveDuration(requeue, projected.nextExpiry, projected.requeue), nil
}

func (w *WebhookNotifier) sendApprovalNotices(ctx context.Context, status taskStatusWriter, task *corev1alpha1.Task) (time.Duration, error) {
	if firstPendingApprovalNotice(task.Status.ApprovalWebhooks) < 0 {
		return 0, nil
	}
	return w.deliverApprovalNotices(ctx, status, task, selectedApprovalWebhookEvents(task))
}

type approvalProjection struct {
	changed    bool
	nextExpiry time.Duration
	requeue    time.Duration
}

const approvalWebhookTrackingRequeue = time.Second

func (w *WebhookNotifier) projectTaskApprovalEvents(
	ctx context.Context,
	eventStore store.ExecutionEventStore,
	task *corev1alpha1.Task,
	selected approvalWebhookSelection,
) (approvalProjection, error) {
	uid := string(task.UID)
	listed, err := approvals.ListEvents(ctx, eventStore, task.Namespace, task.Name)
	if err != nil {
		return approvalProjection{}, err
	}
	var result approvalProjection
	if startApprovalWebhookTracking(ctx, task, listed) {
		result.changed = true
		result.requeue = approvalWebhookTrackingRequeue
		return result, nil
	}
	own := taskApprovals(listed, uid)
	if selected.allows(corev1alpha1.TaskApprovalWebhookResolved) {
		attempted, next, err := recordDueApprovalExpiries(ctx, eventStore, task, own, w.now())
		result.nextExpiry = next
		if err != nil {
			return result, err
		}
		if attempted {
			if listed, err = approvals.ListEvents(ctx, eventStore, task.Namespace, task.Name); err != nil {
				return result, err
			}
			own = taskApprovals(listed, uid)
		}
	}
	projected, err := projectApprovalNotices(ctx, task, approvalWebhookTransitions(own), selected, w.now())
	result.changed = result.changed || projected
	return result, err
}

func startTrackingInitialApprovalSelection(task *corev1alpha1.Task) {
	if len(selectedApprovalWebhookEvents(task)) > 0 && task.Status.ApprovalWebhooks == nil {
		task.Status.ApprovalWebhooks = &corev1alpha1.TaskApprovalWebhooks{Tracking: true}
	}
}

func startApprovalWebhookTracking(ctx context.Context, task *corev1alpha1.Task, listed []store.ExecutionEvent) bool {
	state := task.Status.ApprovalWebhooks
	if state != nil && state.Tracking {
		return false
	}
	if state == nil {
		state = &corev1alpha1.TaskApprovalWebhooks{}
	}
	for _, event := range listed {
		state.ProjectedSeq = max(state.ProjectedSeq, event.Seq)
	}
	state.Tracking = true
	task.Status.ApprovalWebhooks = state
	logf.FromContext(ctx).Info("approval webhook events selected; earlier approval history is not notified", "projectedSeq", state.ProjectedSeq)
	return true
}

func (w *WebhookNotifier) deliverApprovalNotices(
	ctx context.Context,
	status taskStatusWriter,
	task *corev1alpha1.Task,
	selected approvalWebhookSelection,
) (time.Duration, error) {
	if retireDeselectedApprovalNotices(ctx, task.Status.ApprovalWebhooks, selected) {
		if err := status.Update(ctx, task); err != nil {
			return 0, err
		}
	}
	for range approvalWebhookAttemptsPerPass {
		index := firstPendingApprovalNotice(task.Status.ApprovalWebhooks)
		if index < 0 {
			return 0, nil
		}
		head := &task.Status.ApprovalWebhooks.Notices[index]
		now := w.now()
		if head.NextAttemptTime != nil && now.Before(head.NextAttemptTime.Time) {
			return head.NextAttemptTime.Sub(now), nil
		}
		if !selected.allows(head.Transition) {
			retireApprovalNotice(ctx, head, approvalWebhookDeselectedReason)
			if err := status.Update(ctx, task); err != nil {
				return 0, err
			}
			continue
		}
		notice, err := renderApprovalNotice(task, head)
		if err != nil || notice.Digest != head.PayloadSHA256 {
			retireApprovalNotice(ctx, head, "the recorded approval notice no longer matches its payload digest")
			if err := status.Update(ctx, task); err != nil {
				return 0, err
			}
			continue
		}
		transition := head.Transition
		stillCurrent := func(current *corev1alpha1.Task) bool {
			record := approvalNoticeRecordOf(current, notice.DeliveryID)
			if record == nil || !selectedApprovalWebhookEvents(current).allows(transition) {
				return false
			}
			rendered, err := renderApprovalNotice(current, record)
			return err == nil && rendered.Digest == notice.Digest
		}
		requeue, err := w.attemptNotice(ctx, status, task, approvalNoticeRecord(head.DeliveryID), notice, false, w.approvalRetry(), stillCurrent, nil)
		if err != nil || requeue > 0 {
			return requeue, err
		}
	}
	if firstPendingApprovalNotice(task.Status.ApprovalWebhooks) >= 0 {
		return time.Second, nil
	}
	return 0, nil
}

func (w *WebhookNotifier) approvalRetry() webhookRetryPolicy {
	return webhookRetryPolicy{
		maxAttempts:        w.approvalMaxAttempts,
		maxBackoff:         w.approvalMaxBackoff,
		signingUnavailable: expireUnsignedApprovalNotices,
	}
}

func approvalSigningDeadline(queuedAt time.Time) *metav1.Time {
	deadline := metav1.NewTime(ceilToSecond(queuedAt.UTC().Add(approvalWebhookSigningWindow)))
	return &deadline
}

func (w *WebhookNotifier) retireUnsignableApprovalNotices(ctx context.Context, status taskStatusWriter, task *corev1alpha1.Task) error {
	state := task.Status.ApprovalWebhooks
	if w.keys == nil || state == nil {
		return nil
	}
	now := w.now()
	if !slices.ContainsFunc(state.Notices, func(notice corev1alpha1.TaskApprovalWebhookNotice) bool {
		return notice.State == corev1alpha1.TaskWebhookDeliveryPending && notice.SigningDeadline != nil && !now.Before(notice.SigningDeadline.Time)
	}) {
		return nil
	}
	if _, err := w.keys.SigningKeys(ctx); err == nil {
		return nil
	}
	if changed, _ := expireUnsignedApprovalNotices(ctx, task, now); changed {
		return status.Update(ctx, task)
	}
	return nil
}

func expireUnsignedApprovalNotices(ctx context.Context, task *corev1alpha1.Task, now time.Time) (bool, time.Duration) {
	state := task.Status.ApprovalWebhooks
	if state == nil {
		return false, 0
	}
	changed := false
	next := time.Duration(0)
	for i := range state.Notices {
		notice := &state.Notices[i]
		if notice.State != corev1alpha1.TaskWebhookDeliveryPending {
			continue
		}
		if notice.SigningDeadline == nil {
			notice.SigningDeadline = approvalSigningDeadline(now)
			changed = true
		}
		if now.Before(notice.SigningDeadline.Time) {
			next = minPositiveDuration(next, notice.SigningDeadline.Sub(now))
			continue
		}
		retireApprovalNotice(ctx, notice, approvalWebhookSigningExpired)
		changed = true
	}
	return changed, next
}

func approvalNoticeRecord(deliveryID string) webhookRecordRef {
	return func(task *corev1alpha1.Task) *corev1alpha1.TaskWebhookDelivery {
		if notice := approvalNoticeRecordOf(task, deliveryID); notice != nil {
			return &notice.TaskWebhookDelivery
		}
		return nil
	}
}

func approvalNoticeRecordOf(task *corev1alpha1.Task, deliveryID string) *corev1alpha1.TaskApprovalWebhookNotice {
	state := task.Status.ApprovalWebhooks
	if state == nil {
		return nil
	}
	for i := range state.Notices {
		if state.Notices[i].DeliveryID == deliveryID {
			return &state.Notices[i]
		}
	}
	return nil
}

func firstPendingApprovalNotice(state *corev1alpha1.TaskApprovalWebhooks) int {
	if state == nil {
		return -1
	}
	return slices.IndexFunc(state.Notices, func(notice corev1alpha1.TaskApprovalWebhookNotice) bool {
		return notice.State == corev1alpha1.TaskWebhookDeliveryPending
	})
}

func retireDeselectedApprovalNotices(ctx context.Context, state *corev1alpha1.TaskApprovalWebhooks, selected approvalWebhookSelection) bool {
	if state == nil {
		return false
	}
	retired := false
	for i := range state.Notices {
		notice := &state.Notices[i]
		if notice.State == corev1alpha1.TaskWebhookDeliveryPending && !selected.allows(notice.Transition) {
			retireApprovalNotice(ctx, notice, approvalWebhookDeselectedReason)
			retired = true
		}
	}
	return retired
}

func retireApprovalNotice(ctx context.Context, notice *corev1alpha1.TaskApprovalWebhookNotice, reason string) {
	notice.State = corev1alpha1.TaskWebhookDeliveryFailed
	notice.NextAttemptTime = nil
	notice.LastError = boundedWebhookError(errors.New(reason))
	logf.FromContext(ctx).Info("retiring approval webhook notice", "deliveryID", notice.DeliveryID, "reason", reason)
}

func minPositiveDuration(values ...time.Duration) time.Duration {
	out := time.Duration(0)
	for _, value := range values {
		if value > 0 && (out == 0 || value < out) {
			out = value
		}
	}
	return out
}
