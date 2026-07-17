package harness

import (
	"net/http"
	"time"

	internal "github.com/orka-agents/orka/internal/harness"
)

const (
	ProtocolVersion = internal.ProtocolVersion
	HTTPTransport   = internal.HTTPTransport

	HealthPath       = internal.HealthPath
	CapabilitiesPath = internal.CapabilitiesPath
	TurnsPath        = internal.TurnsPath

	ToolExecutionModeObserved = internal.ToolExecutionModeObserved
	ToolExecutionModeBrokered = internal.ToolExecutionModeBrokered

	BrokeredToolClassRead         = internal.BrokeredToolClassRead
	BrokeredToolClassWrite        = internal.BrokeredToolClassWrite
	BrokeredToolClassCoordination = internal.BrokeredToolClassCoordination

	FrameTurnStarted        = internal.FrameTurnStarted
	FrameRuntimeOutput      = internal.FrameRuntimeOutput
	FrameToolCallRequested  = internal.FrameToolCallRequested
	FrameToolResultReceived = internal.FrameToolResultReceived
	FrameApprovalRequested  = internal.FrameApprovalRequested
	FrameTurnCompleted      = internal.FrameTurnCompleted
	FrameTurnFailed         = internal.FrameTurnFailed
	FrameTurnCancelled      = internal.FrameTurnCancelled
	FrameRuntimeLog         = internal.FrameRuntimeLog

	ProviderKindKubernetesService = internal.ProviderKindKubernetesService
	ProviderKindSidecar           = internal.ProviderKindSidecar
	ProviderKindAgentSandbox      = internal.ProviderKindAgentSandbox
	ProviderKindSubstrate         = internal.ProviderKindSubstrate
	ProviderKindRemote            = internal.ProviderKindRemote

	HealthStatusOK        = internal.HealthStatusOK
	HealthStatusDegraded  = internal.HealthStatusDegraded
	HealthStatusUnhealthy = internal.HealthStatusUnhealthy

	TurnResourceEvents   = internal.TurnResourceEvents
	TurnResourceContinue = internal.TurnResourceContinue
	TurnResourceCancel   = internal.TurnResourceCancel
	TurnResourceOutput   = internal.TurnResourceOutput
)

type (
	RuntimeSessionID = internal.RuntimeSessionID
	HarnessTurnID    = internal.HarnessTurnID

	ToolExecutionMode = internal.ToolExecutionMode
	BrokeredToolClass = internal.BrokeredToolClass
	FrameType         = internal.FrameType
	ProviderKind      = internal.ProviderKind
	HealthStatus      = internal.HealthStatus

	StartTurnRequest  = internal.StartTurnRequest
	AuthIdentity      = internal.AuthIdentity
	PolicyRef         = internal.PolicyRef
	TurnInput         = internal.TurnInput
	ToolDefinition    = internal.ToolDefinition
	TurnEnvVar        = internal.TurnEnvVar
	ContextRef        = internal.ContextRef
	StartTurnResponse = internal.StartTurnResponse

	CancelTurnRequest    = internal.CancelTurnRequest
	CancelTurnResponse   = internal.CancelTurnResponse
	ContinueTurnRequest  = internal.ContinueTurnRequest
	ContinueTurnResponse = internal.ContinueTurnResponse

	CapabilitiesResponse = internal.CapabilitiesResponse
	HealthResponse       = internal.HealthResponse
	HarnessEventFrame    = internal.HarnessEventFrame
	TurnCompleted        = internal.TurnCompleted
	TurnFailed           = internal.TurnFailed
	ArtifactRef          = internal.ArtifactRef
	ErrorInfo            = internal.ErrorInfo
	ToolCallRequest      = internal.ToolCallRequest
	ToolCallResult       = internal.ToolCallResult

	Client       = internal.Client
	ClientOption = internal.ClientOption
	ClientError  = internal.ClientError
)

var ErrTurnPathNotFound = internal.ErrTurnPathNotFound

// ValidateTurnPathSegment validates that a turn ID occupies one URL path segment.
func ValidateTurnPathSegment(turnID HarnessTurnID) error {
	return internal.ValidateTurnPathSegment(turnID)
}

// TurnResourcePath builds the escaped path for a harness turn sub-resource.
func TurnResourcePath(turnID HarnessTurnID, resource string) (string, error) {
	return internal.TurnResourcePath(turnID, resource)
}

// EventStreamPath builds the escaped SSE event stream path for a turn.
func EventStreamPath(turnID HarnessTurnID) (string, error) {
	return internal.EventStreamPath(turnID)
}

// CancelTurnPath builds the escaped cancel path for a turn.
func CancelTurnPath(turnID HarnessTurnID) (string, error) {
	return internal.CancelTurnPath(turnID)
}

// ContinueTurnPath builds the continuation path for a suspended turn.
func ContinueTurnPath(turnID HarnessTurnID) (string, error) {
	return internal.ContinueTurnPath(turnID)
}

// OutputTurnPath builds the escaped output fetch path for a turn.
func OutputTurnPath(turnID HarnessTurnID) (string, error) {
	return internal.OutputTurnPath(turnID)
}

// ParseTurnResourcePath extracts the turn ID and resource from a turn resource path.
func ParseTurnResourcePath(rawPath string) (HarnessTurnID, string, error) {
	return internal.ParseTurnResourcePath(rawPath)
}

// IsKnownFrameType reports whether value is a protocol-defined frame type.
func IsKnownFrameType(value FrameType) bool {
	return internal.IsKnownFrameType(value)
}

// IsKnownToolExecutionMode reports whether value is a protocol-defined tool execution mode.
func IsKnownToolExecutionMode(value ToolExecutionMode) bool {
	return internal.IsKnownToolExecutionMode(value)
}

// IsKnownBrokeredToolClass reports whether value is a protocol-defined brokered tool class.
func IsKnownBrokeredToolClass(value BrokeredToolClass) bool {
	return internal.IsKnownBrokeredToolClass(value)
}

// IsKnownHealthStatus reports whether value is a protocol-defined health status.
func IsKnownHealthStatus(value HealthStatus) bool {
	return internal.IsKnownHealthStatus(value)
}

// ToolRequestIdempotencyKey returns the canonical idempotency key for a brokered tool call.
func ToolRequestIdempotencyKey(runtimeSessionID RuntimeSessionID, turnID HarnessTurnID, toolCallID string) string {
	return internal.ToolRequestIdempotencyKey(runtimeSessionID, turnID, toolCallID)
}

// NewClient creates a harness protocol client for baseURL.
func NewClient(baseURL string, opts ...ClientOption) (*Client, error) {
	return internal.NewClient(baseURL, opts...)
}

// WithHTTPClient configures the HTTP client used by a harness client.
func WithHTTPClient(httpClient *http.Client) ClientOption {
	return internal.WithHTTPClient(httpClient)
}

// WithControlTimeout configures the timeout used for non-streaming control requests.
func WithControlTimeout(timeout time.Duration) ClientOption {
	return internal.WithControlTimeout(timeout)
}

// WithBearerToken configures bearer authentication for harness requests.
func WithBearerToken(token string) ClientOption {
	return internal.WithBearerToken(token)
}

// WriteJSON writes value as a JSON response with status.
func WriteJSON(w http.ResponseWriter, status int, value any) {
	internal.WriteJSON(w, status, value)
}

// WriteError writes a JSON error response with status.
func WriteError(w http.ResponseWriter, status int, message string) {
	internal.WriteError(w, status, message)
}

// WriteSSEFrame writes one harness event frame to an SSE response.
func WriteSSEFrame(w http.ResponseWriter, frame HarnessEventFrame) error {
	return internal.WriteSSEFrame(w, frame)
}

// WriteSSEDone writes the terminal SSE sentinel.
func WriteSSEDone(w http.ResponseWriter) error {
	return internal.WriteSSEDone(w)
}
