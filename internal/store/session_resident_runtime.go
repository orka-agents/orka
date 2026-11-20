package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// SessionResidentRuntimeState is the lifecycle of an ADR 0059 handoff record.
type SessionResidentRuntimeState string

const (
	// SessionResidentRuntimeResident is a record that one later turn may claim.
	SessionResidentRuntimeResident SessionResidentRuntimeState = "Resident"
	// SessionResidentRuntimeClaimed is a record one turn's lease acquisition
	// consumed. It never authorizes a second claim.
	SessionResidentRuntimeClaimed SessionResidentRuntimeState = "Claimed"
	// SessionResidentRuntimeRetiring is a record that can never be claimed. It
	// is inert metadata until the next keep or the Session's deletion.
	SessionResidentRuntimeRetiring SessionResidentRuntimeState = "Retiring"
)

// SessionResidentRuntimeClaim names the exact turn whose lease acquisition
// claimed a resident runtime.
type SessionResidentRuntimeClaim struct {
	TaskUID         string `json:"taskUid"`
	Attempt         int64  `json:"attempt"`
	PromptID        string `json:"promptId"`
	LeaseGeneration int64  `json:"leaseGeneration"`
}

// SessionResidentRuntime is the ADR 0059 handoff record: the one live ACP
// RuntimeSession a Session keeps across a turn boundary. It holds identities
// and digests only, never a credential, token, or token digest.
type SessionResidentRuntime struct {
	HandoffID string `json:"handoffId"`

	SourceTaskUID         string `json:"sourceTaskUid"`
	SourceAttempt         int64  `json:"sourceAttempt"`
	SourcePromptAttemptID string `json:"sourcePromptAttemptId"`
	SourceTurnID          string `json:"sourceTurnId"`
	SourceLeaseGeneration int64  `json:"sourceLeaseGeneration"`

	ControllerEpochName string `json:"controllerEpochName"`
	ControllerEpoch     int64  `json:"controllerEpoch"`

	WorkspaceName            string `json:"workspaceName"`
	WorkspaceUID             string `json:"workspaceUid"`
	WorkspaceAttachmentEpoch int64  `json:"workspaceAttachmentEpoch"`

	RuntimePoolName       string `json:"runtimePoolName"`
	RuntimePoolUID        string `json:"runtimePoolUid"`
	RuntimePoolGeneration int64  `json:"runtimePoolGeneration"`

	RuntimeInstanceID  string `json:"runtimeInstanceId"`
	SupervisorBootID   string `json:"supervisorBootId"`
	ServedRecordDigest string `json:"servedRecordDigest"`

	RuntimeSessionUID        string `json:"runtimeSessionUid"`
	RuntimeSessionGeneration int64  `json:"runtimeSessionGeneration"`
	ProfileDigest            string `json:"profileDigest"`
	MCPDigest                string `json:"mcpDigest"`
	// WorkspaceDigest is the RuntimeSession workspace binding digest the next
	// turn must compute for reuse; after a rebase it is the rebased identity.
	WorkspaceDigest     string `json:"workspaceDigest"`
	LineageConfigDigest string `json:"lineageConfigDigest"`

	ReuseDigest          string `json:"reuseDigest"`
	TranscriptHeadDigest string `json:"transcriptHeadDigest"`
	// RebasedTreeDigest is the workspace tree identity a write-intent rebase
	// installed. Empty for read intent and for an unpublished write turn.
	RebasedTreeDigest string                  `json:"rebasedTreeDigest,omitempty"`
	VerifiedBaseline  *VerifiedBranchBaseline `json:"verifiedBaseline,omitempty"`
	PromptDelivered   bool                    `json:"promptDelivered,omitempty"`

	KeptAt    time.Time `json:"keptAt"`
	ExpiresAt time.Time `json:"expiresAt"`

	State SessionResidentRuntimeState  `json:"state"`
	Claim *SessionResidentRuntimeClaim `json:"claim,omitempty"`
}

// ClaimSessionResidentRuntime asks a lease acquisition to claim the exact
// resident runtime named by HandoffID in the same write.
type ClaimSessionResidentRuntime struct {
	HandoffID string `json:"handoffId"`
}

// RetireSessionResidentRuntimeRequest moves the exact named record to
// Retiring. It never revives a record.
type RetireSessionResidentRuntimeRequest struct {
	Namespace   string               `json:"namespace"`
	SessionName string               `json:"sessionName"`
	SessionUID  string               `json:"sessionUid"`
	HandoffID   string               `json:"handoffId"`
	Fence       ControllerEpochFence `json:"fence"`
	RetiredAt   time.Time            `json:"retiredAt"`
}

// SessionResidentRuntimeSupport is implemented by control stores that persist
// ADR 0059 handoff records. A store without it never keeps a runtime, so the
// persistent-session path always falls back to a fresh session.
type SessionResidentRuntimeSupport interface {
	SupportsSessionResidentRuntime() bool
	RetireSessionResidentRuntime(ctx context.Context, request RetireSessionResidentRuntimeRequest) (*SessionControl, error)
}

// SessionResidentRuntimeHandoffID derives the record's identity from every
// field except the identity, state and claim.
func SessionResidentRuntimeHandoffID(record SessionResidentRuntime) (string, error) {
	record.HandoffID = ""
	record.State = ""
	record.Claim = nil
	record.KeptAt = residentRuntimeTime(record.KeptAt)
	record.ExpiresAt = residentRuntimeTime(record.ExpiresAt)
	canonical, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = h.Write([]byte("orka.acp.session-resident-runtime/v1\x00"))
	_, _ = h.Write(canonical)
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// residentRuntimeTime is the persisted precision of a record's times. Both
// backends store whole seconds, so the handoff identity is derived from them.
func residentRuntimeTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	return value.UTC().Truncate(time.Second)
}

// NewSessionResidentRuntime validates a record about to be kept and returns it
// with its handoff identity and Resident state.
func NewSessionResidentRuntime(record SessionResidentRuntime) (SessionResidentRuntime, error) {
	record.State = SessionResidentRuntimeResident
	record.Claim = nil
	record.KeptAt = residentRuntimeTime(record.KeptAt)
	record.ExpiresAt = residentRuntimeTime(record.ExpiresAt)
	id, err := SessionResidentRuntimeHandoffID(record)
	if err != nil {
		return SessionResidentRuntime{}, err
	}
	record.HandoffID = id
	if err := ValidateSessionResidentRuntime(record); err != nil {
		return SessionResidentRuntime{}, err
	}
	return record, nil
}

// ValidateSessionResidentRuntime checks a record's shape and identity.
//
//nolint:gocyclo // Every identity field is checked in one auditable place.
func ValidateSessionResidentRuntime(record SessionResidentRuntime) error {
	for field, value := range map[string]string{
		"resident runtime source Task UID":         record.SourceTaskUID,
		"resident runtime source PromptAttempt ID": record.SourcePromptAttemptID,
		"resident runtime source SessionTurn ID":   record.SourceTurnID,
		"resident runtime controller epoch name":   record.ControllerEpochName,
		"resident runtime workspace name":          record.WorkspaceName,
		"resident runtime workspace UID":           record.WorkspaceUID,
		"resident runtime pool name":               record.RuntimePoolName,
		"resident runtime pool UID":                record.RuntimePoolUID,
		"resident runtime instance ID":             record.RuntimeInstanceID,
		"resident runtime supervisor boot ID":      record.SupervisorBootID,
		"resident runtime session UID":             record.RuntimeSessionUID,
	} {
		if err := ValidateControlIdentifier(field, strings.TrimSpace(value)); err != nil {
			return err
		}
	}
	for field, value := range map[string]string{
		"resident runtime handoff ID":            record.HandoffID,
		"resident runtime served record digest":  record.ServedRecordDigest,
		"resident runtime profile digest":        record.ProfileDigest,
		"resident runtime MCP digest":            record.MCPDigest,
		"resident runtime workspace digest":      record.WorkspaceDigest,
		"resident runtime lineage config digest": record.LineageConfigDigest,
		"resident runtime reuse digest":          record.ReuseDigest,
		"resident runtime transcript head":       record.TranscriptHeadDigest,
	} {
		if err := ValidateCanonicalDigest(field, value); err != nil {
			return err
		}
	}
	if record.RebasedTreeDigest != "" {
		if err := ValidateCanonicalDigest("resident runtime rebased tree digest", record.RebasedTreeDigest); err != nil {
			return err
		}
	}
	if record.SourceAttempt < 1 || record.SourceLeaseGeneration < 1 || record.ControllerEpoch < 1 ||
		record.WorkspaceAttachmentEpoch < 1 || record.RuntimePoolGeneration < 1 || record.RuntimeSessionGeneration < 1 {
		return ValidationErrorf("resident runtime generations, epochs and attempts must be positive")
	}
	if record.KeptAt.IsZero() || !record.ExpiresAt.After(record.KeptAt) {
		return ValidationErrorf("resident runtime expiry must be after it was kept")
	}
	if record.VerifiedBaseline != nil {
		if err := ValidateVerifiedBaseline(*record.VerifiedBaseline); err != nil {
			return err
		}
	}
	if record.RebasedTreeDigest != "" && record.VerifiedBaseline == nil {
		return ValidationErrorf("a rebased resident runtime requires the verified baseline it was rebased to")
	}
	expected, err := SessionResidentRuntimeHandoffID(record)
	if err != nil {
		return err
	}
	if record.HandoffID != expected {
		return ValidationErrorf("resident runtime handoff ID does not match its fields")
	}
	switch record.State {
	case SessionResidentRuntimeResident:
		if record.Claim != nil {
			return ValidationErrorf("a resident runtime that is still resident has no claim")
		}
	case SessionResidentRuntimeClaimed:
		if record.Claim == nil {
			return ValidationErrorf("a claimed resident runtime names its claim")
		}
	case SessionResidentRuntimeRetiring:
	default:
		return ValidationErrorf("unsupported resident runtime state %q", record.State)
	}
	if record.Claim != nil {
		if err := ValidateControlIdentifier("resident runtime claim Task UID", record.Claim.TaskUID); err != nil {
			return err
		}
		if err := ValidateControlIdentifier("resident runtime claim prompt ID", record.Claim.PromptID); err != nil {
			return err
		}
		if record.Claim.Attempt < 1 || record.Claim.LeaseGeneration <= record.SourceLeaseGeneration {
			return ValidationErrorf("a resident runtime claim must come from a later lease generation")
		}
	}
	return nil
}

// ValidateResidentRuntimeForFinalization checks that a record kept by a
// SessionTurn finalization names exactly that turn under the write's fence.
func ValidateResidentRuntimeForFinalization(record SessionResidentRuntime, key SessionTurnKey, promptAttemptID string, fence ControllerEpochFence) error {
	if err := ValidateSessionResidentRuntime(record); err != nil {
		return err
	}
	turnID, err := key.CanonicalID()
	if err != nil {
		return err
	}
	if record.State != SessionResidentRuntimeResident || record.SourceTurnID != turnID ||
		record.SourceTaskUID != key.TaskUID || record.SourceAttempt != key.Attempt ||
		record.SourceLeaseGeneration != key.LeaseGeneration || record.SourcePromptAttemptID != promptAttemptID ||
		record.RuntimeSessionUID != key.SessionUID {
		return ValidationErrorf("resident runtime does not name the finalized SessionTurn")
	}
	if record.ControllerEpochName != fence.Name || record.ControllerEpoch != fence.Epoch {
		return ValidationErrorf("resident runtime was kept under a different controller epoch")
	}
	return nil
}

// ResolveResidentRuntimeOnAcquire returns the record a new mutation lease
// acquisition commits in the same write as the lease. A Resident record is
// claimed only by a matching request under the record's own controller epoch
// before it expires; any other acquisition moves it to Retiring, so no later
// turn can claim a runtime this turn will delete or replace. Claimed and
// Retiring records only ever move to Retiring.
func ResolveResidentRuntimeOnAcquire(
	current *SessionResidentRuntime,
	claim *ClaimSessionResidentRuntime,
	fence ControllerEpochFence,
	lease SessionResidentRuntimeClaim,
	acquiredAt time.Time,
) *SessionResidentRuntime {
	if current == nil {
		return nil
	}
	next := *current
	if current.Claim != nil {
		copyClaim := *current.Claim
		next.Claim = &copyClaim
	}
	if current.State == SessionResidentRuntimeResident && claim != nil && claim.HandoffID == current.HandoffID &&
		current.ControllerEpochName == fence.Name && current.ControllerEpoch == fence.Epoch &&
		acquiredAt.Before(current.ExpiresAt) && lease.LeaseGeneration > current.SourceLeaseGeneration {
		next.State = SessionResidentRuntimeClaimed
		claimed := lease
		next.Claim = &claimed
		return &next
	}
	next.State = SessionResidentRuntimeRetiring
	return &next
}

// ResidentRuntimeClaimedBy reports whether the record is claimed by the exact
// lease of one turn.
func ResidentRuntimeClaimedBy(record *SessionResidentRuntime, key SessionTurnKey) bool {
	return record != nil && record.State == SessionResidentRuntimeClaimed && record.Claim != nil &&
		record.Claim.TaskUID == key.TaskUID && record.Claim.Attempt == key.Attempt &&
		record.Claim.PromptID == key.PromptID && record.Claim.LeaseGeneration == key.LeaseGeneration &&
		record.RuntimeSessionUID == key.SessionUID
}

// CloneSessionResidentRuntime returns a deep copy.
func CloneSessionResidentRuntime(record *SessionResidentRuntime) *SessionResidentRuntime {
	if record == nil {
		return nil
	}
	copied := *record
	if record.Claim != nil {
		claim := *record.Claim
		copied.Claim = &claim
	}
	if record.VerifiedBaseline != nil {
		baseline := *record.VerifiedBaseline
		copied.VerifiedBaseline = &baseline
	}
	return &copied
}
