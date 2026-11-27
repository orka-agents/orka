package v1

import (
	"context"
	"errors"
	"time"
)

var ErrGatewaySessionCleanupPending = errors.New("gateway session cleanup is pending")

type GatewaySessionCleanupProof struct {
	GatewayUID     string    `json:"gatewayUid"`
	BindingUID     string    `json:"bindingUid"`
	CreatedAt      time.Time `json:"createdAt"`
	TerminalCutoff time.Time `json:"terminalCutoff"`
}

type GatewaySessionCleanupCandidate struct {
	Namespace   string                     `json:"namespace"`
	SessionName string                     `json:"sessionName"`
	SessionUID  string                     `json:"sessionUid,omitempty"`
	Proof       GatewaySessionCleanupProof `json:"proof"`
}

type GatewaySessionCleanupFilter struct {
	Namespace        string
	TerminalCutoff   time.Time
	AfterNamespace   string
	AfterSessionName string
	Limit            int
}

type GatewaySessionCleanupPage struct {
	Candidates      []GatewaySessionCleanupCandidate
	NextNamespace   string
	NextSessionName string
	Complete        bool
}

type GatewaySessionCleanupCandidateStore interface {
	ListGatewaySessionCleanupCandidates(context.Context, GatewaySessionCleanupFilter) (GatewaySessionCleanupPage, error)
}
