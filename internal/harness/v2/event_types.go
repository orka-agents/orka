package v2

import (
	"encoding/json"
	"fmt"
	"time"
)

type EventType string

const (
	EventAccepted            EventType = "accepted"
	EventUpdate              EventType = "update"
	EventPermissionRequested EventType = "permission_requested"
	EventCompleted           EventType = "completed"
	EventCancelled           EventType = "cancelled"
	EventFailed              EventType = "failed"
	EventOutcomeUnknown      EventType = "outcome_unknown"
)

func (t EventType) IsTerminal() bool {
	switch t {
	case EventCompleted, EventCancelled, EventFailed, EventOutcomeUnknown:
		return true
	default:
		return false
	}
}

type EventIdentity struct {
	RuntimeInstanceID        RuntimeInstanceID `json:"runtimeInstanceID"`
	SupervisorBootID         SupervisorBootID  `json:"supervisorBootID"`
	RuntimeSessionUID        RuntimeSessionUID `json:"runtimeSessionUID"`
	RuntimeSessionGeneration uint64            `json:"runtimeSessionGeneration"`
	TaskUID                  TaskUID           `json:"taskUID"`
	TaskAttempt              uint32            `json:"taskAttempt"`
	PromptID                 PromptID          `json:"promptID"`
	Sequence                 uint64            `json:"sequence"`
	RequestDigest            RequestDigest     `json:"requestDigest"`
	Timestamp                time.Time         `json:"timestamp"`
}

func (i EventIdentity) Validate() error {
	if err := requireIdentifier("runtime instance ID", string(i.RuntimeInstanceID)); err != nil {
		return err
	}
	if err := requireIdentifier("supervisor boot ID", string(i.SupervisorBootID)); err != nil {
		return err
	}
	if err := requireIdentifier("runtime session UID", string(i.RuntimeSessionUID)); err != nil {
		return err
	}
	if i.RuntimeSessionGeneration == 0 {
		return fmt.Errorf("runtime session generation must be positive")
	}
	if err := requireIdentifier("task UID", string(i.TaskUID)); err != nil {
		return err
	}
	if i.TaskAttempt == 0 {
		return fmt.Errorf("task attempt must be positive")
	}
	if err := requireIdentifier("prompt ID", string(i.PromptID)); err != nil {
		return err
	}
	if i.Sequence == 0 {
		return fmt.Errorf("event sequence must be positive")
	}
	if err := ValidateRequestDigest(i.RequestDigest); err != nil {
		return fmt.Errorf("request digest: %w", err)
	}
	return validateTimestamp("event timestamp", i.Timestamp)
}

type EventExpectation struct {
	RuntimeInstanceID        RuntimeInstanceID
	SupervisorBootID         SupervisorBootID
	RuntimeSessionUID        RuntimeSessionUID
	RuntimeSessionGeneration uint64
	TaskUID                  TaskUID
	TaskAttempt              uint32
	PromptID                 PromptID
	RequestDigest            RequestDigest
}

func EventExpectationFromMetadata(metadata MutationMetadata) EventExpectation {
	return EventExpectation{
		RuntimeInstanceID:        metadata.Fence.RuntimeInstanceID,
		SupervisorBootID:         metadata.Fence.SupervisorBootID,
		RuntimeSessionUID:        metadata.Fence.RuntimeSessionUID,
		RuntimeSessionGeneration: metadata.Fence.RuntimeSessionGeneration,
		TaskUID:                  metadata.TaskUID,
		TaskAttempt:              metadata.TaskAttempt,
		PromptID:                 metadata.PromptID,
		RequestDigest:            metadata.RequestDigest,
	}
}

func (e EventExpectation) Validate() error {
	identity := EventIdentity{
		RuntimeInstanceID:        e.RuntimeInstanceID,
		SupervisorBootID:         e.SupervisorBootID,
		RuntimeSessionUID:        e.RuntimeSessionUID,
		RuntimeSessionGeneration: e.RuntimeSessionGeneration,
		TaskUID:                  e.TaskUID,
		TaskAttempt:              e.TaskAttempt,
		PromptID:                 e.PromptID,
		Sequence:                 1,
		RequestDigest:            e.RequestDigest,
		Timestamp:                time.Unix(1, 0).UTC(),
	}
	return identity.Validate()
}

func (e EventExpectation) Matches(identity EventIdentity) error {
	switch {
	case e.RuntimeInstanceID != identity.RuntimeInstanceID:
		return fmt.Errorf("event runtime instance ID %q does not match expected %q", identity.RuntimeInstanceID, e.RuntimeInstanceID)
	case e.SupervisorBootID != identity.SupervisorBootID:
		return fmt.Errorf("event supervisor boot ID %q does not match expected %q", identity.SupervisorBootID, e.SupervisorBootID)
	case e.RuntimeSessionUID != identity.RuntimeSessionUID:
		return fmt.Errorf("event runtime session UID %q does not match expected %q", identity.RuntimeSessionUID, e.RuntimeSessionUID)
	case e.RuntimeSessionGeneration != identity.RuntimeSessionGeneration:
		return fmt.Errorf("event runtime session generation %d does not match expected %d", identity.RuntimeSessionGeneration, e.RuntimeSessionGeneration)
	case e.TaskUID != identity.TaskUID || e.TaskAttempt != identity.TaskAttempt:
		return fmt.Errorf("event task identity does not match expectation")
	case e.PromptID != identity.PromptID:
		return fmt.Errorf("event prompt ID %q does not match expected %q", identity.PromptID, e.PromptID)
	case e.RequestDigest != identity.RequestDigest:
		return fmt.Errorf("event request digest %q does not match expected %q", identity.RequestDigest, e.RequestDigest)
	default:
		return nil
	}
}

type AcceptedEvent struct {
	AcceptedAt time.Time   `json:"acceptedAt"`
	Lease      PromptLease `json:"lease"`
	ACPVersion string      `json:"acpVersion"`
}

func (e AcceptedEvent) Validate() error {
	if err := validateTimestamp("prompt acceptance timestamp", e.AcceptedAt); err != nil {
		return err
	}
	if e.ACPVersion != ACPProfileV1 {
		return fmt.Errorf("accepted event ACP version %q is unsupported", e.ACPVersion)
	}
	return e.Lease.ValidateAt(e.AcceptedAt, 0, 0)
}

type UpdateKind string

const (
	UpdateAssistantMessageChunk UpdateKind = "assistant_message_chunk"
	UpdateToolCall              UpdateKind = "tool_call"
	UpdateToolCallUpdate        UpdateKind = "tool_call_update"
	UpdatePlan                  UpdateKind = "plan"
	UpdateUsage                 UpdateKind = "usage"
	UpdateDiagnostic            UpdateKind = "diagnostic"
)

type AssistantMessageChunk struct {
	Text string `json:"text"`
}

type ToolCallStatus string

const (
	ToolCallStatusPending    ToolCallStatus = "pending"
	ToolCallStatusInProgress ToolCallStatus = "in_progress"
	ToolCallStatusCompleted  ToolCallStatus = "completed"
	ToolCallStatusFailed     ToolCallStatus = "failed"
)

type ToolCallUpdate struct {
	ToolCallID     string         `json:"toolCallID"`
	Title          string         `json:"title,omitempty"`
	Kind           string         `json:"kind,omitempty"`
	Status         ToolCallStatus `json:"status"`
	Content        []ContentBlock `json:"content,omitempty"`
	ContentReplace bool           `json:"contentReplace,omitempty"`
}

func (u ToolCallUpdate) Validate() error {
	if err := requireIdentifier("tool call ID", u.ToolCallID); err != nil {
		return err
	}
	if err := validateBoundedString("tool call title", u.Title, false, 1024); err != nil {
		return err
	}
	if err := validateBoundedString("tool call kind", u.Kind, false, 128); err != nil {
		return err
	}
	switch u.Status {
	case ToolCallStatusPending, ToolCallStatusInProgress, ToolCallStatusCompleted, ToolCallStatusFailed:
	default:
		return fmt.Errorf("unsupported tool call status %q", u.Status)
	}
	if len(u.Content) > MaxContentBlocks {
		return fmt.Errorf("tool call content block count exceeds %d", MaxContentBlocks)
	}
	for i := range u.Content {
		if err := u.Content[i].ValidateToolOutput(); err != nil {
			return fmt.Errorf("tool call content block %d: %w", i, err)
		}
	}
	return nil
}

type PlanEntryStatus string

const (
	PlanEntryPending    PlanEntryStatus = "pending"
	PlanEntryInProgress PlanEntryStatus = "in_progress"
	PlanEntryCompleted  PlanEntryStatus = "completed"
)

type PlanEntry struct {
	Content  string          `json:"content"`
	Priority string          `json:"priority,omitempty"`
	Status   PlanEntryStatus `json:"status"`
}

type PlanUpdate struct {
	Entries []PlanEntry `json:"entries"`
}

func (u PlanUpdate) Validate() error {
	if len(u.Entries) == 0 || len(u.Entries) > 128 {
		return fmt.Errorf("plan entry count must be in range 1..128")
	}
	for i, entry := range u.Entries {
		if err := validateBoundedString("plan entry content", entry.Content, true, MaxProtocolStringBytes); err != nil {
			return fmt.Errorf("plan entry %d: %w", i, err)
		}
		if err := validateBoundedString("plan entry priority", entry.Priority, false, 64); err != nil {
			return fmt.Errorf("plan entry %d: %w", i, err)
		}
		switch entry.Status {
		case PlanEntryPending, PlanEntryInProgress, PlanEntryCompleted:
		default:
			return fmt.Errorf("plan entry %d has unsupported status %q", i, entry.Status)
		}
	}
	return nil
}

type UsageUpdate struct {
	InputTokens       uint64  `json:"inputTokens,omitempty"`
	OutputTokens      uint64  `json:"outputTokens,omitempty"`
	CachedInputTokens uint64  `json:"cachedInputTokens,omitempty"`
	ContextWindowUsed *uint64 `json:"contextWindowUsed,omitempty"`
	ContextWindowSize *uint64 `json:"contextWindowSize,omitempty"`
}

func (u UsageUpdate) Validate() error {
	if (u.ContextWindowUsed == nil) != (u.ContextWindowSize == nil) {
		return fmt.Errorf("context window usage requires both used and size")
	}
	if u.ContextWindowUsed != nil && *u.ContextWindowUsed > *u.ContextWindowSize {
		return fmt.Errorf("context window used tokens must not exceed size")
	}
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.CachedInputTokens == 0 && u.ContextWindowUsed == nil {
		return fmt.Errorf("usage update must carry token or context-window telemetry")
	}
	return nil
}

type DiagnosticUpdate struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (u DiagnosticUpdate) Validate() error {
	if err := validateBoundedString("diagnostic code", u.Code, true, 128); err != nil {
		return err
	}
	return validateBoundedString("diagnostic message", u.Message, true, MaxDiagnosticBytes)
}

type UpdateEvent struct {
	Kind             UpdateKind             `json:"kind"`
	AssistantMessage *AssistantMessageChunk `json:"assistantMessage,omitempty"`
	ToolCall         *ToolCallUpdate        `json:"toolCall,omitempty"`
	Plan             *PlanUpdate            `json:"plan,omitempty"`
	Usage            *UsageUpdate           `json:"usage,omitempty"`
	Diagnostic       *DiagnosticUpdate      `json:"diagnostic,omitempty"`
}

func (e UpdateEvent) Validate() error {
	if !exactlyOne(e.AssistantMessage != nil, e.ToolCall != nil, e.Plan != nil, e.Usage != nil, e.Diagnostic != nil) {
		return fmt.Errorf("update event must carry exactly one typed payload")
	}
	switch e.Kind {
	case UpdateAssistantMessageChunk:
		if e.AssistantMessage == nil {
			return fmt.Errorf("assistant message update requires assistantMessage payload")
		}
		if e.AssistantMessage.Text == "" {
			return fmt.Errorf("assistant message chunk is required")
		}
		return validateBoundedString("assistant message chunk", e.AssistantMessage.Text, false, MaxProtocolStringBytes)
	case UpdateToolCall, UpdateToolCallUpdate:
		if e.ToolCall == nil {
			return fmt.Errorf("tool call update requires toolCall payload")
		}
		return e.ToolCall.Validate()
	case UpdatePlan:
		if e.Plan == nil {
			return fmt.Errorf("plan update requires plan payload")
		}
		return e.Plan.Validate()
	case UpdateUsage:
		if e.Usage == nil {
			return fmt.Errorf("usage update requires usage payload")
		}
		return e.Usage.Validate()
	case UpdateDiagnostic:
		if e.Diagnostic == nil {
			return fmt.Errorf("diagnostic update requires diagnostic payload")
		}
		return e.Diagnostic.Validate()
	default:
		return fmt.Errorf("unsupported update kind %q", e.Kind)
	}
}

type PermissionRequestedEvent struct {
	RequestID  PermissionRequestID `json:"requestID"`
	ToolCallID string              `json:"toolCallID,omitempty"`
	ToolName   string              `json:"toolName,omitempty"`
	Title      string              `json:"title"`
	Options    []PermissionOption  `json:"options"`
	ExpiresAt  time.Time           `json:"expiresAt"`
}

func (e PermissionRequestedEvent) Validate(eventTime time.Time) error {
	if err := requireIdentifier("permission request ID", string(e.RequestID)); err != nil {
		return err
	}
	if e.ToolCallID != "" {
		if err := requireIdentifier("permission tool call ID", e.ToolCallID); err != nil {
			return err
		}
	}
	if e.ToolName != "" {
		if err := requireIdentifier("permission tool name", e.ToolName); err != nil {
			return err
		}
	}
	if err := validateBoundedString("permission title", e.Title, true, 1024); err != nil {
		return err
	}
	if len(e.Options) == 0 || len(e.Options) > 16 {
		return fmt.Errorf("permission option count must be in range 1..16")
	}
	seen := make(map[string]struct{}, len(e.Options))
	for i := range e.Options {
		if err := e.Options[i].Validate(); err != nil {
			return fmt.Errorf("permission option %d: %w", i, err)
		}
		if _, ok := seen[e.Options[i].OptionID]; ok {
			return fmt.Errorf("duplicate permission option ID %q", e.Options[i].OptionID)
		}
		seen[e.Options[i].OptionID] = struct{}{}
	}
	if err := validateTimestamp("permission expiry", e.ExpiresAt); err != nil {
		return err
	}
	if !e.ExpiresAt.After(eventTime) {
		return fmt.Errorf("permission expiry must follow event timestamp")
	}
	return nil
}

type PromptResult struct {
	Content []ContentBlock `json:"content"`
	Model   string         `json:"model,omitempty"`
	Usage   UsageUpdate    `json:"usage,omitempty"`
}

func (r PromptResult) Validate() error {
	if len(r.Content) == 0 || len(r.Content) > MaxContentBlocks {
		return fmt.Errorf("prompt result content block count must be in range 1..%d", MaxContentBlocks)
	}
	for i := range r.Content {
		if err := r.Content[i].Validate(); err != nil {
			return fmt.Errorf("prompt result content block %d: %w", i, err)
		}
	}
	return validateBoundedString("result model", r.Model, false, 256)
}

type CompletedEvent struct {
	StopReason ACPStopReason `json:"stopReason"`
	Result     PromptResult  `json:"result"`
}

func (e CompletedEvent) Validate() error {
	if e.StopReason != ACPStopReasonEndTurn {
		return fmt.Errorf("completed event requires end_turn stop reason, got %q", e.StopReason)
	}
	return e.Result.Validate()
}

type CancelledEvent struct {
	StopReason ACPStopReason `json:"stopReason"`
	Reason     string        `json:"reason,omitempty"`
}

func (e CancelledEvent) Validate() error {
	if e.StopReason != ACPStopReasonCancelled {
		return fmt.Errorf("cancelled event requires cancelled stop reason, got %q", e.StopReason)
	}
	return validateBoundedString("cancellation reason", e.Reason, false, MaxDiagnosticBytes)
}

type FailedEvent struct {
	StopReason ACPStopReason `json:"stopReason,omitempty"`
	Code       string        `json:"code"`
	Message    string        `json:"message"`
	Retryable  bool          `json:"retryable"`
}

func (e FailedEvent) Validate() error {
	if e.StopReason == ACPStopReasonEndTurn || e.StopReason == ACPStopReasonCancelled {
		return fmt.Errorf("failed event has non-failure stop reason %q", e.StopReason)
	}
	if err := validateBoundedString("failure code", e.Code, true, 128); err != nil {
		return err
	}
	if err := validateBoundedString("failure message", e.Message, true, MaxDiagnosticBytes); err != nil {
		return err
	}
	if e.Retryable {
		return fmt.Errorf("accepted prompt failure must not advertise automatic replay")
	}
	return nil
}

type OutcomeUnknownEvent struct {
	Code              string `json:"code"`
	Message           string `json:"message"`
	ForcedTermination bool   `json:"forcedTermination,omitempty"`
	Retryable         bool   `json:"retryable"`
}

func (e OutcomeUnknownEvent) Validate() error {
	if err := validateBoundedString("outcome unknown code", e.Code, true, 128); err != nil {
		return err
	}
	if err := validateBoundedString("outcome unknown message", e.Message, true, MaxDiagnosticBytes); err != nil {
		return err
	}
	if e.Retryable {
		return fmt.Errorf("outcome_unknown must never be retryable")
	}
	return nil
}

type Event struct {
	Protocol            string                    `json:"protocol"`
	Type                EventType                 `json:"type"`
	Identity            EventIdentity             `json:"identity"`
	Accepted            *AcceptedEvent            `json:"accepted,omitempty"`
	Update              *UpdateEvent              `json:"update,omitempty"`
	PermissionRequested *PermissionRequestedEvent `json:"permissionRequested,omitempty"`
	Completed           *CompletedEvent           `json:"completed,omitempty"`
	Cancelled           *CancelledEvent           `json:"cancelled,omitempty"`
	Failed              *FailedEvent              `json:"failed,omitempty"`
	OutcomeUnknown      *OutcomeUnknownEvent      `json:"outcomeUnknown,omitempty"`
}

func (e Event) Validate(limits EventStreamLimits) error {
	if err := limits.Validate(); err != nil {
		return fmt.Errorf("event limits: %w", err)
	}
	if err := validateProtocol(e.Protocol); err != nil {
		return err
	}
	if err := e.Identity.Validate(); err != nil {
		return fmt.Errorf("event identity: %w", err)
	}
	if !exactlyOne(e.Accepted != nil, e.Update != nil, e.PermissionRequested != nil,
		e.Completed != nil, e.Cancelled != nil, e.Failed != nil, e.OutcomeUnknown != nil) {
		return fmt.Errorf("event must carry exactly one typed payload")
	}
	var err error
	switch e.Type {
	case EventAccepted:
		if e.Accepted == nil {
			return fmt.Errorf("accepted event requires accepted payload")
		}
		err = e.Accepted.Validate()
	case EventUpdate:
		if e.Update == nil {
			return fmt.Errorf("update event requires update payload")
		}
		err = e.Update.Validate()
	case EventPermissionRequested:
		if e.PermissionRequested == nil {
			return fmt.Errorf("permission_requested event requires permissionRequested payload")
		}
		err = e.PermissionRequested.Validate(e.Identity.Timestamp)
	case EventCompleted:
		if e.Completed == nil {
			return fmt.Errorf("completed event requires completed payload")
		}
		err = e.Completed.Validate()
	case EventCancelled:
		if e.Cancelled == nil {
			return fmt.Errorf("cancelled event requires cancelled payload")
		}
		err = e.Cancelled.Validate()
	case EventFailed:
		if e.Failed == nil {
			return fmt.Errorf("failed event requires failed payload")
		}
		err = e.Failed.Validate()
	case EventOutcomeUnknown:
		if e.OutcomeUnknown == nil {
			return fmt.Errorf("outcome_unknown event requires outcomeUnknown payload")
		}
		err = e.OutcomeUnknown.Validate()
	default:
		return fmt.Errorf("unsupported event type %q", e.Type)
	}
	if err != nil {
		return fmt.Errorf("%s payload: %w", e.Type, err)
	}
	if e.Type.IsTerminal() {
		payload, err := e.terminalPayloadJSON()
		if err != nil {
			return err
		}
		if len(payload) > limits.MaxTerminalResultBytes {
			return fmt.Errorf("terminal payload exceeds %d bytes", limits.MaxTerminalResultBytes)
		}
	}
	return nil
}

func (e Event) terminalPayloadJSON() ([]byte, error) {
	var value any
	switch e.Type {
	case EventCompleted:
		value = e.Completed
	case EventCancelled:
		value = e.Cancelled
	case EventFailed:
		value = e.Failed
	case EventOutcomeUnknown:
		value = e.OutcomeUnknown
	default:
		return nil, fmt.Errorf("event type %q is not terminal", e.Type)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal terminal event payload: %w", err)
	}
	return encoded, nil
}
