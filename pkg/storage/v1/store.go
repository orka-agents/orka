package v1

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrNotFound = errors.New("not found")

var ErrConflict = errors.New("conflict")

var ErrSessionConfigurationMismatch = errors.New("session configuration mismatch")

var ErrNotReady = errors.New("not ready")

var ErrDuplicateMismatch = errors.New("duplicate payload mismatch")

var ErrGatewayMessageReplayOnly = errors.New("gateway message receipt not found for replay-only admission")

var ErrCapacity = errors.New("capacity exceeded")

var ErrValidation = errors.New("validation error")

var ErrGatewayOwnedSession = errors.New("gateway-owned session")

var ErrUnavailable = errors.New("storage unavailable")

var ErrOutcomeUnknown = errors.New("storage outcome unknown")

var ErrFenced = errors.New("fenced")

var ErrTooLarge = errors.New("too large")

var ErrPreconditionFailed = errors.New("precondition failed")

var ErrUnauthenticated = errors.New("storage authentication failed")

var ErrIncompatible = errors.New("storage incompatible")

type FencedError struct {
	Message string
}

func (e FencedError) Error() string {
	return e.Message
}

func (e FencedError) Is(target error) bool {
	return target == ErrFenced || target == ErrConflict
}

func FencedErrorf(format string, args ...any) error {
	return FencedError{Message: fmt.Sprintf(format, args...)}
}

type PreconditionFailedError struct {
	Message string
}

func (e PreconditionFailedError) Error() string {
	return e.Message
}

func (e PreconditionFailedError) Is(target error) bool {
	return target == ErrPreconditionFailed || target == ErrConflict
}

func PreconditionFailedErrorf(format string, args ...any) error {
	return PreconditionFailedError{Message: fmt.Sprintf(format, args...)}
}

type TooLargeError struct {
	What  string
	Size  int64
	Limit int64
}

func (e TooLargeError) Error() string {
	return fmt.Sprintf("%s is %d bytes, which exceeds the %d byte limit", e.What, e.Size, e.Limit)
}

func (e TooLargeError) Unwrap() error {
	return ErrTooLarge
}

type ValidationError struct {
	Message string
}

func (e ValidationError) Error() string {
	return e.Message
}

func (e ValidationError) Unwrap() error {
	return ErrValidation
}

func ValidationErrorf(format string, args ...any) error {
	return ValidationError{Message: fmt.Sprintf(format, args...)}
}

type HealthChecker interface {
	HealthCheck(ctx context.Context) error
}

type ResultStore interface {
	SaveResult(ctx context.Context, namespace, taskName string, data []byte) error
	GetResult(ctx context.Context, namespace, taskName string) ([]byte, error)
	DeleteResult(ctx context.Context, namespace, taskName string) error
}

type PromptResultReceipt struct {
	AttemptID       string
	Namespace       string
	TaskName        string
	OperationID     string
	OperationDigest string
	Data            []byte
}

type PromptResultReceiptStore interface {
	SavePromptResultReceipt(ctx context.Context, receipt PromptResultReceipt) error
	GetPromptResultReceipt(ctx context.Context, attemptID string) (*PromptResultReceipt, error)
}

type SessionStore interface {
	CreateSession(ctx context.Context, session *SessionRecord) error
	GetSession(ctx context.Context, namespace, name string) (*SessionRecord, error)
	ListSessions(ctx context.Context, namespace string) ([]SessionMetadata, error)
	ListSessionsPage(ctx context.Context, namespace string, after *SessionPageCursor, limit int, excludeType string) (items []SessionMetadata, more bool, err error)
	DeleteSession(ctx context.Context, namespace, name string, now time.Time) error

	AcquireLock(ctx context.Context, namespace, name, taskName, taskUID string, now time.Time) error
	ReleaseLock(ctx context.Context, namespace, name, taskName, taskUID string) error
	IsLocked(ctx context.Context, namespace, name, currentTask, currentTaskUID string, now time.Time) (bool, error)

	AppendMessages(ctx context.Context, namespace, name string, messages []SessionMessage) error
	LoadTranscript(ctx context.Context, namespace, name string, maxMessages int) ([]SessionMessage, error)
	LoadTranscriptThrough(ctx context.Context, namespace, name, throughMessageID string, maxMessages int) ([]SessionMessage, error)
	SearchTranscript(ctx context.Context, filter TranscriptSearchFilter) ([]TranscriptSearchResult, error)

	UpdateTokenCounts(ctx context.Context, namespace, name string, inputTokens, outputTokens int) error
}

type AcquireChatTurnRequest struct {
	Session   *SessionRecord
	TurnID    string
	Now       time.Time
	ExpiresAt time.Time
}

type CommitSessionTurnRequest struct {
	Session              *SessionRecord
	TurnID               string
	ExpectedMessageCount int
	Messages             []SessionMessage
	InputTokens          int
	OutputTokens         int
	Now                  time.Time
}

type SessionTurnCommitter interface {
	AcquireChatTurn(ctx context.Context, request AcquireChatTurnRequest) (created bool, err error)
	ReleaseChatTurn(ctx context.Context, namespace, name, turnID string, deleteEmptyCreatedSession bool) error
	CommitSessionTurn(ctx context.Context, request CommitSessionTurnRequest) error
}

type SessionOwnerWrite struct {
	Namespace string
	Name      string
	OwnerName string
	OwnerUID  string
	Now       time.Time
}

type FencedSessionWriteStore interface {
	AppendMessagesWithLock(ctx context.Context, owner SessionOwnerWrite, messages []SessionMessage) error
	UpdateTokenCountsWithLock(ctx context.Context, owner SessionOwnerWrite, inputTokens, outputTokens int) error
	ReleaseLockWithTokenUsage(ctx context.Context, owner SessionOwnerWrite, inputTokens, outputTokens int) error
}

type GatewayEventStore interface {
	AdmitGatewayEvent(ctx context.Context, admission GatewayEventAdmission) (*GatewayEvent, bool, error)
	GetGatewayEvent(ctx context.Context, namespace, id string) (*GatewayEvent, error)
	GetGatewayEventDuplicate(ctx context.Context, candidate *GatewayEvent, now time.Time) (*GatewayEvent, error)
	GetGatewayEventForTask(ctx context.Context, namespace, taskName, taskUID string) (*GatewayEvent, error)
	HasGatewayTaskTombstone(ctx context.Context, namespace, taskName, taskUID string) (bool, error)
	ListGatewayEvents(ctx context.Context, filter GatewayEventFilter) ([]GatewayEvent, error)
	ClaimNextGatewayEvent(ctx context.Context, namespace, owner string, now time.Time, lease time.Duration) (*GatewayEvent, error)
	RenewGatewayEventClaim(ctx context.Context, namespace, id, owner string, now time.Time, lease time.Duration) (*GatewayEvent, error)
	FreezeGatewayEventTaskRuntimeAllowedTools(ctx context.Context, namespace, id, owner string, allowedTools []string, now time.Time) (*GatewayEvent, error)
	MarkGatewayEventTaskCreated(ctx context.Context, namespace, id, taskName, taskUID, owner string, now time.Time) error
	RetryGatewayEvent(ctx context.Context, namespace, id, owner, reason string, nextAttemptAt time.Time) error
	DeferGatewayEventProjection(ctx context.Context, namespace, id string, nextAttemptAt time.Time) error
	ExpireGatewayEvent(ctx context.Context, namespace, id, owner, reason string, now time.Time) error
	ExpireGatewayEventWithDelivery(ctx context.Context, projection GatewayExpiryProjection) (*GatewayDelivery, bool, error)
	MarkExpiredGatewayEventDeadLettered(ctx context.Context, namespace, id, reason string, now time.Time) error
	ProjectGatewayTerminal(ctx context.Context, projection GatewayTerminalProjection) (*GatewayDelivery, bool, error)
	GetGatewayQueueStats(ctx context.Context, namespace string) (GatewayQueueStats, error)
}

type GatewayDeliveryStore interface {
	GetGatewayMessageBudget(ctx context.Context, query GatewayMessageBudgetQuery) (*GatewayMessageBudget, error)
	EnqueueGatewayMessage(ctx context.Context, request GatewayMessageEnqueue) (*GatewayDelivery, bool, error)
	CreateGatewayDelivery(ctx context.Context, delivery *GatewayDelivery) (*GatewayDelivery, bool, error)
	GetGatewayDelivery(ctx context.Context, namespace, id string) (*GatewayDelivery, error)
	ListGatewayDeliveries(ctx context.Context, filter GatewayDeliveryFilter) ([]GatewayDelivery, error)
	ClaimNextGatewayDelivery(ctx context.Context, namespace, owner string, now time.Time, lease time.Duration) (*GatewayDelivery, error)
	MarkGatewayDeliveryDelivered(ctx context.Context, namespace, id, owner, providerMessageID string, now time.Time) error
	ScheduleGatewayDeliveryRetry(ctx context.Context, namespace, id, owner, reason string, nextAttemptAt time.Time) error
	MarkGatewayDeliveryTerminal(ctx context.Context, namespace, id, owner string, state GatewayDeliveryState, reason string, now time.Time) error
	RetryGatewayDelivery(ctx context.Context, namespace, id string, now, expiresAt time.Time) (*GatewayDelivery, error)
	MaintainGatewayRecords(ctx context.Context, namespace string, now, terminalCutoff time.Time) (GatewayMaintenanceResult, error)
}

type PlanStore interface {
	SavePlan(ctx context.Context, namespace, taskName string, plan *PlanState) error
	GetPlan(ctx context.Context, namespace, taskName string) (*PlanState, error)
	DeletePlan(ctx context.Context, namespace, taskName string) error
}

type ArtifactStore interface {
	SaveArtifact(ctx context.Context, request SaveArtifactRequest) error
	GetArtifact(ctx context.Context, key ArtifactKey) ([]byte, string, error)
	ListArtifacts(ctx context.Context, owner ArtifactOwner) ([]ArtifactMetadata, error)
	DeleteArtifacts(ctx context.Context, owner ArtifactOwner) error
}

type RepositoryMonitorStore interface {
	UpsertRepositoryMonitor(ctx context.Context, monitor *RepositoryMonitorRecord) error
	GetRepositoryMonitor(ctx context.Context, namespace, name string) (*RepositoryMonitorRecord, error)
	DeleteRepositoryMonitor(ctx context.Context, namespace, name string) error

	CreateMonitorRun(ctx context.Context, run *MonitorRun) error
	UpdateMonitorRun(ctx context.Context, run *MonitorRun) error
	GetMonitorRun(ctx context.Context, namespace, id string) (*MonitorRun, error)
	ListMonitorRuns(ctx context.Context, filter MonitorRunFilter) ([]MonitorRun, string, error)

	UpsertMonitorItem(ctx context.Context, item *MonitorItem) error
	GetMonitorItem(ctx context.Context, namespace, monitorName, kind, itemKey string) (*MonitorItem, error)
	ListMonitorItems(ctx context.Context, filter MonitorItemFilter) ([]MonitorItem, string, error)

	CreateWorkAction(ctx context.Context, action *WorkAction) error
	UpdateWorkAction(ctx context.Context, action *WorkAction) error
	GetWorkAction(ctx context.Context, namespace, id string) (*WorkAction, error)
	ListWorkActions(ctx context.Context, filter WorkActionFilter) ([]WorkAction, string, error)
	CancelWorkActions(ctx context.Context, namespace, monitorName, targetKind string, targetNumber int64, reason, exceptActionID string) (int, error)
	MarkWorkActionRetryPending(ctx context.Context, request MarkWorkActionRetryPendingRequest) (bool, error)

	CreateActionRecord(ctx context.Context, record *ActionRecord) error
	UpdateActionRecord(ctx context.Context, record *ActionRecord) error
	GetActionRecord(ctx context.Context, namespace, id string) (*ActionRecord, error)
	ListActionRecords(ctx context.Context, filter ActionRecordFilter) ([]ActionRecord, string, error)

	CreateReviewRecord(ctx context.Context, record *ReviewRecord) error
	GetReviewRecord(ctx context.Context, namespace, id string) (*ReviewRecord, error)
	ListReviewRecords(ctx context.Context, filter ReviewRecordFilter) ([]ReviewRecord, string, error)

	CreateReviewPublishRecord(ctx context.Context, record *ReviewPublishRecord) error
	UpdateReviewPublishRecord(ctx context.Context, record *ReviewPublishRecord) error
	GetReviewPublishRecord(ctx context.Context, namespace, id string) (*ReviewPublishRecord, error)
	ListReviewPublishRecords(ctx context.Context, filter ReviewPublishRecordFilter) ([]ReviewPublishRecord, string, error)

	CreateCommandEvent(ctx context.Context, event *CommandEvent) error
	UpdateCommandEvent(ctx context.Context, event *CommandEvent) error
	GetCommandEvent(ctx context.Context, namespace, id string) (*CommandEvent, error)
	ListCommandEvents(ctx context.Context, filter CommandEventFilter) ([]CommandEvent, string, error)

	CreateImplementationJob(ctx context.Context, job *ImplementationJob) error
	UpdateImplementationJob(ctx context.Context, job *ImplementationJob) error
	GetImplementationJob(ctx context.Context, namespace, id string) (*ImplementationJob, error)
	ListImplementationJobs(ctx context.Context, filter ImplementationJobFilter) ([]ImplementationJob, string, error)
	CountImplementationJobs(ctx context.Context, filter ImplementationJobFilter) (int, error)

	CreateGitHubMutationRecord(ctx context.Context, record *GitHubMutationRecord) error
	UpdateGitHubMutationRecord(ctx context.Context, record *GitHubMutationRecord) error
	GetGitHubMutationRecord(ctx context.Context, namespace, id string) (*GitHubMutationRecord, error)
	ListGitHubMutationRecords(ctx context.Context, filter GitHubMutationRecordFilter) ([]GitHubMutationRecord, string, error)

	CreateRepairJob(ctx context.Context, job *RepairJob) error
	UpdateRepairJob(ctx context.Context, job *RepairJob) error
	GetRepairJob(ctx context.Context, namespace, id string) (*RepairJob, error)
	ListRepairJobs(ctx context.Context, filter RepairJobFilter) ([]RepairJob, string, error)

	CreateMonitorEvent(ctx context.Context, event *MonitorEvent) error
	ListMonitorEvents(ctx context.Context, filter MonitorEventFilter) ([]MonitorEvent, string, error)
}

const MaxMessageBatch = 500

type Message struct {
	ID         int64     `json:"id"`
	Namespace  string    `json:"namespace"`
	FromTask   string    `json:"fromTask"`
	ToTask     string    `json:"toTask"`
	ParentTask string    `json:"parentTask"`
	Content    string    `json:"content"`
	Read       bool      `json:"read"`
	CreatedAt  time.Time `json:"createdAt"`
}

type ConsumeMessagesRequest struct {
	Namespace  string
	TaskName   string
	ParentTask string
	Limit      int
}

type MessageStore interface {
	SendMessage(ctx context.Context, msg *Message) error

	ListUnreadMessages(ctx context.Context, namespace, taskName, parentTask string, limit int) ([]Message, error)

	ConsumeMessages(ctx context.Context, request ConsumeMessagesRequest) ([]Message, error)

	DeleteTaskMessages(ctx context.Context, namespace, taskName string) error

	DeleteParentMessages(ctx context.Context, namespace, parentTask string) error
}

func MessageBatchLimit(limit int) (int, error) {
	switch {
	case limit == 0:
		return MaxMessageBatch, nil
	case limit < 0 || limit > MaxMessageBatch:
		return 0, ValidationErrorf("message batch limit must be between 1 and %d", MaxMessageBatch)
	default:
		return limit, nil
	}
}
