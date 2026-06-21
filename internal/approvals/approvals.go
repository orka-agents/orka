package approvals

import (
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
	ID             string     `json:"id"`
	Action         string     `json:"action"`
	RiskSummary    string     `json:"riskSummary,omitempty"`
	ToolCallID     string     `json:"toolCallID,omitempty"`
	Status         string     `json:"status"`
	CreatedAt      time.Time  `json:"createdAt"`
	ExpiresAt      *time.Time `json:"expiresAt,omitempty"`
	Timeout        string     `json:"timeout,omitempty"`
	DecisionSeq    int64      `json:"decisionSeq,omitempty"`
	DecisionTime   *time.Time `json:"decisionTime,omitempty"`
	DecisionReason string     `json:"decisionReason,omitempty"`
	DecisionActor  string     `json:"decisionActor,omitempty"`
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
				ID:          id,
				Action:      firstNonEmpty(payload.Action, event.ToolName),
				RiskSummary: firstNonEmpty(payload.RiskSummary, event.Summary),
				ToolCallID:  firstNonEmpty(payload.ToolCallID, event.ToolCallID),
				Status:      StatusPending,
				CreatedAt:   createdAt,
				Timeout:     payload.Timeout,
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
	ID          string `json:"id"`
	ApprovalID  string `json:"approvalID"`
	Action      string `json:"action"`
	RiskSummary string `json:"riskSummary"`
	ToolCallID  string `json:"toolCallID"`
	Timeout     string `json:"timeout"`
	ExpiresAt   string `json:"expiresAt"`
	Reason      string `json:"reason"`
	Actor       string `json:"actor"`
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
