package v1

import (
	"context"
	"time"
)

// GatewaySessionCleanupProof freezes the owner and retention boundary of one
// Gateway transcript. It is internal cleanup authority, never a Session API flag.
type GatewaySessionCleanupProof struct {
	GatewayUID     string    `json:"gatewayUid"`
	BindingUID     string    `json:"bindingUid"`
	CreatedAt      time.Time `json:"createdAt"`
	TerminalCutoff time.Time `json:"terminalCutoff"`
}

// GatewaySessionCleanupCandidate identifies an inactive retained transcript.
// Eligibility is checked again in the transaction that persists its intent.
type GatewaySessionCleanupCandidate struct {
	Namespace   string                     `json:"namespace"`
	SessionName string                     `json:"sessionName"`
	SessionUID  string                     `json:"sessionUid,omitempty"`
	Proof       GatewaySessionCleanupProof `json:"proof"`
}

// GatewaySessionCleanupFilter selects a bounded page in namespace/Session name
// order. The cursor is exclusive; both cursor fields must be set or both empty.
// Limit must be between 1 and 100.
type GatewaySessionCleanupFilter struct {
	Namespace        string
	TerminalCutoff   time.Time
	AfterNamespace   string
	AfterSessionName string
	Limit            int
}

// GatewaySessionCleanupPage reports progress through the raw candidate rows,
// including rows skipped for malformed ownership. NextNamespace and
// NextSessionName identify the last scanned row. Complete means fewer than
// Limit rows were scanned; a full final page requires one more empty read.
type GatewaySessionCleanupPage struct {
	Candidates      []GatewaySessionCleanupCandidate
	NextNamespace   string
	NextSessionName string
	Complete        bool
}

type GatewaySessionCleanupCandidateStore interface {
	ListGatewaySessionCleanupCandidates(context.Context, GatewaySessionCleanupFilter) (GatewaySessionCleanupPage, error)
}
