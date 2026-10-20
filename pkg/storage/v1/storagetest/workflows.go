package storagetest

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

const (
	workflowNamespace      = "tenant-workflows"
	workflowOtherNamespace = "other-workflows"
	workflowScan           = "repository-scan"
	workflowMonitor        = "monitor"
)

var workflowBase = time.Date(2026, time.January, 2, 3, 4, 5, 987654321, time.UTC)

func workflowAt(minutes int) time.Time { return workflowBase.Add(time.Duration(minutes) * time.Minute) }

func requireWorkflowTime(t *testing.T, want, got time.Time) {
	t.Helper()
	requireSameInstant(t, want, got, "workflow time")
}

func workflowScanRun(id, phase string) *storagev1.ScanRun {
	return &storagev1.ScanRun{ID: id, Namespace: workflowNamespace, RepositoryScan: workflowScan, TaskName: "scan-" + id,
		Mode: "full", Phase: phase, StartedAt: workflowBase, RepositoryScanUID: "scan-uid", RepositoryScanGeneration: 1}
}

func workflowFinding(fingerprint, severity string) *storagev1.Finding {
	return &storagev1.Finding{Namespace: workflowNamespace, RepositoryScan: workflowScan, ScanRunID: "run-1",
		Fingerprint: fingerprint, Title: "Finding " + fingerprint, Summary: "summary", Severity: severity, Confidence: "high",
		ValidationStatus: "unvalidated", State: "open", FilePath: "api/handler.go", Line: 12,
		Evidence: []storagev1.FindingEvidenceRef{{Kind: "code", Path: "api/handler.go", StartLine: 10, EndLine: 14}}}
}

func workflowFindingIDs(findings []storagev1.Finding) []string {
	ids := make([]string, 0, len(findings))
	for _, finding := range findings {
		ids = append(ids, finding.ID)
	}
	return ids
}

func runSecurity(t *testing.T, newProvider StateFactory) {
	t.Run("an invalid cursor is a validation error", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		_, _, err := p.ListScanRuns(ctx, workflowNamespace, workflowScan, 2, "not-a-cursor")
		require.ErrorIs(t, err, storagev1.ErrValidation)
		_, _, err = p.ListFindings(ctx, storagev1.FindingFilter{Namespace: workflowNamespace, Limit: 2, Cursor: "not-a-cursor"})
		require.ErrorIs(t, err, storagev1.ErrValidation)
	})

	t.Run("scan run cancellation handshake", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		run := workflowScanRun("run-1", "pending")
		require.NoError(t, p.CreateScanRun(ctx, run))
		require.ErrorIs(t, p.CreateScanRun(ctx, workflowScanRun("run-2", "pending")), storagev1.ErrConflict)
		_, err := p.GetScanRun(ctx, workflowOtherNamespace, run.ID)
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		run.Phase, run.SliceCount = "running", 3
		require.NoError(t, p.UpdateScanRun(ctx, run))
		stale := *run
		stale.RepositoryScanUID = "replaced-scan-uid"
		require.ErrorIs(t, p.UpdateScanRun(ctx, &stale), storagev1.ErrConflict)
		require.ErrorIs(t, p.UpdateScanRun(ctx, workflowScanRun("missing", "running")), storagev1.ErrNotFound)
		active, err := p.ListActiveScanRuns(ctx, workflowNamespace, workflowScan)
		require.NoError(t, err)
		require.Len(t, active, 1)
		require.Equal(t, 3, active[0].SliceCount)

		require.NoError(t, p.RequestScanRunCancellation(ctx, run, "operator cancelled"))
		require.Equal(t, int64(1), run.CancellationVersion)
		require.True(t, run.CancellationPending)
		run.SliceCount = 4
		require.ErrorIs(t, p.UpdateScanRun(ctx, run), storagev1.ErrConflict, "cancellation freezes ordinary progress")
		require.NoError(t, p.RequestScanRunCancellation(ctx, run, "retry"))
		require.Equal(t, int64(2), run.CancellationVersion)
		pending, err := p.ListScanRunsPendingCancellation(ctx, workflowNamespace, workflowScan)
		require.NoError(t, err)
		require.Len(t, pending, 1)

		stale = *run
		stale.CancellationVersion = 1
		require.ErrorIs(t, p.CompleteScanRunCancellation(ctx, &stale), storagev1.ErrConflict)
		require.NoError(t, p.CompleteScanRunCancellation(ctx, run))
		require.False(t, run.CancellationPending)
		got, err := p.GetScanRun(ctx, workflowNamespace, run.ID)
		require.NoError(t, err)
		require.Equal(t, "failed", got.Phase)
		require.NotNil(t, got.CompletedAt)
		require.Equal(t, "operator cancelled", got.ErrorMessage, "the first cancellation reason is retained")
		require.Equal(t, 3, got.SliceCount)
		require.False(t, got.CancellationPending)

		active, err = p.ListActiveScanRuns(ctx, workflowNamespace, workflowScan)
		require.NoError(t, err)
		require.Empty(t, active)
		pending, err = p.ListScanRunsPendingCancellation(ctx, workflowNamespace, workflowScan)
		require.NoError(t, err)
		require.Empty(t, pending)
		require.NoError(t, p.CreateScanRun(ctx, workflowScanRun("run-2", "pending")))
		require.ErrorIs(t, p.RequestScanRunCancellation(ctx, workflowScanRun("missing", "running"), "gone"), storagev1.ErrNotFound)
	})

	t.Run("scan runs list in admission order", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		for _, run := range []*storagev1.ScanRun{
			workflowScanRun("run-a", "succeeded"), workflowScanRun("run-b", "failed"), workflowScanRun("run-c", "pending"),
		} {
			require.NoError(t, p.CreateScanRun(ctx, run))
		}
		first, cursor, err := p.ListScanRuns(ctx, workflowNamespace, workflowScan, 2, "")
		require.NoError(t, err)
		require.Equal(t, []string{"run-c", "run-b"}, []string{first[0].ID, first[1].ID})
		require.NotEmpty(t, cursor)
		rest, cursor, err := p.ListScanRuns(ctx, workflowNamespace, workflowScan, 2, cursor)
		require.NoError(t, err)
		require.Len(t, rest, 1)
		require.Equal(t, "run-a", rest[0].ID)
		require.Empty(t, cursor)

		old := workflowScanRun("run-a", "running")
		require.ErrorIs(t, p.UpdateScanRun(ctx, old), storagev1.ErrConflict, "an older run cannot be reactivated")
		other, _, err := p.ListScanRuns(ctx, workflowOtherNamespace, workflowScan, 10, "")
		require.NoError(t, err)
		require.Empty(t, other)
	})

	t.Run("review slices", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		slice := &storagev1.ReviewSlice{ID: "slice-1", Namespace: workflowNamespace, RepositoryScan: workflowScan,
			Source: "deterministic", Title: "API handlers", Summary: "request routing", LastScanRunID: "run-1",
			OwnedFiles:        []storagev1.ReviewSliceFile{{Path: "api/handler.go", Reason: "entrypoint"}},
			Tests:             []storagev1.ReviewSliceTest{{Path: "api/handler_test.go", Command: "go test ./api"}},
			ChangedLineRanges: []storagev1.ChangedLineRange{{Path: "api/handler.go", StartLine: 10, EndLine: 20}},
			Tags:              []string{"auth"}}
		require.NoError(t, p.UpsertReviewSlice(ctx, slice))
		got, err := p.GetReviewSlice(ctx, workflowNamespace, workflowScan, "slice-1")
		require.NoError(t, err)
		require.Equal(t, "pending", got.Status)
		require.Equal(t, slice.OwnedFiles, got.OwnedFiles)
		require.Equal(t, slice.Tests, got.Tests)
		require.Equal(t, slice.ChangedLineRanges, got.ChangedLineRanges)
		require.Equal(t, []string{"auth"}, got.Tags)
		_, err = p.GetReviewSlice(ctx, workflowOtherNamespace, workflowScan, "slice-1")
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		require.ErrorIs(t, p.UpdateReviewSliceStatus(ctx, workflowNamespace, workflowScan, "slice-1", "run-other", "reviewed"),
			storagev1.ErrNotFound, "status updates are fenced by the slice's scan run")
		require.NoError(t, p.UpdateReviewSliceStatus(ctx, workflowNamespace, workflowScan, "slice-1", "run-1", "reviewed"))
		reviewed, err := p.GetReviewSlice(ctx, workflowNamespace, workflowScan, "slice-1")
		require.NoError(t, err)
		require.Equal(t, "reviewed", reviewed.Status)
		require.NotNil(t, reviewed.LastReviewedAt)

		slice.Title, slice.Status, slice.LastReviewedAt = "API handlers v2", "reviewed", nil
		require.NoError(t, p.UpsertReviewSlice(ctx, slice))
		updated, err := p.GetReviewSlice(ctx, workflowNamespace, workflowScan, "slice-1")
		require.NoError(t, err)
		require.Equal(t, "API handlers v2", updated.Title)
		require.NotNil(t, updated.LastReviewedAt, "re-upsert keeps the recorded review time")

		require.NoError(t, p.UpsertReviewSlice(ctx, &storagev1.ReviewSlice{ID: "slice-2", Namespace: workflowNamespace,
			RepositoryScan: workflowScan, Source: "deterministic", Title: "CLI", LastScanRunID: "run-1"}))
		pendingSlices, _, err := p.ListReviewSlices(ctx, storagev1.ReviewSliceFilter{Namespace: workflowNamespace,
			RepositoryScan: workflowScan, Status: "pending"})
		require.NoError(t, err)
		require.Len(t, pendingSlices, 1)
		require.Equal(t, "slice-2", pendingSlices[0].ID)
		page, cursor, err := p.ListReviewSlices(ctx, storagev1.ReviewSliceFilter{Namespace: workflowNamespace,
			RepositoryScan: workflowScan, Limit: 1})
		require.NoError(t, err)
		require.Len(t, page, 1)
		next, cursor, err := p.ListReviewSlices(ctx, storagev1.ReviewSliceFilter{Namespace: workflowNamespace,
			RepositoryScan: workflowScan, Limit: 1, Cursor: cursor})
		require.NoError(t, err)
		require.Len(t, next, 1)
		require.NotEqual(t, page[0].ID, next[0].ID)
		last, _, err := p.ListReviewSlices(ctx, storagev1.ReviewSliceFilter{Namespace: workflowNamespace,
			RepositoryScan: workflowScan, Limit: 1, Cursor: cursor})
		require.NoError(t, err)
		require.Empty(t, last)
	})

	t.Run("threat model versions", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		_, err := p.GetLatestThreatModel(ctx, workflowNamespace, workflowScan)
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		first := &storagev1.ThreatModel{Namespace: workflowNamespace, RepositoryScan: workflowScan,
			Content: "Trust boundary: public API", Source: "generated", GeneratedByScan: "run-1"}
		require.NoError(t, p.SaveThreatModel(ctx, first))
		require.Equal(t, int64(1), first.Version)
		second := &storagev1.ThreatModel{Namespace: workflowNamespace, RepositoryScan: workflowScan,
			Content: "Trust boundary: operator edits", Source: "user"}
		require.NoError(t, p.SaveThreatModelIfVersion(ctx, second, 1))
		require.Equal(t, int64(2), second.Version)
		stale := &storagev1.ThreatModel{Namespace: workflowNamespace, RepositoryScan: workflowScan,
			Content: "Trust boundary: stale edit", Source: "user"}
		require.ErrorIs(t, p.SaveThreatModelIfVersion(ctx, stale, 1), storagev1.ErrConflict)
		latest, err := p.GetLatestThreatModel(ctx, workflowNamespace, workflowScan)
		require.NoError(t, err)
		require.Equal(t, int64(2), latest.Version)
		require.Equal(t, "Trust boundary: operator edits", latest.Content)
		require.NoError(t, p.SaveThreatModelIfVersion(ctx, &storagev1.ThreatModel{Namespace: workflowOtherNamespace,
			RepositoryScan: workflowScan, Content: "Other team", Source: "user"}, 0))
		latest, err = p.GetLatestThreatModel(ctx, workflowNamespace, workflowScan)
		require.NoError(t, err)
		require.Equal(t, int64(2), latest.Version)
	})

	t.Run("findings", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		high := workflowFinding("fp-high", "high")
		require.NoError(t, p.UpsertFinding(ctx, high))
		require.Equal(t, "fp-high", high.ID, "the fingerprint identifies a finding without an ID")
		high.Title = "Renamed finding"
		require.NoError(t, p.UpsertFinding(ctx, high))
		got, err := p.GetFinding(ctx, workflowNamespace, "fp-high")
		require.NoError(t, err)
		require.Equal(t, "Renamed finding", got.Title)
		require.Equal(t, high.Evidence, got.Evidence)
		_, err = p.GetFinding(ctx, workflowOtherNamespace, "fp-high")
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		require.NoError(t, p.UpsertFinding(ctx, workflowFinding("fp-critical", "critical")))
		require.NoError(t, p.UpsertFinding(ctx, workflowFinding("fp-low", "low")))
		counts, err := p.GetFindingCounts(ctx, workflowNamespace, workflowScan)
		require.NoError(t, err)
		require.Equal(t, storagev1.FindingCounts{Total: 3, Critical: 1, High: 1, Low: 1}, counts)
		critical, _, err := p.ListFindings(ctx, storagev1.FindingFilter{Namespace: workflowNamespace, Severity: "critical"})
		require.NoError(t, err)
		require.Equal(t, []string{"fp-critical"}, workflowFindingIDs(critical))
		page, cursor, err := p.ListFindings(ctx, storagev1.FindingFilter{Namespace: workflowNamespace, Limit: 2})
		require.NoError(t, err)
		require.Len(t, page, 2)
		rest, cursor, err := p.ListFindings(ctx, storagev1.FindingFilter{Namespace: workflowNamespace, Limit: 2, Cursor: cursor})
		require.NoError(t, err)
		require.Len(t, rest, 1)
		require.Empty(t, cursor)

		require.NoError(t, p.UpdateFindingState(ctx, workflowNamespace, "fp-low", "dismissed"))
		require.ErrorIs(t, p.UpdateFindingState(ctx, workflowNamespace, "missing", "dismissed"), storagev1.ErrNotFound)
		require.NoError(t, p.UpsertFinding(ctx, workflowFinding("fp-low", "low")))
		got, err = p.GetFinding(ctx, workflowNamespace, "fp-low")
		require.NoError(t, err)
		require.Equal(t, "dismissed", got.State, "a later scan does not override a user decision")
		counts, err = p.GetFindingCounts(ctx, workflowNamespace, workflowScan)
		require.NoError(t, err)
		require.Equal(t, int32(2), counts.Total)

		pr := 7
		opened := workflowFinding("fp-pr", "medium")
		opened.State, opened.PRNumber = "pr_open", &pr
		require.NoError(t, p.UpsertFinding(ctx, opened))
		for _, attempt := range []struct {
			run string
			pr  int
		}{{"run-1", 8}, {"run-2", 7}} {
			resolved, err := p.ResolveFindingIfCurrent(ctx, workflowNamespace, "fp-pr", attempt.run, attempt.pr)
			require.NoError(t, err)
			require.False(t, resolved)
		}
		resolved, err := p.ResolveFindingIfCurrent(ctx, workflowNamespace, "fp-pr", "run-1", 7)
		require.NoError(t, err)
		require.True(t, resolved)
		resolved, err = p.ResolveFindingIfCurrent(ctx, workflowNamespace, "fp-pr", "run-1", 7)
		require.NoError(t, err)
		require.False(t, resolved, "resolution applies only to the open PR occurrence")

		reopened := workflowFinding("fp-pr", "medium")
		require.NoError(t, p.UpsertFinding(ctx, reopened))
		got, err = p.GetFinding(ctx, workflowNamespace, "fp-pr")
		require.NoError(t, err)
		require.Equal(t, "resolved", got.State, "an ordinary upsert keeps a resolved finding resolved")
		require.NoError(t, p.UpsertObservedFinding(ctx, reopened))
		got, err = p.GetFinding(ctx, workflowNamespace, "fp-pr")
		require.NoError(t, err)
		require.Equal(t, "open", got.State, "a current observation reopens a remediated finding")
	})

	t.Run("duplicates and patch proposals", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		require.NoError(t, p.UpsertFinding(ctx, workflowFinding("canonical", "high")))
		require.NoError(t, p.UpsertFinding(ctx, workflowFinding("alias", "high")))
		older := &storagev1.PatchProposal{ID: "patch-1", Namespace: workflowNamespace, RepositoryScan: workflowScan,
			FindingID: "alias", TaskName: "patch-task-1", Branch: "orka/patch-1", Status: "pending", CreatedAt: workflowAt(0)}
		newer := &storagev1.PatchProposal{ID: "patch-2", Namespace: workflowNamespace, RepositoryScan: workflowScan,
			FindingID: "canonical", TaskName: "patch-task-2", Branch: "orka/patch-2", Status: "pending", CreatedAt: workflowAt(1)}
		require.NoError(t, p.CreatePatchProposal(ctx, older))
		require.NoError(t, p.CreatePatchProposal(ctx, newer))

		require.NoError(t, p.MarkFindingDuplicate(ctx, workflowNamespace, "alias", "canonical"))
		require.ErrorIs(t, p.MarkFindingDuplicate(ctx, workflowNamespace, "canonical", "canonical"), storagev1.ErrValidation)
		require.ErrorIs(t, p.MarkFindingDuplicate(ctx, workflowNamespace, "missing", "canonical"), storagev1.ErrNotFound)
		require.ErrorIs(t, p.MarkFindingDuplicate(ctx, workflowNamespace, "canonical", "alias"), storagev1.ErrValidation,
			"an alias cannot become canonical")
		canonical, _, err := p.ListFindings(ctx, storagev1.FindingFilter{Namespace: workflowNamespace})
		require.NoError(t, err)
		require.Equal(t, []string{"canonical"}, workflowFindingIDs(canonical))
		all, _, err := p.ListFindings(ctx, storagev1.FindingFilter{Namespace: workflowNamespace, IncludeDuplicates: true})
		require.NoError(t, err)
		require.Len(t, all, 2)
		alias, err := p.GetFinding(ctx, workflowNamespace, "alias")
		require.NoError(t, err)
		require.Equal(t, "canonical", alias.DuplicateOf)
		counts, err := p.GetFindingCounts(ctx, workflowNamespace, workflowScan)
		require.NoError(t, err)
		require.Equal(t, int32(1), counts.Total)

		for _, findingID := range []string{"alias", "canonical"} {
			proposals, err := p.ListPatchProposals(ctx, workflowNamespace, findingID)
			require.NoError(t, err)
			require.Len(t, proposals, 2)
			require.Equal(t, "patch-2", proposals[0].ID, "proposals are newest first")
		}
		require.NoError(t, p.UpdateFindingState(ctx, workflowNamespace, "alias", "dismissed"))
		got, err := p.GetFinding(ctx, workflowNamespace, "canonical")
		require.NoError(t, err)
		require.Equal(t, "dismissed", got.State, "state changes through an alias apply to the canonical finding")

		older.Status, older.Reason = "failed", "patch did not build"
		require.NoError(t, p.UpdatePatchProposal(ctx, older))
		proposals, err := p.ListPatchProposals(ctx, workflowNamespace, "canonical")
		require.NoError(t, err)
		require.Equal(t, "failed", proposals[1].Status)
		require.Equal(t, "patch did not build", proposals[1].Reason)
		require.ErrorIs(t, p.UpdatePatchProposal(ctx, &storagev1.PatchProposal{ID: "missing", Namespace: workflowNamespace}),
			storagev1.ErrNotFound)
		require.ErrorIs(t, p.UpdatePatchProposal(ctx, &storagev1.PatchProposal{ID: "patch-1"}), storagev1.ErrValidation)
	})

	t.Run("patch publication evidence is immutable", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		head, baseline := strings.Repeat("a", 40), strings.Repeat("d", 40)
		evidence := &storagev1.PatchPublicationEvidence{PublicationID: "publication-1",
			ArtifactDigest: "sha256:" + strings.Repeat("b", 64), SourceRepositoryID: "repo-source", SourceRef: baseline,
			SourceBaselineSHA: baseline, TargetRepositoryID: "repo-fork", TargetRef: "refs/heads/orka/fix-1",
			ExpectedCommitSHA: head, VerifiedRemoteSHA: head,
			PRIntent: storagev1.PullRequestIntent{BaseRepositoryID: "repo-source", BaseRef: "refs/heads/main",
				HeadRepositoryID: "repo-fork", HeadRef: "refs/heads/orka/fix-1", PublicationGeneration: 1, ExpectedHeadSHA: head},
			PRReceipt: storagev1.PatchPullRequestEvidence{IntentKey: "sha256:" + strings.Repeat("c", 64), ForgeID: "PR_1",
				Number: 42, URL: "https://forge.example/pr/42", State: "Open", HeadSHA: head}}
		proposal := &storagev1.PatchProposal{ID: "patch-1", Namespace: workflowNamespace, RepositoryScan: workflowScan,
			FindingID: "finding", TaskName: "patch-task", Branch: "orka/fix-1", Status: "pending"}
		withEvidence := *proposal
		withEvidence.PublicationEvidence = evidence
		require.ErrorIs(t, p.CreatePatchProposal(ctx, &withEvidence), storagev1.ErrValidation)
		require.NoError(t, p.CreatePatchProposal(ctx, proposal))
		require.ErrorIs(t, p.CreatePatchProposal(ctx, proposal), storagev1.ErrConflict, "a reused patch proposal ID conflicts")
		require.ErrorIs(t, p.UpdatePatchProposal(ctx, &withEvidence), storagev1.ErrValidation)

		number := 42
		bound := *proposal
		bound.Status, bound.PRNumber, bound.PRURL, bound.PublicationEvidence = "pr_opened", &number, evidence.PRReceipt.URL, evidence
		require.NoError(t, p.BindPatchProposalPublicationEvidence(ctx, &bound))
		replay := bound
		require.NoError(t, p.BindPatchProposalPublicationEvidence(ctx, &replay), "an identical replay is a no-op")
		changedEvidence := *evidence
		changedEvidence.PublicationID = "publication-2"
		conflicting := bound
		conflicting.PublicationEvidence = &changedEvidence
		require.ErrorIs(t, p.BindPatchProposalPublicationEvidence(ctx, &conflicting), storagev1.ErrConflict)

		projection := bound
		projection.PublicationEvidence = nil
		require.NoError(t, p.UpdatePatchProposal(ctx, &projection), "an unchanged projection update is a no-op")
		projection.Status = "failed"
		require.ErrorIs(t, p.UpdatePatchProposal(ctx, &projection), storagev1.ErrConflict)
		proposals, err := p.ListPatchProposals(ctx, workflowNamespace, "finding")
		require.NoError(t, err)
		require.Len(t, proposals, 1)
		require.Equal(t, "pr_opened", proposals[0].Status)
		require.Equal(t, evidence, proposals[0].PublicationEvidence)
	})

	t.Run("dropped findings", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		first := &storagev1.DroppedFinding{ID: "drop-1", Namespace: workflowNamespace, RepositoryScan: workflowScan,
			ScanRunID: "run-1", TaskName: "scan-task", Reason: "missing evidence", Layer: "schema", CreatedAt: workflowAt(0)}
		require.NoError(t, p.CreateDroppedFinding(ctx, first))
		replay := *first
		replay.Reason = "replayed reason"
		require.NoError(t, p.CreateDroppedFinding(ctx, &replay), "an existing diagnostic ID is kept")
		second := &storagev1.DroppedFinding{Namespace: workflowNamespace, RepositoryScan: workflowScan, ScanRunID: "run-1",
			TaskName: "scan-task", Reason: "duplicate finding", Layer: "dedupe", CreatedAt: workflowAt(1)}
		require.NoError(t, p.CreateDroppedFinding(ctx, second))
		require.NotEmpty(t, second.ID)

		all, _, err := p.ListDroppedFindings(ctx, storagev1.DroppedFindingFilter{Namespace: workflowNamespace})
		require.NoError(t, err)
		require.Len(t, all, 2)
		require.Equal(t, second.ID, all[0].ID)
		require.Equal(t, "missing evidence", all[1].Reason)
		matching, _, err := p.ListDroppedFindings(ctx, storagev1.DroppedFindingFilter{Namespace: workflowNamespace,
			ReasonContains: "evidence"})
		require.NoError(t, err)
		require.Len(t, matching, 1)
		layered, _, err := p.ListDroppedFindings(ctx, storagev1.DroppedFindingFilter{Namespace: workflowNamespace, Layer: "dedupe"})
		require.NoError(t, err)
		require.Len(t, layered, 1)
		page, cursor, err := p.ListDroppedFindings(ctx, storagev1.DroppedFindingFilter{Namespace: workflowNamespace, Limit: 1})
		require.NoError(t, err)
		require.Len(t, page, 1)
		require.NotEmpty(t, cursor)
		other, _, err := p.ListDroppedFindings(ctx, storagev1.DroppedFindingFilter{Namespace: workflowOtherNamespace})
		require.NoError(t, err)
		require.Empty(t, other)
	})

	t.Run("ingestion receipts require an applied ingestion", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		identity := storagev1.ScanTaskIdentity{Namespace: workflowNamespace, RepositoryScan: workflowScan, ScanRunID: "run-1",
			TaskName: "scan-task", TaskUID: "scan-task-uid", Stage: "review", SliceID: "slice-1"}
		_, err := p.GetScanTaskIngestion(ctx, identity)
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		require.ErrorIs(t, p.CompleteScanTaskIngestion(ctx, identity), storagev1.ErrNotFound)
	})
}

func runRepositoryMonitors(t *testing.T, newProvider StateFactory) {
	t.Run("an invalid cursor is a validation error", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		_, _, err := p.ListMonitorRuns(ctx, storagev1.MonitorRunFilter{Namespace: workflowNamespace, Cursor: "not-a-cursor"})
		require.ErrorIs(t, err, storagev1.ErrValidation)
		_, _, err = p.ListMonitorEvents(ctx, storagev1.MonitorEventFilter{Namespace: workflowNamespace, Cursor: "-1"})
		require.ErrorIs(t, err, storagev1.ErrValidation)
	})

	t.Run("a reused record ID conflicts", func(t *testing.T) {
		p := newProvider(t)
		ctx := t.Context()
		creates := map[string]func() error{
			"action record": func() error {
				return p.CreateActionRecord(ctx, &storagev1.ActionRecord{ID: "reused", MonitorNamespace: workflowNamespace,
					MonitorName: workflowMonitor, Kind: "pull_request", ActionKind: "comment"})
			},
			"review record": func() error {
				return p.CreateReviewRecord(ctx, &storagev1.ReviewRecord{ID: "reused", MonitorNamespace: workflowNamespace,
					MonitorName: workflowMonitor, Kind: "pull_request", Number: 1})
			},
			"repair job": func() error {
				return p.CreateRepairJob(ctx, &storagev1.RepairJob{ID: "reused", MonitorNamespace: workflowNamespace,
					MonitorName: workflowMonitor})
			},
			"monitor event": func() error {
				return p.CreateMonitorEvent(ctx, &storagev1.MonitorEvent{ID: "reused", MonitorNamespace: workflowNamespace,
					MonitorName: workflowMonitor, EventType: "observed"})
			},
			"implementation job": func() error {
				return p.CreateImplementationJob(ctx, &storagev1.ImplementationJob{ID: "reused", MonitorNamespace: workflowNamespace,
					MonitorName: workflowMonitor})
			},
			"GitHub mutation record": func() error {
				return p.CreateGitHubMutationRecord(ctx, &storagev1.GitHubMutationRecord{ID: "reused",
					MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor, Operation: "comment"})
			},
		}
		for name, create := range creates {
			require.NoError(t, create(), name)
			require.ErrorIs(t, create(), storagev1.ErrConflict, name)
		}
	})

	monitorRecord := func(uid string, generation int64) *storagev1.RepositoryMonitorRecord {
		return &storagev1.RepositoryMonitorRecord{Namespace: workflowNamespace, Name: workflowMonitor, UID: uid,
			RepoURL: "https://github.com/org/repo", Owner: "org", Repository: "repo", Branch: "main", Generation: generation}
	}
	monitorRun := func(id, phase string, minute int) *storagev1.MonitorRun {
		return &storagev1.MonitorRun{ID: id, MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor,
			Trigger: "scheduled", Phase: phase, StartedAt: workflowAt(minute)}
	}

	t.Run("monitor identity owns dependent state", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		require.ErrorIs(t, p.UpsertRepositoryMonitor(ctx, nil), storagev1.ErrValidation)
		require.ErrorIs(t, p.UpsertRepositoryMonitor(ctx, &storagev1.RepositoryMonitorRecord{Namespace: workflowNamespace}),
			storagev1.ErrValidation)
		first := monitorRecord("uid-1", 1)
		require.NoError(t, p.UpsertRepositoryMonitor(ctx, first))
		require.NoError(t, p.UpsertRepositoryMonitor(ctx, monitorRecord("uid-1", 2)))
		got, err := p.GetRepositoryMonitor(ctx, workflowNamespace, workflowMonitor)
		require.NoError(t, err)
		require.Equal(t, int64(2), got.Generation)
		requireWorkflowTime(t, first.CreatedAt, got.CreatedAt)
		_, err = p.GetRepositoryMonitor(ctx, workflowOtherNamespace, workflowMonitor)
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		require.NoError(t, p.CreateMonitorRun(ctx, monitorRun("run-old", "succeeded", 0)))
		require.NoError(t, p.UpsertRepositoryMonitor(ctx, monitorRecord("uid-2", 1)))
		_, err = p.GetMonitorRun(ctx, workflowNamespace, "run-old")
		require.ErrorIs(t, err, storagev1.ErrNotFound, "a recreated monitor does not inherit stale history")

		require.NoError(t, p.CreateMonitorRun(ctx, monitorRun("run-new", "succeeded", 1)))
		for _, record := range []*storagev1.GitHubMutationRecord{
			{ID: "readiness", Operation: "readiness_status", ExternalID: "status-1"},
			{ID: "comment", Operation: "comment"},
		} {
			record.MonitorNamespace, record.MonitorName = workflowNamespace, workflowMonitor
			require.NoError(t, p.CreateGitHubMutationRecord(ctx, record))
		}
		require.NoError(t, p.DeleteRepositoryMonitor(ctx, workflowNamespace, workflowMonitor))
		_, err = p.GetRepositoryMonitor(ctx, workflowNamespace, workflowMonitor)
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		_, err = p.GetMonitorRun(ctx, workflowNamespace, "run-new")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		retained, _, err := p.ListGitHubMutationRecords(ctx, storagev1.GitHubMutationRecordFilter{Namespace: workflowNamespace})
		require.NoError(t, err)
		require.Len(t, retained, 1, "recorded readiness statuses outlive their monitor")
		require.Equal(t, "readiness", retained[0].ID)
		require.ErrorIs(t, p.DeleteRepositoryMonitor(ctx, workflowNamespace, "missing"), storagev1.ErrNotFound)
	})

	t.Run("monitor runs and items", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		require.ErrorIs(t, p.CreateMonitorRun(ctx, &storagev1.MonitorRun{MonitorNamespace: workflowNamespace}), storagev1.ErrValidation)
		first := monitorRun("run-1", "running", 0)
		require.NoError(t, p.CreateMonitorRun(ctx, first))
		require.ErrorIs(t, p.CreateMonitorRun(ctx, monitorRun("run-2", "running", 1)), storagev1.ErrConflict,
			"one monitor has at most one running run")
		completed := workflowAt(5)
		first.Phase, first.CompletedAt, first.CreatedTaskCount = "succeeded", &completed, 2
		require.NoError(t, p.UpdateMonitorRun(ctx, first))
		require.NoError(t, p.CreateMonitorRun(ctx, monitorRun("run-2", "running", 1)))
		got, err := p.GetMonitorRun(ctx, workflowNamespace, "run-1")
		require.NoError(t, err)
		require.Equal(t, 2, got.CreatedTaskCount)
		require.NotNil(t, got.CompletedAt)
		requireWorkflowTime(t, completed, *got.CompletedAt)
		newest, _, err := p.ListMonitorRuns(ctx, storagev1.MonitorRunFilter{Namespace: workflowNamespace, MonitorName: workflowMonitor})
		require.NoError(t, err)
		require.Equal(t, []string{"run-2", "run-1"}, []string{newest[0].ID, newest[1].ID})
		oldest, _, err := p.ListMonitorRuns(ctx, storagev1.MonitorRunFilter{Namespace: workflowNamespace, OldestFirst: true})
		require.NoError(t, err)
		require.Equal(t, "run-1", oldest[0].ID)
		running, _, err := p.ListMonitorRuns(ctx, storagev1.MonitorRunFilter{Namespace: workflowNamespace, Phase: "running"})
		require.NoError(t, err)
		require.Len(t, running, 1)
		_, err = p.GetMonitorRun(ctx, workflowOtherNamespace, "run-1")
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		require.ErrorIs(t, p.UpsertMonitorItem(ctx, &storagev1.MonitorItem{MonitorNamespace: workflowNamespace,
			MonitorName: workflowMonitor, Number: 1}), storagev1.ErrValidation)
		pr := &storagev1.MonitorItem{MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor, Kind: "pull_request",
			Number: 12, Title: "Fix bug", State: "open", HeadSHA: "head-1"}
		require.NoError(t, p.UpsertMonitorItem(ctx, pr))
		require.Equal(t, "12", pr.ItemKey, "a numbered item is keyed by its number")
		pr.LastVerdict, pr.HeadSHA = "approve", "head-2"
		require.NoError(t, p.UpsertMonitorItem(ctx, pr))
		commit := &storagev1.MonitorItem{MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor, Kind: "commit",
			SHA: "abc123", Title: "Commit"}
		require.NoError(t, p.UpsertMonitorItem(ctx, commit))
		require.Equal(t, "abc123", commit.ItemKey, "a commit is keyed by its SHA")
		gotItem, err := p.GetMonitorItem(ctx, workflowNamespace, workflowMonitor, "pull_request", "12")
		require.NoError(t, err)
		require.Equal(t, "head-2", gotItem.HeadSHA)
		require.Equal(t, "approve", gotItem.LastVerdict)
		approved, _, err := p.ListMonitorItems(ctx, storagev1.MonitorItemFilter{Namespace: workflowNamespace, ReviewVerdict: "approve"})
		require.NoError(t, err)
		require.Len(t, approved, 1)
		commits, _, err := p.ListMonitorItems(ctx, storagev1.MonitorItemFilter{Namespace: workflowNamespace, Kind: "commit"})
		require.NoError(t, err)
		require.Len(t, commits, 1)
		_, err = p.GetMonitorItem(ctx, workflowNamespace, workflowMonitor, "issue", "12")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
	})

	t.Run("work actions and implementation jobs", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		action := func(id, status, dedupe string, number int64) *storagev1.WorkAction {
			return &storagev1.WorkAction{ID: id, MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor,
				TargetKind: "pull_request", TargetNumber: number, DesiredAction: "review", DedupeKey: dedupe, Status: status}
		}
		require.ErrorIs(t, p.CreateWorkAction(ctx, &storagev1.WorkAction{ID: "no-status"}), storagev1.ErrValidation)
		for _, created := range []*storagev1.WorkAction{
			action("queued", "queued", "review-12", 12), action("control", "running", "", 12),
			action("finished", "succeeded", "", 12), action("other-target", "queued", "", 13),
		} {
			require.NoError(t, p.CreateWorkAction(ctx, created))
		}
		require.ErrorIs(t, p.CreateWorkAction(ctx, action("duplicate", "queued", "review-12", 12)),
			storagev1.ErrConflict, "an active dedupe key admits one action")
		cancelled, err := p.CancelWorkActions(ctx, workflowNamespace, workflowMonitor, "pull_request", 12, "superseded", "control")
		require.NoError(t, err)
		require.Equal(t, 1, cancelled, "terminal actions and the excepted action are kept")
		got, err := p.GetWorkAction(ctx, workflowNamespace, "queued")
		require.NoError(t, err)
		require.Equal(t, "cancelled", got.Status)
		require.Equal(t, "superseded", got.BlockedReason)
		require.NotNil(t, got.CompletedAt)
		require.NoError(t, p.CreateWorkAction(ctx, action("duplicate", "queued", "review-12", 12)),
			"a cancelled action releases its dedupe key")
		deduped, _, err := p.ListWorkActions(ctx, storagev1.WorkActionFilter{Namespace: workflowNamespace, DedupeKey: "review-12",
			Status: "queued"})
		require.NoError(t, err)
		require.Len(t, deduped, 1)
		require.Equal(t, "duplicate", deduped[0].ID)

		leaseUntil := workflowAt(10)
		leased := action("other-target", "leased", "", 13)
		leased.LeaseOwner, leased.LeaseExpiresAt, leased.Attempt = "worker-1", &leaseUntil, 1
		require.NoError(t, p.UpdateWorkAction(ctx, leased))
		got, err = p.GetWorkAction(ctx, workflowNamespace, "other-target")
		require.NoError(t, err)
		require.Equal(t, "worker-1", got.LeaseOwner)
		require.NotNil(t, got.LeaseExpiresAt)
		requireWorkflowTime(t, leaseUntil, *got.LeaseExpiresAt)
		require.ErrorIs(t, p.UpdateWorkAction(ctx, action("missing", "queued", "", 1)), storagev1.ErrNotFound)

		job := func(id, phase, workAction string) *storagev1.ImplementationJob {
			return &storagev1.ImplementationJob{ID: id, MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor,
				Repo: "org/repo", IssueNumber: 5, Phase: phase, WorkActionID: workAction}
		}
		require.ErrorIs(t, p.CreateImplementationJob(ctx, &storagev1.ImplementationJob{ID: "job"}), storagev1.ErrValidation)
		require.NoError(t, p.CreateImplementationJob(ctx, job("job-active", "coding", "control")))
		require.NoError(t, p.CreateImplementationJob(ctx, job("job-done", "validating", "finished")))
		require.NoError(t, p.CreateImplementationJob(ctx, job("job-unlinked", "coding", "")))
		for _, tc := range []struct {
			filter storagev1.ImplementationJobFilter
			want   int
		}{
			{storagev1.ImplementationJobFilter{MonitorName: workflowMonitor}, 3},
			{storagev1.ImplementationJobFilter{Phases: []string{"coding"}}, 2},
			{storagev1.ImplementationJobFilter{ExcludeWorkActionStatuses: []string{"succeeded", "failed", "cancelled"}}, 2},
		} {
			tc.filter.Namespace = workflowNamespace
			count, err := p.CountImplementationJobs(ctx, tc.filter)
			require.NoError(t, err)
			require.Equal(t, tc.want, count, "%+v", tc.filter)
		}
		active := job("job-active", "pr_opened", "control")
		active.PRNumber = 99
		require.NoError(t, p.UpdateImplementationJob(ctx, active))
		gotJob, err := p.GetImplementationJob(ctx, workflowNamespace, "job-active")
		require.NoError(t, err)
		require.Equal(t, int64(99), gotJob.PRNumber)
		opened, _, err := p.ListImplementationJobs(ctx, storagev1.ImplementationJobFilter{Namespace: workflowNamespace, Phase: "pr_opened"})
		require.NoError(t, err)
		require.Len(t, opened, 1)
		require.ErrorIs(t, p.UpdateImplementationJob(ctx, job("missing", "coding", "")), storagev1.ErrNotFound)
	})

	t.Run("audit and review records", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		require.ErrorIs(t, p.CreateActionRecord(ctx, &storagev1.ActionRecord{ID: "incomplete"}), storagev1.ErrValidation)
		record := &storagev1.ActionRecord{ID: "action-1", MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor,
			Kind: "pull_request", Number: 12, ActionKind: "triage", Summary: "raw summary", PayloadJSON: `{"secret":"x"}`}
		require.NoError(t, p.CreateActionRecord(ctx, record))
		record.Summary, record.PayloadJSON = "redacted summary", `{}`
		require.NoError(t, p.UpdateActionRecord(ctx, record))
		gotAction, err := p.GetActionRecord(ctx, workflowNamespace, "action-1")
		require.NoError(t, err)
		require.Equal(t, "redacted summary", gotAction.Summary)
		require.JSONEq(t, `{}`, gotAction.PayloadJSON)
		require.ErrorIs(t, p.UpdateActionRecord(ctx, &storagev1.ActionRecord{ID: "missing", MonitorNamespace: workflowNamespace}),
			storagev1.ErrNotFound)
		triage, _, err := p.ListActionRecords(ctx, storagev1.ActionRecordFilter{Namespace: workflowNamespace, ActionKind: "triage"})
		require.NoError(t, err)
		require.Len(t, triage, 1)

		require.NoError(t, p.CreateReviewRecord(ctx, &storagev1.ReviewRecord{ID: "review-1", MonitorNamespace: workflowNamespace,
			MonitorName: workflowMonitor, Kind: "pull_request", Number: 12, HeadSHA: "head-1", Verdict: "approve",
			FindingsJSON: `[]`}))
		gotReview, err := p.GetReviewRecord(ctx, workflowNamespace, "review-1")
		require.NoError(t, err)
		require.Equal(t, "approve", gotReview.Verdict)
		approvals, _, err := p.ListReviewRecords(ctx, storagev1.ReviewRecordFilter{Namespace: workflowNamespace, Verdict: "approve"})
		require.NoError(t, err)
		require.Len(t, approvals, 1)

		publish := &storagev1.ReviewPublishRecord{ID: "publish-1", MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor,
			ItemKind: "pull_request", ItemNumber: 12, HeadSHA: "head-1", ReviewRecordID: "review-1", Phase: "started"}
		require.NoError(t, p.CreateReviewPublishRecord(ctx, publish))
		require.ErrorIs(t, p.CreateReviewPublishRecord(ctx, &storagev1.ReviewPublishRecord{ID: "publish-2",
			MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor, ItemKind: "pull_request", ItemNumber: 12,
			HeadSHA: "head-1", Phase: "started"}), storagev1.ErrConflict, "one head has at most one active publication")
		publish.Phase, publish.GitHubReviewID, publish.UpdatedAt = "succeeded", "gh-review-1", workflowAt(1)
		require.NoError(t, p.UpdateReviewPublishRecord(ctx, publish))
		published, _, err := p.ListReviewPublishRecords(ctx, storagev1.ReviewPublishRecordFilter{Namespace: workflowNamespace,
			ReviewRecordID: "review-1"})
		require.NoError(t, err)
		require.Len(t, published, 1)
		require.Equal(t, "gh-review-1", published[0].GitHubReviewID)
		require.ErrorIs(t, p.UpdateReviewPublishRecord(ctx, &storagev1.ReviewPublishRecord{ID: "missing",
			MonitorNamespace: workflowNamespace}), storagev1.ErrNotFound)

		event := &storagev1.CommandEvent{ID: "command-1", MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor,
			Kind: "pull_request", Number: 12, DedupeKey: "comment-1", Command: "/orka review", Intent: "review", Status: "received"}
		require.NoError(t, p.CreateCommandEvent(ctx, event))
		require.ErrorIs(t, p.CreateCommandEvent(ctx, &storagev1.CommandEvent{ID: "command-2", MonitorNamespace: workflowNamespace,
			MonitorName: workflowMonitor, DedupeKey: "comment-1"}), storagev1.ErrConflict, "a dedupe key admits one command")
		processed := workflowAt(2)
		event.Status, event.ProcessedAt = "processed", &processed
		require.NoError(t, p.UpdateCommandEvent(ctx, event))
		gotEvent, err := p.GetCommandEvent(ctx, workflowNamespace, "command-1")
		require.NoError(t, err)
		require.Equal(t, "processed", gotEvent.Status)
		require.NotNil(t, gotEvent.ProcessedAt)
		processedEvents, _, err := p.ListCommandEvents(ctx, storagev1.CommandEventFilter{Namespace: workflowNamespace, Status: "processed"})
		require.NoError(t, err)
		require.Len(t, processedEvents, 1)

		repair := &storagev1.RepairJob{ID: "repair-1", MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor,
			Repo: "org/repo", PRNumber: 12, Intent: "fix-ci", Phase: "queued"}
		require.NoError(t, p.CreateRepairJob(ctx, repair))
		repair.Phase, repair.RepairCountPR = "running", 1
		require.NoError(t, p.UpdateRepairJob(ctx, repair))
		gotRepair, err := p.GetRepairJob(ctx, workflowNamespace, "repair-1")
		require.NoError(t, err)
		require.Equal(t, 1, gotRepair.RepairCountPR)
		runningRepairs, _, err := p.ListRepairJobs(ctx, storagev1.RepairJobFilter{Namespace: workflowNamespace, Phase: "running"})
		require.NoError(t, err)
		require.Len(t, runningRepairs, 1)
		_, err = p.GetRepairJob(ctx, workflowOtherNamespace, "repair-1")
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		require.ErrorIs(t, p.CreateGitHubMutationRecord(ctx, &storagev1.GitHubMutationRecord{ID: "incomplete"}), storagev1.ErrValidation)
		for _, namespace := range []string{workflowNamespace, workflowOtherNamespace} {
			require.NoError(t, p.CreateGitHubMutationRecord(ctx, &storagev1.GitHubMutationRecord{ID: "mutation-" + namespace,
				MonitorNamespace: namespace, MonitorName: workflowMonitor, Operation: "comment", Status: "pending"}))
		}
		require.NoError(t, p.UpdateGitHubMutationRecord(ctx, &storagev1.GitHubMutationRecord{ID: "mutation-" + workflowNamespace,
			MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor, Operation: "comment", Status: "succeeded"}))
		mutation, err := p.GetGitHubMutationRecord(ctx, workflowNamespace, "mutation-"+workflowNamespace)
		require.NoError(t, err)
		require.Equal(t, "succeeded", mutation.Status)
		require.ErrorIs(t, p.UpdateGitHubMutationRecord(ctx, &storagev1.GitHubMutationRecord{ID: "missing",
			MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor, Operation: "comment"}), storagev1.ErrNotFound)
		scoped, _, err := p.ListGitHubMutationRecords(ctx, storagev1.GitHubMutationRecordFilter{Namespace: workflowNamespace})
		require.NoError(t, err)
		require.Len(t, scoped, 1)
		everywhere, _, err := p.ListGitHubMutationRecords(ctx, storagev1.GitHubMutationRecordFilter{AllNamespaces: true,
			Operation: "comment"})
		require.NoError(t, err)
		require.Len(t, everywhere, 2)
	})

	t.Run("monitor events page newest first", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		for i := range 5 {
			require.NoError(t, p.CreateMonitorEvent(ctx, &storagev1.MonitorEvent{ID: "event-" + string(rune('0'+i)),
				MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor, EventType: "observed", CreatedAt: workflowAt(i)}))
		}
		require.NoError(t, p.CreateMonitorEvent(ctx, &storagev1.MonitorEvent{ID: "binding", MonitorNamespace: workflowNamespace,
			MonitorName: workflowMonitor, EventType: "validation-binding", CreatedAt: workflowAt(-1)}))
		var ids []string
		cursor := ""
		for page := 0; ; page++ {
			events, next, err := p.ListMonitorEvents(ctx, storagev1.MonitorEventFilter{Namespace: workflowNamespace,
				EventType: "observed", Limit: 2, Cursor: cursor})
			require.NoError(t, err)
			require.LessOrEqual(t, len(events), 2)
			for _, event := range events {
				ids = append(ids, event.ID)
			}
			if next == "" {
				break
			}
			require.Less(t, page, 5, "pagination terminates")
			cursor = next
		}
		require.Equal(t, []string{"event-4", "event-3", "event-2", "event-1", "event-0"}, ids)
		byID, _, err := p.ListMonitorEvents(ctx, storagev1.MonitorEventFilter{Namespace: workflowNamespace, ID: "binding"})
		require.NoError(t, err)
		require.Len(t, byID, 1)
		require.Equal(t, "validation-binding", byID[0].EventType)
		other, _, err := p.ListMonitorEvents(ctx, storagev1.MonitorEventFilter{Namespace: workflowOtherNamespace})
		require.NoError(t, err)
		require.Empty(t, other)
	})
}

func runUsage(t *testing.T, newProvider StateFactory) {
	tokens := func(value int64) *int64 { return &value }
	registerWork := func(t *testing.T, p storagev1.StateProvider, number int64, at time.Time) string {
		t.Helper()
		require.NoError(t, p.RegisterUsageWork(t.Context(), storagev1.UsageWorkRequest{Namespace: workflowNamespace,
			MonitorName: workflowMonitor, MonitorUID: "monitor-uid", Repository: "Org/Repo", Kind: "issue", Number: number,
			StartedAt: at}))
		data, err := p.LoadUsage(t.Context(), storagev1.UsageFilter{Namespaces: []string{workflowNamespace}, AsOf: time.Now().UTC()})
		require.NoError(t, err)
		for _, work := range data.Works {
			if work.Number == number {
				require.NotEmpty(t, work.ID)
				require.Equal(t, "org/repo", work.Repository, "repositories are normalized")
				return work.ID
			}
		}
		t.Fatalf("usage work %d was not loaded", number)
		return ""
	}
	observation := func(task, id, counter, scope string, input, output int64, at time.Time) storagev1.UsageObservation {
		return storagev1.UsageObservation{ID: id, Namespace: workflowNamespace, TaskUID: task, TaskName: task, CounterID: counter,
			Scope: scope, Source: "agent", Model: "model", InputTokens: tokens(input), OutputTokens: tokens(output),
			Status: "running", ObservedAt: at}
	}

	t.Run("work and task ownership", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		require.ErrorIs(t, p.RegisterUsageWork(ctx, storagev1.UsageWorkRequest{Namespace: workflowNamespace, MonitorName: workflowMonitor,
			MonitorUID: "monitor-uid", Repository: "org/repo", Kind: "commit", Number: 1}), storagev1.ErrValidation)
		work := registerWork(t, p, 1, workflowAt(0))
		require.Equal(t, work, registerWork(t, p, 1, workflowAt(30)), "registration is idempotent")

		require.ErrorIs(t, p.RegisterUsageTask(ctx, storagev1.UsageTask{Namespace: workflowNamespace, TaskName: "no-uid"}),
			storagev1.ErrValidation)
		task := storagev1.UsageTask{Namespace: workflowNamespace, TaskUID: "task-1", TaskName: "task-1", WorkID: work,
			Phase: "Running", StartedAt: workflowAt(1), PhaseObservedAt: workflowAt(1)}
		require.NoError(t, p.RegisterUsageTask(ctx, task))
		task.Phase, task.PhaseObservedAt = "Succeeded", workflowAt(10)
		require.NoError(t, p.RegisterUsageTask(ctx, task))
		renamed := task
		renamed.TaskName = "renamed"
		require.ErrorIs(t, p.RegisterUsageTask(ctx, renamed), storagev1.ErrConflict, "a Task UID keeps its identity")
		require.NoError(t, p.RegisterUsageTask(ctx, storagev1.UsageTask{Namespace: workflowNamespace, TaskUID: "child",
			TaskName: "child", ParentTaskUID: "task-1", Phase: "Running", StartedAt: workflowAt(2)}))
		require.ErrorIs(t, p.RegisterUsageTask(ctx, storagev1.UsageTask{Namespace: workflowOtherNamespace, TaskUID: "foreign",
			TaskName: "foreign", WorkID: work}), storagev1.ErrNotFound, "work belongs to one namespace")

		data, err := p.LoadUsage(ctx, storagev1.UsageFilter{Namespaces: []string{workflowNamespace}, AsOf: time.Now().UTC()})
		require.NoError(t, err)
		require.Len(t, data.Tasks, 2)
		for _, loaded := range data.Tasks {
			require.Equal(t, work, loaded.WorkID, "a delegated Task inherits its parent's work")
			if loaded.TaskUID == "task-1" {
				require.Equal(t, "Succeeded", loaded.Phase)
				require.Len(t, loaded.PhaseHistory, 2)
			} else {
				require.Equal(t, "delegated", loaded.Role)
			}
		}
	})

	t.Run("observations are idempotent counters", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		for _, invalid := range []storagev1.UsageObservation{
			{ID: "x", Namespace: workflowNamespace, Scope: "call", Source: "agent", ObservedAt: workflowAt(0)},
			observation("task", "bad-scope", "c", "lifetime", 1, 1, workflowAt(0)),
			observation("task", "negative", "c", "call", -1, 1, workflowAt(0)),
		} {
			require.ErrorIs(t, p.RecordUsage(ctx, invalid), storagev1.ErrValidation, invalid.ID)
		}
		cached := observation("task", "cache", "c", "call", 5, 1, workflowAt(0))
		cached.CachedInputTokens = tokens(6)
		require.ErrorIs(t, p.RecordUsage(ctx, cached), storagev1.ErrValidation, "cache counts are part of the input count")

		first := observation("task-a", "a-session", "session-1", "session", 100, 10, workflowAt(0))
		require.NoError(t, p.RecordUsage(ctx, first))
		require.NoError(t, p.RecordUsage(ctx, first), "a repeated observation is a no-op")
		changed := first
		changed.InputTokens = tokens(500)
		require.ErrorIs(t, p.RecordUsage(ctx, changed), storagev1.ErrConflict, "an observation ID cannot be reused")
		require.NoError(t, p.RecordUsage(ctx, observation("task-b", "b-session", "session-1", "session", 160, 16,
			workflowAt(0).Add(2*time.Second))))
		require.NoError(t, p.RecordUsage(ctx, observation("task-b", "b-call-running", "call-b", "call", 3, 0,
			workflowAt(0).Add(time.Second))))
		require.NoError(t, p.RecordUsage(ctx, observation("task-b", "b-call-final", "call-b", "call", 7, 2,
			workflowAt(0).Add(3*time.Second))))
		foreign := changed
		foreign.Namespace = workflowOtherNamespace
		require.NoError(t, p.RecordUsage(ctx, foreign), "observation IDs are namespace scoped")
		require.NoError(t, p.RecordUsage(ctx, observation("task-c", "c-call", "call-c", "call", 9, 9,
			workflowAt(0).Add(time.Second))))

		for _, tc := range []struct {
			namespace, task string
			input, output   int64
		}{
			{workflowNamespace, "task-c", 9, 9},
			{workflowNamespace, "task-b", 67, 8},
			{workflowNamespace, "unreported", 0, 0},
			{workflowOtherNamespace, "task-b", 0, 0},
		} {
			input, output, err := p.TaskTokenUsage(ctx, tc.namespace, tc.task)
			require.NoError(t, err)
			require.Equal(t, [2]int64{tc.input, tc.output}, [2]int64{input, output}, "%s/%s", tc.namespace, tc.task)
		}
	})

	t.Run("pull request links and outcomes", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		work := registerWork(t, p, 1, workflowAt(0))
		link := storagev1.UsagePRLink{Namespace: workflowNamespace, WorkID: work, Repository: "org/repo", Number: 7,
			Origin: "assisted", EvidenceID: "evidence-1", LinkedAt: workflowAt(5)}
		invalid := link
		invalid.Origin = "mentioned"
		require.ErrorIs(t, p.LinkUsagePullRequest(ctx, invalid), storagev1.ErrValidation)
		invalid = link
		invalid.EvidenceID = ""
		require.ErrorIs(t, p.LinkUsagePullRequest(ctx, invalid), storagev1.ErrValidation)
		invalid = link
		invalid.Repository = "org/other"
		require.ErrorIs(t, p.LinkUsagePullRequest(ctx, invalid), storagev1.ErrConflict)
		require.NoError(t, p.LinkUsagePullRequest(ctx, link))
		link.Origin = "created"
		require.NoError(t, p.LinkUsagePullRequest(ctx, link))
		link.Origin = "review_only"
		require.NoError(t, p.LinkUsagePullRequest(ctx, link), "a created link is never downgraded")

		require.ErrorIs(t, p.RecordUsagePullRequest(ctx, storagev1.UsagePullRequest{Namespace: workflowNamespace,
			Repository: "org/repo", Number: 7, State: "open", Ready: true, ObservedAt: workflowAt(6)}), storagev1.ErrValidation)
		require.ErrorIs(t, p.RecordUsagePullRequest(ctx, storagev1.UsagePullRequest{Namespace: workflowNamespace,
			Repository: "org/repo", Number: 7, State: "merged", ObservedAt: workflowAt(6)}), storagev1.ErrValidation)
		require.NoError(t, p.RecordUsagePullRequest(ctx, storagev1.UsagePullRequest{Namespace: workflowNamespace,
			Repository: "org/repo", Number: 7, GitHubID: "PR_7", URL: "https://forge.example/pr/7", State: "open",
			ObservedAt: workflowAt(6)}))
		data, err := p.LoadUsage(ctx, storagev1.UsageFilter{Namespaces: []string{workflowNamespace}, AsOf: time.Now().UTC()})
		require.NoError(t, err)
		require.Len(t, data.Links, 1)
		require.Equal(t, "created", data.Links[0].Origin)
		require.Len(t, data.PullRequests, 1)
		require.Equal(t, "open", data.PullRequests[0].State)

		_, err = p.ListUsagePullRequestLinks(ctx, workflowNamespace, "monitor-uid", workflowAt(60), 0)
		require.ErrorIs(t, err, storagev1.ErrValidation)
		stale, err := p.ListUsagePullRequestLinks(ctx, workflowNamespace, "monitor-uid", workflowAt(60), 10)
		require.NoError(t, err)
		require.Len(t, stale, 1, "an open PR observed before the cutoff needs a refresh")
		merged := workflowAt(20)
		require.NoError(t, p.RecordUsagePullRequest(ctx, storagev1.UsagePullRequest{Namespace: workflowNamespace,
			Repository: "org/repo", Number: 7, GitHubID: "PR_7", URL: "https://forge.example/pr/7", State: "merged",
			MergedAt: &merged, ObservedAt: workflowAt(21)}))
		stale, err = p.ListUsagePullRequestLinks(ctx, workflowNamespace, "monitor-uid", workflowAt(60), 10)
		require.NoError(t, err)
		require.Empty(t, stale, "a merged PR is not refreshed again")
	})

	t.Run("report selection filters", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		first := registerWork(t, p, 1, workflowAt(0))
		registerWork(t, p, 2, workflowAt(60))
		_, err := p.LoadUsage(ctx, storagev1.UsageFilter{})
		require.ErrorIs(t, err, storagev1.ErrValidation, "a report names its namespaces")
		_, err = p.LoadUsage(ctx, storagev1.UsageFilter{Namespaces: []string{""}})
		require.ErrorIs(t, err, storagev1.ErrValidation)
		asOf := time.Now().UTC()
		for _, tc := range []struct {
			name   string
			filter storagev1.UsageFilter
			want   int
		}{
			{"all", storagev1.UsageFilter{}, 2},
			{"work", storagev1.UsageFilter{WorkID: first}, 1},
			{"period", storagev1.UsageFilter{From: workflowAt(-1), Until: workflowAt(1)}, 1},
			{"kind", storagev1.UsageFilter{Kind: "pull_request"}, 0},
			{"repository", storagev1.UsageFilter{Repository: "ORG/REPO"}, 2},
			{"as of", storagev1.UsageFilter{AsOf: workflowAt(30)}, 1},
			{"other namespace", storagev1.UsageFilter{Namespaces: []string{workflowOtherNamespace}}, 0},
		} {
			filter := tc.filter
			if filter.Namespaces == nil {
				filter.Namespaces = []string{workflowNamespace}
			}
			if filter.AsOf.IsZero() {
				filter.AsOf = asOf
			}
			data, err := p.LoadUsage(ctx, filter)
			require.NoError(t, err, tc.name)
			require.Len(t, data.Works, tc.want, tc.name)
		}
	})

	t.Run("retention prunes whole inactive cohorts", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		start := time.Now().UTC().Add(-120 * 24 * time.Hour).Truncate(time.Second)
		expired := registerWork(t, p, 1, start)
		active := registerWork(t, p, 2, start)
		for _, task := range []storagev1.UsageTask{
			{Namespace: workflowNamespace, TaskUID: "expired", TaskName: "expired", WorkID: expired, Phase: "Failed",
				StartedAt: start, PhaseObservedAt: start},
			{Namespace: workflowNamespace, TaskUID: "active", TaskName: "active", WorkID: active, Phase: "Succeeded",
				StartedAt: start, PhaseObservedAt: start},
		} {
			require.NoError(t, p.RegisterUsageTask(ctx, task))
		}
		require.NoError(t, p.RecordUsage(ctx, observation("expired", "old-call", "old-call", "call", 20, 0, start)))
		require.NoError(t, p.RecordUsage(ctx, observation("active", "early-call", "early-call", "call", 10, 0, start)))
		require.NoError(t, p.RecordUsage(ctx, observation("active", "late-call", "late-call", "call", 30, 0,
			time.Now().UTC().Add(-time.Hour))))

		cutoff := time.Now().UTC().Add(-90 * 24 * time.Hour)
		require.NoError(t, p.PruneUsage(ctx, cutoff))
		require.NoError(t, p.PruneUsage(ctx, cutoff.Add(-24*time.Hour)), "an earlier cutoff never moves retention back")
		data, err := p.LoadUsage(ctx, storagev1.UsageFilter{Namespaces: []string{workflowNamespace}, AsOf: time.Now().UTC()})
		require.NoError(t, err)
		require.Len(t, data.Works, 1)
		require.Equal(t, active, data.Works[0].ID, "an active cohort keeps its early records")
		observed := make([]string, 0, len(data.Observations))
		for _, loaded := range data.Observations {
			observed = append(observed, loaded.ID)
		}
		require.ElementsMatch(t, []string{"early-call", "late-call"}, observed)
		require.NotNil(t, data.RetainedSince)
		requireWorkflowTime(t, cutoff, *data.RetainedSince)
	})
}
