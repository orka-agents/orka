package harness

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

// EventMapContext supplies Orka stream ownership for harness frames. Harness
// frames deliberately do not own namespace/task authority; Orka supplies it from
// the Task/RuntimeSession claim.
type EventMapContext struct {
	Namespace   string
	TaskName    string
	SessionName string
	AgentName   string
	StreamID    string
}

func (c EventMapContext) normalized() EventMapContext {
	c.Namespace = strings.TrimSpace(c.Namespace)
	c.TaskName = strings.TrimSpace(c.TaskName)
	c.SessionName = strings.TrimSpace(c.SessionName)
	c.AgentName = strings.TrimSpace(c.AgentName)
	c.StreamID = strings.TrimSpace(c.StreamID)
	if c.StreamID == "" {
		c.StreamID = c.TaskName
	}
	return c
}

func (c EventMapContext) validate() error {
	c = c.normalized()
	if c.Namespace == "" {
		return fmt.Errorf("namespace is required")
	}
	if c.TaskName == "" {
		return fmt.Errorf("task name is required")
	}
	if c.StreamID == "" {
		return fmt.Errorf("stream id is required")
	}
	return nil
}

func MapFrameToExecutionEvent(frame HarnessEventFrame, mapCtx EventMapContext) (*store.ExecutionEvent, error) {
	if err := mapCtx.validate(); err != nil {
		return nil, err
	}
	mapCtx = mapCtx.normalized()
	if err := frame.ValidateRequired(); err != nil {
		return nil, fmt.Errorf("invalid harness frame: %w", err)
	}

	eventType, severity, summary := mapFrameType(frame)
	content, err := buildMappedContent(frame)
	if err != nil {
		return nil, err
	}
	if IsKnownFrameType(frame.Type) && strings.TrimSpace(frame.Summary) != "" {
		summary = strings.TrimSpace(frame.Summary)
	}
	if severity == "" {
		severity = events.NormalizeExecutionEventSeverity(frame.Severity)
	}
	createdAt := frame.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	event := &store.ExecutionEvent{
		Namespace:   mapCtx.Namespace,
		StreamType:  store.ExecutionEventStreamTypeTask,
		StreamID:    mapCtx.StreamID,
		Type:        eventType,
		Severity:    severity,
		TaskName:    mapCtx.TaskName,
		SessionName: mapCtx.SessionName,
		AgentName:   mapCtx.AgentName,
		ToolName:    strings.TrimSpace(frame.ToolName),
		ToolCallID:  strings.TrimSpace(frame.ToolCallID),
		Summary:     summary,
		Content:     content,
		ContentText: frame.ContentText,
		CreatedAt:   createdAt.UTC(),
	}
	if err := store.SanitizeExecutionEventPayloadFields(event); err != nil {
		return nil, fmt.Errorf("sanitize mapped harness event: %w", err)
	}
	return event, nil
}

func mapFrameType(frame HarnessEventFrame) (eventType, severity, summary string) {
	severity = events.NormalizeExecutionEventSeverity(frame.Severity)
	switch frame.Type {
	case FrameTurnStarted:
		return events.ExecutionEventTypeAgentRuntimeStarted, severity, "harness turn started"
	case FrameRuntimeOutput:
		return events.ExecutionEventTypeModelMessage, severity, "runtime output"
	case FrameToolCallRequested:
		return events.ExecutionEventTypeToolCallStarted, severity, "tool call requested"
	case FrameToolResultReceived:
		return events.ExecutionEventTypeToolCallCompleted, severity, "tool result received"
	case FrameApprovalRequested:
		return events.ExecutionEventTypeApprovalRequested, severity, "approval requested"
	case FrameTurnCompleted:
		return events.ExecutionEventTypeAgentRuntimeCompleted, severity, "harness turn completed"
	case FrameTurnFailed:
		return events.ExecutionEventTypeAgentRuntimeFailed, events.ExecutionEventSeverityError, "harness turn failed"
	case FrameTurnCancelled:
		return events.ExecutionEventTypeAgentRuntimeCancelled, events.ExecutionEventSeverityWarning, "harness turn cancelled"
	case FrameRuntimeLog:
		return events.ExecutionEventTypeAgentRuntimeCommandStarted, severity, "runtime log"
	default:
		return events.ExecutionEventTypeAgentRuntimeCommandStarted, events.ExecutionEventSeverityWarning, fmt.Sprintf("unknown harness frame %q", frame.Type)
	}
}

func buildMappedContent(frame HarnessEventFrame) (json.RawMessage, error) {
	content := map[string]any{
		"harness": MappedFrameIdentityFromFrame(frame),
	}
	if len(frame.Metadata) > 0 {
		content["metadata"] = frame.Metadata
	}
	if frame.ApprovalID != "" {
		content["approvalID"] = frame.ApprovalID
	}
	if frame.Completed != nil {
		content["completed"] = frame.Completed
	}
	if frame.Failed != nil {
		content["failed"] = frame.Failed
	}
	if frame.Error != nil {
		content["error"] = frame.Error
	}
	if len(frame.Content) > 0 {
		var decoded any
		if err := json.Unmarshal(frame.Content, &decoded); err != nil {
			return nil, fmt.Errorf("invalid harness frame content JSON: %w", err)
		}
		content["frameContent"] = decoded
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return nil, fmt.Errorf("marshal mapped harness content: %w", err)
	}
	return encoded, nil
}
