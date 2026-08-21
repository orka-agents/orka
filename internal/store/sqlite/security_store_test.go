/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

const (
	testFindingStatePROpen = "pr_open"
	testScanRunID2         = "scan-2"
	testStateOpen          = "open"
)

func bundleStatusTestScanRun(id string, status store.BundleStatus) *store.ScanRun {
	quality := store.LegacyScanQuality()
	quality.BundleStatus = status
	return &store.ScanRun{
		ID:                       id,
		RunUID:                   "run_" + strings.Repeat("a", 64),
		Namespace:                "ns1",
		RepositoryScan:           "repo1",
		RepositoryScanUID:        "repository-scan-uid-1",
		RepositoryScanGeneration: 1,
		TaskName:                 "task-" + id,
		Mode:                     "manual",
		Phase:                    "succeeded",
		StartedAt:                time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC),
		Quality:                  quality,
		Summary:                  "draft snapshot",
	}
}

func latestRunListTestRun(
	id, namespace, repository, repositoryUID string,
	repositoryGeneration int64,
	startedAt time.Time,
) *store.ScanRun {
	return &store.ScanRun{
		ID:                       id,
		Namespace:                namespace,
		RepositoryScan:           repository,
		RepositoryScanUID:        repositoryUID,
		RepositoryScanGeneration: repositoryGeneration,
		TaskName:                 "task-" + id,
		Mode:                     "manual",
		Phase:                    "succeeded",
		StartedAt:                startedAt,
		Quality:                  store.LegacyScanQuality(),
	}
}

func TestListLatestScanRunsBatchesByCurrentRepositoryIncarnation(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	baseTime := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)

	runs := []*store.ScanRun{
		latestRunListTestRun("scan-repo-a-older", "ns1", "repo-a", "repo-a-uid", 3, baseTime),
		latestRunListTestRun("scan-repo-a-tie-a", "ns1", "repo-a", "repo-a-uid", 3, baseTime.Add(time.Minute)),
		latestRunListTestRun("scan-repo-a-tie-z", "ns1", "repo-a", "repo-a-uid", 3, baseTime.Add(time.Minute)),
		latestRunListTestRun("scan-repo-a-stale-incarnation", "ns1", "repo-a", "repo-a-old-uid", 9, baseTime.Add(2*time.Minute)),
		latestRunListTestRun("scan-repo-b-latest", "ns1", "repo-b", "repo-b-uid", 1, baseTime.Add(3*time.Minute)),
		latestRunListTestRun("scan-repo-b-other-namespace", "ns2", "repo-b", "repo-b-uid", 1, baseTime.Add(4*time.Minute)),
	}
	for _, run := range runs {
		if err := s.CreateScanRun(ctx, run); err != nil {
			t.Fatalf("CreateScanRun(%s) error = %v", run.ID, err)
		}
	}

	requested := []store.RepositoryScanIdentity{
		{Name: "repo-b", UID: "repo-b-uid", Generation: 1},
		{Name: "repo-a", UID: "repo-a-uid", Generation: 3},
		{Name: "repo-a", UID: "repo-a-uid", Generation: 3},
		{Name: "repo-without-runs", UID: "repo-empty-uid", Generation: 1},
	}
	got, err := s.ListLatestScanRuns(ctx, "ns1", requested)
	if err != nil {
		t.Fatalf("ListLatestScanRuns() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListLatestScanRuns() returned %d runs, want 2: %#v", len(got), got)
	}

	gotByIdentity := make(map[store.RepositoryScanIdentity]store.ScanRun, len(got))
	for _, run := range got {
		identity := store.RepositoryScanIdentity{
			Name:       run.RepositoryScan,
			UID:        run.RepositoryScanUID,
			Generation: run.RepositoryScanGeneration,
		}
		gotByIdentity[identity] = run
	}
	if run := gotByIdentity[(store.RepositoryScanIdentity{Name: "repo-a", UID: "repo-a-uid", Generation: 3})]; run.ID != "scan-repo-a-tie-z" {
		t.Fatalf("latest repo-a run = %q, want scan-repo-a-tie-z", run.ID)
	}
	if run := gotByIdentity[(store.RepositoryScanIdentity{Name: "repo-b", UID: "repo-b-uid", Generation: 1})]; run.ID != "scan-repo-b-latest" {
		t.Fatalf("latest repo-b run = %q, want scan-repo-b-latest", run.ID)
	}
}

func TestListLatestScanRunsValidatesRepositoryIdentities(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	runs, err := s.ListLatestScanRuns(ctx, "ns1", nil)
	if err != nil {
		t.Fatalf("ListLatestScanRuns(empty) error = %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("ListLatestScanRuns(empty) = %#v, want empty", runs)
	}

	for _, tc := range []struct {
		name     string
		identity store.RepositoryScanIdentity
	}{
		{name: "missing name", identity: store.RepositoryScanIdentity{UID: "repo-uid", Generation: 1}},
		{name: "name whitespace", identity: store.RepositoryScanIdentity{Name: " repo", UID: "repo-uid", Generation: 1}},
		{name: "missing UID", identity: store.RepositoryScanIdentity{Name: "repo", Generation: 1}},
		{name: "UID whitespace", identity: store.RepositoryScanIdentity{Name: "repo", UID: "repo-uid ", Generation: 1}},
		{name: "zero generation", identity: store.RepositoryScanIdentity{Name: "repo", UID: "repo-uid"}},
		{name: "negative generation", identity: store.RepositoryScanIdentity{Name: "repo", UID: "repo-uid", Generation: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.ListLatestScanRuns(ctx, "ns1", []store.RepositoryScanIdentity{tc.identity}); !errors.Is(err, store.ErrValidation) {
				t.Fatalf("ListLatestScanRuns() error = %v, want ErrValidation", err)
			}
		})
	}
}

func TestValidateScanRunBundleStatusTransitionGraph(t *testing.T) {
	statuses := []store.BundleStatus{
		store.BundleStatusNotStarted,
		store.BundleStatusDraft,
		store.BundleStatusSealing,
		store.BundleStatusSealed,
		store.BundleStatusRetryableFailed,
		store.BundleStatusFailed,
	}
	allowed := map[store.BundleStatus]map[store.BundleStatus]bool{
		store.BundleStatusNotStarted: {
			store.BundleStatusNotStarted: true,
			store.BundleStatusDraft:      true,
			store.BundleStatusSealing:    true,
			store.BundleStatusFailed:     true,
		},
		store.BundleStatusDraft: {
			store.BundleStatusDraft:           true,
			store.BundleStatusSealing:         true,
			store.BundleStatusRetryableFailed: true,
			store.BundleStatusFailed:          true,
		},
		store.BundleStatusSealing: {
			store.BundleStatusSealing:         true,
			store.BundleStatusSealed:          true,
			store.BundleStatusRetryableFailed: true,
			store.BundleStatusFailed:          true,
		},
		store.BundleStatusRetryableFailed: {
			store.BundleStatusRetryableFailed: true,
			store.BundleStatusSealing:         true,
			store.BundleStatusFailed:          true,
		},
		store.BundleStatusFailed: {
			store.BundleStatusFailed: true,
		},
	}

	for _, current := range statuses {
		for _, requested := range statuses {
			t.Run(string(current)+"_to_"+string(requested), func(t *testing.T) {
				err := validateScanRunBundleStatusTransition(current, requested)
				if allowed[current][requested] {
					if err != nil {
						t.Fatalf("validateScanRunBundleStatusTransition() error = %v", err)
					}
					return
				}
				if !errors.Is(err, store.ErrConflict) {
					t.Fatalf("validateScanRunBundleStatusTransition() error = %v, want ErrConflict", err)
				}
			})
		}
	}
}

func TestUpdateScanRunRejectedBundleTransitionDoesNotMutateProjection(t *testing.T) {
	tests := []struct {
		name      string
		current   store.BundleStatus
		requested store.BundleStatus
	}{
		{name: "draft cannot jump to sealed", current: store.BundleStatusDraft, requested: store.BundleStatusSealed},
		{name: "retryable failure cannot return to not started", current: store.BundleStatusRetryableFailed, requested: store.BundleStatusNotStarted},
		{name: "retryable failure cannot return to draft", current: store.BundleStatusRetryableFailed, requested: store.BundleStatusDraft},
		{name: "failure cannot return to draft", current: store.BundleStatusFailed, requested: store.BundleStatusDraft},
		{name: "failure cannot restart sealing", current: store.BundleStatusFailed, requested: store.BundleStatusSealing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := setupTestStore(t)
			ctx := context.Background()
			run := bundleStatusTestScanRun("scan-bundle-transition", tt.current)
			if err := s.CreateScanRun(ctx, run); err != nil {
				t.Fatalf("CreateScanRun() error = %v", err)
			}
			update, err := s.GetScanRun(ctx, run.Namespace, run.ID)
			if err != nil {
				t.Fatalf("GetScanRun() error = %v", err)
			}
			update.Quality.BundleStatus = tt.requested
			update.Summary = "rejected stale projection"
			if err := s.UpdateScanRun(ctx, update); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("UpdateScanRun() error = %v, want ErrConflict", err)
			}
			got, err := s.GetScanRun(ctx, run.Namespace, run.ID)
			if err != nil {
				t.Fatalf("GetScanRun(after rejection) error = %v", err)
			}
			if got.Quality.BundleStatus != tt.current || got.Summary != run.Summary {
				t.Fatalf("stored scan run = (%q, %q), want (%q, %q)", got.Quality.BundleStatus, got.Summary, tt.current, run.Summary)
			}
		})
	}
}

func TestUpdateScanRunRejectsStaleBundleStatusAcrossSealing(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	run := bundleStatusTestScanRun("scan-bundle-stale", store.BundleStatusDraft)
	if err := s.CreateScanRun(ctx, run); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}

	stale, err := s.GetScanRun(ctx, run.Namespace, run.ID)
	if err != nil {
		t.Fatalf("GetScanRun(stale) error = %v", err)
	}
	sealing := *stale
	sealing.Quality.BundleStatus = store.BundleStatusSealing
	sealing.Summary = "sealing snapshot"
	if err := s.UpdateScanRun(ctx, &sealing); err != nil {
		t.Fatalf("UpdateScanRun(sealing) error = %v", err)
	}

	for _, status := range []store.BundleStatus{store.BundleStatusNotStarted, store.BundleStatusDraft} {
		t.Run(string(status), func(t *testing.T) {
			staleUpdate := *stale
			staleUpdate.Quality.BundleStatus = status
			staleUpdate.Summary = "stale " + string(status) + " snapshot"
			if err := s.UpdateScanRun(ctx, &staleUpdate); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("UpdateScanRun(stale %s) error = %v, want ErrConflict", status, err)
			}
			got, err := s.GetScanRun(ctx, run.Namespace, run.ID)
			if err != nil {
				t.Fatalf("GetScanRun(after stale %s) error = %v", status, err)
			}
			if got.Quality.BundleStatus != store.BundleStatusSealing || got.Summary != sealing.Summary {
				t.Fatalf("stored scan run = (%q, %q), want (%q, %q)", got.Quality.BundleStatus, got.Summary,
					store.BundleStatusSealing, sealing.Summary)
			}
		})
	}
}

func TestUpdateScanRunAllowsSealingTerminalTransitions(t *testing.T) {
	for _, status := range []store.BundleStatus{
		store.BundleStatusSealed,
		store.BundleStatusRetryableFailed,
		store.BundleStatusFailed,
	} {
		t.Run(string(status), func(t *testing.T) {
			s := setupTestStore(t)
			ctx := context.Background()
			run := bundleStatusTestScanRun("scan-bundle-"+string(status), store.BundleStatusSealing)
			if err := s.CreateScanRun(ctx, run); err != nil {
				t.Fatalf("CreateScanRun() error = %v", err)
			}

			transition, err := s.GetScanRun(ctx, run.Namespace, run.ID)
			if err != nil {
				t.Fatalf("GetScanRun() error = %v", err)
			}
			transition.Quality.BundleStatus = status
			transition.Summary = "sealer " + string(status)
			if err := s.UpdateScanRun(ctx, transition); err != nil {
				t.Fatalf("UpdateScanRun(%s) error = %v", status, err)
			}
			got, err := s.GetScanRun(ctx, run.Namespace, run.ID)
			if err != nil {
				t.Fatalf("GetScanRun(after %s) error = %v", status, err)
			}
			if got.Quality.BundleStatus != status || got.Summary != transition.Summary {
				t.Fatalf("stored scan run = (%q, %q), want (%q, %q)", got.Quality.BundleStatus, got.Summary,
					status, transition.Summary)
			}

			if status == store.BundleStatusSealed {
				sealedUpdate := *got
				sealedUpdate.Summary = "mutated after seal"
				if err := s.UpdateScanRun(ctx, &sealedUpdate); !errors.Is(err, store.ErrConflict) {
					t.Fatalf("UpdateScanRun(sealed) error = %v, want ErrConflict", err)
				}
			}
		})
	}
}

func TestUpdateScanRunConcurrentSealingRejectsStaleCopies(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	run := bundleStatusTestScanRun("scan-bundle-concurrent", store.BundleStatusDraft)
	if err := s.CreateScanRun(ctx, run); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}

	stale, err := s.GetScanRun(ctx, run.Namespace, run.ID)
	if err != nil {
		t.Fatalf("GetScanRun(stale) error = %v", err)
	}
	sealing := *stale
	sealing.Quality.BundleStatus = store.BundleStatusSealing
	sealing.Summary = "sealing snapshot"
	if err := s.UpdateScanRun(ctx, &sealing); err != nil {
		t.Fatalf("UpdateScanRun(sealing) error = %v", err)
	}

	sealer, err := s.GetScanRun(ctx, run.Namespace, run.ID)
	if err != nil {
		t.Fatalf("GetScanRun(sealer) error = %v", err)
	}
	sealer.Quality.BundleStatus = store.BundleStatusSealed
	sealer.Summary = "sealed snapshot"

	const staleWriters = 12
	type updateResult struct {
		sealer bool
		err    error
	}
	start := make(chan struct{})
	results := make(chan updateResult, staleWriters+1)
	for i := range staleWriters {
		staleUpdate := *stale
		if i%2 == 0 {
			staleUpdate.Quality.BundleStatus = store.BundleStatusNotStarted
		}
		staleUpdate.Summary = fmt.Sprintf("stale snapshot %d", i)
		go func(update store.ScanRun) {
			<-start
			results <- updateResult{err: s.UpdateScanRun(ctx, &update)}
		}(staleUpdate)
	}
	go func() {
		<-start
		results <- updateResult{sealer: true, err: s.UpdateScanRun(ctx, sealer)}
	}()
	close(start)

	for range staleWriters + 1 {
		result := <-results
		if result.sealer {
			if result.err != nil {
				t.Fatalf("UpdateScanRun(sealer) error = %v", result.err)
			}
			continue
		}
		if !errors.Is(result.err, store.ErrConflict) {
			t.Fatalf("UpdateScanRun(stale concurrent copy) error = %v, want ErrConflict", result.err)
		}
	}

	got, err := s.GetScanRun(ctx, run.Namespace, run.ID)
	if err != nil {
		t.Fatalf("GetScanRun(final) error = %v", err)
	}
	if got.Quality.BundleStatus != store.BundleStatusSealed || got.Summary != sealer.Summary {
		t.Fatalf("final scan run = (%q, %q), want (%q, %q)", got.Quality.BundleStatus, got.Summary,
			store.BundleStatusSealed, sealer.Summary)
	}
}

func testPatchProposalPublication(suffix string) (*store.PatchProposal, *store.PatchProposal) {
	proposal := &store.PatchProposal{
		ID:             "patch-" + suffix,
		Namespace:      "ns1",
		RepositoryScan: "repo1",
		FindingID:      "finding-" + suffix,
		TaskName:       "task-" + suffix,
		Branch:         "orka/security/" + suffix,
		Status:         "pending",
	}
	bound := *proposal
	prNumber := 42
	headSHA := strings.Repeat("b", 40)
	bound.DiffArtifact = "security-patch-" + suffix + ".diff"
	bound.SummaryArtifact = "security-patch-" + suffix + ".json"
	bound.Status = securityPatchProposalStatusPROpened
	bound.PRNumber = &prNumber
	bound.PRURL = "https://github.com/example/source/pull/42"
	bound.PublicationEvidence = &store.PatchPublicationEvidence{
		PublicationID:      "pub-" + suffix,
		ArtifactDigest:     "sha256:" + strings.Repeat("a", 64),
		SourceRepositoryID: "github.com/example/source",
		SourceRef:          strings.Repeat("1", 40),
		SourceBaselineSHA:  strings.Repeat("1", 40),
		TargetRepositoryID: "github.com/example/target",
		TargetRef:          "refs/heads/" + bound.Branch,
		ExpectedCommitSHA:  headSHA,
		VerifiedRemoteSHA:  headSHA,
		PRIntent: store.PullRequestIntent{
			BaseRepositoryID:      "github.com/example/source",
			BaseRef:               "refs/heads/main",
			HeadRepositoryID:      "github.com/example/target",
			HeadRef:               "refs/heads/" + bound.Branch,
			PublicationGeneration: 1,
			ExpectedHeadSHA:       headSHA,
		},
		PRReceipt: store.PatchPullRequestEvidence{
			IntentKey: "sha256:" + strings.Repeat("c", 64),
			ForgeID:   "github:123:42",
			Number:    prNumber,
			URL:       bound.PRURL,
			State:     "Open",
			HeadSHA:   headSHA,
		},
	}
	return proposal, &bound
}

func clonePatchProposal(proposal *store.PatchProposal) *store.PatchProposal {
	if proposal == nil {
		return nil
	}
	clone := *proposal
	if proposal.PRNumber != nil {
		value := *proposal.PRNumber
		clone.PRNumber = &value
	}
	if proposal.PublicationEvidence != nil {
		evidence := *proposal.PublicationEvidence
		clone.PublicationEvidence = &evidence
	}
	return &clone
}

func onlyPatchProposal(t *testing.T, s *Store, namespace, findingID string) store.PatchProposal {
	t.Helper()
	proposals, err := s.ListPatchProposals(context.Background(), namespace, findingID)
	if err != nil {
		t.Fatalf("ListPatchProposals() error = %v", err)
	}
	if len(proposals) != 1 {
		t.Fatalf("len(proposals) = %d, want 1", len(proposals))
	}
	return proposals[0]
}

func TestPatchProposalPublicationEvidenceBindIsImmutableAndReplaySafe(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	initial, bound := testPatchProposalPublication("immutable")
	if err := s.CreatePatchProposal(ctx, initial); err != nil {
		t.Fatalf("CreatePatchProposal() error = %v", err)
	}
	if err := s.BindPatchProposalPublicationEvidence(ctx, bound); err != nil {
		t.Fatalf("BindPatchProposalPublicationEvidence() error = %v", err)
	}

	stored := onlyPatchProposal(t, s, initial.Namespace, initial.FindingID)
	if !reflect.DeepEqual(stored.PublicationEvidence, bound.PublicationEvidence) {
		t.Fatalf("publication evidence = %#v, want %#v", stored.PublicationEvidence, bound.PublicationEvidence)
	}
	firstUpdatedAt := stored.UpdatedAt

	replay := clonePatchProposal(bound)
	replay.CreatedAt = time.Time{}
	replay.UpdatedAt = time.Time{}
	if err := s.BindPatchProposalPublicationEvidence(ctx, replay); err != nil {
		t.Fatalf("identical BindPatchProposalPublicationEvidence() replay error = %v", err)
	}
	if !replay.UpdatedAt.Equal(firstUpdatedAt) {
		t.Fatalf("identical replay updatedAt = %v, want unchanged %v", replay.UpdatedAt, firstUpdatedAt)
	}

	identicalUpdate := clonePatchProposal(&stored)
	identicalUpdate.PublicationEvidence = nil
	if err := s.UpdatePatchProposal(ctx, identicalUpdate); err != nil {
		t.Fatalf("identical UpdatePatchProposal() error = %v", err)
	}
	if !identicalUpdate.UpdatedAt.Equal(firstUpdatedAt) {
		t.Fatalf("identical generic update updatedAt = %v, want unchanged %v", identicalUpdate.UpdatedAt, firstUpdatedAt)
	}

	mutations := []struct {
		name   string
		mutate func(*store.PatchProposal)
	}{
		{name: "task name", mutate: func(p *store.PatchProposal) { p.TaskName = "other-task" }},
		{name: "branch", mutate: func(p *store.PatchProposal) { p.Branch = "orka/security/other" }},
		{name: "diff artifact", mutate: func(p *store.PatchProposal) { p.DiffArtifact = "other.diff" }},
		{name: "summary artifact", mutate: func(p *store.PatchProposal) { p.SummaryArtifact = "other.json" }},
		{name: "status", mutate: func(p *store.PatchProposal) { p.Status = publishPhaseFailed }},
		{name: "PR number", mutate: func(p *store.PatchProposal) { value := 43; p.PRNumber = &value }},
		{name: "PR URL", mutate: func(p *store.PatchProposal) { p.PRURL = "https://github.com/example/source/pull/43" }},
	}
	for _, tt := range mutations {
		t.Run(tt.name, func(t *testing.T) {
			candidate := clonePatchProposal(&stored)
			candidate.PublicationEvidence = nil
			tt.mutate(candidate)
			if err := s.UpdatePatchProposal(ctx, candidate); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("UpdatePatchProposal() error = %v, want conflict", err)
			}
			after := onlyPatchProposal(t, s, initial.Namespace, initial.FindingID)
			if !after.UpdatedAt.Equal(firstUpdatedAt) || !reflect.DeepEqual(after, stored) {
				t.Fatalf("bound proposal changed after rejected %s mutation: got %#v, want %#v", tt.name, after, stored)
			}
		})
	}

	conflictingBind := clonePatchProposal(bound)
	conflictingBind.PublicationEvidence.ArtifactDigest = "sha256:" + strings.Repeat("d", 64)
	if err := s.BindPatchProposalPublicationEvidence(ctx, conflictingBind); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("conflicting BindPatchProposalPublicationEvidence() error = %v, want conflict", err)
	}
	afterConflict := onlyPatchProposal(t, s, initial.Namespace, initial.FindingID)
	if !reflect.DeepEqual(afterConflict, stored) {
		t.Fatalf("bound proposal changed after conflicting bind: got %#v, want %#v", afterConflict, stored)
	}
}

func TestCreateScanRunAtomicallyRejectsConcurrentActiveIdempotency(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	start := make(chan struct{})
	results := make(chan error, 2)
	for index, id := range []string{"scan-concurrent-a", "scan-concurrent-b"} {
		idempotencyKey := "scanidem:concurrent-a"
		if index == 1 {
			idempotencyKey = "scanidem:concurrent-b"
		}
		go func() {
			<-start
			results <- s.CreateScanRun(ctx, &store.ScanRun{
				ID:             id,
				Namespace:      "ns1",
				RepositoryScan: "repo1",
				TaskName:       id + "-task",
				Mode:           "manual",
				Phase:          "pending",
				IdempotencyKey: idempotencyKey,
				StartedAt:      time.Now(),
			})
		}()
	}
	close(start)

	successes := 0
	conflicts := 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, store.ErrConflict):
			conflicts++
		default:
			t.Fatalf("CreateScanRun() error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("CreateScanRun() successes/conflicts = %d/%d, want 1/1", successes, conflicts)
	}

	runs, _, err := s.ListScanRuns(ctx, "ns1", "repo1", 10, "")
	if err != nil {
		t.Fatalf("ListScanRuns() error = %v", err)
	}
	if len(runs) != 1 || runs[0].IdempotencyKey == "" || runs[0].Phase != "pending" {
		t.Fatalf("runs = %#v, want one pending claimed run", runs)
	}

	runs[0].Phase = "failed"
	completedAt := time.Now()
	runs[0].CompletedAt = &completedAt
	if err := s.UpdateScanRun(ctx, &runs[0]); err != nil {
		t.Fatalf("UpdateScanRun() error = %v", err)
	}
	if err := s.CreateScanRun(ctx, &store.ScanRun{
		ID:             "scan-concurrent-retry",
		Namespace:      "ns1",
		RepositoryScan: "repo1",
		TaskName:       "scan-concurrent-retry-task",
		Mode:           "manual",
		Phase:          "pending",
		IdempotencyKey: "scanidem:concurrent-retry",
		StartedAt:      time.Now(),
	}); err != nil {
		t.Fatalf("CreateScanRun() after terminal run error = %v", err)
	}
}

func TestCreateScanRunAtomicallyRejectsConcurrentActiveAcrossConnections(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "security-scan-admission.db")
	dbA, err := NewDB(databasePath)
	if err != nil {
		t.Fatalf("NewDB(first) error = %v", err)
	}
	t.Cleanup(func() { _ = dbA.Close() })
	dbB, err := NewDB(databasePath)
	if err != nil {
		t.Fatalf("NewDB(second) error = %v", err)
	}
	t.Cleanup(func() { _ = dbB.Close() })
	stores := []*Store{NewStore(dbA, databasePath), NewStore(dbB, databasePath)}

	ctx := context.Background()
	start := make(chan struct{})
	results := make(chan error, len(stores))
	for index := range stores {
		go func() {
			<-start
			results <- stores[index].CreateScanRun(ctx, &store.ScanRun{
				ID:             "scan-cross-connection-" + string(rune('a'+index)),
				Namespace:      "ns1",
				RepositoryScan: "repo1",
				TaskName:       "scan-cross-connection-task-" + string(rune('a'+index)),
				Mode:           "manual",
				Phase:          "pending",
				IdempotencyKey: "scanidem:cross-connection-" + string(rune('a'+index)),
				StartedAt:      time.Now(),
			})
		}()
	}
	close(start)

	successes := 0
	conflicts := 0
	for range stores {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, store.ErrConflict):
			conflicts++
		default:
			t.Fatalf("CreateScanRun() cross-connection error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("CreateScanRun() cross-connection successes/conflicts = %d/%d, want 1/1", successes, conflicts)
	}

	runs, _, err := stores[0].ListScanRuns(ctx, "ns1", "repo1", 10, "")
	if err != nil {
		t.Fatalf("ListScanRuns() error = %v", err)
	}
	if len(runs) != 1 || runs[0].Phase != "pending" {
		t.Fatalf("runs = %#v, want one pending cross-connection claim", runs)
	}
}

func TestSaveThreatModelReplacesCurrentModel(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	first := &store.ThreatModel{
		Namespace:      "ns1",
		RepositoryScan: "repo1",
		Content:        "first threat model",
		Source:         "generated",
	}
	if err := s.SaveThreatModel(ctx, first); err != nil {
		t.Fatalf("SaveThreatModel(first): %v", err)
	}

	second := &store.ThreatModel{
		Namespace:      "ns1",
		RepositoryScan: "repo1",
		Content:        "updated threat model",
		Source:         "edited",
	}
	if err := s.SaveThreatModel(ctx, second); err != nil {
		t.Fatalf("SaveThreatModel(second): %v", err)
	}

	got, err := s.GetLatestThreatModel(ctx, "ns1", "repo1")
	if err != nil {
		t.Fatalf("GetLatestThreatModel: %v", err)
	}
	if got.Content != "updated threat model" {
		t.Fatalf("Content = %q, want %q", got.Content, "updated threat model")
	}
	if got.Source != "edited" {
		t.Fatalf("Source = %q, want %q", got.Source, "edited")
	}
	if got.Version != 2 {
		t.Fatalf("Version = %d, want 2", got.Version)
	}

	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM security_threat_models WHERE namespace = ? AND repository_scan = ?`,
		"ns1", "repo1",
	).Scan(&count); err != nil {
		t.Fatalf("count threat models: %v", err)
	}
	if count != 1 {
		t.Fatalf("threat model row count = %d, want 1", count)
	}
}

func TestSaveThreatModelRedactsCredentialContent(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	model := &store.ThreatModel{
		Namespace: "ns1", RepositoryScan: "repo1", Source: "edited",
		Content: strings.Join([]string{"Author", "ization: ", "Bear", "er mutable-value-for-redaction"}, ""),
	}
	if err := s.SaveThreatModel(ctx, model); err != nil {
		t.Fatalf("SaveThreatModel() error = %v", err)
	}
	got, err := s.GetLatestThreatModel(ctx, "ns1", "repo1")
	if err != nil {
		t.Fatalf("GetLatestThreatModel() error = %v", err)
	}
	if strings.Contains(got.Content, "mutable-value-for-redaction") || !strings.Contains(got.Content, "[REDACTED]") {
		t.Fatalf("persisted threat model retained credential: %q", got.Content)
	}
}

func TestSaveThreatModelCollapsesExistingVersions(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	createdAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)

	for version, content := range map[int]string{
		1: "older model",
		2: "newer model",
	} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO security_threat_models
			 (namespace, repository_scan, version, content, source, generated_by_scan, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			"ns1", "repo1", version, content, "generated", "", createdAt, createdAt,
		); err != nil {
			t.Fatalf("seed threat model version %d: %v", version, err)
		}
	}

	current := &store.ThreatModel{
		Namespace:      "ns1",
		RepositoryScan: "repo1",
		Content:        "singleton threat model",
		Source:         "edited",
	}
	if err := s.SaveThreatModel(ctx, current); err != nil {
		t.Fatalf("SaveThreatModel(current): %v", err)
	}

	got, err := s.GetLatestThreatModel(ctx, "ns1", "repo1")
	if err != nil {
		t.Fatalf("GetLatestThreatModel: %v", err)
	}
	if got.Content != "singleton threat model" {
		t.Fatalf("Content = %q, want %q", got.Content, "singleton threat model")
	}
	if got.Version != 3 {
		t.Fatalf("Version = %d, want 3", got.Version)
	}

	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM security_threat_models WHERE namespace = ? AND repository_scan = ?`,
		"ns1", "repo1",
	).Scan(&count); err != nil {
		t.Fatalf("count threat models: %v", err)
	}
	if count != 1 {
		t.Fatalf("threat model row count = %d, want 1", count)
	}
}

func TestUpsertFindingPreservesMostAdvancedStateAndPRMetadata(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	prNumber := 123
	initial := &store.Finding{
		ID:               "fnd-123",
		Namespace:        "ns1",
		RepositoryScan:   "repo1",
		ScanRunID:        "scan-1",
		Fingerprint:      "repo1:file.go:unauthenticated-preview",
		Title:            "Preview disclosure",
		Summary:          "initial summary",
		Severity:         "medium",
		Confidence:       "high",
		ValidationStatus: "validated",
		State:            testFindingStatePROpen,
		PatchProposalID:  "patch-123",
		PRNumber:         &prNumber,
		PRURL:            "https://github.com/example/repo/pull/123",
	}
	if err := s.UpsertFinding(ctx, initial); err != nil {
		t.Fatalf("UpsertFinding(initial): %v", err)
	}

	laterStage := &store.Finding{
		ID:               "fnd-123",
		Namespace:        "ns1",
		RepositoryScan:   "repo1",
		ScanRunID:        testScanRunID2,
		Fingerprint:      initial.Fingerprint,
		Title:            initial.Title,
		Summary:          "later summary",
		Severity:         initial.Severity,
		Confidence:       initial.Confidence,
		ValidationStatus: "pending",
		State:            "patch_ready",
	}
	if err := s.UpsertFinding(ctx, laterStage); err != nil {
		t.Fatalf("UpsertFinding(laterStage): %v", err)
	}

	got, err := s.GetFinding(ctx, "ns1", "fnd-123")
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if got.State != testFindingStatePROpen {
		t.Fatalf("State = %q, want %q", got.State, testFindingStatePROpen)
	}
	if got.ValidationStatus != assessmentStatusValidated {
		t.Fatalf("ValidationStatus = %q, want %q", got.ValidationStatus, "validated")
	}
	if got.PatchProposalID != "patch-123" {
		t.Fatalf("PatchProposalID = %q, want %q", got.PatchProposalID, "patch-123")
	}
	if got.PRNumber == nil || *got.PRNumber != prNumber {
		t.Fatalf("PRNumber = %#v, want %d", got.PRNumber, prNumber)
	}
	if got.PRURL != "https://github.com/example/repo/pull/123" {
		t.Fatalf("PRURL = %q, want preserved PR URL", got.PRURL)
	}
	if got.Summary != "later summary" {
		t.Fatalf("Summary = %q, want later summary to keep newer descriptive fields", got.Summary)
	}
}

func TestUpsertFindingPreservesCanonicalHistoryProjectionFromUnboundUpsert(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	canonical := &store.Finding{
		ID:                            "fnd_canonical_monotonic",
		Namespace:                     "ns1",
		RepositoryScan:                "repo1",
		ScanRunID:                     "scan-canonical",
		SliceID:                       "slice-canonical",
		Fingerprint:                   "compat-canonical-monotonic",
		IdentityQuality:               store.IdentityQualityProducerProposed,
		IdentityAlgorithmVersion:      "producer-v1",
		SemanticFingerprint:           integrityTestDigest("canonical-semantic"),
		LegacyFingerprint:             "legacy-canonical",
		HistoryStatus:                 store.FindingHistoryCanonical,
		CurrentOccurrenceID:           "occ_canonical_monotonic",
		Title:                         "Canonical title",
		Category:                      "injection",
		Summary:                       "Canonical summary",
		Severity:                      "high",
		Confidence:                    "high",
		Triage:                        "confirmed",
		ValidationStatus:              "unvalidated",
		State:                         "open",
		FilePath:                      "internal/api/canonical.go",
		Line:                          42,
		CommitSHA:                     strings.Repeat("a", 40),
		RootCause:                     "Canonical root cause",
		Reproduction:                  "Canonical reproduction",
		Remediation:                   "Canonical remediation",
		SuggestedAction:               "Canonical action",
		WhyTestsDoNotAlreadyCoverThis: "Canonical coverage gap",
		SuggestedRegressionTest:       "Canonical regression test",
		MinimumFixScope:               "Canonical minimum scope",
		Evidence: []store.FindingEvidenceRef{{
			Kind:          "file",
			Path:          "internal/api/canonical.go",
			StartLine:     42,
			EndLine:       45,
			Symbol:        "canonicalHandler",
			Quote:         "canonical evidence",
			ContentSHA256: integrityTestRawDigest("canonical evidence"),
			ContentSize:   128,
		}},
	}
	if err := s.UpsertFinding(ctx, canonical); err != nil {
		t.Fatalf("UpsertFinding(canonical): %v", err)
	}

	prNumber := 123
	proposal := *canonical
	proposal.ScanRunID = "scan-proposed"
	proposal.SliceID = "slice-proposed"
	proposal.IdentityQuality = store.IdentityQualityProducerProposed
	proposal.IdentityAlgorithmVersion = "producer-v2"
	proposal.SemanticFingerprint = integrityTestDigest("producer-semantic")
	proposal.LegacyFingerprint = "legacy-proposed"
	proposal.HistoryStatus = store.FindingHistoryLegacyUnrebuildable
	proposal.CurrentOccurrenceID = "occ_proposed_monotonic"
	proposal.Title = "Producer title"
	proposal.Category = "producer-category"
	proposal.Summary = "Producer summary"
	proposal.Severity = "low"
	proposal.Confidence = "low"
	proposal.Triage = "producer-triage"
	proposal.FilePath = "producer/path.go"
	proposal.Line = 7
	proposal.CommitSHA = strings.Repeat("b", 40)
	proposal.RootCause = "Producer root cause"
	proposal.Reproduction = "Producer reproduction"
	proposal.Remediation = "Producer remediation"
	proposal.SuggestedAction = "Producer action"
	proposal.WhyTestsDoNotAlreadyCoverThis = "Producer coverage gap"
	proposal.SuggestedRegressionTest = "Producer regression test"
	proposal.MinimumFixScope = "Producer minimum scope"
	proposal.Evidence = []store.FindingEvidenceRef{{
		Kind:      "file",
		Path:      "producer/path.go",
		StartLine: 7,
		EndLine:   8,
		Quote:     "producer evidence",
	}}
	proposal.ValidationStatus = "validated"
	proposal.ValidationJSON = validatedStatusJSON
	proposal.State = testFindingStatePROpen
	proposal.PatchProposalID = "patch-proposed"
	proposal.PRNumber = &prNumber
	proposal.PRURL = "https://github.com/example/repo/pull/123"
	if err := s.UpsertFinding(ctx, &proposal); err != nil {
		t.Fatalf("UpsertFinding(producer proposal): %v", err)
	}

	got, err := s.GetFinding(ctx, canonical.Namespace, canonical.ID)
	if err != nil {
		t.Fatalf("GetFinding(): %v", err)
	}
	type occurrenceProjection struct {
		ScanRunID, SliceID, Fingerprint                                         string
		IdentityQuality, IdentityAlgorithmVersion, SemanticFingerprint          string
		LegacyFingerprint, HistoryStatus, CurrentOccurrenceID                   string
		Title, Category, Summary, Severity, Confidence, Triage                  string
		FilePath                                                                string
		Line                                                                    int
		CommitSHA, RootCause, Reproduction, Remediation, SuggestedAction        string
		WhyTestsDoNotAlreadyCoverThis, SuggestedRegressionTest, MinimumFixScope string
		Evidence                                                                []store.FindingEvidenceRef
	}
	projection := func(finding *store.Finding) occurrenceProjection {
		return occurrenceProjection{
			ScanRunID: finding.ScanRunID, SliceID: finding.SliceID, Fingerprint: finding.Fingerprint,
			IdentityQuality: finding.IdentityQuality, IdentityAlgorithmVersion: finding.IdentityAlgorithmVersion,
			SemanticFingerprint: finding.SemanticFingerprint, LegacyFingerprint: finding.LegacyFingerprint,
			HistoryStatus: finding.HistoryStatus, CurrentOccurrenceID: finding.CurrentOccurrenceID,
			Title: finding.Title, Category: finding.Category, Summary: finding.Summary, Severity: finding.Severity,
			Confidence: finding.Confidence, Triage: finding.Triage, FilePath: finding.FilePath, Line: finding.Line,
			CommitSHA: finding.CommitSHA, RootCause: finding.RootCause, Reproduction: finding.Reproduction,
			Remediation: finding.Remediation, SuggestedAction: finding.SuggestedAction,
			WhyTestsDoNotAlreadyCoverThis: finding.WhyTestsDoNotAlreadyCoverThis,
			SuggestedRegressionTest:       finding.SuggestedRegressionTest, MinimumFixScope: finding.MinimumFixScope,
			Evidence: finding.Evidence,
		}
	}
	if want := projection(canonical); !reflect.DeepEqual(projection(got), want) {
		t.Fatalf("unbound upsert hybridized canonical-history occurrence projection:\n got: %#v\nwant: %#v", projection(got), want)
	}
	if got.ValidationStatus != canonical.ValidationStatus || got.ValidationJSON != canonical.ValidationJSON ||
		got.State != canonical.State || got.PatchProposalID != canonical.PatchProposalID ||
		got.PRNumber != nil || got.PRURL != canonical.PRURL {
		t.Fatalf("unbound canonical-history source changed lifecycle projection: %#v", got)
	}
}

func TestUpsertFindingAllowsLifecycleForMatchingCanonicalHistorySource(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	canonical := &store.Finding{
		ID:                       "fnd_canonical_matching_lifecycle",
		Namespace:                "ns1",
		RepositoryScan:           "repo1",
		ScanRunID:                "scan-matching",
		Fingerprint:              "compat-canonical-matching-lifecycle",
		IdentityQuality:          store.IdentityQualityProducerProposed,
		IdentityAlgorithmVersion: "producer-v1",
		SemanticFingerprint:      integrityTestDigest("canonical-matching-lifecycle"),
		HistoryStatus:            store.FindingHistoryCanonical,
		CurrentOccurrenceID:      "occ_matching_lifecycle",
		Title:                    "Canonical title",
		Summary:                  "Canonical summary",
		Severity:                 "high",
		Confidence:               "high",
		ValidationStatus:         "unvalidated",
		State:                    testStateOpen,
	}
	if err := s.UpsertFinding(ctx, canonical); err != nil {
		t.Fatalf("UpsertFinding(canonical): %v", err)
	}

	prNumber := 42
	proposal := *canonical
	proposal.IdentityQuality = store.IdentityQualityProducerProposed
	proposal.IdentityAlgorithmVersion = "producer-v2"
	proposal.SemanticFingerprint = integrityTestDigest("producer-matching-lifecycle")
	proposal.HistoryStatus = store.FindingHistoryLegacyUnrebuildable
	proposal.ValidationStatus = assessmentStatusValidated
	proposal.ValidationJSON = validatedStatusJSON
	proposal.State = testFindingStatePROpen
	proposal.PatchProposalID = "patch-matching"
	proposal.PRNumber = &prNumber
	proposal.PRURL = "https://github.com/example/repo/pull/42"
	if err := s.UpsertFinding(ctx, &proposal); err != nil {
		t.Fatalf("UpsertFinding(proposal): %v", err)
	}

	got, err := s.GetFinding(ctx, canonical.Namespace, canonical.ID)
	if err != nil {
		t.Fatalf("GetFinding(): %v", err)
	}
	if got.ValidationStatus != proposal.ValidationStatus || got.ValidationJSON != proposal.ValidationJSON ||
		got.State != proposal.State || got.PatchProposalID != proposal.PatchProposalID ||
		got.PRNumber == nil || *got.PRNumber != prNumber || got.PRURL != proposal.PRURL {
		t.Fatalf("matching canonical-history source did not advance lifecycle projection: %#v", got)
	}
	if got.IdentityQuality != canonical.IdentityQuality || got.CurrentOccurrenceID != canonical.CurrentOccurrenceID ||
		got.ScanRunID != canonical.ScanRunID || got.Summary != canonical.Summary {
		t.Fatalf("matching canonical-history source changed retained occurrence projection: %#v", got)
	}
}

func TestUpsertFindingLowerQualityLifecycleAllowsOnlyFullyLegacyEmptyBindings(t *testing.T) {
	for _, tc := range []struct {
		name         string
		scanRunID    string
		occurrenceID string
		wantAdvance  bool
	}{
		{name: "both empty", wantAdvance: true},
		{name: "only occurrence bound", occurrenceID: "occ_partial_legacy", wantAdvance: false},
		{name: "only run bound", scanRunID: "scan-partial-legacy", wantAdvance: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupTestStore(t)
			ctx := context.Background()
			existing := &store.Finding{
				ID:                       "fnd_legacy_empty_" + strings.ReplaceAll(tc.name, " ", "_"),
				Namespace:                "ns1",
				RepositoryScan:           "repo1",
				ScanRunID:                tc.scanRunID,
				Fingerprint:              "compat-legacy-empty-" + tc.name,
				IdentityQuality:          store.IdentityQualityProducerProposed,
				IdentityAlgorithmVersion: "producer-v1",
				HistoryStatus:            store.FindingHistoryLegacyUnrebuildable,
				CurrentOccurrenceID:      tc.occurrenceID,
				Title:                    "Existing title",
				Summary:                  "Existing summary",
				Severity:                 "medium",
				Confidence:               "medium",
				ValidationStatus:         "unvalidated",
				State:                    testStateOpen,
			}
			if err := s.UpsertFinding(ctx, existing); err != nil {
				t.Fatalf("UpsertFinding(existing): %v", err)
			}

			prNumber := 7
			lower := *existing
			lower.IdentityQuality = store.IdentityQualityLegacy
			lower.IdentityAlgorithmVersion = store.IdentityAlgorithmLegacyV2
			lower.ValidationStatus = assessmentStatusValidated
			lower.ValidationJSON = validatedStatusJSON
			lower.State = testFindingStatePROpen
			lower.PatchProposalID = "patch-legacy"
			lower.PRNumber = &prNumber
			lower.PRURL = "https://github.com/example/repo/pull/7"
			if err := s.UpsertFinding(ctx, &lower); err != nil {
				t.Fatalf("UpsertFinding(lower): %v", err)
			}

			got, err := s.GetFinding(ctx, existing.Namespace, existing.ID)
			if err != nil {
				t.Fatalf("GetFinding(): %v", err)
			}
			advanced := got.ValidationStatus == lower.ValidationStatus && got.ValidationJSON == lower.ValidationJSON &&
				got.State == lower.State && got.PatchProposalID == lower.PatchProposalID &&
				got.PRNumber != nil && *got.PRNumber == prNumber && got.PRURL == lower.PRURL
			if advanced != tc.wantAdvance {
				t.Fatalf("lifecycle advanced = %v, want %v: %#v", advanced, tc.wantAdvance, got)
			}
		})
	}
}

func TestUpsertFindingAllowsPendingValidationToBecomeTerminal(t *testing.T) {
	for _, tc := range []struct {
		status         string
		validationJSON string
	}{
		{
			status:         "failed",
			validationJSON: `{"status":"failed","summary":"validation failed"}`,
		},
		{
			status:         "skipped",
			validationJSON: `{"status":"skipped","summary":"validation skipped"}`,
		},
	} {
		t.Run(tc.status, func(t *testing.T) {
			s := setupTestStore(t)
			ctx := context.Background()

			initial := &store.Finding{
				ID:               "fnd-" + tc.status,
				Namespace:        "ns1",
				RepositoryScan:   "repo1",
				ScanRunID:        "scan-1",
				Fingerprint:      "repo1:file.go:" + tc.status,
				Title:            "Finding",
				Summary:          "pending validation",
				Severity:         "high",
				Confidence:       "medium",
				ValidationStatus: "pending",
				State:            testStateOpen,
				ValidationJSON:   `{"status":"pending"}`,
			}
			if err := s.UpsertFinding(ctx, initial); err != nil {
				t.Fatalf("UpsertFinding(initial): %v", err)
			}

			terminal := *initial
			terminal.ScanRunID = testScanRunID2
			terminal.Summary = "terminal validation"
			terminal.ValidationStatus = tc.status
			terminal.ValidationJSON = tc.validationJSON
			if err := s.UpsertFinding(ctx, &terminal); err != nil {
				t.Fatalf("UpsertFinding(terminal): %v", err)
			}

			got, err := s.GetFinding(ctx, "ns1", initial.ID)
			if err != nil {
				t.Fatalf("GetFinding: %v", err)
			}
			if got.ValidationStatus != tc.status {
				t.Fatalf("ValidationStatus = %q, want %q", got.ValidationStatus, tc.status)
			}
			if got.ValidationJSON != tc.validationJSON {
				t.Fatalf("ValidationJSON = %q, want %q", got.ValidationJSON, tc.validationJSON)
			}
		})
	}
}

func TestUpsertFindingKeepsValidationJSONWhenValidatedStatusIsPreserved(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	initial := &store.Finding{
		ID:               "fnd-validated",
		Namespace:        "ns1",
		RepositoryScan:   "repo1",
		ScanRunID:        "scan-1",
		Fingerprint:      "repo1:file.go:validated",
		Title:            "Finding",
		Summary:          "validated",
		Severity:         "high",
		Confidence:       "medium",
		ValidationStatus: "validated",
		State:            testStateOpen,
		ValidationJSON:   `{"status":"validated","summary":"confirmed"}`,
	}
	if err := s.UpsertFinding(ctx, initial); err != nil {
		t.Fatalf("UpsertFinding(initial): %v", err)
	}

	lowerStatus := *initial
	lowerStatus.ScanRunID = testScanRunID2
	lowerStatus.ValidationStatus = "failed"
	lowerStatus.ValidationJSON = `{"status":"failed","summary":"later failure"}`
	if err := s.UpsertFinding(ctx, &lowerStatus); err != nil {
		t.Fatalf("UpsertFinding(lowerStatus): %v", err)
	}

	got, err := s.GetFinding(ctx, "ns1", initial.ID)
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if got.ValidationStatus != assessmentStatusValidated {
		t.Fatalf("ValidationStatus = %q, want validated", got.ValidationStatus)
	}
	if got.ValidationJSON != initial.ValidationJSON {
		t.Fatalf("ValidationJSON = %q, want %q", got.ValidationJSON, initial.ValidationJSON)
	}
}

func TestUpsertFindingAllowsPatchPendingToReturnOpen(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	initial := &store.Finding{
		ID:               "fnd-patch-pending",
		Namespace:        "ns1",
		RepositoryScan:   "repo1",
		ScanRunID:        "scan-1",
		Fingerprint:      "repo1:file.go:patch-pending",
		Title:            "Finding",
		Summary:          "patch pending",
		Severity:         "high",
		Confidence:       "medium",
		ValidationStatus: "validated",
		State:            "patch_pending",
		PatchProposalID:  "patch-123",
	}
	if err := s.UpsertFinding(ctx, initial); err != nil {
		t.Fatalf("UpsertFinding(initial): %v", err)
	}

	open := *initial
	open.ScanRunID = testScanRunID2
	open.State = testStateOpen
	if err := s.UpsertFinding(ctx, &open); err != nil {
		t.Fatalf("UpsertFinding(open): %v", err)
	}

	got, err := s.GetFinding(ctx, "ns1", initial.ID)
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if got.State != testStateOpen {
		t.Fatalf("State = %q, want open", got.State)
	}
}

func TestUpsertFindingPreservesFinalStatesOverOpen(t *testing.T) {
	for _, finalState := range []string{"fixed", "resolved", "dismissed", "suppressed", "false_positive"} {
		t.Run(finalState, func(t *testing.T) {
			s := setupTestStore(t)
			ctx := context.Background()

			initial := &store.Finding{
				ID:               "fnd-" + finalState,
				Namespace:        "ns1",
				RepositoryScan:   "repo1",
				ScanRunID:        "scan-1",
				Fingerprint:      "repo1:file.go:" + finalState,
				Title:            "Finding",
				Summary:          "final state",
				Severity:         "high",
				Confidence:       "medium",
				ValidationStatus: "validated",
				State:            finalState,
			}
			if err := s.UpsertFinding(ctx, initial); err != nil {
				t.Fatalf("UpsertFinding(initial): %v", err)
			}

			reopened := *initial
			reopened.ScanRunID = testScanRunID2
			reopened.State = testStateOpen
			if err := s.UpsertFinding(ctx, &reopened); err != nil {
				t.Fatalf("UpsertFinding(reopened): %v", err)
			}

			got, err := s.GetFinding(ctx, "ns1", initial.ID)
			if err != nil {
				t.Fatalf("GetFinding: %v", err)
			}
			if got.State != finalState {
				t.Fatalf("State = %q, want %q", got.State, finalState)
			}
		})
	}
}

func TestClearFindingPatchProjectionResetsPatchBackedStateAndMetadata(t *testing.T) {
	for _, state := range []string{"patch_pending", "patch_ready", testFindingStatePROpen} {
		t.Run(state, func(t *testing.T) {
			s := setupTestStore(t)
			ctx := context.Background()
			prNumber := 123
			finding := &store.Finding{
				ID:               "fnd-clear-" + state,
				Namespace:        "ns1",
				RepositoryScan:   "repo1",
				ScanRunID:        "scan-1",
				Fingerprint:      "repo1:file.go:clear-" + state,
				Title:            "Finding",
				Summary:          "stale patch projection",
				Severity:         "high",
				Confidence:       "medium",
				ValidationStatus: assessmentStatusValidated,
				State:            state,
				PatchProposalID:  "patch-123",
				PRNumber:         &prNumber,
				PRURL:            "https://github.com/example/repo/pull/123",
			}
			if err := s.UpsertFinding(ctx, finding); err != nil {
				t.Fatalf("UpsertFinding: %v", err)
			}

			if err := s.ClearFindingPatchProjection(ctx, finding.Namespace, finding.ID, finding.PatchProposalID); err != nil {
				t.Fatalf("ClearFindingPatchProjection: %v", err)
			}

			got, err := s.GetFinding(ctx, finding.Namespace, finding.ID)
			if err != nil {
				t.Fatalf("GetFinding: %v", err)
			}
			if got.State != testStateOpen {
				t.Fatalf("State = %q, want %q", got.State, testStateOpen)
			}
			if got.PatchProposalID != "" {
				t.Fatalf("PatchProposalID = %q, want empty", got.PatchProposalID)
			}
			if got.PRNumber != nil {
				t.Fatalf("PRNumber = %#v, want nil", got.PRNumber)
			}
			if got.PRURL != "" {
				t.Fatalf("PRURL = %q, want empty", got.PRURL)
			}
		})
	}
}

func TestClearFindingPatchProjectionPreservesUnrelatedLifecycleState(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	prNumber := 123
	finding := &store.Finding{
		ID:               "fnd-clear-dismissed",
		Namespace:        "ns1",
		RepositoryScan:   "repo1",
		ScanRunID:        "scan-1",
		Fingerprint:      "repo1:file.go:clear-dismissed",
		Title:            "Finding",
		Summary:          "stale patch projection on dismissed finding",
		Severity:         "high",
		Confidence:       "medium",
		ValidationStatus: assessmentStatusValidated,
		State:            "dismissed",
		PatchProposalID:  "patch-123",
		PRNumber:         &prNumber,
		PRURL:            "https://github.com/example/repo/pull/123",
	}
	if err := s.UpsertFinding(ctx, finding); err != nil {
		t.Fatalf("UpsertFinding: %v", err)
	}

	if err := s.ClearFindingPatchProjection(ctx, finding.Namespace, finding.ID, finding.PatchProposalID); err != nil {
		t.Fatalf("ClearFindingPatchProjection: %v", err)
	}

	got, err := s.GetFinding(ctx, finding.Namespace, finding.ID)
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if got.State != finding.State {
		t.Fatalf("State = %q, want %q", got.State, finding.State)
	}
	if got.PatchProposalID != "" || got.PRNumber != nil || got.PRURL != "" {
		t.Fatalf("patch projection = (%q, %#v, %q), want cleared", got.PatchProposalID, got.PRNumber, got.PRURL)
	}
}

func TestReviewSliceStoreRoundTripFilteringAndNamespaceIsolation(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	slice := &store.ReviewSlice{
		ID:              "slice_repo1_api",
		Namespace:       "ns1",
		RepositoryScan:  "repo1",
		Source:          "deterministic-go-package",
		Title:           "Go package internal/api",
		Summary:         "API handlers",
		Kind:            "package",
		Confidence:      "high",
		Status:          "pending",
		LastScanRunID:   "scan-current",
		Entrypoints:     []store.ReviewSliceFile{{Path: "internal/api/security.go", Reason: "handler"}},
		OwnedFiles:      []store.ReviewSliceFile{{Path: "internal/api/security.go", Reason: "source"}},
		ContextFiles:    []store.ReviewSliceFile{{Path: "internal/api/security_test.go", Reason: "tests"}},
		Tests:           []store.ReviewSliceTest{{Path: "internal/api/security_test.go", Command: "go test ./internal/api"}},
		Tags:            []string{"language:go"},
		TrustBoundaries: []string{"network"},
	}
	if err := s.UpsertReviewSlice(ctx, slice); err != nil {
		t.Fatalf("UpsertReviewSlice() error = %v", err)
	}

	got, err := s.GetReviewSlice(ctx, "ns1", "repo1", "slice_repo1_api")
	if err != nil {
		t.Fatalf("GetReviewSlice() error = %v", err)
	}
	if got.Title != slice.Title || len(got.OwnedFiles) != 1 || got.OwnedFiles[0].Path != "internal/api/security.go" {
		t.Fatalf("GetReviewSlice() = %#v, want JSON fields round-tripped", got)
	}

	if err := s.UpdateReviewSliceStatus(ctx, "ns1", "repo1", "slice_repo1_api", "scan-stale", "reviewed"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateReviewSliceStatus(stale run) error = %v, want not found", err)
	}
	if err := s.UpdateReviewSliceStatus(ctx, "ns1", "repo1", "slice_repo1_api", "scan-current", "reviewed"); err != nil {
		t.Fatalf("UpdateReviewSliceStatus() error = %v", err)
	}
	reviewed, _, err := s.ListReviewSlices(ctx, store.ReviewSliceFilter{
		Namespace:      "ns1",
		RepositoryScan: "repo1",
		Status:         "reviewed",
		LastScanRunID:  "scan-current",
	})
	if err != nil {
		t.Fatalf("ListReviewSlices(reviewed) error = %v", err)
	}
	if len(reviewed) != 1 || reviewed[0].LastReviewedAt == nil {
		t.Fatalf("reviewed slices = %#v, want reviewed slice with timestamp", reviewed)
	}
	staleRun, _, err := s.ListReviewSlices(ctx, store.ReviewSliceFilter{
		Namespace:      "ns1",
		RepositoryScan: "repo1",
		Status:         "reviewed",
		LastScanRunID:  "scan-stale",
	})
	if err != nil {
		t.Fatalf("ListReviewSlices(stale run) error = %v", err)
	}
	if len(staleRun) != 0 {
		t.Fatalf("ListReviewSlices(stale run) = %#v, want run isolation", staleRun)
	}

	otherNamespace, _, err := s.ListReviewSlices(ctx, store.ReviewSliceFilter{
		Namespace:      "ns2",
		RepositoryScan: "repo1",
	})
	if err != nil {
		t.Fatalf("ListReviewSlices(ns2) error = %v", err)
	}
	if len(otherNamespace) != 0 {
		t.Fatalf("ListReviewSlices(ns2) = %#v, want namespace isolation", otherNamespace)
	}
}

func TestDroppedFindingStoreRoundTripFiltering(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	first := &store.DroppedFinding{
		ID:             "drop1",
		Namespace:      "ns1",
		RepositoryScan: "repo1",
		ScanRunID:      "scan1",
		TaskName:       "task1",
		SliceID:        "slice1",
		Reason:         "evidence file was not included in review context",
		SampleJSON:     `{"title":"bad"}`,
	}
	if err := s.CreateDroppedFinding(ctx, first); err != nil {
		t.Fatalf("CreateDroppedFinding(first) error = %v", err)
	}
	if err := s.CreateDroppedFinding(ctx, &store.DroppedFinding{
		ID:             "drop2",
		Namespace:      "ns1",
		RepositoryScan: "repo1",
		ScanRunID:      "scan2",
		TaskName:       "task2",
		Reason:         "missing evidence",
	}); err != nil {
		t.Fatalf("CreateDroppedFinding(second) error = %v", err)
	}

	got, _, err := s.ListDroppedFindings(ctx, store.DroppedFindingFilter{
		Namespace:      "ns1",
		RepositoryScan: "repo1",
		ScanRunID:      "scan1",
	})
	if err != nil {
		t.Fatalf("ListDroppedFindings() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "drop1" || got[0].SampleJSON != first.SampleJSON {
		t.Fatalf("ListDroppedFindings() = %#v, want scan1 diagnostic", got)
	}
}

func TestFailedValidationExcludesFindingFromRecommendedFilter(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	items := []*store.Finding{
		{ID: "f_failed", Namespace: "ns1", RepositoryScan: "repo1", ScanRunID: "scan1", Fingerprint: "fp-failed", Title: "failed", Summary: "failed", Severity: "critical", Confidence: "high", ValidationStatus: "failed", State: "open"},
		{ID: "f_open", Namespace: "ns1", RepositoryScan: "repo1", ScanRunID: "scan1", Fingerprint: "fp-open", Title: "open", Summary: "open", Severity: "high", Confidence: "high", ValidationStatus: "unvalidated", State: "open"},
	}
	for _, item := range items {
		if err := s.UpsertFinding(ctx, item); err != nil {
			t.Fatalf("UpsertFinding(%s) error = %v", item.ID, err)
		}
	}
	got, _, err := s.ListFindings(ctx, store.FindingFilter{Namespace: "ns1", RepositoryScan: "repo1", Recommended: true})
	if err != nil {
		t.Fatalf("ListFindings(recommended) error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "f_open" {
		t.Fatalf("recommended findings = %#v, want only unvalidated open finding", got)
	}
}

func TestValidatedFindingsRankHigher(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	items := []*store.Finding{
		{ID: "f_unvalidated", Namespace: "ns1", RepositoryScan: "repo1", ScanRunID: "scan1", Fingerprint: "fp-unvalidated", Title: "unvalidated", Summary: "unvalidated", Severity: "high", Confidence: "high", ValidationStatus: "unvalidated", State: "open"},
		{ID: "f_validated", Namespace: "ns1", RepositoryScan: "repo1", ScanRunID: "scan1", Fingerprint: "fp-validated", Title: "validated", Summary: "validated", Severity: "high", Confidence: "medium", ValidationStatus: "validated", State: "open"},
	}
	for _, item := range items {
		if err := s.UpsertFinding(ctx, item); err != nil {
			t.Fatalf("UpsertFinding(%s) error = %v", item.ID, err)
		}
	}
	got, _, err := s.ListFindings(ctx, store.FindingFilter{Namespace: "ns1", RepositoryScan: "repo1", Recommended: true})
	if err != nil {
		t.Fatalf("ListFindings(recommended) error = %v", err)
	}
	if len(got) != 2 || got[0].ID != "f_validated" {
		t.Fatalf("recommended findings order = %#v, want validated first", got)
	}
}

func TestUpsertFindingReplacesScanRunAndOccurrenceBindingAtomically(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	finding := &store.Finding{
		ID:                       "fnd_atomic_occurrence_binding",
		Namespace:                "ns1",
		RepositoryScan:           "repo1",
		ScanRunID:                "scan-1",
		Fingerprint:              "compat-atomic-occurrence-binding",
		IdentityQuality:          store.IdentityQualityProducerProposed,
		IdentityAlgorithmVersion: "producer-v1",
		HistoryStatus:            store.FindingHistoryLegacyUnrebuildable,
		CurrentOccurrenceID:      "occ_old",
		Title:                    "Finding",
		Summary:                  "Summary",
		Severity:                 "high",
		Confidence:               "high",
		ValidationStatus:         "unvalidated",
		State:                    testStateOpen,
	}
	if err := s.UpsertFinding(ctx, finding); err != nil {
		t.Fatal(err)
	}

	newRun := *finding
	newRun.ScanRunID = "scan-2"
	newRun.CurrentOccurrenceID = ""
	if err := s.UpsertFinding(ctx, &newRun); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetFinding(ctx, finding.Namespace, finding.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ScanRunID != newRun.ScanRunID || got.CurrentOccurrenceID != "" {
		t.Fatalf("source binding = (%q, %q), want (%q, empty)", got.ScanRunID, got.CurrentOccurrenceID, newRun.ScanRunID)
	}

	newOccurrence := newRun
	newOccurrence.CurrentOccurrenceID = "occ_new"
	if err := s.UpsertFinding(ctx, &newOccurrence); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetFinding(ctx, finding.Namespace, finding.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ScanRunID != newOccurrence.ScanRunID || got.CurrentOccurrenceID != newOccurrence.CurrentOccurrenceID {
		t.Fatalf("source binding = (%q, %q), want (%q, %q)", got.ScanRunID, got.CurrentOccurrenceID,
			newOccurrence.ScanRunID, newOccurrence.CurrentOccurrenceID)
	}
}

func TestUpsertFindingSanitizesPersistedProjectionStrings(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	redactionValue := strings.Join([]string{"github", "_pat_", "abcdefghijklmnopqrstuvwxyz1234567890"}, "")
	finding := &store.Finding{
		ID:                            "fnd_projection_redaction",
		Namespace:                     "ns1",
		RepositoryScan:                "repo1",
		ScanRunID:                     "scan-1",
		Fingerprint:                   "compat-projection-redaction",
		Title:                         "title token=" + redactionValue,
		Category:                      "category token=" + redactionValue,
		Summary:                       "summary token=" + redactionValue,
		Severity:                      "high",
		Confidence:                    "high",
		Triage:                        "triage token=" + redactionValue,
		ValidationStatus:              "validated",
		State:                         testStateOpen,
		FilePath:                      "internal/api/handler.go",
		CommitSHA:                     strings.Repeat("a", 40),
		RootCause:                     "root token=" + redactionValue,
		Reproduction:                  "reproduction token=" + redactionValue,
		Remediation:                   "remediation token=" + redactionValue,
		SuggestedAction:               "action token=" + redactionValue,
		WhyTestsDoNotAlreadyCoverThis: "coverage token=" + redactionValue,
		SuggestedRegressionTest:       "regression token=" + redactionValue,
		MinimumFixScope:               "scope token=" + redactionValue,
		ValidationJSON: fmt.Sprintf(
			`{"status":"validated","%s":"%s"}`,
			strings.Join([]string{"to", "ken"}, ""), redactionValue,
		),
		PRURL: "https://github.com/example/repo/pull/1",
		Evidence: []store.FindingEvidenceRef{{
			Kind:  "file",
			Path:  "internal/api/handler.go",
			Label: "label token=" + redactionValue,
			Quote: "quote token=" + redactionValue,
		}},
	}
	if err := s.UpsertFinding(ctx, finding); err != nil {
		t.Fatalf("UpsertFinding() error = %v", err)
	}
	got, err := s.GetFinding(ctx, finding.Namespace, finding.ID)
	if err != nil {
		t.Fatal(err)
	}
	persisted := fmt.Sprintf("%#v", got)
	if strings.Contains(persisted, redactionValue) {
		t.Fatalf("persisted finding retained credential: %s", persisted)
	}
	for name, value := range map[string]string{
		"title": got.Title, "category": got.Category, "summary": got.Summary, "triage": got.Triage,
		"rootCause": got.RootCause, "reproduction": got.Reproduction, "remediation": got.Remediation,
		"suggestedAction": got.SuggestedAction, "whyTests": got.WhyTestsDoNotAlreadyCoverThis,
		"regressionTest": got.SuggestedRegressionTest, "minimumScope": got.MinimumFixScope,
		"validationJSON": got.ValidationJSON, "evidenceLabel": got.Evidence[0].Label, "evidenceQuote": got.Evidence[0].Quote,
	} {
		if !strings.Contains(value, "[REDACTED]") {
			t.Fatalf("%s = %q, want redaction", name, value)
		}
	}
}

func TestUpsertFindingRejectsCredentialBearingProjectionCoordinates(t *testing.T) {
	redactionValue := strings.Join([]string{"github", "_pat_", "abcdefghijklmnopqrstuvwxyz1234567890"}, "")
	base := func() *store.Finding {
		return &store.Finding{
			ID: "fnd_projection_reject", Namespace: "ns1", RepositoryScan: "repo1", ScanRunID: "scan-1",
			Fingerprint: "compat-projection-reject", Title: "Finding", Summary: "Summary", Severity: "high",
			Confidence: "high", ValidationStatus: "unvalidated", State: testStateOpen,
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*store.Finding)
	}{
		{name: "file path", mutate: func(f *store.Finding) { f.FilePath = "internal/token=" + redactionValue }},
		{name: "PR URL", mutate: func(f *store.Finding) { f.PRURL = "https://user:" + redactionValue + "@example.com/pull/1" }},
		{name: "patch proposal ID", mutate: func(f *store.Finding) { f.PatchProposalID = redactionValue }},
		{name: "evidence path", mutate: func(f *store.Finding) {
			f.Evidence = []store.FindingEvidenceRef{{Kind: "file", Path: "internal/token=" + redactionValue}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupTestStore(t)
			finding := base()
			tc.mutate(finding)
			if err := s.UpsertFinding(context.Background(), finding); !errors.Is(err, store.ErrValidation) {
				t.Fatalf("UpsertFinding() error = %v, want ErrValidation", err)
			}
		})
	}
}

func TestUpdateScanRunIgnoresStaleTerminalReactivationAfterNewerRun(t *testing.T) {
	for _, tt := range []struct {
		name             string
		completeNewerRun bool
	}{
		{name: "newer run active"},
		{name: "newer run completed", completeNewerRun: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := setupTestStore(t)
			ctx := context.Background()
			first := &store.ScanRun{
				ID: "scan-z-first", RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Namespace: "ns", RepositoryScan: "repo", RepositoryScanUID: "repo-uid", RepositoryScanGeneration: 1,
				TaskName: "task-first", Mode: "initial", Phase: storedScanRunPhasePending,
				RequestIdempotencyKey: "req-first", IdempotencyKey: "req-first",
				StartedAt: time.Now().UTC(), Quality: store.LegacyScanQuality(),
			}
			if err := s.CreateScanRun(ctx, first); err != nil {
				t.Fatalf("CreateScanRun(first) error = %v", err)
			}
			stale, err := s.GetScanRun(ctx, first.Namespace, first.ID)
			if err != nil {
				t.Fatal(err)
			}
			completed, err := s.GetScanRun(ctx, first.Namespace, first.ID)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			completed.Phase = storedScanRunPhaseSucceeded
			completed.CompletedAt = &now
			completed.Summary = "completed"
			completed.Quality.BundleStatus = store.BundleStatusSealing
			if err := s.UpdateScanRun(ctx, completed); err != nil {
				t.Fatalf("UpdateScanRun(first sealing) error = %v", err)
			}
			completed.Quality.BundleStatus = store.BundleStatusSealed
			if err := s.UpdateScanRun(ctx, completed); err != nil {
				t.Fatalf("UpdateScanRun(first sealed) error = %v", err)
			}
			second := &store.ScanRun{
				ID: "scan-a-second", RunUID: "run_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				Namespace: first.Namespace, RepositoryScan: first.RepositoryScan, RepositoryScanUID: first.RepositoryScanUID,
				RepositoryScanGeneration: first.RepositoryScanGeneration, TaskName: "task-second", Mode: "manual",
				Phase: storedScanRunPhasePending, RequestIdempotencyKey: "req-second", IdempotencyKey: "req-second",
				StartedAt: now.Add(-time.Hour), Quality: store.LegacyScanQuality(),
			}
			if err := s.CreateScanRun(ctx, second); err != nil {
				t.Fatalf("CreateScanRun(second) error = %v", err)
			}
			wantSecondPhase := storedScanRunPhasePending
			if tt.completeNewerRun {
				newerCompleted := now.Add(2 * time.Second)
				second.Phase = storedScanRunPhaseSucceeded
				second.CompletedAt = &newerCompleted
				if err := s.UpdateScanRun(ctx, second); err != nil {
					t.Fatalf("UpdateScanRun(second terminal) error = %v", err)
				}
				wantSecondPhase = storedScanRunPhaseSucceeded
			}
			stale.Phase = storedScanRunPhaseRunning
			stale.CompletedAt = nil
			stale.Summary = "stale replay"
			if err := s.UpdateScanRun(ctx, stale); err != nil {
				t.Fatalf("UpdateScanRun(stale terminal replay) error = %v", err)
			}
			gotFirst, err := s.GetScanRun(ctx, first.Namespace, first.ID)
			if err != nil {
				t.Fatal(err)
			}
			if gotFirst.Phase != storedScanRunPhaseSucceeded || gotFirst.Summary != "completed" || gotFirst.CompletedAt == nil ||
				gotFirst.Quality.BundleStatus != store.BundleStatusSealed {
				t.Fatalf("first run after stale replay = %#v", gotFirst)
			}
			gotSecond, err := s.GetScanRun(ctx, second.Namespace, second.ID)
			if err != nil {
				t.Fatal(err)
			}
			if gotSecond.Phase != wantSecondPhase {
				t.Fatalf("second run phase = %q, want %q", gotSecond.Phase, wantSecondPhase)
			}
		})
	}
}
