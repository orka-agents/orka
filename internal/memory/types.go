package memory

import (
	"time"

	"github.com/orka-agents/orka/internal/store"
)

const (
	ReasonBackendUnavailable     = "MEMORY_BACKEND_UNAVAILABLE"
	ReasonBackendReadOnly        = "MEMORY_BACKEND_READ_ONLY"
	ReasonBackendDisabled        = "MEMORY_BACKEND_DISABLED"
	ReasonBackendRemoved         = "MEMORY_BACKEND_REMOVED"
	ReasonOperationInProgress    = "MEMORY_OPERATION_IN_PROGRESS"
	ReasonIdempotencyKeyRequired = "MEMORY_IDEMPOTENCY_KEY_REQUIRED"
	ReasonIdempotencyKeyReuse    = "MEMORY_IDEMPOTENCY_KEY_REUSE"
	ReasonPending                = "MEMORY_PENDING"
	ReasonDiverged               = "MEMORY_DIVERGED"
	ReasonIdentityMismatch       = "MEMORY_IDENTITY_MISMATCH"
	ReasonResultSetIncomplete    = "MEMORY_RESULT_SET_INCOMPLETE"
	ReasonSearchModeUnsupported  = "MEMORY_SEARCH_MODE_UNSUPPORTED"
	ReasonSearchRemoteAuth       = "MEMORY_SEARCH_REMOTE_AUTH_REQUIRED"
)

// CreateRequest contains only caller-owned memory fields.
type CreateRequest struct {
	ID          string   `json:"id,omitempty"`
	Namespace   string   `json:"namespace,omitempty"`
	SessionName string   `json:"sessionName,omitempty"`
	AgentName   string   `json:"agentName,omitempty"`
	TaskName    string   `json:"taskName,omitempty"`
	ParentTask  string   `json:"parentTask,omitempty"`
	Source      string   `json:"source,omitempty"`
	Content     string   `json:"content"`
	Tags        []string `json:"tags,omitempty"`
}

// UpdateRequest is a partial update over caller-owned mutable fields. Pointer
// fields preserve the distinction between absent and explicitly empty input.
type UpdateRequest struct {
	Namespace   string    `json:"namespace,omitempty"`
	SessionName *string   `json:"sessionName,omitempty"`
	AgentName   *string   `json:"agentName,omitempty"`
	TaskName    *string   `json:"taskName,omitempty"`
	ParentTask  *string   `json:"parentTask,omitempty"`
	Content     *string   `json:"content,omitempty"`
	Source      *string   `json:"source,omitempty"`
	Tags        *[]string `json:"tags,omitempty"`
}

// SearchRequest is the explicit bounded search API request.
type SearchRequest struct {
	Query           string              `json:"query"`
	Tags            []string            `json:"tags,omitempty"`
	IDs             []string            `json:"ids,omitempty"`
	Sources         []string            `json:"sources,omitempty"`
	SessionName     string              `json:"sessionName,omitempty"`
	TaskName        string              `json:"taskName,omitempty"`
	ParentTask      string              `json:"parentTask,omitempty"`
	AgentName       string              `json:"agentName,omitempty"`
	Trust           []store.MemoryTrust `json:"trust,omitempty"`
	Limit           int                 `json:"limit,omitempty"`
	Cursor          string              `json:"cursor,omitempty"`
	Mode            string              `json:"mode,omitempty"`
	AllowIncomplete bool                `json:"allowIncomplete,omitempty"`
	IncludeDisabled bool                `json:"includeDisabled,omitempty"`
	IncludeDeleted  bool                `json:"includeDeleted,omitempty"`
}

// SearchContext carries authenticated server-derived audit metadata and an
// authorization check that is invoked only when the selected authority will
// actually perform remote search egress.
type SearchContext struct {
	Actor              string
	RequestID          string
	RemoteAuthorized   bool
	AuthorizeRemote    func() error
	PreserveEmptyTrust bool
}

// SearchHit is one verified remote or legacy search result.
type SearchHit struct {
	Memory store.Memory `json:"memory"`
	Score  float64      `json:"score,omitempty"`
}

// SearchResponse reports the actual provider mode and explicit completeness.
type SearchResponse struct {
	Items      []SearchHit `json:"items"`
	ActualMode string      `json:"actualMode"`
	Cursor     string      `json:"cursor,omitempty"`
	Exhausted  bool        `json:"exhausted"`
	Complete   bool        `json:"complete"`
}

// ListPage is the bounded deterministic memory-list result. Paginated is false
// only for the unchanged legacy SQLite path, whose historical response shape
// and behavior remain intact.
type ListPage struct {
	Items     []store.Memory
	Cursor    string
	Exhausted bool
	Complete  bool
	Paginated bool
}

// IncompleteSearchError reports strict-mode scan-budget exhaustion while
// preserving an opaque, authority-bound continuation cursor for the caller.
type IncompleteSearchError struct {
	Cause  error
	Cursor string
}

func (e *IncompleteSearchError) Error() string {
	if e == nil || e.Cause == nil {
		return "memory search result set is incomplete"
	}
	return e.Cause.Error()
}

func (e *IncompleteSearchError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// SafeCursor returns the opaque continuation token safe to expose in an API error.
func (e *IncompleteSearchError) SafeCursor() string {
	if e == nil {
		return ""
	}
	return e.Cursor
}

// MutationContext carries authenticated, server-derived request metadata.
type MutationContext struct {
	Principal      string
	Actor          string
	IdempotencyKey string
	Route          string
	RequestID      string
	Reason         string
	LocationBase   string
}

// MutationResult is either an immediately materialized Memory or a durable operation.
type MutationResult struct {
	Memory     *store.Memory
	Operation  *Operation
	StatusCode int
	Location   string
	RetryAfter time.Duration
	Replayed   bool
}

// BackendActionRequest is used by audited lifecycle and administrative routes.
type BackendActionRequest struct {
	Reason string `json:"reason"`
	DryRun bool   `json:"dryRun,omitempty"`
}

// TrustRequest changes only the server-owned local trust overlay.
type TrustRequest struct {
	Trust  store.MemoryTrust `json:"trust"`
	Reason string            `json:"reason"`
}

// TrustContext carries authenticated server-derived audit metadata and an
// authorization check used only when a durable remote binding exists.
type TrustContext struct {
	Actor           string
	RequestID       string
	AuthorizeRemote func() error
}

// Operation is the public allowlisted durable operation representation.
type Operation struct {
	ID                string                     `json:"id"`
	Namespace         string                     `json:"namespace"`
	MemoryID          string                     `json:"memoryId"`
	ProposalID        string                     `json:"proposalId,omitempty"`
	Kind              store.MemoryOperationKind  `json:"kind"`
	DesiredGeneration int64                      `json:"desiredGeneration"`
	State             store.MemoryOperationState `json:"state"`
	Attempts          int                        `json:"attempts"`
	NextRetryAt       time.Time                  `json:"nextRetryAt,omitempty"`
	ErrorCode         string                     `json:"errorCode,omitempty"`
	ErrorMessage      string                     `json:"errorMessage,omitempty"`
	AppliedGeneration int64                      `json:"appliedGeneration,omitempty"`
	BackendVersion    string                     `json:"backendVersion,omitempty"`
	ContentDigest     string                     `json:"contentDigest,omitempty"`
	MutationDigest    string                     `json:"mutationDigest,omitempty"`
	CreatedAt         time.Time                  `json:"createdAt"`
	UpdatedAt         time.Time                  `json:"updatedAt"`
	CompletedAt       *time.Time                 `json:"completedAt,omitempty"`
}

// OperationFromStore removes backend binding and provider-local identity fields.
func OperationFromStore(operation store.MemoryOperation) Operation {
	return Operation{
		ID: operation.ID, Namespace: operation.Namespace, MemoryID: operation.MemoryID,
		ProposalID: operation.ProposalID, Kind: operation.Kind, DesiredGeneration: operation.DesiredGeneration,
		State: operation.State, Attempts: operation.Attempts, NextRetryAt: operation.NextRetryAt,
		ErrorCode: operation.ErrorCode, ErrorMessage: operation.ErrorMessage,
		AppliedGeneration: operation.ReceiptAppliedGeneration, BackendVersion: operation.ReceiptBackendVersion,
		ContentDigest: operation.ReceiptContentDigest, MutationDigest: operation.ReceiptMutationDigest,
		CreatedAt: operation.CreatedAt, UpdatedAt: operation.UpdatedAt, CompletedAt: operation.CompletedAt,
	}
}
