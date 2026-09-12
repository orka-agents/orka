package v2

import (
	"fmt"
	"strings"
	"time"
)

type HealthStatus string

const (
	HealthStatusOK        HealthStatus = "ok"
	HealthStatusDegraded  HealthStatus = "degraded"
	HealthStatusUnhealthy HealthStatus = "unhealthy"
)

type HealthResponse struct {
	Protocol  string       `json:"protocol"`
	Status    HealthStatus `json:"status"`
	Timestamp time.Time    `json:"timestamp"`
}

func (r HealthResponse) Validate() error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	switch r.Status {
	case HealthStatusOK, HealthStatusDegraded, HealthStatusUnhealthy:
	default:
		return fmt.Errorf("unsupported health status %q", r.Status)
	}
	return validateTimestamp("health timestamp", r.Timestamp)
}

type ProviderCapabilities struct {
	ProviderKinds             []string `json:"providerKinds"`
	Models                    []string `json:"models,omitempty"`
	SupportsPermissions       bool     `json:"supportsPermissions"`
	SupportsCancel            bool     `json:"supportsCancel"`
	SupportsTools             bool     `json:"supportsTools"`
	SupportsImages            bool     `json:"supportsImages,omitempty"`
	SupportsAudio             bool     `json:"supportsAudio,omitempty"`
	SupportsEmbeddedResources bool     `json:"supportsEmbeddedResources,omitempty"`
}

func (c ProviderCapabilities) Validate() error {
	if len(c.ProviderKinds) == 0 {
		return fmt.Errorf("at least one provider kind is required")
	}
	if err := validateUniqueStrings("provider kind", c.ProviderKinds, 32); err != nil {
		return err
	}
	if err := validateUniqueStrings("model", c.Models, 256); err != nil {
		return err
	}
	return nil
}

const (
	DefaultMaxResidentSessions  uint32 = 10
	DefaultMaxConcurrentPrompts uint32 = 4
)

type ProtocolLimits struct {
	MaxResidentSessions      uint32 `json:"maxResidentSessions"`
	MaxConcurrentPrompts     uint32 `json:"maxConcurrentPrompts"`
	MaxRequestBytes          int    `json:"maxRequestBytes"`
	MaxEventLineBytes        int    `json:"maxEventLineBytes"`
	MaxTerminalResultBytes   int    `json:"maxTerminalResultBytes"`
	MaxBufferedEvents        int    `json:"maxBufferedEvents"`
	MaxUpdateEventsPerSecond int    `json:"maxUpdateEventsPerSecond"`
	MinPromptLeaseMillis     int64  `json:"minPromptLeaseMillis"`
	MaxPromptLeaseMillis     int64  `json:"maxPromptLeaseMillis"`
	MaxPendingPermissions    uint32 `json:"maxPendingPermissions"`
	MaxWorkspaceDeltaBytes   int64  `json:"maxWorkspaceDeltaBytes"`
}

func DefaultProtocolLimits() ProtocolLimits {
	stream := DefaultEventStreamLimits()
	return ProtocolLimits{
		MaxResidentSessions:      DefaultMaxResidentSessions,
		MaxConcurrentPrompts:     DefaultMaxConcurrentPrompts,
		MaxRequestBytes:          1 << 20,
		MaxEventLineBytes:        stream.MaxLineBytes,
		MaxTerminalResultBytes:   stream.MaxTerminalResultBytes,
		MaxBufferedEvents:        stream.MaxBufferedEvents,
		MaxUpdateEventsPerSecond: stream.MaxUpdateEventsPerSecond,
		MinPromptLeaseMillis:     5_000,
		MaxPromptLeaseMillis:     120_000,
		MaxPendingPermissions:    32,
		MaxWorkspaceDeltaBytes:   512 << 20,
	}
}

func (l ProtocolLimits) Validate() error {
	if l.MaxResidentSessions == 0 {
		return fmt.Errorf("max resident sessions must be positive")
	}
	if l.MaxConcurrentPrompts == 0 || l.MaxConcurrentPrompts > l.MaxResidentSessions {
		return fmt.Errorf("max concurrent prompts must be positive and no greater than resident sessions")
	}
	streamLimits := EventStreamLimits{
		MaxLineBytes:             l.MaxEventLineBytes,
		MaxTerminalResultBytes:   l.MaxTerminalResultBytes,
		MaxBufferedEvents:        l.MaxBufferedEvents,
		MaxUpdateEventsPerSecond: l.MaxUpdateEventsPerSecond,
	}
	if err := streamLimits.Validate(); err != nil {
		return fmt.Errorf("event limits: %w", err)
	}
	if l.MaxRequestBytes <= 0 || l.MaxRequestBytes > MaxCanonicalJSONBytes {
		return fmt.Errorf("max request bytes must be in range 1..%d", MaxCanonicalJSONBytes)
	}
	if l.MinPromptLeaseMillis <= 0 || l.MaxPromptLeaseMillis < l.MinPromptLeaseMillis {
		return fmt.Errorf("prompt lease bounds are invalid")
	}
	if l.MaxPendingPermissions == 0 {
		return fmt.Errorf("max pending permissions must be positive")
	}
	if l.MaxWorkspaceDeltaBytes <= 0 {
		return fmt.Errorf("max workspace delta bytes must be positive")
	}
	return nil
}

type WorkspaceGovernanceMode string

const (
	WorkspaceGovernanceStrict  WorkspaceGovernanceMode = "strict-governed"
	WorkspaceGovernanceTrusted WorkspaceGovernanceMode = "trusted-non-governed"
)

type WorkspaceGovernanceCapabilities struct {
	Mode                            WorkspaceGovernanceMode `json:"mode"`
	Trusted                         bool                    `json:"trusted,omitempty"`
	OrkaOwnedWorkspaceDeltas        bool                    `json:"orkaOwnedWorkspaceDeltas"`
	PromptScopedBrokerAuthorization bool                    `json:"promptScopedBrokerAuthorization"`
	NoDirectSCMPublication          bool                    `json:"noDirectSCMPublication"`
	OrkaOwnedCleanRoomPublication   bool                    `json:"orkaOwnedCleanRoomPublication"`
	ExactInstanceFencing            bool                    `json:"exactInstanceFencing"`
	DuplicateSafeMutations          bool                    `json:"duplicateSafeMutations"`
	CancellationSettlement          bool                    `json:"cancellationSettlement"`
}

func StrictWorkspaceGovernanceCapabilities() WorkspaceGovernanceCapabilities {
	return WorkspaceGovernanceCapabilities{
		Mode: WorkspaceGovernanceStrict, OrkaOwnedWorkspaceDeltas: true,
		PromptScopedBrokerAuthorization: true, NoDirectSCMPublication: true,
		OrkaOwnedCleanRoomPublication: true, ExactInstanceFencing: true,
		DuplicateSafeMutations: true, CancellationSettlement: true,
	}
}

func (c WorkspaceGovernanceCapabilities) Strict() bool {
	return c.Mode == WorkspaceGovernanceStrict && !c.Trusted && c.OrkaOwnedWorkspaceDeltas &&
		c.PromptScopedBrokerAuthorization && c.NoDirectSCMPublication && c.OrkaOwnedCleanRoomPublication &&
		c.ExactInstanceFencing && c.DuplicateSafeMutations && c.CancellationSettlement
}

func (c WorkspaceGovernanceCapabilities) Validate() error {
	switch c.Mode {
	case WorkspaceGovernanceStrict:
		if !c.Strict() {
			return fmt.Errorf("strict workspace governance requires every governance guarantee and trusted=false")
		}
	case WorkspaceGovernanceTrusted:
		if !c.Trusted || c.OrkaOwnedWorkspaceDeltas || c.PromptScopedBrokerAuthorization || c.NoDirectSCMPublication ||
			c.OrkaOwnedCleanRoomPublication || c.ExactInstanceFencing || c.DuplicateSafeMutations || c.CancellationSettlement {
			return fmt.Errorf("trusted non-governed runtime must be explicitly marked trusted and must not claim strict guarantees")
		}
	default:
		return fmt.Errorf("unsupported workspace governance mode %q", c.Mode)
	}
	return nil
}

type CapabilitiesResponse struct {
	Protocol                          string                          `json:"protocol"`
	Transport                         string                          `json:"transport"`
	ACPVersion                        string                          `json:"acpVersion"`
	RuntimeProfileDigest              ProfileDigest                   `json:"runtimeProfileDigest"`
	ProfileDigestSchemaVersion        uint32                          `json:"profileDigestSchemaVersion"`
	AdapterDigests                    map[string]string               `json:"adapterDigests"`
	Limits                            ProtocolLimits                  `json:"limits"`
	Provider                          ProviderCapabilities            `json:"provider"`
	WorkspaceGovernance               WorkspaceGovernanceCapabilities `json:"workspaceGovernance"`
	SupportsDrain                     bool                            `json:"supportsDrain"`
	SupportsPublicationFinalization   bool                            `json:"supportsPublicationFinalization"`
	SupportsAgentSessionConfiguration bool                            `json:"supportsAgentSessionConfiguration,omitempty"`
}

func (r CapabilitiesResponse) Validate() error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if r.Transport != "http+ndjson" {
		return fmt.Errorf("transport %q is unsupported", r.Transport)
	}
	if r.ACPVersion != ACPProfileV1 {
		return fmt.Errorf("ACP version %q is unsupported; want %q", r.ACPVersion, ACPProfileV1)
	}
	if err := ValidateProfileDigest(r.RuntimeProfileDigest); err != nil {
		return fmt.Errorf("runtime profile digest: %w", err)
	}
	if r.ProfileDigestSchemaVersion != ProfileDigestSchemaVersion {
		return fmt.Errorf("profile digest schema version %d is unsupported; want %d", r.ProfileDigestSchemaVersion, ProfileDigestSchemaVersion)
	}
	if len(r.AdapterDigests) == 0 {
		return fmt.Errorf("at least one adapter digest is required")
	}
	if len(r.AdapterDigests) > 32 {
		return fmt.Errorf("adapter digest count exceeds 32")
	}
	for name, digest := range r.AdapterDigests {
		if err := validateBoundedString("adapter name", name, true, 128); err != nil {
			return err
		}
		if err := validateSHA256Digest(digest); err != nil {
			return fmt.Errorf("adapter %q digest: %w", name, err)
		}
	}
	if err := r.Limits.Validate(); err != nil {
		return err
	}
	if err := r.Provider.Validate(); err != nil {
		return err
	}
	return r.WorkspaceGovernance.Validate()
}

type SupervisorLifecycle string

const (
	SupervisorLifecycleBooting     SupervisorLifecycle = "booting"
	SupervisorLifecycleReady       SupervisorLifecycle = "ready"
	SupervisorLifecycleDraining    SupervisorLifecycle = "draining"
	SupervisorLifecycleTerminating SupervisorLifecycle = "terminating"
	SupervisorLifecycleUnhealthy   SupervisorLifecycle = "unhealthy"
)

const DrainReasonSessionIdentityCapacity = "session_identity_capacity"

type DrainStatus struct {
	AcceptingNewSessions bool      `json:"acceptingNewSessions"`
	Requested            bool      `json:"requested"`
	RequestedAt          time.Time `json:"requestedAt,omitempty"`
	Reason               string    `json:"reason,omitempty"`
}

type RuntimeSessionStatus struct {
	RuntimeSessionID        RuntimeSessionID    `json:"runtimeSessionID"`
	RuntimeSessionUID       RuntimeSessionUID   `json:"runtimeSessionUID"`
	Generation              uint64              `json:"generation"`
	State                   RuntimeSessionState `json:"state"`
	ActivePromptID          PromptID            `json:"activePromptID,omitempty"`
	PendingPermissionCount  uint32              `json:"pendingPermissionCount"`
	ReservedForFinalization bool                `json:"reservedForFinalization"`
	LiveDescendantCount     uint32              `json:"liveDescendantCount"`
	LastTransitionAt        time.Time           `json:"lastTransitionAt"`
}

func (s RuntimeSessionStatus) Validate() error {
	if err := requireIdentifier("runtime session ID", string(s.RuntimeSessionID)); err != nil {
		return err
	}
	if err := requireIdentifier("runtime session UID", string(s.RuntimeSessionUID)); err != nil {
		return err
	}
	if s.Generation == 0 {
		return fmt.Errorf("runtime session generation must be positive")
	}
	if !IsKnownRuntimeSessionState(s.State) {
		return fmt.Errorf("unsupported runtime session state %q", s.State)
	}
	if s.State == RuntimeSessionStatePromptRunning {
		if err := requireIdentifier("active prompt ID", string(s.ActivePromptID)); err != nil {
			return err
		}
	} else if s.ActivePromptID != "" {
		return fmt.Errorf("active prompt ID is only valid while prompt is running")
	}
	if s.State == RuntimeSessionStateFinalizing && !s.ReservedForFinalization {
		return fmt.Errorf("finalizing runtime session must be reserved for finalization")
	}
	if s.ReservedForFinalization && s.State != RuntimeSessionStateFinalizing &&
		s.State != RuntimeSessionStateDeleting && s.State != RuntimeSessionStatePoisoned {
		return fmt.Errorf("finalization reservation is only valid while finalizing, deleting, or poisoned")
	}
	if err := validateTimestamp("last transition timestamp", s.LastTransitionAt); err != nil {
		return err
	}
	return nil
}

type ActivePromptStatus struct {
	RuntimeSessionUID  RuntimeSessionUID `json:"runtimeSessionUID"`
	SessionGeneration  uint64            `json:"sessionGeneration"`
	TaskUID            TaskUID           `json:"taskUID"`
	TaskAttempt        uint32            `json:"taskAttempt"`
	PromptID           PromptID          `json:"promptID"`
	LeaseExpiresAt     time.Time         `json:"leaseExpiresAt"`
	FrameSequence      uint64            `json:"frameSequence"`
	PendingPermissions uint32            `json:"pendingPermissions"`
	StartedAt          time.Time         `json:"startedAt"`
}

func (s ActivePromptStatus) Validate() error {
	if err := requireIdentifier("runtime session UID", string(s.RuntimeSessionUID)); err != nil {
		return err
	}
	if s.SessionGeneration == 0 {
		return fmt.Errorf("session generation must be positive")
	}
	if err := requireIdentifier("task UID", string(s.TaskUID)); err != nil {
		return err
	}
	if s.TaskAttempt == 0 {
		return fmt.Errorf("task attempt must be positive")
	}
	if err := requireIdentifier("prompt ID", string(s.PromptID)); err != nil {
		return err
	}
	if err := validateTimestamp("lease expiry", s.LeaseExpiresAt); err != nil {
		return err
	}
	if s.FrameSequence == 0 {
		return fmt.Errorf("frame sequence must be positive")
	}
	return validateTimestamp("prompt start timestamp", s.StartedAt)
}

type PendingPermissionStatus struct {
	RuntimeSessionUID RuntimeSessionUID   `json:"runtimeSessionUID"`
	PromptID          PromptID            `json:"promptID"`
	RequestID         PermissionRequestID `json:"requestID"`
	RequestedAt       time.Time           `json:"requestedAt"`
	ExpiresAt         time.Time           `json:"expiresAt"`
}

func (s PendingPermissionStatus) Validate() error {
	if err := requireIdentifier("runtime session UID", string(s.RuntimeSessionUID)); err != nil {
		return err
	}
	if err := requireIdentifier("prompt ID", string(s.PromptID)); err != nil {
		return err
	}
	if err := requireIdentifier("permission request ID", string(s.RequestID)); err != nil {
		return err
	}
	if err := validateTimestamp("permission request timestamp", s.RequestedAt); err != nil {
		return err
	}
	if err := validateTimestamp("permission expiry", s.ExpiresAt); err != nil {
		return err
	}
	if !s.ExpiresAt.After(s.RequestedAt) {
		return fmt.Errorf("permission expiry must follow request timestamp")
	}
	return nil
}

type PressureMetadata struct {
	ResidentSessions      uint32 `json:"residentSessions"`
	ActivePrompts         uint32 `json:"activePrompts"`
	QueuedAdmissions      uint32 `json:"queuedAdmissions"`
	PendingPermissions    uint32 `json:"pendingPermissions"`
	LiveDescendants       uint32 `json:"liveDescendants"`
	MemoryWorkingSetBytes uint64 `json:"memoryWorkingSetBytes,omitempty"`
	WritableBytes         uint64 `json:"writableBytes,omitempty"`
	PIDCurrent            uint32 `json:"pidCurrent,omitempty"`
}

// SessionIdentityCapacity reports the finite, never-reused child identity
// budget for one supervisor boot. ExhaustionReserve is deliberately excluded
// from admission so the controller can replace the Pod before Remaining reaches
// zero.
type SessionIdentityCapacity struct {
	Total             uint64 `json:"total"`
	Remaining         uint64 `json:"remaining"`
	ExhaustionReserve uint64 `json:"exhaustionReserve"`
}

func (c SessionIdentityCapacity) Validate() error {
	if c.Total == 0 {
		return fmt.Errorf("session identity capacity total must be positive")
	}
	if c.ExhaustionReserve == 0 || c.ExhaustionReserve >= c.Total {
		return fmt.Errorf("session identity exhaustion reserve must be positive and less than total capacity")
	}
	if c.Remaining > c.Total {
		return fmt.Errorf("remaining session identity capacity exceeds total capacity")
	}
	return nil
}

func (c SessionIdentityCapacity) RotationRequired() bool {
	return c.Remaining <= c.ExhaustionReserve
}

type StatusResponse struct {
	Protocol                string                    `json:"protocol"`
	Fence                   Fence                     `json:"fence"`
	Lifecycle               SupervisorLifecycle       `json:"lifecycle"`
	Drain                   DrainStatus               `json:"drain"`
	Sessions                []RuntimeSessionStatus    `json:"sessions"`
	ActivePrompts           []ActivePromptStatus      `json:"activePrompts"`
	PendingPermissions      []PendingPermissionStatus `json:"pendingPermissions"`
	Pressure                PressureMetadata          `json:"pressure"`
	SessionIdentityCapacity *SessionIdentityCapacity  `json:"sessionIdentityCapacity,omitempty"`
	Timestamp               time.Time                 `json:"timestamp"`
}

//nolint:gocyclo // The explicit state-machine branches are easier to audit together.
func (r StatusResponse) Validate() error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Fence.Validate(false); err != nil {
		return fmt.Errorf("fence: %w", err)
	}
	switch r.Lifecycle {
	case SupervisorLifecycleBooting, SupervisorLifecycleReady, SupervisorLifecycleDraining,
		SupervisorLifecycleTerminating, SupervisorLifecycleUnhealthy:
	default:
		return fmt.Errorf("unsupported supervisor lifecycle %q", r.Lifecycle)
	}
	if r.Fence.RuntimeSessionUID != "" || r.Fence.RuntimeSessionGeneration != 0 {
		return fmt.Errorf("status response must use a pool-wide fence without runtime session identity")
	}
	if r.SessionIdentityCapacity != nil {
		if err := r.SessionIdentityCapacity.Validate(); err != nil {
			return err
		}
	}
	if r.Drain.Requested {
		if r.Drain.AcceptingNewSessions {
			return fmt.Errorf("draining supervisor must not accept new sessions")
		}
		if err := validateTimestamp("drain request timestamp", r.Drain.RequestedAt); err != nil {
			return err
		}
		if r.Lifecycle != SupervisorLifecycleDraining && r.Lifecycle != SupervisorLifecycleTerminating {
			return fmt.Errorf("requested drain requires draining or terminating lifecycle")
		}
	} else {
		if !r.Drain.RequestedAt.IsZero() || r.Drain.Reason != "" {
			return fmt.Errorf("inactive drain must not carry request metadata")
		}
		if r.Lifecycle == SupervisorLifecycleDraining {
			return fmt.Errorf("draining lifecycle requires requested drain")
		}
	}
	if len(r.Sessions) > 10_000 || len(r.ActivePrompts) > 10_000 || len(r.PendingPermissions) > 10_000 {
		return fmt.Errorf("status response exceeds bounded item count")
	}
	sessionsByUID := make(map[RuntimeSessionUID]RuntimeSessionStatus, len(r.Sessions))
	for i := range r.Sessions {
		if err := r.Sessions[i].Validate(); err != nil {
			return fmt.Errorf("session status %d: %w", i, err)
		}
		if r.Sessions[i].PendingPermissionCount > 0 && r.Sessions[i].State != RuntimeSessionStatePromptRunning {
			return fmt.Errorf("session %q has pending permissions outside prompt_running", r.Sessions[i].RuntimeSessionUID)
		}
		if _, ok := sessionsByUID[r.Sessions[i].RuntimeSessionUID]; ok {
			return fmt.Errorf("duplicate session UID %q", r.Sessions[i].RuntimeSessionUID)
		}
		sessionsByUID[r.Sessions[i].RuntimeSessionUID] = r.Sessions[i]
	}
	activeBySession := make(map[RuntimeSessionUID]ActivePromptStatus, len(r.ActivePrompts))
	for i := range r.ActivePrompts {
		if err := r.ActivePrompts[i].Validate(); err != nil {
			return fmt.Errorf("active prompt %d: %w", i, err)
		}
		session, ok := sessionsByUID[r.ActivePrompts[i].RuntimeSessionUID]
		if !ok {
			return fmt.Errorf("active prompt %d references unknown session UID %q", i, r.ActivePrompts[i].RuntimeSessionUID)
		}
		if session.State != RuntimeSessionStatePromptRunning || session.ActivePromptID != r.ActivePrompts[i].PromptID || session.Generation != r.ActivePrompts[i].SessionGeneration {
			return fmt.Errorf("active prompt %d does not match session status", i)
		}
		if _, ok := activeBySession[r.ActivePrompts[i].RuntimeSessionUID]; ok {
			return fmt.Errorf("multiple active prompts for session UID %q", r.ActivePrompts[i].RuntimeSessionUID)
		}
		activeBySession[r.ActivePrompts[i].RuntimeSessionUID] = r.ActivePrompts[i]
	}
	for i := range r.PendingPermissions {
		if err := r.PendingPermissions[i].Validate(); err != nil {
			return fmt.Errorf("pending permission %d: %w", i, err)
		}
		active, ok := activeBySession[r.PendingPermissions[i].RuntimeSessionUID]
		if !ok || active.PromptID != r.PendingPermissions[i].PromptID {
			return fmt.Errorf("pending permission %d does not match an active prompt", i)
		}
	}
	if uint32(len(r.Sessions)) != r.Pressure.ResidentSessions {
		return fmt.Errorf("pressure resident session count does not match session list")
	}
	if uint32(len(r.ActivePrompts)) != r.Pressure.ActivePrompts {
		return fmt.Errorf("pressure active prompt count does not match prompt list")
	}
	if uint32(len(r.PendingPermissions)) != r.Pressure.PendingPermissions {
		return fmt.Errorf("pressure pending permission count does not match permission list")
	}
	return validateTimestamp("status timestamp", r.Timestamp)
}

type ErrorCode string

const (
	ErrorCodeInvalidRequest      ErrorCode = "invalid_request"
	ErrorCodeUnauthenticated     ErrorCode = "unauthenticated"
	ErrorCodeForbidden           ErrorCode = "forbidden"
	ErrorCodeExpired             ErrorCode = "expired"
	ErrorCodeStaleFence          ErrorCode = "stale_fence"
	ErrorCodeDigestConflict      ErrorCode = "digest_conflict"
	ErrorCodeAlreadyAccepted     ErrorCode = "already_accepted"
	ErrorCodeSettled             ErrorCode = "settled"
	ErrorCodeRateLimited         ErrorCode = "rate_limited"
	ErrorCodeSessionPoisoned     ErrorCode = "session_poisoned"
	ErrorCodeWorkspaceResumeLost ErrorCode = "workspace_resume_lost"
	ErrorCodeOutcomeUnknown      ErrorCode = "outcome_unknown"
)

type ErrorResponse struct {
	Protocol       string          `json:"protocol"`
	Code           ErrorCode       `json:"code"`
	Message        string          `json:"message"`
	Classification *Classification `json:"classification,omitempty"`
	Retryable      bool            `json:"retryable"`
}

func (r ErrorResponse) Validate() error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	switch r.Code {
	case ErrorCodeInvalidRequest, ErrorCodeUnauthenticated, ErrorCodeForbidden, ErrorCodeExpired,
		ErrorCodeStaleFence, ErrorCodeDigestConflict, ErrorCodeAlreadyAccepted, ErrorCodeSettled,
		ErrorCodeRateLimited, ErrorCodeSessionPoisoned, ErrorCodeWorkspaceResumeLost, ErrorCodeOutcomeUnknown:
	default:
		return fmt.Errorf("unsupported error code %q", r.Code)
	}
	if err := validateBoundedString("error message", r.Message, true, MaxDiagnosticBytes); err != nil {
		return err
	}
	if r.Classification != nil {
		if err := r.Classification.Validate(); err != nil {
			return fmt.Errorf("classification: %w", err)
		}
	}
	if (r.Code == ErrorCodeOutcomeUnknown || r.Code == ErrorCodeWorkspaceResumeLost) && r.Retryable {
		return fmt.Errorf("%s must never be retryable", r.Code)
	}
	return nil
}

func validateUniqueStrings(name string, values []string, max int) error {
	if len(values) > max {
		return fmt.Errorf("%s count exceeds %d", name, max)
	}
	seen := make(map[string]struct{}, len(values))
	for i, value := range values {
		value = strings.TrimSpace(value)
		if err := validateBoundedString(name, value, true, 256); err != nil {
			return fmt.Errorf("%s %d: %w", name, i, err)
		}
		if _, ok := seen[value]; ok {
			return fmt.Errorf("duplicate %s %q", name, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}
