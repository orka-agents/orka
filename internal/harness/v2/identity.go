package v2

import (
	"fmt"
	"strings"
	"time"
)

type RuntimeInstanceID string
type SupervisorBootID string
type RuntimePoolUID string
type RuntimeSessionID string
type RuntimeSessionUID string
type TaskUID string
type PromptID string
type OperationID string
type PermissionRequestID string
type WorkspaceDeltaID string
type ArtifactID string
type RequestDigest string
type ProfileDigest string

const (
	RequestDigestSchemaVersion uint32 = 1
	ProfileDigestSchemaVersion uint32 = 1
)

// Fence binds a request to one exact supervisor boot, controller leadership
// epoch, pool generation, runtime-session generation, and immutable runtime
// profile. Runtime-session fields are omitted only for pool-wide operations such
// as drain.
type Fence struct {
	RuntimeInstanceID          RuntimeInstanceID `json:"runtimeInstanceID"`
	SupervisorBootID           SupervisorBootID  `json:"supervisorBootID"`
	ControllerEpoch            uint64            `json:"controllerEpoch"`
	RuntimePoolUID             RuntimePoolUID    `json:"runtimePoolUID"`
	RuntimePoolGeneration      uint64            `json:"runtimePoolGeneration"`
	RuntimeSessionUID          RuntimeSessionUID `json:"runtimeSessionUID,omitempty"`
	RuntimeSessionGeneration   uint64            `json:"runtimeSessionGeneration,omitempty"`
	RuntimeProfileDigest       ProfileDigest     `json:"runtimeProfileDigest"`
	ProfileDigestSchemaVersion uint32            `json:"profileDigestSchemaVersion"`
}

func (f Fence) Validate(requireSession bool) error {
	if err := requireIdentifier("runtime instance ID", string(f.RuntimeInstanceID)); err != nil {
		return err
	}
	if err := requireIdentifier("supervisor boot ID", string(f.SupervisorBootID)); err != nil {
		return err
	}
	if f.ControllerEpoch == 0 {
		return fmt.Errorf("controller epoch must be positive")
	}
	if err := requireIdentifier("runtime pool UID", string(f.RuntimePoolUID)); err != nil {
		return err
	}
	if f.RuntimePoolGeneration == 0 {
		return fmt.Errorf("runtime pool generation must be positive")
	}
	if requireSession {
		if err := requireIdentifier("runtime session UID", string(f.RuntimeSessionUID)); err != nil {
			return err
		}
		if f.RuntimeSessionGeneration == 0 {
			return fmt.Errorf("runtime session generation must be positive")
		}
	} else if (f.RuntimeSessionUID == "") != (f.RuntimeSessionGeneration == 0) {
		return fmt.Errorf("runtime session UID and generation must be supplied together")
	}
	if err := ValidateProfileDigest(f.RuntimeProfileDigest); err != nil {
		return fmt.Errorf("runtime profile digest: %w", err)
	}
	if f.ProfileDigestSchemaVersion != ProfileDigestSchemaVersion {
		return fmt.Errorf("profile digest schema version %d is unsupported; want %d", f.ProfileDigestSchemaVersion, ProfileDigestSchemaVersion)
	}
	return nil
}

// MutationMetadata is common to every mutating request. RequestDigest is the
// digest of the complete request with this field omitted, as produced by
// CanonicalRequestDigest.
type MutationMetadata struct {
	Fence                      Fence         `json:"fence"`
	TaskUID                    TaskUID       `json:"taskUID,omitempty"`
	TaskAttempt                uint32        `json:"taskAttempt,omitempty"`
	PromptID                   PromptID      `json:"promptID,omitempty"`
	OperationID                OperationID   `json:"operationID"`
	RequestDigestSchemaVersion uint32        `json:"requestDigestSchemaVersion"`
	RequestDigest              RequestDigest `json:"requestDigest"`
	ExpiresAt                  time.Time     `json:"expiresAt"`
}

type metadataRequirements struct {
	session bool
	task    bool
	prompt  bool
}

func (m MutationMetadata) ValidateAt(now time.Time) error {
	return m.validateAt(now, metadataRequirements{})
}

func (m MutationMetadata) validateAt(now time.Time, req metadataRequirements) error {
	if err := m.Fence.Validate(req.session); err != nil {
		return fmt.Errorf("fence: %w", err)
	}
	if req.task {
		if err := requireIdentifier("task UID", string(m.TaskUID)); err != nil {
			return err
		}
		if m.TaskAttempt == 0 {
			return fmt.Errorf("task attempt must be positive")
		}
	} else if (m.TaskUID == "") != (m.TaskAttempt == 0) {
		return fmt.Errorf("task UID and attempt must be supplied together")
	}
	if req.prompt {
		if !req.task {
			return fmt.Errorf("internal validation error: prompt metadata requires task metadata")
		}
		if err := requireIdentifier("prompt ID", string(m.PromptID)); err != nil {
			return err
		}
	} else if m.PromptID != "" && m.TaskUID == "" {
		return fmt.Errorf("prompt ID requires task identity")
	}
	if err := requireIdentifier("operation ID", string(m.OperationID)); err != nil {
		return err
	}
	if m.RequestDigestSchemaVersion != RequestDigestSchemaVersion {
		return fmt.Errorf("request digest schema version %d is unsupported; want %d", m.RequestDigestSchemaVersion, RequestDigestSchemaVersion)
	}
	if err := ValidateRequestDigest(m.RequestDigest); err != nil {
		return fmt.Errorf("request digest: %w", err)
	}
	if m.ExpiresAt.IsZero() {
		return fmt.Errorf("request expiry is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if !m.ExpiresAt.After(now) {
		return fmt.Errorf("request expired at %s", m.ExpiresAt.UTC().Format(time.RFC3339Nano))
	}
	return nil
}

func (m MutationMetadata) ValidateDigest(request any) error {
	got, err := CanonicalRequestDigest(request)
	if err != nil {
		return err
	}
	if got != m.RequestDigest {
		return fmt.Errorf("request digest mismatch: got %q, want %q", m.RequestDigest, got)
	}
	return nil
}

type FenceMismatch string

const (
	FenceMatch                            FenceMismatch = ""
	FenceMismatchRuntimeInstance          FenceMismatch = "runtime_instance"
	FenceMismatchSupervisorBoot           FenceMismatch = "supervisor_boot"
	FenceMismatchControllerEpoch          FenceMismatch = "controller_epoch"
	FenceMismatchRuntimePoolUID           FenceMismatch = "runtime_pool_uid"
	FenceMismatchRuntimePoolGeneration    FenceMismatch = "runtime_pool_generation"
	FenceMismatchRuntimeSessionUID        FenceMismatch = "runtime_session_uid"
	FenceMismatchRuntimeSessionGeneration FenceMismatch = "runtime_session_generation"
	FenceMismatchRuntimeProfile           FenceMismatch = "runtime_profile"
	FenceMismatchProfileSchema            FenceMismatch = "profile_digest_schema"
)

// CompareFence compares the expected current fence with a request fence. A
// non-empty result is stale and must be handled as HTTP 410 by the transport.
func CompareFence(expected, request Fence, requireSession bool) FenceMismatch {
	switch {
	case expected.RuntimeInstanceID != request.RuntimeInstanceID:
		return FenceMismatchRuntimeInstance
	case expected.SupervisorBootID != request.SupervisorBootID:
		return FenceMismatchSupervisorBoot
	case expected.ControllerEpoch != request.ControllerEpoch:
		return FenceMismatchControllerEpoch
	case expected.RuntimePoolUID != request.RuntimePoolUID:
		return FenceMismatchRuntimePoolUID
	case expected.RuntimePoolGeneration != request.RuntimePoolGeneration:
		return FenceMismatchRuntimePoolGeneration
	case requireSession && expected.RuntimeSessionUID != request.RuntimeSessionUID:
		return FenceMismatchRuntimeSessionUID
	case requireSession && expected.RuntimeSessionGeneration != request.RuntimeSessionGeneration:
		return FenceMismatchRuntimeSessionGeneration
	case expected.RuntimeProfileDigest != request.RuntimeProfileDigest:
		return FenceMismatchRuntimeProfile
	case expected.ProfileDigestSchemaVersion != request.ProfileDigestSchemaVersion:
		return FenceMismatchProfileSchema
	default:
		return FenceMatch
	}
}

func requireIdentifier(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > maxPathSegmentBytes {
		return fmt.Errorf("%s exceeds %d bytes", name, maxPathSegmentBytes)
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return fmt.Errorf("%s contains whitespace or non-ASCII bytes", name)
		}
	}
	return nil
}
