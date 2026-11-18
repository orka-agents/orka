package v2

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/operatoraccess"
)

const (
	// OperatorExecPathTemplate upgrades to a WebSocket that streams one
	// operator command running as the RuntimeSession's child identity.
	OperatorExecPathTemplate = "/v2/runtime-sessions/{sessionID}/operator-exec"
	// OperatorFilesPathTemplate reads (GET) or writes (PUT) one regular file
	// beneath the RuntimeSession workspace as the child identity.
	OperatorFilesPathTemplate = "/v2/runtime-sessions/{sessionID}/operator-files"
	// OperatorRequestHeader carries the base64url canonical JSON request so
	// the supervisor authenticates the complete operation on headers alone,
	// before it upgrades the connection or reads a body.
	OperatorRequestHeader = "X-Orka-Operator-Request"
	// MaxOperatorRequestHeaderBytes bounds the encoded request header.
	MaxOperatorRequestHeaderBytes = 16 << 10
	// OperatorCapabilityTTL bounds how long a signed operator request may be
	// presented. The operation itself is bounded by its own timeout.
	OperatorCapabilityTTL = 2 * time.Minute
)

// OperatorExecPath returns the exec route for a RuntimeSession.
func OperatorExecPath(sessionID RuntimeSessionID) (string, error) {
	sessionPath, err := RuntimeSessionPath(sessionID)
	if err != nil {
		return "", err
	}
	return sessionPath + "/operator-exec", nil
}

// OperatorFilesPath returns the file route for a RuntimeSession.
func OperatorFilesPath(sessionID RuntimeSessionID) (string, error) {
	sessionPath, err := RuntimeSessionPath(sessionID)
	if err != nil {
		return "", err
	}
	return sessionPath + "/operator-files", nil
}

// OperatorExecRequest is the signed exec request.
type OperatorExecRequest struct {
	Protocol string                     `json:"protocol"`
	Metadata MutationMetadata           `json:"metadata"`
	AuditID  string                     `json:"auditID"`
	Exec     operatoraccess.ExecOptions `json:"exec"`
}

// OperatorFileRequest is the signed file request.
type OperatorFileRequest struct {
	Protocol string           `json:"protocol"`
	Metadata MutationMetadata `json:"metadata"`
	AuditID  string           `json:"auditID"`
	Path     string           `json:"path"`
	Write    bool             `json:"write,omitempty"`
	MaxBytes int64            `json:"maxBytes"`
	// Size is the exact upload length. A write commits only when exactly
	// Size bytes arrive, so a truncated upload never replaces the target.
	Size int64 `json:"size,omitempty"`
}

// OperatorFileResult is the JSON response of a successful write.
type OperatorFileResult struct {
	Protocol string `json:"protocol"`
	Bytes    int64  `json:"bytes"`
}

func validateOperatorEnvelope(protocol string, metadata MutationMetadata, auditID string, now time.Time) error {
	if err := validateProtocol(protocol); err != nil {
		return err
	}
	if err := metadata.validateAt(now, metadataRequirements{session: true}); err != nil {
		return err
	}
	if err := requireIdentifier("audit ID", auditID); err != nil {
		return err
	}
	if metadata.ExpiresAt.After(now.Add(OperatorCapabilityTTL + time.Minute)) {
		return fmt.Errorf("operator request expiry exceeds the capability bound")
	}
	return nil
}

// ValidateAt validates and clamps the request.
func (r *OperatorExecRequest) ValidateAt(now time.Time) error {
	if err := validateOperatorEnvelope(r.Protocol, r.Metadata, r.AuditID, now); err != nil {
		return err
	}
	return r.Exec.Normalize()
}

// ValidateAt validates and clamps the request.
func (r *OperatorFileRequest) ValidateAt(now time.Time) error {
	if err := validateOperatorEnvelope(r.Protocol, r.Metadata, r.AuditID, now); err != nil {
		return err
	}
	cleaned, err := operatoraccess.CleanWorkspacePath(r.Path)
	if err != nil {
		return err
	}
	if cleaned != r.Path {
		return fmt.Errorf("operator file path must be canonical")
	}
	if r.MaxBytes <= 0 || r.MaxBytes > operatoraccess.MaxFileBytes {
		return fmt.Errorf("operator file size bound is invalid")
	}
	if r.Size < 0 || r.Size > r.MaxBytes || (!r.Write && r.Size != 0) {
		return fmt.Errorf("operator file size is invalid")
	}
	return nil
}

// EncodeOperatorRequestHeader canonicalizes and encodes a request header.
func EncodeOperatorRequestHeader(request any) (string, error) {
	raw, err := CanonicalValue(request)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	if len(encoded) > MaxOperatorRequestHeaderBytes {
		return "", fmt.Errorf("operator request exceeds %d bytes", MaxOperatorRequestHeaderBytes)
	}
	return encoded, nil
}

// DecodeOperatorRequestHeader decodes a request header strictly and verifies
// that metadata.requestDigest is the canonical digest of the decoded request.
func DecodeOperatorRequestHeader(header string, target any) error {
	header = strings.TrimSpace(header)
	if header == "" || len(header) > MaxOperatorRequestHeaderBytes {
		return fmt.Errorf("operator request header is missing or too large")
	}
	raw, err := base64.RawURLEncoding.DecodeString(header)
	if err != nil {
		return fmt.Errorf("operator request header is not base64url")
	}
	digest, err := CanonicalRequestDigestJSON(raw)
	if err != nil {
		return fmt.Errorf("operator request is not canonical JSON: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode operator request: %w", err)
	}
	var metadata struct {
		Metadata MutationMetadata `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return fmt.Errorf("decode operator request metadata: %w", err)
	}
	if metadata.Metadata.RequestDigest != digest {
		return fmt.Errorf("operator request digest does not match its content")
	}
	return nil
}

// SealOperatorRequest stamps metadata.requestDigest and signs the request.
// It returns the encoded header and the capability token.
func SealOperatorRequest(secret []byte, request any, metadata *MutationMetadata) (string, string, error) {
	metadata.RequestDigest = ""
	metadata.RequestDigestSchemaVersion = RequestDigestSchemaVersion
	digest, err := CanonicalRequestDigest(request)
	if err != nil {
		return "", "", err
	}
	metadata.RequestDigest = digest
	header, err := EncodeOperatorRequestHeader(request)
	if err != nil {
		return "", "", err
	}
	token, err := SignOperationCapability(secret, ClaimsForMutation(*metadata))
	if err != nil {
		return "", "", err
	}
	return header, token, nil
}
