package controller

import (
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/security"
	storepkg "github.com/orka-agents/orka/internal/store"
)

const coverageOwnedFile = "internal/api/security.go"

func (f *reviewResultRetryFixture) emptyReview(reviewedFiles ...string) security.FindingsV2Artifact {
	return security.FindingsV2Artifact{
		SchemaVersion: security.SchemaVersionFindingsV2,
		Repository:    security.FindingsV2Repository{RepoURL: f.scan.Spec.RepoURL, Branch: "main"},
		Scan: security.FindingsV2Scan{
			Mode: f.run.Mode, SliceID: f.slice.ID, Summary: "...", ReviewedFiles: reviewedFiles,
		},
		Findings: []security.FindingsV2Finding{},
	}
}

func (f *reviewResultRetryFixture) completeTask(t *testing.T, task *corev1alpha1.Task, findings security.FindingsV2Artifact) {
	t.Helper()
	saveFindingsTaskResult(t, f.store, task, f.scan.Name, f.run.ID, f.run.PolicyDigest, f.slice.ReviewContextHash, f.slice.ID, findings)
	task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	task.Status.CompletionTime = &metav1.Time{Time: time.Now()}
	if err := f.client.Update(f.ctx, task); err != nil {
		t.Fatalf("Update(%s) error = %v", task.Name, err)
	}
}

func (f *reviewResultRetryFixture) storedSlice(t *testing.T) *storepkg.ReviewSlice {
	t.Helper()
	reviewSlice, err := f.store.GetReviewSlice(f.ctx, defaultNS, f.scan.Name, f.slice.ID)
	if err != nil {
		t.Fatalf("GetReviewSlice() error = %v", err)
	}
	return reviewSlice
}

func (f *reviewResultRetryFixture) storedRun(t *testing.T) *storepkg.ScanRun {
	t.Helper()
	run, err := f.store.GetScanRun(f.ctx, defaultNS, f.run.ID)
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	return run
}

// An empty result that names none of its trusted context looks exactly like
// the echoed result template. It gets the one retry; when the retry shows
// nothing either, the slice counts as reviewed with no coverage.
func TestIngestReviewTaskRetriesAnEchoedEmptyTemplate(t *testing.T) {
	fixture := newReviewResultRetryFixture(t)
	fixture.completeTask(t, fixture.sourceTask, fixture.emptyReview())
	if err := fixture.reconciler.ingestScanTask(fixture.ctx, fixture.scan, fixture.sourceTask); err != nil {
		t.Fatalf("ingestScanTask(source) error = %v", err)
	}
	retryTask := fixture.getRetryTask(t)
	if !strings.Contains(retryTask.Spec.Prompt, "set scan.reviewedFiles to the repository files you actually read") {
		t.Fatalf("retry prompt does not ask for coverage: %q", retryTask.Spec.Prompt)
	}
	if run := fixture.storedRun(t); run.Phase != scanRunPhaseRunning || !strings.Contains(run.Summary, "no coverage evidence") {
		t.Fatalf("run after coverage retry = %#v, want running with the coverage cause", run)
	}
	if reviewSlice := fixture.storedSlice(t); reviewSlice.Status != reviewSliceStatusPending || reviewSlice.Coverage != "" {
		t.Fatalf("slice during retry = %q/%q, want pending without coverage", reviewSlice.Status, reviewSlice.Coverage)
	}

	fixture.completeTask(t, retryTask, fixture.emptyReview("README.md"))
	if err := fixture.reconciler.ingestOwnedTasks(fixture.ctx, fixture.scan); err != nil {
		t.Fatalf("ingestOwnedTasks() error = %v", err)
	}
	if run := fixture.storedRun(t); run.Phase != scanRunPhaseSucceeded || run.ReviewedSliceCount != 1 {
		t.Fatalf("run after empty retry = %#v, want one reviewed slice", run)
	}
	reviewSlice := fixture.storedSlice(t)
	if reviewSlice.Status != reviewSliceStatusReviewed || reviewSlice.Coverage != security.ReviewCoverageNone || len(reviewSlice.ReviewedFiles) != 0 {
		t.Fatalf("slice after empty retry = %q/%q/%v, want reviewed with no coverage", reviewSlice.Status, reviewSlice.Coverage, reviewSlice.ReviewedFiles)
	}
	resolution, err := fixture.reconciler.repositoryScanReviewedSlicesForResolution(fixture.ctx, fixture.scan, fixture.run)
	if err != nil {
		t.Fatalf("repositoryScanReviewedSlicesForResolution() error = %v", err)
	}
	if ranges, ok := resolution[fixture.slice.ID]; !ok || len(ranges) != 0 {
		t.Fatalf("resolution ranges = %#v, want the slice with no reviewed file", resolution)
	}
}

// A zero-finding review that corroborates every owned file is a legitimate
// zero: no retry, complete coverage, and an idempotent receipt replay.
func TestIngestReviewTaskRecordsCoverageOfALegitimateZero(t *testing.T) {
	fixture := newReviewResultRetryFixture(t)
	fixture.completeTask(t, fixture.sourceTask, fixture.emptyReview(coverageOwnedFile, "README.md"))
	for attempt := range 2 {
		if err := fixture.reconciler.ingestScanTask(fixture.ctx, fixture.scan, fixture.sourceTask); err != nil {
			t.Fatalf("ingestScanTask(source, attempt %d) error = %v", attempt, err)
		}
		retry := &corev1alpha1.Task{}
		if err := fixture.client.Get(fixture.ctx, client.ObjectKey{Namespace: defaultNS, Name: fixture.retryTaskName()}, retry); !apierrors.IsNotFound(err) {
			t.Fatalf("Get(retry Task) error = %v, want no retry for a covered zero", err)
		}
		if run := fixture.storedRun(t); run.Phase != scanRunPhaseSucceeded || run.ReviewedSliceCount != 1 {
			t.Fatalf("run after attempt %d = %#v, want one reviewed slice", attempt, run)
		}
		reviewSlice := fixture.storedSlice(t)
		if reviewSlice.Status != reviewSliceStatusReviewed || reviewSlice.Coverage != security.ReviewCoverageComplete ||
			!slices.Equal(reviewSlice.ReviewedFiles, []string{coverageOwnedFile}) || reviewSlice.LastReviewedAt == nil {
			t.Fatalf("slice after attempt %d = %#v, want reviewed with complete coverage of %s", attempt, reviewSlice, coverageOwnedFile)
		}
	}
}

// A review that corroborates only part of its slice keeps its result, and only
// the files it corroborated can resolve a finding it no longer reports.
func TestIngestReviewTaskLimitsResolutionToCorroboratedFiles(t *testing.T) {
	fixture := newReviewResultRetryFixture(t)
	fixture.slice.OwnedFiles = append(fixture.slice.OwnedFiles, storepkg.ReviewSliceFile{Path: "internal/api/auth.go", Reason: "source"})
	bindReviewSliceContext(t, fixture.slice)
	if err := fixture.store.UpsertReviewSlice(fixture.ctx, fixture.slice); err != nil {
		t.Fatalf("UpsertReviewSlice() error = %v", err)
	}
	fixture.completeTask(t, fixture.sourceTask, fixture.emptyReview(coverageOwnedFile))
	if err := fixture.reconciler.ingestScanTask(fixture.ctx, fixture.scan, fixture.sourceTask); err != nil {
		t.Fatalf("ingestScanTask(source) error = %v", err)
	}
	reviewSlice := fixture.storedSlice(t)
	if reviewSlice.Status != reviewSliceStatusReviewed || reviewSlice.Coverage != security.ReviewCoveragePartial ||
		!slices.Equal(reviewSlice.ReviewedFiles, []string{coverageOwnedFile}) {
		t.Fatalf("slice = %#v, want reviewed with partial coverage of %s", reviewSlice, coverageOwnedFile)
	}

	resolution, err := fixture.reconciler.repositoryScanReviewedSlicesForResolution(fixture.ctx, fixture.scan, fixture.run)
	if err != nil {
		t.Fatalf("repositoryScanReviewedSlicesForResolution() error = %v", err)
	}
	ranges := resolution[fixture.slice.ID]
	if _, ok := ranges[coverageOwnedFile]; !ok || len(ranges) != 1 {
		t.Fatalf("resolution ranges = %#v, want only %s", ranges, coverageOwnedFile)
	}
	prNumber := 7
	earlierRun := &storepkg.ScanRun{ID: "scan_earlier"}
	for _, tt := range []struct {
		path string
		want bool
	}{
		{path: coverageOwnedFile, want: true},
		{path: "internal/api/auth.go", want: false},
	} {
		finding := &storepkg.Finding{ScanRunID: earlierRun.ID, SliceID: fixture.slice.ID, FilePath: tt.path, Line: 10, PRNumber: &prNumber}
		if got := findingEligibleForMergedResolution(finding, fixture.run, nil, resolution, false); got != tt.want {
			t.Fatalf("eligible(%s) = %t, want %t", tt.path, got, tt.want)
		}
	}
}

// A slice reviewed before coverage was recorded resolves nothing: its review
// never said which files it read.
func TestReviewedSlicesWithoutRecordedCoverageResolveNothing(t *testing.T) {
	fixture := newReviewResultRetryFixture(t)
	fixture.slice.Status = reviewSliceStatusReviewed
	if err := fixture.store.UpsertReviewSlice(fixture.ctx, fixture.slice); err != nil {
		t.Fatalf("UpsertReviewSlice() error = %v", err)
	}
	resolution, err := fixture.reconciler.repositoryScanReviewedSlicesForResolution(fixture.ctx, fixture.scan, fixture.run)
	if err != nil {
		t.Fatalf("repositoryScanReviewedSlicesForResolution() error = %v", err)
	}
	prNumber := 7
	finding := &storepkg.Finding{ScanRunID: "scan_earlier", SliceID: fixture.slice.ID, FilePath: coverageOwnedFile, Line: 10, PRNumber: &prNumber}
	if findingEligibleForMergedResolution(finding, fixture.run, nil, resolution, false) {
		t.Fatalf("a slice without recorded coverage made %s eligible for resolution: %#v", coverageOwnedFile, resolution)
	}
}

// A slice with no readable trusted context has nothing a retry could
// corroborate, so its empty review is recorded without one.
func TestIngestReviewTaskDoesNotRetryASliceWithoutReadableContext(t *testing.T) {
	fixture := newReviewResultRetryFixture(t)
	fixture.slice.OwnedFiles = nil
	bindReviewSliceContext(t, fixture.slice)
	if err := fixture.store.UpsertReviewSlice(fixture.ctx, fixture.slice); err != nil {
		t.Fatalf("UpsertReviewSlice() error = %v", err)
	}
	fixture.completeTask(t, fixture.sourceTask, fixture.emptyReview())
	if err := fixture.reconciler.ingestScanTask(fixture.ctx, fixture.scan, fixture.sourceTask); err != nil {
		t.Fatalf("ingestScanTask(source) error = %v", err)
	}
	retry := &corev1alpha1.Task{}
	if err := fixture.client.Get(fixture.ctx, client.ObjectKey{Namespace: defaultNS, Name: fixture.retryTaskName()}, retry); !apierrors.IsNotFound(err) {
		t.Fatalf("Get(retry Task) error = %v, want no retry without readable context", err)
	}
	if reviewSlice := fixture.storedSlice(t); reviewSlice.Status != reviewSliceStatusReviewed || reviewSlice.Coverage != security.ReviewCoverageNone {
		t.Fatalf("slice = %q/%q, want reviewed with no coverage", reviewSlice.Status, reviewSlice.Coverage)
	}
}
