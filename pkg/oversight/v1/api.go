/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1

import "time"

// Signal is how a decision reads at a glance.
type Signal string

const (
	// SignalConsistent found nothing: the action fits its task.
	SignalConsistent Signal = "consistent"
	// SignalRisky crossed a scorer threshold or would have paused.
	SignalRisky Signal = "risky"
	// SignalAgainstCharter went against a bound of the charter, or was held
	// for a person.
	SignalAgainstCharter Signal = "against_charter"
	// SignalMonitorUnavailable had a monitor tier give no usable answer: the
	// action would be held (fail closed), never silently passed.
	SignalMonitorUnavailable Signal = "monitor_unavailable"
)

// SignalOf classifies a decision. A charter finding or a hold for a person is
// against the charter; a fail-closed or degraded outcome, or any scorer or
// advisor without a usable answer, is monitor_unavailable; a triggered score,
// an advisor concern, a flag or a would-pause is risky; anything else is
// consistent.
func SignalOf(d *OversightDecision) Signal {
	switch {
	case len(d.CharterFindings) > 0, d.Disposition == DispositionHeldForHuman:
		return SignalAgainstCharter
	case d.Disposition == DispositionFailClosed, d.Disposition == DispositionDegraded, d.MonitorFailed():
		return SignalMonitorUnavailable
	case d.WouldHavePaused,
		d.Disposition == DispositionAuditFlagged,
		d.Disposition == DispositionPaused,
		len(d.TriggeredConcerns()) > 0:
		return SignalRisky
	default:
		return SignalConsistent
	}
}

// IngestRequest is the body of POST /internal/v1/oversight/{namespace}/decisions.
type IngestRequest struct {
	Decisions []OversightDecision `json:"decisions"`
}

// IngestResult reports one submitted decision.
type IngestResult struct {
	ID        string `json:"id"`
	Seq       int64  `json:"seq"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

// IngestResponse reports a whole batch.
type IngestResponse struct {
	Ingested   int            `json:"ingested"`
	Duplicates int            `json:"duplicates"`
	Results    []IngestResult `json:"results"`
}

// DecisionView is a stored decision as the API serves it.
type DecisionView struct {
	OversightDecision
	Signal          Signal            `json:"signal"`
	SignatureHash   string            `json:"signatureHash,omitempty"`
	ActionNarrative string            `json:"actionNarrative,omitempty"`
	Feedback        *DecisionFeedback `json:"feedback,omitempty"`
}

// QueryTerm is one parsed search term, echoed so a client can show how the
// query was read.
type QueryTerm struct {
	// Key is a facet or field name, or "text" for a free-text term.
	Key   string `json:"key"`
	Value string `json:"value"`
}

// DecisionListResponse is a page of decisions, newest first unless the
// request asked for ascending order.
type DecisionListResponse struct {
	Namespace string         `json:"namespace"`
	Query     []QueryTerm    `json:"query,omitempty"`
	Items     []DecisionView `json:"items"`
	// Continue is the cursor for the next page, empty on the last one.
	Continue string `json:"continue,omitempty"`
}

// SimilarItem is a decision ranked by its likeness to the source.
type SimilarItem struct {
	Decision DecisionView `json:"decision"`
	// Score is the Jaccard overlap of the two signatures, 1 for an exact match.
	Score          float64   `json:"score"`
	ExactSignature bool      `json:"exactSignature,omitempty"`
	Shared         []Feature `json:"shared"`
}

// SimilarResponse lists decisions like one decision.
type SimilarResponse struct {
	Namespace string       `json:"namespace"`
	Source    DecisionView `json:"source"`
	// Signature is the source's normalized features.
	Signature []Feature     `json:"signature"`
	Items     []SimilarItem `json:"items"`
	// Searched counts the candidates compared; Truncated says more existed.
	Searched  int  `json:"searched"`
	Truncated bool `json:"truncated,omitempty"`
}

// DecisionGroup summarizes the matching decisions of one agent or Task.
type DecisionGroup struct {
	Key            string    `json:"key"`
	Count          int       `json:"count"`
	Risky          int       `json:"risky"`
	AgainstCharter int       `json:"againstCharter"`
	FirstSeen      time.Time `json:"firstSeen"`
	LastSeen       time.Time `json:"lastSeen"`
	// Tasks lists up to a few Task names in the group.
	Tasks []string `json:"tasks,omitempty"`
}

// GroupsResponse groups a query's matches by agent or Task.
type GroupsResponse struct {
	Namespace string          `json:"namespace"`
	By        string          `json:"by"`
	Query     []QueryTerm     `json:"query,omitempty"`
	Groups    []DecisionGroup `json:"groups"`
	Searched  int             `json:"searched"`
	Truncated bool            `json:"truncated,omitempty"`
}

// FeedbackRequest labels one decision.
type FeedbackRequest struct {
	Label FeedbackLabel `json:"label"`
	Note  string        `json:"note,omitempty"`
}

// BulkFeedbackRequest labels several decisions with one label and note.
type BulkFeedbackRequest struct {
	IDs   []string      `json:"ids"`
	Label FeedbackLabel `json:"label"`
	Note  string        `json:"note,omitempty"`
}

// MaxBulkFeedback bounds one bulk feedback request.
const MaxBulkFeedback = 200

// FeedbackResponse reports labels recorded.
type FeedbackResponse struct {
	Recorded int              `json:"recorded"`
	Feedback DecisionFeedback `json:"feedback"`
}

// Confidence states, from best to worst.
const (
	ConfidenceWarranted         = "warranted"
	ConfidenceDegraded          = "degraded"
	ConfidenceBlind             = "blind"
	ConfidenceAttentionRequired = "attention_required"
)

// Visibility states.
const (
	VisibilityFull    = "full"
	VisibilityPartial = "partial"
	VisibilityBlind   = "blind"
)

// Control states.
const (
	ControlEnforced    = "enforced"
	ControlNotEnforced = "not_enforced"
)

// PostureResponse answers "is anything untoward happening?" with a state and
// the evidence it rests on. It never reports a bare success: every state
// lists its reasons.
type PostureResponse struct {
	Namespace   string    `json:"namespace"`
	Window      string    `json:"window"`
	GeneratedAt time.Time `json:"generatedAt"`
	Mode        Mode      `json:"mode,omitempty"`
	// Confidence is the overall state. While no action can be held it is at
	// most degraded.
	Confidence PostureState `json:"confidence"`
	// Visibility is whether the monitor saw everything it was asked to: full,
	// partial or blind.
	Visibility PostureState `json:"visibility"`
	// Control is enforced only when oversight can hold actions.
	Control          PostureState      `json:"control"`
	CharterRevisions []string          `json:"charterRevisions,omitempty"`
	ConcernsRevision []string          `json:"concernsRevisions,omitempty"`
	Basis            PostureBasis      `json:"basis"`
	Producers        []PostureProducer `json:"producers,omitempty"`
	// Incomplete says the window held more decisions than were read.
	Incomplete bool `json:"incomplete,omitempty"`
}

// PostureState is a state, a plain summary and the reasons for it.
type PostureState struct {
	State   string   `json:"state"`
	Summary string   `json:"summary"`
	Reasons []string `json:"reasons,omitempty"`
}

// PostureBasis is the evidence behind a posture.
type PostureBasis struct {
	Actions         int             `json:"actions"`
	Tasks           int             `json:"tasks"`
	Agents          int             `json:"agents"`
	Dispositions    map[string]int  `json:"dispositions"`
	Signals         map[string]int  `json:"signals"`
	WouldHavePaused int             `json:"wouldHavePaused"`
	CharterFindings int             `json:"charterFindings"`
	Unreviewed      int             `json:"unreviewedFlagged"`
	FalseAlarms     int             `json:"falseAlarms"`
	RealProblems    int             `json:"realProblems"`
	Coverage        PostureCoverage `json:"coverage"`
	LastObservedAt  *time.Time      `json:"lastObservedAt,omitempty"`
}

// PostureCoverage is how much of the window's actions were seen.
type PostureCoverage struct {
	MaxEvidenceLevel   EvidenceLevel `json:"maxEvidenceLevel,omitempty"`
	StreamOK           int           `json:"streamOK"`
	SensorAttributed   int           `json:"sensorAttributed"`
	UnattributedEvents int           `json:"unattributedEvents"`
	// ObservedEvents sums the effect counts the sensor attributed to the
	// window's actions: the raw events behind them.
	ObservedEvents int            `json:"observedEvents"`
	BlindSpots     map[string]int `json:"blindSpots,omitempty"`
}

// PostureProducer is one scorer or advisor seen in the window.
type PostureProducer struct {
	Role        string `json:"role"`
	ProviderRef string `json:"providerRef"`
	ModelID     string `json:"modelID"`
	TrustState  string `json:"trustState,omitempty"`
	Evaluations int    `json:"evaluations"`
	Failures    int    `json:"failures"`
}

// ProducerMetric is one measured number an operator declared for a producer,
// such as a held-out catch rate. Names and units come from the declaration.
type ProducerMetric struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit,omitempty"`
}

// ProducerInfo is one scorer, advisor or rule producer: how the operator
// declared it, and how it behaved in the posture window.
//
// Phase 0 reads declarations from an operator-owned ConfigMap. That is
// provisional: the eventual home is an admin-owned provider resource whose
// trust state Orka earns through qualification. Until then a declared trust
// state is only the operator's claim, and TrustVerified is always false.
type ProducerInfo struct {
	ProviderRef string `json:"providerRef"`
	// Label is the operator's display name; clients fall back to ProviderRef.
	Label       string `json:"label,omitempty"`
	Role        string `json:"role,omitempty"`
	ModelID     string `json:"modelID,omitempty"`
	ModelFamily string `json:"modelFamily,omitempty"`
	ModelDigest string `json:"modelDigest,omitempty"`
	// DeclaredTrustState is what the declaration claims; TrustVerified says
	// whether Orka verified it.
	DeclaredTrustState string `json:"declaredTrustState,omitempty"`
	TrustVerified      bool   `json:"trustVerified"`
	// ObservedTrustStates are the trust states decisions in the window carried.
	ObservedTrustStates []string         `json:"observedTrustStates,omitempty"`
	EvalRunID           string           `json:"evalRunID,omitempty"`
	SampleSize          int              `json:"sampleSize,omitempty"`
	Metrics             []ProducerMetric `json:"metrics,omitempty"`
	Note                string           `json:"note,omitempty"`
	// Evaluations and Failures count the window's decisions it took part in.
	Evaluations int `json:"evaluations"`
	Failures    int `json:"failures"`
}

// ProducersResponse lists the namespace's producers.
type ProducersResponse struct {
	Namespace string `json:"namespace"`
	// Declared says whether an operator declaration was found.
	Declared  bool           `json:"declared"`
	Producers []ProducerInfo `json:"producers"`
}

// CharterBound is one bound of a charter, as the charter states it.
type CharterBound struct {
	ID       string `json:"id"`
	Sentence string `json:"sentence"`
	// Highlight marks a bound an operator wants shown first.
	Highlight bool `json:"highlight,omitempty"`
}

// CharterDeclaration is the text of one charter revision. Phase 0 reads it
// from an operator-owned declaration; nothing here enforces a bound.
type CharterDeclaration struct {
	Revision string `json:"revision"`
	Title    string `json:"title,omitempty"`
	// Preview marks a charter whose rules are a preview check, audit only.
	Preview bool           `json:"preview,omitempty"`
	Bounds  []CharterBound `json:"bounds"`
}

// ConfigResponse is GET /api/v1/oversight/config.
type ConfigResponse struct {
	QueryKeys       []string `json:"queryKeys"`
	MaxBulkFeedback int      `json:"maxBulkFeedback"`
	// DemoCharterRevisions lists the preview charter revisions.
	DemoCharterRevisions []string             `json:"demoCharterRevisions"`
	Charters             []CharterDeclaration `json:"charters"`
	// Context is what an operator declared about this environment, such as
	// "Workload: synthetic fleet". Orka shows it as declared; it checks none of it.
	Context []ContextLabel `json:"context"`
	// Concerns names the questions the monitor asks, so a client can show
	// "Unrequested destination" rather than a question ID.
	Concerns []ConcernDeclaration `json:"concerns"`
}

// ConcernDeclaration is one question a scorer answers, as an operator named it.
type ConcernDeclaration struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Meaning string `json:"meaning,omitempty"`
}

// ContextLabel is one operator-declared fact about the environment.
type ContextLabel struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
