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

// Approval webhook notices (ADR 0053).
//
// A Task that selects approval events in spec.webhookEvents has its durable
// approval events projected, during Task reconcile, into a bounded queue in
// status.approvalWebhooks. The projection cursor and every new notice are
// persisted in one status write before any attempt, so a restarted
// controller never projects a transition twice. Each notice is then sent with
// the terminal notice's machinery: dial-time destination checks, Ed25519 JWS
// signing, a durable attempt lease, and a bounded number of attempts.
const (
	// ApprovalWebhookSchemaVersion identifies the approval notice body.
	ApprovalWebhookSchemaVersion = "orka.ai/task-approval-webhook/v1"

	approvalWebhookEventOverflow = "task.approval_overflow"

	// Review windows are about ten minutes, so an approval notice that cannot
	// be delivered within a few minutes has lost its value. Five attempts at
	// 10s, 20s, 40s and 60s spacing settle within about three minutes and do
	// not hold back later notices of the same Task for longer than that.
	defaultApprovalWebhookMaxAttempts = 5
	defaultApprovalWebhookMaxBackoff  = time.Minute

	// approvalWebhookAttemptsPerPass bounds the sends of one reconcile. Task
	// reconciles share one worker, so, as for the terminal notice, a slow
	// receiver holds it for at most one request per pass.
	approvalWebhookAttemptsPerPass = 1
	// approvalWebhookReadRetry paces retries after an event store failure.
	approvalWebhookReadRetry = 30 * time.Second
	// approvalExpiryGrace lets the broker, which owns a live review, record
	// its own expiry before the controller records it for a lost one.
	approvalExpiryGrace = 5 * time.Second
	// approvalExpiryReason is the reason recorded with a controller expiry.
	approvalExpiryReason = "approval expired"
)

// approvalWebhookNamespace scopes deterministic approval notice delivery IDs.
var approvalWebhookNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://orka.ai/adr/0053/approval-webhook-delivery"))

var (
	approvalWebhookIDPattern       = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,256}$`)
	approvalWebhookToolPattern     = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,128}$`)
	approvalWebhookSeverityPattern = regexp.MustCompile(`^[a-z]{1,16}$`)
)

// ApprovalWebhookPayload is the approval notice body. It carries identifiers
// only: never argument previews, action or risk text, reasons, digests,
// bindings, or the identity of whoever decided.
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

// approvalWebhookEventType is the notice event of a transition.
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

// approvalWebhookSelection is the set of approval events a Task selected.
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

// allows reports whether a notice of transition is still wanted. The
// overflow notice is part of any approval selection.
func (s approvalWebhookSelection) allows(transition corev1alpha1.TaskApprovalWebhookTransition) bool {
	if transition == corev1alpha1.TaskApprovalWebhookOverflow {
		return len(s) > 0
	}
	return s[approvalWebhookEventType(transition)]
}

// approvalWebhookDeliveryID is UUIDv5(task UID, approval ID, transition): the
// same transition always has the same ID, across restarts and replicas.
func approvalWebhookDeliveryID(taskUID, approvalID string, transition corev1alpha1.TaskApprovalWebhookTransition) string {
	name := strings.Join([]string{taskUID, approvalID, string(transition)}, "\x00")
	return uuid.NewSHA1(approvalWebhookNamespace, []byte(name)).String()
}

// approvalTransition is one projectable approval change.
type approvalTransition struct {
	seq        int64
	transition corev1alpha1.TaskApprovalWebhookTransition
	approval   approvals.Approval
	occurredAt time.Time
}

// taskApprovals derives the approvals the Task itself requested. Only
// explicit approval events decide an approval, as for the controller and the
// approval API; an approval whose request names another Task UID, or none,
// never reaches this Task's receiver.
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

// approvalWebhookTransitions lists every requested and resolved transition
// in stream order. Each has the stable time of the event that recorded it.
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

// approvalExpiryEnforced reports whether the approval model enforces the
// approval's expiresAt: brokered reviews and parked sign-offs. AI-worker
// approvals resolve only by explicit decision.
func approvalExpiryEnforced(approval approvals.Approval) bool {
	return approval.ExpiresAt != nil && (approval.Binding != nil || approvalIsParked(approval))
}

// newApprovalNotice builds the record for a transition, keeping only
// identifiers that match their safe shape. It reports false when the
// approval ID itself is not safe to send.
func newApprovalNotice(task *corev1alpha1.Task, t approvalTransition) (corev1alpha1.TaskApprovalWebhookNotice, bool, error) {
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
	err := sealApprovalNotice(task, &notice, t.occurredAt)
	return notice, err == nil, err
}

// newApprovalOverflowNotice builds the single notice that reports omitted
// transitions, starting at firstOmittedSeq.
func newApprovalOverflowNotice(task *corev1alpha1.Task, firstOmittedSeq int64, now time.Time) (corev1alpha1.TaskApprovalWebhookNotice, error) {
	notice := corev1alpha1.TaskApprovalWebhookNotice{
		Transition: corev1alpha1.TaskApprovalWebhookOverflow,
		StreamSeq:  firstOmittedSeq,
	}
	return notice, sealApprovalNotice(task, &notice, now)
}

// sealApprovalNotice fills the delivery record of a new notice: its
// deterministic ID, event, time, and the digest of the body rendered from
// the record alone.
func sealApprovalNotice(task *corev1alpha1.Task, notice *corev1alpha1.TaskApprovalWebhookNotice, occurredAt time.Time) error {
	notice.DeliveryID = approvalWebhookDeliveryID(string(task.UID), notice.ApprovalID, notice.Transition)
	notice.EventType = approvalWebhookEventType(notice.Transition)
	// Status times have second precision; the body must survive a round trip.
	notice.OccurredAt = metav1.NewTime(occurredAt.UTC().Truncate(time.Second))
	notice.State = corev1alpha1.TaskWebhookDeliveryPending
	rendered, err := renderApprovalNotice(task, notice)
	if err != nil {
		return err
	}
	notice.PayloadSHA256 = rendered.Digest
	return nil
}

// renderApprovalNotice renders the exact body of a recorded notice.
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

// projectApprovalNotices records a notice for every selected transition past
// the cursor and advances the cursor. It reports whether status changed.
//
// Overflow policy: a pending notice is never removed. Settled notices are
// pruned, oldest first, to make room. When every slot but the reserved one
// holds a pending notice, a single overflow notice is recorded in that slot
// and no later transition is notified; status counts them in omitted. The
// cursor still advances, so nothing is regenerated afterwards.
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
		notice, ok, err := newApprovalNotice(task, t)
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

// pruneSettledApprovalNotices removes settled notices, oldest first, until
// fewer than limit remain or only pending notices are left.
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

// recordDueApprovalExpiries makes clock expiry durable before it is
// notified. A brokered review past its expiresAt is already expired for the
// approval API and the broker; when nothing recorded that, the controller
// appends the ApprovalExpired event once. The store keeps one terminal event
// per approval, so a decision that landed first wins and this append is
// refused. It reports whether an append was attempted, so the caller reads
// the approvals again, and when the next pending expiry falls due.
//
// Like the broker's recovery, the append copies the request's Session
// association from history. It therefore runs under the namespace cleanup
// fence against a fresh read of the retained request: timeline or Session
// cleanup that commits after that read refuses the append and the read is
// repeated, so the controller never recreates a cleaned request's history
// or restores a cleaned Session association.
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
					return nil, nil // A terminal event recorded since the first read wins.
				}
				event, err := approvalExpiryEvent(task, listed, current)
				if err != nil {
					return nil, err
				}
				appended, err := eventStore.AppendExecutionEvent(txCtx, event)
				if errors.Is(err, store.ErrConflict) {
					return nil, nil // Another terminal event won; read it again.
				}
				return appended, err
			})
		if err != nil {
			return attempted, next, fmt.Errorf("record approval expiry: %w", err)
		}
	}
	return attempted, next, nil
}

// approvalExpiryEvent builds the controller's ApprovalExpired event from a
// fenced read of the approval's history, copying the request's current
// Session and agent association.
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

// reconcileApprovalNotices projects the Task's approval events into notices
// and advances their delivery. It returns how long to wait before calling
// again (zero when nothing is pending). An event store failure never blocks
// the Task: it is logged and retried later, while recorded notices are still
// delivered.
func (w *WebhookNotifier) reconcileApprovalNotices(
	ctx context.Context,
	status taskStatusWriter,
	eventStore store.ExecutionEventStore,
	task *corev1alpha1.Task,
) (time.Duration, error) {
	selected := selectedApprovalWebhookEvents(task)
	if len(selected) == 0 {
		if state := task.Status.ApprovalWebhooks; state != nil && state.Tracking {
			// Opting out ends tracking, so selecting again starts from the
			// stream's high-water mark rather than replaying what happened
			// while nothing was selected.
			state.Tracking = false
			if err := status.Update(ctx, task); err != nil {
				return 0, err
			}
		}
		if firstPendingApprovalNotice(task.Status.ApprovalWebhooks) < 0 {
			return 0, nil
		}
	}
	log := logf.FromContext(ctx)
	requeue := time.Duration(0)
	if len(selected) > 0 && eventStore != nil && task.UID != "" {
		projected, err := w.projectTaskApprovalEvents(ctx, eventStore, task, selected)
		switch {
		case err != nil:
			log.Error(err, "failed to project approval webhook notices")
			requeue = approvalWebhookReadRetry
		case projected.changed:
			if err := status.Update(ctx, task); err != nil {
				return 0, err
			}
		}
		requeue = minPositiveDuration(requeue, projected.nextExpiry)
	}
	delivery, err := w.deliverApprovalNotices(ctx, status, task, selected)
	if err != nil {
		return 0, err
	}
	return minPositiveDuration(requeue, delivery), nil
}

type approvalProjection struct {
	changed    bool
	nextExpiry time.Duration
}

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
	result.changed = startApprovalWebhookTracking(ctx, task, listed)
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

// startTrackingInitialApprovalSelection marks, while the controller
// initializes a new Task's status, that the Task selected approval events
// from the start. Nothing has run yet, so every later transition of the Task
// is projected.
func startTrackingInitialApprovalSelection(task *corev1alpha1.Task) {
	if len(selectedApprovalWebhookEvents(task)) > 0 && task.Status.ApprovalWebhooks == nil {
		task.Status.ApprovalWebhooks = &corev1alpha1.TaskApprovalWebhooks{Tracking: true}
	}
}

// startApprovalWebhookTracking sets the selection boundary when the
// controller observes a Task selecting approval events that it did not
// select when its status was initialized, or that it selects again after the
// controller observed it selecting none. The cursor is first raised to the
// stream's high-water mark in listed, so approval history from before the
// selection is never notified and cannot fill the queue. A selection that
// was removed and restored between two observations is not seen as removed.
// It reports whether status changed.
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

// deliverApprovalNotices sends pending notices in stream order. A later
// notice waits until every earlier one has settled, so a receiver sees a
// resolution only after its request, unless the request's notice failed.
func (w *WebhookNotifier) deliverApprovalNotices(
	ctx context.Context,
	status taskStatusWriter,
	task *corev1alpha1.Task,
	selected approvalWebhookSelection,
) (time.Duration, error) {
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
			retireApprovalNotice(ctx, head, "the event is no longer selected in spec.webhookEvents")
			if err := status.Update(ctx, task); err != nil {
				return 0, err
			}
			continue
		}
		notice, err := renderApprovalNotice(task, head)
		if err != nil || notice.Digest != head.PayloadSHA256 {
			// Never send a body other than the one recorded.
			retireApprovalNotice(ctx, head, "the recorded approval notice no longer matches its payload digest")
			if err := status.Update(ctx, task); err != nil {
				return 0, err
			}
			continue
		}
		requeue, err := w.attemptNotice(ctx, status, task, approvalNoticeRecord(head.DeliveryID), notice, false, w.approvalRetry(), nil)
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
	return webhookRetryPolicy{maxAttempts: w.approvalMaxAttempts, maxBackoff: w.approvalMaxBackoff}
}

func approvalNoticeRecord(deliveryID string) webhookRecordRef {
	return func(task *corev1alpha1.Task) *corev1alpha1.TaskWebhookDelivery {
		state := task.Status.ApprovalWebhooks
		if state == nil {
			return nil
		}
		for i := range state.Notices {
			if state.Notices[i].DeliveryID == deliveryID {
				return &state.Notices[i].TaskWebhookDelivery
			}
		}
		return nil
	}
}

func firstPendingApprovalNotice(state *corev1alpha1.TaskApprovalWebhooks) int {
	if state == nil {
		return -1
	}
	return slices.IndexFunc(state.Notices, func(notice corev1alpha1.TaskApprovalWebhookNotice) bool {
		return notice.State == corev1alpha1.TaskWebhookDeliveryPending
	})
}

func retireApprovalNotice(ctx context.Context, notice *corev1alpha1.TaskApprovalWebhookNotice, reason string) {
	notice.State = corev1alpha1.TaskWebhookDeliveryFailed
	notice.NextAttemptTime = nil
	notice.LastError = boundedWebhookError(errors.New(reason))
	logf.FromContext(ctx).Info("retiring approval webhook notice", "deliveryID", notice.DeliveryID, "reason", reason)
}

// minPositiveDuration returns the smallest positive value, or zero.
func minPositiveDuration(values ...time.Duration) time.Duration {
	out := time.Duration(0)
	for _, value := range values {
		if value > 0 && (out == 0 || value < out) {
			out = value
		}
	}
	return out
}
