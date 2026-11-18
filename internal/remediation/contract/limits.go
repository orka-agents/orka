package contract

import (
	"context"
	"time"
)

const (
	MaxRunDuration = 90 * time.Minute
	MaxModelCalls  = 10

	// BoundedModelRequestsEnv limits actual outbound model HTTP attempts, not
	// logical Tasks or agent-loop iterations. An absent value preserves legacy
	// worker behavior.
	BoundedModelRequestsEnv = "ORKA_MODEL_MAX_REQUESTS"
	ModelBudgetRunEnv       = "ORKA_MODEL_BUDGET_RUN"
	ModelBudgetRoleEnv      = "ORKA_MODEL_BUDGET_ROLE"
	ModelBudgetDeadlineEnv  = "ORKA_MODEL_BUDGET_DEADLINE"
)

// ModelCallClaim is derived from TokenReview-authenticated Pod/Job/Task
// identities by the internal API, never from a caller's request body.
type ModelCallClaim struct {
	Namespace string
	TaskName  string
	TaskUID   string
	JobUID    string
	PodUID    string
}

type ModelCallBudgetService interface {
	ClaimModelCall(context.Context, ModelCallClaim) error
}
