package storagetest

import (
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// Write cases for the sealed families, Task data fences and conditional
// commands: snapshots, native sessions, Session turn records, connector
// custody, memory proposal application, scan ingestion and grouped execution
// events. Their records carry opaque sealed bytes; the cases compare them
// exactly.

const (
	sealedCaseTask       = "snapshot-task-uid"
	sealedCaseConnection = "connection-uid"
	sealedCaseSession    = "turn-session"
	sealedCaseProposal   = "proposal-a"
	sealedCaseMemory     = "memory-a"
)

var sealedCaseKey = sealedSnapshot(sealedCaseTask, "case").Key

func observeTaskDataFence(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	fence, registered, err := p.ResolveTaskDataFence(t.Context(), "tenant", "task-a")
	require.NoError(t, err)
	return []any{fence, registered}
}

func observeSnapshots(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	page, err := p.ListSealedAgentExecutionSnapshots(t.Context(), "", 100)
	require.NoError(t, err)
	return page
}

func createCaseSnapshot(t *testing.T, p storagev1.StateProvider, label string) {
	t.Helper()
	require.NoError(t, p.CreateSealedAgentExecutionSnapshot(t.Context(), sealedSnapshot(sealedCaseTask, label)))
}

func observeNativeSession(name, uid string) func(t *testing.T, p storagev1.StateProvider) any {
	return func(t *testing.T, p storagev1.StateProvider) any {
		t.Helper()
		record, err := p.GetSealedNativeSession(t.Context(), "tenant", name, uid)
		return observed(t, record, err)
	}
}

func createBoundSession(t *testing.T, p storagev1.StateProvider, name, uid string) {
	t.Helper()
	require.NoError(t, p.CreateSession(t.Context(), &storagev1.SessionRecord{Namespace: "tenant", Name: name, SessionType: "task"}))
	require.NoError(t, p.BindSessionCleanupIdentity(t.Context(), "tenant", name, uid))
}

// caseTurnRequest creates the open turn of sealedCaseSession under fence.
func caseTurnRequest(fence storagev1.ControllerEpochFence) storagev1.CreateSessionTurnRecordRequest {
	return storagev1.CreateSessionTurnRecordRequest{
		Namespace: "tenant", SessionName: sealedCaseSession, Fence: fence,
		Turn: storagev1.SessionTurn{
			Key: storagev1.SessionTurnKey{
				SessionUID: sealedCaseSession + "-uid", LeaseGeneration: 1, TaskUID: "turn-task", Attempt: 1, PromptID: "prompt",
			},
			PromptAttemptID: "turn-attempt", RequestDigest: controlDigest("case turn"), UserPrompt: "the question",
			CreatedAt: sealedBaseTime, UpdatedAt: sealedBaseTime,
		},
	}
}

func caseTurnID(t *testing.T) string {
	t.Helper()
	id, err := caseTurnRequest(suiteFence).Turn.Key.CanonicalID()
	require.NoError(t, err)
	return id
}

// caseTurnFinalization finalizes the case turn at version 1, as created.
func caseTurnFinalization(fence storagev1.ControllerEpochFence) storagev1.SealedSessionTurnFinalization {
	key := caseTurnRequest(fence).Turn.Key
	id, _ := key.CanonicalID()
	payload := []byte(`{"phase":"Succeeded"}`)
	return storagev1.SealedSessionTurnFinalization{
		Key: key, Namespace: "tenant", SessionName: sealedCaseSession, Fence: fence,
		ExpectedTurnVersion: 1, FinalizationDigest: controlDigest("case finalization"),
		TerminalKind: storagev1.SessionTurnAssistantResult, TerminalContent: "the answer",
		Projection: storagev1.OutboxProjection{
			ID: "projection-" + id, AggregateKind: "SessionTurn", AggregateID: id,
			ProjectionKind: "TaskTerminalStatus", Payload: payload, PayloadDigest: storagev1.CanonicalBytesDigest(payload),
		},
		FinalizedAt: sealedBaseTime.Add(time.Minute), UserMessageID: "user-case", TerminalMessageID: "terminal-case",
	}
}

func setupCaseTurn(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	createBoundSession(t, p, sealedCaseSession, sealedCaseSession+"-uid")
	_, err := p.CreateSessionTurnRecord(t.Context(), caseTurnRequest(suiteFence))
	require.NoError(t, err)
}

func setupFinalizedCaseTurn(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	setupCaseTurn(t, p)
	_, err := p.CommitSealedSessionTurnFinalization(t.Context(), caseTurnFinalization(suiteFence))
	require.NoError(t, err)
}

func observeCaseTurn(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	turn, err := p.GetSessionTurn(t.Context(), caseTurnID(t))
	transcript, transcriptErr := p.LoadTranscript(t.Context(), "tenant", sealedCaseSession, 0)
	require.NoError(t, transcriptErr)
	projection, projectionErr := p.GetOutboxProjection(t.Context(), caseTurnFinalization(suiteFence).Projection.ID)
	return []any{observed(t, turn, err), transcript, observed(t, projection, projectionErr)}
}

func observeCustody(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	custody := readCustody(t, p, sealedCaseConnection)
	consents, err := p.ListSealedConnectorConsents(t.Context(), "", 100)
	require.NoError(t, err)
	return []any{custody, consents}
}

func caseCredential(grant int64, label string) storagev1.SealedConnectorCredential {
	return sealedConnectorCredential(sealedCaseConnection, grant, label)
}

func putCaseCredential(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	stored := putGrant(t, p, caseCredential(1, "first"))
	require.Equal(t, caseCredentialVersion, stored.Version, "the first grant's version is fixed for the cases")
}

// caseCredentialVersion is the version a fresh dataset gives its first grant.
// The cases check it at setup, so the expectations they send stay fixed.
var caseCredentialVersion int64 = 1

func caseConsent() storagev1.SealedConnectorConsent {
	return sealedConnectorConsent(sealedCaseConnection, "consent-case", 1)
}

func caseCompletion() storagev1.SealedConnectorCompletion {
	return sealedConnectorCompletion(sealedCaseConnection, "completion-case", 1)
}

func parkCaseCompletion(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	require.NoError(t, p.CreateSealedConnectorCompletion(t.Context(), storagev1.CreateSealedConnectorCompletionRequest{
		Completion: caseCompletion(), Now: connectorNow,
	}))
}

func observeMemoryApplication(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	proposal, err := p.GetMemoryProposal(t.Context(), conditionalNamespace, sealedCaseProposal)
	require.NoError(t, err)
	memories, err := p.ListMemories(t.Context(), storagev1.MemoryFilter{Namespace: conditionalNamespace})
	require.NoError(t, err)
	return []any{proposal, memories}
}

func caseScanRun(t *testing.T, p storagev1.StateProvider) storagev1.ScanRun {
	t.Helper()
	run, err := p.GetScanRun(t.Context(), workflowNamespace, "run-1")
	require.NoError(t, err)
	return *run
}

// caseIngestionCommit reviews one finding against the case run as setup
// stored it.
func caseIngestionCommit(run storagev1.ScanRun) storagev1.ScanTaskIngestionCommit {
	updated := run
	updated.AcceptedFindings++
	updated.ReviewedSliceCount++
	notFound := storagev1.ScanIngestionReadResult{NotFound: true}
	at := workflowAt(30)
	return storagev1.ScanTaskIngestionCommit{
		Ingestion:   storagev1.ScanTaskIngestion{ScanTaskIdentity: ingestionIdentity("review-a"), FindingIDs: []string{"finding-1"}, IngestedAt: at},
		ExpectedRun: run, Run: updated, At: at,
		Steps: []storagev1.ScanIngestionStep{
			{GetFinding: &storagev1.FindingKey{Namespace: workflowNamespace, ID: "finding-1"}, Expect: &notFound},
			{UpsertObservedFinding: ingestionFinding("finding-1")},
		},
	}
}

func observeIngestion(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	ingestion, err := p.GetScanTaskIngestion(t.Context(), ingestionIdentity("review-a"))
	finding, findingErr := p.GetFinding(t.Context(), workflowNamespace, "finding-1")
	return []any{observed(t, ingestion, err), caseScanRun(t, p), observed(t, finding, findingErr)}
}

func caseEventGroup() []storagev1.ExecutionEventAppend {
	second := caseEvent()
	second.Summary = "planned again"
	return []storagev1.ExecutionEventAppend{{Event: caseEvent(), DedupeKey: "group-1"}, {Event: second, DedupeKey: "group-2"}}
}

func init() {
	// scanRun is the case run as setup stored it, which a scan ingestion
	// commit names as the run it read. Cases run one at a time.
	var scanRun storagev1.ScanRun
	stateWriteCases = append(stateWriteCases,
		writeCase[storagev1.StateProvider]{
			method:  "RegisterTaskDataFence",
			args:    fixed("tenant", "task-a"),
			observe: observeTaskDataFence,
			afterCommit: func(t *testing.T, p storagev1.StateProvider) {
				fence, registered, err := p.ResolveTaskDataFence(t.Context(), "tenant", "task-a")
				require.NoError(t, err)
				require.True(t, registered)
				require.NoError(t, p.ReleaseTaskDataFence(t.Context(), fence))
			},
		},
		writeCase[storagev1.StateProvider]{
			method: "ReleaseTaskDataFence",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				_, _, err := p.RegisterTaskDataFence(t.Context(), "tenant", "task-a")
				require.NoError(t, err)
			},
			args:    fixed(storagev1.TaskDataFence{Namespace: "tenant", TaskName: "task-a"}),
			observe: observeTaskDataFence,
			afterCommit: func(t *testing.T, p storagev1.StateProvider) {
				_, _, err := p.RegisterTaskDataFence(t.Context(), "tenant", "task-a")
				require.NoError(t, err)
			},
		},
		writeCase[storagev1.StateProvider]{
			method:  "CreateSealedAgentExecutionSnapshot",
			args:    fixed(sealedSnapshot(sealedCaseTask, "case")),
			observe: observeSnapshots,
			afterCommit: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.DeleteAgentExecutionSnapshot(t.Context(), sealedCaseKey))
			},
		},
		writeCase[storagev1.StateProvider]{
			method:  "DeleteAgentExecutionSnapshot",
			setup:   func(t *testing.T, p storagev1.StateProvider) { createCaseSnapshot(t, p, "case") },
			args:    fixed(sealedCaseKey),
			observe: observeSnapshots,
			afterCommit: func(t *testing.T, p storagev1.StateProvider) {
				createCaseSnapshot(t, p, "case")
			},
		},
		writeCase[storagev1.StateProvider]{
			method: "DeleteAgentExecutionSnapshots",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				createCaseSnapshot(t, p, "case")
				createCaseSnapshot(t, p, "other")
			},
			args:    fixed(sealedCaseTask),
			observe: observeSnapshots,
			afterCommit: func(t *testing.T, p storagev1.StateProvider) {
				createCaseSnapshot(t, p, "case")
			},
		},
		writeCase[storagev1.StateProvider]{
			method: "SaveSealedNativeSession",
			setup:  func(t *testing.T, p storagev1.StateProvider) { createBoundSession(t, p, "captured", "owner-uid") },
			args: fixed(storagev1.SaveSealedNativeSessionRequest{
				Record: sealedNativeSession("captured", "owner-uid", "case", 1), RequestDigest: controlDigest("capture request case"),
			}),
			observe: observeNativeSession("captured", "owner-uid"),
		},
		writeCase[storagev1.StateProvider]{
			method:  "StageSealedNativeSessionImport",
			args:    fixed(nativeImportRequest("imported", "case")),
			observe: observeNativeSession("imported", ""),
		},
		writeCase[storagev1.StateProvider]{
			method: "CreateSessionTurnRecord",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				createBoundSession(t, p, sealedCaseSession, sealedCaseSession+"-uid")
			},
			args:          func(fence storagev1.ControllerEpochFence) []any { return []any{caseTurnRequest(fence)} },
			explicitFence: true,
			observe:       observeCaseTurn,
		},
		writeCase[storagev1.StateProvider]{
			method:        "CommitSealedSessionTurnFinalization",
			setup:         setupCaseTurn,
			args:          func(fence storagev1.ControllerEpochFence) []any { return []any{caseTurnFinalization(fence)} },
			explicitFence: true,
			observe:       observeCaseTurn,
		},
		writeCase[storagev1.StateProvider]{
			method: "ActivateSessionTurnProjection",
			setup:  setupFinalizedCaseTurn,
			args: func(fence storagev1.ControllerEpochFence) []any {
				finalization := caseTurnFinalization(fence)
				id, _ := finalization.Key.CanonicalID()
				return []any{storagev1.ActivateSessionTurnProjectionRequest{
					TurnID: id, ProjectionID: finalization.Projection.ID, Fence: fence,
					FinalizationDigest: finalization.FinalizationDigest, ExpectedAggregateKind: finalization.Projection.AggregateKind,
					ExpectedProjectionKind: finalization.Projection.ProjectionKind, ExpectedPayloadDigest: finalization.Projection.PayloadDigest,
					AvailableAt: finalization.FinalizedAt, UpdatedAt: finalization.FinalizedAt,
				}}
			},
			explicitFence: true,
			observe:       observeCaseTurn,
		},
		writeCase[storagev1.StateProvider]{
			method:  "PutSealedConnectorCredential",
			args:    fixed(storagev1.PutSealedConnectorCredentialRequest{Credential: caseCredential(1, "first"), Now: connectorNow}),
			observe: observeCustody,
		},
		writeCase[storagev1.StateProvider]{
			method: "ReplaceSealedConnectorCredential",
			setup:  putCaseCredential,
			args: fixed(storagev1.ReplaceSealedConnectorCredentialRequest{
				Credential: caseCredential(1, "refreshed"), ExpectedVersion: caseCredentialVersion,
				Retire: &storagev1.ConnectorRetirement{RevocableUntil: connectorNow.Add(time.Hour)}, Now: connectorNow,
			}),
			observe: observeCustody,
		},
		writeCase[storagev1.StateProvider]{
			method: "RetireSealedConnectorCredential",
			setup:  putCaseCredential,
			args: fixed(storagev1.SealedRetiredConnectorCredential{
				ConnectionUID: sealedCaseConnection, DataKeyNonce: sealedBytes("retired data key nonce"),
				DataKeyCiphertext: sealedBytes("retired data key"), Nonce: sealedBytes("retired nonce"),
				Ciphertext: sealedBytes("retired ciphertext"), RetiredAt: connectorNow,
			}),
			observe: observeCustody,
		},
		writeCase[storagev1.StateProvider]{
			method:  "TombstoneConnectorCustody",
			setup:   putCaseCredential,
			args:    fixed(sealedCaseConnection),
			observe: observeCustody,
		},
		writeCase[storagev1.StateProvider]{
			method:  "ShredConnectorCredential",
			setup:   putCaseCredential,
			args:    fixed(sealedCaseConnection, caseCredentialVersion),
			observe: observeCustody,
			afterCommit: func(t *testing.T, p storagev1.StateProvider) {
				putGrant(t, p, caseCredential(2, "after shred"))
			},
		},
		writeCase[storagev1.StateProvider]{
			method:  "DeleteConnectorCredential",
			setup:   putCaseCredential,
			args:    fixed(sealedCaseConnection),
			observe: observeCustody,
		},
		writeCase[storagev1.StateProvider]{
			method:  "CreateSealedConnectorConsent",
			args:    fixed(storagev1.CreateSealedConnectorConsentRequest{Consent: caseConsent(), Now: connectorNow}),
			observe: observeCustody,
		},
		writeCase[storagev1.StateProvider]{
			method: "ConsumeSealedConnectorConsent",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.CreateSealedConnectorConsent(t.Context(),
					storagev1.CreateSealedConnectorConsentRequest{Consent: caseConsent(), Now: connectorNow}))
			},
			args:    fixed(caseConsent().Nonce, connectorNow),
			observe: observeCustody,
		},
		writeCase[storagev1.StateProvider]{
			method:  "CreateSealedConnectorCompletion",
			args:    fixed(storagev1.CreateSealedConnectorCompletionRequest{Completion: caseCompletion(), Now: connectorNow}),
			observe: observeCustody,
		},
		writeCase[storagev1.StateProvider]{
			method:  "ConsumeSealedConnectorCompletion",
			setup:   parkCaseCompletion,
			args:    fixed(caseCompletion().Nonce),
			observe: observeCustody,
			afterCommit: func(t *testing.T, p storagev1.StateProvider) {
				parkCaseCompletion(t, p)
			},
		},
		writeCase[storagev1.StateProvider]{
			method: "CommitSealedConnectorCompletion",
			setup:  parkCaseCompletion,
			args: fixed(storagev1.CommitSealedConnectorCompletionRequest{
				Nonce: caseCompletion().Nonce, PayloadNonce: sealedBytes("committed payload nonce"), Payload: sealedBytes("committed payload"),
				Credential: caseCredential(1, "committed"), ExpectedCompletions: []storagev1.SealedConnectorCompletionRef{caseCompletion().Ref()},
				Now: connectorNow,
			}),
			observe: observeCustody,
		},
		writeCase[storagev1.StateProvider]{
			method:  "DeleteConnectorCompletion",
			setup:   parkCaseCompletion,
			args:    fixed(caseCompletion().Nonce),
			observe: observeCustody,
			afterCommit: func(t *testing.T, p storagev1.StateProvider) {
				parkCaseCompletion(t, p)
			},
		},
		writeCase[storagev1.StateProvider]{
			method: "DeleteConnectorConsentsForConnection",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.CreateSealedConnectorConsent(t.Context(),
					storagev1.CreateSealedConnectorConsentRequest{Consent: caseConsent(), Now: connectorNow}))
				parkCaseCompletion(t, p)
			},
			args:    fixed(sealedCaseConnection),
			observe: observeCustody,
			afterCommit: func(t *testing.T, p storagev1.StateProvider) {
				parkCaseCompletion(t, p)
			},
		},
		writeCase[storagev1.StateProvider]{
			method: "CommitMemoryProposalApplication",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				acceptedMemoryProposal(t, p, sealedCaseProposal)
			},
			args: func(storagev1.ControllerEpochFence) []any {
				memory := &storagev1.Memory{ID: sealedCaseMemory, Namespace: conditionalNamespace, TaskName: "task-a", AgentName: "agent-a",
					Content: "content of " + sealedCaseProposal, Tags: []string{"applied"}, CreatedAt: sealedBaseTime, UpdatedAt: sealedBaseTime}
				return []any{storagev1.MemoryProposalApplication{
					Target: storagev1.MemoryProposalApplyTarget{
						Namespace: conditionalNamespace, ProposalID: sealedCaseProposal, ProposalStatus: storagev1.MemoryProposalStatusAccepted,
					},
					AppliedBy: "reviewer", AppliedAt: sealedBaseTime, Memory: memory,
				}}
			},
			observe: observeMemoryApplication,
		},
		writeCase[storagev1.StateProvider]{
			method: "CommitScanTaskIngestion",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.CreateScanRun(t.Context(), workflowScanRun("run-1", "running")))
				scanRun = caseScanRun(t, p)
			},
			args:    func(storagev1.ControllerEpochFence) []any { return []any{caseIngestionCommit(scanRun)} },
			observe: observeIngestion,
		},
		writeCase[storagev1.StateProvider]{
			method: "CompleteScanTaskIngestion",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.CreateScanRun(t.Context(), workflowScanRun("run-1", "running")))
				applied, err := p.CommitScanTaskIngestion(t.Context(), caseIngestionCommit(caseScanRun(t, p)))
				require.NoError(t, err)
				require.True(t, applied)
			},
			args:    fixed(ingestionIdentity("review-a")),
			observe: observeIngestion,
		},
		writeCase[storagev1.StateProvider]{
			method:  "AppendExecutionEventsIfAbsent",
			args:    func(storagev1.ControllerEpochFence) []any { return []any{caseEventGroup()} },
			observe: observeEvents,
		},
	)
}
