package storagetest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// securityState is what the security write cases observe: every record a
// security write can change in the workflow namespace.
type securityState struct {
	ScanRuns    []storagev1.ScanRun
	Findings    []storagev1.Finding
	Counts      storagev1.FindingCounts
	Slices      []storagev1.ReviewSlice
	ThreatModel any
	Proposals   []storagev1.PatchProposal
	Dropped     []storagev1.DroppedFinding
}

func observeSecurity(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	ctx := t.Context()
	var state securityState
	var err error
	state.ScanRuns, _, err = p.ListScanRuns(ctx, workflowNamespace, workflowScan, 100, "")
	require.NoError(t, err)
	state.Findings, _, err = p.ListFindings(ctx, storagev1.FindingFilter{Namespace: workflowNamespace, IncludeDuplicates: true})
	require.NoError(t, err)
	state.Counts, err = p.GetFindingCounts(ctx, workflowNamespace, workflowScan)
	require.NoError(t, err)
	state.Slices, _, err = p.ListReviewSlices(ctx, storagev1.ReviewSliceFilter{Namespace: workflowNamespace, RepositoryScan: workflowScan})
	require.NoError(t, err)
	model, err := p.GetLatestThreatModel(ctx, workflowNamespace, workflowScan)
	state.ThreatModel = observed(t, model, err)
	state.Proposals, err = p.ListPatchProposals(ctx, workflowNamespace, "finding")
	require.NoError(t, err)
	state.Dropped, _, err = p.ListDroppedFindings(ctx, storagev1.DroppedFindingFilter{Namespace: workflowNamespace})
	require.NoError(t, err)
	return state
}

func securityWriteSlice() *storagev1.ReviewSlice {
	return &storagev1.ReviewSlice{ID: "slice-1", Namespace: workflowNamespace, RepositoryScan: workflowScan,
		Source: "deterministic", Title: "API handlers", Summary: "request routing", LastScanRunID: "run-1",
		OwnedFiles: []storagev1.ReviewSliceFile{{Path: "api/handler.go", Reason: "entrypoint"}},
		Tests:      []storagev1.ReviewSliceTest{{Path: "api/handler_test.go", Command: "go test ./api"}},
		Tags:       []string{"auth"}}
}

func securityWriteProposal() *storagev1.PatchProposal {
	return &storagev1.PatchProposal{ID: "patch-1", Namespace: workflowNamespace, RepositoryScan: workflowScan,
		FindingID: "finding", TaskName: "patch-task", Branch: "orka/fix-1", Status: "pending", CreatedAt: workflowAt(0)}
}

func securityWriteBoundProposal() *storagev1.PatchProposal {
	head, baseline := strings.Repeat("a", 40), strings.Repeat("d", 40)
	number := 42
	proposal := securityWriteProposal()
	proposal.Status, proposal.PRNumber, proposal.PRURL = "pr_opened", &number, "https://forge.example/pr/42"
	proposal.PublicationEvidence = &storagev1.PatchPublicationEvidence{PublicationID: "publication-1",
		ArtifactDigest: "sha256:" + strings.Repeat("b", 64), SourceRepositoryID: "repo-source", SourceRef: baseline,
		SourceBaselineSHA: baseline, TargetRepositoryID: "repo-fork", TargetRef: "refs/heads/orka/fix-1",
		ExpectedCommitSHA: head, VerifiedRemoteSHA: head,
		PRIntent: storagev1.PullRequestIntent{BaseRepositoryID: "repo-source", BaseRef: "refs/heads/main",
			HeadRepositoryID: "repo-fork", HeadRef: "refs/heads/orka/fix-1", PublicationGeneration: 1, ExpectedHeadSHA: head},
		PRReceipt: storagev1.PatchPullRequestEvidence{IntentKey: "sha256:" + strings.Repeat("c", 64), ForgeID: "PR_1",
			Number: 42, URL: "https://forge.example/pr/42", State: "Open", HeadSHA: head}}
	return proposal
}

func setupScanRun(phase string) func(t *testing.T, p storagev1.StateProvider) {
	return func(t *testing.T, p storagev1.StateProvider) {
		t.Helper()
		require.NoError(t, p.CreateScanRun(t.Context(), workflowScanRun("run-1", phase)))
	}
}

func setupFindings(findings ...func() *storagev1.Finding) func(t *testing.T, p storagev1.StateProvider) {
	return func(t *testing.T, p storagev1.StateProvider) {
		t.Helper()
		for _, finding := range findings {
			require.NoError(t, p.UpsertFinding(t.Context(), finding()))
		}
	}
}

func findingFixture(fingerprint, severity string) func() *storagev1.Finding {
	return func() *storagev1.Finding { return workflowFinding(fingerprint, severity) }
}

func init() {
	security := func(c writeCase[storagev1.StateProvider]) writeCase[storagev1.StateProvider] {
		c.observe = observeSecurity
		return c
	}
	stateWriteCases = append(stateWriteCases,
		security(writeCase[storagev1.StateProvider]{
			method: "CreateScanRun",
			args: func(storagev1.ControllerEpochFence) []any {
				return []any{workflowScanRun("run-1", "pending")}
			},
		}),
		security(writeCase[storagev1.StateProvider]{
			method: "UpdateScanRun",
			setup:  setupScanRun("pending"),
			args: func(storagev1.ControllerEpochFence) []any {
				run := workflowScanRun("run-1", "running")
				run.SliceCount = 3
				return []any{run}
			},
		}),
		security(writeCase[storagev1.StateProvider]{
			method: "RequestScanRunCancellation",
			setup:  setupScanRun("running"),
			args: func(storagev1.ControllerEpochFence) []any {
				return []any{workflowScanRun("run-1", "running"), "operator cancelled"}
			},
		}),
		security(writeCase[storagev1.StateProvider]{
			method: "CompleteScanRunCancellation",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				run := workflowScanRun("run-1", "running")
				require.NoError(t, p.CreateScanRun(t.Context(), run))
				require.NoError(t, p.RequestScanRunCancellation(t.Context(), run, "operator cancelled"))
			},
			args: func(storagev1.ControllerEpochFence) []any {
				run := workflowScanRun("run-1", "running")
				run.CancellationVersion, run.CancellationPending = 1, true
				return []any{run}
			},
		}),
		security(writeCase[storagev1.StateProvider]{
			method: "UpsertReviewSlice",
			args:   func(storagev1.ControllerEpochFence) []any { return []any{securityWriteSlice()} },
		}),
		security(writeCase[storagev1.StateProvider]{
			method: "UpdateReviewSliceStatus",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.UpsertReviewSlice(t.Context(), securityWriteSlice()))
			},
			args: fixed(workflowNamespace, workflowScan, "slice-1", "run-1", "reviewed"),
		}),
		security(writeCase[storagev1.StateProvider]{
			method: "SaveThreatModel",
			args: func(storagev1.ControllerEpochFence) []any {
				return []any{&storagev1.ThreatModel{Namespace: workflowNamespace, RepositoryScan: workflowScan,
					Content: "Trust boundary: public API", Source: "generated", GeneratedByScan: "run-1"}}
			},
		}),
		security(writeCase[storagev1.StateProvider]{
			method: "SaveThreatModelIfVersion",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.SaveThreatModel(t.Context(), &storagev1.ThreatModel{Namespace: workflowNamespace,
					RepositoryScan: workflowScan, Content: "Trust boundary: public API", Source: "generated"}))
			},
			args: func(storagev1.ControllerEpochFence) []any {
				return []any{&storagev1.ThreatModel{Namespace: workflowNamespace, RepositoryScan: workflowScan,
					Content: "Trust boundary: operator edits", Source: "user"}, int64(1)}
			},
		}),
		security(writeCase[storagev1.StateProvider]{
			method: "UpsertFinding",
			args:   func(storagev1.ControllerEpochFence) []any { return []any{workflowFinding("fp-high", "high")} },
		}),
		security(writeCase[storagev1.StateProvider]{
			method: "UpsertObservedFinding",
			setup: setupFindings(func() *storagev1.Finding {
				finding := workflowFinding("fp-observed", "medium")
				finding.State = "resolved"
				return finding
			}),
			args: func(storagev1.ControllerEpochFence) []any { return []any{workflowFinding("fp-observed", "medium")} },
		}),
		security(writeCase[storagev1.StateProvider]{
			method: "UpdateFindingState",
			setup:  setupFindings(findingFixture("fp-low", "low")),
			args:   fixed(workflowNamespace, "fp-low", "dismissed"),
		}),
		security(writeCase[storagev1.StateProvider]{
			method: "ResolveFindingIfCurrent",
			setup: setupFindings(func() *storagev1.Finding {
				pr := 7
				finding := workflowFinding("fp-pr", "medium")
				finding.State, finding.PRNumber = "pr_open", &pr
				return finding
			}),
			args: fixed(workflowNamespace, "fp-pr", "run-1", 7),
		}),
		security(writeCase[storagev1.StateProvider]{
			method: "MarkFindingDuplicate",
			setup:  setupFindings(findingFixture("canonical", "high"), findingFixture("alias", "high")),
			args:   fixed(workflowNamespace, "alias", "canonical"),
		}),
		security(writeCase[storagev1.StateProvider]{
			method: "CreatePatchProposal",
			args:   func(storagev1.ControllerEpochFence) []any { return []any{securityWriteProposal()} },
		}),
		security(writeCase[storagev1.StateProvider]{
			method: "UpdatePatchProposal",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.CreatePatchProposal(t.Context(), securityWriteProposal()))
			},
			args: func(storagev1.ControllerEpochFence) []any {
				proposal := securityWriteProposal()
				proposal.Status, proposal.Reason = "failed", "patch did not build"
				return []any{proposal}
			},
		}),
		security(writeCase[storagev1.StateProvider]{
			method: "BindPatchProposalPublicationEvidence",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.CreatePatchProposal(t.Context(), securityWriteProposal()))
			},
			args: func(storagev1.ControllerEpochFence) []any { return []any{securityWriteBoundProposal()} },
		}),
		security(writeCase[storagev1.StateProvider]{
			method: "CreateDroppedFinding",
			args: func(storagev1.ControllerEpochFence) []any {
				return []any{&storagev1.DroppedFinding{ID: "drop-1", Namespace: workflowNamespace, RepositoryScan: workflowScan,
					ScanRunID: "run-1", TaskName: "scan-task", Reason: "missing evidence", Layer: "schema", CreatedAt: workflowAt(0)}}
			},
		}),
	)
}
