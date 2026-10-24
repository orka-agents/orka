package v1

import "maps"

// MethodKind classifies a provider method by what it may change.
type MethodKind uint8

const (
	// MethodRead marks a method that changes nothing. It carries no write
	// envelope and leaves no operation receipt.
	MethodRead MethodKind = iota + 1
	// MethodWrite marks a method that may change stored state. Once Orka
	// attaches write envelopes, it carries a [WriteEnvelope], commits only
	// under the current fence, and leaves an operation receipt with its change.
	MethodWrite
	// MethodAdmin marks an administrative write that establishes the fence or
	// settles operations. It carries no envelope and is not checked against the
	// current fence.
	MethodAdmin
)

// String returns the kind's name.
func (k MethodKind) String() string {
	switch k {
	case MethodRead:
		return "read"
	case MethodWrite:
		return "write"
	case MethodAdmin:
		return "admin"
	default:
		return "unknown"
	}
}

// MethodInfo describes one provider method in the method catalog.
type MethodInfo struct {
	// Kind says what the method may change.
	Kind MethodKind
	// Contract is the contract part whose provider serves the method,
	// StateContract or ArtifactsContract, or empty for a method every part
	// serves, such as Describe. Once Orka fences writes per dataset, a write
	// waits only for the fence of the dataset that serves its own contract.
	Contract string
	// NaturalKey lists, separated by ", ", the request values by which the
	// provider recognizes a repeat of a write's effect. While the record or
	// tombstone that carries the key is kept, running the logical operation
	// again as a new operation with the same key never repeats the effect: it
	// returns the stored outcome or, when the stored record has moved on since,
	// fails with an error such as ErrConflict or ErrDuplicateMismatch.
	// Each path starts with $N, the Nth argument after the context counting
	// from 1, followed by field names. It is empty when the write has no such
	// key: a retry of the same operation is still safe through its operation
	// receipt, but running it again as a new operation repeats its effect
	// unless the write is idempotent by the state it changes, such as a delete.
	NaturalKey string
	// Class is the call class a storage client gives the method unless the
	// caller chooses another one, such as [CallStream] for a poller's read or
	// [CallBackground] for a retention loop. The catalog assigns
	// [CallControl], [CallInteractive], [CallBackground] or [CallBulk].
	Class CallClass
}

// CallClass says how a storage client schedules a call: its deadline, its
// retry rule and the concurrency limit it shares with other calls of the
// class. A network transport also uses it to carry bulk transfers on their
// own connection, so a large transfer cannot delay a health check queued
// behind it.
type CallClass uint8

const (
	// CallControl marks a call that checks or establishes a binding or its
	// ownership, such as HealthCheck, Describe or AdvanceControllerEpoch. It
	// must get through when every other class is saturated.
	CallControl CallClass = iota + 1
	// CallInteractive marks an ordinary, small read or write made for an API
	// request, a reconciler or a worker. Most methods are interactive.
	CallInteractive
	// CallBackground marks maintenance work, such as retention, receipt
	// pruning and recovery scans, which favors throughput and may wait.
	CallBackground
	// CallStream marks a poller's read, such as an event stream following new
	// events. The catalog never assigns it; the poller chooses it for an
	// otherwise interactive read, so the number of open streams cannot use up
	// the interactive limit.
	CallStream
	// CallBulk marks a call that moves a whole body of up to several
	// megabytes, such as a result, a prompt result receipt or an artifact.
	CallBulk
)

// String returns the class's name.
func (c CallClass) String() string {
	switch c {
	case CallControl:
		return "control"
	case CallInteractive:
		return "interactive"
	case CallBackground:
		return "background"
	case CallStream:
		return "stream"
	case CallBulk:
		return "bulk"
	default:
		return "unknown"
	}
}

// LookupMethod returns the catalog entry for a provider method.
func LookupMethod(name string) (MethodInfo, bool) {
	info, ok := methodCatalog[name]
	return info, ok
}

// Methods returns a copy of the method catalog, keyed by method name. It lists
// every method of [StateProvider], [ArtifactProvider], [OperationReceiptStore]
// and [ControllerEpochAdvancer], so a storage client, a transport or the
// shared suite can tell reads from writes, and bulk transfers from ordinary
// calls, without guessing from names.
func Methods() map[string]MethodInfo {
	return maps.Clone(methodCatalog)
}

func stateRead() MethodInfo {
	return MethodInfo{Kind: MethodRead, Contract: StateContract, Class: CallInteractive}
}

func stateWrite(naturalKey string) MethodInfo {
	return MethodInfo{Kind: MethodWrite, Contract: StateContract, NaturalKey: naturalKey, Class: CallInteractive}
}

// as returns info with its call class set to class.
func (info MethodInfo) as(class CallClass) MethodInfo {
	info.Class = class
	return info
}

var methodCatalog = map[string]MethodInfo{
	"AcquireChatTurn":                           stateWrite(""),
	"AcquireLock":                               stateWrite(""),
	"AdmitGatewayEvent":                         stateWrite("$1.Event.Namespace, $1.Event.GatewayUID, $1.Event.ExternalEventID"),
	"AdvanceControllerEpoch":                    {Kind: MethodAdmin, Class: CallControl},
	"AppendExecutionEvent":                      stateWrite(""),
	"AppendExecutionEventIfAbsent":              stateWrite("$1.Namespace, $1.StreamType, $1.StreamID, $2"),
	"AppendExecutionEventWithPlanIfAbsent":      stateWrite("$1.Namespace, $1.StreamType, $1.StreamID, $2"),
	"AppendMessages":                            stateWrite(""),
	"AppendMessagesWithLock":                    stateWrite(""),
	"ArchiveMemoryProposal":                     stateWrite(""),
	"BindPatchProposalPublicationEvidence":      stateWrite(""),
	"BindSessionCleanupIdentity":                stateWrite(""),
	"CancelWorkActions":                         stateWrite(""),
	"CheckTaskJobAuthority":                     stateRead(),
	"ClaimNextGatewayDelivery":                  stateWrite(""),
	"ClaimNextGatewayEvent":                     stateWrite(""),
	"ClaimOutboxProjectionRecords":              stateWrite(""),
	"CommitSessionTurn":                         stateWrite(""),
	"CompleteOutboxProjectionRecord":            stateWrite("$1.ID, $1.OperationID, $1.OperationDigest"),
	"CompleteScanRunCancellation":               stateWrite(""),
	"CompleteScanTaskIngestion":                 stateWrite(""),
	"CompleteSessionCleanup":                    stateWrite("$1.Namespace, $1.SessionName, $1.OperationID, $1.OperationDigest"),
	"CountImplementationJobs":                   stateRead(),
	"CountUnsettledOutboxProjections":           stateRead(),
	"CreateActionRecord":                        stateWrite(""),
	"CreateCommandEvent":                        stateWrite(""),
	"CreateDroppedFinding":                      stateWrite(""),
	"CreateGatewayDelivery":                     stateWrite("$1.Namespace, $1.ID"),
	"CreateGitHubMutationRecord":                stateWrite(""),
	"CreateImplementationJob":                   stateWrite(""),
	"CreateMemory":                              stateWrite(""),
	"CreateMemoryProposal":                      stateWrite(""),
	"CreateMonitorEvent":                        stateWrite(""),
	"CreateMonitorRun":                          stateWrite(""),
	"CreatePatchProposal":                       stateWrite(""),
	"CreateRepairJob":                           stateWrite(""),
	"CreateReviewPublishRecord":                 stateWrite(""),
	"CreateReviewRecord":                        stateWrite(""),
	"CreateScanRun":                             stateWrite(""),
	"CreateSession":                             stateWrite(""),
	"CreateWorkAction":                          stateWrite(""),
	"DeferGatewayEventProjection":               stateWrite(""),
	"DeleteArtifacts":                           {Kind: MethodWrite, Contract: ArtifactsContract, Class: CallInteractive},
	"DeleteExecutionEvents":                     stateWrite(""),
	"DeleteGatewayTaskCleanupReceipt":           stateWrite("").as(CallBackground),
	"DeleteMemory":                              stateWrite(""),
	"DeleteParentMessages":                      stateWrite(""),
	"DeletePlan":                                stateWrite(""),
	"DeleteRepositoryMonitor":                   stateWrite(""),
	"DeleteResult":                              stateWrite(""),
	"DeleteSession":                             stateWrite(""),
	"DeleteTaskJobRevocations":                  stateWrite(""),
	"DeleteTaskMessages":                        stateWrite(""),
	"Describe":                                  {Kind: MethodRead, Class: CallControl},
	"EnqueueGatewayMessage":                     stateWrite("$1.Namespace, $1.EventID, $1.RequestID"),
	"EnqueueOutboxProjectionRecord":             stateWrite("$1.ID, $1.PayloadDigest"),
	"EnsureSessionSoulWithLock":                 stateWrite(""),
	"ExpireGatewayEvent":                        stateWrite(""),
	"ExpireGatewayEventWithDelivery":            stateWrite("$1.Delivery.Namespace, $1.EventID"),
	"FreezeGatewayEventTaskRuntimeAllowedTools": stateWrite(""),
	"GetActionRecord":                           stateRead(),
	"GetArtifact":                               {Kind: MethodRead, Contract: ArtifactsContract, Class: CallBulk},
	"GetCommandEvent":                           stateRead(),
	"GetControllerEpochMirror":                  {Kind: MethodRead, Class: CallControl},
	"GetFinding":                                stateRead(),
	"GetFindingCounts":                          stateRead(),
	"GetGatewayDelivery":                        stateRead(),
	"GetGatewayEvent":                           stateRead(),
	"GetGatewayEventDuplicate":                  stateRead(),
	"GetGatewayEventForTask":                    stateRead(),
	"GetGatewayMessageBudget":                   stateRead(),
	"GetGatewayQueueStats":                      stateRead(),
	"GetGatewayTaskCleanupReceipt":              stateRead(),
	"GetGitHubMutationRecord":                   stateRead(),
	"GetImplementationJob":                      stateRead(),
	"GetLatestExecutionEventSeq":                stateRead(),
	"GetLatestExecutionEventSeqs":               stateRead(),
	"GetLatestThreatModel":                      stateRead(),
	"GetMemory":                                 stateRead(),
	"GetMemoryProposal":                         stateRead(),
	// GetMessages marks the messages it returns as read when markRead is set.
	"GetMessages":                  stateWrite(""),
	"GetMonitorItem":               stateRead(),
	"GetMonitorRun":                stateRead(),
	"GetOutboxProjection":          stateRead(),
	"GetPlan":                      stateRead(),
	"GetPromptResultReceipt":       stateRead().as(CallBulk),
	"GetRepairJob":                 stateRead(),
	"GetRepositoryMonitor":         stateRead(),
	"GetResult":                    stateRead().as(CallBulk),
	"GetReviewPublishRecord":       stateRead(),
	"GetReviewRecord":              stateRead(),
	"GetReviewSlice":               stateRead(),
	"GetScanRun":                   stateRead(),
	"GetScanTaskIngestion":         stateRead(),
	"GetSession":                   stateRead(),
	"GetSessionCleanupCompletion":  stateRead(),
	"GetSessionCleanupIdentity":    stateRead(),
	"GetSessionCleanupIntent":      stateRead(),
	"GetSessionLineage":            stateRead(),
	"GetSessionTurnCleanupReceipt": stateRead(),
	"GetSessionType":               stateRead(),
	"GetWorkAction":                stateRead(),
	"HasGatewayTaskTombstone":      stateRead(),
	"HasSessionCleanupFenceForUID": stateRead(),
	"HasSessionCleanupIntent":      stateRead(),
	"HealthCheck":                  {Kind: MethodRead, Class: CallControl},
	// IsLocked clears an expired Task lock it finds.
	"IsLocked":                            stateWrite(""),
	"LinkUsagePullRequest":                stateWrite(""),
	"ListActionRecords":                   stateRead(),
	"ListActiveScanRuns":                  stateRead(),
	"ListArtifacts":                       {Kind: MethodRead, Contract: ArtifactsContract, Class: CallInteractive},
	"ListCommandEvents":                   stateRead(),
	"ListDroppedFindings":                 stateRead(),
	"ListExecutionEvents":                 stateRead(),
	"ListFindings":                        stateRead(),
	"ListGatewayDeliveries":               stateRead(),
	"ListGatewayEvents":                   stateRead(),
	"ListGatewaySessionCleanupCandidates": stateRead().as(CallBackground),
	"ListGatewayTaskCleanupReceipts":      stateRead().as(CallBackground),
	"ListGitHubMutationRecords":           stateRead(),
	"ListImplementationJobs":              stateRead(),
	"ListMemories":                        stateRead(),
	"ListMemoryProposals":                 stateRead(),
	"ListMonitorEvents":                   stateRead(),
	"ListMonitorItems":                    stateRead(),
	"ListMonitorRuns":                     stateRead(),
	"ListPatchProposals":                  stateRead(),
	"ListRepairJobs":                      stateRead(),
	"ListReviewPublishRecords":            stateRead(),
	"ListReviewRecords":                   stateRead(),
	"ListReviewSlices":                    stateRead(),
	"ListScanRuns":                        stateRead(),
	"ListScanRunsPendingCancellation":     stateRead(),
	"ListSessionCleanupIntents":           stateRead().as(CallBackground),
	"ListSessionCleanupTurns":             stateRead(),
	"ListSessionExecutionEvents":          stateRead(),
	"ListSessions":                        stateRead(),
	"ListSessionsPage":                    stateRead(),
	"ListTaskCounterObservations":         stateRead(),
	"ListUsagePullRequestLinks":           stateRead(),
	"ListWorkActions":                     stateRead(),
	"LoadTranscript":                      stateRead(),
	"LoadTranscriptThrough":               stateRead(),
	"LoadUsage":                           stateRead(),
	"MaintainGatewayRecords":              stateWrite("").as(CallBackground),
	"MarkExpiredGatewayEventDeadLettered": stateWrite(""),
	"MarkFindingDuplicate":                stateWrite(""),
	"MarkGatewayDeliveryDelivered":        stateWrite(""),
	"MarkGatewayDeliveryTerminal":         stateWrite(""),
	"MarkGatewayEventTaskCreated":         stateWrite(""),
	"PrepareSessionCleanup":               stateWrite(""),
	"ProjectGatewayTerminal":              stateWrite("$1.Delivery.Namespace, $1.EventID"),
	"ProjectSessionLineage":               stateWrite(""),
	"PruneOperationReceipts":              {Kind: MethodAdmin, Class: CallBackground},
	"PruneUsage":                          stateWrite("").as(CallBackground),
	"ReadSessionSoul":                     stateRead(),
	"RecordUsage":                         stateWrite("$1.Namespace, $1.ID"),
	"RecordUsagePullRequest":              stateWrite(""),
	"RegisterUsageTask":                   stateWrite(""),
	"RegisterUsageWork":                   stateWrite(""),
	"ReleaseChatTurn":                     stateWrite(""),
	"ReleaseLock":                         stateWrite(""),
	"ReleaseLockWithTokenUsage":           stateWrite(""),
	"RenewGatewayEventClaim":              stateWrite(""),
	"RequestScanRunCancellation":          stateWrite(""),
	"ResolveFindingIfCurrent":             stateWrite(""),
	"ResolveOperation":                    {Kind: MethodAdmin, Class: CallControl},
	"RetryGatewayDelivery":                stateWrite(""),
	"RetryGatewayEvent":                   stateWrite(""),
	"ReviewMemoryProposal":                stateWrite(""),
	"RevokeTaskJob":                       stateWrite(""),
	"SaveArtifact":                        {Kind: MethodWrite, Contract: ArtifactsContract, Class: CallBulk},
	"SavePlan":                            stateWrite(""),
	"SavePromptResultReceipt":             stateWrite("$1.AttemptID").as(CallBulk),
	"SaveResult":                          stateWrite("").as(CallBulk),
	"SaveThreatModel":                     stateWrite(""),
	"SaveThreatModelIfVersion":            stateWrite(""),
	"ScheduleGatewayDeliveryRetry":        stateWrite(""),
	"SearchTranscript":                    stateRead(),
	"SendMessage":                         stateWrite(""),
	"SetMemoryDisabled":                   stateWrite(""),
	"SyncControllerEpochMirror":           {Kind: MethodAdmin, Contract: StateContract, Class: CallControl},
	"UpdateActionRecord":                  stateWrite(""),
	"UpdateCommandEvent":                  stateWrite(""),
	"UpdateFindingState":                  stateWrite(""),
	"UpdateGitHubMutationRecord":          stateWrite(""),
	"UpdateImplementationJob":             stateWrite(""),
	"UpdateMemory":                        stateWrite(""),
	"UpdateMonitorRun":                    stateWrite(""),
	"UpdatePatchProposal":                 stateWrite(""),
	"UpdateRepairJob":                     stateWrite(""),
	"UpdateReviewPublishRecord":           stateWrite(""),
	"UpdateReviewSliceStatus":             stateWrite(""),
	"UpdateScanRun":                       stateWrite(""),
	"UpdateTokenCounts":                   stateWrite(""),
	"UpdateTokenCountsWithLock":           stateWrite(""),
	"UpdateWorkAction":                    stateWrite(""),
	"UpsertFinding":                       stateWrite(""),
	"UpsertMonitorItem":                   stateWrite(""),
	"UpsertObservedFinding":               stateWrite(""),
	"UpsertRepositoryMonitor":             stateWrite(""),
	"UpsertReviewSlice":                   stateWrite(""),
}
