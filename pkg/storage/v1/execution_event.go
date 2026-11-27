package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	DefaultExecutionEventLimit = 100
	MaxExecutionEventLimit     = 1000
)

type ExecutionEvent struct {
	ID          string                    `json:"id"`
	Namespace   string                    `json:"namespace"`
	StreamType  string                    `json:"streamType"`
	StreamID    string                    `json:"streamID"`
	Seq         int64                     `json:"seq"`
	Type        string                    `json:"type"`
	Severity    string                    `json:"severity"`
	TaskName    string                    `json:"taskName,omitempty"`
	SessionName string                    `json:"sessionName,omitempty"`
	AgentName   string                    `json:"agentName,omitempty"`
	ToolName    string                    `json:"toolName,omitempty"`
	ToolCallID  string                    `json:"toolCallID,omitempty"`
	Summary     string                    `json:"summary,omitempty"`
	Content     json.RawMessage           `json:"content,omitempty"`
	ContentText string                    `json:"contentText,omitempty"`
	Truncation  *ExecutionEventTruncation `json:"truncation,omitempty"`
	CreatedAt   time.Time                 `json:"createdAt"`

	Usage *ExecutionEventUsage `json:"-"`
}

type ExecutionEventUsage struct {
	Observation         UsageObservation
	KeepFirstObservedAt bool
}

type ExecutionEventFilter struct {
	Namespace   string
	StreamType  string
	StreamID    string
	TaskName    string
	SessionName string
	EventTypes  []string
	AfterSeq    int64
	Limit       int
}

type SessionExecutionEventFilter struct {
	Namespace   string
	SessionName string
	EventTypes  []string
	AfterSeq    int64
	Limit       int
}

type SessionExecutionEvent struct {
	ExecutionEvent
	SessionSeq int64
	TaskSeq    int64
}

func (f ExecutionEventFilter) Normalized() ExecutionEventFilter {
	f.Namespace = strings.TrimSpace(f.Namespace)
	f.StreamType = strings.TrimSpace(f.StreamType)
	if f.StreamType == "" {
		f.StreamType = ExecutionEventStreamTypeTask
	}
	f.StreamID = strings.TrimSpace(f.StreamID)
	f.TaskName = strings.TrimSpace(f.TaskName)
	f.SessionName = strings.TrimSpace(f.SessionName)
	if f.Limit <= 0 {
		f.Limit = DefaultExecutionEventLimit
	} else if f.Limit > MaxExecutionEventLimit {
		f.Limit = MaxExecutionEventLimit
	}
	if f.AfterSeq < 0 {
		f.AfterSeq = 0
	}
	if len(f.EventTypes) > 0 {
		types := make([]string, 0, len(f.EventTypes))
		for _, typ := range f.EventTypes {
			if typ = strings.TrimSpace(typ); typ != "" {
				types = append(types, typ)
			}
		}
		f.EventTypes = types
	}
	return f
}

func (f ExecutionEventFilter) Validate() error {
	f = f.Normalized()
	if !IsValidExecutionEventStreamType(f.StreamType) {
		return ValidationErrorf("unsupported execution event stream type %q", f.StreamType)
	}
	for _, typ := range f.EventTypes {
		if !IsValidExecutionEventType(typ) {
			return ValidationErrorf("unsupported execution event type %q", typ)
		}
	}
	return nil
}

func (f SessionExecutionEventFilter) Normalized() SessionExecutionEventFilter {
	f.Namespace = strings.TrimSpace(f.Namespace)
	f.SessionName = strings.TrimSpace(f.SessionName)
	if f.Limit <= 0 {
		f.Limit = DefaultExecutionEventLimit
	} else if f.Limit > MaxExecutionEventLimit {
		f.Limit = MaxExecutionEventLimit
	}
	if f.AfterSeq < 0 {
		f.AfterSeq = 0
	}
	if len(f.EventTypes) > 0 {
		types := make([]string, 0, len(f.EventTypes))
		for _, typ := range f.EventTypes {
			if typ = strings.TrimSpace(typ); typ != "" {
				types = append(types, typ)
			}
		}
		f.EventTypes = types
	}
	return f
}

func (f SessionExecutionEventFilter) Validate() error {
	f = f.Normalized()
	if f.Namespace == "" {
		return ValidationErrorf("session event namespace is required")
	}
	if f.SessionName == "" {
		return ValidationErrorf("session name is required")
	}
	for _, typ := range f.EventTypes {
		if !IsValidExecutionEventType(typ) {
			return ValidationErrorf("unsupported execution event type %q", typ)
		}
	}
	return nil
}

type ExecutionEventStore interface {
	AppendExecutionEvent(ctx context.Context, event *ExecutionEvent) (*ExecutionEvent, error)
	ListExecutionEvents(ctx context.Context, filter ExecutionEventFilter) ([]ExecutionEvent, error)
	ListSessionExecutionEvents(ctx context.Context, filter SessionExecutionEventFilter) ([]SessionExecutionEvent, int64, error)
	GetLatestExecutionEventSeq(ctx context.Context, namespace, streamType, streamID string) (int64, error)
	GetLatestExecutionEventSeqs(ctx context.Context, namespace, streamType string, streamIDs []string) (map[string]int64, error)
	DeleteExecutionEvents(ctx context.Context, namespace, streamType, streamID string) error
}

type ExecutionEventAppend struct {
	Event     *ExecutionEvent
	DedupeKey string
}

type ExecutionEventAppendResult struct {
	Event    *ExecutionEvent
	Appended bool
}

type GroupedExecutionEventStore interface {
	AppendExecutionEventsIfAbsent(ctx context.Context, appends []ExecutionEventAppend) ([]ExecutionEventAppendResult, error)
}

type DeduplicatingExecutionEventStore interface {
	ExecutionEventStore
	AppendExecutionEventIfAbsent(
		ctx context.Context,
		event *ExecutionEvent,
		dedupeKey string,
	) (persisted *ExecutionEvent, appended bool, err error)
}

type AtomicExecutionEventPlanStore interface {
	DeduplicatingExecutionEventStore
	AppendExecutionEventWithPlanIfAbsent(
		ctx context.Context,
		event *ExecutionEvent,
		dedupeKey string,
		plan *PlanState,
	) (persisted *ExecutionEvent, appended bool, err error)
}

const (
	MaxExecutionEventDedupeKeyBytes = 512
)

func IsTerminalApprovalExecutionEventType(value string) bool {
	switch value {
	case ExecutionEventTypeApprovalApproved,
		ExecutionEventTypeApprovalDeclined,
		ExecutionEventTypeApprovalExpired,
		ExecutionEventTypeApprovalCancelled:
		return true
	default:
		return false
	}
}

func ApprovalIDFromExecutionEvent(event ExecutionEvent) string {
	toolCallID := strings.TrimSpace(event.ToolCallID)
	if len(event.Content) == 0 {
		return toolCallID
	}
	var content map[string]any
	if err := json.Unmarshal(event.Content, &content); err != nil {
		return toolCallID
	}
	for _, key := range []string{"approvalID", "approvalId", "approval_id", "id"} {
		if value, ok := content[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return toolCallID
}

func TerminalApprovalConflict(existingType, approvalID string) error {
	return fmt.Errorf("%w: approval %q already has terminal event %s", ErrConflict, approvalID, existingType)
}

func NormalizeExecutionEventDedupeKey(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ValidationErrorf("execution event dedupe key is required")
	}
	if len(value) > MaxExecutionEventDedupeKeyBytes {
		return "", ValidationErrorf("execution event dedupe key exceeds %d bytes", MaxExecutionEventDedupeKeyBytes)
	}
	return value, nil
}
