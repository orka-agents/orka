package v1

import "maps"

type MethodKind uint8

const (
	MethodRead MethodKind = iota + 1
	MethodWrite
	MethodAdmin
)

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

type MethodInfo struct {
	Kind       MethodKind
	Contract   string
	NaturalKey string
	Class      CallClass
}

type CallClass uint8

const (
	CallControl CallClass = iota + 1
	CallInteractive
	CallBackground
	CallStream
	CallBulk
)

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

func LookupMethod(name string) (MethodInfo, bool) {
	info, ok := methodCatalog[name]
	return info, ok
}

func Methods() map[string]MethodInfo {
	return maps.Clone(methodCatalog)
}

func stateRead() MethodInfo {
	return MethodInfo{Kind: MethodRead, Contract: StateContract, Class: CallInteractive}
}

func stateWrite(naturalKey string) MethodInfo {
	return MethodInfo{Kind: MethodWrite, Contract: StateContract, NaturalKey: naturalKey, Class: CallInteractive}
}

func (info MethodInfo) as(class CallClass) MethodInfo {
	info.Class = class
	return info
}

var methodCatalog = map[string]MethodInfo{
	"AcquireChatTurn":                           stateWrite(""),
	"AcquireLock":                               stateWrite(""),
	"ActivateSessionTurnProjection":             stateWrite(""),
	"AdmitGatewayEvent":                         stateWrite("$1.Event.Namespace, $1.Event.GatewayUID, $1.Event.ExternalEventID"),
	"AdvanceControllerEpoch":                    {Kind: MethodAdmin, Class: CallControl},
	"AppendAHPJournal":                          stateWrite(""),
	"AppendExecutionEvent":                      stateWrite(""),
	"AppendExecutionEventIfAbsent":              stateWrite("$1.Namespace, $1.StreamType, $1.StreamID, $2"),
	"AppendExecutionEventWithPlanIfAbsent":      stateWrite("$1.Namespace, $1.StreamType, $1.StreamID, $2"),
	"AppendExecutionEventsIfAbsent":             stateWrite(""),
	"AppendMessages":                            stateWrite(""),
	"AppendMessagesWithLock":                    stateWrite(""),
	"ArchiveMemoryProposal":                     stateWrite(""),
	"BeginMemoryConsolidationRun":               stateWrite("$1.Namespace, $1.TaskUID"),
	"BindDataset":                               {Kind: MethodAdmin, Class: CallControl},
	"BindPatchProposalPublicationEvidence":      stateWrite(""),
	"BindSessionCleanupIdentity":                stateWrite(""),
	"CancelWorkActions":                         stateWrite(""),
	"CheckTaskDataFence":                        stateRead(),
	"CheckTaskJobAuthority":                     stateRead(),
	"ClaimNextGatewayDelivery":                  stateWrite(""),
	"ClaimNextGatewayEvent":                     stateWrite(""),
	"ClaimOutboxProjectionRecords":              stateWrite(""),
	"CommitMemoryProposalApplication":           stateWrite(""),
	"CommitScanTaskIngestion":                   stateWrite(""),
	"CompleteMemoryConsolidationRun":            stateWrite("$1.Namespace, $1.TaskUID"),
	"CommitSealedConnectorCompletion":           stateWrite(""),
	"CommitSealedSessionTurnFinalization":       stateWrite("$1.Key.SessionUID, $1.Key.LeaseGeneration, $1.Key.TaskUID, $1.Key.Attempt, $1.Key.PromptID").as(CallBulk),
	"CommitSessionTurn":                         stateWrite(""),
	"CompleteDatasetBootstrap":                  {Kind: MethodAdmin, Class: CallControl},
	"CompleteOutboxProjectionRecord":            stateWrite("$1.ID, $1.OperationID, $1.OperationDigest"),
	"CompleteScanRunCancellation":               stateWrite(""),
	"CompleteScanTaskIngestion":                 stateWrite(""),
	"CompleteSessionCleanup":                    stateWrite("$1.Namespace, $1.SessionName, $1.OperationID, $1.OperationDigest"),
	"ConsumeMessages":                           stateWrite(""),
	"ConsumeSealedConnectorCompletion":          stateWrite(""),
	"ConsumeSealedConnectorConsent":             stateWrite(""),
	"CountAgentExecutionSnapshotReferences":     stateRead().as(CallBackground),
	"CountImplementationJobs":                   stateRead(),
	"CountUnsettledOutboxProjections":           stateRead(),
	"CreateAHPToken":                            stateWrite(""),
	"CreateActionRecord":                        stateWrite(""),
	"CreateAuthSession":                         stateWrite(""),
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
	"CreateSealedAgentExecutionSnapshot":        stateWrite("$1.Key.TaskUID, $1.Key.Digest").as(CallBulk),
	"CreateSealedConnectorCompletion":           stateWrite(""),
	"CreateSealedConnectorConsent":              stateWrite(""),
	"CreateSession":                             stateWrite(""),
	"CreateSessionTurnRecord":                   stateWrite("$1.Turn.Key.SessionUID, $1.Turn.Key.LeaseGeneration, $1.Turn.Key.TaskUID, $1.Turn.Key.Attempt, $1.Turn.Key.PromptID"),
	"CreateWorkAction":                          stateWrite(""),
	"DeferGatewayEventProjection":               stateWrite(""),
	"DeleteAHPChannelStates":                    stateWrite(""),
	"DeleteAHPRecord":                           stateWrite(""),
	"DeleteAHPRecords":                          stateWrite(""),
	"DeleteAHPSession":                          stateWrite(""),
	"DeleteAgentExecutionSnapshot":              stateWrite("").as(CallBackground),
	"DeleteAgentExecutionSnapshots":             stateWrite(""),
	"DeleteArtifacts":                           {Kind: MethodWrite, Contract: ArtifactsContract, Class: CallInteractive},
	"DeleteConnectorCompletion":                 stateWrite(""),
	"DeleteConnectorConsentsForConnection":      stateWrite(""),
	"DeleteConnectorCredential":                 stateWrite(""),
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
	"DetachDatasetBinding":                      {Kind: MethodAdmin, Class: CallControl},
	"EnqueueGatewayMessage":                     stateWrite("$1.Namespace, $1.EventID, $1.RequestID"),
	"EnqueueOutboxProjectionRecord":             stateWrite("$1.ID, $1.PayloadDigest"),
	"EnsureSessionSoulWithLock":                 stateWrite(""),
	"ExpireGatewayEvent":                        stateWrite(""),
	"ExpireGatewayEventWithDelivery":            stateWrite("$1.Delivery.Namespace, $1.EventID"),
	"FreezeGatewayEventTaskRuntimeAllowedTools": stateWrite(""),
	"GetAHPChannelState":                        stateRead(),
	"GetAHPClient":                              stateRead(),
	"GetAHPRecord":                              stateRead(),
	"GetAHPServerSeq":                           stateRead(),
	"GetAHPSession":                             stateRead(),
	"GetAHPTokenByHash":                         stateRead(),
	"GetActionRecord":                           stateRead(),
	"GetArtifact":                               {Kind: MethodRead, Contract: ArtifactsContract, Class: CallBulk},
	"GetAuthSessionByHash":                      stateRead(),
	"GetCommandEvent":                           stateRead(),
	"GetControllerEpochMirror":                  {Kind: MethodRead, Class: CallControl},
	"GetDatasetBinding":                         {Kind: MethodRead, Class: CallControl},
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
	"GetMemoryConsolidationRun":                 stateRead(),
	"GetMemoryProposal":                         stateRead(),
	"GetMonitorItem":                            stateRead(),
	"GetMonitorRun":                             stateRead(),
	"GetOutboxProjection":                       stateRead(),
	"GetPlan":                                   stateRead(),
	"GetPromptResultReceipt":                    stateRead().as(CallBulk),
	"GetRepairJob":                              stateRead(),
	"GetRepositoryMonitor":                      stateRead(),
	"GetResult":                                 stateRead().as(CallBulk),
	"GetReviewPublishRecord":                    stateRead(),
	"GetReviewRecord":                           stateRead(),
	"GetReviewSlice":                            stateRead(),
	"GetScanRun":                                stateRead(),
	"GetScanTaskIngestion":                      stateRead(),
	"GetSealedAgentExecutionSnapshot":           stateRead().as(CallBulk),
	"GetSealedConnectorCompletion":              stateRead(),
	"GetSealedConnectorCredential":              stateRead(),
	"GetSealedNativeSession":                    stateRead().as(CallBulk),
	"GetSession":                                stateRead(),
	"GetSessionCleanupCompletion":               stateRead(),
	"GetSessionCreatedAt":                       stateRead(),
	"GetSessionCleanupIdentity":                 stateRead(),
	"GetSessionCleanupIntent":                   stateRead(),
	"GetSessionLineage":                         stateRead(),
	"GetSessionTurn":                            stateRead(),
	"GetSessionTurnCleanupReceipt":              stateRead(),
	"GetSessionType":                            stateRead(),
	"GetWorkAction":                             stateRead(),
	"HasGatewayTaskTombstone":                   stateRead(),
	"HasSessionCleanupFenceForUID":              stateRead(),
	"HasSessionCleanupIntent":                   stateRead(),
	"HealthCheck":                               {Kind: MethodRead, Class: CallControl},
	"GetMemoryConsolidationCursor":              stateRead(),
	"IsLocked":                                  stateWrite(""),
	"LinkUsagePullRequest":                      stateWrite(""),
	"ListAHPChannels":                           stateRead(),
	"ListAHPRecords":                            stateRead(),
	"ListAHPSessions":                           stateRead(),
	"ListAHPTokens":                             stateRead(),
	"ListActionRecords":                         stateRead(),
	"ListActiveScanRuns":                        stateRead(),
	"ListAgentExecutionSnapshotMetadataBefore":  stateRead().as(CallBackground),
	"ListArtifacts":                             {Kind: MethodRead, Contract: ArtifactsContract, Class: CallInteractive},
	"ListCommandEvents":                         stateRead(),
	"ListDroppedFindings":                       stateRead(),
	"ListExecutionEvents":                       stateRead(),
	"ListFindings":                              stateRead(),
	"ListGatewayDeliveries":                     stateRead(),
	"ListGatewayEvents":                         stateRead(),
	"ListGatewaySessionCleanupCandidates":       stateRead().as(CallBackground),
	"ListGatewayTaskCleanupReceipts":            stateRead().as(CallBackground),
	"ListGitHubMutationRecords":                 stateRead(),
	"ListImplementationJobs":                    stateRead(),
	"ListMemories":                              stateRead(),
	"ListMemoryConsolidationSessions":           stateRead().as(CallBackground),
	"ListMemoryProposals":                       stateRead(),
	"ListMonitorEvents":                         stateRead(),
	"ListMonitorItems":                          stateRead(),
	"ListMonitorRuns":                           stateRead(),
	"ListPatchProposals":                        stateRead(),
	"ListRepairJobs":                            stateRead(),
	"ListReviewPublishRecords":                  stateRead(),
	"ListReviewRecords":                         stateRead(),
	"ListReviewSlices":                          stateRead(),
	"ListScanRuns":                              stateRead(),
	"ListScanRunsPendingCancellation":           stateRead(),
	"ListSealedAgentExecutionSnapshots":         stateRead().as(CallBulk),
	"ListSealedConnectorCompletions":            stateRead(),
	"ListSealedConnectorConsents":               stateRead(),
	"ListSealedConnectorCredentials":            stateRead(),
	"ListSealedNativeSessions":                  stateRead().as(CallBulk),
	"ListSealedRetiredConnectorCredentials":     stateRead(),
	"ListSessionCleanupIntents":                 stateRead().as(CallBackground),
	"ListSessionCleanupTurns":                   stateRead(),
	"ListSessionExecutionEvents":                stateRead(),
	"ListSessions":                              stateRead(),
	"ListSessionsPage":                          stateRead(),
	"ListTaskCounterObservations":               stateRead(),
	"ListUnreadMessages":                        stateRead(),
	"ListUsagePullRequestLinks":                 stateRead(),
	"ListUsageTasksInSessions":                  stateRead(),
	"ListWorkActions":                           stateRead(),
	"LoadTranscript":                            stateRead(),
	"LoadTranscriptThrough":                     stateRead(),
	"LoadUsage":                                 stateRead(),
	"MaintainGatewayRecords":                    stateWrite("").as(CallBackground),
	"MarkExpiredGatewayEventDeadLettered":       stateWrite(""),
	"MarkFindingDuplicate":                      stateWrite(""),
	"MarkGatewayDeliveryDelivered":              stateWrite(""),
	"MarkGatewayDeliveryTerminal":               stateWrite(""),
	"MarkGatewayEventTaskCreated":               stateWrite(""),
	"MarkWorkActionRetryPending":                stateWrite(""),
	"PrepareSessionCleanup":                     stateWrite(""),
	"PreviewScanTaskIngestion":                  stateRead(),
	"ProjectGatewayTerminal":                    stateWrite("$1.Delivery.Namespace, $1.EventID"),
	"ProjectSessionLineage":                     stateWrite(""),
	"PruneOperationReceipts":                    {Kind: MethodAdmin, Class: CallBackground},
	"PruneUsage":                                stateWrite("").as(CallBackground),
	"PutAHPClient":                              stateWrite(""),
	"PutAHPRecord":                              stateWrite(""),
	"PutAHPSession":                             stateWrite(""),
	"PutSealedConnectorCredential":              stateWrite(""),
	"ReadAHPJournal":                            stateRead(),
	"ReadConnectorCustody":                      stateRead(),
	"ReadSessionSoul":                           stateRead(),
	"ReadSessionTurnFinalizationBasis":          stateRead().as(CallBulk),
	"RecordMemoryRecall":                        stateWrite(""),
	"RecordUsage":                               stateWrite("$1.Namespace, $1.ID"),
	"RecordUsagePullRequest":                    stateWrite(""),
	"RegisterTaskDataFence":                     stateWrite(""),
	"RegisterUsageTask":                         stateWrite(""),
	"RegisterUsageWork":                         stateWrite(""),
	"ReleaseChatTurn":                           stateWrite(""),
	"ReleaseLock":                               stateWrite(""),
	"ReleaseLockWithTokenUsage":                 stateWrite(""),
	"ReleaseMemoryConsolidationRun":             stateWrite("$1, $2"),
	"ReleaseTaskDataFence":                      stateWrite(""),
	"RenewAuthSession":                          stateWrite(""),
	"RenewGatewayEventClaim":                    stateWrite(""),
	"ReplaceSealedConnectorCredential":          stateWrite(""),
	"RequestScanRunCancellation":                stateWrite(""),
	"ResolveFindingIfCurrent":                   stateWrite(""),
	"ResolveMemoryProposalApplication":          stateRead(),
	"ResolveOperation":                          {Kind: MethodAdmin, Class: CallControl},
	"ResolveTaskDataFence":                      stateRead(),
	"RetireSealedConnectorCredential":           stateWrite(""),
	"RetryGatewayDelivery":                      stateWrite(""),
	"RetryGatewayEvent":                         stateWrite(""),
	"ReviewMemoryProposal":                      stateWrite(""),
	"RevokeAHPToken":                            stateWrite(""),
	"RevokeAuthSession":                         stateWrite(""),
	"RevokeTaskJob":                             stateWrite(""),
	"SaveArtifact":                              {Kind: MethodWrite, Contract: ArtifactsContract, Class: CallBulk},
	"SavePlan":                                  stateWrite(""),
	"SavePromptResultReceipt":                   stateWrite("$1.AttemptID").as(CallBulk),
	"SaveResult":                                stateWrite("").as(CallBulk),
	"SaveSealedNativeSession":                   stateWrite("").as(CallBulk),
	"SaveThreatModel":                           stateWrite(""),
	"SaveThreatModelIfVersion":                  stateWrite(""),
	"ScheduleGatewayDeliveryRetry":              stateWrite(""),
	"SearchTranscript":                          stateRead(),
	"SendMessage":                               stateWrite(""),
	"SetMemoryDisabled":                         stateWrite(""),
	"ShredConnectorCredential":                  stateWrite(""),
	"StageSealedNativeSessionImport":            stateWrite("").as(CallBulk),
	"SyncControllerEpochMirror":                 {Kind: MethodAdmin, Contract: StateContract, Class: CallControl},
	"TombstoneConnectorCustody":                 stateWrite(""),
	"UpdateActionRecord":                        stateWrite(""),
	"UpdateBindingKeyFingerprint":               {Kind: MethodAdmin, Class: CallControl},
	"UpdateCommandEvent":                        stateWrite(""),
	"UpdateFindingState":                        stateWrite(""),
	"UpdateGitHubMutationRecord":                stateWrite(""),
	"UpdateImplementationJob":                   stateWrite(""),
	"UpdateMemory":                              stateWrite(""),
	"UpdateMonitorRun":                          stateWrite(""),
	"UpdatePatchProposal":                       stateWrite(""),
	"UpdateRepairJob":                           stateWrite(""),
	"UpdateReviewPublishRecord":                 stateWrite(""),
	"UpdateReviewSliceStatus":                   stateWrite(""),
	"UpdateScanRun":                             stateWrite(""),
	"UpdateTokenCounts":                         stateWrite(""),
	"UpdateTokenCountsWithLock":                 stateWrite(""),
	"UpdateWorkAction":                          stateWrite(""),
	"UpsertFinding":                             stateWrite(""),
	"UpsertMonitorItem":                         stateWrite(""),
	"UpsertObservedFinding":                     stateWrite(""),
	"UpsertRepositoryMonitor":                   stateWrite(""),
	"UpsertReviewSlice":                         stateWrite(""),
}
