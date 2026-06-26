package approvals

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/store"
)

const (
	StatusPending   = "pending"
	StatusApproved  = "approved"
	StatusDeclined  = "declined"
	StatusExpired   = "expired"
	StatusCancelled = "cancelled"
)

type Approval struct {
	ID                string          `json:"id"`
	Action            string          `json:"action"`
	RiskSummary       string          `json:"riskSummary,omitempty"`
	TaskUID           string          `json:"taskUID,omitempty"`
	TargetTool        string          `json:"targetTool,omitempty"`
	TargetArgsDigest  string          `json:"targetArgsDigest,omitempty"`
	TargetSpecDigest  string          `json:"targetSpecDigest,omitempty"`
	TargetArgsPreview json.RawMessage `json:"targetArgsPreview,omitempty"`
	Severity          string          `json:"severity,omitempty"`
	ToolCallID        string          `json:"toolCallID,omitempty"`
	Status            string          `json:"status"`
	CreatedAt         time.Time       `json:"createdAt"`
	ExpiresAt         *time.Time      `json:"expiresAt,omitempty"`
	Timeout           string          `json:"timeout,omitempty"`
	DecisionSeq       int64           `json:"decisionSeq,omitempty"`
	DecisionTime      *time.Time      `json:"decisionTime,omitempty"`
	DecisionReason    string          `json:"decisionReason,omitempty"`
	DecisionActor     string          `json:"decisionActor,omitempty"`
}

// Derive returns current approval state from a task event stream.
func Derive(input []store.ExecutionEvent, now time.Time) []Approval {
	eventsCopy := make([]store.ExecutionEvent, 0, len(input))
	eventsCopy = append(eventsCopy, input...)
	sort.SliceStable(eventsCopy, func(i, j int) bool { return eventsCopy[i].Seq < eventsCopy[j].Seq })

	byID := map[string]*Approval{}
	order := []string{}
	for _, event := range eventsCopy {
		switch event.Type {
		case events.ExecutionEventTypeApprovalRequested:
			payload := approvalPayload(event.Content)
			id := firstNonEmpty(payload.ApprovalID, payload.ID, event.ToolCallID)
			if id == "" {
				id = event.ID
			}
			if _, ok := byID[id]; ok {
				// Duplicate request events are idempotent: once an approval ID exists,
				// later replays must not reset terminal decisions back to pending.
				continue
			}
			order = append(order, id)
			createdAt := event.CreatedAt
			approval := &Approval{
				ID:                id,
				Action:            firstNonEmpty(payload.Action, event.ToolName),
				RiskSummary:       firstNonEmpty(payload.RiskSummary, event.Summary),
				TaskUID:           payload.TaskUID,
				TargetTool:        payload.TargetTool,
				TargetArgsDigest:  payload.TargetArgsDigest,
				TargetSpecDigest:  payload.TargetSpecDigest,
				TargetArgsPreview: payload.TargetArgsPreview,
				Severity:          payload.Severity,
				ToolCallID:        firstNonEmpty(payload.ToolCallID, event.ToolCallID),
				Status:            StatusPending,
				CreatedAt:         createdAt,
				Timeout:           payload.Timeout,
			}
			if payload.ExpiresAt != "" {
				if parsed, err := time.Parse(time.RFC3339, payload.ExpiresAt); err == nil {
					approval.ExpiresAt = &parsed
				}
			}
			byID[id] = approval
		case events.ExecutionEventTypeApprovalApproved, events.ExecutionEventTypeApprovalDeclined, events.ExecutionEventTypeApprovalExpired, events.ExecutionEventTypeApprovalCancelled:
			payload := approvalPayload(event.Content)
			id := firstNonEmpty(payload.ApprovalID, payload.ID, event.ToolCallID)
			if id == "" {
				continue
			}
			approval, ok := byID[id]
			if !ok {
				approval = &Approval{ID: id, Status: StatusPending, CreatedAt: event.CreatedAt}
				byID[id] = approval
				order = append(order, id)
			}
			if approval.Status != StatusPending {
				continue
			}
			status := statusForEvent(event.Type)
			approval.Status = status
			approval.DecisionSeq = event.Seq
			decisionTime := event.CreatedAt
			approval.DecisionTime = &decisionTime
			approval.DecisionReason = firstNonEmpty(payload.Reason, event.Summary)
			approval.DecisionActor = payload.Actor
		}
	}

	if !now.IsZero() {
		for _, approval := range byID {
			if approval.Status == StatusPending && approval.ExpiresAt != nil && !now.Before(*approval.ExpiresAt) {
				approval.Status = StatusExpired
			}
		}
	}

	out := make([]Approval, 0, len(order))
	for _, id := range order {
		approval := *byID[id]
		out = append(out, approval)
	}
	return out
}

func Pending(input []store.ExecutionEvent, now time.Time) []Approval {
	all := Derive(input, now)
	pending := all[:0]
	for _, approval := range all {
		if approval.Status == StatusPending {
			pending = append(pending, approval)
		}
	}
	return pending
}

func statusForEvent(eventType string) string {
	switch eventType {
	case events.ExecutionEventTypeApprovalApproved:
		return StatusApproved
	case events.ExecutionEventTypeApprovalDeclined:
		return StatusDeclined
	case events.ExecutionEventTypeApprovalExpired:
		return StatusExpired
	case events.ExecutionEventTypeApprovalCancelled:
		return StatusCancelled
	default:
		return StatusPending
	}
}

type payload struct {
	ID                string          `json:"id"`
	ApprovalID        string          `json:"approvalID"`
	Action            string          `json:"action"`
	RiskSummary       string          `json:"riskSummary"`
	TaskUID           string          `json:"taskUID"`
	TargetTool        string          `json:"targetTool"`
	TargetArgsDigest  string          `json:"targetArgsDigest"`
	TargetSpecDigest  string          `json:"targetSpecDigest"`
	TargetArgsPreview json.RawMessage `json:"targetArgsPreview"`
	Severity          string          `json:"severity"`
	ToolCallID        string          `json:"toolCallID"`
	Timeout           string          `json:"timeout"`
	ExpiresAt         string          `json:"expiresAt"`
	Reason            string          `json:"reason"`
	Actor             string          `json:"actor"`
}

func approvalPayload(raw json.RawMessage) payload {
	var p payload
	if len(raw) == 0 {
		return p
	}
	_ = json.Unmarshal(raw, &p)
	return p
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// EventTypes returns the approval-related execution event types needed to derive
// current approval state from a task stream.
func EventTypes() []string {
	return []string{
		events.ExecutionEventTypeApprovalRequested,
		events.ExecutionEventTypeApprovalApproved,
		events.ExecutionEventTypeApprovalDeclined,
		events.ExecutionEventTypeApprovalExpired,
		events.ExecutionEventTypeApprovalCancelled,
	}
}

// ListEvents reads all approval-related events for a task stream.
func ListEvents(ctx context.Context, eventStore store.ExecutionEventStore, namespace, taskName string) ([]store.ExecutionEvent, error) {
	if eventStore == nil {
		return nil, nil
	}
	out := []store.ExecutionEvent{}
	after := int64(0)
	for {
		batch, err := eventStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
			Namespace:  namespace,
			StreamType: events.ExecutionEventStreamTypeTask,
			StreamID:   taskName,
			EventTypes: EventTypes(),
			AfterSeq:   after,
			Limit:      store.MaxExecutionEventLimit,
		})
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		out = append(out, batch...)
		after = batch[len(batch)-1].Seq
		if len(batch) < store.MaxExecutionEventLimit {
			break
		}
	}
	return out, nil
}

// Resolved returns terminal approval decisions in a compact worker-facing shape.
func Resolved(values []Approval) []ResolvedApproval {
	out := make([]ResolvedApproval, 0, len(values))
	for _, approval := range values {
		switch approval.Status {
		case StatusApproved, StatusDeclined, StatusExpired, StatusCancelled:
			decisionTime := ""
			if approval.DecisionTime != nil {
				decisionTime = approval.DecisionTime.UTC().Format(time.RFC3339)
			}
			out = append(out, ResolvedApproval{
				ID:                approval.ID,
				TaskUID:           approval.TaskUID,
				TargetTool:        approval.TargetTool,
				TargetArgsDigest:  approval.TargetArgsDigest,
				TargetSpecDigest:  approval.TargetSpecDigest,
				TargetArgsPreview: approval.TargetArgsPreview,
				Status:            approval.Status,
				Actor:             approval.DecisionActor,
				DecisionTime:      decisionTime,
				Reason:            boundApprovalTargetText(approval.DecisionReason),
				Action:            approval.Action,
				RiskSummary:       approval.RiskSummary,
				Severity:          approval.Severity,
			})
		}
	}
	return out
}

// FilterEventsForTaskUID keeps approval events emitted for the current
// immutable Task instance. Untagged events are retained as a migration path for
// in-flight approval streams created before taskUID was added; task finalizer
// cleanup still owns removal of old per-name streams when Tasks are deleted.
func FilterEventsForTaskUID(input []store.ExecutionEvent, taskUID string) []store.ExecutionEvent {
	taskUID = strings.TrimSpace(taskUID)
	if taskUID == "" {
		return input
	}
	out := make([]store.ExecutionEvent, 0, len(input))
	for _, event := range input {
		payload := approvalPayload(event.Content)
		eventTaskUID := strings.TrimSpace(payload.TaskUID)
		if eventTaskUID == "" || eventTaskUID == taskUID {
			out = append(out, event)
		}
	}
	return out
}
