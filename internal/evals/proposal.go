package evals

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// ProposalRequest asks for one candidate change learned from a training
// run's failed trials.
type ProposalRequest struct {
	// HeldOut names the held-out case ConfigMap the experiment runs on. Empty
	// picks the one held-out set that lists the run's monitor as a baseline
	// monitor.
	HeldOut string `json:"heldOut,omitempty"`
	// Target is the experiment target; empty targets the overall verdict.
	Target *corev1alpha1.EvalExperimentTarget `json:"target,omitempty"`
	// RequestedBy is the person who asked, recorded on the proposer Task.
	RequestedBy string `json:"requestedBy,omitempty"`
}

// ProposalRefusal is a proposal the controller declines; the API reports it
// as a conflict with its reason.
type ProposalRefusal struct{ Reason string }

func (e *ProposalRefusal) Error() string { return e.Reason }

// Proposal is one proposal and what became of it.
type Proposal struct {
	Name           string      `json:"name"`
	Phase          string      `json:"phase,omitempty"`
	State          string      `json:"state,omitempty"`
	Reason         string      `json:"reason,omitempty"`
	Hypothesis     string      `json:"hypothesis,omitempty"`
	CandidateAgent string      `json:"candidateAgent,omitempty"`
	BaselineRun    string      `json:"baselineRun,omitempty"`
	CandidateRun   string      `json:"candidateRun,omitempty"`
	HeldOut        string      `json:"heldOut,omitempty"`
	RequestedBy    string      `json:"requestedBy,omitempty"`
	CreatedAt      metav1.Time `json:"createdAt"`
}
