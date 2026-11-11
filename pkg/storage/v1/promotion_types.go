package v1

import "time"

// AgentPromotionSchemaVersion is the schema of an Agent promotion record.
const AgentPromotionSchemaVersion = "orka.agent.promotion.v1"

// Agent promotion actions.
const (
	// AgentPromotionPromote writes an approved candidate's promotable values
	// onto the stable Agent.
	AgentPromotionPromote = "promote"
	// AgentPromotionRollback writes back the values a promotion replaced.
	AgentPromotionRollback = "rollback"
)

// Agent promotion record states. A record starts pending, moves to applying
// just before the Agent is updated, and ends complete, failed or canceled.
// Every transition is a compare-and-swap on the state.
const (
	AgentPromotionPending  = "pending"
	AgentPromotionApplying = "applying"
	AgentPromotionComplete = "complete"
	AgentPromotionFailed   = "failed"
	AgentPromotionCanceled = "canceled"
)

// MaxAgentPromotionRecordBytes bounds one encoded promotion record; it
// carries two resolved system prompts. A new record must leave
// AgentPromotionTransitionReserveBytes of it free, so the state, applied
// generation and digest, and reason that transitions add always fit.
const (
	MaxAgentPromotionRecordBytes         = 256 * 1024
	AgentPromotionTransitionReserveBytes = 8 * 1024
)

// AgentPromotionValues are an Agent's promotable fields. The system prompt
// is resolved text, so a rollback does not depend on a ConfigMap that may
// have changed or gone.
type AgentPromotionValues struct {
	SystemPrompt string `json:"systemPrompt"`
	// SystemPromptConfigMap is the ConfigMap the prompt came from, if any.
	// Writing the values back keeps the reference rather than copying the
	// ConfigMap's text into the Agent, where every Agent reader could see
	// it; the write checks the ConfigMap still holds SystemPrompt.
	SystemPromptConfigMap *AgentPromotionConfigMapRef `json:"systemPromptConfigMap,omitempty"`
	Model                 string                      `json:"model,omitempty"`
}

// AgentPromotionConfigMapRef names a ConfigMap key holding a system prompt.
type AgentPromotionConfigMapRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// AgentPromotionRecord is one rollout action on a stable Agent.
type AgentPromotionRecord struct {
	SchemaVersion string `json:"schemaVersion"`
	ID            string `json:"id"`
	Namespace     string `json:"namespace"`
	Agent         string `json:"agent"`
	AgentUID      string `json:"agentUID"`
	Action        string `json:"action"`
	State         string `json:"state"`

	// DecisionID, CandidateRun and CandidateRunUID name the approval a
	// promotion carries out; CandidateAgent is the Agent it copies.
	DecisionID      string             `json:"decisionID,omitempty"`
	CandidateRun    string             `json:"candidateRun,omitempty"`
	CandidateRunUID string             `json:"candidateRunUID,omitempty"`
	CandidateAgent  *EvalAgentIdentity `json:"candidateAgent,omitempty"`
	// PromotionID is the promotion a rollback reverts.
	PromotionID string `json:"promotionID,omitempty"`

	// Previous are the Agent's values before the action and Target the
	// values it writes.
	Previous AgentPromotionValues `json:"previous"`
	Target   AgentPromotionValues `json:"target"`
	// AgentResourceVersion and AgentGeneration are the stable Agent as the
	// action read it; the update is preconditioned on that resourceVersion.
	AgentResourceVersion string `json:"agentResourceVersion"`
	AgentGeneration      int64  `json:"agentGeneration"`
	// AppliedGeneration and AppliedDigest are what the update produced.
	AppliedGeneration int64  `json:"appliedGeneration,omitempty"`
	AppliedDigest     string `json:"appliedDigest,omitempty"`

	Actor     EvalDecisionActor `json:"actor"`
	Reason    string            `json:"reason,omitempty"`
	CreatedAt time.Time         `json:"createdAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
}

// AgentPromotionTransition moves one record from one state to another.
type AgentPromotionTransition struct {
	Namespace string
	ID        string
	From      string
	To        string
	// RequireLatestApproval, for a move to applying, fails the transition
	// unless the record's decision is still the latest for its experiment
	// and approves, checked in the same transaction.
	RequireLatestApproval bool
	AppliedGeneration     int64
	AppliedDigest         string
	Reason                string
	UpdatedAt             time.Time
}
