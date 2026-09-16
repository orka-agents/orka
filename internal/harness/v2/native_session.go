package v2

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
)

// NativeSessionMaxBytes bounds the compressed, private conversation payload.
// It fits the existing request limit, including JSON's base64 expansion.
const NativeSessionMaxBytes = 512 << 10

// NativeSessionSnapshot contains only provider conversation data. It is never
// a workspace artifact and must not be exposed by public artifact APIs.
type NativeSessionSnapshot struct {
	SessionUID           RuntimeSessionUID `json:"sessionUID"`
	ProviderKind         string            `json:"providerKind"`
	ProviderVersion      string            `json:"providerVersion"`
	ProviderSessionID    string            `json:"providerSessionID"`
	ProfileDigest        ProfileDigest     `json:"profileDigest"`
	WorkingDirectory     string            `json:"workingDirectory"`
	WorkspaceStateDigest string            `json:"workspaceStateDigest"`
	DataDigest           string            `json:"dataDigest"`
	Data                 []byte            `json:"data"`
}

func (s NativeSessionSnapshot) Validate() error {
	if err := requireIdentifier("native Session UID", string(s.SessionUID)); err != nil {
		return err
	}
	for field, value := range map[string]string{
		"native provider": s.ProviderKind, "native provider version": s.ProviderVersion,
		"native provider session ID": s.ProviderSessionID,
	} {
		if err := validateBoundedString(field, value, true, 256); err != nil {
			return err
		}
	}
	if err := ValidateProfileDigest(s.ProfileDigest); err != nil {
		return err
	}
	if err := validateBoundedString("native working directory", s.WorkingDirectory, true, 4096); err != nil {
		return err
	}
	if !path.IsAbs(s.WorkingDirectory) || path.Clean(s.WorkingDirectory) != s.WorkingDirectory {
		return fmt.Errorf("native working directory must be an absolute, clean path")
	}
	if err := validateSHA256Digest(s.WorkspaceStateDigest); err != nil {
		return fmt.Errorf("native workspace state digest: %w", err)
	}
	if len(s.Data) == 0 || len(s.Data) > NativeSessionMaxBytes {
		return fmt.Errorf("native session data exceeds its size bounds")
	}
	if NativeSessionDataDigest(s.Data) != s.DataDigest {
		return fmt.Errorf("native session data digest does not match")
	}
	return nil
}

func NativeSessionDataDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// NativeSessionRestore authorizes one immutable saved copy in the existing
// create operation. Its complete contents are covered by the request digest.
type NativeSessionRestore struct {
	SnapshotID string                `json:"snapshotID"`
	Snapshot   NativeSessionSnapshot `json:"snapshot"`
}

func (r NativeSessionRestore) validateFor(request CreateRuntimeSessionRequest) error {
	if err := requireIdentifier("native snapshot ID", r.SnapshotID); err != nil {
		return err
	}
	if err := r.Snapshot.Validate(); err != nil {
		return err
	}
	if r.Snapshot.SessionUID != request.Metadata.Fence.RuntimeSessionUID ||
		r.Snapshot.ProfileDigest != request.Metadata.Fence.RuntimeProfileDigest ||
		r.Snapshot.ProviderKind != request.Profile.ProviderKind {
		return fmt.Errorf("native restore identity does not match the session request")
	}
	if request.Workspace.Intent != WorkspaceIntentRead || request.Bootstrap != nil {
		return fmt.Errorf("native restore requires read intent and no transcript bootstrap")
	}
	return nil
}

// NativeSessionRestoration reports recovery without exposing conversation data.
type NativeSessionRestoration struct {
	SnapshotID string `json:"snapshotID,omitempty"`
	Method     string `json:"method"`
	Reason     string `json:"reason,omitempty"`
}

func (r NativeSessionRestoration) Validate() error {
	switch r.Method {
	case "session/resume", "session/load":
		if err := requireIdentifier("native snapshot ID", r.SnapshotID); err != nil {
			return err
		}
	case "reconstructed":
	default:
		return fmt.Errorf("unsupported native restoration method")
	}
	return validateBoundedString("native restoration reason", r.Reason, false, 128)
}
