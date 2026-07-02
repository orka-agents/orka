/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

// SubmitExecutionEventRequest is the internal worker submission DTO.
// Unknown JSON fields are ignored by Go's standard decoder; route handlers can opt into
// strict decoding later by using json.Decoder.DisallowUnknownFields.
type SubmitExecutionEventRequest struct {
	Type        string                           `json:"type"`
	Severity    string                           `json:"severity,omitempty"`
	TaskName    string                           `json:"taskName,omitempty"`
	SessionName string                           `json:"sessionName,omitempty"`
	AgentName   string                           `json:"agentName,omitempty"`
	ToolName    string                           `json:"toolName,omitempty"`
	ToolCallID  string                           `json:"toolCallID,omitempty"`
	Summary     string                           `json:"summary,omitempty"`
	Content     json.RawMessage                  `json:"content,omitempty"`
	ContentText string                           `json:"contentText,omitempty"`
	Truncation  *events.ExecutionEventTruncation `json:"truncation,omitempty"`
}

// ToStoreEvent converts a submission DTO to the store-facing event contract.
func (r SubmitExecutionEventRequest) ToStoreEvent(namespace, streamType, streamID string) (*store.ExecutionEvent, error) {
	typ := events.NormalizeExecutionEventType(r.Type)
	if typ == "" {
		return nil, fmt.Errorf("unsupported execution event type %q", r.Type)
	}
	payload, err := events.SanitizeExecutionEventPayload(r.Summary, r.Content, r.ContentText)
	if err != nil {
		return nil, err
	}
	return &store.ExecutionEvent{
		Namespace:   namespace,
		StreamType:  streamType,
		StreamID:    streamID,
		Type:        typ,
		Severity:    events.NormalizeExecutionEventSeverity(r.Severity),
		TaskName:    r.TaskName,
		SessionName: r.SessionName,
		AgentName:   r.AgentName,
		ToolName:    r.ToolName,
		ToolCallID:  r.ToolCallID,
		Summary:     payload.Summary,
		Content:     payload.Content,
		ContentText: payload.ContentText,
		Truncation:  store.MergeExecutionEventTruncation(r.Truncation, payload.Truncation),
	}, nil
}

// ExecutionEventResponse is the public API representation of an execution event.
type ExecutionEventResponse struct {
	ID           string                           `json:"id"`
	Namespace    string                           `json:"namespace"`
	StreamType   string                           `json:"streamType"`
	StreamID     string                           `json:"streamID"`
	Seq          int64                            `json:"seq"`
	Type         string                           `json:"type"`
	Severity     string                           `json:"severity"`
	TaskName     string                           `json:"taskName,omitempty"`
	SessionName  string                           `json:"sessionName,omitempty"`
	AgentName    string                           `json:"agentName,omitempty"`
	ToolName     string                           `json:"toolName,omitempty"`
	ToolCallID   string                           `json:"toolCallID,omitempty"`
	Provider     string                           `json:"provider,omitempty"`
	Model        string                           `json:"model,omitempty"`
	StopReason   string                           `json:"stopReason,omitempty"`
	InputTokens  int                              `json:"inputTokens,omitempty"`
	OutputTokens int                              `json:"outputTokens,omitempty"`
	Summary      string                           `json:"summary,omitempty"`
	Content      json.RawMessage                  `json:"content,omitempty"`
	ContentText  string                           `json:"contentText,omitempty"`
	Truncation   *events.ExecutionEventTruncation `json:"truncation,omitempty"`
	CreatedAt    time.Time                        `json:"createdAt"`
}

// SubmitExecutionEventResponse is returned after an event append succeeds.
type SubmitExecutionEventResponse struct {
	ID        string    `json:"id"`
	Seq       int64     `json:"seq"`
	CreatedAt time.Time `json:"createdAt"`
}

// ListExecutionEventsResponse is the API response for listing one event stream.
type ListExecutionEventsResponse struct {
	Namespace  string                   `json:"namespace"`
	StreamType string                   `json:"streamType"`
	StreamID   string                   `json:"streamID"`
	AfterSeq   int64                    `json:"afterSeq"`
	LatestSeq  int64                    `json:"latestSeq"`
	Events     []ExecutionEventResponse `json:"events"`
}

// SessionExecutionEventResponse is the public representation of a task-derived
// session event. Seq is the session-level cursor; taskSeq is the source task
// stream sequence.
type SessionExecutionEventResponse struct {
	ExecutionEventResponse
	TaskSeq      int64  `json:"taskSeq"`
	TaskStreamID string `json:"taskStreamID"`
}

// ListSessionExecutionEventsResponse is the API response for an aggregated session stream.
type ListSessionExecutionEventsResponse struct {
	Namespace  string                          `json:"namespace"`
	StreamType string                          `json:"streamType"`
	StreamID   string                          `json:"streamID"`
	AfterSeq   int64                           `json:"afterSeq"`
	LatestSeq  int64                           `json:"latestSeq"`
	Events     []SessionExecutionEventResponse `json:"events"`
}

// NewExecutionEventResponse converts a store event to an API DTO and intentionally
// omits store-only fields such as ExecutionEvent.Internal.
func NewExecutionEventResponse(event store.ExecutionEvent) ExecutionEventResponse {
	var provider, model, stopReason string
	var inTok, outTok int
	if executionEventTypeCarriesModelTelemetry(event.Type) {
		provider, model, stopReason, inTok, outTok = executionEventTelemetryFields(event.Type, event.Content)
	}
	return ExecutionEventResponse{
		ID:           event.ID,
		Namespace:    event.Namespace,
		StreamType:   event.StreamType,
		StreamID:     event.StreamID,
		Seq:          event.Seq,
		Type:         event.Type,
		Severity:     events.NormalizeExecutionEventSeverity(event.Severity),
		TaskName:     event.TaskName,
		SessionName:  event.SessionName,
		AgentName:    event.AgentName,
		ToolName:     event.ToolName,
		ToolCallID:   event.ToolCallID,
		Provider:     provider,
		Model:        model,
		StopReason:   stopReason,
		InputTokens:  inTok,
		OutputTokens: outTok,
		Summary:      event.Summary,
		Content:      cloneRawMessage(event.Content),
		ContentText:  event.ContentText,
		Truncation:   cloneExecutionEventTruncation(event.Truncation),
		CreatedAt:    event.CreatedAt,
	}
}

// NewListExecutionEventsResponse builds a list DTO from store events.
func NewListExecutionEventsResponse(namespace, streamType, streamID string, afterSeq, latestSeq int64, storeEvents []store.ExecutionEvent) ListExecutionEventsResponse {
	responses := make([]ExecutionEventResponse, 0, len(storeEvents))
	for _, event := range storeEvents {
		responses = append(responses, NewExecutionEventResponse(event))
	}
	return ListExecutionEventsResponse{
		Namespace:  namespace,
		StreamType: streamType,
		StreamID:   streamID,
		AfterSeq:   afterSeq,
		LatestSeq:  latestSeq,
		Events:     responses,
	}
}

// NewSessionExecutionEventResponse converts an aggregated store event to a session DTO.
func NewSessionExecutionEventResponse(event store.SessionExecutionEvent) SessionExecutionEventResponse {
	response := NewExecutionEventResponse(event.ExecutionEvent)
	response.Seq = event.SessionSeq
	response.StreamType = events.ExecutionEventStreamTypeSession
	response.StreamID = event.SessionName
	return SessionExecutionEventResponse{
		ExecutionEventResponse: response,
		TaskSeq:                event.TaskSeq,
		TaskStreamID:           event.StreamID,
	}
}

// NewListSessionExecutionEventsResponse builds a session timeline DTO.
func NewListSessionExecutionEventsResponse(namespace, sessionName string, afterSeq, latestSeq int64, storeEvents []store.SessionExecutionEvent) ListSessionExecutionEventsResponse {
	responses := make([]SessionExecutionEventResponse, 0, len(storeEvents))
	for _, event := range storeEvents {
		responses = append(responses, NewSessionExecutionEventResponse(event))
	}
	return ListSessionExecutionEventsResponse{
		Namespace:  namespace,
		StreamType: events.ExecutionEventStreamTypeSession,
		StreamID:   sessionName,
		AfterSeq:   afterSeq,
		LatestSeq:  latestSeq,
		Events:     responses,
	}
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func cloneExecutionEventTruncation(value *events.ExecutionEventTruncation) *events.ExecutionEventTruncation {
	if value == nil {
		return nil
	}
	truncationCopy := *value
	return &truncationCopy
}

func executionEventTypeCarriesModelTelemetry(typ string) bool {
	switch typ {
	case events.ExecutionEventTypeModelRequestStarted,
		events.ExecutionEventTypeModelRequestCompleted,
		events.ExecutionEventTypeModelRequestFailed:
		return true
	default:
		return false
	}
}

func executionEventTelemetryFields(typ string, content json.RawMessage) (provider, model, stopReason string, inTok, outTok int) {
	if len(content) == 0 {
		return "", "", "", 0, 0
	}
	var body map[string]any
	if err := json.Unmarshal(content, &body); err != nil {
		return "", "", "", 0, 0
	}
	provider = stringField(body, "provider", "gen_ai.provider.name")
	modelKeys := []string{"model", "gen_ai.request.model", "gen_ai.response.model"}
	if typ == events.ExecutionEventTypeModelRequestCompleted || typ == events.ExecutionEventTypeModelRequestFailed {
		modelKeys = []string{"model", "gen_ai.response.model", "gen_ai.request.model"}
	}
	model = stringField(body, modelKeys...)
	stopReason = stringField(body, "stopReason", "stop_reason", "finishReason", "gen_ai.response.finish_reasons")
	inTok = intField(body, "inputTokens", "input_tokens", "promptTokens", "prompt_tokens", "gen_ai.usage.input_tokens")
	outTok = intField(body, "outputTokens", "output_tokens", "completionTokens", "completion_tokens", "gen_ai.usage.output_tokens")
	return provider, model, stopReason, inTok, outTok
}

func stringField(body map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := body[key]; ok {
			switch typed := value.(type) {
			case string:
				return typed
			case []string:
				for _, item := range typed {
					if item != "" {
						return item
					}
				}
			case []any:
				for _, item := range typed {
					if s, ok := item.(string); ok && s != "" {
						return s
					}
				}
			}
		}
	}
	return ""
}

func intField(body map[string]any, keys ...string) int {
	for _, key := range keys {
		if value, ok := body[key]; ok {
			switch typed := value.(type) {
			case float64:
				return int(typed)
			case int:
				return typed
			case json.Number:
				if i, err := typed.Int64(); err == nil {
					return int(i)
				}
			}
		}
	}
	return 0
}
