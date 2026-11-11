// Package contract contains the standalone remediation request and execution
// boundary shared by the coordinator, API, and trusted validation worker.
package contract

import (
	"context"
	"encoding/json"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
)

type Mode string

const (
	Generate Mode = "generate"
	Validate Mode = "validate"
)

type Request struct {
	RequestID             string          `json:"requestID"`
	Mode                  Mode            `json:"mode"`
	Report                json.RawMessage `json:"report"`
	Repository            string          `json:"repository"`
	Ref                   string          `json:"ref"`
	RequiredEvidenceScope EvidenceScope   `json:"requiredEvidenceScope,omitempty"`
}

type Status struct {
	ID         string `json:"id"`
	Namespace  string `json:"namespace"`
	TaskName   string `json:"taskName,omitempty"`
	TaskUID    string `json:"taskUID,omitempty"`
	Phase      string `json:"phase"`
	Stage      string `json:"stage"`
	Outcome    string `json:"outcome,omitempty"`
	Reason     string `json:"reason,omitempty"`
	PatchReady bool   `json:"patchReady"`
	// EvidenceScope is the frozen backend boundary, not proof that checks ran.
	// Omission denotes a legacy run whose scope was not recorded.
	EvidenceScope EvidenceScope `json:"evidenceScope,omitempty"`
}

type Source struct {
	Repository string   `json:"repository"`
	Commit     string   `json:"commit"`
	Tree       string   `json:"tree"`
	Paths      []string `json:"paths,omitempty"`
}

type Checks struct {
	Scope        []string                    `json:"scope"`
	Gaps         []string                    `json:"gaps,omitempty"`
	Requirements []pv.EnvironmentRequirement `json:"requirements,omitempty"`
	Files        []pv.FrozenFile             `json:"files"`
	Checks       []pv.Check                  `json:"checks"`
	Services     []pv.Service                `json:"services,omitempty"`
	Application  *pv.ApplicationPlan         `json:"application,omitempty"`
}

type ExecutionRequest struct {
	ID              string              `json:"id"`
	Namespace       string              `json:"namespace"`
	ParentTaskName  string              `json:"parentTaskName"`
	ParentTaskUID   string              `json:"parentTaskUID"`
	Action          pv.Action           `json:"action"`
	Problem         string              `json:"problem"`
	Source          Source              `json:"source"`
	Checks          Checks              `json:"checks"`
	Patch           string              `json:"patch,omitempty"`
	DeclaredChanges []pv.DeclaredChange `json:"declaredChanges,omitempty"`
	EvidenceScope   EvidenceScope       `json:"evidenceScope,omitempty"`
	// ApplicationPolicy is copied from operator configuration, never a proposal.
	ApplicationPolicy *pv.ApplicationPolicy `json:"applicationPolicy,omitempty"`
	// Deadline is frozen for application execution and recovery. Omission
	// preserves legacy/source-harness request bytes and digests.
	Deadline *time.Time `json:"deadline,omitempty"`
}

type ExecutionReceipt struct {
	Namespace string `json:"namespace"`
	TaskName  string `json:"taskName"`
	TaskUID   string `json:"taskUID"`
	// InputDigest also binds EvidenceScope and an application run's deadline.
	InputDigest string `json:"inputDigest"`
}

// ExecutionRecovery returns either a durably recorded exact workload identity,
// or proof that a reservation settled without dispatch. Neither permits Start.
type ExecutionRecovery struct {
	Receipt *ExecutionReceipt
	Settled bool
}

// ExecutionBackend must persist or recover an exact workload identity before
// returning evidence. Resume never authorizes a replacement execution.
type ExecutionBackend interface {
	ConfigurationDigest() string
	RequestDigest(ExecutionRequest) (string, error)
	Start(context.Context, ExecutionRequest, func(ExecutionReceipt) error) (*pv.Record, error)
	Recover(context.Context, ExecutionRequest) (ExecutionRecovery, error)
	Resume(context.Context, ExecutionRequest, ExecutionReceipt) (*pv.Record, error)
	Cancel(context.Context, ExecutionReceipt) error
}

// ApplicationPlanningBackend exposes only operator-owned, non-secret defaults
// for model proposals. They are hints, not authority: the backend revalidates
// the complete frozen plan before every execution.
type ApplicationPlanningBackend interface {
	EvidenceScopedBackend
	ApplicationPlanDefaults() pv.ApplicationPlan
	ApplicationPolicy() *pv.ApplicationPolicy
}

type Service interface {
	Submit(context.Context, string, string, Request) (*Status, error)
	Get(context.Context, string, string) (*Status, error)
	Cancel(context.Context, string, string) (*Status, error)
	Patch(context.Context, string, string) ([]byte, error)
}
