package v1

import (
	"time"
)

// RepositoryMonitorRecord stores normalized RepositoryMonitor metadata.
type RepositoryMonitorRecord struct {
	Namespace  string    `json:"namespace"`
	Name       string    `json:"name"`
	UID        string    `json:"uid"`
	RepoURL    string    `json:"repoURL"`
	Owner      string    `json:"owner"`
	Repository string    `json:"repository"`
	Branch     string    `json:"branch"`
	Generation int64     `json:"generation"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// MonitorRun represents one scheduled, manual, or exact-event repository monitor run.
type MonitorRun struct {
	ID               string     `json:"id"`
	MonitorNamespace string     `json:"monitorNamespace"`
	MonitorName      string     `json:"monitorName"`
	Trigger          string     `json:"trigger"`
	TargetKind       string     `json:"targetKind,omitempty"`
	TargetNumber     int64      `json:"targetNumber,omitempty"`
	TargetSHA        string     `json:"targetSHA,omitempty"`
	CommandEventID   string     `json:"commandEventID,omitempty"`
	Phase            string     `json:"phase"`
	StartedAt        time.Time  `json:"startedAt"`
	CompletedAt      *time.Time `json:"completedAt,omitempty"`
	SelectedCount    int        `json:"selectedCount"`
	CreatedTaskCount int        `json:"createdTaskCount"`
	SkippedCount     int        `json:"skippedCount"`
	Error            string     `json:"error,omitempty"`
}

// MonitorRunFilter constrains monitor run list queries.
type MonitorRunFilter struct {
	Namespace    string
	MonitorName  string
	Trigger      string
	TargetKind   string
	TargetNumber int64
	TargetSHA    string
	Phase        string
	OldestFirst  bool
	Limit        int
	Cursor       string
}

// MonitorItem stores the latest state for one monitored issue, pull request, or commit.
type MonitorItem struct {
	MonitorNamespace    string    `json:"monitorNamespace"`
	MonitorName         string    `json:"monitorName"`
	Kind                string    `json:"kind"`
	ItemKey             string    `json:"itemKey"`
	Number              int64     `json:"number,omitempty"`
	SHA                 string    `json:"sha,omitempty"`
	Title               string    `json:"title,omitempty"`
	Body                string    `json:"body,omitempty"`
	HTMLURL             string    `json:"htmlURL,omitempty"`
	Author              string    `json:"author,omitempty"`
	State               string    `json:"state,omitempty"`
	LabelsJSON          string    `json:"labelsJSON,omitempty"`
	SnapshotDigest      string    `json:"snapshotDigest,omitempty"`
	GitHubUpdatedAt     time.Time `json:"githubUpdatedAt,omitempty"`
	WorkflowPhase       string    `json:"workflowPhase,omitempty"`
	LinkedPRNumber      int64     `json:"linkedPRNumber,omitempty"`
	LastCommandID       string    `json:"lastCommandID,omitempty"`
	LastCommandIntent   string    `json:"lastCommandIntent,omitempty"`
	LastActionID        string    `json:"lastActionID,omitempty"`
	LastActionKind      string    `json:"lastActionKind,omitempty"`
	LastActionTaskName  string    `json:"lastActionTaskName,omitempty"`
	BaseBranch          string    `json:"baseBranch,omitempty"`
	HeadBranch          string    `json:"headBranch,omitempty"`
	HeadSHA             string    `json:"headSHA,omitempty"`
	BaseSHA             string    `json:"baseSHA,omitempty"`
	Draft               bool      `json:"draft,omitempty"`
	MergeableState      string    `json:"mergeableState,omitempty"`
	CIState             string    `json:"ciState,omitempty"`
	SkipReason          string    `json:"skipReason,omitempty"`
	LastReviewID        string    `json:"lastReviewID,omitempty"`
	LastReviewedHeadSHA string    `json:"lastReviewedHeadSHA,omitempty"`
	LastVerdict         string    `json:"lastVerdict,omitempty"`
	RepairState         string    `json:"repairState,omitempty"`
	AutomergeState      string    `json:"automergeState,omitempty"`
	StatusCommentID     string    `json:"statusCommentID,omitempty"`
	StatusCommentURL    string    `json:"statusCommentURL,omitempty"`
	LastPublishID       string    `json:"lastPublishID,omitempty"`
	LastPublishPhase    string    `json:"lastPublishPhase,omitempty"`
	LastPublishReason   string    `json:"lastPublishReason,omitempty"`
	LastPublishURL      string    `json:"lastPublishURL,omitempty"`
	UpdatedAt           time.Time `json:"updatedAt"`
	LastSeenAt          time.Time `json:"lastSeenAt"`
}

// MonitorItemFilter constrains monitor item list queries.
type MonitorItemFilter struct {
	Namespace      string
	MonitorName    string
	Kind           string
	Number         int64
	State          string
	ReviewVerdict  string
	RepairState    string
	AutomergeState string
	Limit          int
	Cursor         string
}

// WorkAction stores one durable workflow action/lease for monitor-owned automation.
type WorkAction struct {
	ID                   string     `json:"id"`
	MonitorNamespace     string     `json:"monitorNamespace"`
	MonitorName          string     `json:"monitorName"`
	RunID                string     `json:"runID,omitempty"`
	CommandEventID       string     `json:"commandEventID,omitempty"`
	MonitorGeneration    int64      `json:"monitorGeneration,omitempty"`
	TargetKind           string     `json:"targetKind,omitempty"`
	TargetNumber         int64      `json:"targetNumber,omitempty"`
	TargetSHA            string     `json:"targetSHA,omitempty"`
	TargetSnapshotDigest string     `json:"targetSnapshotDigest,omitempty"`
	Intent               string     `json:"intent,omitempty"`
	DesiredAction        string     `json:"desiredAction,omitempty"`
	DependsOnActionID    string     `json:"dependsOnActionID,omitempty"`
	DedupeKey            string     `json:"dedupeKey,omitempty"`
	IdempotencyKey       string     `json:"idempotencyKey,omitempty"`
	Status               string     `json:"status"`
	Phase                string     `json:"phase,omitempty"`
	Attempt              int        `json:"attempt"`
	LeaseOwner           string     `json:"leaseOwner,omitempty"`
	LeaseExpiresAt       *time.Time `json:"leaseExpiresAt,omitempty"`
	TaskName             string     `json:"taskName,omitempty"`
	BlockedReason        string     `json:"blockedReason,omitempty"`
	Error                string     `json:"error,omitempty"`
	ArtifactIDs          string     `json:"artifactIDs,omitempty"`
	PayloadDigest        string     `json:"payloadDigest,omitempty"`
	MetadataJSON         string     `json:"metadataJSON,omitempty"`
	CreatedAt            time.Time  `json:"createdAt"`
	UpdatedAt            time.Time  `json:"updatedAt"`
	CompletedAt          *time.Time `json:"completedAt,omitempty"`
}

// WorkAction statuses. An active status holds the action's dedupe key: among
// the actions of one monitor that share a non-empty DedupeKey, at most one is
// active. Every other status is finished: it holds no key and
// [RepositoryMonitorStore.MarkWorkActionRetryPending] never reopens it.
//
//	queued → (leased →) running → succeeded | failed | blocked | cancelled
//	retry_pending: a handoff that failed and waits to be retried
//	completed: an action coalesced into another active action when created
const (
	WorkActionStatusQueued       = "queued"
	WorkActionStatusLeased       = "leased"
	WorkActionStatusRunning      = "running"
	WorkActionStatusRetryPending = "retry_pending"
	WorkActionStatusSucceeded    = "succeeded"
	WorkActionStatusFailed       = "failed"
	WorkActionStatusBlocked      = "blocked"
	WorkActionStatusCancelled    = "cancelled"
	WorkActionStatusCompleted    = "completed"
)

// WorkActionActiveStatuses lists the active statuses, in lookup order.
func WorkActionActiveStatuses() []string {
	return []string{WorkActionStatusQueued, WorkActionStatusLeased, WorkActionStatusRunning, WorkActionStatusRetryPending}
}

// IsWorkActionActive reports whether status is an active status.
func IsWorkActionActive(status string) bool {
	switch status {
	case WorkActionStatusQueued, WorkActionStatusLeased, WorkActionStatusRunning, WorkActionStatusRetryPending:
		return true
	default:
		return false
	}
}

// MarkWorkActionRetryPendingRequest names the work action to park for a retry
// and what to record on it.
type MarkWorkActionRetryPendingRequest struct {
	Namespace string
	ID        string
	// RunID, BlockedReason and Error replace the action's fields of the same
	// names.
	RunID         string
	BlockedReason string
	Error         string
}

// WorkActionFilter constrains workflow action list queries.
type WorkActionFilter struct {
	Namespace      string
	MonitorName    string
	TargetKind     string
	TargetNumber   int64
	TargetSHA      string
	Intent         string
	DesiredAction  string
	Status         string
	RunID          string
	CommandEventID string
	TaskName       string
	DedupeKey      string
	Limit          int
	Cursor         string
}

// ImplementationJob tracks issue coding attempts and patch validation state.
type ImplementationJob struct {
	ID                string     `json:"id"`
	MonitorNamespace  string     `json:"monitorNamespace"`
	MonitorName       string     `json:"monitorName"`
	Repo              string     `json:"repo,omitempty"`
	IssueNumber       int64      `json:"issueNumber,omitempty"`
	PlanID            string     `json:"planID,omitempty"`
	SnapshotDigest    string     `json:"snapshotDigest,omitempty"`
	Phase             string     `json:"phase,omitempty"`
	Attempt           int        `json:"attempt"`
	Branch            string     `json:"branch,omitempty"`
	PatchArtifactID   string     `json:"patchArtifactID,omitempty"`
	PRNumber          int64      `json:"prNumber,omitempty"`
	ValidationState   string     `json:"validationState,omitempty"`
	TaskName          string     `json:"taskName,omitempty"`
	MutationTaskName  string     `json:"mutationTaskName,omitempty"`
	CommandEventID    string     `json:"commandEventID,omitempty"`
	WorkActionID      string     `json:"workActionID,omitempty"`
	MonitorGeneration int64      `json:"monitorGeneration,omitempty"`
	Error             string     `json:"error,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
	CompletedAt       *time.Time `json:"completedAt,omitempty"`
}

// ImplementationJobFilter constrains implementation job list queries.
type ImplementationJobFilter struct {
	Namespace                 string
	MonitorName               string
	Repo                      string
	IssueNumber               int64
	Phase                     string
	Phases                    []string
	TaskName                  string
	ExcludeWorkActionStatuses []string
	Limit                     int
	Cursor                    string
}

// GitHubMutationRecord stores one controller-owned GitHub write audit record.
type GitHubMutationRecord struct {
	ID                string     `json:"id"`
	MonitorNamespace  string     `json:"monitorNamespace"`
	MonitorName       string     `json:"monitorName"`
	RunID             string     `json:"runID,omitempty"`
	CommandEventID    string     `json:"commandEventID,omitempty"`
	WorkActionID      string     `json:"workActionID,omitempty"`
	MonitorGeneration int64      `json:"monitorGeneration,omitempty"`
	Operation         string     `json:"operation"`
	TargetKind        string     `json:"targetKind,omitempty"`
	TargetNumber      int64      `json:"targetNumber,omitempty"`
	TargetSHA         string     `json:"targetSHA,omitempty"`
	Actor             string     `json:"actor,omitempty"`
	Reason            string     `json:"reason,omitempty"`
	RequestDigest     string     `json:"requestDigest,omitempty"`
	GitHubURL         string     `json:"githubURL,omitempty"`
	GitHubRequestID   string     `json:"githubRequestID,omitempty"`
	ExternalID        string     `json:"externalID,omitempty"`
	Status            string     `json:"status,omitempty"`
	Error             string     `json:"error,omitempty"`
	PendingAt         *time.Time `json:"pendingAt,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
}

// GitHubMutationRecordFilter constrains mutation audit list queries.
type GitHubMutationRecordFilter struct {
	Namespace string
	// AllNamespaces explicitly enables controller-wide audit lookup. API lists
	// remain namespace-scoped and must not accept this from client input.
	AllNamespaces bool
	MonitorName   string
	Operation     string
	TargetKind    string
	TargetNumber  int64
	TargetSHA     string
	Status        string
	Limit         int
	Cursor        string
}

// ActionRecord stores one generic typed result from an agent or deterministic controller action.
type ActionRecord struct {
	ID                string    `json:"id"`
	MonitorNamespace  string    `json:"monitorNamespace"`
	MonitorName       string    `json:"monitorName"`
	Kind              string    `json:"kind"`
	Number            int64     `json:"number,omitempty"`
	ActionKind        string    `json:"actionKind"`
	SnapshotDigest    string    `json:"snapshotDigest,omitempty"`
	HeadSHA           string    `json:"headSHA,omitempty"`
	TaskName          string    `json:"taskName,omitempty"`
	CommandEventID    string    `json:"commandEventID,omitempty"`
	WorkActionID      string    `json:"workActionID,omitempty"`
	MonitorGeneration int64     `json:"monitorGeneration,omitempty"`
	Verdict           string    `json:"verdict,omitempty"`
	Confidence        string    `json:"confidence,omitempty"`
	Summary           string    `json:"summary,omitempty"`
	PayloadJSON       string    `json:"payloadJSON,omitempty"`
	PayloadDigest     string    `json:"payloadDigest,omitempty"`
	CreatedAt         time.Time `json:"createdAt"`
}

// ActionRecordFilter constrains action record list queries.
type ActionRecordFilter struct {
	Namespace   string
	MonitorName string
	Kind        string
	Number      int64
	ActionKind  string
	TaskName    string
	Limit       int
	Cursor      string
}

// ReviewRecord stores one immutable typed review result.
type ReviewRecord struct {
	ID                      string    `json:"id"`
	MonitorNamespace        string    `json:"monitorNamespace"`
	MonitorName             string    `json:"monitorName"`
	Kind                    string    `json:"kind"`
	Number                  int64     `json:"number,omitempty"`
	HeadSHA                 string    `json:"headSHA,omitempty"`
	TaskName                string    `json:"taskName,omitempty"`
	TaskNamespace           string    `json:"taskNamespace,omitempty"`
	Verdict                 string    `json:"verdict,omitempty"`
	Confidence              string    `json:"confidence,omitempty"`
	Repairable              bool      `json:"repairable,omitempty"`
	SecurityStatus          string    `json:"securityStatus,omitempty"`
	FindingsJSON            string    `json:"findingsJSON,omitempty"`
	Summary                 string    `json:"summary,omitempty"`
	SuggestedComment        string    `json:"suggestedComment,omitempty"`
	ValidationTask          string    `json:"validationTask,omitempty"`
	ValidationImage         string    `json:"validationImage,omitempty"`
	ValidationCommandDigest string    `json:"validationCommandDigest,omitempty"`
	ValidationStatus        string    `json:"validationStatus,omitempty"`
	ValidationEvidence      string    `json:"validationEvidence,omitempty"`
	RenderedComment         string    `json:"renderedComment,omitempty"`
	Marker                  string    `json:"marker,omitempty"`
	GitHubReviewID          string    `json:"githubReviewID,omitempty"`
	GitHubCommentID         string    `json:"githubCommentID,omitempty"`
	GitHubCommentURL        string    `json:"githubCommentURL,omitempty"`
	CreatedAt               time.Time `json:"createdAt"`
}

// ReviewRecordFilter constrains review record list queries.
type ReviewRecordFilter struct {
	Namespace   string
	MonitorName string
	Kind        string
	Number      int64
	HeadSHA     string
	Verdict     string
	Limit       int
	Cursor      string
}

// ReviewPublishRecord stores one GitHub review publish attempt/outcome.
type ReviewPublishRecord struct {
	ID                 string    `json:"id"`
	MonitorNamespace   string    `json:"monitorNamespace"`
	MonitorName        string    `json:"monitorName"`
	ItemKind           string    `json:"itemKind"`
	ItemNumber         int64     `json:"itemNumber,omitempty"`
	HeadSHA            string    `json:"headSHA,omitempty"`
	RunID              string    `json:"runID,omitempty"`
	ReviewTaskName     string    `json:"reviewTaskName,omitempty"`
	ReviewRecordID     string    `json:"reviewRecordID,omitempty"`
	Phase              string    `json:"phase"`
	Event              string    `json:"event,omitempty"`
	GitHubReviewID     string    `json:"githubReviewID,omitempty"`
	GitHubReviewURL    string    `json:"githubReviewURL,omitempty"`
	BodyDigest         string    `json:"bodyDigest,omitempty"`
	InlineCommentCount int       `json:"inlineCommentCount"`
	SkipReason         string    `json:"skipReason,omitempty"`
	Error              string    `json:"error,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

// ReviewPublishRecordFilter constrains review publish record list queries.
type ReviewPublishRecordFilter struct {
	Namespace      string
	MonitorName    string
	ItemKind       string
	ItemNumber     int64
	HeadSHA        string
	ReviewRecordID string
	Phase          string
	Limit          int
	Cursor         string
}

// CommandEventFilter constrains command event list queries.
type CommandEventFilter struct {
	Namespace   string
	MonitorName string
	Kind        string
	Number      int64
	Intent      string
	Status      string
	Limit       int
	Cursor      string
}

// CommandEvent stores one maintainer command intake event.
type CommandEvent struct {
	ID                  string     `json:"id"`
	MonitorNamespace    string     `json:"monitorNamespace"`
	MonitorName         string     `json:"monitorName"`
	Repo                string     `json:"repo,omitempty"`
	Kind                string     `json:"kind,omitempty"`
	Number              int64      `json:"number,omitempty"`
	Source              string     `json:"source,omitempty"`
	DeliveryID          string     `json:"deliveryID,omitempty"`
	Label               string     `json:"label,omitempty"`
	MonitorGeneration   int64      `json:"monitorGeneration,omitempty"`
	DedupeKey           string     `json:"dedupeKey,omitempty"`
	IdempotencyKey      string     `json:"idempotencyKey,omitempty"`
	CommentID           string     `json:"commentID,omitempty"`
	CommentURL          string     `json:"commentURL,omitempty"`
	Author              string     `json:"author,omitempty"`
	AuthorAssociation   string     `json:"authorAssociation,omitempty"`
	Permission          string     `json:"permission,omitempty"`
	Command             string     `json:"command,omitempty"`
	Intent              string     `json:"intent,omitempty"`
	HeadSHA             string     `json:"headSHA,omitempty"`
	IssueSnapshotDigest string     `json:"issueSnapshotDigest,omitempty"`
	Status              string     `json:"status,omitempty"`
	StatusCommentID     string     `json:"statusCommentID,omitempty"`
	CreatedRepairJobID  string     `json:"createdRepairJobID,omitempty"`
	CreatedAt           time.Time  `json:"createdAt"`
	ProcessedAt         *time.Time `json:"processedAt,omitempty"`
	Error               string     `json:"error,omitempty"`
}

// RepairJob stores durable repair and automerge state.
type RepairJob struct {
	ID                 string     `json:"id"`
	MonitorNamespace   string     `json:"monitorNamespace"`
	MonitorName        string     `json:"monitorName"`
	Repo               string     `json:"repo,omitempty"`
	PRNumber           int64      `json:"prNumber,omitempty"`
	Intent             string     `json:"intent,omitempty"`
	Source             string     `json:"source,omitempty"`
	HeadSHA            string     `json:"headSHA,omitempty"`
	BaseSHA            string     `json:"baseSHA,omitempty"`
	BaseBranch         string     `json:"baseBranch,omitempty"`
	Phase              string     `json:"phase,omitempty"`
	RepairCountPR      int        `json:"repairCountPR"`
	RepairCountHead    int        `json:"repairCountHead"`
	ValidationAttempts int        `json:"validationAttempts"`
	ReviewFixAttempts  int        `json:"reviewFixAttempts"`
	TaskName           string     `json:"taskName,omitempty"`
	Branch             string     `json:"branch,omitempty"`
	PushedSHA          string     `json:"pushedSHA,omitempty"`
	LastError          string     `json:"lastError,omitempty"`
	CreatedAt          time.Time  `json:"createdAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
	CompletedAt        *time.Time `json:"completedAt,omitempty"`
}

// RepairJobFilter constrains repair job list queries.
type RepairJobFilter struct {
	Namespace   string
	MonitorName string
	Repo        string
	PRNumber    int64
	Intent      string
	Phase       string
	Limit       int
	Cursor      string
}

// MonitorEvent stores an append-only monitor audit event.
type MonitorEvent struct {
	ID               string    `json:"id"`
	MonitorNamespace string    `json:"monitorNamespace"`
	MonitorName      string    `json:"monitorName"`
	RunID            string    `json:"runID,omitempty"`
	ItemKind         string    `json:"itemKind,omitempty"`
	ItemNumber       int64     `json:"itemNumber,omitempty"`
	ItemSHA          string    `json:"itemSHA,omitempty"`
	EventType        string    `json:"eventType"`
	Actor            string    `json:"actor,omitempty"`
	Summary          string    `json:"summary,omitempty"`
	MetadataJSON     string    `json:"metadataJSON,omitempty"`
	CreatedAt        time.Time `json:"createdAt"`
}

// MonitorEventFilter constrains monitor event list queries.
type MonitorEventFilter struct {
	Namespace   string
	ID          string
	MonitorName string
	RunID       string
	ItemKind    string
	ItemNumber  int64
	EventType   string
	Limit       int
	Cursor      string
}

// MonitorFeedbackAttempt is one immutable attempt to address one piece of
// reviewer feedback on a pull request: a review comment or a
// changes-requested review. Attempts of one feedback ID are numbered from 1
// without gaps, and each number is admitted once, so they count the
// feedback's lifetime attempts. Edits to the feedback keep its ID.
type MonitorFeedbackAttempt struct {
	ID               string `json:"id"`
	MonitorNamespace string `json:"monitorNamespace"`
	MonitorName      string `json:"monitorName"`
	Repo             string `json:"repo"`
	PRNumber         int64  `json:"prNumber"`
	// FeedbackID names the feedback, such as review_comment:123 or
	// review:456.
	FeedbackID string `json:"feedbackID"`
	Attempt    int    `json:"attempt"`
	// HeadSHA is the pull request head the attempt was admitted for.
	HeadSHA string `json:"headSHA"`
	// SnapshotDigest identifies the feedback snapshot that admitted it.
	SnapshotDigest string `json:"snapshotDigest"`
	CommandEventID string `json:"commandEventID"`
	RepairJobID    string `json:"repairJobID"`
	// CreatedAt is when the attempt was admitted, in UTC. A provider keeps
	// the time the caller set and uses its own clock only when it is zero.
	CreatedAt time.Time `json:"createdAt"`
}

// MonitorFeedbackAttemptFilter constrains feedback attempt list queries.
type MonitorFeedbackAttemptFilter struct {
	Namespace      string
	MonitorName    string
	Repo           string
	PRNumber       int64
	FeedbackID     string
	CommandEventID string
	Limit          int
	Cursor         string
}

// MonitorFeedbackAdmission admits one feedback repair: the command event
// that queues it and the attempt of each feedback ID it addresses.
type MonitorFeedbackAdmission struct {
	Command  CommandEvent             `json:"command"`
	Attempts []MonitorFeedbackAttempt `json:"attempts"`
}

// MaxMonitorFeedbackAttemptsPerAdmission bounds the attempts one admission
// records.
const MaxMonitorFeedbackAttemptsPerAdmission = 50

// Feedback outcome states. A feedback repair's outcome is reserved when the
// repair is admitted, creating while its repair Task's create is in flight,
// started once the Task exists, and then ends in exactly one terminal state,
// which never changes.
const (
	// MonitorFeedbackOutcomeReserved is a queued feedback repair whose Task
	// does not exist yet. Every admitted feedback ID is reserved.
	MonitorFeedbackOutcomeReserved = "reserved"
	// MonitorFeedbackOutcomeCreating is a feedback repair whose Task create
	// was requested and may have succeeded: its Task may exist, or may have
	// run and been deleted. Every admitted feedback ID stays charged until
	// the outcome starts, or returns to reserved because the API server
	// definitely refused the create.
	MonitorFeedbackOutcomeCreating = "creating"
	// MonitorFeedbackOutcomeStarted is a feedback repair whose Task exists,
	// with the feedback targets its prompt froze.
	MonitorFeedbackOutcomeStarted = "started"
	// MonitorFeedbackOutcomeWithdrawn ends a reserved repair whose feedback
	// no longer needed a repair before its Task existed.
	MonitorFeedbackOutcomeWithdrawn = "withdrawn"
	// MonitorFeedbackOutcomeNoChange ends a started repair whose Task
	// verifiably changed nothing.
	MonitorFeedbackOutcomeNoChange = "no_change"
	// MonitorFeedbackOutcomeFailed ends a reserved or started repair that
	// did not succeed.
	MonitorFeedbackOutcomeFailed = "failed"
	// MonitorFeedbackOutcomeSucceeded ends a started repair whose Task
	// delivered its change.
	MonitorFeedbackOutcomeSucceeded = "succeeded"
)

// IsMonitorFeedbackOutcomeTerminal reports whether state is a terminal
// feedback outcome state.
func IsMonitorFeedbackOutcomeTerminal(state string) bool {
	switch state {
	case MonitorFeedbackOutcomeWithdrawn, MonitorFeedbackOutcomeNoChange, MonitorFeedbackOutcomeFailed, MonitorFeedbackOutcomeSucceeded:
		return true
	}
	return false
}

// MonitorFeedbackOutcome is the one mutable record of a feedback repair,
// kept beside its immutable attempts: what was admitted, what its repair
// Task was given to address, and how it ended.
type MonitorFeedbackOutcome struct {
	MonitorNamespace string `json:"monitorNamespace"`
	MonitorName      string `json:"monitorName"`
	Repo             string `json:"repo"`
	PRNumber         int64  `json:"prNumber"`
	// CommandEventID names the feedback repair's command event and
	// identifies the outcome within its namespace.
	CommandEventID string `json:"commandEventID"`
	HeadSHA        string `json:"headSHA"`
	State          string `json:"state"`
	// AdmittedIDs are the feedback IDs of the command's attempts, sorted.
	AdmittedIDs []string `json:"admittedIDs"`
	// Targets are the feedback IDs the repair Task's prompt froze, sorted, a
	// subset of AdmittedIDs. Only a started outcome, or a terminal one that
	// was started, has them.
	Targets []string `json:"targets,omitempty"`
	// TaskName is the repair Task's name, set when its create was requested
	// or the outcome started.
	TaskName string `json:"taskName,omitempty"`
	// Reason says why a terminal outcome ended as it did.
	Reason string `json:"reason,omitempty"`
	// ResultSHA is the head a succeeded repair pushed, so its result can be
	// projected without its Task.
	ResultSHA string    `json:"resultSHA,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// TaskCreateAt is when the repair Task's create was requested, kept
	// until a definite refusal releases it. An outcome with it set and no
	// start may have run a Task.
	TaskCreateAt *time.Time `json:"taskCreateAt,omitempty"`
	StartedAt    *time.Time `json:"startedAt,omitempty"`
	CompletedAt  *time.Time `json:"completedAt,omitempty"`
	// ProjectedAt is when the controller confirmed that a terminal outcome's
	// projections, such as its repair job and work action, record its end.
	ProjectedAt *time.Time `json:"projectedAt,omitempty"`
}

// ChargedIDs returns the feedback IDs whose attempt budget the outcome
// charges: every admitted ID while it is reserved or creating, the targets
// its repair Task was given once it started, every admitted ID when it ended
// while its Task may have run, and none when no Task create was requested.
func (o MonitorFeedbackOutcome) ChargedIDs() []string {
	switch {
	case o.State == MonitorFeedbackOutcomeReserved || o.State == MonitorFeedbackOutcomeCreating:
		return o.AdmittedIDs
	case len(o.Targets) > 0:
		return o.Targets
	case o.TaskCreateAt != nil:
		return o.AdmittedIDs
	}
	return nil
}

// MonitorFeedbackOutcomeFilter constrains feedback outcome list queries.
type MonitorFeedbackOutcomeFilter struct {
	Namespace   string
	MonitorName string
	Repo        string
	PRNumber    int64
	HeadSHA     string
	State       string
	// Unprojected keeps only terminal outcomes whose projections were not
	// confirmed.
	Unprojected bool
	Limit       int
	Cursor      string
}

// PrepareMonitorFeedbackRepairTaskRequest records that a feedback repair's
// Task create is about to be requested.
type PrepareMonitorFeedbackRepairTaskRequest struct {
	Namespace      string `json:"namespace"`
	CommandEventID string `json:"commandEventID"`
	TaskName       string `json:"taskName"`
}

// ReleaseMonitorFeedbackRepairTaskRequest records that the API server
// definitely refused a feedback repair's Task create.
type ReleaseMonitorFeedbackRepairTaskRequest struct {
	Namespace      string `json:"namespace"`
	CommandEventID string `json:"commandEventID"`
}

// MarkMonitorFeedbackOutcomeProjectedRequest records that a terminal
// feedback outcome's projections record its end.
type MarkMonitorFeedbackOutcomeProjectedRequest struct {
	Namespace      string `json:"namespace"`
	CommandEventID string `json:"commandEventID"`
}

// StartMonitorFeedbackRepairRequest records that a feedback repair's Task
// exists and which admitted feedback its prompt froze as targets.
type StartMonitorFeedbackRepairRequest struct {
	Namespace      string   `json:"namespace"`
	CommandEventID string   `json:"commandEventID"`
	Targets        []string `json:"targets"`
	TaskName       string   `json:"taskName"`
}

// FinishMonitorFeedbackRepairRequest records how a feedback repair ended.
type FinishMonitorFeedbackRepairRequest struct {
	Namespace      string `json:"namespace"`
	CommandEventID string `json:"commandEventID"`
	// State is a terminal outcome state.
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	// ResultSHA is the head a succeeded repair pushed.
	ResultSHA string `json:"resultSHA,omitempty"`
}
