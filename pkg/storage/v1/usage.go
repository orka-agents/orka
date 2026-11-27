package v1

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

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
	Requester       *UsageRequester    `json:"requester,omitempty"`
	AHPTurn         bool               `json:"ahpTurn,omitempty"`
}

type UsageRequester struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

type UsageGatewayOwner struct {
	Namespace    string `json:"namespace"`
	NamespaceUID string `json:"namespaceUID"`
	Name         string `json:"name"`
	UID          string `json:"uid"`
}

type UsageTaskPhase struct {
	Phase      string    `json:"phase"`
	ObservedAt time.Time `json:"observedAt"`
	RecordedAt time.Time `json:"recordedAt,omitzero"`
	Attempt    int32     `json:"attempt,omitempty"`
}

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
	Namespaces    []string          `json:"teams"`
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
	Works                  []UsageWorkRequest
	Tasks                  []UsageTask
	Observations           []UsageObservation
	Links                  []UsagePRLink
	PullRequests           []UsagePullRequest
	RetainedSince          *time.Time
	HiddenTasks            map[string]bool
	HiddenSessionBaselines map[string]bool
}

type UsageStore interface {
	RegisterUsageWork(context.Context, UsageWorkRequest) error
	RegisterUsageTask(context.Context, UsageTask) error
	RecordUsage(context.Context, UsageObservation) error
	LinkUsagePullRequest(context.Context, UsagePRLink) error
	RecordUsagePullRequest(context.Context, UsagePullRequest) error
	LoadUsage(context.Context, UsageFilter) (UsageData, error)
	ListUsagePullRequestLinks(context.Context, string, string, time.Time, int) ([]UsagePRLink, error)
	ListUsageTasksInSessions(ctx context.Context, namespace, namespaceUID string, sessions []string, startedBy time.Time, limit int) ([]UsageTask, error)
	PruneUsage(context.Context, time.Time) error
}

type TaskUsageObservationReader interface {
	ListTaskCounterObservations(ctx context.Context, namespace, taskUID string) ([]UsageObservation, error)
}

var ErrUsageSelectionTooLarge = errors.New("usage report selection is too large")

func ValidateUsageObservation(observation UsageObservation) error {
	if observation.ID == "" || observation.Namespace == "" || observation.CounterID == "" || observation.ObservedAt.IsZero() {
		return ValidationErrorf("usage observation requires identity, namespace, counter and timestamp")
	}
	switch observation.Scope {
	case UsageScopeCall, UsageScopeAttempt, UsageScopeSession:
	default:
		return ValidationErrorf("unsupported usage counter scope")
	}
	switch observation.Source {
	case UsageSourceProvider, UsageSourceAgent, UsageSourceEstimate:
	default:
		return ValidationErrorf("unsupported usage source")
	}
	for _, count := range []*int64{observation.InputTokens, observation.OutputTokens, observation.CachedInputTokens, observation.CacheWriteInputTokens} {
		if count != nil && (*count < 0 || *count > MaxUsageTokenCount) {
			return ValidationErrorf("invalid usage count")
		}
	}
	if observation.InputTokens != nil {
		remainingInput := *observation.InputTokens
		for _, count := range []*int64{observation.CachedInputTokens, observation.CacheWriteInputTokens} {
			if count == nil {
				continue
			}
			if *count > remainingInput {
				return ValidationErrorf("cache usage exceeds inclusive input count")
			}
			remainingInput -= *count
		}
	}
	return nil
}

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

func UsageWorkID(namespace, monitorUID, repository, kind string, number int64) string {
	repository = strings.ToLower(strings.TrimSpace(repository))
	data, _ := json.Marshal([]any{namespace, monitorUID, repository, kind, number})
	digest := sha256.Sum256(data)
	return "work-" + hex.EncodeToString(digest[:16])
}
