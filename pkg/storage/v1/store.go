package v1

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned when a requested resource does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when a resource cannot be updated because it changed concurrently.
var ErrConflict = errors.New("conflict")

// ErrSessionConfigurationMismatch marks an immutable conflict, not lease contention.
var ErrSessionConfigurationMismatch = errors.New("session configuration mismatch")

// ErrNotReady is returned when a durable prerequisite is expected to become ready shortly.
var ErrNotReady = errors.New("not ready")

// ErrDuplicateMismatch is returned when a stable external identifier, or an
// operation ID (see [WriteEnvelope]), is reused with a different payload.
var ErrDuplicateMismatch = errors.New("duplicate payload mismatch")

// ErrGatewayMessageReplayOnly is returned when receipt-only admission finds no
// existing message. The caller must return its original live admission gate error.
var ErrGatewayMessageReplayOnly = errors.New("gateway message receipt not found for replay-only admission")

// ErrCapacity is returned when a bounded durable store quota is full.
var ErrCapacity = errors.New("capacity exceeded")

// ErrValidation is returned when supplied input fails store-level validation.
var ErrValidation = errors.New("validation error")

// ErrGatewayOwnedSession is returned when a generic deletion targets canonical gateway history.
var ErrGatewayOwnedSession = errors.New("gateway-owned session")

// ErrUnavailable is returned when the provider could not run an operation and
// nothing was committed. Retrying the same operation is safe.
var ErrUnavailable = errors.New("storage unavailable")

// ErrOutcomeUnknown is returned when a write may or may not have committed,
// for example because the connection failed after the commit was sent. The
// caller must not report success or repeat the action as a new operation. It
// may retry only with the same operation ID and request digest (see
// [WriteEnvelope]), or settle the outcome with
// [OperationReceiptStore.ResolveOperation].
var ErrOutcomeUnknown = errors.New("storage outcome unknown")

// ErrFenced is returned when a write is rejected because the caller's
// controller epoch fence or owner fence is stale. The caller has lost the
// authority the write required and must stop rather than retry. Providers
// report it with [FencedErrorf].
var ErrFenced = errors.New("fenced")

// ErrTooLarge is returned when a value exceeds a size limit of the contract or
// of the provider. Providers report it with [TooLargeError].
var ErrTooLarge = errors.New("too large")

// FencedError reports a write rejected by a stale fence. It matches
// [ErrFenced]. Orka callers written before ErrFenced existed branch on
// [ErrConflict], so a FencedError also matches ErrConflict until those callers
// move to ErrFenced. Providers must not rely on that compatibility match.
type FencedError struct {
	Message string
}

func (e FencedError) Error() string {
	return e.Message
}

// Is reports whether target is ErrFenced or, for compatibility, ErrConflict.
func (e FencedError) Is(target error) bool {
	return target == ErrFenced || target == ErrConflict
}

// FencedErrorf formats a fence rejection.
func FencedErrorf(format string, args ...any) error {
	return FencedError{Message: fmt.Sprintf(format, args...)}
}

// TooLargeError reports a value that exceeds a size limit. It matches
// [ErrTooLarge].
type TooLargeError struct {
	// What names the rejected value, such as "artifact \"report.txt\"".
	What string
	// Size is the rejected value's size in bytes.
	Size int64
	// Limit is the largest accepted size in bytes.
	Limit int64
}

func (e TooLargeError) Error() string {
	return fmt.Sprintf("%s is %d bytes, which exceeds the %d byte limit", e.What, e.Size, e.Limit)
}

func (e TooLargeError) Unwrap() error {
	return ErrTooLarge
}

// ValidationError carries a client-safe validation message while remaining
// comparable with ErrValidation via errors.Is.
type ValidationError struct {
	Message string
}

func (e ValidationError) Error() string {
	return e.Message
}

func (e ValidationError) Unwrap() error {
	return ErrValidation
}

// ValidationErrorf formats a store validation error.
func ValidationErrorf(format string, args ...any) error {
	return ValidationError{Message: fmt.Sprintf(format, args...)}
}

// HealthChecker can verify its underlying storage is reachable.
type HealthChecker interface {
	HealthCheck(ctx context.Context) error
}

// ResultStore handles task result persistence.
type ResultStore interface {
	SaveResult(ctx context.Context, namespace, taskName string, data []byte) error
	GetResult(ctx context.Context, namespace, taskName string) ([]byte, error)
	DeleteResult(ctx context.Context, namespace, taskName string) error
}

// PromptResultReceipt preserves the exact result bytes behind a fenced prompt
// settling transition. The receipt is immutable for one PromptAttempt so a
// controller takeover can repair a result write interrupted after the durable
// control-plane transition.
type PromptResultReceipt struct {
	AttemptID       string
	Namespace       string
	TaskName        string
	OperationID     string
	OperationDigest string
	Data            []byte
}

// PromptResultReceiptStore persists attempt-bound result receipts before the
// corresponding PromptAttempt enters Settling.
type PromptResultReceiptStore interface {
	SavePromptResultReceipt(ctx context.Context, receipt PromptResultReceipt) error
	GetPromptResultReceipt(ctx context.Context, attemptID string) (*PromptResultReceipt, error)
}

// SessionStore handles session transcript persistence.
//
// Session locks and chat turn reservations are leases. A provider decides
// whether one has expired only by comparing its stored expiry, which came from
// an earlier request, with the time the current request carries (now, or the
// Now field of a request), never with its own clock. Orka passes its own
// clock, so every lease is judged on the controller clock, and the
// commit-time fence makes an early reclaim after a takeover harmless. A
// request's time is covered by its request digest, so a retry is judged at the
// original time. A zero request time returns ErrValidation.
type SessionStore interface {
	CreateSession(ctx context.Context, session *SessionRecord) error
	GetSession(ctx context.Context, namespace, name string) (*SessionRecord, error)
	ListSessions(ctx context.Context, namespace string) ([]SessionMetadata, error)
	// ListSessionsPage returns up to limit sessions of the namespace newest
	// first (creation time descending, then name), starting strictly after
	// the after cursor (nil = first page) and skipping excludeType (empty =
	// no exclusion). more reports whether further rows exist.
	ListSessionsPage(ctx context.Context, namespace string, after *SessionPageCursor, limit int, excludeType string) (items []SessionMetadata, more bool, err error)
	// DeleteSession removes a Session and its transcript. A chat turn
	// reservation or a transient Task lock that has not expired at now keeps
	// the Session, with ErrConflict; an expired one does not.
	DeleteSession(ctx context.Context, namespace, name string, now time.Time) error

	// Locking

	// AcquireLock gives the Session a durable lock for one Task incarnation.
	// A transient lock or chat turn reservation that has expired at now is
	// reclaimed; a live one returns ErrConflict.
	AcquireLock(ctx context.Context, namespace, name, taskName, taskUID string, now time.Time) error
	ReleaseLock(ctx context.Context, namespace, name, taskName, taskUID string) error
	// IsLocked reports whether another Task incarnation, or a chat turn
	// reservation, holds the Session at now. It clears a transient Task lock
	// that has expired at now.
	IsLocked(ctx context.Context, namespace, name, currentTask, currentTaskUID string, now time.Time) (bool, error)

	// Transcript
	AppendMessages(ctx context.Context, namespace, name string, messages []SessionMessage) error
	LoadTranscript(ctx context.Context, namespace, name string, maxMessages int) ([]SessionMessage, error)
	LoadTranscriptThrough(ctx context.Context, namespace, name, throughMessageID string, maxMessages int) ([]SessionMessage, error)
	// SearchTranscript excludes gateway sessions and applies the result limit
	// after filtering by the authorized session names.
	SearchTranscript(ctx context.Context, filter TranscriptSearchFilter) ([]TranscriptSearchResult, error)

	// Token tracking
	UpdateTokenCounts(ctx context.Context, namespace, name string, inputTokens, outputTokens int) error
}

// AcquireChatTurnRequest reserves a chat Session's current transcript revision
// for one turn.
type AcquireChatTurnRequest struct {
	// Session names the chat Session. When the reservation creates it, the
	// Session gets this type and cancellation state, and CreatedAt, or Now
	// when CreatedAt is zero.
	Session *SessionRecord
	// TurnID identifies the turn that holds the reservation.
	TurnID string
	// Now is the controller clock when the request was made. An earlier
	// reservation or transient Task lock is reclaimable once it has expired
	// at Now.
	Now time.Time
	// ExpiresAt is when the reservation lapses. It must be after Now.
	ExpiresAt time.Time
}

// CommitSessionTurnRequest commits one chat turn's transcript messages and
// token usage.
type CommitSessionTurnRequest struct {
	// Session names the chat Session.
	Session *SessionRecord
	// TurnID is the turn whose reservation the commit requires.
	TurnID string
	// ExpectedMessageCount fences the transcript revision the turn observed
	// after its reservation.
	ExpectedMessageCount int
	// Messages are the turn's transcript messages.
	Messages []SessionMessage
	// InputTokens and OutputTokens are added to the Session's counters.
	InputTokens  int
	OutputTokens int
	// Now is the controller clock when the request was made. The commit
	// requires the turn's reservation to expire after Now.
	Now time.Time
}

// SessionTurnCommitter reserves chat turns and atomically commits their
// transcript messages with token usage. The expected message count fences the
// session revision observed after acquiring the turn lease. Leases follow the
// rules of [SessionStore].
type SessionTurnCommitter interface {
	AcquireChatTurn(ctx context.Context, request AcquireChatTurnRequest) (created bool, err error)
	ReleaseChatTurn(ctx context.Context, namespace, name, turnID string, deleteEmptyCreatedSession bool) error
	CommitSessionTurn(ctx context.Context, request CommitSessionTurnRequest) error
}

// SessionOwnerWrite names the Session lock owner a write requires, the exact
// Task or transient owner that holds the lock, and the controller clock when
// the request was made. A transient lock that has expired at Now no longer
// authorizes the write.
type SessionOwnerWrite struct {
	Namespace string
	Name      string
	OwnerName string
	OwnerUID  string
	Now       time.Time
}

// FencedSessionWriteStore binds transcript and token writes to the exact active
// Task or transient lock owner so a stale owner cannot write after takeover.
// A write whose owner no longer holds the lock at owner.Now returns a
// FencedError.
type FencedSessionWriteStore interface {
	AppendMessagesWithLock(ctx context.Context, owner SessionOwnerWrite, messages []SessionMessage) error
	UpdateTokenCountsWithLock(ctx context.Context, owner SessionOwnerWrite, inputTokens, outputTokens int) error
	// ReleaseLockWithTokenUsage adds a finished owner's token usage and
	// releases its lock in one write, so a replayed completion cannot count the
	// usage twice: either nothing was written or the lock is already gone.
	ReleaseLockWithTokenUsage(ctx context.Context, owner SessionOwnerWrite, inputTokens, outputTokens int) error
}

// GatewayEventStore handles durable normalized ingress records and atomic Session projection.
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

// GatewayDeliveryStore handles durable adapter outbox records.
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

// PlanStore handles autonomous plan state persistence.
type PlanStore interface {
	SavePlan(ctx context.Context, namespace, taskName string, plan *PlanState) error
	GetPlan(ctx context.Context, namespace, taskName string) (*PlanState, error)
	DeletePlan(ctx context.Context, namespace, taskName string) error
}

// ArtifactStore handles task artifact persistence. Artifact operations never
// share a transaction with state.v1 records.
type ArtifactStore interface {
	// SaveArtifact stores an artifact, replacing an earlier file with the same
	// filename for the same Task. Data larger than MaxArtifactBytes is rejected
	// with ErrTooLarge.
	SaveArtifact(ctx context.Context, request SaveArtifactRequest) error
	// GetArtifact returns an artifact's data and content type, or ErrNotFound.
	GetArtifact(ctx context.Context, key ArtifactKey) ([]byte, string, error)
	// ListArtifacts returns metadata for a Task's artifacts, ordered by
	// filename in byte order.
	ListArtifacts(ctx context.Context, owner ArtifactOwner) ([]ArtifactMetadata, error)
	// DeleteArtifacts removes a Task's artifacts. Deleting a Task with no
	// artifacts succeeds.
	DeleteArtifacts(ctx context.Context, owner ArtifactOwner) error
}

// RepositoryMonitorStore handles durable repository monitor state.
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

// MaxMessageBatch is the most messages one ListUnreadMessages or
// ConsumeMessages call returns. A reader with a longer backlog reads it over
// several calls.
const MaxMessageBatch = 500

// Message represents an inter-agent message.
type Message struct {
	// ID is assigned by the provider when it stores the message. IDs increase
	// in the order a provider stores messages and are never reused, so a
	// replayed ConsumeMessages that reads its messages again by ID cannot
	// return a later one.
	ID         int64  `json:"id"`
	Namespace  string `json:"namespace"`
	FromTask   string `json:"fromTask"`
	ToTask     string `json:"toTask"` // "*" for broadcast to all siblings
	ParentTask string `json:"parentTask"`
	Content    string `json:"content"`
	Read       bool   `json:"read"`
	// CreatedAt is when the message was sent, in UTC. A provider keeps the
	// time the caller set and uses its own clock only when it is zero.
	CreatedAt time.Time `json:"createdAt"`
}

// ConsumeMessagesRequest names the inbox ConsumeMessages reads.
type ConsumeMessagesRequest struct {
	Namespace  string
	TaskName   string
	ParentTask string
	// Limit is the most messages to return, from 1 to MaxMessageBatch. Zero
	// selects MaxMessageBatch.
	Limit int
}

// MessageStore handles inter-agent message persistence.
//
// A Task's inbox holds the unread messages of its namespace and parent Task
// that are addressed to it or broadcast ("*"), except those it sent itself.
// A broadcast has one read flag: the first sibling to consume it marks it read
// for all of them.
type MessageStore interface {
	// SendMessage stores a new message. Use toTask="*" for broadcast. The
	// provider writes the assigned ID and creation time back into msg; a
	// replay writes back the same ones and stores nothing.
	SendMessage(ctx context.Context, msg *Message) error

	// ListUnreadMessages returns up to limit of the oldest unread messages in
	// a Task's inbox, ordered by ID, and changes nothing. A limit of zero
	// selects MaxMessageBatch; a negative limit or one above MaxMessageBatch
	// returns ErrValidation.
	ListUnreadMessages(ctx context.Context, namespace, taskName, parentTask string, limit int) ([]Message, error)

	// ConsumeMessages returns up to request.Limit of the oldest unread
	// messages in a Task's inbox, ordered by ID, and marks exactly those
	// messages read. A replay returns the same messages and marks nothing
	// more; it leaves out a message that DeleteTaskMessages or
	// DeleteParentMessages removed in between, which happens only while the
	// reader itself is being deleted. An invalid limit returns ErrValidation.
	ConsumeMessages(ctx context.Context, request ConsumeMessagesRequest) ([]Message, error)

	// DeleteTaskMessages deletes all messages involving a task (sent or received).
	DeleteTaskMessages(ctx context.Context, namespace, taskName string) error

	// DeleteParentMessages deletes all messages for children of a parent task.
	DeleteParentMessages(ctx context.Context, namespace, parentTask string) error
}

// MessageBatchLimit returns the batch size limit selects: MaxMessageBatch for
// zero, limit itself from 1 to MaxMessageBatch, and ErrValidation otherwise.
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
