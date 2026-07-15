package harness

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	// ProtocolVersion is the frozen MVP harness protocol version. It is intentionally
	// backend-neutral: provider-specific capabilities belong in CapabilitiesResponse.
	ProtocolVersion = "orka.harness.v1"
	HTTPTransport   = "http+sse"
)

const (
	HealthPath       = "/v1/health"
	CapabilitiesPath = "/v1/capabilities"
	TurnsPath        = "/v1/turns"
)

type RuntimeSessionID string
type HarnessTurnID string

type ToolExecutionMode string

const (
	ToolExecutionModeObserved ToolExecutionMode = "observed"
	ToolExecutionModeBrokered ToolExecutionMode = "brokered"
)

type BrokeredToolClass string

const (
	BrokeredToolClassRead         BrokeredToolClass = "read"
	BrokeredToolClassWrite        BrokeredToolClass = "write"
	BrokeredToolClassCoordination BrokeredToolClass = "coordination"
)

type FrameType string

const (
	FrameTurnStarted        FrameType = "TurnStarted"
	FrameRuntimeOutput      FrameType = "RuntimeOutput"
	FrameToolCallRequested  FrameType = "ToolCallRequested"
	FrameToolResultReceived FrameType = "ToolResultReceived"
	FrameApprovalRequested  FrameType = "ApprovalRequested"
	FrameTurnCompleted      FrameType = "TurnCompleted"
	FrameTurnFailed         FrameType = "TurnFailed"
	FrameTurnCancelled      FrameType = "TurnCancelled"
	FrameRuntimeLog         FrameType = "RuntimeLog"
)

type ProviderKind string

const (
	ProviderKindKubernetesService ProviderKind = "kubernetes-service"
	ProviderKindSidecar           ProviderKind = "sidecar"
	ProviderKindAgentSandbox      ProviderKind = "agent-sandbox"
	ProviderKindSubstrate         ProviderKind = "substrate"
	ProviderKindRemote            ProviderKind = "remote"
)

type HealthStatus string

const (
	HealthStatusOK        HealthStatus = "ok"
	HealthStatusDegraded  HealthStatus = "degraded"
	HealthStatusUnhealthy HealthStatus = "unhealthy"
)

// StartTurnRequest is the Orka-to-harness request that starts one explicit turn.
// Identity and policy fields are safe references or verified metadata. Resolved
// literal credentials destined for the runtime subprocess ARE permitted in
// Input.Env (see TurnEnvVar): this request is the controller-to-wrapper delivery
// channel for credentials and is the architectural equivalent of mounting a
// Secret into the wrapper pod. The prohibition on raw secrets/TxTokens applies to
// OBSERVABLE and DURABLE surfaces only — Task status, persisted annotations,
// execution events/frames, logs, and trace output — never to this in-memory
// request body. Raw TxTokens remain disallowed entirely (use owner-referenced
// child Secrets and fail-closed TTS exchanges). Callers MUST NOT log this request
// or persist Input.Env, and the wrapper should drop Input.Env from retained turn
// state once child env is materialized. Transport confidentiality (TLS) for this
// channel is a deployment-posture concern tracked separately.
type StartTurnRequest struct {
	Version           string            `json:"version"`
	Namespace         string            `json:"namespace"`
	TaskName          string            `json:"taskName"`
	SessionName       string            `json:"sessionName"`
	RuntimeSessionID  RuntimeSessionID  `json:"runtimeSessionID"`
	TurnID            HarnessTurnID     `json:"turnID"`
	CorrelationID     string            `json:"correlationID"`
	Deadline          time.Time         `json:"deadline"`
	AuthIdentity      AuthIdentity      `json:"authIdentity"`
	ToolPolicyRef     *PolicyRef        `json:"toolPolicyRef,omitempty"`
	ApprovalPolicyRef *PolicyRef        `json:"approvalPolicyRef,omitempty"`
	EventCursor       int64             `json:"eventCursor,omitempty"`
	Input             TurnInput         `json:"input,omitempty"`
	ToolExecutionMode ToolExecutionMode `json:"toolExecutionMode,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
}

type AuthIdentity struct {
	Subject  string   `json:"subject,omitempty"`
	Issuer   string   `json:"issuer,omitempty"`
	Username string   `json:"username,omitempty"`
	Groups   []string `json:"groups,omitempty"`
	Roles    []string `json:"roles,omitempty"`
}

type PolicyRef struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

type TurnInput struct {
	Prompt      string       `json:"prompt,omitempty"`
	ContextRefs []ContextRef `json:"contextRefs,omitempty"`
	Env         []TurnEnvVar `json:"env,omitempty"`
	// Tools carries safe Orka-governed tool schemas that the remote runtime may request
	// through brokered governance. It intentionally omits downstream execution URLs,
	// auth references, headers, and credentials; Orka remains the only executor.
	Tools []ToolDefinition `json:"tools,omitempty"`
}

// ToolDefinition is the safe schema view exposed to brokered remote runtimes.
// It is a declaration of what may be requested, not execution authority.
type ToolDefinition struct {
	Name          string            `json:"name"`
	Description   string            `json:"description,omitempty"`
	BrokeredClass BrokeredToolClass `json:"brokeredClass,omitempty"`
	Parameters    json.RawMessage   `json:"parameters,omitempty"`
}

// TurnEnvVar is a resolved, literal environment variable passed to the
// wrapper subprocess. It intentionally does not model SecretKeyRef/ValueFrom;
// controller-side validation must reject UNRESOLVED references (SecretKeyRef/
// ValueFrom) and raw TxTokens. Resolved literal credential values (e.g. a
// provider API key or git token already read from a Secret by the controller)
// ARE permitted here — this is the credential delivery channel — but must never
// be logged, persisted, or surfaced in events/status/trace.
type TurnEnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
}

type ContextRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Seq  int64  `json:"seq,omitempty"`
}

type StartTurnResponse struct {
	Version          string           `json:"version"`
	Accepted         bool             `json:"accepted"`
	RuntimeSessionID RuntimeSessionID `json:"runtimeSessionID"`
	TurnID           HarnessTurnID    `json:"turnID"`
	CorrelationID    string           `json:"correlationID,omitempty"`
	EventStreamPath  string           `json:"eventStreamPath,omitempty"`
}

type CancelTurnRequest struct {
	Version          string           `json:"version"`
	Namespace        string           `json:"namespace"`
	TaskName         string           `json:"taskName"`
	SessionName      string           `json:"sessionName"`
	RuntimeSessionID RuntimeSessionID `json:"runtimeSessionID"`
	TurnID           HarnessTurnID    `json:"turnID"`
	CorrelationID    string           `json:"correlationID"`
	Reason           string           `json:"reason,omitempty"`
}

type CancelTurnResponse struct {
	Version          string           `json:"version"`
	Accepted         bool             `json:"accepted"`
	RuntimeSessionID RuntimeSessionID `json:"runtimeSessionID"`
	TurnID           HarnessTurnID    `json:"turnID"`
	CorrelationID    string           `json:"correlationID,omitempty"`
	Message          string           `json:"message,omitempty"`
}

type ContinueTurnRequest struct {
	Version          string            `json:"version"`
	Namespace        string            `json:"namespace"`
	TaskName         string            `json:"taskName"`
	SessionName      string            `json:"sessionName"`
	RuntimeSessionID RuntimeSessionID  `json:"runtimeSessionID"`
	TurnID           HarnessTurnID     `json:"turnID"`
	CorrelationID    string            `json:"correlationID"`
	ToolResults      []ToolCallResult  `json:"toolResults,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

type ContinueTurnResponse struct {
	Version          string           `json:"version"`
	Accepted         bool             `json:"accepted"`
	RuntimeSessionID RuntimeSessionID `json:"runtimeSessionID"`
	TurnID           HarnessTurnID    `json:"turnID"`
	CorrelationID    string           `json:"correlationID,omitempty"`
	Message          string           `json:"message,omitempty"`
}

type CapabilitiesResponse struct {
	Version                   string              `json:"version"`
	ProtocolVersion           string              `json:"protocolVersion"`
	Transport                 string              `json:"transport"`
	RuntimeName               string              `json:"runtimeName"`
	RuntimeVersion            string              `json:"runtimeVersion,omitempty"`
	ProviderKind              ProviderKind        `json:"providerKind"`
	ToolExecutionModes        []ToolExecutionMode `json:"toolExecutionModes"`
	BrokeredToolClasses       []BrokeredToolClass `json:"brokeredToolClasses,omitempty"`
	SupportsCancel            bool                `json:"supportsCancel"`
	SupportsRuntimeSessions   bool                `json:"supportsRuntimeSessions"`
	SupportsContinuation      bool                `json:"supportsContinuation,omitempty"`
	SupportsArtifacts         bool                `json:"supportsArtifacts,omitempty"`
	SupportsSuspend           bool                `json:"supportsSuspend,omitempty"`
	SupportsWorkspaceSnapshot bool                `json:"supportsWorkspaceSnapshot,omitempty"`
	MaxConcurrentTurns        int                 `json:"maxConcurrentTurns,omitempty"`
	MaxTurnSeconds            int                 `json:"maxTurnSeconds,omitempty"`
	MaxOutputBytes            int64               `json:"maxOutputBytes,omitempty"`
	Metadata                  map[string]string   `json:"metadata,omitempty"`
}

type HealthResponse struct {
	Version          string            `json:"version"`
	Status           HealthStatus      `json:"status"`
	Ready            bool              `json:"ready"`
	RuntimeSessionID RuntimeSessionID  `json:"runtimeSessionID,omitempty"`
	Message          string            `json:"message,omitempty"`
	CheckedAt        time.Time         `json:"checkedAt"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// HarnessEventFrame is the harness-to-Orka stream unit. Frame types are stable,
// while Content/Metadata carry frame-specific data after redaction at the client
// and event-store boundaries.
type HarnessEventFrame struct {
	Version          string            `json:"version"`
	Type             FrameType         `json:"type"`
	RuntimeSessionID RuntimeSessionID  `json:"runtimeSessionID"`
	TurnID           HarnessTurnID     `json:"turnID"`
	CorrelationID    string            `json:"correlationID"`
	Seq              int64             `json:"seq"`
	CreatedAt        time.Time         `json:"createdAt,omitempty"`
	Severity         string            `json:"severity,omitempty"`
	Summary          string            `json:"summary,omitempty"`
	Content          json.RawMessage   `json:"content,omitempty"`
	ContentText      string            `json:"contentText,omitempty"`
	ToolName         string            `json:"toolName,omitempty"`
	ToolCallID       string            `json:"toolCallID,omitempty"`
	ApprovalID       string            `json:"approvalID,omitempty"`
	Completed        *TurnCompleted    `json:"completed,omitempty"`
	Failed           *TurnFailed       `json:"failed,omitempty"`
	Error            *ErrorInfo        `json:"error,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

type TurnCompleted struct {
	Result        string         `json:"result,omitempty"`
	Data          map[string]any `json:"data,omitempty"`
	Artifacts     []ArtifactRef  `json:"artifacts,omitempty"`
	OutputRef     string         `json:"outputRef,omitempty"`
	FinalEventSeq int64          `json:"finalEventSeq,omitempty"`
	RetainSession bool           `json:"retainSession,omitempty"`
}

type TurnFailed struct {
	Reason    string         `json:"reason"`
	Message   string         `json:"message,omitempty"`
	Result    string         `json:"result,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
	Artifacts []ArtifactRef  `json:"artifacts,omitempty"`
	OutputRef string         `json:"outputRef,omitempty"`
	Retryable bool           `json:"retryable,omitempty"`
}

// ArtifactRef is safe artifact metadata passed through the harness protocol.
// Artifact bytes remain in Orka-managed storage or at an adapter-provided outputRef.
type ArtifactRef struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType,omitempty"`
	Size        int64  `json:"size,omitempty"`
	Description string `json:"description,omitempty"`
}

type ErrorInfo struct {
	Code      string `json:"code"`
	Message   string `json:"message,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`
}

type ToolCallRequest struct {
	Version           string            `json:"version"`
	RuntimeSessionID  RuntimeSessionID  `json:"runtimeSessionID"`
	TurnID            HarnessTurnID     `json:"turnID"`
	ToolCallID        string            `json:"toolCallID"`
	ToolName          string            `json:"toolName"`
	IdempotencyKey    string            `json:"idempotencyKey"`
	Input             json.RawMessage   `json:"input,omitempty"`
	RequiresApproval  bool              `json:"requiresApproval,omitempty"`
	ApprovalPolicyRef *PolicyRef        `json:"approvalPolicyRef,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
}

type ToolCallResult struct {
	Version          string           `json:"version"`
	RuntimeSessionID RuntimeSessionID `json:"runtimeSessionID"`
	TurnID           HarnessTurnID    `json:"turnID"`
	ToolCallID       string           `json:"toolCallID"`
	IdempotencyKey   string           `json:"idempotencyKey"`
	Approved         bool             `json:"approved,omitempty"`
	Output           json.RawMessage  `json:"output,omitempty"`
	Error            *ErrorInfo       `json:"error,omitempty"`
}

func (r StartTurnRequest) Validate() error {
	if err := validateVersion(r.Version); err != nil {
		return err
	}
	if strings.TrimSpace(r.Namespace) == "" {
		return fmt.Errorf("namespace is required")
	}
	if strings.TrimSpace(r.TaskName) == "" {
		return fmt.Errorf("task name is required")
	}
	if strings.TrimSpace(r.SessionName) == "" {
		return fmt.Errorf("session name is required")
	}
	if strings.TrimSpace(string(r.RuntimeSessionID)) == "" {
		return fmt.Errorf("runtime session id is required")
	}
	if strings.TrimSpace(string(r.TurnID)) == "" {
		return fmt.Errorf("turn id is required")
	}
	if strings.TrimSpace(r.CorrelationID) == "" {
		return fmt.Errorf("correlation id is required")
	}
	if r.Deadline.IsZero() {
		return fmt.Errorf("deadline is required")
	}
	if strings.TrimSpace(r.AuthIdentity.Subject) == "" && strings.TrimSpace(r.AuthIdentity.Username) == "" {
		return fmt.Errorf("auth identity subject or username is required")
	}
	if r.EventCursor < 0 {
		return fmt.Errorf("event cursor must be non-negative")
	}
	if r.ToolExecutionMode != "" && !IsKnownToolExecutionMode(r.ToolExecutionMode) {
		return fmt.Errorf("unsupported tool execution mode %q", r.ToolExecutionMode)
	}
	if r.ToolPolicyRef != nil && strings.TrimSpace(r.ToolPolicyRef.Name) == "" {
		return fmt.Errorf("tool policy ref name is required")
	}
	if r.ApprovalPolicyRef != nil && strings.TrimSpace(r.ApprovalPolicyRef.Name) == "" {
		return fmt.Errorf("approval policy ref name is required")
	}
	for i, env := range r.Input.Env {
		if strings.TrimSpace(env.Name) == "" {
			return fmt.Errorf("input env %d name is required", i)
		}
		if !isValidTurnEnvName(env.Name) {
			return fmt.Errorf("input env %d name %q is invalid", i, env.Name)
		}
	}
	for i, ref := range r.Input.ContextRefs {
		if strings.TrimSpace(ref.Kind) == "" {
			return fmt.Errorf("context ref %d kind is required", i)
		}
		if strings.TrimSpace(ref.Name) == "" {
			return fmt.Errorf("context ref %d name is required", i)
		}
		if ref.Seq < 0 {
			return fmt.Errorf("context ref %d seq must be non-negative", i)
		}
	}
	for i, tool := range r.Input.Tools {
		if err := tool.Validate(); err != nil {
			return fmt.Errorf("input tool %d: %w", i, err)
		}
	}
	return nil
}

func (t ToolDefinition) Validate() error {
	if strings.TrimSpace(t.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if t.BrokeredClass != "" && !IsKnownBrokeredToolClass(t.BrokeredClass) {
		return fmt.Errorf("unsupported brokered class %q", t.BrokeredClass)
	}
	if len(t.Parameters) > 0 && !json.Valid(t.Parameters) {
		return fmt.Errorf("parameters must be valid JSON")
	}
	return nil
}

func isValidTurnEnvName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func (r CancelTurnRequest) Validate() error {
	if err := validateVersion(r.Version); err != nil {
		return err
	}
	if strings.TrimSpace(r.Namespace) == "" {
		return fmt.Errorf("namespace is required")
	}
	if strings.TrimSpace(r.TaskName) == "" {
		return fmt.Errorf("task name is required")
	}
	if strings.TrimSpace(r.SessionName) == "" {
		return fmt.Errorf("session name is required")
	}
	if strings.TrimSpace(string(r.RuntimeSessionID)) == "" {
		return fmt.Errorf("runtime session id is required")
	}
	if strings.TrimSpace(string(r.TurnID)) == "" {
		return fmt.Errorf("turn id is required")
	}
	if strings.TrimSpace(r.CorrelationID) == "" {
		return fmt.Errorf("correlation id is required")
	}
	return nil
}

func (r ContinueTurnRequest) Validate() error {
	if err := validateVersion(r.Version); err != nil {
		return err
	}
	if strings.TrimSpace(r.Namespace) == "" {
		return fmt.Errorf("namespace is required")
	}
	if strings.TrimSpace(r.TaskName) == "" {
		return fmt.Errorf("task name is required")
	}
	if strings.TrimSpace(r.SessionName) == "" {
		return fmt.Errorf("session name is required")
	}
	if strings.TrimSpace(string(r.RuntimeSessionID)) == "" {
		return fmt.Errorf("runtime session id is required")
	}
	if strings.TrimSpace(string(r.TurnID)) == "" {
		return fmt.Errorf("turn id is required")
	}
	if strings.TrimSpace(r.CorrelationID) == "" {
		return fmt.Errorf("correlation id is required")
	}
	if len(r.ToolResults) == 0 {
		return fmt.Errorf("at least one tool result is required")
	}
	seenToolCallIDs := make(map[string]struct{}, len(r.ToolResults))
	for i, result := range r.ToolResults {
		if err := result.Validate(); err != nil {
			return fmt.Errorf("tool result %d is invalid: %w", i, err)
		}
		if _, exists := seenToolCallIDs[result.ToolCallID]; exists {
			return fmt.Errorf("tool result %d duplicates tool call id %q", i, result.ToolCallID)
		}
		seenToolCallIDs[result.ToolCallID] = struct{}{}
		expectedKey := ToolRequestIdempotencyKey(result.RuntimeSessionID, result.TurnID, result.ToolCallID)
		if result.IdempotencyKey != expectedKey {
			return fmt.Errorf("tool result %d idempotency key %q does not match canonical key %q", i, result.IdempotencyKey, expectedKey)
		}
		if len(result.Output) == 0 && result.Error == nil {
			return fmt.Errorf("tool result %d output or error is required", i)
		}
		if result.RuntimeSessionID != r.RuntimeSessionID {
			return fmt.Errorf("tool result %d runtime session id %q does not match continue request %q", i, result.RuntimeSessionID, r.RuntimeSessionID)
		}
		if result.TurnID != r.TurnID {
			return fmt.Errorf("tool result %d turn id %q does not match continue request %q", i, result.TurnID, r.TurnID)
		}
	}
	return nil
}

func (r ContinueTurnResponse) ValidateFor(request ContinueTurnRequest) error {
	if err := validateVersion(r.Version); err != nil {
		return err
	}
	if !r.Accepted {
		return fmt.Errorf("harness did not accept continuation")
	}
	if r.RuntimeSessionID != request.RuntimeSessionID {
		return fmt.Errorf("harness continued runtime session %q, want %q", r.RuntimeSessionID, request.RuntimeSessionID)
	}
	if r.TurnID != request.TurnID {
		return fmt.Errorf("harness continued turn %q, want %q", r.TurnID, request.TurnID)
	}
	if r.CorrelationID != "" && r.CorrelationID != request.CorrelationID {
		return fmt.Errorf("harness continued correlation id %q, want %q", r.CorrelationID, request.CorrelationID)
	}
	return nil
}

func (r CapabilitiesResponse) Validate() error {
	if err := validateVersion(r.Version); err != nil {
		return err
	}
	if strings.TrimSpace(r.ProtocolVersion) != ProtocolVersion {
		return fmt.Errorf("unsupported protocol version %q", r.ProtocolVersion)
	}
	if strings.TrimSpace(r.Transport) == "" {
		return fmt.Errorf("transport is required")
	}
	if strings.TrimSpace(r.RuntimeName) == "" {
		return fmt.Errorf("runtime name is required")
	}
	if strings.TrimSpace(string(r.ProviderKind)) == "" {
		return fmt.Errorf("provider kind is required")
	}
	if len(r.ToolExecutionModes) == 0 {
		return fmt.Errorf("at least one tool execution mode is required")
	}
	for _, mode := range r.ToolExecutionModes {
		if !IsKnownToolExecutionMode(mode) {
			return fmt.Errorf("unsupported tool execution mode %q", mode)
		}
	}
	for _, class := range r.BrokeredToolClasses {
		if !IsKnownBrokeredToolClass(class) {
			return fmt.Errorf("unsupported brokered tool class %q", class)
		}
	}
	if len(r.BrokeredToolClasses) > 0 && !containsToolExecutionMode(r.ToolExecutionModes, ToolExecutionModeBrokered) {
		return fmt.Errorf("brokeredToolClasses require tool execution mode %q", ToolExecutionModeBrokered)
	}
	if containsToolExecutionMode(r.ToolExecutionModes, ToolExecutionModeBrokered) && len(r.BrokeredToolClasses) == 0 {
		return fmt.Errorf("brokered tool execution mode requires at least one brokeredToolClass")
	}
	if containsToolExecutionMode(r.ToolExecutionModes, ToolExecutionModeBrokered) && !r.SupportsContinuation {
		return fmt.Errorf("brokered tool execution mode requires supportsContinuation")
	}
	if r.MaxConcurrentTurns < 0 {
		return fmt.Errorf("max concurrent turns must be non-negative")
	}
	if r.MaxTurnSeconds < 0 {
		return fmt.Errorf("max turn seconds must be non-negative")
	}
	if r.MaxOutputBytes < 0 {
		return fmt.Errorf("max output bytes must be non-negative")
	}
	return nil
}

func (r HealthResponse) Validate() error {
	if err := validateVersion(r.Version); err != nil {
		return err
	}
	if strings.TrimSpace(string(r.Status)) == "" {
		return fmt.Errorf("health status is required")
	}
	if !IsKnownHealthStatus(r.Status) {
		return fmt.Errorf("unsupported health status %q", r.Status)
	}
	if r.CheckedAt.IsZero() {
		return fmt.Errorf("checked at is required")
	}
	return nil
}

func (f HarnessEventFrame) ValidateRequired() error {
	if err := validateVersion(f.Version); err != nil {
		return err
	}
	if strings.TrimSpace(string(f.Type)) == "" {
		return fmt.Errorf("frame type is required")
	}
	if strings.TrimSpace(string(f.RuntimeSessionID)) == "" {
		return fmt.Errorf("runtime session id is required")
	}
	if strings.TrimSpace(string(f.TurnID)) == "" {
		return fmt.Errorf("turn id is required")
	}
	if strings.TrimSpace(f.CorrelationID) == "" {
		return fmt.Errorf("correlation id is required")
	}
	if f.Seq <= 0 {
		return fmt.Errorf("frame seq must be positive")
	}
	if f.Type == FrameTurnCompleted && f.Completed == nil {
		return fmt.Errorf("turn completed payload is required")
	}
	if f.Type == FrameTurnFailed && f.Failed == nil {
		return fmt.Errorf("turn failed payload is required")
	}
	return nil
}

func (r ToolCallRequest) Validate() error {
	if err := validateVersion(r.Version); err != nil {
		return err
	}
	if strings.TrimSpace(string(r.RuntimeSessionID)) == "" {
		return fmt.Errorf("runtime session id is required")
	}
	if strings.TrimSpace(string(r.TurnID)) == "" {
		return fmt.Errorf("turn id is required")
	}
	if strings.TrimSpace(r.ToolCallID) == "" {
		return fmt.Errorf("tool call id is required")
	}
	if strings.TrimSpace(r.ToolName) == "" {
		return fmt.Errorf("tool name is required")
	}
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		return fmt.Errorf("idempotency key is required")
	}
	if r.ApprovalPolicyRef != nil && strings.TrimSpace(r.ApprovalPolicyRef.Name) == "" {
		return fmt.Errorf("approval policy ref name is required")
	}
	return nil
}

func (r ToolCallResult) Validate() error {
	if err := validateVersion(r.Version); err != nil {
		return err
	}
	if strings.TrimSpace(string(r.RuntimeSessionID)) == "" {
		return fmt.Errorf("runtime session id is required")
	}
	if strings.TrimSpace(string(r.TurnID)) == "" {
		return fmt.Errorf("turn id is required")
	}
	if strings.TrimSpace(r.ToolCallID) == "" {
		return fmt.Errorf("tool call id is required")
	}
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		return fmt.Errorf("idempotency key is required")
	}
	return nil
}

func IsKnownFrameType(value FrameType) bool {
	switch value {
	case FrameTurnStarted, FrameRuntimeOutput, FrameToolCallRequested, FrameToolResultReceived,
		FrameApprovalRequested, FrameTurnCompleted, FrameTurnFailed, FrameTurnCancelled, FrameRuntimeLog:
		return true
	default:
		return false
	}
}

func IsKnownToolExecutionMode(value ToolExecutionMode) bool {
	switch value {
	case ToolExecutionModeObserved, ToolExecutionModeBrokered:
		return true
	default:
		return false
	}
}

func IsKnownBrokeredToolClass(value BrokeredToolClass) bool {
	switch value {
	case BrokeredToolClassRead, BrokeredToolClassWrite, BrokeredToolClassCoordination:
		return true
	default:
		return false
	}
}

func containsToolExecutionMode(modes []ToolExecutionMode, want ToolExecutionMode) bool {
	return slices.Contains(modes, want)
}

func IsKnownHealthStatus(value HealthStatus) bool {
	switch value {
	case HealthStatusOK, HealthStatusDegraded, HealthStatusUnhealthy:
		return true
	default:
		return false
	}
}

func ToolRequestIdempotencyKey(runtimeSessionID RuntimeSessionID, turnID HarnessTurnID, toolCallID string) string {
	return strings.Join([]string{
		strings.TrimSpace(string(runtimeSessionID)),
		strings.TrimSpace(string(turnID)),
		strings.TrimSpace(toolCallID),
	}, ":")
}

func validateVersion(version string) error {
	version = strings.TrimSpace(version)
	if version == "" {
		return fmt.Errorf("version is required")
	}
	if version != ProtocolVersion {
		return fmt.Errorf("unsupported version %q", version)
	}
	return nil
}
