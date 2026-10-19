package v1

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// SessionLineage durably records the execution protocol and runtime identity
// of one conversation Session. LineageGeneration is independent of the Session
// mutation-lease generation and any v2 RuntimeSession generation.
type SessionLineage struct {
	Namespace    string
	SessionName  string
	NamespaceUID string
	SessionUID   string
	// ContractVersion is orka.harness.v1 or orka.harness.v2.
	ContractVersion   string
	LineageGeneration int64
	// RuntimeIdentity is the built-in runtime type or the AgentRuntime UID.
	RuntimeIdentity string
	// ConfigDigest is the configuration/snapshot digest recorded when the
	// lineage was established.
	ConfigDigest string
	Version      int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Validate rejects an incomplete authoritative Session lineage.
func (l SessionLineage) Validate() error {
	claim := ClaimSessionLineageRequest{
		Namespace: l.Namespace, SessionName: l.SessionName, NamespaceUID: l.NamespaceUID,
		SessionUID: l.SessionUID, ContractVersion: l.ContractVersion,
		LineageGeneration: l.LineageGeneration, RuntimeIdentity: l.RuntimeIdentity, ConfigDigest: l.ConfigDigest,
	}
	if err := claim.Validate(); err != nil {
		return err
	}
	if l.LineageGeneration < 1 {
		return ValidationErrorf("session lineage generation must be at least 1")
	}
	if l.Version < 1 {
		return ValidationErrorf("session lineage projection version must be at least 1")
	}
	if l.CreatedAt.IsZero() || l.UpdatedAt.IsZero() {
		return ValidationErrorf("session lineage creation and update times are required")
	}
	return nil
}

// ClaimSessionLineageRequest atomically establishes or verifies a Session
// lineage. Callers must invoke it under the same serialization that acquires
// the Session mutation lease so two concurrent first-use Tasks cannot
// establish different protocols.
type ClaimSessionLineageRequest struct {
	Namespace       string
	SessionName     string
	NamespaceUID    string
	SessionUID      string
	ContractVersion string
	// LineageGeneration is independent of mutation-lease and runtime-session
	// generations. Ordinary first use establishes generation 1.
	LineageGeneration int64
	RuntimeIdentity   string
	ConfigDigest      string

	// EstablishIfAbsent permits creating the lineage row. It must be true only
	// when the caller has proven the Session is fresh or is a controller-owned
	// Gateway Session whose first message was admitted atomically.
	EstablishIfAbsent bool
}

// Validate rejects incomplete lineage claims.
func (r ClaimSessionLineageRequest) Validate() error {
	switch {
	case strings.TrimSpace(r.Namespace) == "":
		return ValidationErrorf("session lineage namespace is required")
	case strings.TrimSpace(r.SessionName) == "":
		return ValidationErrorf("session lineage session name is required")
	case strings.TrimSpace(r.NamespaceUID) == "":
		return ValidationErrorf("session lineage namespace UID is required")
	case strings.TrimSpace(r.SessionUID) == "":
		return ValidationErrorf("session lineage session UID is required")
	case r.ContractVersion != "orka.harness.v1" && r.ContractVersion != "orka.harness.v2":
		return ValidationErrorf("session lineage contract version %q must be orka.harness.v1 or orka.harness.v2", r.ContractVersion)
	case r.LineageGeneration < 1:
		return ValidationErrorf("session lineage generation must be at least 1")
	case strings.TrimSpace(r.RuntimeIdentity) == "":
		return ValidationErrorf("session lineage runtime identity is required")
	}
	if err := ValidateCanonicalDigest("session lineage config digest", r.ConfigDigest); err != nil {
		return err
	}
	return nil
}

// SessionLineageStore persists a payload projection of Kubernetes-authoritative
// Session protocol/runtime lineage. Projection failure may block dispatch but
// never changes or releases the Kubernetes lineage/Lease authority.
type SessionLineageStore interface {
	// ProjectSessionLineage idempotently stores the exact authoritative record.
	// Any mismatch returns ErrConflict and must be repaired explicitly; SQLite
	// never adjudicates which lineage owns a Session.
	ProjectSessionLineage(ctx context.Context, lineage SessionLineage) (*SessionLineage, error)

	// GetSessionLineage returns the lineage for one Session, or ErrNotFound.
	GetSessionLineage(ctx context.Context, namespace, sessionName string) (*SessionLineage, error)
}

// HarnessV1AttemptState is the durable harness v1 attempt state machine.
type HarnessV1AttemptState string

const (
	HarnessV1AttemptPrepared         HarnessV1AttemptState = "Prepared"
	HarnessV1AttemptSubmitting       HarnessV1AttemptState = "Submitting"
	HarnessV1AttemptRejected         HarnessV1AttemptState = "Rejected"
	HarnessV1AttemptSubmittedUnknown HarnessV1AttemptState = "SubmittedUnknown"
	HarnessV1AttemptAccepted         HarnessV1AttemptState = "Accepted"
	HarnessV1AttemptRunning          HarnessV1AttemptState = "Running"
	HarnessV1AttemptCancelRequested  HarnessV1AttemptState = "CancelRequested"
	HarnessV1AttemptSettling         HarnessV1AttemptState = "Settling"
	HarnessV1AttemptSucceeded        HarnessV1AttemptState = "Succeeded"
	HarnessV1AttemptFailed           HarnessV1AttemptState = "Failed"
	HarnessV1AttemptCancelled        HarnessV1AttemptState = "Cancelled"
	HarnessV1AttemptOutcomeUnknown   HarnessV1AttemptState = "OutcomeUnknown"
)

// HarnessV1AttemptKey identifies one durable v1 attempt.
type HarnessV1AttemptKey struct {
	Namespace string
	TaskUID   string
	Attempt   int32
}

// CanonicalID returns the canonical attempt record identity.
func (k HarnessV1AttemptKey) CanonicalID() string {
	return fmt.Sprintf("%s/%s/%d", k.Namespace, k.TaskUID, k.Attempt)
}

// SessionReferenceID returns the bounded content-derived identity stored in a
// protocol-neutral SessionTurn. The attempt store continues to use the
// human-readable CanonicalID as its primary key; this separate identifier
// prevents a v1 attempt from being confused with a v2 PromptAttempt.
func (k HarnessV1AttemptKey) SessionReferenceID() string {
	return CanonicalControlID("harness-v1-attempt", k.Namespace, k.TaskUID, fmt.Sprint(k.Attempt))
}

// Validate rejects incomplete attempt keys.
func (k HarnessV1AttemptKey) Validate() error {
	switch {
	case strings.TrimSpace(k.Namespace) == "":
		return ValidationErrorf("harness v1 attempt namespace is required")
	case strings.TrimSpace(k.TaskUID) == "":
		return ValidationErrorf("harness v1 attempt task UID is required")
	case k.Attempt < 1:
		return ValidationErrorf("harness v1 attempt number must be positive")
	}
	return nil
}

// HarnessV1AttemptRetryClass bounds retry eligibility recorded from the
// immutable snapshot classification.
type HarnessV1AttemptRetryClass string

const (
	// HarnessV1RetryClassNone forbids retry regardless of failure shape.
	HarnessV1RetryClassNone HarnessV1AttemptRetryClass = "none"
	// HarnessV1RetryClassDuplicateSafe permits retry only after a definitive
	// pre-submission rejection or definitive retryable terminal failure.
	HarnessV1RetryClassDuplicateSafe HarnessV1AttemptRetryClass = "duplicate-safe"
)

// HarnessV1Attempt is the durable v1 attempt aggregate. Attempt-specific state
// stays separate from the lifetime Task binding; every record carries the
// binding digest.
type HarnessV1Attempt struct {
	Namespace string
	TaskName  string
	TaskUID   string
	Attempt   int32

	BindingDigest  string
	SnapshotDigest string
	RequestDigest  string

	TurnID           string
	RuntimeSessionID string
	CorrelationID    string

	// Backend identifies the executor: built-in wrapper or external endpoint.
	Backend string
	// BackendEndpoint is the non-secret endpoint identity selected at dispatch.
	BackendEndpoint string

	AuthSecretNamespace       string
	AuthSecretName            string
	AuthSecretKey             string
	AuthSecretUID             string
	AuthSecretResourceVersion string

	State HarnessV1AttemptState
	// LastEventSeq is the highest persisted frame sequence.
	LastEventSeq int64
	// CancelRequestedAt is set when cancellation was requested; CancelAccepted
	// remains nonterminal until a terminal frame or settlement receipt.
	CancelRequestedAt *time.Time
	// TerminalReceiptDigest digests the authoritative terminal or
	// OutcomeUnknown receipt.
	TerminalReceiptDigest string
	// TerminalReason is a bounded reason code for terminal states.
	TerminalReason string

	DuplicateSafe bool
	RetryClass    HarnessV1AttemptRetryClass

	ControllerEpochName string
	ControllerEpoch     int64
	LastOperationID     string
	LastOperationDigest string
	Version             int64
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// Validate rejects incomplete attempt records at creation.
func (a *HarnessV1Attempt) Validate() error {
	if a == nil {
		return ValidationErrorf("harness v1 attempt is required")
	}
	key := HarnessV1AttemptKey{Namespace: a.Namespace, TaskUID: a.TaskUID, Attempt: a.Attempt}
	if err := key.Validate(); err != nil {
		return err
	}
	if err := ValidateCanonicalDigest("harness v1 attempt binding digest", a.BindingDigest); err != nil {
		return err
	}
	if err := ValidateCanonicalDigest("harness v1 attempt snapshot digest", a.SnapshotDigest); err != nil {
		return err
	}
	if err := ValidateCanonicalDigest("harness v1 attempt request digest", a.RequestDigest); err != nil {
		return err
	}
	if strings.TrimSpace(a.TurnID) == "" {
		return ValidationErrorf("harness v1 attempt turn ID is required")
	}
	if a.State != HarnessV1AttemptPrepared {
		return ValidationErrorf("harness v1 attempts are created in Prepared state, got %q", string(a.State))
	}
	switch a.RetryClass {
	case HarnessV1RetryClassNone, HarnessV1RetryClassDuplicateSafe:
	default:
		return ValidationErrorf("harness v1 attempt retry class %q is not supported", string(a.RetryClass))
	}
	return nil
}

// HarnessV1AttemptUpdates carries optional field updates applied atomically
// with a state transition.
type HarnessV1AttemptUpdates struct {
	RuntimeSessionID      *string
	CorrelationID         *string
	BackendEndpoint       *string
	LastEventSeq          *int64
	CancelRequestedAt     *time.Time
	TerminalReceiptDigest *string
	TerminalReason        *string
}

// HarnessV1AttemptTransition is a fenced, idempotent CAS state transition.
type HarnessV1AttemptTransition struct {
	Key             HarnessV1AttemptKey
	ExpectedVersion int64
	ExpectedState   HarnessV1AttemptState
	TargetState     HarnessV1AttemptState
	OperationID     string
	OperationDigest string
	Fence           ControllerEpochFence
	Updates         HarnessV1AttemptUpdates
}

// ReclaimHarnessV1AttemptsRequest removes Task-owned v1 attempt aggregates
// only after terminal state and, for continuity Tasks, durable SessionTurn and
// delivered outbox barriers have been re-proven in the same transaction.
type ReclaimHarnessV1AttemptsRequest struct {
	Namespace       string
	TaskUID         string
	BindingDigest   string
	SessionRequired bool
	Fence           ControllerEpochFence
}

// HarnessV1AttemptStore persists durable v1 attempt aggregates.
type HarnessV1AttemptStore interface {
	// CreateHarnessV1Attempt persists a new Prepared attempt exactly once. A
	// duplicate identical create is idempotent; a duplicate with different
	// content returns ErrDuplicateMismatch.
	CreateHarnessV1Attempt(ctx context.Context, attempt *HarnessV1Attempt, fence ControllerEpochFence) error

	// GetHarnessV1Attempt returns one attempt or ErrNotFound.
	GetHarnessV1Attempt(ctx context.Context, key HarnessV1AttemptKey) (*HarnessV1Attempt, error)

	// ListHarnessV1AttemptsByTask returns every attempt for one Task UID.
	ListHarnessV1AttemptsByTask(ctx context.Context, namespace, taskUID string) ([]HarnessV1Attempt, error)

	// TransitionHarnessV1Attempt applies a fenced CAS transition. A replay with
	// the same operation ID and digest against the already-applied state is
	// idempotent; conflicting expectations return ErrConflict.
	TransitionHarnessV1Attempt(ctx context.Context, transition HarnessV1AttemptTransition) (*HarnessV1Attempt, error)

	// ReclaimHarnessV1Attempts deletes all terminal attempts for one immutable
	// Task. Active attempts or incomplete Session/outbox settlement fail closed.
	ReclaimHarnessV1Attempts(ctx context.Context, request ReclaimHarnessV1AttemptsRequest) (int, error)
}

var harnessV1AttemptTransitions = map[HarnessV1AttemptState][]HarnessV1AttemptState{
	HarnessV1AttemptPrepared:   {HarnessV1AttemptSubmitting, HarnessV1AttemptRejected, HarnessV1AttemptCancelled},
	HarnessV1AttemptSubmitting: {HarnessV1AttemptRejected, HarnessV1AttemptSubmittedUnknown, HarnessV1AttemptAccepted},
	HarnessV1AttemptSubmittedUnknown: {
		HarnessV1AttemptRejected, HarnessV1AttemptAccepted, HarnessV1AttemptOutcomeUnknown,
	},
	HarnessV1AttemptAccepted: {
		HarnessV1AttemptRunning, HarnessV1AttemptCancelRequested, HarnessV1AttemptSettling,
		HarnessV1AttemptSucceeded, HarnessV1AttemptFailed, HarnessV1AttemptCancelled, HarnessV1AttemptOutcomeUnknown,
	},
	HarnessV1AttemptRunning: {
		HarnessV1AttemptCancelRequested, HarnessV1AttemptSettling,
		HarnessV1AttemptSucceeded, HarnessV1AttemptFailed, HarnessV1AttemptCancelled, HarnessV1AttemptOutcomeUnknown,
	},
	HarnessV1AttemptCancelRequested: {
		HarnessV1AttemptSettling, HarnessV1AttemptSucceeded, HarnessV1AttemptFailed,
		HarnessV1AttemptCancelled, HarnessV1AttemptOutcomeUnknown,
	},
	HarnessV1AttemptSettling: {
		HarnessV1AttemptSucceeded, HarnessV1AttemptFailed, HarnessV1AttemptCancelled, HarnessV1AttemptOutcomeUnknown,
	},
}

// IsTerminalHarnessV1AttemptState reports whether a state admits no further
// transitions. Rejected is terminal for the attempt and is the only
// submission-state path eligible for a safe resend through a new attempt.
func IsTerminalHarnessV1AttemptState(state HarnessV1AttemptState) bool {
	switch state {
	case HarnessV1AttemptRejected, HarnessV1AttemptSucceeded, HarnessV1AttemptFailed,
		HarnessV1AttemptCancelled, HarnessV1AttemptOutcomeUnknown:
		return true
	default:
		return false
	}
}

// ValidateHarnessV1AttemptTransition rejects illegal state transitions.
func ValidateHarnessV1AttemptTransition(from, to HarnessV1AttemptState) error {
	if slices.Contains(harnessV1AttemptTransitions[from], to) {
		return nil
	}
	return ValidationErrorf("harness v1 attempt transition %s -> %s is not allowed", from, to)
}
