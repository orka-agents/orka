package workspaceagent

import (
	"net/url"
	"strings"
	"time"
)

const (
	// ProtocolVersion is carried by every versioned workspace-agent DTO.
	ProtocolVersion = "workspace.orka.ai/v1"

	HealthPath              = "/v1/health"
	LegacyHealthPath        = "/healthz"
	CapabilitiesPath        = "/v1/capabilities"
	AttachmentControlPath   = "/v1/control/attachment"
	AttachmentControlPrefix = AttachmentControlPath + "/"
	ExecPath                = "/v1/exec"
	ExecStatusPrefix        = ExecPath + "/"
	FilesPath               = "/v1/files"
	FilesDownloadPath       = "/v1/files/download"
	ScrubPath               = "/v1/scrub"
	ResetPath               = "/v1/reset"

	WorkspaceUIDHeader    = "X-Orka-Workspace-UID"
	AttachmentEpochHeader = "X-Orka-Workspace-Epoch"
)

// Versioned is embedded in all request and response bodies.
type Versioned struct {
	ProtocolVersion string `json:"protocolVersion"`
}

// NewVersioned returns a protocol marker for new requests.
func NewVersioned() Versioned {
	return Versioned{ProtocolVersion: ProtocolVersion}
}

// HealthResponse reports basic liveness without secret data.
type HealthResponse struct {
	Versioned
	Status string `json:"status"`
}

// CapabilitiesResponse advertises bounded protocol capabilities.
type CapabilitiesResponse struct {
	Versioned
	Features                []string `json:"features"`
	MaxRequestBytes         int64    `json:"maxRequestBytes"`
	MaxOutputBytes          int64    `json:"maxOutputBytes"`
	MaxDownloadBytes        int64    `json:"maxDownloadBytes"`
	OperationRetentionSec   int64    `json:"operationRetentionSeconds"`
	MaxRetainedOperations   int      `json:"maxRetainedOperations"`
	MaxOperationIDsPerEpoch int      `json:"maxOperationIDsPerEpoch"`
	BindingGeneration       string   `json:"bindingGeneration"`
}

// AttachmentControlRequest activates one exclusive attachment epoch. TokenSHA256
// is the digest of the bearer token; the raw token is never sent on this endpoint.
type AttachmentControlRequest struct {
	Versioned
	WorkspaceUID      string    `json:"workspaceUID"`
	BindingGeneration string    `json:"bindingGeneration"`
	TaskUID           string    `json:"taskUID"`
	Epoch             int64     `json:"epoch"`
	TokenSHA256       string    `json:"tokenSHA256"`
	ExpiresAt         time.Time `json:"expiresAt"`
}

// SetTokenDigest sets the SHA-256 attachment bearer digest without exposing the
// raw bearer on the privileged control request.
func (r *AttachmentControlRequest) SetTokenDigest(value string) {
	r.TokenSHA256 = value
}

// AttachmentRevocationRequest identifies the exact workspace binding being revoked.
type AttachmentRevocationRequest struct {
	Versioned
	WorkspaceUID      string `json:"workspaceUID"`
	BindingGeneration string `json:"bindingGeneration"`
}

// AttachmentControlResponse reports the active epoch after activation or revocation.
type AttachmentControlResponse struct {
	Versioned
	WorkspaceUID      string `json:"workspaceUID,omitempty"`
	BindingGeneration string `json:"bindingGeneration"`
	ActiveEpoch       int64  `json:"activeEpoch,omitempty"`
	Active            bool   `json:"active"`
}

// AttachmentCredentials are supplied by a Task worker for data-plane requests.
type AttachmentCredentials struct {
	WorkspaceUID string
	Epoch        int64
	Bearer       string
}

// OperationState is the durable state of an idempotent exec operation.
type OperationState string

const (
	OperationStateRunning   OperationState = "Running"
	OperationStateSucceeded OperationState = "Succeeded"
	OperationStateFailed    OperationState = "Failed"
	OperationStateCancelled OperationState = "Cancelled"
)

// ExecRequest starts an idempotent command operation. OperationID is caller-supplied.
type ExecRequest struct {
	Versioned
	OperationID    string            `json:"operationID,omitempty"`
	Command        []string          `json:"command"`
	Env            map[string]string `json:"env,omitempty"`
	WorkDir        string            `json:"workDir,omitempty"`
	Stdin          []byte            `json:"stdin,omitempty"`
	TimeoutSeconds int64             `json:"timeoutSeconds,omitempty"`
	MaxOutputBytes int64             `json:"maxOutputBytes,omitempty"`

	// Detach is retained for legacy callers. New callers use OperationID and poll.
	Detach      bool   `json:"detach,omitempty"`
	Resident    bool   `json:"resident,omitempty"`
	ResidentKey string `json:"residentKey,omitempty"`
}

// ExecResponse is retained for the bounded operation result window.
type ExecResponse struct {
	Versioned
	OperationID     string         `json:"operationID,omitempty"`
	State           OperationState `json:"state,omitempty"`
	Stdout          string         `json:"stdout"`
	Stderr          string         `json:"stderr"`
	ExitCode        int            `json:"exitCode"`
	StdoutTruncated bool           `json:"stdoutTruncated"`
	StderrTruncated bool           `json:"stderrTruncated"`
	// IsolationFailed reports that post-command process isolation cleanup failed.
	// The command State and ExitCode remain the workload outcome so callers never
	// replay a successful command because finalization degraded.
	IsolationFailed bool      `json:"isolationFailed,omitempty"`
	StartedAt       time.Time `json:"startedAt"`
	FinishedAt      time.Time `json:"finishedAt,omitempty"`

	// ExecID and Running retain wire compatibility with the legacy private protocol.
	ExecID  string `json:"execId,omitempty"`
	Running bool   `json:"running,omitempty"`
}

// CancelResponse reports the operation state after a cancellation request.
type CancelResponse struct {
	Versioned
	OperationID string         `json:"operationID"`
	State       OperationState `json:"state"`
}

// UploadRequest writes bounded files. Repeating the same path and digest is idempotent.
type UploadRequest struct {
	Versioned
	Files []UploadFile `json:"files"`
}

// UploadFile is one requested file write.
type UploadFile struct {
	Path    string    `json:"path"`
	Data    []byte    `json:"data"`
	Digest  string    `json:"digest,omitempty"`
	Mode    uint32    `json:"mode,omitempty"`
	ModTime time.Time `json:"modTime,omitempty"`
}

// UploadResponse reports safe artifact metadata.
type UploadResponse struct {
	Versioned
	Artifacts []Artifact `json:"artifacts"`
}

// DownloadRequest reads selected files; an empty list requests the workspace tree.
type DownloadRequest struct {
	Versioned
	Paths []string `json:"paths,omitempty"`
}

// DownloadResponse contains requested file bytes and metadata.
type DownloadResponse struct {
	Versioned
	Artifacts []DownloadedArtifact `json:"artifacts"`
}

// ScrubRequest idempotently removes selected paths.
type ScrubRequest struct {
	Versioned
	WorkspaceUID      string   `json:"workspaceUID"`
	BindingGeneration string   `json:"bindingGeneration"`
	Paths             []string `json:"paths"`
}

// ScrubResponse acknowledges an idempotent scrub.
type ScrubResponse struct {
	Versioned
	Scrubbed bool `json:"scrubbed"`
}

// ResetRequest idempotently resets the workspace roots selected by the provider.
type ResetRequest struct {
	Versioned
	OperationID       string   `json:"operationID"`
	WorkspaceUID      string   `json:"workspaceUID"`
	BindingGeneration string   `json:"bindingGeneration"`
	Paths             []string `json:"paths,omitempty"`
}

// ResetResponse acknowledges an idempotent reset.
type ResetResponse struct {
	Versioned
	Reset             bool   `json:"reset"`
	BindingGeneration string `json:"bindingGeneration"`
}

// Artifact is safe metadata for one workspace file.
type Artifact struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	Digest  string    `json:"digest"`
	Mode    uint32    `json:"mode"`
	ModTime time.Time `json:"modTime"`
}

// DownloadedArtifact includes file bytes.
type DownloadedArtifact struct {
	Artifact
	Data []byte `json:"data"`
}

// ExecStatusPath returns the escaped status endpoint for operationID.
func ExecStatusPath(operationID string) string {
	return ExecStatusPrefix + url.PathEscape(strings.TrimSpace(operationID))
}

// ExecCancelPath returns the escaped cancellation endpoint for operationID.
func ExecCancelPath(operationID string) string {
	return ExecStatusPath(operationID) + "/cancel"
}

// AttachmentRevocationPath returns the escaped revocation endpoint for epoch.
func AttachmentRevocationPath(epoch string) string {
	return AttachmentControlPrefix + url.PathEscape(strings.TrimSpace(epoch))
}
