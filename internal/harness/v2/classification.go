package v2

import (
	"fmt"
	"net/http"
	"time"
)

type OperationPhase string

const (
	OperationPhaseRecorded OperationPhase = "recorded"
	OperationPhaseApplied  OperationPhase = "applied"
	OperationPhaseAccepted OperationPhase = "accepted"
	OperationPhaseSettled  OperationPhase = "settled"
	OperationPhaseDeleted  OperationPhase = "deleted"
)

type OperationRecord struct {
	OperationID   OperationID    `json:"operationID"`
	RequestDigest RequestDigest  `json:"requestDigest"`
	Phase         OperationPhase `json:"phase"`
	TerminalEvent EventType      `json:"terminalEvent,omitempty"`
	RecordedAt    time.Time      `json:"recordedAt"`
	UpdatedAt     time.Time      `json:"updatedAt"`
}

func (r OperationRecord) Validate() error {
	if err := requireIdentifier("operation ID", string(r.OperationID)); err != nil {
		return err
	}
	if err := ValidateRequestDigest(r.RequestDigest); err != nil {
		return fmt.Errorf("request digest: %w", err)
	}
	switch r.Phase {
	case OperationPhaseRecorded, OperationPhaseApplied, OperationPhaseAccepted, OperationPhaseDeleted:
		if r.TerminalEvent != "" {
			return fmt.Errorf("operation phase %q must not carry terminal event", r.Phase)
		}
	case OperationPhaseSettled:
		if !r.TerminalEvent.IsTerminal() {
			return fmt.Errorf("settled operation requires terminal event")
		}
	default:
		return fmt.Errorf("unsupported operation phase %q", r.Phase)
	}
	if r.RecordedAt.IsZero() {
		return fmt.Errorf("operation recorded timestamp is required")
	}
	if !r.UpdatedAt.IsZero() && r.UpdatedAt.Before(r.RecordedAt) {
		return fmt.Errorf("operation updated timestamp precedes recorded timestamp")
	}
	return nil
}

type RequestClassification string

const (
	RequestClassificationFresh           RequestClassification = "fresh"
	RequestClassificationDuplicate       RequestClassification = "duplicate"
	RequestClassificationAlreadyAccepted RequestClassification = "already_accepted"
	RequestClassificationSettled         RequestClassification = "settled"
	RequestClassificationDigestConflict  RequestClassification = "digest_conflict"
	RequestClassificationStaleFence      RequestClassification = "stale_fence"
	RequestClassificationExpired         RequestClassification = "expired"
)

type Classification struct {
	Class         RequestClassification `json:"class"`
	FenceMismatch FenceMismatch         `json:"fenceMismatch,omitempty"`
	Phase         OperationPhase        `json:"phase,omitempty"`
	TerminalEvent EventType             `json:"terminalEvent,omitempty"`
}

func (c Classification) Validate() error {
	switch c.Class {
	case RequestClassificationFresh:
		if c.FenceMismatch != FenceMatch || c.Phase != "" || c.TerminalEvent != "" {
			return fmt.Errorf("fresh classification must not carry prior-operation metadata")
		}
	case RequestClassificationDuplicate, RequestClassificationAlreadyAccepted:
		if c.FenceMismatch != FenceMatch || c.Phase == "" || c.TerminalEvent != "" {
			return fmt.Errorf("%s classification requires non-terminal prior phase", c.Class)
		}
	case RequestClassificationSettled:
		if c.FenceMismatch != FenceMatch || c.Phase != OperationPhaseSettled || !c.TerminalEvent.IsTerminal() {
			return fmt.Errorf("settled classification requires settled phase and terminal event")
		}
	case RequestClassificationDigestConflict:
		if c.FenceMismatch != FenceMatch || c.Phase == "" {
			return fmt.Errorf("digest conflict classification requires existing operation phase")
		}
	case RequestClassificationStaleFence:
		if c.FenceMismatch == FenceMatch || c.Phase != "" || c.TerminalEvent != "" {
			return fmt.Errorf("stale fence classification requires exactly one fence mismatch")
		}
	case RequestClassificationExpired:
		if c.FenceMismatch != FenceMatch || c.Phase != "" || c.TerminalEvent != "" {
			return fmt.Errorf("expired classification must not carry prior-operation metadata")
		}
	default:
		return fmt.Errorf("unsupported request classification %q", c.Class)
	}
	return nil
}

func (c Classification) HTTPStatus() int {
	switch c.Class {
	case RequestClassificationFresh:
		return 0
	case RequestClassificationDuplicate, RequestClassificationAlreadyAccepted, RequestClassificationSettled:
		return http.StatusOK
	case RequestClassificationDigestConflict:
		return http.StatusConflict
	case RequestClassificationStaleFence, RequestClassificationExpired:
		return http.StatusGone
	default:
		return http.StatusInternalServerError
	}
}

// ClassifyOperation applies the v2 replay matrix. Callers pass the current
// expected fence and, when present, the durable/tombstoned record for the exact
// operation identity. It never authorizes replay: Fresh only means no matching
// side-effect record was supplied.
func ClassifyOperation(
	expected Fence,
	incoming MutationMetadata,
	existing *OperationRecord,
	requireSession bool,
	now time.Time,
) (Classification, error) {
	if err := requireIdentifier("operation ID", string(incoming.OperationID)); err != nil {
		return Classification{}, err
	}
	if incoming.RequestDigestSchemaVersion != RequestDigestSchemaVersion {
		return Classification{}, fmt.Errorf("unsupported request digest schema version %d", incoming.RequestDigestSchemaVersion)
	}
	if err := ValidateRequestDigest(incoming.RequestDigest); err != nil {
		return Classification{}, fmt.Errorf("request digest: %w", err)
	}
	if mismatch := CompareFence(expected, incoming.Fence, requireSession); mismatch != FenceMatch {
		return Classification{Class: RequestClassificationStaleFence, FenceMismatch: mismatch}, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if incoming.ExpiresAt.IsZero() || !incoming.ExpiresAt.After(now) {
		return Classification{Class: RequestClassificationExpired}, nil
	}
	if existing == nil {
		return Classification{Class: RequestClassificationFresh}, nil
	}
	if err := existing.Validate(); err != nil {
		return Classification{}, fmt.Errorf("existing operation record: %w", err)
	}
	if existing.OperationID != incoming.OperationID {
		return Classification{}, fmt.Errorf("existing operation ID %q does not match incoming operation ID %q", existing.OperationID, incoming.OperationID)
	}
	if existing.RequestDigest != incoming.RequestDigest {
		return Classification{Class: RequestClassificationDigestConflict, Phase: existing.Phase, TerminalEvent: existing.TerminalEvent}, nil
	}
	switch existing.Phase {
	case OperationPhaseAccepted:
		return Classification{Class: RequestClassificationAlreadyAccepted, Phase: existing.Phase}, nil
	case OperationPhaseSettled:
		return Classification{Class: RequestClassificationSettled, Phase: existing.Phase, TerminalEvent: existing.TerminalEvent}, nil
	default:
		return Classification{Class: RequestClassificationDuplicate, Phase: existing.Phase}, nil
	}
}

// RuntimeSessionTombstone preserves exact-generation replay classification after
// session deletion. It remains live until the controller durably retires that
// RuntimeSession generation.
type RuntimeSessionTombstone struct {
	RuntimeSessionUID        RuntimeSessionUID `json:"runtimeSessionUID"`
	RuntimeSessionGeneration uint64            `json:"runtimeSessionGeneration"`
	RuntimeProfileDigest     ProfileDigest     `json:"runtimeProfileDigest"`
	DeletedAt                time.Time         `json:"deletedAt"`
	Operations               []OperationRecord `json:"operations,omitempty"`
}

func (t RuntimeSessionTombstone) Validate() error {
	if err := requireIdentifier("runtime session UID", string(t.RuntimeSessionUID)); err != nil {
		return err
	}
	if t.RuntimeSessionGeneration == 0 {
		return fmt.Errorf("runtime session generation must be positive")
	}
	if err := ValidateProfileDigest(t.RuntimeProfileDigest); err != nil {
		return fmt.Errorf("runtime profile digest: %w", err)
	}
	if t.DeletedAt.IsZero() {
		return fmt.Errorf("deleted timestamp is required")
	}
	if len(t.Operations) > 4096 {
		return fmt.Errorf("tombstoned operation count exceeds 4096")
	}
	seen := make(map[OperationID]struct{}, len(t.Operations))
	for i := range t.Operations {
		if err := t.Operations[i].Validate(); err != nil {
			return fmt.Errorf("operation %d: %w", i, err)
		}
		if _, ok := seen[t.Operations[i].OperationID]; ok {
			return fmt.Errorf("duplicate tombstoned operation ID %q", t.Operations[i].OperationID)
		}
		seen[t.Operations[i].OperationID] = struct{}{}
	}
	return nil
}

func (t RuntimeSessionTombstone) CanRetire(sessionUID RuntimeSessionUID, retiredThroughGeneration uint64) bool {
	return sessionUID == t.RuntimeSessionUID && retiredThroughGeneration >= t.RuntimeSessionGeneration
}
