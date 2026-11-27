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
	OperatorExecPathTemplate      = "/v2/runtime-sessions/{sessionID}/operator-exec"
	OperatorFilesPathTemplate     = "/v2/runtime-sessions/{sessionID}/operator-files"
	OperatorRequestHeader         = "X-Orka-Operator-Request"
	MaxOperatorRequestHeaderBytes = 16 << 10
	OperatorCapabilityTTL         = 2 * time.Minute
)

func OperatorExecPath(sessionID RuntimeSessionID) (string, error) {
	sessionPath, err := RuntimeSessionPath(sessionID)
	if err != nil {
		return "", err
	}
	return sessionPath + "/operator-exec", nil
}

func OperatorFilesPath(sessionID RuntimeSessionID) (string, error) {
	sessionPath, err := RuntimeSessionPath(sessionID)
	if err != nil {
		return "", err
	}
	return sessionPath + "/operator-files", nil
}

type OperatorExecRequest struct {
	Protocol string                     `json:"protocol"`
	Metadata MutationMetadata           `json:"metadata"`
	AuditID  string                     `json:"auditID"`
	Exec     operatoraccess.ExecOptions `json:"exec"`
}

type OperatorFileRequest struct {
	Protocol string           `json:"protocol"`
	Metadata MutationMetadata `json:"metadata"`
	AuditID  string           `json:"auditID"`
	Path     string           `json:"path"`
	Write    bool             `json:"write,omitempty"`
	MaxBytes int64            `json:"maxBytes"`
	Size     int64            `json:"size,omitempty"`
}

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

func (r *OperatorExecRequest) ValidateAt(now time.Time) error {
	if err := validateOperatorEnvelope(r.Protocol, r.Metadata, r.AuditID, now); err != nil {
		return err
	}
	return r.Exec.Normalize()
}

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
