package v2

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// NativeSessionLimitsHeader opts into the additive capability limit. Older
// clients strictly decode capabilities and must not receive unknown fields.
const NativeSessionLimitsHeader = "X-Orka-Native-Session-Limits"

const (
	// DefaultMaxNativeSessionBytes is the default encoded bundle policy.
	DefaultMaxNativeSessionBytes = 8 << 20
	// MaxNativeSessionBytes is the absolute supported encoded bundle ceiling.
	MaxNativeSessionBytes = 64 << 20
	// LegacyMaxNativeSessionBytes applies when an older runtime advertises no cap.
	LegacyMaxNativeSessionBytes = 512 << 10
)

// NormalizeNativeSessionMaxBytes resolves an omitted policy and rejects unsafe limits.
func NormalizeNativeSessionMaxBytes(limit int) (int, error) {
	if limit == 0 {
		return DefaultMaxNativeSessionBytes, nil
	}
	if limit < 1 || limit > MaxNativeSessionBytes {
		return 0, fmt.Errorf("native session max bytes must be in range 1..%d", MaxNativeSessionBytes)
	}
	return limit, nil
}

// NativeSessionJSONLimit allows base64 bundle encoding plus bounded control metadata.
// This exemption is only for native restore and capture, not ordinary control JSON.
func NativeSessionJSONLimit(limit int) int {
	return 2*limit + MaxCanonicalJSONBytes
}

// NativeSessionSnapshot contains private provider conversation state. It must
// never be included in Task status, events, or diagnostic logging.
type NativeSessionSnapshot struct {
	Data                 []byte            `json:"data"`
	DataDigest           string            `json:"dataDigest"`
	ProviderSessionID    string            `json:"providerSessionID"`
	ProviderKind         string            `json:"providerKind"`
	ProviderVersion      string            `json:"providerVersion"`
	RuntimeSessionUID    RuntimeSessionUID `json:"runtimeSessionUID"`
	RuntimeProfileDigest ProfileDigest     `json:"runtimeProfileDigest"`
	WorkingDirectory     string            `json:"workingDirectory"`
}

func (s NativeSessionSnapshot) Validate() error {
	if len(s.Data) == 0 || len(s.Data) > MaxNativeSessionBytes {
		return fmt.Errorf("native session data must be in range 1..%d bytes", MaxNativeSessionBytes)
	}
	digest := sha256.Sum256(s.Data)
	if s.DataDigest != "sha256:"+hex.EncodeToString(digest[:]) {
		return fmt.Errorf("native session data digest does not match data")
	}
	if s.ProviderKind != providerKindCodex {
		return fmt.Errorf("native session provider must be codex")
	}
	id, err := uuid.Parse(s.ProviderSessionID)
	if err != nil || id == uuid.Nil || id.String() != s.ProviderSessionID {
		return fmt.Errorf("native provider session ID must be a canonical UUID")
	}
	if err := validateBoundedString("native provider version", s.ProviderVersion, true, 128); err != nil {
		return err
	}
	if err := requireIdentifier("native runtime session UID", string(s.RuntimeSessionUID)); err != nil {
		return err
	}
	if err := ValidateProfileDigest(s.RuntimeProfileDigest); err != nil {
		return err
	}
	if err := validateBoundedString("native working directory", s.WorkingDirectory, true, MaxProtocolStringBytes); err != nil {
		return err
	}
	if !filepath.IsAbs(s.WorkingDirectory) || filepath.Clean(s.WorkingDirectory) != s.WorkingDirectory {
		return fmt.Errorf("native working directory must be an absolute clean path")
	}
	return nil
}

type NativeSessionRestore struct {
	Snapshot NativeSessionSnapshot `json:"snapshot"`
}

func (r NativeSessionRestore) ValidateFor(request CreateRuntimeSessionRequest) error {
	if err := r.Snapshot.Validate(); err != nil {
		return err
	}
	if request.Profile.ProviderKind != r.Snapshot.ProviderKind ||
		request.Metadata.Fence.RuntimeSessionUID != r.Snapshot.RuntimeSessionUID ||
		request.Metadata.Fence.RuntimeProfileDigest != r.Snapshot.RuntimeProfileDigest {
		return fmt.Errorf("native restore does not match the current runtime session fence and provider")
	}
	if request.Bootstrap != nil || request.BootstrapArtifactAuthorization != nil {
		return fmt.Errorf("native restore cannot be combined with transcript bootstrap")
	}
	return nil
}

type NativeSessionRestoration struct {
	DataDigest        string `json:"dataDigest"`
	ProviderSessionID string `json:"providerSessionID"`
	Loaded            bool   `json:"loaded"`
}

func (r NativeSessionRestoration) Validate() error {
	if err := validateSHA256Digest(r.DataDigest); err != nil {
		return err
	}
	if !r.Loaded {
		return fmt.Errorf("native restoration must confirm ACP session load")
	}
	id, err := uuid.Parse(r.ProviderSessionID)
	if err != nil || id == uuid.Nil || id.String() != r.ProviderSessionID {
		return fmt.Errorf("native restoration provider session ID must be a canonical UUID")
	}
	return nil
}

type CaptureNativeSessionRequest struct {
	Protocol string           `json:"protocol"`
	Metadata MutationMetadata `json:"metadata"`
	// These fields reconcile one known capture under fresh current authority.
	// A reconciliation never starts another capture if the original is absent.
	OriginalOperationID   OperationID   `json:"originalOperationID,omitempty"`
	OriginalRequestDigest RequestDigest `json:"originalRequestDigest,omitempty"`
}

func (r CaptureNativeSessionRequest) ValidateAt(now time.Time) error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Metadata.validateAt(now, metadataRequirements{session: true, task: true}); err != nil {
		return err
	}
	if (r.OriginalOperationID == "") != (r.OriginalRequestDigest == "") {
		return fmt.Errorf("native capture reconciliation requires both original operation ID and request digest")
	}
	if r.OriginalOperationID != "" {
		if err := requireIdentifier("original native capture operation ID", string(r.OriginalOperationID)); err != nil {
			return err
		}
		if err := validateSHA256Digest(string(r.OriginalRequestDigest)); err != nil {
			return err
		}
		if r.OriginalOperationID == r.Metadata.OperationID {
			return fmt.Errorf("native capture reconciliation requires a distinct current operation ID")
		}
	}
	return r.Metadata.ValidateDigest(r)
}

type CaptureNativeSessionResponse struct {
	Protocol       string                   `json:"protocol"`
	Classification Classification           `json:"classification"`
	Session        RuntimeSessionDescriptor `json:"session"`
	Snapshot       NativeSessionSnapshot    `json:"snapshot"`
}

func (r CaptureNativeSessionResponse) ValidateFor(request CaptureNativeSessionRequest) error {
	if err := validateProtocol(r.Protocol); err != nil {
		return err
	}
	if err := r.Classification.Validate(); err != nil {
		return err
	}
	if r.Classification.Class != RequestClassificationFresh && r.Classification.Class != RequestClassificationDuplicate {
		return fmt.Errorf("native capture response classification is invalid")
	}
	if err := r.Session.Validate(); err != nil {
		return err
	}
	if err := r.Snapshot.Validate(); err != nil {
		return err
	}
	fence := request.Metadata.Fence
	if r.Session.RuntimeSessionUID != fence.RuntimeSessionUID || r.Session.Generation != fence.RuntimeSessionGeneration ||
		r.Session.RuntimeProfileDigest != fence.RuntimeProfileDigest || r.Session.RuntimeInstanceID != fence.RuntimeInstanceID ||
		r.Session.SupervisorBootID != fence.SupervisorBootID || r.Session.State != RuntimeSessionStatePoisoned ||
		r.Snapshot.RuntimeSessionUID != r.Session.RuntimeSessionUID || r.Snapshot.RuntimeProfileDigest != r.Session.RuntimeProfileDigest ||
		r.Snapshot.ProviderSessionID != r.Session.ProviderSessionID {
		return fmt.Errorf("native capture response does not match the stopped current session")
	}
	return nil
}

func RuntimeSessionNativeSessionPath(sessionID RuntimeSessionID) (string, error) {
	session, err := RuntimeSessionPath(sessionID)
	if err != nil {
		return "", err
	}
	return session + "/native-session", nil
}
