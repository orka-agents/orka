package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var ErrUsageSelectionTooLarge = errors.New("usage report selection is too large")

// MaxUsageTokenCount is the largest exactly representable JSON integer for UI clients.
const MaxUsageTokenCount int64 = 1<<53 - 1

const (
	UsageSourceProvider  = "provider"
	UsageSourceAgent     = "agent"
	UsageSourceEstimate  = "estimate"
	UsageScopeCall       = "call"
	UsageScopeAttempt    = "attempt"
	UsageScopeSession    = "session"
	UsageStatusStarted   = "started"
	UsageStatusCompleted = "completed"
	UsageStatusFailed    = "failed"
	UsageStatusCancelled = "cancelled"
	UsagePRCreated       = "created"
	UsagePRAssisted      = "assisted"
	UsagePRReview        = "review_only"
)

// UsageWorkRequest identifies an original monitor request, before model work.
// Namespace is the team boundary. It is supplied by the controller, never by
// model output or caller-supplied billing metadata.
type UsageWorkRequest struct {
	ID           string    `json:"id"`
	Namespace    string    `json:"namespace"`
	NamespaceUID string    `json:"namespaceUID,omitempty"`
	MonitorName  string    `json:"monitorName"`
	MonitorUID   string    `json:"monitorUID"`
	Repository   string    `json:"repository"`
	Kind         string    `json:"kind"`
	Number       int64     `json:"number"`
	StartedAt    time.Time `json:"startedAt"`
}

// UsageWorkID keeps retries and follow-up commands for one issue together.
func UsageWorkID(namespace, monitorUID, repository, kind string, number int64) string {
	repository = strings.ToLower(strings.TrimSpace(repository))
	data, _ := json.Marshal([]any{namespace, monitorUID, repository, kind, number})
	digest := sha256.Sum256(data)
	return "work-" + hex.EncodeToString(digest[:16])
}

// UsageTask retains only identifiers, classification and execution state.
// WorkID is set by monitor task creation or inherited from a verified parent.
type UsageTask struct {
	Namespace       string             `json:"namespace"`
	NamespaceUID    string             `json:"namespaceUID,omitempty"`
	TaskUID         string             `json:"taskUID"`
	TaskName        string             `json:"taskName"`
	SessionName     string             `json:"sessionName,omitempty"`
	ParentTaskUID   string             `json:"parentTaskUID,omitempty"`
	WorkID          string             `json:"workID,omitempty"`
	Repository      string             `json:"repository,omitempty"`
	PRNumber        int64              `json:"prNumber,omitempty"`
	Phase           string             `json:"phase"`
	Role            string             `json:"role,omitempty"`
	Runtime         string             `json:"runtime,omitempty"`
	StartedAt       time.Time          `json:"startedAt"`
	PhaseHistory    []UsageTaskPhase   `json:"phaseHistory,omitempty"`
	PhaseObservedAt time.Time          `json:"-"`
	PhaseAttempt    int32              `json:"-"`
	GatewayOwner    *UsageGatewayOwner `json:"gatewayOwner,omitempty"`
}

// UsageGatewayOwner retains the immutable access boundary after Task cleanup.
type UsageGatewayOwner struct {
	Namespace    string `json:"namespace"`
	NamespaceUID string `json:"namespaceUID"`
	Name         string `json:"name"`
	UID          string `json:"uid"`
}

// UsageTaskPhase retains the lifecycle timestamp in ObservedAt for ordering
// and retention. RecordedAt is when the store first learned this state. Legacy
// entries without RecordedAt remain visible at their original ObservedAt.
type UsageTaskPhase struct {
	Phase      string    `json:"phase"`
	ObservedAt time.Time `json:"observedAt"`
	RecordedAt time.Time `json:"recordedAt,omitzero"`
	Attempt    int32     `json:"attempt,omitempty"`
}

// UsageObservation is a counts-only snapshot. CounterID identifies a cumulative
// counter, not an event: repeated snapshots and final summaries replace its
// high-water mark. A different provider call must use a different counter.
// InputTokens includes cache reads/writes; those fields are breakdowns only.
// Nil counts mean unavailable, including subscriptions without reported usage.
type UsageObservation struct {
	ID                    string    `json:"id"`
	Namespace             string    `json:"namespace"`
	NamespaceUID          string    `json:"namespaceUID,omitempty"`
	TaskUID               string    `json:"taskUID,omitempty"`
	TaskName              string    `json:"taskName,omitempty"`
	SessionName           string    `json:"sessionName,omitempty"`
	AttemptID             string    `json:"attemptID,omitempty"`
	CounterID             string    `json:"counterID"`
	Scope                 string    `json:"scope"`
	Source                string    `json:"source"`
	Provider              string    `json:"provider,omitempty"`
	Model                 string    `json:"model,omitempty"`
	InputTokens           *int64    `json:"inputTokens"`
	OutputTokens          *int64    `json:"outputTokens"`
	CachedInputTokens     *int64    `json:"cachedInputTokens"`
	CacheWriteInputTokens *int64    `json:"cacheWriteInputTokens"`
	Status                string    `json:"status"`
	Complete              bool      `json:"complete"`
	ObservedAt            time.Time `json:"observedAt"`
}

// UsagePRLink is written only after a controller-owned GitHub publication.
type UsagePRLink struct {
	Namespace    string    `json:"namespace"`
	NamespaceUID string    `json:"namespaceUID,omitempty"`
	WorkID       string    `json:"workID"`
	Repository   string    `json:"repository"`
	Number       int64     `json:"number"`
	Origin       string    `json:"origin"`
	EvidenceID   string    `json:"evidenceID"`
	LinkedAt     time.Time `json:"linkedAt"`
}

// UsagePullRequest is an authenticated GitHub observation, including readiness
// of exactly HeadSHA. Observations are retained so an as-of report is repeatable.
type UsagePullRequest struct {
	Namespace       string     `json:"namespace"`
	NamespaceUID    string     `json:"namespaceUID,omitempty"`
	Repository      string     `json:"repository"`
	Number          int64      `json:"number"`
	GitHubID        string     `json:"githubID,omitempty"`
	URL             string     `json:"url"`
	HeadSHA         string     `json:"headSHA,omitempty"`
	State           string     `json:"state"`
	Ready           bool       `json:"ready"`
	ReadinessReason string     `json:"readinessReason,omitempty"`
	CreatedAt       *time.Time `json:"createdAt,omitempty"`
	MergedAt        *time.Time `json:"mergedAt,omitempty"`
	ClosedAt        *time.Time `json:"closedAt,omitempty"`
	ObservedAt      time.Time  `json:"observedAt"`
}

type UsageFilter struct {
	Namespaces []string `json:"teams"`
	// NamespaceUIDs is supplied by the server after live namespace lookup.
	NamespaceUIDs map[string]string `json:"-"`
	WorkID        string            `json:"-"`
	MaxRecords    int               `json:"-"`
	Repository    string            `json:"repository,omitempty"`
	Kind          string            `json:"kind,omitempty"`
	Model         string            `json:"model,omitempty"`
	From          time.Time         `json:"from"`
	Until         time.Time         `json:"until"`
	AsOf          time.Time         `json:"asOf"`
}

type UsageData struct {
	Works []UsageWorkRequest
	Tasks []UsageTask
	// Observations with equal timestamps are in durable accounting order.
	Observations  []UsageObservation
	Links         []UsagePRLink
	PullRequests  []UsagePullRequest
	RetainedSince *time.Time
	// HiddenTasks are accounted for only as cumulative-counter baselines. Their
	// counts and identifiers must not appear in the public report.
	HiddenTasks map[string]bool
}

// UsageStore is independent of task/transcript cleanup. Implementations must
// make repeated observations idempotent and preserve immutable work ownership.
type UsageStore interface {
	RegisterUsageWork(context.Context, UsageWorkRequest) error
	RegisterUsageTask(context.Context, UsageTask) error
	RecordUsage(context.Context, UsageObservation) error
	LinkUsagePullRequest(context.Context, UsagePRLink) error
	RecordUsagePullRequest(context.Context, UsagePullRequest) error
	LoadUsage(context.Context, UsageFilter) (UsageData, error)
	ListUsagePullRequestLinks(context.Context, string, string, time.Time, int) ([]UsagePRLink, error)
	PruneUsage(context.Context, time.Time) error
}
