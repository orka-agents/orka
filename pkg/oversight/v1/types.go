/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1

import "time"

// Mode says whether oversight only records what it would do or holds actions.
type Mode string

const (
	// ModeAudit records decisions without holding any action.
	ModeAudit Mode = "audit"
	// ModeGate holds actions whose evaluation crosses a gate threshold.
	ModeGate Mode = "gate"
)

// OutageFallback says what happens to an action when a monitor tier is
// unavailable.
type OutageFallback string

const (
	// OutageFallbackFailClosed holds the action.
	OutageFallbackFailClosed OutageFallback = "fail_closed"
	// OutageFallbackAuditOnly lets the action run and records the gap.
	OutageFallbackAuditOnly OutageFallback = "audit_only"
)

// EvidenceLevel is how much of an action the pipeline saw.
type EvidenceLevel string

const (
	// EvidenceLevelIssued means only the agent's request was seen.
	EvidenceLevelIssued EvidenceLevel = "issued"
	// EvidenceLevelApplied means the platform recorded that the action ran.
	EvidenceLevelApplied EvidenceLevel = "applied"
	// EvidenceLevelObserved means an independent sensor saw its effects.
	EvidenceLevelObserved EvidenceLevel = "observed"
	// EvidenceLevelAttested is reserved for signed node evidence.
	EvidenceLevelAttested EvidenceLevel = "attested"
)

// Disposition is the outcome Orka recorded for an action.
type Disposition string

const (
	// DispositionAllow lets the action stand.
	DispositionAllow Disposition = "allow"
	// DispositionAuditFlagged is an audit-mode action that gate mode would
	// have paused or held.
	DispositionAuditFlagged Disposition = "audit_flagged"
	// DispositionPaused waits on a further tier or a person.
	DispositionPaused Disposition = "paused"
	// DispositionHeldForHuman waits on a person.
	DispositionHeldForHuman Disposition = "held_for_human"
	// DispositionFailClosed held the action because a monitor tier gave no
	// usable answer.
	DispositionFailClosed Disposition = "fail_closed"
	// DispositionDegraded let the action run under the audit-only outage
	// fallback while a monitor tier gave no usable answer.
	DispositionDegraded Disposition = "degraded"
)

// Dispositions lists every disposition, in severity order from least.
var Dispositions = []Disposition{
	DispositionAllow, DispositionDegraded, DispositionAuditFlagged, DispositionPaused,
	DispositionHeldForHuman, DispositionFailClosed,
}

// Stream states in ActionCoverage.StreamState.
const (
	StreamStateOK      = "ok"
	StreamStatePartial = "partial"
	StreamStateMissing = "missing"
)

// Sensor states in ActionCoverage.SensorState.
const (
	SensorStateEvents       = "events"
	SensorStateNone         = "none"
	SensorStateUnavailable  = "unavailable"
	SensorStateUnattributed = "unattributed"
)

// Trust states of a scorer or advisor.
const (
	TrustStateQualified        = "qualified"
	TrustStateShadow           = "shadow"
	TrustStateUntrustedMonitor = "untrusted_monitor"
)

// Evaluation statuses of a scorer or advisor.
const (
	EvaluationStatusOK              = "ok"
	EvaluationStatusUnavailable     = "unavailable"
	EvaluationStatusTimeout         = "timeout"
	EvaluationStatusError           = "error"
	EvaluationStatusContentFiltered = "content_filtered"
)

// OversightDecision is the immutable record of one agent action evaluated by
// an oversight pipeline. It is idempotent on (Namespace, ID): submitting the
// same ID again with the same content is a no-op, and with different content
// is refused.
type OversightDecision struct {
	// ID is the caller's deterministic identifier, such as "<task>@<actionSeq>".
	ID string `json:"id"`
	// Seq is the store's monotonic cursor, assigned on ingest; any submitted
	// value is ignored.
	Seq       int64  `json:"seq,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	TaskName  string `json:"taskName"`
	TaskUID   string `json:"taskUID,omitempty"`
	AgentName string `json:"agentName,omitempty"`
	// WorkloadIdentity is the subject the action ran as, such as a
	// ServiceAccount, SPIFFE ID or OIDC subject.
	WorkloadIdentity string    `json:"workloadIdentity,omitempty"`
	RunID            string    `json:"runID,omitempty"`
	ActionSeq        int       `json:"actionSeq"`
	ObservedAt       time.Time `json:"observedAt"`
	// RecordedAt is assigned on ingest; any submitted value is ignored.
	RecordedAt time.Time `json:"recordedAt,omitzero"`

	Mode             Mode           `json:"mode"`
	OutageFallback   OutageFallback `json:"outageFallback,omitempty"`
	CharterRevision  string         `json:"charterRevision,omitempty"`
	ConcernsRef      string         `json:"concernsRef,omitempty"`
	ConcernsRevision string         `json:"concernsRevision,omitempty"`
	ConcernsDigest   string         `json:"concernsDigest,omitempty"`
	// PolicyRevision names the monitor policy that mapped scores to this
	// disposition, such as its thresholds and escalation rules.
	PolicyRevision string `json:"policyRevision,omitempty"`

	Action        ActionEnvelope `json:"action"`
	EvidenceLevel EvidenceLevel  `json:"evidenceLevel"`
	Coverage      ActionCoverage `json:"coverage"`

	Tier0 *Tier0Evaluation `json:"tier0,omitempty"`
	// CharterFindings are bounds of the charter the action went against.
	CharterFindings []CharterFinding    `json:"charterFindings,omitempty"`
	Scorers         []ScorerEvaluation  `json:"scorers,omitempty"`
	Advisors        []AdvisorEvaluation `json:"advisors,omitempty"`

	Disposition      Disposition `json:"disposition"`
	WouldHavePaused  bool        `json:"wouldHavePaused,omitempty"`
	EscalatedBecause []string    `json:"escalatedBecause,omitempty"`
	// ApprovalRef is reserved for the approval a hold opens.
	ApprovalRef string `json:"approvalRef,omitempty"`

	// Attestation is reserved for signed evidence.
	Attestation *AttestationEnvelope `json:"attestation,omitempty"`
}

// ActionEnvelope is what the agent asked to do, what was observed, and the
// causal context.
type ActionEnvelope struct {
	ToolName       string          `json:"toolName"`
	ToolCallID     string          `json:"toolCallID,omitempty"`
	RequestSummary string          `json:"requestSummary,omitempty"`
	Effects        ObservedEffects `json:"effects"`
	// PriorActionIDs are the decision IDs of the preceding actions the
	// evaluation saw.
	PriorActionIDs []string `json:"priorActionIDs,omitempty"`
	// TaintFlags only accumulate over a Task's lifetime.
	TaintFlags     []string `json:"taintFlags,omitempty"`
	InheritedTaint []string `json:"inheritedTaint,omitempty"`
}

// ObservedEffects summarizes what a sensor saw the action do.
type ObservedEffects struct {
	// Counts maps an effect kind, such as "exec" or "net_connect", to how
	// often it was seen.
	Counts       map[string]int `json:"counts,omitempty"`
	Binaries     []CountedValue `json:"binaries,omitempty"`
	Paths        []CountedValue `json:"paths,omitempty"`
	Destinations []CountedValue `json:"destinations,omitempty"`
	BlockedCount int            `json:"blockedCount,omitempty"`
	// BytesSent is the payload bytes the sensor saw delivered outbound. Nil
	// means the sensor did not observe sends, which is not the same as zero.
	BytesSent     *int64        `json:"bytesSent,omitempty"`
	NotableEvents []EffectEvent `json:"notableEvents,omitempty"`
	// OmittedEvents counts events left out of NotableEvents.
	OmittedEvents int `json:"omittedEvents,omitempty"`
}

// CountedValue is a value and how often it was seen.
type CountedValue struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// EffectEvent is one sensor event an evaluation may cite.
type EffectEvent struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Binary  string `json:"binary,omitempty"`
	Path    string `json:"path,omitempty"`
	Dest    string `json:"dest,omitempty"`
	Args    string `json:"args,omitempty"`
	Outcome string `json:"outcome,omitempty"`
	Role    string `json:"role,omitempty"`
}

// ActionCoverage says how completely the action was seen. It is never
// omitted: a decision about an action nobody watched must say so.
type ActionCoverage struct {
	StreamState       string         `json:"streamState"`
	SensorState       string         `json:"sensorState"`
	AttributionMethod string         `json:"attributionMethod,omitempty"`
	AttributionCounts map[string]int `json:"attributionCounts,omitempty"`
	// UnattributedEvents counts sensor events in the action's window that
	// could not be attributed to any action.
	UnattributedEvents int `json:"unattributedEvents,omitempty"`
	// BlindSpots are machine codes for what the sensor cannot see here.
	BlindSpots []string `json:"blindSpots,omitempty"`
}

// Tier0Evaluation is the deterministic rule tier.
type Tier0Evaluation struct {
	// Rules are the rule IDs that matched.
	Rules []string `json:"rules,omitempty"`
	// HighStakesClass names the deterministic high-stakes class the action
	// fell in, if any. Such actions are never released by an advisor.
	HighStakesClass string `json:"highStakesClass,omitempty"`
}

// CharterFinding records that an action went against one bound of the
// charter in force. Sentence quotes the bound as the charter states it.
type CharterFinding struct {
	ProducerRef   string   `json:"producerRef"`
	BoundID       string   `json:"boundID"`
	Sentence      string   `json:"sentence"`
	CitedEventIDs []string `json:"citedEventIDs,omitempty"`
	// Enforced is true only when the bound was enforced, not just watched.
	Enforced bool `json:"enforced,omitempty"`
}

// ScorerEvaluation is one tier 1 decision model's answer. Several scorers
// combine as "any flags".
type ScorerEvaluation struct {
	ProviderRef    string         `json:"providerRef"`
	ModelID        string         `json:"modelID"`
	ModelFamily    string         `json:"modelFamily,omitempty"`
	ModelDigest    string         `json:"modelDigest,omitempty"`
	PromptRevision string         `json:"promptRevision,omitempty"`
	TrustState     string         `json:"trustState,omitempty"`
	WindowSize     int            `json:"windowSize,omitempty"`
	LatencyMs      int64          `json:"latencyMs,omitempty"`
	Status         string         `json:"status"`
	CitedEventIDs  []string       `json:"citedEventIDs,omitempty"`
	Scores         []ConcernScore `json:"scores,omitempty"`
}

// ConcernScore is a scorer's probability for one concern or meta question.
type ConcernScore struct {
	QuestionID  string   `json:"questionID"`
	Label       string   `json:"label,omitempty"`
	Probability float64  `json:"probability"`
	Threshold   *float64 `json:"threshold,omitempty"`
	Triggered   bool     `json:"triggered,omitempty"`
}

// AdvisorEvaluation is one tier 2 advisor's answer. An advisor can only
// request a release; Orka decides whether it is granted.
type AdvisorEvaluation struct {
	ProviderRef         string   `json:"providerRef"`
	ModelID             string   `json:"modelID"`
	ModelFamily         string   `json:"modelFamily,omitempty"`
	ModelDigest         string   `json:"modelDigest,omitempty"`
	PromptRevision      string   `json:"promptRevision,omitempty"`
	TrustState          string   `json:"trustState,omitempty"`
	Status              string   `json:"status"`
	Verdict             string   `json:"verdict,omitempty"`
	ReleaseRequested    bool     `json:"releaseRequested,omitempty"`
	ReleaseGranted      bool     `json:"releaseGranted,omitempty"`
	ReleaseDeniedReason string   `json:"releaseDeniedReason,omitempty"`
	CitedEventIDs       []string `json:"citedEventIDs,omitempty"`
	Concerns            []string `json:"concerns,omitempty"`
	Summary             string   `json:"summary,omitempty"`
	LatencyMs           int64    `json:"latencyMs,omitempty"`
}

// AttestationEnvelope is reserved for signed evidence about an action.
type AttestationEnvelope struct {
	Format        string `json:"format,omitempty"`
	Issuer        string `json:"issuer,omitempty"`
	SubjectDigest string `json:"subjectDigest,omitempty"`
	Verified      *bool  `json:"verified,omitempty"`
	PayloadRef    string `json:"payloadRef,omitempty"`
}

// FeedbackLabel is a person's outcome label for a decision. Labels calibrate
// monitors; they change no agent behaviour.
type FeedbackLabel string

const (
	FeedbackRealProblem FeedbackLabel = "real_problem"
	FeedbackFalseAlarm  FeedbackLabel = "false_alarm"
	FeedbackUnclear     FeedbackLabel = "unclear"
)

// DecisionFeedback is the latest outcome label recorded for a decision.
type DecisionFeedback struct {
	Label      FeedbackLabel `json:"label"`
	Note       string        `json:"note,omitempty"`
	Reviewer   string        `json:"reviewer,omitempty"`
	RecordedAt time.Time     `json:"recordedAt"`
}

// TriggeredConcerns returns the concern and question IDs any scorer
// triggered, plus the concerns any advisor named, sorted and unique.
func (d *OversightDecision) TriggeredConcerns() []string {
	seen := map[string]bool{}
	for _, scorer := range d.Scorers {
		for _, score := range scorer.Scores {
			if score.Triggered && score.QuestionID != "" {
				seen[score.QuestionID] = true
			}
		}
	}
	for _, advisor := range d.Advisors {
		for _, concern := range advisor.Concerns {
			if concern != "" {
				seen[concern] = true
			}
		}
	}
	return sortedKeys(seen)
}

// MonitorFailed reports whether a tier gave no usable answer: every scorer,
// or every advisor, that counts failed. Shadow evaluators count only when a
// tier has nothing else, so a shadow scorer timing out never hides a working
// one, and a failed advisor is covered by one that answered.
func (d *OversightDecision) MonitorFailed() bool {
	scorers := make([]evaluation, 0, len(d.Scorers))
	for _, s := range d.Scorers {
		scorers = append(scorers, evaluation{s.TrustState, s.Status})
	}
	advisors := make([]evaluation, 0, len(d.Advisors))
	for _, a := range d.Advisors {
		advisors = append(advisors, evaluation{a.TrustState, a.Status})
	}
	return tierFailed(scorers) || tierFailed(advisors)
}

type evaluation struct{ trust, status string }

func tierFailed(tier []evaluation) bool {
	if len(tier) == 0 {
		return false
	}
	counted := 0
	for _, e := range tier {
		if e.trust == TrustStateShadow {
			continue
		}
		counted++
		if e.status == EvaluationStatusOK {
			return false
		}
	}
	if counted > 0 {
		return true
	}
	for _, e := range tier {
		if e.status == EvaluationStatusOK {
			return false
		}
	}
	return true
}

// MaxProbability returns the highest probability any scorer gave any
// question, or 0 without scores.
func (d *OversightDecision) MaxProbability() float64 {
	highest := 0.0
	for _, scorer := range d.Scorers {
		for _, score := range scorer.Scores {
			highest = max(highest, score.Probability)
		}
	}
	return highest
}
