/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/security"
	storepkg "github.com/sozercan/orka/internal/store"
	sqlitestore "github.com/sozercan/orka/internal/store/sqlite"
	"github.com/sozercan/orka/workers/common"
)

const readyReasonScanFailed = "ScanFailed"

func TestRepositoryScanConditionMessageUsesFallback(t *testing.T) {
	got := repositoryScanConditionMessage("  \n\t ", "scan completed successfully")
	if got != "scan completed successfully" {
		t.Fatalf("repositoryScanConditionMessage() = %q, want fallback", got)
	}
}

func TestRepositoryScanConditionMessageTruncatesToKubernetesLimit(t *testing.T) {
	longMessage := strings.Repeat("世", repositoryScanConditionMessageLimit)

	got := repositoryScanConditionMessage(longMessage, "fallback")

	if len(got) > repositoryScanConditionMessageLimit {
		t.Fatalf("len(message) = %d, want <= %d", len(got), repositoryScanConditionMessageLimit)
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncated message is not valid UTF-8")
	}
	if !strings.HasSuffix(got, repositoryScanConditionMessageSuffix) {
		t.Fatalf("message suffix = %q, want %q", got[len(got)-len(repositoryScanConditionMessageSuffix):], repositoryScanConditionMessageSuffix)
	}
}

func TestLatestTerminalScanTaskPrefersNewestCompletedScan(t *testing.T) {
	tasks := []corev1alpha1.Task{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "older-failed-scan",
				CreationTimestamp: metav1.NewTime(mustParseTime(t, "2026-04-10T04:45:33Z")),
				Labels: map[string]string{
					labels.LabelSecurityTarget: "kaset",
				},
			},
			Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseFailed},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "patch-task",
				CreationTimestamp: metav1.NewTime(mustParseTime(t, "2026-04-10T04:58:00Z")),
				Labels: map[string]string{
					labels.LabelSecurityTarget:    "kaset",
					labels.LabelSecurityFindingID: "fnd_123",
				},
			},
			Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "newest-succeeded-scan",
				CreationTimestamp: metav1.NewTime(mustParseTime(t, "2026-04-10T04:59:05Z")),
				Labels: map[string]string{
					labels.LabelSecurityTarget: "kaset",
				},
			},
			Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "running-scan",
				CreationTimestamp: metav1.NewTime(mustParseTime(t, "2026-04-10T05:00:00Z")),
				Labels: map[string]string{
					labels.LabelSecurityTarget: "kaset",
				},
			},
			Status: corev1alpha1.TaskStatus{},
		},
	}

	got := latestTerminalScanTask(tasks)
	if got == nil {
		t.Fatal("latestTerminalScanTask() = nil, want newest terminal scan task")
	}
	if got.Name != "newest-succeeded-scan" {
		t.Fatalf("latestTerminalScanTask() = %q, want %q", got.Name, "newest-succeeded-scan")
	}
}

func TestTrustedFindingsRepositoryScopesRefOnlyScan(t *testing.T) {
	run := &storepkg.ScanRun{
		BaseCommit: "base",
		HeadCommit: "head",
	}
	tests := []struct {
		name string
		spec corev1alpha1.RepositoryScanSpec
		want string
	}{
		{
			name: "implicit main",
			spec: corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo"},
			want: "main",
		},
		{
			name: "explicit branch wins",
			spec: corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo", Branch: "release", Ref: "v1.2.3"},
			want: "release",
		},
		{
			name: "ref-only scan is ref scoped",
			spec: corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo", Ref: "refs/tags/v1.2.3"},
			want: "ref:refs/tags/v1.2.3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scan := &corev1alpha1.RepositoryScan{Spec: tt.spec}

			got := trustedFindingsRepository(scan, run)

			if got.Branch != tt.want {
				t.Fatalf("trustedFindingsRepository().Branch = %q, want %q", got.Branch, tt.want)
			}
			if got.BaseSHA != "base" || got.HeadSHA != "head" {
				t.Fatalf("trustedFindingsRepository() SHAs = %q/%q, want base/head", got.BaseSHA, got.HeadSHA)
			}
		})
	}
}

func TestLatestOwnedScanPipelineRunIDIgnoresPatchAndValidationTasks(t *testing.T) {
	tasks := []corev1alpha1.Task{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "kaset-manual-old",
				CreationTimestamp: metav1.NewTime(mustParseTime(t, "2026-04-10T05:00:00Z")),
				Labels: map[string]string{
					labels.LabelSecurityTarget: "kaset",
					labels.LabelSecurityScanID: "scan_old",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "kaset-validation-f1",
				CreationTimestamp: metav1.NewTime(mustParseTime(t, "2026-04-10T05:02:00Z")),
				Labels: map[string]string{
					labels.LabelSecurityTarget:    "kaset",
					labels.LabelSecurityScanID:    "scan_old",
					labels.LabelSecurityStage:     security.StageValidation,
					labels.LabelSecurityFindingID: "f1",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "kaset-manual-threat-model-new",
				CreationTimestamp: metav1.NewTime(mustParseTime(t, "2026-04-10T05:03:00Z")),
				Labels: map[string]string{
					labels.LabelSecurityTarget: "kaset",
					labels.LabelSecurityScanID: "scan_new",
					labels.LabelSecurityStage:  security.StageThreatModel,
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "kaset-patch-f1",
				CreationTimestamp: metav1.NewTime(mustParseTime(t, "2026-04-10T05:04:00Z")),
				Labels: map[string]string{
					labels.LabelSecurityTarget:    "kaset",
					labels.LabelSecurityScanID:    "scan_new",
					labels.LabelSecurityStage:     security.StagePatch,
					labels.LabelSecurityFindingID: "f1",
				},
			},
		},
	}

	if got := latestOwnedScanPipelineRunID(tasks); got != "scan_new" {
		t.Fatalf("latestOwnedScanPipelineRunID() = %q, want %q", got, "scan_new")
	}
}

func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()

	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("time.Parse(%q): %v", value, err)
	}
	return parsed
}

func newSucceededSecurityTask(name, scanID, stage string, completed metav1.Time) *corev1alpha1.Task {
	labelsMap := map[string]string{
		labels.LabelSecurityTarget: "kaset",
		labels.LabelSecurityScanID: scanID,
		labels.LabelSecurityStage:  stage,
	}
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: defaultNS,
			Labels:    labelsMap,
		},
		Status: corev1alpha1.TaskStatus{
			Phase:          corev1alpha1.TaskPhaseSucceeded,
			CompletionTime: &completed,
		},
	}
}

func TestIngestMapperTaskPersistsReviewSlices(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	reconciler := &RepositoryScanReconciler{
		SecurityStore: store,
		ArtifactStore: store,
	}
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kaset-mapper",
			Namespace: defaultNS,
			Labels: map[string]string{
				labels.LabelSecurityTarget: "kaset",
				labels.LabelSecurityScanID: "scan_mapper",
				labels.LabelSecurityMode:   "initial",
				labels.LabelSecurityStage:  security.StageMapper,
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
	}
	artifact := security.ReviewSlicesArtifact{
		SchemaVersion: security.SchemaVersionReviewSlices,
		Slices: []storepkg.ReviewSlice{{
			ID:             "slice_kaset_api",
			RepositoryScan: "kaset",
			Source:         "deterministic-go-package",
			Title:          "Go package internal/api",
			Summary:        "API handlers",
			Kind:           "package",
			OwnedFiles:     []storepkg.ReviewSliceFile{{Path: "internal/api/security.go", Reason: "source"}},
			Confidence:     "high",
			Status:         reviewSliceStatusPending,
		}},
	}
	data, err := json.Marshal(artifact)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if err := store.SaveArtifact(ctx, task.Namespace, task.Name, security.ArtifactSlices, "application/json", data); err != nil {
		t.Fatalf("SaveArtifact(slices) error = %v", err)
	}

	if err := reconciler.ingestScanTask(ctx, scan, task); err != nil {
		t.Fatalf("ingestScanTask() error = %v", err)
	}
	got, err := store.GetReviewSlice(ctx, defaultNS, "kaset", "slice_kaset_api")
	if err != nil {
		t.Fatalf("GetReviewSlice() error = %v", err)
	}
	if got.LastScanRunID != "scan_mapper" || got.Namespace != defaultNS {
		t.Fatalf("review slice = %#v, want scan metadata", got)
	}
	run, err := store.GetScanRun(ctx, defaultNS, "scan_mapper")
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if run.SliceCount != 1 {
		t.Fatalf("run.SliceCount = %d, want 1", run.SliceCount)
	}
}

func TestIngestMapperTaskSelectsIncrementalSlicesFromChangedFiles(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	reconciler := &RepositoryScanReconciler{
		SecurityStore: store,
		ArtifactStore: store,
	}
	maxFindings := int32(1)
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
		Spec: corev1alpha1.RepositoryScanSpec{
			MaxFindingsPerRun: &maxFindings,
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kaset-incremental-mapper",
			Namespace: defaultNS,
			Labels: map[string]string{
				labels.LabelSecurityTarget: "kaset",
				labels.LabelSecurityScanID: "scan_incremental_mapper",
				labels.LabelSecurityMode:   "incremental",
				labels.LabelSecurityStage:  security.StageMapper,
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
	}
	artifact := security.ReviewSlicesArtifact{
		SchemaVersion:        security.SchemaVersionReviewSlices,
		BaseCommit:           "base123",
		HeadCommit:           "head456",
		ChangedFilesComputed: true,
		ChangedFiles:         []string{"internal/api/security.go", "internal/security/security_test.go"},
		Slices: []storepkg.ReviewSlice{
			{
				ID:             "slice_api",
				RepositoryScan: "kaset",
				Source:         "deterministic-go-package",
				Title:          "Go package internal/api",
				Summary:        "API handlers",
				Kind:           "package",
				OwnedFiles:     []storepkg.ReviewSliceFile{{Path: "internal/api/security.go", Reason: "source"}},
				Confidence:     "high",
				Status:         reviewSliceStatusReviewed,
			},
			{
				ID:             "slice_security_tests",
				RepositoryScan: "kaset",
				Source:         "deterministic-go-package",
				Title:          "Go package internal/security",
				Summary:        "Security helpers",
				Kind:           "package",
				OwnedFiles:     []storepkg.ReviewSliceFile{{Path: "internal/security/security.go", Reason: "source"}},
				ContextFiles:   []storepkg.ReviewSliceFile{{Path: "internal/security/security_test.go", Reason: "tests"}},
				Confidence:     "high",
				Status:         reviewSliceStatusReviewed,
			},
			{
				ID:             "slice_unaffected",
				RepositoryScan: "kaset",
				Source:         "deterministic-go-package",
				Title:          "Go package internal/store",
				Summary:        "Store helpers",
				Kind:           "package",
				OwnedFiles:     []storepkg.ReviewSliceFile{{Path: "internal/store/store.go", Reason: "source"}},
				Confidence:     "high",
				Status:         reviewSliceStatusReviewed,
			},
		},
	}
	data, err := json.Marshal(artifact)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if err := store.SaveArtifact(ctx, task.Namespace, task.Name, security.ArtifactSlices, "application/json", data); err != nil {
		t.Fatalf("SaveArtifact(slices) error = %v", err)
	}
	if err := store.CreateScanRun(ctx, &storepkg.ScanRun{
		ID:             "scan_incremental_mapper",
		Namespace:      defaultNS,
		RepositoryScan: "kaset",
		TaskName:       task.Name,
		Mode:           "incremental",
		Phase:          scanRunPhaseRunning,
		BaseCommit:     "base123",
		IdempotencyKey: "original-active-key",
		StartedAt:      time.Now(),
	}); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}

	if err := reconciler.ingestScanTask(ctx, scan, task); err != nil {
		t.Fatalf("ingestScanTask() error = %v", err)
	}

	for _, id := range []string{"slice_api", "slice_security_tests"} {
		reviewSlice, err := store.GetReviewSlice(ctx, defaultNS, "kaset", id)
		if err != nil {
			t.Fatalf("GetReviewSlice(%s) error = %v", id, err)
		}
		if reviewSlice.Status != reviewSliceStatusPending {
			t.Fatalf("%s status = %q, want pending", id, reviewSlice.Status)
		}
	}
	reviewSlice, err := store.GetReviewSlice(ctx, defaultNS, "kaset", "slice_unaffected")
	if err != nil {
		t.Fatalf("GetReviewSlice(slice_unaffected) error = %v", err)
	}
	if reviewSlice.Status != reviewSliceStatusSkipped {
		t.Fatalf("slice_unaffected status = %q, want skipped", reviewSlice.Status)
	}
	run, err := store.GetScanRun(ctx, defaultNS, "scan_incremental_mapper")
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if run.SliceCount != 3 || run.SkippedSliceCount != 1 {
		t.Fatalf("run slice counts = %d/%d, want 3/1", run.SliceCount, run.SkippedSliceCount)
	}
	if run.HeadCommit != "head456" {
		t.Fatalf("run.HeadCommit = %q, want head456", run.HeadCommit)
	}
	if run.IdempotencyKey != "original-active-key" {
		t.Fatalf("run.IdempotencyKey = %q, want stable active key", run.IdempotencyKey)
	}
}

func TestMapperReingestPreservesReviewedSliceForCurrentRun(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	reconciler := &RepositoryScanReconciler{
		SecurityStore: store,
		ArtifactStore: store,
	}
	maxFindings := int32(1)
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
		Spec: corev1alpha1.RepositoryScanSpec{
			MaxFindingsPerRun: &maxFindings,
		},
	}

	mapperTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kaset-mapper-reingest",
			Namespace: defaultNS,
			Labels: map[string]string{
				labels.LabelSecurityTarget: "kaset",
				labels.LabelSecurityScanID: "scan_mapper_reingest",
				labels.LabelSecurityMode:   "initial",
				labels.LabelSecurityStage:  security.StageMapper,
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
	}
	mapperArtifact := security.ReviewSlicesArtifact{
		SchemaVersion: security.SchemaVersionReviewSlices,
		Slices: []storepkg.ReviewSlice{{
			ID:             "slice_api",
			RepositoryScan: "kaset",
			Source:         "deterministic-go-package",
			Title:          "Go package internal/api",
			Summary:        "API handlers",
			Kind:           "package",
			OwnedFiles:     []storepkg.ReviewSliceFile{{Path: "internal/api/security.go", Reason: "source"}},
			Confidence:     "high",
			Status:         reviewSliceStatusPending,
		}},
	}
	mapperData, err := json.Marshal(mapperArtifact)
	if err != nil {
		t.Fatalf("json.Marshal(mapperArtifact) error = %v", err)
	}
	if err := store.SaveArtifact(ctx, mapperTask.Namespace, mapperTask.Name, security.ArtifactSlices, "application/json", mapperData); err != nil {
		t.Fatalf("SaveArtifact(slices) error = %v", err)
	}

	reviewTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kaset-review-reingest",
			Namespace: defaultNS,
			Labels: map[string]string{
				labels.LabelSecurityTarget:  "kaset",
				labels.LabelSecurityScanID:  "scan_mapper_reingest",
				labels.LabelSecurityMode:    "initial",
				labels.LabelSecurityStage:   security.StageReview,
				labels.LabelSecuritySliceID: "slice_api",
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
	}
	manifest := security.ReviewContextManifest{
		SchemaVersion: security.SchemaVersionReviewContext,
		SliceID:       "slice_api",
		IncludedFiles: []security.ReviewContextIncludedFile{{
			Path:               "internal/api/security.go",
			Role:               "owned",
			IncludedLineRanges: []security.ReviewContextLineRange{{StartLine: 1, EndLine: 20}},
			Readable:           true,
		}},
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal(manifest) error = %v", err)
	}
	if err := store.SaveArtifact(ctx, reviewTask.Namespace, reviewTask.Name, security.ReviewContextArtifactName("slice_api"), "application/json", manifestData); err != nil {
		t.Fatalf("SaveArtifact(manifest) error = %v", err)
	}
	findings := security.FindingsV2Artifact{
		SchemaVersion: security.SchemaVersionFindingsV2,
		Repository: security.FindingsV2Repository{
			RepoURL: "https://github.com/example/repo",
			Branch:  "main",
			HeadSHA: "head123",
		},
		Scan: security.FindingsV2Scan{Mode: "initial", SliceID: "slice_api", Summary: "one accepted"},
		Findings: []security.FindingsV2Finding{{
			Title:       "Unsafe API behavior",
			Category:    "authz",
			Severity:    "high",
			Confidence:  "high",
			Summary:     "API path lacks authorization.",
			Remediation: "Add authorization checks.",
			Evidence: []security.FindingsV2EvidenceRef{{
				Path:      "internal/api/security.go",
				StartLine: 5,
				EndLine:   8,
			}},
		}},
	}
	findingsData, err := json.Marshal(findings)
	if err != nil {
		t.Fatalf("json.Marshal(findings) error = %v", err)
	}
	if err := store.SaveArtifact(ctx, reviewTask.Namespace, reviewTask.Name, security.ArtifactFindingsV2, "application/json", findingsData); err != nil {
		t.Fatalf("SaveArtifact(findings v2) error = %v", err)
	}

	if err := reconciler.ingestScanTask(ctx, scan, mapperTask); err != nil {
		t.Fatalf("ingest mapper error = %v", err)
	}
	if err := reconciler.ingestScanTask(ctx, scan, reviewTask); err != nil {
		t.Fatalf("ingest review error = %v", err)
	}
	if err := reconciler.ingestScanTask(ctx, scan, mapperTask); err != nil {
		t.Fatalf("reingest mapper error = %v", err)
	}
	if err := reconciler.ingestScanTask(ctx, scan, reviewTask); err != nil {
		t.Fatalf("reingest review error = %v", err)
	}

	run, err := store.GetScanRun(ctx, defaultNS, "scan_mapper_reingest")
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if run.ReviewedSliceCount != 1 || run.AcceptedFindings != 1 || run.DroppedFindings != 0 {
		t.Fatalf("run counts = reviewed:%d accepted:%d dropped:%d, want 1/1/0", run.ReviewedSliceCount, run.AcceptedFindings, run.DroppedFindings)
	}
	reviewSlice, err := store.GetReviewSlice(ctx, defaultNS, "kaset", "slice_api")
	if err != nil {
		t.Fatalf("GetReviewSlice() error = %v", err)
	}
	if reviewSlice.Status != reviewSliceStatusReviewed {
		t.Fatalf("review slice status = %q, want reviewed after mapper reingest", reviewSlice.Status)
	}
}

func TestRepositoryScanCustomPolicyIncludedInReviewPrompt(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1.AddToScheme() error = %v", err)
	}
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:                   "https://github.com/example/repo",
			AnalysisAgentRef:          corev1alpha1.AgentReference{Name: "scan-reviewer"},
			CustomScanInstructionsRef: &corev1alpha1.PolicyConfigMapKeyRef{Name: "scan-policy", Key: "scan"},
			FalsePositivePolicyRef:    &corev1alpha1.PolicyConfigMapKeyRef{Name: "scan-policy", Key: "fp"},
		},
	}
	policyConfig := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "scan-policy", Namespace: defaultNS, Labels: map[string]string{security.PolicyConfigMapAllowedLabel: "true"}},
		Data: map[string]string{
			"scan": "Focus on operator RBAC drift.",
			"fp":   "Suppress intentionally public demo endpoint noise.",
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(scan, policyConfig).Build()
	reconciler := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: store}
	run := &storepkg.ScanRun{ID: "scan_policy", Namespace: defaultNS, RepositoryScan: "kaset", Mode: "initial", Phase: scanRunPhaseRunning}
	if err := reconciler.createReviewTasks(ctx, scan, run, "", []storepkg.ReviewSlice{{ID: "slice_api", RepositoryScan: "kaset", Source: "deterministic", Title: "API", Kind: "package", Status: reviewSliceStatusPending}}); err != nil {
		t.Fatalf("createReviewTasks() error = %v", err)
	}
	var tasks corev1alpha1.TaskList
	if err := cl.List(ctx, &tasks, client.InNamespace(defaultNS)); err != nil {
		t.Fatalf("List(Task) error = %v", err)
	}
	if len(tasks.Items) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks.Items))
	}
	prompt := tasks.Items[0].Spec.Prompt
	for _, want := range []string{"Focus on operator RBAC drift", "public demo endpoint", "Default Orka security policy"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("review prompt missing %q:\n%s", want, prompt)
		}
	}
	if run.PolicyDigest == "" {
		t.Fatal("run.PolicyDigest was not populated")
	}
}

func TestRepositoryScanCustomPolicyMissingConfigMapFails(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1.AddToScheme() error = %v", err)
	}
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:                   "https://github.com/example/repo",
			AnalysisAgentRef:          corev1alpha1.AgentReference{Name: "scan-reviewer"},
			CustomScanInstructionsRef: &corev1alpha1.PolicyConfigMapKeyRef{Name: "missing", Key: "policy"},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan).Build()
	reconciler := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: setupControllerSQLiteStore(t)}
	if err := reconciler.createScanRun(ctx, scan, "initial", "", ""); err == nil || !strings.Contains(err.Error(), "customScanInstructionsRef") {
		t.Fatalf("createScanRun() error = %v, want missing custom policy error", err)
	}
	current := &corev1alpha1.RepositoryScan{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		t.Fatalf("Get(RepositoryScan) error = %v", err)
	}
	if current.Status.Phase != repositoryScanPhaseError {
		t.Fatalf("RepositoryScan phase = %q, want %q", current.Status.Phase, repositoryScanPhaseError)
	}
	ready := meta.FindStatusCondition(current.Status.Conditions, "Ready")
	if ready == nil || ready.Reason != readyReasonScanFailed || !strings.Contains(ready.Message, "customScanInstructionsRef") {
		t.Fatalf("Ready condition = %#v, want ScanFailed policy error", ready)
	}
}

func TestRepositoryScanIdempotencySkipsDuplicateActiveRun(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
		Spec:       corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "scan-reviewer"}},
	}
	policyDigest := security.ScannerPolicyDigest(security.ScannerPolicy{})
	key := security.ScanRunIdempotencyKey(defaultNS, "kaset", scanModeIncremental, "base", "", "", policyDigest)
	if err := store.CreateScanRun(ctx, &storepkg.ScanRun{ID: "scan_existing", Namespace: defaultNS, RepositoryScan: "kaset", TaskName: "existing", Mode: scanModeIncremental, Phase: scanRunPhaseRunning, IdempotencyKey: key, PolicyDigest: policyDigest, StartedAt: time.Now()}); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	existingTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-pipeline",
			Namespace: defaultNS,
			Labels: map[string]string{
				labels.LabelSecurityTarget: labels.SelectorValue(scan.Name),
				labels.LabelSecurityScanID: "scan_existing",
				labels.LabelSecurityStage:  security.StageThreatModel,
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(scan, existingTask).Build()
	reconciler := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: store}
	if err := reconciler.createScanRun(ctx, scan, scanModeIncremental, "base", ""); err != nil {
		t.Fatalf("createScanRun() error = %v", err)
	}
	var tasks corev1alpha1.TaskList
	if err := cl.List(ctx, &tasks, client.InNamespace(defaultNS)); err != nil {
		t.Fatalf("List(Task) error = %v", err)
	}
	if len(tasks.Items) != 1 || tasks.Items[0].Name != "existing-pipeline" {
		t.Fatalf("tasks = %#v, want existing active pipeline only", tasks.Items)
	}
}

func TestRepositoryScanIdempotencyMarksOrphanedRunFailedAndStartsReplacement(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
		Spec:       corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "scan-reviewer"}},
	}
	policyDigest := security.ScannerPolicyDigest(security.ScannerPolicy{})
	key := security.ScanRunIdempotencyKey(defaultNS, "kaset", scanModeIncremental, "base", "", "", policyDigest)
	if err := store.CreateScanRun(ctx, &storepkg.ScanRun{ID: "scan_orphaned", Namespace: defaultNS, RepositoryScan: "kaset", TaskName: "missing", Mode: scanModeIncremental, Phase: scanRunPhaseRunning, IdempotencyKey: key, PolicyDigest: policyDigest, StartedAt: time.Now()}); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan).Build()
	reconciler := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: store}
	if err := reconciler.createScanRun(ctx, scan, scanModeIncremental, "base", ""); err != nil {
		t.Fatalf("createScanRun() error = %v", err)
	}
	orphaned, err := store.GetScanRun(ctx, defaultNS, "scan_orphaned")
	if err != nil {
		t.Fatalf("GetScanRun(orphaned) error = %v", err)
	}
	if orphaned.Phase != scanRunPhaseFailed || !strings.Contains(orphaned.ErrorMessage, "no active pipeline task") {
		t.Fatalf("orphaned run = %#v, want failed stale run", orphaned)
	}
	var tasks corev1alpha1.TaskList
	if err := cl.List(ctx, &tasks, client.InNamespace(defaultNS)); err != nil {
		t.Fatalf("List(Task) error = %v", err)
	}
	if len(tasks.Items) != 1 || taskSecurityStage(&tasks.Items[0]) != security.StageThreatModel {
		t.Fatalf("tasks = %#v, want replacement threat-model task", tasks.Items)
	}
}

func TestProgressLatestScanRunStartsReviewTasksForPendingSlices(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1alpha1.GroupVersion.String(),
			Kind:       "RepositoryScan",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:          "https://github.com/sozercan/kaset",
			Branch:           "main",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "scan-reviewer"},
		},
		Status: corev1alpha1.RepositoryScanStatus{LastScanID: "scan_review"},
	}
	threatTask := newSucceededSecurityTask("kaset-initial-threat", "scan_review", security.StageThreatModel, metav1.Now())
	mapperTask := newSucceededSecurityTask("kaset-initial-mapper", "scan_review", security.StageMapper, metav1.Now())
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(scan, threatTask, mapperTask).
		Build()
	reconciler := &RepositoryScanReconciler{
		Client:        cl,
		Scheme:        scheme,
		SecurityStore: store,
	}
	if err := store.CreateScanRun(ctx, &storepkg.ScanRun{
		ID:             "scan_review",
		Namespace:      defaultNS,
		RepositoryScan: "kaset",
		TaskName:       threatTask.Name,
		Mode:           "initial",
		Phase:          scanRunPhasePending,
		StartedAt:      time.Now(),
	}); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	if err := store.UpsertReviewSlice(ctx, &storepkg.ReviewSlice{
		SchemaVersion:  1,
		ID:             "slice_api",
		Namespace:      defaultNS,
		RepositoryScan: "kaset",
		Source:         "deterministic-go-package",
		Title:          "Go package internal/api",
		Summary:        "API handlers",
		Kind:           "package",
		OwnedFiles:     []storepkg.ReviewSliceFile{{Path: "internal/api/security.go", Reason: "source"}},
		Confidence:     "high",
		Status:         reviewSliceStatusPending,
		LastScanRunID:  "scan_review",
	}); err != nil {
		t.Fatalf("UpsertReviewSlice() error = %v", err)
	}

	progressed, err := reconciler.progressLatestScanRun(ctx, scan)
	if err != nil {
		t.Fatalf("progressLatestScanRun() error = %v", err)
	}
	if !progressed {
		t.Fatal("progressLatestScanRun() = false, want true")
	}

	var reviewTasks corev1alpha1.TaskList
	if err := cl.List(ctx, &reviewTasks,
		client.InNamespace(defaultNS),
		client.MatchingLabels(map[string]string{
			labels.LabelSecurityTarget:  "kaset",
			labels.LabelSecurityScanID:  "scan_review",
			labels.LabelSecurityStage:   security.StageReview,
			labels.LabelSecuritySliceID: "slice_api",
		}),
	); err != nil {
		t.Fatalf("List(review tasks) error = %v", err)
	}
	if len(reviewTasks.Items) != 1 {
		t.Fatalf("len(review tasks) = %d, want 1", len(reviewTasks.Items))
	}
	if !strings.Contains(reviewTasks.Items[0].Spec.Prompt, security.ArtifactFindingsV2) ||
		!strings.Contains(reviewTasks.Items[0].Spec.Prompt, security.ReviewContextArtifactName("slice_api")) {
		t.Fatalf("review prompt does not mention required v2 artifacts: %q", reviewTasks.Items[0].Spec.Prompt)
	}

}

func TestProgressLatestScanRunFailsMapperArtifactValidationProblem(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1alpha1.GroupVersion.String(),
			Kind:       "RepositoryScan",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:          "https://github.com/sozercan/kaset",
			Branch:           "main",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "scan-reviewer"},
		},
		Status: corev1alpha1.RepositoryScanStatus{LastScanID: "scan_mapper_failed"},
	}
	completed := metav1.Now()
	threatTask := newSucceededSecurityTask("kaset-initial-threat", "scan_mapper_failed", security.StageThreatModel, completed)
	mapperTask := newSucceededSecurityTask("kaset-initial-mapper", "scan_mapper_failed", security.StageMapper, completed)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(scan, threatTask, mapperTask).
		Build()
	reconciler := &RepositoryScanReconciler{
		Client:        cl,
		Scheme:        scheme,
		SecurityStore: store,
	}
	if err := store.CreateScanRun(ctx, &storepkg.ScanRun{
		ID:             "scan_mapper_failed",
		Namespace:      defaultNS,
		RepositoryScan: "kaset",
		TaskName:       threatTask.Name,
		Mode:           "initial",
		Phase:          scanRunPhaseRunning,
		ErrorMessage:   "mapper stage failed: security-slices.json is missing",
		StartedAt:      time.Now(),
	}); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}

	progressed, err := reconciler.progressLatestScanRun(ctx, scan)
	if err != nil {
		t.Fatalf("progressLatestScanRun() error = %v", err)
	}
	if !progressed {
		t.Fatal("progressLatestScanRun() = false, want true")
	}

	run, err := store.GetScanRun(ctx, defaultNS, "scan_mapper_failed")
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if run.Phase != scanRunPhaseFailed {
		t.Fatalf("run.Phase = %q, want failed", run.Phase)
	}
	if !strings.Contains(run.Summary, security.ArtifactSlices) {
		t.Fatalf("run.Summary = %q, want mapper artifact failure", run.Summary)
	}

	current := &corev1alpha1.RepositoryScan{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		t.Fatalf("Get(scan) error = %v", err)
	}
	if current.Status.Phase != repositoryScanPhaseError {
		t.Fatalf("scan status phase = %q, want Error", current.Status.Phase)
	}
	ready := meta.FindStatusCondition(current.Status.Conditions, "Ready")
	if ready == nil {
		t.Fatal("Ready condition missing")
	}
	if ready.Status != metav1.ConditionFalse || ready.Reason != readyReasonScanFailed {
		t.Fatalf("Ready condition = %#v, want failed condition", ready)
	}
}

func TestProgressLatestScanRunRetriesPendingSlicesWithoutTasks(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1alpha1.GroupVersion.String(),
			Kind:       "RepositoryScan",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:          "https://github.com/sozercan/kaset",
			Branch:           "main",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "scan-reviewer"},
		},
		Status: corev1alpha1.RepositoryScanStatus{LastScanID: "scan_partial_review"},
	}
	const sliceAPI = "slice_api"
	threatTask := newSucceededSecurityTask("kaset-partial-threat", "scan_partial_review", security.StageThreatModel, metav1.Now())
	mapperTask := newSucceededSecurityTask("kaset-partial-mapper", "scan_partial_review", security.StageMapper, metav1.Now())
	reviewTask := newSucceededSecurityTask("kaset-review-slice-api", "scan_partial_review", security.StageReview, metav1.Now())
	reviewTask.Labels[labels.LabelSecuritySliceID] = sliceAPI
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(scan, threatTask, mapperTask, reviewTask).
		Build()
	reconciler := &RepositoryScanReconciler{
		Client:        cl,
		Scheme:        scheme,
		SecurityStore: store,
	}
	if err := store.CreateScanRun(ctx, &storepkg.ScanRun{
		ID:             "scan_partial_review",
		Namespace:      defaultNS,
		RepositoryScan: "kaset",
		TaskName:       threatTask.Name,
		Mode:           "initial",
		Phase:          scanRunPhaseRunning,
		SliceCount:     2,
		StartedAt:      time.Now(),
	}); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	for _, slice := range []storepkg.ReviewSlice{
		{
			SchemaVersion:  1,
			ID:             sliceAPI,
			Namespace:      defaultNS,
			RepositoryScan: "kaset",
			Source:         "deterministic-go-package",
			Title:          "Go package internal/api",
			Summary:        "Already reviewed.",
			Kind:           "package",
			OwnedFiles:     []storepkg.ReviewSliceFile{{Path: "internal/api/security.go", Reason: "source"}},
			Confidence:     "high",
			Status:         reviewSliceStatusReviewed,
			LastScanRunID:  "scan_partial_review",
		},
		{
			SchemaVersion:  1,
			ID:             "slice_store",
			Namespace:      defaultNS,
			RepositoryScan: "kaset",
			Source:         "deterministic-go-package",
			Title:          "Go package internal/store",
			Summary:        "Task creation was interrupted before this slice started.",
			Kind:           "package",
			OwnedFiles:     []storepkg.ReviewSliceFile{{Path: "internal/store/store.go", Reason: "source"}},
			Confidence:     "high",
			Status:         reviewSliceStatusPending,
			LastScanRunID:  "scan_partial_review",
		},
	} {
		if err := store.UpsertReviewSlice(ctx, &slice); err != nil {
			t.Fatalf("UpsertReviewSlice(%s) error = %v", slice.ID, err)
		}
	}

	progressed, err := reconciler.progressLatestScanRun(ctx, scan)
	if err != nil {
		t.Fatalf("progressLatestScanRun() error = %v", err)
	}
	if !progressed {
		t.Fatal("progressLatestScanRun() = false, want true")
	}

	var reviewTasks corev1alpha1.TaskList
	if err := cl.List(ctx, &reviewTasks,
		client.InNamespace(defaultNS),
		client.MatchingLabels(map[string]string{
			labels.LabelSecurityTarget:  "kaset",
			labels.LabelSecurityScanID:  "scan_partial_review",
			labels.LabelSecurityStage:   security.StageReview,
			labels.LabelSecuritySliceID: "slice_store",
		}),
	); err != nil {
		t.Fatalf("List(review tasks) error = %v", err)
	}
	if len(reviewTasks.Items) != 1 {
		t.Fatalf("len(review tasks) = %d, want retry task for missing slice", len(reviewTasks.Items))
	}
	run, err := store.GetScanRun(ctx, defaultNS, "scan_partial_review")
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if run.Phase != scanRunPhaseRunning || !strings.Contains(run.Summary, "retrying 1 pending review slices") {
		t.Fatalf("run phase/summary = %q/%q, want running retry summary", run.Phase, run.Summary)
	}
}

func TestPendingReviewSlicesPaginatesAllPendingSlices(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	reconciler := &RepositoryScanReconciler{SecurityStore: store}
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
	}

	const totalSlices = 1005
	for i := range totalSlices {
		if err := store.UpsertReviewSlice(ctx, &storepkg.ReviewSlice{
			SchemaVersion:  1,
			ID:             fmt.Sprintf("slice_bulk_%04d", i),
			Namespace:      defaultNS,
			RepositoryScan: "kaset",
			Source:         "deterministic-generic",
			Title:          fmt.Sprintf("Bulk slice %04d", i),
			Summary:        "Bulk pending slice.",
			Kind:           "unknown",
			OwnedFiles:     []storepkg.ReviewSliceFile{{Path: fmt.Sprintf("src/file_%04d.go", i), Reason: "source"}},
			Confidence:     "medium",
			Status:         reviewSliceStatusPending,
			LastScanRunID:  "scan_review",
		}); err != nil {
			t.Fatalf("UpsertReviewSlice(%d) error = %v", i, err)
		}
	}

	if err := store.UpsertReviewSlice(ctx, &storepkg.ReviewSlice{
		SchemaVersion:  1,
		ID:             "slice_stale",
		Namespace:      defaultNS,
		RepositoryScan: "kaset",
		Source:         "deterministic-generic",
		Title:          "Stale slice",
		Summary:        "Pending from another run.",
		Kind:           "unknown",
		OwnedFiles:     []storepkg.ReviewSliceFile{{Path: "src/stale.go", Reason: "source"}},
		Confidence:     "medium",
		Status:         reviewSliceStatusPending,
		LastScanRunID:  "scan_stale",
	}); err != nil {
		t.Fatalf("UpsertReviewSlice(stale) error = %v", err)
	}

	got, err := reconciler.pendingReviewSlices(ctx, scan, "scan_review")
	if err != nil {
		t.Fatalf("pendingReviewSlices() error = %v", err)
	}
	if len(got) != totalSlices {
		t.Fatalf("len(pendingReviewSlices) = %d, want %d", len(got), totalSlices)
	}
}

func TestProgressLatestScanRunCompletesNoopIncrementalWhenNoSlicesMatch(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1alpha1.GroupVersion.String(),
			Kind:       "RepositoryScan",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:          "https://github.com/sozercan/kaset",
			Branch:           "main",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "scan-reviewer"},
		},
		Status: corev1alpha1.RepositoryScanStatus{LastScanID: "scan_noop_incremental"},
	}
	threatTask := newSucceededSecurityTask("kaset-incremental-threat", "scan_noop_incremental", security.StageThreatModel, metav1.Now())
	mapperTask := newSucceededSecurityTask("kaset-incremental-mapper", "scan_noop_incremental", security.StageMapper, metav1.Now())
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(scan, threatTask, mapperTask).
		Build()
	reconciler := &RepositoryScanReconciler{
		Client:        cl,
		Scheme:        scheme,
		SecurityStore: store,
	}
	if err := store.CreateScanRun(ctx, &storepkg.ScanRun{
		ID:                "scan_noop_incremental",
		Namespace:         defaultNS,
		RepositoryScan:    "kaset",
		TaskName:          threatTask.Name,
		Mode:              "incremental",
		Phase:             scanRunPhaseRunning,
		BaseCommit:        "base123",
		HeadCommit:        "head456",
		SliceCount:        2,
		SkippedSliceCount: 2,
		Summary:           "Threat model generated; no review slices matched 1 changed files",
		StartedAt:         time.Now(),
	}); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	for _, id := range []string{"slice_api", "slice_store"} {
		if err := store.UpsertReviewSlice(ctx, &storepkg.ReviewSlice{
			SchemaVersion:  1,
			ID:             id,
			Namespace:      defaultNS,
			RepositoryScan: "kaset",
			Source:         "deterministic-go-package",
			Title:          id,
			Summary:        "No changed files matched this slice.",
			Kind:           "package",
			OwnedFiles:     []storepkg.ReviewSliceFile{{Path: id + ".go", Reason: "source"}},
			Confidence:     "high",
			Status:         reviewSliceStatusSkipped,
			LastScanRunID:  "scan_noop_incremental",
		}); err != nil {
			t.Fatalf("UpsertReviewSlice(%s) error = %v", id, err)
		}
	}

	progressed, err := reconciler.progressLatestScanRun(ctx, scan)
	if err != nil {
		t.Fatalf("progressLatestScanRun() error = %v", err)
	}
	if !progressed {
		t.Fatal("progressLatestScanRun() = false, want true")
	}

	run, err := store.GetScanRun(ctx, defaultNS, "scan_noop_incremental")
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if run.Phase != scanRunPhaseSucceeded || run.CompletedAt == nil {
		t.Fatalf("run phase/completedAt = %q/%v, want succeeded with completion", run.Phase, run.CompletedAt)
	}
	if run.Summary != "Threat model generated; no review slices matched 1 changed files" {
		t.Fatalf("run.Summary = %q, want mapper no-op summary", run.Summary)
	}
	current := &corev1alpha1.RepositoryScan{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		t.Fatalf("Get(scan) error = %v", err)
	}
	if current.Status.Phase != repositoryScanPhaseReady || current.Status.LastProcessedCommit != "head456" {
		t.Fatalf("scan status phase/processed = %q/%q, want Ready/head456", current.Status.Phase, current.Status.LastProcessedCommit)
	}
	ready := meta.FindStatusCondition(current.Status.Conditions, "Ready")
	if ready == nil {
		t.Fatal("Ready condition missing")
	}
	if strings.Contains(ready.Message, "pending") {
		t.Fatalf("Ready condition message = %q, want completed no-op summary", ready.Message)
	}
}

func TestRefreshScanRunStatusKeepsReviewRunRunningWithPendingSlices(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1alpha1.GroupVersion.String(),
			Kind:       "RepositoryScan",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:          "https://github.com/sozercan/kaset",
			Branch:           "main",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "scan-reviewer"},
		},
		Status: corev1alpha1.RepositoryScanStatus{LastScanID: "scan_review_incomplete"},
	}
	completed := metav1.Now()
	threatTask := newSucceededSecurityTask("kaset-incomplete-threat", "scan_review_incomplete", security.StageThreatModel, completed)
	mapperTask := newSucceededSecurityTask("kaset-incomplete-mapper", "scan_review_incomplete", security.StageMapper, completed)
	reviewTask := newSucceededSecurityTask("kaset-incomplete-review-api", "scan_review_incomplete", security.StageReview, completed)
	reviewTask.Labels[labels.LabelSecuritySliceID] = "slice_api"
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(scan, threatTask, mapperTask, reviewTask).
		Build()
	reconciler := &RepositoryScanReconciler{
		Client:        cl,
		Scheme:        scheme,
		SecurityStore: store,
	}
	if err := store.CreateScanRun(ctx, &storepkg.ScanRun{
		ID:             "scan_review_incomplete",
		Namespace:      defaultNS,
		RepositoryScan: "kaset",
		TaskName:       threatTask.Name,
		Mode:           "initial",
		Phase:          scanRunPhaseRunning,
		SliceCount:     2,
		HeadCommit:     "head456",
		StartedAt:      time.Now(),
	}); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	for _, slice := range []storepkg.ReviewSlice{
		{
			SchemaVersion:  1,
			ID:             "slice_api",
			Namespace:      defaultNS,
			RepositoryScan: "kaset",
			Source:         "deterministic-go-package",
			Title:          "Go package internal/api",
			Summary:        "Already reviewed.",
			Kind:           "package",
			OwnedFiles:     []storepkg.ReviewSliceFile{{Path: "internal/api/security.go", Reason: "source"}},
			Confidence:     "high",
			Status:         reviewSliceStatusReviewed,
			LastScanRunID:  "scan_review_incomplete",
		},
		{
			SchemaVersion:  1,
			ID:             "slice_store",
			Namespace:      defaultNS,
			RepositoryScan: "kaset",
			Source:         "deterministic-go-package",
			Title:          "Go package internal/store",
			Summary:        "Still pending.",
			Kind:           "package",
			OwnedFiles:     []storepkg.ReviewSliceFile{{Path: "internal/store/store.go", Reason: "source"}},
			Confidence:     "high",
			Status:         reviewSliceStatusPending,
			LastScanRunID:  "scan_review_incomplete",
		},
	} {
		if err := store.UpsertReviewSlice(ctx, &slice); err != nil {
			t.Fatalf("UpsertReviewSlice(%s) error = %v", slice.ID, err)
		}
	}

	run, err := store.GetScanRun(ctx, defaultNS, "scan_review_incomplete")
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if err := reconciler.refreshScanRunStatus(ctx, scan, run, "scan_review_incomplete", true); err != nil {
		t.Fatalf("refreshScanRunStatus() error = %v", err)
	}

	run, err = store.GetScanRun(ctx, defaultNS, "scan_review_incomplete")
	if err != nil {
		t.Fatalf("GetScanRun() after refresh error = %v", err)
	}
	if run.Phase != scanRunPhaseRunning || run.CompletedAt != nil {
		t.Fatalf("run phase/completedAt = %q/%v, want running without completion", run.Phase, run.CompletedAt)
	}
	if !strings.Contains(run.Summary, "1 review slices remain pending") {
		t.Fatalf("run.Summary = %q, want pending slice summary", run.Summary)
	}
	current := &corev1alpha1.RepositoryScan{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		t.Fatalf("Get(scan) error = %v", err)
	}
	if current.Status.Phase != repositoryScanPhaseScanning || current.Status.LastProcessedCommit != "" {
		t.Fatalf("scan status phase/processed = %q/%q, want Scanning with no processed commit", current.Status.Phase, current.Status.LastProcessedCommit)
	}
}

func TestIngestReviewTaskRejectsMismatchedV2SliceID(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	reconciler := &RepositoryScanReconciler{
		SecurityStore: store,
		ArtifactStore: store,
	}
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
	}
	if err := store.UpsertReviewSlice(ctx, &storepkg.ReviewSlice{
		SchemaVersion:  1,
		ID:             "slice_api",
		Namespace:      defaultNS,
		RepositoryScan: "kaset",
		Source:         "deterministic-go-package",
		Title:          "Go package internal/api",
		Summary:        "API handlers",
		Kind:           "package",
		OwnedFiles:     []storepkg.ReviewSliceFile{{Path: "internal/api/security.go", Reason: "source"}},
		Confidence:     "high",
		Status:         reviewSliceStatusPending,
		LastScanRunID:  "scan_mismatched_slice",
	}); err != nil {
		t.Fatalf("UpsertReviewSlice() error = %v", err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kaset-review-mismatched-slice",
			Namespace: defaultNS,
			Labels: map[string]string{
				labels.LabelSecurityTarget:  "kaset",
				labels.LabelSecurityScanID:  "scan_mismatched_slice",
				labels.LabelSecurityMode:    "initial",
				labels.LabelSecurityStage:   security.StageReview,
				labels.LabelSecuritySliceID: "slice_api",
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
	}
	maliciousManifest := security.ReviewContextManifest{
		SchemaVersion: security.SchemaVersionReviewContext,
		SliceID:       "slice_other",
		IncludedFiles: []security.ReviewContextIncludedFile{{
			Path:               "internal/api/omitted.go",
			Role:               "owned",
			IncludedLineRanges: []security.ReviewContextLineRange{{StartLine: 1, EndLine: 20}},
			Readable:           true,
		}},
	}
	manifestData, err := json.Marshal(maliciousManifest)
	if err != nil {
		t.Fatalf("json.Marshal(manifest) error = %v", err)
	}
	if err := store.SaveArtifact(
		ctx,
		task.Namespace,
		task.Name,
		security.ReviewContextArtifactName("slice_other"),
		"application/json",
		manifestData,
	); err != nil {
		t.Fatalf("SaveArtifact(manifest) error = %v", err)
	}
	findings := security.FindingsV2Artifact{
		SchemaVersion: security.SchemaVersionFindingsV2,
		Repository: security.FindingsV2Repository{
			RepoURL: "https://github.com/example/repo",
			Branch:  "main",
			HeadSHA: "head123",
		},
		Scan: security.FindingsV2Scan{
			Mode:    "initial",
			SliceID: "slice_other",
			Summary: "mismatched context",
		},
		Findings: []security.FindingsV2Finding{{
			Title:       "Speculative issue",
			Category:    "authz",
			Severity:    "high",
			Confidence:  "high",
			Summary:     "Cites a file outside the assigned review slice.",
			Remediation: "Add authorization checks.",
			Evidence: []security.FindingsV2EvidenceRef{{
				Path:      "internal/api/omitted.go",
				StartLine: 5,
				EndLine:   8,
			}},
		}},
	}
	findingsData, err := json.Marshal(findings)
	if err != nil {
		t.Fatalf("json.Marshal(findings) error = %v", err)
	}
	if err := store.SaveArtifact(ctx, task.Namespace, task.Name, security.ArtifactFindingsV2, "application/json", findingsData); err != nil {
		t.Fatalf("SaveArtifact(findings v2) error = %v", err)
	}

	if err := reconciler.ingestScanTask(ctx, scan, task); err != nil {
		t.Fatalf("ingestScanTask() error = %v", err)
	}

	run, err := store.GetScanRun(ctx, defaultNS, "scan_mismatched_slice")
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if run.Phase != scanRunPhaseFailed || !strings.Contains(run.ErrorMessage, "does not match task slice") {
		t.Fatalf("run phase/error = %q/%q, want failed slice mismatch", run.Phase, run.ErrorMessage)
	}
	reviewSlice, err := store.GetReviewSlice(ctx, defaultNS, "kaset", "slice_api")
	if err != nil {
		t.Fatalf("GetReviewSlice() error = %v", err)
	}
	if reviewSlice.Status != reviewSliceStatusFailed {
		t.Fatalf("review slice status = %q, want failed", reviewSlice.Status)
	}
	listed, _, err := store.ListFindings(ctx, storepkg.FindingFilter{Namespace: defaultNS, RepositoryScan: "kaset"})
	if err != nil {
		t.Fatalf("ListFindings() error = %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("len(findings) = %d, want no accepted findings for mismatched slice", len(listed))
	}
}

func TestIngestReviewTaskPartitionsV2FindingsAndMarksSliceReviewed(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	reconciler := &RepositoryScanReconciler{
		SecurityStore: store,
		ArtifactStore: store,
	}
	maxFindings := int32(1)
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
		Spec: corev1alpha1.RepositoryScanSpec{
			MaxFindingsPerRun: &maxFindings,
		},
	}
	if err := store.CreateScanRun(ctx, &storepkg.ScanRun{
		ID:             "scan_review_ingest",
		Namespace:      defaultNS,
		RepositoryScan: "kaset",
		TaskName:       "kaset-review-slice",
		Mode:           "initial",
		Phase:          scanRunPhaseRunning,
		BaseCommit:     "trusted-base",
		HeadCommit:     "trusted-head",
		StartedAt:      time.Now(),
	}); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	if err := store.UpsertReviewSlice(ctx, &storepkg.ReviewSlice{
		SchemaVersion:  1,
		ID:             "slice_api",
		Namespace:      defaultNS,
		RepositoryScan: "kaset",
		Source:         "deterministic-go-package",
		Title:          "Go package internal/api",
		Summary:        "API handlers",
		Kind:           "package",
		OwnedFiles:     []storepkg.ReviewSliceFile{{Path: "internal/api/security.go", Reason: "source"}},
		Confidence:     "high",
		Status:         reviewSliceStatusPending,
		LastScanRunID:  "scan_review_ingest",
	}); err != nil {
		t.Fatalf("UpsertReviewSlice() error = %v", err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kaset-review-slice",
			Namespace: defaultNS,
			Labels: map[string]string{
				labels.LabelSecurityTarget:  "kaset",
				labels.LabelSecurityScanID:  "scan_review_ingest",
				labels.LabelSecurityMode:    "initial",
				labels.LabelSecurityStage:   security.StageReview,
				labels.LabelSecuritySliceID: "slice_api",
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
	}
	manifest := security.ReviewContextManifest{
		SchemaVersion: security.SchemaVersionReviewContext,
		SliceID:       "slice_api",
		IncludedFiles: []security.ReviewContextIncludedFile{{
			Path:               "internal/api/security.go",
			Role:               "owned",
			IncludedLineRanges: []security.ReviewContextLineRange{{StartLine: 1, EndLine: 20}},
			Readable:           true,
		}},
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal(manifest) error = %v", err)
	}
	if err := store.SaveArtifact(ctx, task.Namespace, task.Name, security.ReviewContextArtifactName("slice_api"), "application/json", manifestData); err != nil {
		t.Fatalf("SaveArtifact(manifest) error = %v", err)
	}
	findings := security.FindingsV2Artifact{
		SchemaVersion: security.SchemaVersionFindingsV2,
		Repository: security.FindingsV2Repository{
			RepoURL: "https://github.com/example/repo",
			Branch:  "main",
			BaseSHA: "artifact-base",
			HeadSHA: "artifact-head",
		},
		Scan: security.FindingsV2Scan{Mode: "manual", SliceID: "slice_api", Summary: "one accepted, two dropped"},
		Findings: []security.FindingsV2Finding{
			{
				Title:       "Unsafe API behavior",
				Category:    "authz",
				Severity:    "high",
				Confidence:  "high",
				Summary:     "API path lacks authorization.",
				Remediation: "Add authorization checks.",
				Evidence: []security.FindingsV2EvidenceRef{{
					Path:      "internal/api/security.go",
					StartLine: 5,
					EndLine:   8,
				}},
			},
			{
				Title:       "Unsafe API audit bypass",
				Category:    "authz",
				Severity:    "medium",
				Confidence:  "high",
				Summary:     "A second valid API issue exceeds the configured run cap.",
				Remediation: "Add authorization checks.",
				Evidence: []security.FindingsV2EvidenceRef{{
					Path:      "internal/api/security.go",
					StartLine: 9,
					EndLine:   12,
				}},
			},
			{
				Title:       "Speculative issue",
				Category:    "authz",
				Severity:    "high",
				Confidence:  "low",
				Summary:     "Cites an omitted file.",
				Remediation: "Fix it.",
				Evidence: []security.FindingsV2EvidenceRef{{
					Path:      "internal/api/omitted.go",
					StartLine: 1,
					EndLine:   1,
				}},
			},
		},
	}
	findingsData, err := json.Marshal(findings)
	if err != nil {
		t.Fatalf("json.Marshal(findings) error = %v", err)
	}
	if err := store.SaveArtifact(ctx, task.Namespace, task.Name, security.ArtifactFindingsV2, "application/json", findingsData); err != nil {
		t.Fatalf("SaveArtifact(findings v2) error = %v", err)
	}

	if err := reconciler.ingestScanTask(ctx, scan, task); err != nil {
		t.Fatalf("ingestScanTask() error = %v", err)
	}
	if err := reconciler.ingestScanTask(ctx, scan, task); err != nil {
		t.Fatalf("second ingestScanTask() error = %v", err)
	}
	run, err := store.GetScanRun(ctx, defaultNS, "scan_review_ingest")
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if run.ReviewedSliceCount != 1 || run.AcceptedFindings != 1 || run.DroppedFindings != 2 {
		t.Fatalf("run counts = reviewed:%d accepted:%d dropped:%d, want 1/1/2", run.ReviewedSliceCount, run.AcceptedFindings, run.DroppedFindings)
	}
	if run.BaseCommit != "trusted-base" || run.HeadCommit != "trusted-head" {
		t.Fatalf("run commits = %q/%q, want trusted-base/trusted-head", run.BaseCommit, run.HeadCommit)
	}
	if run.Mode != "initial" {
		t.Fatalf("run mode = %q, want trusted initial mode", run.Mode)
	}
	reviewSlice, err := store.GetReviewSlice(ctx, defaultNS, "kaset", "slice_api")
	if err != nil {
		t.Fatalf("GetReviewSlice() error = %v", err)
	}
	if reviewSlice.Status != reviewSliceStatusReviewed || reviewSlice.LastReviewedAt == nil {
		t.Fatalf("review slice status = %q lastReviewedAt=%v, want reviewed with timestamp", reviewSlice.Status, reviewSlice.LastReviewedAt)
	}
	listed, _, err := store.ListFindings(ctx, storepkg.FindingFilter{Namespace: defaultNS, RepositoryScan: "kaset", SliceID: "slice_api"})
	if err != nil {
		t.Fatalf("ListFindings() error = %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("len(findings) = %d, want one accepted review finding", len(listed))
	}
	if listed[0].CommitSHA != "trusted-head" {
		t.Fatalf("finding.CommitSHA = %q, want trusted run head", listed[0].CommitSHA)
	}
	dropped, _, err := store.ListDroppedFindings(ctx, storepkg.DroppedFindingFilter{Namespace: defaultNS, RepositoryScan: "kaset", ScanRunID: "scan_review_ingest", SliceID: "slice_api"})
	if err != nil {
		t.Fatalf("ListDroppedFindings() error = %v", err)
	}
	if len(dropped) != 2 {
		t.Fatalf("len(dropped) = %d, want two diagnostics", len(dropped))
	}
	sawCapDrop := false
	for _, item := range dropped {
		if strings.Contains(item.Reason, "maxFindingsPerRun limit 1 reached") {
			sawCapDrop = true
		}
	}
	if !sawCapDrop {
		t.Fatalf("dropped diagnostics = %#v, want maxFindingsPerRun cap diagnostic", dropped)
	}
}

func TestIngestReviewTaskPersistsFilterDroppedDiagnosticsBeforeCap(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	reconciler := &RepositoryScanReconciler{SecurityStore: store, ArtifactStore: store}
	maxFindings := int32(1)
	scan := &corev1alpha1.RepositoryScan{ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS}, Spec: corev1alpha1.RepositoryScanSpec{MaxFindingsPerRun: &maxFindings}}
	if err := store.CreateScanRun(ctx, &storepkg.ScanRun{ID: "scan_review_filter", Namespace: defaultNS, RepositoryScan: "kaset", TaskName: "kaset-review-filter", Mode: "initial", Phase: scanRunPhaseRunning, HeadCommit: "trusted-head", StartedAt: time.Now()}); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	if err := store.UpsertReviewSlice(ctx, &storepkg.ReviewSlice{SchemaVersion: 1, ID: "slice_filter", Namespace: defaultNS, RepositoryScan: "kaset", Source: "deterministic", Title: "Filter slice", Kind: "package", OwnedFiles: []storepkg.ReviewSliceFile{{Path: "docs/security.md"}, {Path: "internal/api/security.go"}}, Confidence: "high", Status: reviewSliceStatusPending, LastScanRunID: "scan_review_filter"}); err != nil {
		t.Fatalf("UpsertReviewSlice() error = %v", err)
	}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "kaset-review-filter", Namespace: defaultNS, Labels: map[string]string{labels.LabelSecurityTarget: "kaset", labels.LabelSecurityScanID: "scan_review_filter", labels.LabelSecurityMode: "initial", labels.LabelSecurityStage: security.StageReview, labels.LabelSecuritySliceID: "slice_filter"}}, Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded}}
	manifest := security.ReviewContextManifest{SchemaVersion: security.SchemaVersionReviewContext, SliceID: "slice_filter", IncludedFiles: []security.ReviewContextIncludedFile{{Path: "docs/security.md", Role: "owned", IncludedLineRanges: []security.ReviewContextLineRange{{StartLine: 1, EndLine: 5}}, Readable: true}, {Path: "internal/api/security.go", Role: "owned", IncludedLineRanges: []security.ReviewContextLineRange{{StartLine: 1, EndLine: 20}}, Readable: true}}}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal(manifest) error = %v", err)
	}
	if err := store.SaveArtifact(ctx, task.Namespace, task.Name, security.ReviewContextArtifactName("slice_filter"), "application/json", manifestData); err != nil {
		t.Fatalf("SaveArtifact(manifest) error = %v", err)
	}
	findings := security.FindingsV2Artifact{SchemaVersion: security.SchemaVersionFindingsV2, Repository: security.FindingsV2Repository{RepoURL: "https://github.com/example/repo", Branch: "main"}, Scan: security.FindingsV2Scan{Mode: "initial", SliceID: "slice_filter", Summary: "filter then cap"}, Findings: []security.FindingsV2Finding{
		{Title: "Docs-only rate limit", Category: "rate-limit", Severity: "medium", Confidence: "high", Summary: "Documentation says rate limiting is missing.", Remediation: "Document it.", Evidence: []security.FindingsV2EvidenceRef{{Path: "docs/security.md", StartLine: 1, EndLine: 1}}},
		{Title: "Unsafe API behavior", Category: "authz", Severity: "high", Confidence: "high", Summary: "Attacker-controlled request crosses auth trust boundary.", Remediation: "Add server-side authorization.", Evidence: []security.FindingsV2EvidenceRef{{Path: "internal/api/security.go", StartLine: 2, EndLine: 3}}},
		{Title: "Unsafe API audit bypass", Category: "authz", Severity: "medium", Confidence: "high", Summary: "Second concrete tenant authorization bypass.", Remediation: "Add server-side authorization.", Evidence: []security.FindingsV2EvidenceRef{{Path: "internal/api/security.go", StartLine: 4, EndLine: 5}}},
	}}
	findingsData, err := json.Marshal(findings)
	if err != nil {
		t.Fatalf("json.Marshal(findings) error = %v", err)
	}
	if err := store.SaveArtifact(ctx, task.Namespace, task.Name, security.ArtifactFindingsV2, "application/json", findingsData); err != nil {
		t.Fatalf("SaveArtifact(findings) error = %v", err)
	}

	if err := reconciler.ingestScanTask(ctx, scan, task); err != nil {
		t.Fatalf("ingestScanTask() error = %v", err)
	}
	listed, _, err := store.ListFindings(ctx, storepkg.FindingFilter{Namespace: defaultNS, RepositoryScan: "kaset", SliceID: "slice_filter"})
	if err != nil {
		t.Fatalf("ListFindings() error = %v", err)
	}
	if len(listed) != 1 || listed[0].Title != "Unsafe API behavior" {
		t.Fatalf("findings = %#v, want first concrete finding only", listed)
	}
	dropped, _, err := store.ListDroppedFindings(ctx, storepkg.DroppedFindingFilter{Namespace: defaultNS, RepositoryScan: "kaset", ScanRunID: "scan_review_filter", SliceID: "slice_filter"})
	if err != nil {
		t.Fatalf("ListDroppedFindings() error = %v", err)
	}
	var sawFilter, sawCap bool
	for _, item := range dropped {
		if item.Layer == "filter" {
			sawFilter = true
		}
		if item.Layer == "cap" {
			sawCap = true
		}
	}
	if len(dropped) != 2 || !sawFilter || !sawCap {
		t.Fatalf("dropped = %#v, want filter and cap diagnostics", dropped)
	}
}

func TestIngestReviewTaskChecksPolicyDriftBeforeFilteringFindings(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1.AddToScheme() error = %v", err)
	}
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:                   "https://github.com/example/repo",
			AnalysisAgentRef:          corev1alpha1.AgentReference{Name: "scan-reviewer"},
			CustomScanInstructionsRef: &corev1alpha1.PolicyConfigMapKeyRef{Name: "scan-policy"},
		},
	}
	policyConfig := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "scan-policy", Namespace: defaultNS, Labels: map[string]string{security.PolicyConfigMapAllowedLabel: "true"}}, Data: map[string]string{"policy": "changed policy"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan, policyConfig).Build()
	reconciler := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: store, ArtifactStore: store}
	run := &storepkg.ScanRun{ID: "scan_review_drift", Namespace: defaultNS, RepositoryScan: "kaset", TaskName: "kaset-review-drift", Mode: "initial", Phase: scanRunPhaseRunning, PolicyDigest: "sha256:old", StartedAt: time.Now()}
	if err := store.CreateScanRun(ctx, run); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	if err := store.UpsertReviewSlice(ctx, &storepkg.ReviewSlice{SchemaVersion: 1, ID: "slice_docs", Namespace: defaultNS, RepositoryScan: "kaset", Source: "deterministic", Title: "Docs", Kind: "package", OwnedFiles: []storepkg.ReviewSliceFile{{Path: "docs/security.md"}}, Confidence: "high", Status: reviewSliceStatusPending, LastScanRunID: run.ID}); err != nil {
		t.Fatalf("UpsertReviewSlice() error = %v", err)
	}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "kaset-review-drift", Namespace: defaultNS, Labels: map[string]string{labels.LabelSecurityTarget: "kaset", labels.LabelSecurityScanID: run.ID, labels.LabelSecurityMode: "initial", labels.LabelSecurityStage: security.StageReview, labels.LabelSecuritySliceID: "slice_docs"}}, Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded}}
	manifest := security.ReviewContextManifest{SchemaVersion: security.SchemaVersionReviewContext, SliceID: "slice_docs", IncludedFiles: []security.ReviewContextIncludedFile{{Path: "docs/security.md", Role: "owned", IncludedLineRanges: []security.ReviewContextLineRange{{StartLine: 1, EndLine: 5}}, Readable: true}}}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal(manifest) error = %v", err)
	}
	if err := store.SaveArtifact(ctx, task.Namespace, task.Name, security.ReviewContextArtifactName("slice_docs"), "application/json", manifestData); err != nil {
		t.Fatalf("SaveArtifact(manifest) error = %v", err)
	}
	findings := security.FindingsV2Artifact{SchemaVersion: security.SchemaVersionFindingsV2, Repository: security.FindingsV2Repository{RepoURL: "https://github.com/example/repo", Branch: "main"}, Scan: security.FindingsV2Scan{Mode: "initial", SliceID: "slice_docs", Summary: "docs only"}, Findings: []security.FindingsV2Finding{{Title: "Docs-only rate limit", Category: "rate-limit", Severity: "medium", Confidence: "high", Summary: "Documentation says rate limiting is missing.", Remediation: "Document it.", Evidence: []security.FindingsV2EvidenceRef{{Path: "docs/security.md", StartLine: 1, EndLine: 1}}}}}
	findingsData, err := json.Marshal(findings)
	if err != nil {
		t.Fatalf("json.Marshal(findings) error = %v", err)
	}
	if err := store.SaveArtifact(ctx, task.Namespace, task.Name, security.ArtifactFindingsV2, "application/json", findingsData); err != nil {
		t.Fatalf("SaveArtifact(findings) error = %v", err)
	}

	err = reconciler.ingestScanTask(ctx, scan, task)
	if err == nil || !strings.Contains(err.Error(), "scanner policy digest changed") {
		t.Fatalf("ingestScanTask() error = %v, want policy drift", err)
	}
	storedRun, err := store.GetScanRun(ctx, defaultNS, run.ID)
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if storedRun.Phase != scanRunPhaseFailed {
		t.Fatalf("run phase = %q, want failed", storedRun.Phase)
	}
	reviewSlice, err := store.GetReviewSlice(ctx, defaultNS, "kaset", "slice_docs")
	if err != nil {
		t.Fatalf("GetReviewSlice() error = %v", err)
	}
	if reviewSlice.Status == reviewSliceStatusReviewed {
		t.Fatal("review slice was marked reviewed despite policy drift")
	}
}

func TestIngestReviewTaskSkipsStaleSliceRun(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	reconciler := &RepositoryScanReconciler{
		SecurityStore: store,
		ArtifactStore: store,
	}
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
	}
	if err := store.CreateScanRun(ctx, &storepkg.ScanRun{
		ID:             "scan_old_review",
		Namespace:      defaultNS,
		RepositoryScan: "kaset",
		TaskName:       "kaset-review-slice-old",
		Mode:           "initial",
		Phase:          scanRunPhaseRunning,
		BaseCommit:     "old-base",
		HeadCommit:     "old-head",
		StartedAt:      time.Now().Add(-1 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateScanRun(old) error = %v", err)
	}
	if err := store.CreateScanRun(ctx, &storepkg.ScanRun{
		ID:             "scan_new_review",
		Namespace:      defaultNS,
		RepositoryScan: "kaset",
		Mode:           scanModeIncremental,
		Phase:          scanRunPhaseRunning,
		BaseCommit:     "new-base",
		HeadCommit:     "new-head",
		StartedAt:      time.Now(),
	}); err != nil {
		t.Fatalf("CreateScanRun(new) error = %v", err)
	}
	if err := store.UpsertReviewSlice(ctx, &storepkg.ReviewSlice{
		SchemaVersion:  1,
		ID:             "slice_api",
		Namespace:      defaultNS,
		RepositoryScan: "kaset",
		Source:         "deterministic-go-package",
		Title:          "Go package internal/api",
		Summary:        "API handlers",
		Kind:           "package",
		OwnedFiles:     []storepkg.ReviewSliceFile{{Path: "internal/api/security.go", Reason: "source"}},
		Confidence:     "high",
		Status:         reviewSliceStatusPending,
		LastScanRunID:  "scan_new_review",
	}); err != nil {
		t.Fatalf("UpsertReviewSlice() error = %v", err)
	}

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kaset-review-slice-old",
			Namespace: defaultNS,
			Labels: map[string]string{
				labels.LabelSecurityTarget:  "kaset",
				labels.LabelSecurityScanID:  "scan_old_review",
				labels.LabelSecurityMode:    "initial",
				labels.LabelSecurityStage:   security.StageReview,
				labels.LabelSecuritySliceID: "slice_api",
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
	}
	manifest := security.ReviewContextManifest{
		SchemaVersion: security.SchemaVersionReviewContext,
		SliceID:       "slice_api",
		IncludedFiles: []security.ReviewContextIncludedFile{{
			Path:               "internal/api/security.go",
			Role:               "owned",
			IncludedLineRanges: []security.ReviewContextLineRange{{StartLine: 1, EndLine: 20}},
			Readable:           true,
		}},
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal(manifest) error = %v", err)
	}
	if err := store.SaveArtifact(ctx, task.Namespace, task.Name, security.ReviewContextArtifactName("slice_api"), "application/json", manifestData); err != nil {
		t.Fatalf("SaveArtifact(manifest) error = %v", err)
	}
	findings := security.FindingsV2Artifact{
		SchemaVersion: security.SchemaVersionFindingsV2,
		Repository: security.FindingsV2Repository{
			RepoURL: "https://github.com/example/repo",
			Branch:  "main",
		},
		Scan: security.FindingsV2Scan{Mode: "initial", SliceID: "slice_api", Summary: "stale review output"},
		Findings: []security.FindingsV2Finding{{
			Title:       "Unsafe API behavior",
			Category:    "authz",
			Severity:    "high",
			Confidence:  "high",
			Summary:     "API path lacks authorization.",
			Remediation: "Add authorization checks.",
			Evidence: []security.FindingsV2EvidenceRef{{
				Path:      "internal/api/security.go",
				StartLine: 5,
				EndLine:   8,
			}},
		}},
	}
	findingsData, err := json.Marshal(findings)
	if err != nil {
		t.Fatalf("json.Marshal(findings) error = %v", err)
	}
	if err := store.SaveArtifact(ctx, task.Namespace, task.Name, security.ArtifactFindingsV2, "application/json", findingsData); err != nil {
		t.Fatalf("SaveArtifact(findings v2) error = %v", err)
	}

	if err := reconciler.ingestScanTask(ctx, scan, task); err != nil {
		t.Fatalf("ingestScanTask() error = %v", err)
	}
	run, err := store.GetScanRun(ctx, defaultNS, "scan_old_review")
	if err != nil {
		t.Fatalf("GetScanRun(old) error = %v", err)
	}
	if run.ReviewedSliceCount != 0 || run.AcceptedFindings != 0 || run.DroppedFindings != 0 {
		t.Fatalf("old run counts = reviewed:%d accepted:%d dropped:%d, want unchanged", run.ReviewedSliceCount, run.AcceptedFindings, run.DroppedFindings)
	}
	reviewSlice, err := store.GetReviewSlice(ctx, defaultNS, "kaset", "slice_api")
	if err != nil {
		t.Fatalf("GetReviewSlice() error = %v", err)
	}
	if reviewSlice.LastScanRunID != "scan_new_review" || reviewSlice.Status != reviewSliceStatusPending {
		t.Fatalf("review slice = %#v, want current run pending slice unchanged", reviewSlice)
	}
	listed, _, err := store.ListFindings(ctx, storepkg.FindingFilter{Namespace: defaultNS, RepositoryScan: "kaset", SliceID: "slice_api"})
	if err != nil {
		t.Fatalf("ListFindings() error = %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("len(findings) = %d, want stale task findings ignored", len(listed))
	}
}

func TestPersistThreatModelIfChangedSkipsOlderGeneratedRun(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)

	reconciler := &RepositoryScanReconciler{SecurityStore: store}
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
	}

	if err := store.SaveThreatModel(ctx, &storepkg.ThreatModel{
		Namespace:      defaultNS,
		RepositoryScan: "kaset",
		Content:        "# Clean Threat Model\n\nCurated content",
		Source:         "cleaned",
	}); err != nil {
		t.Fatalf("SaveThreatModel() error = %v", err)
	}

	latest, err := store.GetLatestThreatModel(ctx, defaultNS, "kaset")
	if err != nil {
		t.Fatalf("GetLatestThreatModel() error = %v", err)
	}

	if err := reconciler.persistThreatModelIfChanged(
		ctx,
		scan,
		"scan_old",
		latest.UpdatedAt.Add(-time.Minute),
		"# Generated Threat Model\n\nOlder scan output",
	); err != nil {
		t.Fatalf("persistThreatModelIfChanged() error = %v", err)
	}

	latest, err = store.GetLatestThreatModel(ctx, defaultNS, "kaset")
	if err != nil {
		t.Fatalf("GetLatestThreatModel() error = %v", err)
	}
	if latest.Source != "cleaned" {
		t.Fatalf("latest.Source = %q, want cleaned", latest.Source)
	}
	if !strings.Contains(latest.Content, "Curated content") {
		t.Fatalf("latest.Content = %q, want cleaned threat model", latest.Content)
	}
}

func TestPersistThreatModelIfChangedPromotesNewerGeneratedRun(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)

	reconciler := &RepositoryScanReconciler{SecurityStore: store}
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
	}

	if err := store.SaveThreatModel(ctx, &storepkg.ThreatModel{
		Namespace:      defaultNS,
		RepositoryScan: "kaset",
		Content:        "# Clean Threat Model\n\nCurated content",
		Source:         "cleaned",
	}); err != nil {
		t.Fatalf("SaveThreatModel() error = %v", err)
	}

	latest, err := store.GetLatestThreatModel(ctx, defaultNS, "kaset")
	if err != nil {
		t.Fatalf("GetLatestThreatModel() error = %v", err)
	}

	if err := reconciler.persistThreatModelIfChanged(
		ctx,
		scan,
		"scan_new",
		latest.UpdatedAt.Add(time.Minute),
		"# Generated Threat Model\n\nFresh scan output",
	); err != nil {
		t.Fatalf("persistThreatModelIfChanged() error = %v", err)
	}

	latest, err = store.GetLatestThreatModel(ctx, defaultNS, "kaset")
	if err != nil {
		t.Fatalf("GetLatestThreatModel() error = %v", err)
	}
	if latest.Source != "generated" {
		t.Fatalf("latest.Source = %q, want generated", latest.Source)
	}
	if latest.GeneratedByScan != "scan_new" {
		t.Fatalf("latest.GeneratedByScan = %q, want scan_new", latest.GeneratedByScan)
	}
	if !strings.Contains(latest.Content, "Fresh scan output") {
		t.Fatalf("latest.Content = %q, want new generated threat model", latest.Content)
	}
}

func TestLoadThreatModelArtifactRejectsToolTranscript(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)

	reconciler := &RepositoryScanReconciler{ArtifactStore: store}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kaset-threat-model-transcript",
			Namespace: defaultNS,
		},
	}

	transcript := `<tool_call><tool_name>shell</tool_name><parameters><command>cat > /workspace/.orka-artifacts/security-threat-model.md <<'EOF'
# Threat Model
EOF
</command></parameters></tool_call>`
	if err := store.SaveArtifact(ctx, task.Namespace, task.Name, security.ArtifactThreatModel, "text/markdown", []byte(transcript)); err != nil {
		t.Fatalf("SaveArtifact() error = %v", err)
	}

	content, validationProblem, err := reconciler.loadThreatModelArtifact(ctx, task)
	if err != nil {
		t.Fatalf("loadThreatModelArtifact() error = %v", err)
	}
	if content != "" {
		t.Fatalf("content = %q, want empty for invalid threat model artifact", content)
	}
	if !strings.Contains(validationProblem, "tool transcript") {
		t.Fatalf("validationProblem = %q, want tool transcript warning", validationProblem)
	}
}

func TestIngestValidationTaskUpdatesFindingValidationDetails(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)

	reconciler := &RepositoryScanReconciler{
		SecurityStore: store,
		ArtifactStore: store,
		ResultStore:   store,
	}
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
	}
	finding := &storepkg.Finding{
		ID:               "fnd_validate",
		Namespace:        defaultNS,
		RepositoryScan:   "kaset",
		ScanRunID:        "scan_validate",
		Fingerprint:      "sha256:test",
		Title:            "Validation target",
		Summary:          "candidate finding",
		Severity:         "high",
		Confidence:       "high",
		ValidationStatus: "unvalidated",
		State:            findingStateOpen,
	}
	if err := store.UpsertFinding(ctx, finding); err != nil {
		t.Fatalf("UpsertFinding() error = %v", err)
	}

	validation := security.ValidationArtifact{
		Version:            1,
		FindingID:          finding.ID,
		Status:             findingValidationStatusValidated,
		Summary:            "Confirmed injection path",
		ValidationSteps:    []string{"Trace input to shell execution", "Confirm shell metacharacters are preserved"},
		AttackPathAnalysis: "Attacker controls package names which reach shell execution.",
		Evidence: []storepkg.FindingEvidenceRef{
			{Kind: "artifact", Name: "security-validation.txt", Label: "Validation transcript"},
		},
	}
	data, err := json.Marshal(validation)
	if err != nil {
		t.Fatalf("json.Marshal(validation) error = %v", err)
	}

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kaset-validation-fnd_validate",
			Namespace: defaultNS,
			Labels: map[string]string{
				labels.LabelSecurityTarget:    "kaset",
				labels.LabelSecurityFindingID: finding.ID,
				labels.LabelSecurityStage:     security.StageValidation,
				labels.LabelSecurityMode:      security.StageValidation,
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
	}

	if err := store.SaveArtifact(ctx, task.Namespace, task.Name, security.ArtifactValidation, "application/json", data); err != nil {
		t.Fatalf("SaveArtifact(validation) error = %v", err)
	}
	if err := store.SaveArtifact(ctx, task.Namespace, task.Name, security.ArtifactValidationText, "text/plain", []byte("validation transcript")); err != nil {
		t.Fatalf("SaveArtifact(validation transcript) error = %v", err)
	}

	if err := reconciler.ingestValidationTask(ctx, scan, task); err != nil {
		t.Fatalf("ingestValidationTask() error = %v", err)
	}

	updated, err := store.GetFinding(ctx, defaultNS, finding.ID)
	if err != nil {
		t.Fatalf("GetFinding() error = %v", err)
	}
	if updated.ValidationStatus != findingValidationStatusValidated {
		t.Fatalf("ValidationStatus = %q, want validated", updated.ValidationStatus)
	}
	if !strings.Contains(updated.ValidationJSON, "Confirmed injection path") {
		t.Fatalf("ValidationJSON = %q, want validation summary", updated.ValidationJSON)
	}
	if len(updated.Evidence) < 2 {
		t.Fatalf("len(Evidence) = %d, want at least 2 refs", len(updated.Evidence))
	}
	foundTranscript := false
	for _, ref := range updated.Evidence {
		if ref.Name == security.ArtifactValidationText && ref.TaskName == task.Name {
			foundTranscript = true
			break
		}
	}
	if !foundTranscript {
		t.Fatalf("updated.Evidence = %#v, want validation transcript artifact ref with task name", updated.Evidence)
	}
}

func TestProgressLatestScanRunUsesNewestOwnedScanWhenStatusIsStale(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1alpha1.GroupVersion.String(),
			Kind:       "RepositoryScan",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:          "https://github.com/sozercan/kaset",
			Branch:           "main",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "scan-opus46-reviewer"},
		},
		Status: corev1alpha1.RepositoryScanStatus{
			LastScanID: "scan_old",
			Phase:      repositoryScanPhaseError,
		},
	}

	oldTask := &corev1alpha1.Task{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1alpha1.GroupVersion.String(),
			Kind:       "Task",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "kaset-incremental-old",
			Namespace:         defaultNS,
			CreationTimestamp: metav1.NewTime(mustParseTime(t, "2026-04-10T05:00:00Z")),
			Labels: map[string]string{
				labels.LabelSecurityTarget: "kaset",
				labels.LabelSecurityScanID: "scan_old",
			},
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded,
		},
	}

	newTask := &corev1alpha1.Task{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1alpha1.GroupVersion.String(),
			Kind:       "Task",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "kaset-manual-threat-model-new",
			Namespace:         defaultNS,
			CreationTimestamp: metav1.NewTime(mustParseTime(t, "2026-04-10T05:05:00Z")),
			Labels: map[string]string{
				labels.LabelSecurityTarget: "kaset",
				labels.LabelSecurityScanID: "scan_new",
				labels.LabelSecurityMode:   "manual",
				labels.LabelSecurityStage:  security.StageThreatModel,
			},
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded,
		},
	}
	mapperTask := &corev1alpha1.Task{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1alpha1.GroupVersion.String(),
			Kind:       "Task",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "kaset-manual-mapper-new",
			Namespace:         defaultNS,
			CreationTimestamp: metav1.NewTime(mustParseTime(t, "2026-04-10T05:06:00Z")),
			Labels: map[string]string{
				labels.LabelSecurityTarget: "kaset",
				labels.LabelSecurityScanID: "scan_new",
				labels.LabelSecurityMode:   "manual",
				labels.LabelSecurityStage:  security.StageMapper,
			},
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded,
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(scan, oldTask, newTask, mapperTask).
		Build()

	reconciler := &RepositoryScanReconciler{
		Client:        cl,
		Scheme:        scheme,
		SecurityStore: store,
	}

	if err := store.CreateScanRun(ctx, &storepkg.ScanRun{
		ID:             "scan_new",
		Namespace:      defaultNS,
		RepositoryScan: "kaset",
		TaskName:       newTask.Name,
		Mode:           "manual",
		Phase:          scanRunPhasePending,
		StartedAt:      newTask.CreationTimestamp.Time,
	}); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}

	progressed, err := reconciler.progressLatestScanRun(ctx, scan)
	if err != nil {
		t.Fatalf("progressLatestScanRun() error = %v", err)
	}
	if !progressed {
		t.Fatal("progressLatestScanRun() = false, want true")
	}

	run, err := store.GetScanRun(ctx, defaultNS, "scan_new")
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if run.Phase != scanRunPhaseSucceeded || run.CompletedAt == nil {
		t.Fatalf("run phase/completedAt = %q/%v, want succeeded with completion", run.Phase, run.CompletedAt)
	}
}

func TestCreateScanRunIsIdempotentWhenTaskAlreadyExists(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1alpha1.GroupVersion.String(),
			Kind:       "RepositoryScan",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo-security-repository-20260425175643",
			Namespace: defaultNS,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:          "https://github.com/sozercan/actions-test.git",
			Branch:           "demo/security-python-command-injection",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "demo-security-analysis"},
		},
		Status: corev1alpha1.RepositoryScanStatus{
			Phase: repositoryScanPhasePending,
		},
	}

	taskName := security.ScanStageTaskName(scan.Name, "initial", security.StageThreatModel, "")
	scanID := security.ScanRunID(taskName)
	timeout := metav1.Duration{Duration: 2 * time.Hour}
	priority := int32(700)
	existingTask := &corev1alpha1.Task{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1alpha1.GroupVersion.String(),
			Kind:       "Task",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskName,
			Namespace: scan.Namespace,
			Labels: map[string]string{
				labels.LabelManaged:        "true",
				labels.LabelCreatedBy:      "repository-security",
				labels.LabelSecurityTarget: labels.SelectorValue(scan.Name),
				labels.LabelSecurityScanID: scanID,
				labels.LabelSecurityMode:   "initial",
				labels.LabelSecurityStage:  security.StageThreatModel,
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAgent,
			AgentRef: &scan.Spec.AnalysisAgentRef,
			Prompt:   security.BuildThreatModelPrompt(scan, "initial", "", "", ""),
			Timeout:  &timeout,
			Priority: &priority,
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(scan, existingTask).
		Build()

	reconciler := &RepositoryScanReconciler{
		Client:        cl,
		Scheme:        scheme,
		SecurityStore: store,
	}

	if err := reconciler.createScanRun(ctx, scan, "initial", "", ""); err != nil {
		t.Fatalf("createScanRun() error = %v", err)
	}

	run, err := store.GetScanRun(ctx, scan.Namespace, scanID)
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if run.TaskName != taskName {
		t.Fatalf("run.TaskName = %q, want %q", run.TaskName, taskName)
	}
	if run.Phase != scanRunPhasePending {
		t.Fatalf("run.Phase = %q, want pending", run.Phase)
	}

	current := &corev1alpha1.RepositoryScan{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		t.Fatalf("Get(scan) error = %v", err)
	}
	if current.Status.Phase != repositoryScanPhaseScanning {
		t.Fatalf("scan.Status.Phase = %q, want %q", current.Status.Phase, repositoryScanPhaseScanning)
	}
	if current.Status.LastScanID != scanID {
		t.Fatalf("scan.Status.LastScanID = %q, want %q", current.Status.LastScanID, scanID)
	}
	if current.Status.LastScanTaskName != taskName {
		t.Fatalf("scan.Status.LastScanTaskName = %q, want %q", current.Status.LastScanTaskName, taskName)
	}
}

type patchIngestFixture struct {
	store      *sqlitestore.Store
	reconciler *RepositoryScanReconciler
	scan       *corev1alpha1.RepositoryScan
	finding    *storepkg.Finding
	proposal   *storepkg.PatchProposal
}

func newPatchIngestFixture(t *testing.T, id string) patchIngestFixture {
	t.Helper()
	ctx := context.Background()
	securityStore := setupControllerSQLiteStore(t)

	findingID := "fnd_patch_" + id
	taskName := "kaset-patch-" + id
	branch := "orka/security/" + findingID
	fixture := patchIngestFixture{
		store: securityStore,
		reconciler: &RepositoryScanReconciler{
			SecurityStore: securityStore,
			ArtifactStore: securityStore,
			ResultStore:   securityStore,
		},
		scan: &corev1alpha1.RepositoryScan{
			ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
		},
		finding: &storepkg.Finding{
			ID:               findingID,
			Namespace:        defaultNS,
			RepositoryScan:   "kaset",
			ScanRunID:        "scan_patch",
			Fingerprint:      "sha256:patch-" + id,
			Title:            "Patch target",
			Summary:          "candidate finding",
			Severity:         "high",
			Confidence:       "high",
			ValidationStatus: "validated",
			State:            findingStatePatchPending,
		},
		proposal: &storepkg.PatchProposal{
			ID:             "patch_" + id,
			Namespace:      defaultNS,
			RepositoryScan: "kaset",
			FindingID:      findingID,
			TaskName:       taskName,
			Branch:         branch,
			Status:         scanRunPhasePending,
			CreatedAt:      time.Now(),
			UpdatedAt:      time.Now(),
		},
	}
	if err := securityStore.UpsertFinding(ctx, fixture.finding); err != nil {
		t.Fatalf("UpsertFinding() error = %v", err)
	}
	if err := securityStore.CreatePatchProposal(ctx, fixture.proposal); err != nil {
		t.Fatalf("CreatePatchProposal() error = %v", err)
	}
	return fixture
}

func patchTaskForFixture(fixture patchIngestFixture, resultAvailable bool) *corev1alpha1.Task {
	diffName := fmt.Sprintf("security-patch-%s.diff", fixture.finding.ID)
	summaryName := fmt.Sprintf("security-patch-%s.json", fixture.finding.ID)
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fixture.proposal.TaskName,
			Namespace: fixture.proposal.Namespace,
			Labels: map[string]string{
				labels.LabelSecurityTarget:    fixture.scan.Name,
				labels.LabelSecurityFindingID: fixture.finding.ID,
				labels.LabelSecurityStage:     security.StagePatch,
				labels.LabelSecurityMode:      security.StagePatch,
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Prompt: fmt.Sprintf("REQUIRED_SECURITY_ARTIFACTS: %s, %s\n", diffName, summaryName),
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					PushBranch: fixture.proposal.Branch,
				},
			},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseSucceeded,
			ResultRef: &corev1alpha1.ResultReference{Available: resultAvailable},
		},
	}
}

func savePatchStructuredResult(t *testing.T, fixture patchIngestFixture, sr *common.StructuredResult) {
	t.Helper()
	result, err := common.FormatStructuredResult(sr)
	if err != nil {
		t.Fatalf("FormatStructuredResult() error = %v", err)
	}
	if err := fixture.store.SaveResult(context.Background(), fixture.proposal.Namespace, fixture.proposal.TaskName, result); err != nil {
		t.Fatalf("SaveResult() error = %v", err)
	}
}

func savePatchArtifacts(t *testing.T, fixture patchIngestFixture, diff string, changedFiles []string) {
	t.Helper()
	ctx := context.Background()
	diffName := fmt.Sprintf("security-patch-%s.diff", fixture.finding.ID)
	summaryName := fmt.Sprintf("security-patch-%s.json", fixture.finding.ID)
	if err := fixture.store.SaveArtifact(ctx, fixture.proposal.Namespace, fixture.proposal.TaskName, diffName, "text/x-diff", []byte(diff)); err != nil {
		t.Fatalf("SaveArtifact(diff) error = %v", err)
	}
	summary := security.PatchSummaryArtifact{
		SchemaVersion: security.SchemaVersionPatchSummary,
		FindingID:     fixture.finding.ID,
		Summary:       "patched successfully",
		ChangedFiles:  changedFiles,
		Risk:          "low",
	}
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("json.Marshal(summary) error = %v", err)
	}
	if err := fixture.store.SaveArtifact(ctx, fixture.proposal.Namespace, fixture.proposal.TaskName, summaryName, "application/json", data); err != nil {
		t.Fatalf("SaveArtifact(summary) error = %v", err)
	}
}

func assertPatchIngestState(t *testing.T, fixture patchIngestFixture, wantProposalStatus, wantFindingState string) {
	t.Helper()
	proposals, err := fixture.store.ListPatchProposals(context.Background(), fixture.proposal.Namespace, fixture.finding.ID)
	if err != nil {
		t.Fatalf("ListPatchProposals() error = %v", err)
	}
	if len(proposals) != 1 {
		t.Fatalf("len(proposals) = %d, want 1", len(proposals))
	}
	if proposals[0].Status != wantProposalStatus {
		t.Fatalf("proposal.Status = %q, want %q", proposals[0].Status, wantProposalStatus)
	}
	updatedFinding, err := fixture.store.GetFinding(context.Background(), fixture.proposal.Namespace, fixture.finding.ID)
	if err != nil {
		t.Fatalf("GetFinding() error = %v", err)
	}
	if updatedFinding.State != wantFindingState {
		t.Fatalf("finding.State = %q, want %q", updatedFinding.State, wantFindingState)
	}
}

func TestIngestPatchTaskMarksPatchReadyAfterConfirmedPush(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "ready")
	diff := "diff --git a/app.py b/app.py"
	savePatchStructuredResult(t, fixture, &common.StructuredResult{
		Summary:    "patched successfully",
		Diff:       diff,
		Files:      []string{"app.py"},
		PushBranch: fixture.proposal.Branch,
	})
	savePatchArtifacts(t, fixture, diff, []string{"app.py"})

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, patchTaskForFixture(fixture, true)); err != nil {
		t.Fatalf("ingestPatchTask() error = %v", err)
	}
	assertPatchIngestState(t, fixture, scanRunPhaseSucceeded, findingStatePatchReady)
}

func TestIngestPatchTaskAcceptsDiffArtifactWithDifferentIndexFormatting(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "diff-index-format")
	actualDiff := strings.Join([]string{
		"diff --git a/app.py b/app.py",
		"index 1111111111111111111111111111111111111111..2222222222222222222222222222222222222222 100644",
		"--- a/app.py",
		"+++ b/app.py",
		"@@ -1 +1 @@",
		"-unsafe()",
		"+safe()",
		"",
	}, "\n")
	artifactDiff := strings.Join([]string{
		"diff --git a/app.py b/app.py",
		"index 1111111..2222222 100644",
		"--- a/app.py",
		"+++ b/app.py",
		"@@ -1 +1 @@",
		"-unsafe()",
		"+safe()",
		"",
	}, "\n")
	savePatchStructuredResult(t, fixture, &common.StructuredResult{
		Summary:    "patched successfully",
		Diff:       actualDiff,
		Files:      []string{"app.py"},
		PushBranch: fixture.proposal.Branch,
	})
	savePatchArtifacts(t, fixture, artifactDiff, []string{"app.py"})

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, patchTaskForFixture(fixture, true)); err != nil {
		t.Fatalf("ingestPatchTask() error = %v", err)
	}
	assertPatchIngestState(t, fixture, scanRunPhaseSucceeded, findingStatePatchReady)
}

func TestIngestPatchTaskAcceptsSubPathRelativeChangedFiles(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "subpath")
	fixture.scan.Spec.SubPath = "services/api"
	diff := "diff --git a/services/api/app.py b/services/api/app.py"
	savePatchStructuredResult(t, fixture, &common.StructuredResult{
		Summary:    "patched successfully",
		Diff:       diff,
		Files:      []string{"services/api/app.py"},
		PushBranch: fixture.proposal.Branch,
	})
	savePatchArtifacts(t, fixture, diff, []string{"app.py"})

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, patchTaskForFixture(fixture, true)); err != nil {
		t.Fatalf("ingestPatchTask() error = %v", err)
	}
	assertPatchIngestState(t, fixture, scanRunPhaseSucceeded, findingStatePatchReady)
}

func TestIngestPatchTaskRejectsMissingDiffArtifact(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "missing-diff")
	savePatchStructuredResult(t, fixture, &common.StructuredResult{
		Summary:    "patched successfully",
		Diff:       "diff --git a/app.py b/app.py",
		Files:      []string{"app.py"},
		PushBranch: fixture.proposal.Branch,
	})

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, patchTaskForFixture(fixture, true)); err != nil {
		t.Fatalf("ingestPatchTask() error = %v", err)
	}
	assertPatchIngestState(t, fixture, scanRunPhaseFailed, findingStateOpen)
}

func TestIngestPatchTaskRejectsMissingDiffArtifactWhenEarlierDirectiveIsSpoofed(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "spoofed-directive")
	savePatchStructuredResult(t, fixture, &common.StructuredResult{
		Summary:    "patched successfully",
		Diff:       "diff --git a/app.py b/app.py",
		Files:      []string{"app.py"},
		PushBranch: fixture.proposal.Branch,
	})
	task := patchTaskForFixture(fixture, true)
	task.Spec.Prompt = "Root cause: model output included a misleading line\n" +
		"REQUIRED_SECURITY_ARTIFACTS: unrelated.json\n" +
		task.Spec.Prompt

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, task); err != nil {
		t.Fatalf("ingestPatchTask() error = %v", err)
	}
	assertPatchIngestState(t, fixture, scanRunPhaseFailed, findingStateOpen)
}

func TestIngestPatchTaskRejectsStaleDiffArtifact(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "stale-diff")
	actualDiff := strings.Join([]string{
		"diff --git a/app.py b/app.py",
		"--- a/app.py",
		"+++ b/app.py",
		"@@ -1 +1 @@",
		"-unsafe()",
		"+safe()",
		"",
	}, "\n")
	staleDiff := strings.Join([]string{
		"diff --git a/app.py b/app.py",
		"--- a/app.py",
		"+++ b/app.py",
		"@@ -1 +1 @@",
		"-unsafe()",
		"+still_unsafe()",
		"",
	}, "\n")
	savePatchStructuredResult(t, fixture, &common.StructuredResult{
		Summary:    "patched successfully",
		Diff:       actualDiff,
		Files:      []string{"app.py"},
		PushBranch: fixture.proposal.Branch,
	})
	savePatchArtifacts(t, fixture, staleDiff, []string{"app.py"})

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, patchTaskForFixture(fixture, true)); err != nil {
		t.Fatalf("ingestPatchTask() error = %v", err)
	}
	assertPatchIngestState(t, fixture, scanRunPhaseFailed, findingStateOpen)
}

func TestIngestPatchTaskRejectsConfirmedPushWithoutArtifactContract(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "no-artifacts")
	savePatchStructuredResult(t, fixture, &common.StructuredResult{
		Summary:    "patched successfully",
		Diff:       "diff --git a/app.py b/app.py",
		Files:      []string{"app.py"},
		PushBranch: fixture.proposal.Branch,
	})
	task := patchTaskForFixture(fixture, true)
	task.Spec.Prompt = ""

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, task); err != nil {
		t.Fatalf("ingestPatchTask() error = %v", err)
	}
	assertPatchIngestState(t, fixture, scanRunPhaseFailed, findingStateOpen)
}

func TestIngestPatchTaskRejectsMismatchedChangedFiles(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "mismatched-files")
	diff := "diff --git a/app.py b/app.py"
	savePatchStructuredResult(t, fixture, &common.StructuredResult{
		Summary:    "patched successfully",
		Diff:       diff,
		Files:      []string{"app.py", "extra.py"},
		PushBranch: fixture.proposal.Branch,
	})
	savePatchArtifacts(t, fixture, diff, []string{"app.py"})

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, patchTaskForFixture(fixture, true)); err != nil {
		t.Fatalf("ingestPatchTask() error = %v", err)
	}
	assertPatchIngestState(t, fixture, scanRunPhaseFailed, findingStateOpen)
}

func TestIngestPatchTaskFailsSucceededTaskWhenPushFails(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "failed")
	savePatchStructuredResult(t, fixture, &common.StructuredResult{
		Summary:   "patch created but push failed",
		Diff:      "diff --git a/app.py b/app.py",
		PushError: "git push failed: remote rejected",
	})

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, patchTaskForFixture(fixture, true)); err != nil {
		t.Fatalf("ingestPatchTask() error = %v", err)
	}
	assertPatchIngestState(t, fixture, scanRunPhaseFailed, findingStateOpen)
}

func TestIngestPatchTaskFailsSucceededTaskWithoutConfirmedPushBranch(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "missing-push")
	savePatchStructuredResult(t, fixture, &common.StructuredResult{
		Summary: "patch created without confirmed push",
		Diff:    "diff --git a/app.py b/app.py",
	})

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, patchTaskForFixture(fixture, true)); err != nil {
		t.Fatalf("ingestPatchTask() error = %v", err)
	}
	assertPatchIngestState(t, fixture, scanRunPhaseFailed, findingStateOpen)
}

func TestIngestPatchTaskKeepsPatchPendingUntilResultIsAvailable(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "pending-ref")

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, patchTaskForFixture(fixture, false)); err != nil {
		t.Fatalf("ingestPatchTask() error = %v", err)
	}
	assertPatchIngestState(t, fixture, scanRunPhasePending, findingStatePatchPending)
}

func TestIngestPatchTaskKeepsPatchPendingUntilResultExists(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "pending-result")

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, patchTaskForFixture(fixture, true)); err != nil {
		t.Fatalf("ingestPatchTask() error = %v", err)
	}
	assertPatchIngestState(t, fixture, scanRunPhasePending, findingStatePatchPending)
}

func TestRefreshScanRunStatusSetsLastScanAtOnFailedRun(t *testing.T) {
	ctx := context.Background()
	secStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: "ts-fail", Namespace: defaultNS},
		Spec:       corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "a"}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan).Build()
	r := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: secStore}

	completed := mustParseTime(t, "2026-05-07T22:41:22Z")
	run := &storepkg.ScanRun{ID: "scan_f", Namespace: defaultNS, RepositoryScan: "ts-fail", TaskName: "t", Mode: "initial", Phase: scanRunPhaseFailed, StartedAt: completed, CompletedAt: &completed, ErrorMessage: "failed", HeadCommit: "abc"}
	if err := secStore.CreateScanRun(ctx, run); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	if err := r.refreshScanRunStatus(ctx, scan, run, run.ID, true); err != nil {
		t.Fatalf("refreshScanRunStatus() error = %v", err)
	}

	current := &corev1alpha1.RepositoryScan{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		t.Fatalf("cl.Get() error = %v", err)
	}
	if current.Status.LastScanAt == nil || !current.Status.LastScanAt.Time.Equal(completed) {
		t.Fatalf("LastScanAt = %v, want %v", current.Status.LastScanAt, completed)
	}
	if current.Status.LastSuccessfulScanAt != nil {
		t.Fatalf("LastSuccessfulScanAt = %v, want nil for failed scan", current.Status.LastSuccessfulScanAt)
	}
}

func TestRefreshScanRunStatusSetsBothTimestampsOnSuccess(t *testing.T) {
	ctx := context.Background()
	secStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: "ts-ok", Namespace: defaultNS},
		Spec:       corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "a"}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan).Build()
	r := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: secStore}

	completed := mustParseTime(t, "2026-05-07T23:00:00Z")
	run := &storepkg.ScanRun{ID: "scan_s", Namespace: defaultNS, RepositoryScan: "ts-ok", TaskName: "t", Mode: "initial", Phase: scanRunPhaseSucceeded, StartedAt: completed, CompletedAt: &completed, HeadCommit: "def"}
	if err := secStore.CreateScanRun(ctx, run); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	if err := r.refreshScanRunStatus(ctx, scan, run, run.ID, true); err != nil {
		t.Fatalf("refreshScanRunStatus() error = %v", err)
	}

	current := &corev1alpha1.RepositoryScan{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		t.Fatalf("cl.Get() error = %v", err)
	}
	if current.Status.LastScanAt == nil || !current.Status.LastScanAt.Time.Equal(completed) {
		t.Fatalf("LastScanAt = %v, want %v", current.Status.LastScanAt, completed)
	}
	if current.Status.LastSuccessfulScanAt == nil || !current.Status.LastSuccessfulScanAt.Time.Equal(completed) {
		t.Fatalf("LastSuccessfulScanAt = %v, want %v", current.Status.LastSuccessfulScanAt, completed)
	}
}

func setupControllerSQLiteStore(t *testing.T) *sqlitestore.Store {
	t.Helper()

	db, err := sqlitestore.NewDB(":memory:")
	if err != nil {
		t.Fatalf("sqlite.NewDB(:memory:) error = %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})
	return sqlitestore.NewStore(db, ":memory:")
}
func TestShouldAutoValidateFindingHonorsModeAndThresholds(t *testing.T) {
	reconciler := &RepositoryScanReconciler{}
	maxOne := int32(1)
	scan := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{
		ValidationMode:              "light",
		ValidationMaxFindingsPerRun: &maxOne,
		ValidationMinSeverity:       "medium",
		ValidationMinConfidence:     "medium",
	}}
	finding := &storepkg.Finding{Severity: "medium", Confidence: "low"}
	if !reconciler.shouldAutoValidateFinding(scan, finding, 0) {
		t.Fatal("shouldAutoValidateFinding() = false, want true for medium severity threshold")
	}
	if reconciler.shouldAutoValidateFinding(scan, finding, 1) {
		t.Fatal("shouldAutoValidateFinding() = true, want false after validation cap")
	}
	scan.Spec.ValidationMode = "off"
	if reconciler.shouldAutoValidateFinding(scan, finding, 0) {
		t.Fatal("shouldAutoValidateFinding() = true, want false when validation is off")
	}
	scan.Spec.ValidationMode = "full"
	scan.Spec.ValidationMinSeverity = ""
	scan.Spec.ValidationMinConfidence = ""
	finding.Severity = "critical"
	finding.Confidence = "low"
	if !reconciler.shouldAutoValidateFinding(scan, finding, 99) {
		t.Fatal("shouldAutoValidateFinding() = false, want true for default full mode regardless of light cap")
	}
	scan.Spec.ValidationMinSeverity = "high"
	scan.Spec.ValidationMinConfidence = "medium"
	if reconciler.shouldAutoValidateFinding(scan, finding, 0) {
		t.Fatal("shouldAutoValidateFinding() = true, want false below full-mode severity threshold")
	}
	finding.Severity = "critical"
	finding.Confidence = "medium"
	if !reconciler.shouldAutoValidateFinding(scan, finding, 99) {
		t.Fatal("shouldAutoValidateFinding() = false, want true for full mode above thresholds regardless of per-task cap")
	}
}

func TestEnqueueAutoValidationTasksHonorsRunCapAcrossExistingTasks(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	maxOne := int32(1)
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:                     "https://github.com/example/repo",
			AnalysisAgentRef:            corev1alpha1.AgentReference{Name: "scan-reviewer"},
			ValidationMode:              "light",
			ValidationMaxFindingsPerRun: &maxOne,
			ValidationMinSeverity:       "high",
			ValidationMinConfidence:     "high",
		},
	}
	existing := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-validation",
			Namespace: defaultNS,
			Labels: map[string]string{
				labels.LabelSecurityTarget: labels.SelectorValue(scan.Name),
				labels.LabelSecurityStage:  security.StageValidation,
				labels.LabelSecurityScanID: "scan_run",
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(scan, existing).Build()
	reconciler := &RepositoryScanReconciler{Client: cl, Scheme: scheme}
	findings := []*storepkg.Finding{{ID: "fnd_new", Namespace: defaultNS, RepositoryScan: "kaset", ScanRunID: "scan_run", Severity: "critical", Confidence: "high"}}
	if err := reconciler.enqueueAutoValidationTasks(ctx, scan, findings); err != nil {
		t.Fatalf("enqueueAutoValidationTasks() error = %v", err)
	}
	var tasks corev1alpha1.TaskList
	if err := cl.List(ctx, &tasks, client.InNamespace(defaultNS)); err != nil {
		t.Fatalf("List(Task) error = %v", err)
	}
	if len(tasks.Items) != 1 {
		t.Fatalf("validation tasks = %d, want existing task only due run cap", len(tasks.Items))
	}
}

func TestRepositoryScanPolicyDigestDriftFailsReviewTaskCreation(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1.AddToScheme() error = %v", err)
	}
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:                   "https://github.com/example/repo",
			AnalysisAgentRef:          corev1alpha1.AgentReference{Name: "scan-reviewer"},
			CustomScanInstructionsRef: &corev1alpha1.PolicyConfigMapKeyRef{Name: "scan-policy"},
		},
	}
	policyConfig := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "scan-policy", Namespace: defaultNS, Labels: map[string]string{security.PolicyConfigMapAllowedLabel: "true"}}, Data: map[string]string{"policy": "new policy text"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan, policyConfig).Build()
	reconciler := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: store}
	run := &storepkg.ScanRun{ID: "scan_policy", Namespace: defaultNS, RepositoryScan: "kaset", Mode: "initial", Phase: scanRunPhaseRunning, PolicyDigest: "sha256:old"}
	if err := store.CreateScanRun(ctx, run); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	err := reconciler.createReviewTasks(ctx, scan, run, "", []storepkg.ReviewSlice{{ID: "slice_api", RepositoryScan: "kaset", Source: "deterministic", Title: "API", Kind: "package", Status: reviewSliceStatusPending}})
	if err == nil || !strings.Contains(err.Error(), "scanner policy digest changed") {
		t.Fatalf("createReviewTasks() error = %v, want policy drift error", err)
	}
	storedRun, err := store.GetScanRun(ctx, defaultNS, run.ID)
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if storedRun.Phase != scanRunPhaseFailed || storedRun.CompletedAt == nil || !strings.Contains(storedRun.ErrorMessage, "scanner policy digest changed") {
		t.Fatalf("stored run = %#v, want terminal failed policy-drift run", storedRun)
	}
	current := &corev1alpha1.RepositoryScan{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		t.Fatalf("Get(RepositoryScan) error = %v", err)
	}
	if current.Status.Phase != repositoryScanPhaseError {
		t.Fatalf("RepositoryScan phase = %q, want %q", current.Status.Phase, repositoryScanPhaseError)
	}
	ready := meta.FindStatusCondition(current.Status.Conditions, "Ready")
	if ready == nil || ready.Reason != readyReasonScanFailed || !strings.Contains(ready.Message, "scanner policy digest changed") {
		t.Fatalf("Ready condition = %#v, want ScanFailed policy-drift message", ready)
	}
}

func TestRepositoryScanPolicyDigestDriftFailsValidationTaskCreationWithoutRequeue(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1.AddToScheme() error = %v", err)
	}
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:                   "https://github.com/example/repo",
			AnalysisAgentRef:          corev1alpha1.AgentReference{Name: "scan-reviewer"},
			CustomScanInstructionsRef: &corev1alpha1.PolicyConfigMapKeyRef{Name: "scan-policy"},
		},
	}
	policyConfig := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "scan-policy", Namespace: defaultNS, Labels: map[string]string{security.PolicyConfigMapAllowedLabel: "true"}}, Data: map[string]string{"policy": "new policy text"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan, policyConfig).Build()
	reconciler := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: store}
	run := &storepkg.ScanRun{ID: "scan_policy", Namespace: defaultNS, RepositoryScan: "kaset", Mode: "initial", Phase: scanRunPhaseRunning, PolicyDigest: "sha256:old"}
	if err := store.CreateScanRun(ctx, run); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	finding := &storepkg.Finding{ID: "finding_policy", Namespace: defaultNS, RepositoryScan: "kaset", ScanRunID: run.ID, Severity: "high", Confidence: "high"}

	if err := reconciler.createValidationTask(ctx, scan, finding); err == nil || !strings.Contains(err.Error(), "scanner policy digest changed") {
		t.Fatalf("createValidationTask() error = %v, want policy drift propagated", err)
	}
	storedRun, err := store.GetScanRun(ctx, defaultNS, run.ID)
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if storedRun.Phase != scanRunPhaseFailed || !strings.Contains(storedRun.ErrorMessage, "scanner policy digest changed") {
		t.Fatalf("stored run = %#v, want terminal policy-drift failure", storedRun)
	}
	var tasks corev1alpha1.TaskList
	if err := cl.List(ctx, &tasks, client.InNamespace(defaultNS)); err != nil {
		t.Fatalf("List(Task) error = %v", err)
	}
	if len(tasks.Items) != 0 {
		t.Fatalf("validation tasks = %d, want none on policy drift", len(tasks.Items))
	}
}

func TestRepositoryScanUnreadablePolicyRefFailsMapperTaskCreation(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1.AddToScheme() error = %v", err)
	}
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:                   "https://github.com/example/repo",
			AnalysisAgentRef:          corev1alpha1.AgentReference{Name: "scan-reviewer"},
			CustomScanInstructionsRef: &corev1alpha1.PolicyConfigMapKeyRef{Name: "missing-policy"},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan).Build()
	reconciler := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: store}
	run := &storepkg.ScanRun{ID: "scan_policy", Namespace: defaultNS, RepositoryScan: "kaset", Mode: "initial", Phase: scanRunPhaseRunning}
	if err := store.CreateScanRun(ctx, run); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}

	err := reconciler.createMapperTask(ctx, scan, run)
	if err == nil || !strings.Contains(err.Error(), "customScanInstructionsRef") {
		t.Fatalf("createMapperTask() error = %v, want missing policy ref error", err)
	}
	storedRun, err := store.GetScanRun(ctx, defaultNS, run.ID)
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if storedRun.Phase != scanRunPhaseFailed || !strings.Contains(storedRun.ErrorMessage, "customScanInstructionsRef") {
		t.Fatalf("stored run = %#v, want terminal missing-policy failure", storedRun)
	}
	current := &corev1alpha1.RepositoryScan{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		t.Fatalf("Get(RepositoryScan) error = %v", err)
	}
	if current.Status.Phase != repositoryScanPhaseError {
		t.Fatalf("RepositoryScan phase = %q, want %q", current.Status.Phase, repositoryScanPhaseError)
	}
}

func TestTerminalScannerPolicyLoadErrorOnlyTerminalForDeterministicErrors(t *testing.T) {
	if !terminalScannerPolicyLoadError(fmt.Errorf("customScanInstructionsRef: key %q is missing in ConfigMap %q", "policy", "scan-policy")) {
		t.Fatal("terminalScannerPolicyLoadError() = false, want true for policy validation/config error")
	}
	if !terminalScannerPolicyLoadError(fmt.Errorf("customScanInstructionsRef: %w", apierrors.NewNotFound(schema.GroupResource{Group: "", Resource: "configmaps"}, "policy"))) {
		t.Fatal("terminalScannerPolicyLoadError() = false, want true for missing ConfigMap")
	}
	if terminalScannerPolicyLoadError(apierrors.NewInternalError(fmt.Errorf("apiserver temporarily unavailable"))) {
		t.Fatal("terminalScannerPolicyLoadError() = true, want false for transient API error")
	}
	if terminalScannerPolicyLoadError(fmt.Errorf("customScanInstructionsRef: %w", context.DeadlineExceeded)) {
		t.Fatal("terminalScannerPolicyLoadError() = true, want false for context deadline")
	}
}
