package v1

import (
	"context"
	"time"
)

// OversightDecisionRecord is a stored oversight decision. The store indexes
// the listed fields and keeps Body, the decision's canonical JSON, as given:
// the decision schema is owned by the oversight contract, not by storage.
// Every text field is already redacted and bounded by the caller.
type OversightDecisionRecord struct {
	Namespace string
	ID        string
	// Seq is assigned by the store: it increases with every stored decision,
	// across namespaces, in commit order.
	Seq       int64
	TaskName  string
	AgentName string
	// WorkloadIdentity is the subject the action ran as, such as a
	// ServiceAccount or workload identity.
	WorkloadIdentity string
	RunID            string
	ToolName         string
	Disposition      string
	WouldHavePaused  bool
	MaxScore         float64
	SignatureHash    string
	ObservedAt       time.Time
	// RecordedAt is assigned by the store.
	RecordedAt time.Time
	// Facets index the decision for exact and prefix matches, such as
	// {"concern", "K2"} or {"dest", "example.com:443"}.
	Facets []OversightFacet
	// SearchText is matched by free-text terms; at most
	// MaxOversightSearchTextBytes. It and ActionNarrative are derived from
	// Body by a fixed template and may be rebuilt; Digest does not cover them.
	SearchText string
	// ActionNarrative is a deterministic plain-language account of the
	// action, at most MaxOversightNarrativeBytes.
	ActionNarrative string
	// Digest identifies Body; a repeat with the same ID and Digest is a
	// duplicate, and with another Digest a mismatch.
	Digest string
	Body   []byte
}

// OversightFacet is one indexed kind and value of a decision.
type OversightFacet struct {
	Kind  string
	Value string
}

// Limits of an oversight decision record.
const (
	MaxOversightBodyBytes       = 64 << 10
	MaxOversightSearchTextBytes = 8 << 10
	MaxOversightNarrativeBytes  = 2 << 10
	MaxOversightFacets          = 256
	MaxOversightBatch           = 100
	MaxOversightListLimit       = 1000
)

// OversightFacetMatch matches decisions with a facet of Kind whose value is
// Value or, with Glob, matches Value where each "*" stands for any text.
type OversightFacetMatch struct {
	Kind  string
	Value string
	Glob  bool
}

// Feedback filters for OversightDecisionFilter.Feedback.
const (
	OversightFeedbackUnreviewed = "unreviewed"
	OversightFeedbackReviewed   = "reviewed"
)

// OversightDecisionFilter selects decisions of one namespace. Every set field
// must match.
type OversightDecisionFilter struct {
	Namespace          string
	IDs                []string
	TaskNames          []string
	TaskPrefix         string
	AgentNames         []string
	WorkloadIdentities []string
	RunID              string
	ToolNames          []string
	Dispositions       []string
	// WouldHavePaused, when set, matches its value.
	WouldHavePaused *bool
	MinScore        *float64
	SignatureHash   string
	// Facets must all match; AnyFacets match when at least one does.
	Facets    []OversightFacetMatch
	AnyFacets []OversightFacetMatch
	// Text terms must all occur in SearchText, compared case-insensitively.
	Text []string
	// Feedback is OversightFeedbackUnreviewed, OversightFeedbackReviewed or a
	// label the latest feedback must carry.
	Feedback string
	// ObservedSince and ObservedBefore bound ObservedAt.
	ObservedSince  time.Time
	ObservedBefore time.Time
	// AfterSeq and BeforeSeq bound Seq exclusively; zero is unbounded.
	AfterSeq  int64
	BeforeSeq int64
	// Descending lists the newest first.
	Descending bool
	// Limit is at most MaxOversightListLimit; zero means that maximum.
	Limit int
}

// OversightAppendResult reports one appended decision.
type OversightAppendResult struct {
	ID        string
	Seq       int64
	Duplicate bool
}

// OversightFeedbackRecord is one outcome label recorded for a decision. Labels
// form a ledger; the latest for a decision is its current label.
type OversightFeedbackRecord struct {
	Namespace  string
	DecisionID string
	// Seq is assigned by the store.
	Seq        int64
	Label      string
	Note       string
	Reviewer   string
	RecordedAt time.Time
}

// OversightStore keeps oversight decisions and their outcome labels.
type OversightStore interface {
	// AppendOversightDecisions stores up to MaxOversightBatch decisions of one
	// namespace in one transaction. A decision whose ID is stored with the
	// same Digest is reported as a duplicate with its stored Seq. A stored ID
	// with another Digest returns ErrDuplicateMismatch and stores nothing; an
	// invalid or oversized record returns ErrValidation or ErrTooLarge.
	AppendOversightDecisions(ctx context.Context, namespace string, records []OversightDecisionRecord) ([]OversightAppendResult, error)
	// GetOversightDecision returns ErrNotFound for an unknown decision.
	GetOversightDecision(ctx context.Context, namespace, id string) (OversightDecisionRecord, error)
	// ListOversightDecisions returns matching decisions in Seq order.
	ListOversightDecisions(ctx context.Context, filter OversightDecisionFilter) ([]OversightDecisionRecord, error)
	// RecordOversightFeedback appends labels for stored decisions of one
	// namespace in one transaction, assigning each Seq and RecordedAt. It
	// returns ErrNotFound, storing nothing, when a decision is unknown.
	RecordOversightFeedback(ctx context.Context, namespace string, records []OversightFeedbackRecord, at time.Time) ([]OversightFeedbackRecord, error)
	// ListLatestOversightFeedback returns the latest label of each named
	// decision that has one.
	ListLatestOversightFeedback(ctx context.Context, namespace string, decisionIDs []string) (map[string]OversightFeedbackRecord, error)
}
