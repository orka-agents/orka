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
	// MethodAdmin marks an administrative write that establishes the fence,
	// settles operations or binds the dataset to an installation. It carries
	// no envelope and is not checked against the current fence.
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
}

// LookupMethod returns the catalog entry for a provider method.
func LookupMethod(name string) (MethodInfo, bool) {
	info, ok := methodCatalog[name]
	return info, ok
}

// Methods returns a copy of the method catalog, keyed by method name. It lists
// every method of [StateProvider], [ArtifactProvider], [OperationReceiptStore],
// [ControllerEpochAdvancer] and [DatasetBindingStore], so a storage client, a
// transport or the shared suite can tell reads from writes without guessing
// from names.
func Methods() map[string]MethodInfo {
	return maps.Clone(methodCatalog)
}

func stateRead() MethodInfo {
	return MethodInfo{Kind: MethodRead, Contract: StateContract}
}

func stateWrite(naturalKey string) MethodInfo {
	return MethodInfo{Kind: MethodWrite, Contract: StateContract, NaturalKey: naturalKey}
}

var methodCatalog = map[string]MethodInfo{
	"AcquireChatTurn":                           stateWrite(""),
	"AcquireLock":                               stateWrite(""),
	"AdmitGatewayEvent":                         stateWrite("$1.Event.Namespace, $1.Event.GatewayUID, $1.Event.ExternalEventID"),
	"AdvanceControllerEpoch":                    {Kind: MethodAdmin},
	"AppendExecutionEvent":                      stateWrite(""),
	"AppendExecutionEventIfAbsent":              stateWrite("$1.Namespace, $1.StreamType, $1.StreamID, $2"),
	"AppendExecutionEventWithPlanIfAbsent":      stateWrite("$1.Namespace, $1.StreamType, $1.StreamID, $2"),
	"AppendMessages":                            stateWrite(""),
	"AppendMessagesWithLock":                    stateWrite(""),
	"ArchiveMemoryProposal":                     stateWrite(""),
	"BindDataset":                               {Kind: MethodAdmin},
	"BindPatchProposalPublicationEvidence":      stateWrite(""),
	"BindSessionCleanupIdentity":                stateWrite(""),
	"CancelWorkActions":                         stateWrite(""),
	"CheckTaskJobAuthority":                     stateRead(),
	"ClaimNextGatewayDelivery":                  stateWrite(""),
	"ClaimNextGatewayEvent":                     stateWrite(""),
	"ClaimOutboxProjectionRecords":              stateWrite(""),
	"CommitSessionTurn":                         stateWrite(""),
	"CompleteDatasetBootstrap":                  {Kind: MethodAdmin},
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
	"DeleteArtifacts":                           {Kind: MethodWrite, Contract: ArtifactsContract},
	"DeleteExecutionEvents":                     stateWrite(""),
	"DeleteGatewayTaskCleanupReceipt":           stateWrite(""),
	"DeleteMemory":                              stateWrite(""),
	"DeleteParentMessages":                      stateWrite(""),
	"DeletePlan":                                stateWrite(""),
	"DeleteRepositoryMonitor":                   stateWrite(""),
	"DeleteResult":                              stateWrite(""),
	"DeleteSession":                             stateWrite(""),
	"DeleteTaskJobRevocations":                  stateWrite(""),
	"DeleteTaskMessages":                        stateWrite(""),
	"Describe":                                  {Kind: MethodRead},
	"DetachDatasetBinding":                      {Kind: MethodAdmin},
	"EnqueueGatewayMessage":                     stateWrite("$1.Namespace, $1.EventID, $1.RequestID"),
	"EnqueueOutboxProjectionRecord":             stateWrite("$1.ID, $1.PayloadDigest"),
	"EnsureSessionSoulWithLock":                 stateWrite(""),
	"ExpireGatewayEvent":                        stateWrite(""),
	"ExpireGatewayEventWithDelivery":            stateWrite("$1.Delivery.Namespace, $1.EventID"),
	"FreezeGatewayEventTaskRuntimeAllowedTools": stateWrite(""),
	"GetActionRecord":                           stateRead(),
	"GetArtifact":                               {Kind: MethodRead, Contract: ArtifactsContract},
	"GetCommandEvent":                           stateRead(),
	"GetControllerEpochMirror":                  {Kind: MethodRead},
	"GetDatasetBinding":                         {Kind: MethodRead},
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
	"GetPromptResultReceipt":       stateRead(),
	"GetRepairJob":                 stateRead(),
	"GetRepositoryMonitor":         stateRead(),
	"GetResult":                    stateRead(),
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
	"HealthCheck":                  {Kind: MethodRead},
	// IsLocked clears an expired Task lock it finds.
	"IsLocked":                            stateWrite(""),
	"LinkUsagePullRequest":                stateWrite(""),
	"ListActionRecords":                   stateRead(),
	"ListActiveScanRuns":                  stateRead(),
	"ListArtifacts":                       {Kind: MethodRead, Contract: ArtifactsContract},
	"ListCommandEvents":                   stateRead(),
	"ListDroppedFindings":                 stateRead(),
	"ListExecutionEvents":                 stateRead(),
	"ListFindings":                        stateRead(),
	"ListGatewayDeliveries":               stateRead(),
	"ListGatewayEvents":                   stateRead(),
	"ListGatewaySessionCleanupCandidates": stateRead(),
	"ListGatewayTaskCleanupReceipts":      stateRead(),
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
	"ListSessionCleanupIntents":           stateRead(),
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
	"MaintainGatewayRecords":              stateWrite(""),
	"MarkExpiredGatewayEventDeadLettered": stateWrite(""),
	"MarkFindingDuplicate":                stateWrite(""),
	"MarkGatewayDeliveryDelivered":        stateWrite(""),
	"MarkGatewayDeliveryTerminal":         stateWrite(""),
	"MarkGatewayEventTaskCreated":         stateWrite(""),
	"PrepareSessionCleanup":               stateWrite(""),
	"ProjectGatewayTerminal":              stateWrite("$1.Delivery.Namespace, $1.EventID"),
	"ProjectSessionLineage":               stateWrite(""),
	"PruneOperationReceipts":              {Kind: MethodAdmin},
	"PruneUsage":                          stateWrite(""),
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
	"ResolveOperation":                    {Kind: MethodAdmin},
	"RetryGatewayDelivery":                stateWrite(""),
	"RetryGatewayEvent":                   stateWrite(""),
	"ReviewMemoryProposal":                stateWrite(""),
	"RevokeTaskJob":                       stateWrite(""),
	"SaveArtifact":                        {Kind: MethodWrite, Contract: ArtifactsContract},
	"SavePlan":                            stateWrite(""),
	"SavePromptResultReceipt":             stateWrite("$1.AttemptID"),
	"SaveResult":                          stateWrite(""),
	"SaveThreatModel":                     stateWrite(""),
	"SaveThreatModelIfVersion":            stateWrite(""),
	"ScheduleGatewayDeliveryRetry":        stateWrite(""),
	"SearchTranscript":                    stateRead(),
	"SendMessage":                         stateWrite(""),
	"SetMemoryDisabled":                   stateWrite(""),
	"SyncControllerEpochMirror":           {Kind: MethodAdmin, Contract: StateContract},
	"UpdateActionRecord":                  stateWrite(""),
	"UpdateBindingKeyFingerprint":         {Kind: MethodAdmin},
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
