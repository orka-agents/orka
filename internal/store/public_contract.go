package store

import (
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// These records, errors and interfaces are defined by the public storage
// contract in pkg/storage/v1. The aliases keep their established internal
// names for Orka callers.
type (
	SessionLineage                            = storagev1.SessionLineage
	ClaimSessionLineageRequest                = storagev1.ClaimSessionLineageRequest
	SessionLineageStore                       = storagev1.SessionLineageStore
	ControllerEpochMirror                     = storagev1.ControllerEpochMirror
	ControllerEpochFence                      = storagev1.ControllerEpochFence
	ControllerEpoch                           = storagev1.ControllerEpoch
	VerifiedBranchBaseline                    = storagev1.VerifiedBranchBaseline
	SessionTurnState                          = storagev1.SessionTurnState
	SessionTurnTerminalKind                   = storagev1.SessionTurnTerminalKind
	SessionTurnKey                            = storagev1.SessionTurnKey
	SessionTurn                               = storagev1.SessionTurn
	BranchClaimAvailability                   = storagev1.BranchClaimAvailability
	RemoteRefState                            = storagev1.RemoteRefState
	PublicationState                          = storagev1.PublicationState
	PullRequestIntent                         = storagev1.PullRequestIntent
	PreparedPublicationReceipt                = storagev1.PreparedPublicationReceipt
	PublishOperationReceipt                   = storagev1.PublishOperationReceipt
	PublicationVerificationReceipt            = storagev1.PublicationVerificationReceipt
	PublicationReceipt                        = storagev1.PublicationReceipt
	PullRequestOperationReceipt               = storagev1.PullRequestOperationReceipt
	OutboxProjectionState                     = storagev1.OutboxProjectionState
	OutboxProjection                          = storagev1.OutboxProjection
	ClaimOutboxProjectionsRequest             = storagev1.ClaimOutboxProjectionsRequest
	CompleteOutboxProjectionRequest           = storagev1.CompleteOutboxProjectionRequest
	ExecutionEvent                            = storagev1.ExecutionEvent
	ExecutionEventUsage                       = storagev1.ExecutionEventUsage
	ExecutionEventFilter                      = storagev1.ExecutionEventFilter
	SessionExecutionEventFilter               = storagev1.SessionExecutionEventFilter
	SessionExecutionEvent                     = storagev1.SessionExecutionEvent
	ExecutionEventStore                       = storagev1.ExecutionEventStore
	DeduplicatingExecutionEventStore          = storagev1.DeduplicatingExecutionEventStore
	GroupedExecutionEventStore                = storagev1.GroupedExecutionEventStore
	ExecutionEventAppend                      = storagev1.ExecutionEventAppend
	ExecutionEventAppendResult                = storagev1.ExecutionEventAppendResult
	AtomicExecutionEventPlanStore             = storagev1.AtomicExecutionEventPlanStore
	GatewaySessionCleanupProof                = storagev1.GatewaySessionCleanupProof
	GatewaySessionCleanupCandidate            = storagev1.GatewaySessionCleanupCandidate
	GatewaySessionCleanupFilter               = storagev1.GatewaySessionCleanupFilter
	GatewaySessionCleanupPage                 = storagev1.GatewaySessionCleanupPage
	GatewaySessionCleanupCandidateStore       = storagev1.GatewaySessionCleanupCandidateStore
	GatewayTaskCleanupReceipt                 = storagev1.GatewayTaskCleanupReceipt
	GatewayTaskCleanupReceiptStore            = storagev1.GatewayTaskCleanupReceiptStore
	GatewayTaskCleanupReceiptFilter           = storagev1.GatewayTaskCleanupReceiptFilter
	GatewayTaskCleanupReceiptMaintenanceStore = storagev1.GatewayTaskCleanupReceiptMaintenanceStore
	GatewayEventState                         = storagev1.GatewayEventState
	GatewayDeliveryState                      = storagev1.GatewayDeliveryState
	GatewayEvent                              = storagev1.GatewayEvent
	GatewayEventAdmission                     = storagev1.GatewayEventAdmission
	GatewayEventFilter                        = storagev1.GatewayEventFilter
	GatewayTerminalProjection                 = storagev1.GatewayTerminalProjection
	GatewayExpiryProjection                   = storagev1.GatewayExpiryProjection
	GatewayMessageBudgetQuery                 = storagev1.GatewayMessageBudgetQuery
	GatewayMessageBudget                      = storagev1.GatewayMessageBudget
	GatewayMessageEnqueue                     = storagev1.GatewayMessageEnqueue
	GatewayDelivery                           = storagev1.GatewayDelivery
	GatewayDeliveryFilter                     = storagev1.GatewayDeliveryFilter
	GatewayQueueStats                         = storagev1.GatewayQueueStats
	GatewayMaintenanceResult                  = storagev1.GatewayMaintenanceResult
	RepositoryMonitorRecord                   = storagev1.RepositoryMonitorRecord
	MonitorRun                                = storagev1.MonitorRun
	MonitorRunFilter                          = storagev1.MonitorRunFilter
	MonitorItem                               = storagev1.MonitorItem
	MonitorItemFilter                         = storagev1.MonitorItemFilter
	WorkAction                                = storagev1.WorkAction
	WorkActionFilter                          = storagev1.WorkActionFilter
	ImplementationJob                         = storagev1.ImplementationJob
	ImplementationJobFilter                   = storagev1.ImplementationJobFilter
	GitHubMutationRecord                      = storagev1.GitHubMutationRecord
	GitHubMutationRecordFilter                = storagev1.GitHubMutationRecordFilter
	ActionRecord                              = storagev1.ActionRecord
	ActionRecordFilter                        = storagev1.ActionRecordFilter
	ReviewRecord                              = storagev1.ReviewRecord
	ReviewRecordFilter                        = storagev1.ReviewRecordFilter
	ReviewPublishRecord                       = storagev1.ReviewPublishRecord
	ReviewPublishRecordFilter                 = storagev1.ReviewPublishRecordFilter
	CommandEventFilter                        = storagev1.CommandEventFilter
	CommandEvent                              = storagev1.CommandEvent
	RepairJob                                 = storagev1.RepairJob
	RepairJobFilter                           = storagev1.RepairJobFilter
	MonitorEvent                              = storagev1.MonitorEvent
	MonitorEventFilter                        = storagev1.MonitorEventFilter
	OutboxPersistenceStore                    = storagev1.OutboxPersistenceStore
	ScanTaskIdentity                          = storagev1.ScanTaskIdentity
	ScanTaskIngestion                         = storagev1.ScanTaskIngestion
	ScanRunKey                                = storagev1.ScanRunKey
	ReviewSliceKey                            = storagev1.ReviewSliceKey
	ThreatModelKey                            = storagev1.ThreatModelKey
	FindingKey                                = storagev1.FindingKey
	ReviewSliceStatusUpdate                   = storagev1.ReviewSliceStatusUpdate
	FindingDuplicate                          = storagev1.FindingDuplicate
	ScanIngestionStep                         = storagev1.ScanIngestionStep
	ScanIngestionReadResult                   = storagev1.ScanIngestionReadResult
	ScanIngestionPreview                      = storagev1.ScanIngestionPreview
	ScanTaskIngestionCommit                   = storagev1.ScanTaskIngestionCommit
	ScanIngestionStore                        = storagev1.ScanIngestionStore
	ScanRun                                   = storagev1.ScanRun
	ThreatModel                               = storagev1.ThreatModel
	FindingEvidenceRef                        = storagev1.FindingEvidenceRef
	Finding                                   = storagev1.Finding
	PatchProposal                             = storagev1.PatchProposal
	PatchPublicationEvidence                  = storagev1.PatchPublicationEvidence
	PatchPullRequestEvidence                  = storagev1.PatchPullRequestEvidence
	FindingCounts                             = storagev1.FindingCounts
	FindingFilter                             = storagev1.FindingFilter
	ChangedLineRange                          = storagev1.ChangedLineRange
	ReviewSliceFile                           = storagev1.ReviewSliceFile
	ReviewSliceTest                           = storagev1.ReviewSliceTest
	ReviewSlice                               = storagev1.ReviewSlice
	ReviewSliceFilter                         = storagev1.ReviewSliceFilter
	DroppedFinding                            = storagev1.DroppedFinding
	DroppedFindingFilter                      = storagev1.DroppedFindingFilter
	SessionCleanupPersistenceStore            = storagev1.SessionCleanupPersistenceStore
	SessionTurnCleanupReceiptStore            = storagev1.SessionTurnCleanupReceiptStore
	SessionCleanupBranchClaim                 = storagev1.SessionCleanupBranchClaim
	SessionCleanupIntent                      = storagev1.SessionCleanupIntent
	CompleteSessionCleanupRequest             = storagev1.CompleteSessionCleanupRequest
	SessionCleanupCompletion                  = storagev1.SessionCleanupCompletion
	SessionTurnCleanupReceipt                 = storagev1.SessionTurnCleanupReceipt
	SessionSoulState                          = storagev1.SessionSoulState
	SessionSoulReader                         = storagev1.SessionSoulReader
	SessionSoulWriter                         = storagev1.SessionSoulWriter
	ValidationError                           = storagev1.ValidationError
	HealthChecker                             = storagev1.HealthChecker
	ResultStore                               = storagev1.ResultStore
	PromptResultReceipt                       = storagev1.PromptResultReceipt
	PromptResultReceiptStore                  = storagev1.PromptResultReceiptStore
	SessionStore                              = storagev1.SessionStore
	SessionTurnCommitter                      = storagev1.SessionTurnCommitter
	FencedSessionWriteStore                   = storagev1.FencedSessionWriteStore
	GatewayEventStore                         = storagev1.GatewayEventStore
	GatewayDeliveryStore                      = storagev1.GatewayDeliveryStore
	PlanStore                                 = storagev1.PlanStore
	ArtifactStore                             = storagev1.ArtifactStore
	RepositoryMonitorStore                    = storagev1.RepositoryMonitorStore
	Message                                   = storagev1.Message
	MessageStore                              = storagev1.MessageStore
	TaskJobIdentity                           = storagev1.TaskJobIdentity
	TaskJobAuthorityStore                     = storagev1.TaskJobAuthorityStore
	SessionRecord                             = storagev1.SessionRecord
	SessionMetadata                           = storagev1.SessionMetadata
	SessionPageCursor                         = storagev1.SessionPageCursor
	SessionMessage                            = storagev1.SessionMessage
	ArtifactMetadata                          = storagev1.ArtifactMetadata
	ArtifactOwner                             = storagev1.ArtifactOwner
	ArtifactKey                               = storagev1.ArtifactKey
	SaveArtifactRequest                       = storagev1.SaveArtifactRequest
	FencedError                               = storagev1.FencedError
	PreconditionFailedError                   = storagev1.PreconditionFailedError
	TooLargeError                             = storagev1.TooLargeError
	PlanState                                 = storagev1.PlanState
	TranscriptSearchFilter                    = storagev1.TranscriptSearchFilter
	TranscriptSearchHistoryBound              = storagev1.TranscriptSearchHistoryBound
	TranscriptSearchResult                    = storagev1.TranscriptSearchResult
	UsageWorkRequest                          = storagev1.UsageWorkRequest
	UsageTask                                 = storagev1.UsageTask
	UsageGatewayOwner                         = storagev1.UsageGatewayOwner
	UsageTaskPhase                            = storagev1.UsageTaskPhase
	UsageObservation                          = storagev1.UsageObservation
	UsagePRLink                               = storagev1.UsagePRLink
	UsagePullRequest                          = storagev1.UsagePullRequest
	UsageFilter                               = storagev1.UsageFilter
	UsageData                                 = storagev1.UsageData
	UsageStore                                = storagev1.UsageStore
	Memory                                    = storagev1.Memory
	MemoryFilter                              = storagev1.MemoryFilter
	MemoryProposal                            = storagev1.MemoryProposal
	MemoryProposalFilter                      = storagev1.MemoryProposalFilter
	MemoryProposalReview                      = storagev1.MemoryProposalReview
	MemoryProposalApplyTarget                 = storagev1.MemoryProposalApplyTarget
	MemoryProposalApplication                 = storagev1.MemoryProposalApplication
	MemoryStore                               = storagev1.MemoryStore
	TaskUsageObservationReader                = storagev1.TaskUsageObservationReader
)

const (
	SessionTurnOpen                   = storagev1.SessionTurnOpen
	SessionTurnFinalized              = storagev1.SessionTurnFinalized
	SessionTurnAssistantResult        = storagev1.SessionTurnAssistantResult
	SessionTurnOutcomeMarker          = storagev1.SessionTurnOutcomeMarker
	BranchClaimAvailable              = storagev1.BranchClaimAvailable
	BranchClaimReconciliationBlocked  = storagev1.BranchClaimReconciliationBlocked
	PublicationPreparing              = storagev1.PublicationPreparing
	PublicationPrepared               = storagev1.PublicationPrepared
	PublicationPublishing             = storagev1.PublicationPublishing
	PublicationVerifying              = storagev1.PublicationVerifying
	PublicationVerifiedExact          = storagev1.PublicationVerifiedExact
	PublicationDeliveredSuperseded    = storagev1.PublicationDeliveredSuperseded
	PublicationCancelledBeforePublish = storagev1.PublicationCancelledBeforePublish
	PublicationDeliveryConflict       = storagev1.PublicationDeliveryConflict
	PublicationCredentialBlocked      = storagev1.PublicationCredentialBlocked
	PublicationPreparationFailed      = storagev1.PublicationPreparationFailed
	PublicationOutcomeUnknown         = storagev1.PublicationOutcomeUnknown
	OutboxProjectionPending           = storagev1.OutboxProjectionPending
	OutboxProjectionDelivering        = storagev1.OutboxProjectionDelivering
	OutboxProjectionDelivered         = storagev1.OutboxProjectionDelivered
	OutboxProjectionDeadLetter        = storagev1.OutboxProjectionDeadLetter
	ExecutionEventStreamTypeTask      = storagev1.ExecutionEventStreamTypeTask
	DefaultExecutionEventLimit        = storagev1.DefaultExecutionEventLimit
	MaxExecutionEventLimit            = storagev1.MaxExecutionEventLimit
	GatewayEventAccepted              = storagev1.GatewayEventAccepted
	GatewayEventQueued                = storagev1.GatewayEventQueued
	GatewayEventDispatching           = storagev1.GatewayEventDispatching
	GatewayEventTaskCreated           = storagev1.GatewayEventTaskCreated
	GatewayEventCompleted             = storagev1.GatewayEventCompleted
	GatewayEventRejected              = storagev1.GatewayEventRejected
	GatewayEventDeadLettered          = storagev1.GatewayEventDeadLettered
	GatewayEventExpired               = storagev1.GatewayEventExpired
	GatewayDeliveryPending            = storagev1.GatewayDeliveryPending
	GatewayDeliverySending            = storagev1.GatewayDeliverySending
	GatewayDeliveryDelivered          = storagev1.GatewayDeliveryDelivered
	GatewayDeliveryRetryScheduled     = storagev1.GatewayDeliveryRetryScheduled
	GatewayDeliveryFailed             = storagev1.GatewayDeliveryFailed
	GatewayDeliveryDeadLettered       = storagev1.GatewayDeliveryDeadLettered
	GatewayDeliveryExpired            = storagev1.GatewayDeliveryExpired
	maxControlIdentifierBytes         = storagev1.MaxControlIdentifierBytes
	MaxExecutionEventDedupeKeyBytes   = storagev1.MaxExecutionEventDedupeKeyBytes
	SessionSoulDigestMetadata         = storagev1.SessionSoulDigestMetadata
	SessionSoulUnboundMetadata        = storagev1.SessionSoulUnboundMetadata
	SessionSoulAnchorSource           = storagev1.SessionSoulAnchorSource
	DefaultControllerEpochName        = storagev1.DefaultControllerEpochName
	SessionTypeChat                   = storagev1.SessionTypeChat
	SessionTypeGateway                = storagev1.SessionTypeGateway
	MaxUsageTokenCount                = storagev1.MaxUsageTokenCount
	UsageSourceProvider               = storagev1.UsageSourceProvider
	UsageSourceAgent                  = storagev1.UsageSourceAgent
	UsageSourceEstimate               = storagev1.UsageSourceEstimate
	UsageScopeCall                    = storagev1.UsageScopeCall
	UsageScopeAttempt                 = storagev1.UsageScopeAttempt
	UsageScopeSession                 = storagev1.UsageScopeSession
	UsageStatusStarted                = storagev1.UsageStatusStarted
	UsageStatusCompleted              = storagev1.UsageStatusCompleted
	UsageStatusFailed                 = storagev1.UsageStatusFailed
	UsageStatusCancelled              = storagev1.UsageStatusCancelled
	UsagePRCreated                    = storagev1.UsagePRCreated
	UsagePRAssisted                   = storagev1.UsagePRAssisted
	UsagePRReview                     = storagev1.UsagePRReview
	MemorySourceManual                = storagev1.MemorySourceManual
	MemorySourceProposal              = storagev1.MemorySourceProposal
	MemoryProposalTypeSkill           = storagev1.MemoryProposalTypeSkill
	MemoryProposalTypeMemory          = storagev1.MemoryProposalTypeMemory
	MemoryProposalStatusPending       = storagev1.MemoryProposalStatusPending
	MemoryProposalStatusAccepted      = storagev1.MemoryProposalStatusAccepted
	MemoryProposalStatusRejected      = storagev1.MemoryProposalStatusRejected
	MemoryProposalStatusArchived      = storagev1.MemoryProposalStatusArchived
	MemoryProposalStatusApplied       = storagev1.MemoryProposalStatusApplied
	MaxArtifactBytes                  = storagev1.MaxArtifactBytes
)

var (
	ErrNotFound                     = storagev1.ErrNotFound
	ErrConflict                     = storagev1.ErrConflict
	ErrNotReady                     = storagev1.ErrNotReady
	ErrDuplicateMismatch            = storagev1.ErrDuplicateMismatch
	ErrGatewayMessageReplayOnly     = storagev1.ErrGatewayMessageReplayOnly
	ErrCapacity                     = storagev1.ErrCapacity
	ErrValidation                   = storagev1.ErrValidation
	ErrGatewayOwnedSession          = storagev1.ErrGatewayOwnedSession
	ErrTaskJobRevoked               = storagev1.ErrTaskJobRevoked
	ErrSessionConfigurationMismatch = storagev1.ErrSessionConfigurationMismatch
	ErrUsageSelectionTooLarge       = storagev1.ErrUsageSelectionTooLarge
	ErrUnavailable                  = storagev1.ErrUnavailable
	ErrOutcomeUnknown               = storagev1.ErrOutcomeUnknown
	ErrFenced                       = storagev1.ErrFenced
	ErrTooLarge                     = storagev1.ErrTooLarge
	ErrGatewaySessionCleanupPending = storagev1.ErrGatewaySessionCleanupPending
	ErrPreconditionFailed           = storagev1.ErrPreconditionFailed
)

func FencedErrorf(format string, args ...any) error {
	return storagev1.FencedErrorf(format, args...)
}

func ScanIngestionReadsEqual(a, b ScanIngestionReadResult) bool {
	return storagev1.ScanIngestionReadsEqual(a, b)
}

func ScanRunsEqual(a, b ScanRun) bool { return storagev1.ScanRunsEqual(a, b) }

func PreconditionFailedErrorf(format string, args ...any) error {
	return storagev1.PreconditionFailedErrorf(format, args...)
}

func ConflictErrorf(format string, args ...any) error {
	return storagev1.ConflictErrorf(format, args...)
}

func NotReadyErrorf(format string, args ...any) error {
	return storagev1.NotReadyErrorf(format, args...)
}

func CanonicalBytesDigest(value []byte) string { return storagev1.CanonicalBytesDigest(value) }

func IsTerminalPublicationState(state PublicationState) bool {
	return storagev1.IsTerminalPublicationState(state)
}

func CanonicalControlID(kind string, components ...string) string {
	return storagev1.CanonicalControlID(kind, components...)
}

func ValidateCanonicalDigest(field, value string) error {
	return storagev1.ValidateCanonicalDigest(field, value)
}

func ValidateGitObjectID(field, value string) error {
	return storagev1.ValidateGitObjectID(field, value)
}

func ValidateControlIdentifier(field, value string) error {
	return storagev1.ValidateControlIdentifier(field, value)
}

func ValidationErrorf(format string, args ...any) error {
	return storagev1.ValidationErrorf(format, args...)
}

// Unexported helpers retained for internal callers of moved validation.
func validateControlIdentifier(field, value string) error {
	return storagev1.ValidateControlIdentifier(field, value)
}

func IsTerminalApprovalExecutionEventType(value string) bool {
	return storagev1.IsTerminalApprovalExecutionEventType(value)
}

func ApprovalIDFromExecutionEvent(event ExecutionEvent) string {
	return storagev1.ApprovalIDFromExecutionEvent(event)
}

func TerminalApprovalConflict(existingType, approvalID string) error {
	return storagev1.TerminalApprovalConflict(existingType, approvalID)
}

func NormalizeExecutionEventDedupeKey(value string) (string, error) {
	return storagev1.NormalizeExecutionEventDedupeKey(value)
}

func GatewayUserMessageID(eventID string) string { return storagev1.GatewayUserMessageID(eventID) }

func GatewayAssistantMessageID(eventID string) string {
	return storagev1.GatewayAssistantMessageID(eventID)
}

func GatewayErrorMessageID(eventID string) string { return storagev1.GatewayErrorMessageID(eventID) }

func GatewayEventEnvelopeDigest(event *GatewayEvent) string {
	return storagev1.GatewayEventEnvelopeDigest(event)
}

func IsValidGatewayEventState(state GatewayEventState) bool {
	return storagev1.IsValidGatewayEventState(state)
}

func IsValidGatewayDeliveryState(state GatewayDeliveryState) bool {
	return storagev1.IsValidGatewayDeliveryState(state)
}

func NormalizeControllerEpochName(name string) (string, error) {
	return storagev1.NormalizeControllerEpochName(name)
}

func NormalizeEpochFence(fence ControllerEpochFence) (ControllerEpochFence, error) {
	return storagev1.NormalizeEpochFence(fence)
}

func ValidateFullBranchRef(ref string) error { return storagev1.ValidateFullBranchRef(ref) }

func ValidateControlReason(field, value string) error {
	return storagev1.ValidateControlReason(field, value)
}

func ValidateControlPayload(field string, payload []byte) error {
	return storagev1.ValidateControlPayload(field, payload)
}

func ValidateUsageObservation(observation UsageObservation) error {
	return storagev1.ValidateUsageObservation(observation)
}

func UsageWorkID(namespace, monitorUID, repository, kind string, number int64) string {
	return storagev1.UsageWorkID(namespace, monitorUID, repository, kind, number)
}
