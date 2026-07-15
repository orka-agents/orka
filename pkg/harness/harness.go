package harness

import internal "github.com/orka-agents/orka/internal/harness"

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

var (
	ErrTurnPathNotFound = internal.ErrTurnPathNotFound

	ValidateTurnPathSegment = internal.ValidateTurnPathSegment
	TurnResourcePath        = internal.TurnResourcePath
	EventStreamPath         = internal.EventStreamPath
	CancelTurnPath          = internal.CancelTurnPath
	ContinueTurnPath        = internal.ContinueTurnPath
	OutputTurnPath          = internal.OutputTurnPath
	ParseTurnResourcePath   = internal.ParseTurnResourcePath

	IsKnownFrameType          = internal.IsKnownFrameType
	IsKnownToolExecutionMode  = internal.IsKnownToolExecutionMode
	IsKnownBrokeredToolClass  = internal.IsKnownBrokeredToolClass
	IsKnownHealthStatus       = internal.IsKnownHealthStatus
	ToolRequestIdempotencyKey = internal.ToolRequestIdempotencyKey

	NewClient          = internal.NewClient
	WithHTTPClient     = internal.WithHTTPClient
	WithControlTimeout = internal.WithControlTimeout
	WithBearerToken    = internal.WithBearerToken

	WriteJSON     = internal.WriteJSON
	WriteError    = internal.WriteError
	WriteSSEFrame = internal.WriteSSEFrame
	WriteSSEDone  = internal.WriteSSEDone
)
