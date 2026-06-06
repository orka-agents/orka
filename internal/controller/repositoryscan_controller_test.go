/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/security"
	storepkg "github.com/sozercan/orka/internal/store"
	sqlitestore "github.com/sozercan/orka/internal/store/sqlite"
	"github.com/sozercan/orka/workers/common"
)

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
			Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
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

func saveTestFindingsArtifact(
	t *testing.T,
	ctx context.Context,
	store storepkg.ArtifactStore,
	namespace string,
	taskName string,
	headSHA string,
) {
	t.Helper()

	findings := security.FindingsArtifact{
		Version: 1,
		Repository: security.FindingsArtifactRepo{
			RepoURL: "https://github.com/example/repo",
			Branch:  "main",
			HeadSHA: headSHA,
			BaseSHA: "base-" + headSHA,
		},
		Scan: security.FindingsArtifactScan{
			Mode:    "initial",
			Summary: "No findings",
		},
		Findings: []security.FindingsArtifactFinding{},
	}
	data, err := json.Marshal(findings)
	if err != nil {
		t.Fatalf("json.Marshal(findings) error = %v", err)
	}
	if err := store.SaveArtifact(ctx, namespace, taskName, security.ArtifactFindings, "application/json", data); err != nil {
		t.Fatalf("SaveArtifact(%s) error = %v", taskName, err)
	}
}

func saveTestFindingsArtifactWithFinding(
	t *testing.T,
	ctx context.Context,
	store storepkg.ArtifactStore,
	namespace string,
	taskName string,
	headSHA string,
	fingerprint string,
) {
	t.Helper()

	findings := security.FindingsArtifact{
		Version: 1,
		Repository: security.FindingsArtifactRepo{
			RepoURL: "https://github.com/example/repo",
			Branch:  "main",
			HeadSHA: headSHA,
			BaseSHA: "base-" + headSHA,
		},
		Scan: security.FindingsArtifactScan{
			Mode:    "initial",
			Summary: "One finding",
		},
		Findings: []security.FindingsArtifactFinding{{
			Fingerprint:      fingerprint,
			Title:            "Late artifact finding",
			Summary:          "Finding persisted after terminal run re-ingest",
			Severity:         "high",
			Confidence:       "high",
			ValidationStatus: "unvalidated",
			FilePath:         "app/routes/index.js",
			Line:             42,
			RootCause:        "late artifact",
			Remediation:      "fix it",
			SuggestedAction:  "patch",
		}},
	}
	data, err := json.Marshal(findings)
	if err != nil {
		t.Fatalf("json.Marshal(findings) error = %v", err)
	}
	if err := store.SaveArtifact(ctx, namespace, taskName, security.ArtifactFindings, "application/json", data); err != nil {
		t.Fatalf("SaveArtifact(%s) error = %v", taskName, err)
	}
}

func newSucceededSecurityTask(name, scanName, scanID, stage, scope string, completed metav1.Time) *corev1alpha1.Task {
	labelsMap := map[string]string{
		labels.LabelSecurityTarget: scanName,
		labels.LabelSecurityScanID: scanID,
		labels.LabelSecurityStage:  stage,
	}
	if scope != "" {
		labelsMap[labels.LabelSecurityScope] = scope
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

func TestIngestScanTaskRefreshesNonLatestSplitRun(t *testing.T) {
	ctx := context.Background()
	secStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	scanName := "split-nonlatest"
	scanID := "scan_split_nonlatest"
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: scanName, Namespace: defaultNS},
		Spec:       corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "a"}},
	}
	completed := metav1.NewTime(mustParseTime(t, "2026-05-10T10:00:00Z"))
	threatTask := newSucceededSecurityTask("split-nonlatest-threat", scanName, scanID, security.StageThreatModel, "", completed)
	objects := []client.Object{scan, threatTask}
	discoveryTasks := []*corev1alpha1.Task{}
	for index, scope := range security.DiscoveryScopes() {
		task := newSucceededSecurityTask(
			fmt.Sprintf("split-nonlatest-discovery-%d", index),
			scanName,
			scanID,
			security.StageDiscovery,
			scope.Name,
			completed,
		)
		objects = append(objects, task)
		discoveryTasks = append(discoveryTasks, task)
		saveTestFindingsArtifact(t, ctx, secStore, task.Namespace, task.Name, fmt.Sprintf("head-%d", index))
	}
	if err := secStore.SaveArtifact(ctx, threatTask.Namespace, threatTask.Name, security.ArtifactThreatModel, "text/markdown", []byte("# Threat Model")); err != nil {
		t.Fatalf("SaveArtifact(threat model) error = %v", err)
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(objects...).
		Build()
	r := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: secStore, ArtifactStore: secStore}
	run := &storepkg.ScanRun{ID: scanID, Namespace: defaultNS, RepositoryScan: scanName, TaskName: threatTask.Name, Mode: "initial", Phase: scanRunPhasePending, StartedAt: completed.Time}
	if err := secStore.CreateScanRun(ctx, run); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}

	if err := r.ingestScanTask(ctx, scan, discoveryTasks[0], false); err != nil {
		t.Fatalf("ingestScanTask() error = %v", err)
	}

	updated, err := secStore.GetScanRun(ctx, scan.Namespace, scanID)
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if updated.Phase != scanRunPhaseSucceeded {
		t.Fatalf("run.Phase = %q, want %q", updated.Phase, scanRunPhaseSucceeded)
	}
	if updated.CompletedAt == nil || !updated.CompletedAt.Equal(completed.Time) {
		t.Fatalf("run.CompletedAt = %v, want %v", updated.CompletedAt, completed.Time)
	}
}

func TestIngestOwnedTasksReingestsTerminalRunWhenArtifactsArrive(t *testing.T) {
	ctx := context.Background()
	secStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	scanName := "split-terminal-late-artifacts"
	scanID := "scan_split_terminal_late_artifacts"
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: scanName, Namespace: defaultNS},
		Spec:       corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "a"}},
	}
	completed := metav1.NewTime(mustParseTime(t, "2026-05-10T11:00:00Z"))
	threatTask := newSucceededSecurityTask("split-terminal-late-threat", scanName, scanID, security.StageThreatModel, "", completed)
	objects := []client.Object{scan, threatTask}
	if err := secStore.SaveArtifact(ctx, threatTask.Namespace, threatTask.Name, security.ArtifactThreatModel, "text/markdown", []byte("# Threat Model")); err != nil {
		t.Fatalf("SaveArtifact(threat model) error = %v", err)
	}
	lateFingerprint := "late-artifact-finding"
	for index, scope := range security.DiscoveryScopes() {
		task := newSucceededSecurityTask(
			fmt.Sprintf("split-terminal-late-discovery-%d", index),
			scanName,
			scanID,
			security.StageDiscovery,
			scope.Name,
			completed,
		)
		objects = append(objects, task)
		if index == 0 {
			saveTestFindingsArtifactWithFinding(t, ctx, secStore, task.Namespace, task.Name, fmt.Sprintf("head-%d", index), lateFingerprint)
			continue
		}
		saveTestFindingsArtifact(t, ctx, secStore, task.Namespace, task.Name, fmt.Sprintf("head-%d", index))
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(objects...).
		Build()
	r := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: secStore, ArtifactStore: secStore}
	completedTime := completed.Time
	run := &storepkg.ScanRun{
		ID:             scanID,
		Namespace:      defaultNS,
		RepositoryScan: scanName,
		TaskName:       threatTask.Name,
		Mode:           "initial",
		Phase:          scanRunPhaseFailed,
		StartedAt:      completed.Add(-5 * time.Minute),
		CompletedAt:    &completedTime,
		ErrorMessage:   security.ArtifactFindings + " is missing",
		Summary:        security.ArtifactFindings + " is missing",
	}
	if err := secStore.CreateScanRun(ctx, run); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}

	if err := r.ingestOwnedTasks(ctx, scan); err != nil {
		t.Fatalf("ingestOwnedTasks() error = %v", err)
	}

	updated, err := secStore.GetScanRun(ctx, scan.Namespace, scanID)
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if updated.Phase != scanRunPhaseSucceeded {
		t.Fatalf("run.Phase = %q, want %q", updated.Phase, scanRunPhaseSucceeded)
	}
	if updated.ErrorMessage != "" {
		t.Fatalf("run.ErrorMessage = %q, want empty after successful re-ingest", updated.ErrorMessage)
	}
	if _, err := secStore.GetLatestThreatModel(ctx, scan.Namespace, scan.Name); err != nil {
		t.Fatalf("GetLatestThreatModel() error = %v", err)
	}
	if _, err := secStore.GetFinding(ctx, scan.Namespace, security.FindingID(lateFingerprint)); err != nil {
		t.Fatalf("GetFinding(late artifact) error = %v", err)
	}
}

func TestIngestOwnedTasksPreservesLatestCombinedScanSummary(t *testing.T) {
	ctx := context.Background()
	secStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	scanName := "combined-latest-summary"
	scanID := "scan_combined_latest_summary"
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: scanName, Namespace: defaultNS},
		Spec:       corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "a"}},
	}
	completed := metav1.NewTime(mustParseTime(t, "2026-05-10T12:00:00Z"))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "combined-latest-summary-task",
			Namespace: defaultNS,
			Labels: map[string]string{
				labels.LabelSecurityTarget: scanName,
				labels.LabelSecurityScanID: scanID,
				labels.LabelSecurityMode:   "initial",
			},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:          corev1alpha1.TaskPhaseSucceeded,
			CompletionTime: &completed,
		},
	}
	findings := security.FindingsArtifact{
		Version: 1,
		Repository: security.FindingsArtifactRepo{
			RepoURL: "https://github.com/example/repo",
			Branch:  "main",
			HeadSHA: "head-combined",
			BaseSHA: "base-combined",
		},
		Scan: security.FindingsArtifactScan{
			Mode:        "initial",
			CommitCount: 7,
			Summary:     "Rich combined scan summary from findings artifact",
		},
		Findings: []security.FindingsArtifactFinding{},
	}
	findingsJSON, err := json.Marshal(findings)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if err := secStore.SaveArtifact(ctx, task.Namespace, task.Name, security.ArtifactFindings, "application/json", findingsJSON); err != nil {
		t.Fatalf("SaveArtifact(findings) error = %v", err)
	}
	if err := secStore.SaveArtifact(ctx, task.Namespace, task.Name, security.ArtifactThreatModel, "text/markdown", []byte("# Threat Model")); err != nil {
		t.Fatalf("SaveArtifact(threat model) error = %v", err)
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(scan, task).
		Build()
	r := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: secStore, ArtifactStore: secStore}

	if err := r.ingestOwnedTasks(ctx, scan); err != nil {
		t.Fatalf("ingestOwnedTasks() error = %v", err)
	}

	run, err := secStore.GetScanRun(ctx, scan.Namespace, scanID)
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if run.Summary != findings.Scan.Summary {
		t.Fatalf("run.Summary = %q, want %q", run.Summary, findings.Scan.Summary)
	}
	current := &corev1alpha1.RepositoryScan{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		t.Fatalf("cl.Get() error = %v", err)
	}
	condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
	if condition == nil || condition.Message != findings.Scan.Summary {
		t.Fatalf("Ready condition = %#v, want message %q", condition, findings.Scan.Summary)
	}
}

func TestIngestScanTaskFailsSucceededTaskWithoutRequiredArtifacts(t *testing.T) {
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
	completedAt := metav1.NewTime(mustParseTime(t, "2026-04-10T05:18:35Z"))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "kaset-manual-1",
			Namespace:         defaultNS,
			CreationTimestamp: metav1.NewTime(mustParseTime(t, "2026-04-10T05:15:08Z")),
			Labels: map[string]string{
				labels.LabelSecurityTarget: "kaset",
				labels.LabelSecurityScanID: "scan_missing_artifacts",
				labels.LabelSecurityMode:   "manual",
			},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:          corev1alpha1.TaskPhaseSucceeded,
			CompletionTime: &completedAt,
		},
	}

	if err := store.SaveResult(ctx, task.Namespace, task.Name, []byte("scan said success")); err != nil {
		t.Fatalf("SaveResult() error = %v", err)
	}

	if err := reconciler.ingestScanTask(ctx, scan, task, false); err != nil {
		t.Fatalf("ingestScanTask() error = %v", err)
	}

	run, err := store.GetScanRun(ctx, scan.Namespace, "scan_missing_artifacts")
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if run.Phase != scanRunPhaseFailed {
		t.Fatalf("run.Phase = %q, want failed", run.Phase)
	}
	if !strings.Contains(run.ErrorMessage, security.ArtifactThreatModel) {
		t.Fatalf("run.ErrorMessage = %q, want it to mention %s", run.ErrorMessage, security.ArtifactThreatModel)
	}
	if !strings.Contains(run.ErrorMessage, security.ArtifactFindings) {
		t.Fatalf("run.ErrorMessage = %q, want it to mention %s", run.ErrorMessage, security.ArtifactFindings)
	}

	_, err = store.GetLatestThreatModel(ctx, scan.Namespace, scan.Name)
	if !errors.Is(err, storepkg.ErrNotFound) {
		t.Fatalf("GetLatestThreatModel() error = %v, want ErrNotFound", err)
	}
}

func TestIngestScanTaskPersistsThreatModelWhenRequiredArtifactsExist(t *testing.T) {
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
	completedAt := metav1.NewTime(mustParseTime(t, "2026-04-10T05:20:35Z"))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "kaset-manual-2",
			Namespace:         defaultNS,
			CreationTimestamp: metav1.NewTime(mustParseTime(t, "2026-04-10T05:19:08Z")),
			Labels: map[string]string{
				labels.LabelSecurityTarget: "kaset",
				labels.LabelSecurityScanID: "scan_with_artifacts",
				labels.LabelSecurityMode:   "manual",
			},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:          corev1alpha1.TaskPhaseSucceeded,
			CompletionTime: &completedAt,
		},
	}

	findings := security.FindingsArtifact{
		Version: 1,
		Repository: security.FindingsArtifactRepo{
			RepoURL: "https://github.com/sozercan/kaset",
			Branch:  "main",
			HeadSHA: "abc123",
			BaseSHA: "def456",
		},
		Scan: security.FindingsArtifactScan{
			Mode:        "manual",
			CommitCount: 3,
			Summary:     "Validated 0 findings",
		},
		Findings: []security.FindingsArtifactFinding{},
	}
	findingsJSON, err := json.Marshal(findings)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if err := store.SaveArtifact(ctx, task.Namespace, task.Name, security.ArtifactFindings, "application/json", findingsJSON); err != nil {
		t.Fatalf("SaveArtifact(findings) error = %v", err)
	}
	if err := store.SaveArtifact(ctx, task.Namespace, task.Name, security.ArtifactThreatModel, "text/markdown", []byte("# Threat Model\n\n- protect secrets")); err != nil {
		t.Fatalf("SaveArtifact(threat model) error = %v", err)
	}

	if err := reconciler.ingestScanTask(ctx, scan, task, false); err != nil {
		t.Fatalf("ingestScanTask() error = %v", err)
	}

	run, err := store.GetScanRun(ctx, scan.Namespace, "scan_with_artifacts")
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if run.Phase != scanRunPhaseSucceeded {
		t.Fatalf("run.Phase = %q, want succeeded", run.Phase)
	}
	if run.HeadCommit != "abc123" {
		t.Fatalf("run.HeadCommit = %q, want abc123", run.HeadCommit)
	}
	if run.Summary != "Validated 0 findings" {
		t.Fatalf("run.Summary = %q, want findings summary", run.Summary)
	}

	model, err := store.GetLatestThreatModel(ctx, scan.Namespace, scan.Name)
	if err != nil {
		t.Fatalf("GetLatestThreatModel() error = %v", err)
	}
	if !strings.Contains(model.Content, "protect secrets") {
		t.Fatalf("model.Content = %q, want saved threat model", model.Content)
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

	if err := reconciler.ingestScanTask(ctx, scan, task, false); err != nil {
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
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
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
		StartedAt:      time.Now(),
	}); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}

	if err := reconciler.ingestScanTask(ctx, scan, task, false); err != nil {
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
}

func TestMapperReingestPreservesReviewedSliceForCurrentRun(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	reconciler := &RepositoryScanReconciler{
		SecurityStore: store,
		ArtifactStore: store,
	}
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
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

	if err := reconciler.ingestScanTask(ctx, scan, mapperTask, false); err != nil {
		t.Fatalf("ingest mapper error = %v", err)
	}
	if err := reconciler.ingestScanTask(ctx, scan, reviewTask, false); err != nil {
		t.Fatalf("ingest review error = %v", err)
	}
	if err := reconciler.ingestScanTask(ctx, scan, mapperTask, false); err != nil {
		t.Fatalf("reingest mapper error = %v", err)
	}
	if err := reconciler.ingestScanTask(ctx, scan, reviewTask, false); err != nil {
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
	threatTask := newSucceededSecurityTask("kaset-initial-threat", "kaset", "scan_review", security.StageThreatModel, "", metav1.Now())
	mapperTask := newSucceededSecurityTask("kaset-initial-mapper", "kaset", "scan_review", security.StageMapper, "", metav1.Now())
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

	var discoveryTasks corev1alpha1.TaskList
	if err := cl.List(ctx, &discoveryTasks,
		client.InNamespace(defaultNS),
		client.MatchingLabels(map[string]string{
			labels.LabelSecurityTarget: "kaset",
			labels.LabelSecurityScanID: "scan_review",
			labels.LabelSecurityStage:  security.StageDiscovery,
		}),
	); err != nil {
		t.Fatalf("List(discovery tasks) error = %v", err)
	}
	if len(discoveryTasks.Items) != 0 {
		t.Fatalf("len(discovery tasks) = %d, want 0 while review slices exist", len(discoveryTasks.Items))
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
	threatTask := newSucceededSecurityTask("kaset-partial-threat", "kaset", "scan_partial_review", security.StageThreatModel, "", metav1.Now())
	mapperTask := newSucceededSecurityTask("kaset-partial-mapper", "kaset", "scan_partial_review", security.StageMapper, "", metav1.Now())
	reviewTask := newSucceededSecurityTask("kaset-review-slice-api", "kaset", "scan_partial_review", security.StageReview, "", metav1.Now())
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
	threatTask := newSucceededSecurityTask("kaset-incremental-threat", "kaset", "scan_noop_incremental", security.StageThreatModel, "", metav1.Now())
	mapperTask := newSucceededSecurityTask("kaset-incremental-mapper", "kaset", "scan_noop_incremental", security.StageMapper, "", metav1.Now())
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

	var discoveryTasks corev1alpha1.TaskList
	if err := cl.List(ctx, &discoveryTasks,
		client.InNamespace(defaultNS),
		client.MatchingLabels(map[string]string{
			labels.LabelSecurityTarget: "kaset",
			labels.LabelSecurityScanID: "scan_noop_incremental",
			labels.LabelSecurityStage:  security.StageDiscovery,
		}),
	); err != nil {
		t.Fatalf("List(discovery tasks) error = %v", err)
	}
	if len(discoveryTasks.Items) != 0 {
		t.Fatalf("len(discovery tasks) = %d, want 0 for no-op incremental", len(discoveryTasks.Items))
	}
	run, err := store.GetScanRun(ctx, defaultNS, "scan_noop_incremental")
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if run.Phase != scanRunPhaseSucceeded || run.CompletedAt == nil {
		t.Fatalf("run phase/completedAt = %q/%v, want succeeded with completion", run.Phase, run.CompletedAt)
	}
	current := &corev1alpha1.RepositoryScan{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		t.Fatalf("Get(scan) error = %v", err)
	}
	if current.Status.Phase != repositoryScanPhaseReady || current.Status.LastProcessedCommit != "head456" {
		t.Fatalf("scan status phase/processed = %q/%q, want Ready/head456", current.Status.Phase, current.Status.LastProcessedCommit)
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
	threatTask := newSucceededSecurityTask("kaset-incomplete-threat", "kaset", "scan_review_incomplete", security.StageThreatModel, "", completed)
	mapperTask := newSucceededSecurityTask("kaset-incomplete-mapper", "kaset", "scan_review_incomplete", security.StageMapper, "", completed)
	reviewTask := newSucceededSecurityTask("kaset-incomplete-review-api", "kaset", "scan_review_incomplete", security.StageReview, "", completed)
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

func TestIngestDiscoveryTaskPartitionsV2Findings(t *testing.T) {
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
			Name:      "kaset-review-slice",
			Namespace: defaultNS,
			Labels: map[string]string{
				labels.LabelSecurityTarget:  "kaset",
				labels.LabelSecurityScanID:  "scan_v2",
				labels.LabelSecurityMode:    "initial",
				labels.LabelSecurityStage:   security.StageDiscovery,
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
			HeadSHA: "head123",
		},
		Scan: security.FindingsV2Scan{Mode: "initial", SliceID: "slice_api", Summary: "one accepted, one dropped"},
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

	if err := reconciler.ingestScanTask(ctx, scan, task, false); err != nil {
		t.Fatalf("ingestScanTask() error = %v", err)
	}
	listed, _, err := store.ListFindings(ctx, storepkg.FindingFilter{Namespace: defaultNS, RepositoryScan: "kaset", Category: "authz"})
	if err != nil {
		t.Fatalf("ListFindings() error = %v", err)
	}
	if len(listed) != 1 || listed[0].SliceID != "slice_api" || listed[0].Category != "authz" {
		t.Fatalf("findings = %#v, want one accepted v2 finding", listed)
	}
	dropped, _, err := store.ListDroppedFindings(ctx, storepkg.DroppedFindingFilter{Namespace: defaultNS, RepositoryScan: "kaset", ScanRunID: "scan_v2"})
	if err != nil {
		t.Fatalf("ListDroppedFindings() error = %v", err)
	}
	if len(dropped) != 1 || dropped[0].Reason == "" {
		t.Fatalf("dropped = %#v, want one diagnostic", dropped)
	}
	if _, _, err := store.GetArtifact(ctx, task.Namespace, task.Name, security.ArtifactDroppedFindings); err != nil {
		t.Fatalf("GetArtifact(dropped findings) error = %v", err)
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

	if err := reconciler.ingestScanTask(ctx, scan, task, false); err != nil {
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
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
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
		Scan: security.FindingsV2Scan{Mode: "initial", SliceID: "slice_api", Summary: "one accepted, one dropped"},
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

	if err := reconciler.ingestScanTask(ctx, scan, task, false); err != nil {
		t.Fatalf("ingestScanTask() error = %v", err)
	}
	if err := reconciler.ingestScanTask(ctx, scan, task, false); err != nil {
		t.Fatalf("second ingestScanTask() error = %v", err)
	}
	run, err := store.GetScanRun(ctx, defaultNS, "scan_review_ingest")
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if run.ReviewedSliceCount != 1 || run.AcceptedFindings != 1 || run.DroppedFindings != 1 {
		t.Fatalf("run counts = reviewed:%d accepted:%d dropped:%d, want 1/1/1", run.ReviewedSliceCount, run.AcceptedFindings, run.DroppedFindings)
	}
	if run.BaseCommit != "trusted-base" || run.HeadCommit != "trusted-head" {
		t.Fatalf("run commits = %q/%q, want trusted-base/trusted-head", run.BaseCommit, run.HeadCommit)
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
	if len(dropped) != 1 {
		t.Fatalf("len(dropped) = %d, want one diagnostic", len(dropped))
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

	if err := reconciler.ingestScanTask(ctx, scan, task, false); err != nil {
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

func TestIngestScanTaskPersistsThreatModelWhenFindingsAreInvalid(t *testing.T) {
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
	completedAt := metav1.NewTime(mustParseTime(t, "2026-04-10T18:24:55Z"))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "kaset-manual-invalid-findings",
			Namespace:         defaultNS,
			CreationTimestamp: metav1.NewTime(mustParseTime(t, "2026-04-10T18:20:31Z")),
			Labels: map[string]string{
				labels.LabelSecurityTarget: "kaset",
				labels.LabelSecurityScanID: "scan_invalid_findings",
				labels.LabelSecurityMode:   "manual",
			},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:          corev1alpha1.TaskPhaseSucceeded,
			CompletionTime: &completedAt,
		},
	}

	invalidFindings := []byte("{\"version\":1,\"scan\":{\"summary\":\"broken\nline\"},\"findings\":[]}")
	if err := store.SaveArtifact(ctx, task.Namespace, task.Name, security.ArtifactFindings, "application/json", invalidFindings); err != nil {
		t.Fatalf("SaveArtifact(findings) error = %v", err)
	}
	if err := store.SaveArtifact(ctx, task.Namespace, task.Name, security.ArtifactThreatModel, "text/markdown", []byte("# Threat Model\n\n- richer model")); err != nil {
		t.Fatalf("SaveArtifact(threat model) error = %v", err)
	}

	if err := reconciler.ingestScanTask(ctx, scan, task, false); err != nil {
		t.Fatalf("ingestScanTask() error = %v", err)
	}

	run, err := store.GetScanRun(ctx, scan.Namespace, "scan_invalid_findings")
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if run.Phase != scanRunPhaseFailed {
		t.Fatalf("run.Phase = %q, want failed", run.Phase)
	}
	if !strings.Contains(run.ErrorMessage, "invalid JSON") {
		t.Fatalf("run.ErrorMessage = %q, want invalid JSON", run.ErrorMessage)
	}

	model, err := store.GetLatestThreatModel(ctx, scan.Namespace, scan.Name)
	if err != nil {
		t.Fatalf("GetLatestThreatModel() error = %v", err)
	}
	if !strings.Contains(model.Content, "richer model") {
		t.Fatalf("model.Content = %q, want persisted threat model", model.Content)
	}
	if model.GeneratedByScan != "scan_invalid_findings" {
		t.Fatalf("model.GeneratedByScan = %q, want scan_invalid_findings", model.GeneratedByScan)
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
		Evidence: security.FindingsArtifactEvidenceRefs{
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

	var tasks corev1alpha1.TaskList
	if err := cl.List(ctx, &tasks,
		client.InNamespace(defaultNS),
		client.MatchingLabels(map[string]string{
			labels.LabelSecurityTarget: "kaset",
			labels.LabelSecurityScanID: "scan_new",
			labels.LabelSecurityStage:  security.StageDiscovery,
		}),
	); err != nil {
		t.Fatalf("List(discovery tasks) error = %v", err)
	}
	if len(tasks.Items) != len(security.DiscoveryScopes()) {
		t.Fatalf("len(discovery tasks) = %d, want %d", len(tasks.Items), len(security.DiscoveryScopes()))
	}

	run, err := store.GetScanRun(ctx, defaultNS, "scan_new")
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if run.Phase != scanRunPhaseRunning {
		t.Fatalf("run.Phase = %q, want running", run.Phase)
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

func TestIngestPatchTaskAcceptsLegacyConfirmedPushWithoutArtifactContract(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "legacy-no-artifacts")
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
	assertPatchIngestState(t, fixture, scanRunPhaseSucceeded, findingStatePatchReady)
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

func TestApplyCombinedScanPhaseStatusUsesRunCompletion(t *testing.T) {
	completed := mustParseTime(t, "2026-05-07T23:30:00Z")

	succeeded := &corev1alpha1.RepositoryScan{}
	applyCombinedScanPhaseStatus(succeeded, corev1alpha1.TaskPhaseSucceeded, &storepkg.ScanRun{
		Phase:       scanRunPhaseSucceeded,
		CompletedAt: &completed,
		HeadCommit:  "abc123",
	})
	if succeeded.Status.LastScanAt == nil || !succeeded.Status.LastScanAt.Time.Equal(completed) {
		t.Fatalf("succeeded LastScanAt = %v, want %v", succeeded.Status.LastScanAt, completed)
	}
	if succeeded.Status.LastSuccessfulScanAt == nil || !succeeded.Status.LastSuccessfulScanAt.Time.Equal(completed) {
		t.Fatalf("succeeded LastSuccessfulScanAt = %v, want %v", succeeded.Status.LastSuccessfulScanAt, completed)
	}

	failed := &corev1alpha1.RepositoryScan{}
	applyCombinedScanPhaseStatus(failed, corev1alpha1.TaskPhaseFailed, &storepkg.ScanRun{
		Phase:       scanRunPhaseFailed,
		CompletedAt: &completed,
	})
	if failed.Status.LastScanAt == nil || !failed.Status.LastScanAt.Time.Equal(completed) {
		t.Fatalf("failed LastScanAt = %v, want %v", failed.Status.LastScanAt, completed)
	}
	if failed.Status.LastSuccessfulScanAt != nil {
		t.Fatalf("failed LastSuccessfulScanAt = %v, want nil", failed.Status.LastSuccessfulScanAt)
	}
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

func TestRefreshScanRunStatusWaitsForAllDiscoveryScopes(t *testing.T) {
	ctx := context.Background()
	secStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: "split-pending", Namespace: defaultNS},
		Spec:       corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "a"}},
	}
	completed := metav1.NewTime(mustParseTime(t, "2026-05-09T10:00:00Z"))
	threatTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "split-pending-threat",
			Namespace: defaultNS,
			Labels: map[string]string{
				labels.LabelSecurityTarget: "split-pending",
				labels.LabelSecurityScanID: "scan_split_pending",
				labels.LabelSecurityStage:  security.StageThreatModel,
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded, CompletionTime: &completed},
	}
	discoveryTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "split-pending-discovery-0",
			Namespace: defaultNS,
			Labels: map[string]string{
				labels.LabelSecurityTarget: "split-pending",
				labels.LabelSecurityScanID: "scan_split_pending",
				labels.LabelSecurityStage:  security.StageDiscovery,
				labels.LabelSecurityScope:  security.DiscoveryScopes()[0].Name,
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded, CompletionTime: &completed},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(scan, threatTask, discoveryTask).
		Build()
	r := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: secStore, ArtifactStore: secStore}

	if err := secStore.SaveArtifact(ctx, threatTask.Namespace, threatTask.Name, security.ArtifactThreatModel, "text/markdown", []byte("# Threat Model")); err != nil {
		t.Fatalf("SaveArtifact(threat model) error = %v", err)
	}
	saveTestFindingsArtifact(t, ctx, secStore, discoveryTask.Namespace, discoveryTask.Name, "head-0")

	run := &storepkg.ScanRun{ID: "scan_split_pending", Namespace: defaultNS, RepositoryScan: "split-pending", TaskName: threatTask.Name, Mode: "initial", Phase: scanRunPhaseRunning, StartedAt: completed.Time}
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
	if current.Status.Phase != repositoryScanPhaseScanning {
		t.Fatalf("scan.Status.Phase = %q, want %q", current.Status.Phase, repositoryScanPhaseScanning)
	}
	if current.Status.LastSuccessfulScanAt != nil {
		t.Fatalf("LastSuccessfulScanAt = %v, want nil while discovery scopes are pending", current.Status.LastSuccessfulScanAt)
	}
}

func TestIngestDiscoveryTaskIgnoresUnsupportedV2ArtifactWhenLegacyFindingsExist(t *testing.T) {
	ctx := context.Background()
	secStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: "legacy-with-extra-v2", Namespace: defaultNS},
		Spec:       corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "a"}},
	}
	completed := metav1.NewTime(mustParseTime(t, "2026-05-09T10:30:00Z"))
	discoveryTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "legacy-with-extra-v2-discovery",
			Namespace: defaultNS,
			Labels: map[string]string{
				labels.LabelSecurityTarget: "legacy-with-extra-v2",
				labels.LabelSecurityScanID: "scan_legacy_extra_v2",
				labels.LabelSecurityStage:  security.StageDiscovery,
				labels.LabelSecurityScope:  security.DiscoveryScopes()[0].Name,
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded, CompletionTime: &completed},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(scan, discoveryTask).
		Build()
	r := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: secStore, ArtifactStore: secStore}
	run := &storepkg.ScanRun{
		ID:             "scan_legacy_extra_v2",
		Namespace:      defaultNS,
		RepositoryScan: "legacy-with-extra-v2",
		TaskName:       discoveryTask.Name,
		Mode:           "initial",
		Phase:          scanRunPhaseRunning,
		StartedAt:      completed.Time,
	}
	if err := secStore.CreateScanRun(ctx, run); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	saveTestFindingsArtifactWithFinding(t, ctx, secStore, discoveryTask.Namespace, discoveryTask.Name, "head-legacy", "legacy-finding")
	extraV2 := security.FindingsV2Artifact{
		SchemaVersion: security.SchemaVersionFindingsV2,
		Repository: security.FindingsV2Repository{
			RepoURL: "https://github.com/example/repo",
			Branch:  "main",
			HeadSHA: "head-legacy",
		},
		Scan:     security.FindingsV2Scan{Mode: "initial", SliceID: "slice_extra"},
		Findings: []security.FindingsV2Finding{},
	}
	extraV2Data, err := json.Marshal(extraV2)
	if err != nil {
		t.Fatalf("json.Marshal(extraV2) error = %v", err)
	}
	if err := secStore.SaveArtifact(ctx, discoveryTask.Namespace, discoveryTask.Name, security.ArtifactFindingsV2, "application/json", extraV2Data); err != nil {
		t.Fatalf("SaveArtifact(v2) error = %v", err)
	}

	if err := r.ingestDiscoveryTask(ctx, scan, discoveryTask, run, false); err != nil {
		t.Fatalf("ingestDiscoveryTask() error = %v", err)
	}
	findings, _, err := secStore.ListFindings(ctx, storepkg.FindingFilter{Namespace: defaultNS, RepositoryScan: "legacy-with-extra-v2"})
	if err != nil {
		t.Fatalf("ListFindings() error = %v", err)
	}
	if len(findings) != 1 || findings[0].Fingerprint != "legacy-finding" {
		t.Fatalf("findings = %#v, want legacy v1 finding ingested", findings)
	}
	updatedRun, err := secStore.GetScanRun(ctx, defaultNS, "scan_legacy_extra_v2")
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if strings.Contains(updatedRun.ErrorMessage, security.ArtifactFindingsV2) {
		t.Fatalf("run.ErrorMessage = %q, want unsupported v2 artifact ignored for broad discovery", updatedRun.ErrorMessage)
	}
}

func TestIngestOwnedTasksFailsSucceededDiscoveryWithoutFindingsArtifact(t *testing.T) {
	ctx := context.Background()
	secStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: "split-missing", Namespace: defaultNS},
		Spec:       corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "a"}},
	}
	completed := metav1.NewTime(mustParseTime(t, "2026-05-09T11:00:00Z"))
	threatTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "split-missing-threat",
			Namespace: defaultNS,
			Labels: map[string]string{
				labels.LabelSecurityTarget: "split-missing",
				labels.LabelSecurityScanID: "scan_split_missing",
				labels.LabelSecurityStage:  security.StageThreatModel,
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded, CompletionTime: &completed},
	}
	objects := []client.Object{scan, threatTask}
	missingScope := security.DiscoveryScopes()[len(security.DiscoveryScopes())-1].Name
	for index, scope := range security.DiscoveryScopes() {
		task := &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("split-missing-discovery-%d", index),
				Namespace: defaultNS,
				Labels: map[string]string{
					labels.LabelSecurityTarget: "split-missing",
					labels.LabelSecurityScanID: "scan_split_missing",
					labels.LabelSecurityStage:  security.StageDiscovery,
					labels.LabelSecurityScope:  scope.Name,
				},
			},
			Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded, CompletionTime: &completed},
		}
		objects = append(objects, task)
		if scope.Name != missingScope {
			saveTestFindingsArtifact(t, ctx, secStore, task.Namespace, task.Name, fmt.Sprintf("head-%d", index))
		}
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(objects...).
		Build()
	r := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: secStore, ArtifactStore: secStore}

	if err := secStore.SaveArtifact(ctx, threatTask.Namespace, threatTask.Name, security.ArtifactThreatModel, "text/markdown", []byte("# Threat Model")); err != nil {
		t.Fatalf("SaveArtifact(threat model) error = %v", err)
	}

	if err := r.ingestOwnedTasks(ctx, scan); err != nil {
		t.Fatalf("ingestOwnedTasks() error = %v", err)
	}

	current := &corev1alpha1.RepositoryScan{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		t.Fatalf("cl.Get() error = %v", err)
	}
	if current.Status.Phase != repositoryScanPhaseError {
		t.Fatalf("scan.Status.Phase = %q, want %q", current.Status.Phase, repositoryScanPhaseError)
	}
	condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
	if condition == nil {
		t.Fatal("Ready condition = nil, want failed condition")
	}
	if !strings.Contains(condition.Message, missingScope) || !strings.Contains(condition.Message, security.ArtifactFindings) {
		t.Fatalf("condition.Message = %q, want missing scope and artifact", condition.Message)
	}
	if current.Status.LastSuccessfulScanAt != nil {
		t.Fatalf("LastSuccessfulScanAt = %v, want nil for failed artifact validation", current.Status.LastSuccessfulScanAt)
	}
}

func TestIngestCombinedScanTaskSetsLastScanAtOnFailure(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: "combined-fail", Namespace: defaultNS},
		Spec:       corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "a"}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan).Build()
	r := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: store, ArtifactStore: store, ResultStore: store}

	completedAt := metav1.NewTime(mustParseTime(t, "2026-05-08T01:00:00Z"))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "combined-fail-scan-1",
			Namespace:         defaultNS,
			CreationTimestamp: metav1.NewTime(mustParseTime(t, "2026-05-08T00:55:00Z")),
			Labels: map[string]string{
				labels.LabelSecurityTarget: "combined-fail",
				labels.LabelSecurityScanID: "scan_combined_fail",
				labels.LabelSecurityMode:   "initial",
			},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:          corev1alpha1.TaskPhaseFailed,
			CompletionTime: &completedAt,
			Message:        "task failed",
		},
	}

	if err := r.ingestScanTask(ctx, scan, task, true); err != nil {
		t.Fatalf("ingestScanTask() error = %v", err)
	}

	current := &corev1alpha1.RepositoryScan{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		t.Fatalf("cl.Get() error = %v", err)
	}
	if current.Status.LastScanAt == nil || !current.Status.LastScanAt.Time.Equal(completedAt.Time) {
		t.Fatalf("LastScanAt = %v, want %v", current.Status.LastScanAt, completedAt.Time)
	}
	if current.Status.LastSuccessfulScanAt != nil {
		t.Fatalf("LastSuccessfulScanAt = %v, want nil for failed scan", current.Status.LastSuccessfulScanAt)
	}
}

func TestIngestCombinedScanTaskSetsBothTimestampsOnSuccess(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: "combined-ok", Namespace: defaultNS},
		Spec:       corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "a"}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan).Build()
	r := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: store, ArtifactStore: store, ResultStore: store}

	completedAt := metav1.NewTime(mustParseTime(t, "2026-05-08T02:00:00Z"))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "combined-ok-scan-1",
			Namespace:         defaultNS,
			CreationTimestamp: metav1.NewTime(mustParseTime(t, "2026-05-08T01:55:00Z")),
			Labels: map[string]string{
				labels.LabelSecurityTarget: "combined-ok",
				labels.LabelSecurityScanID: "scan_combined_ok",
				labels.LabelSecurityMode:   "initial",
			},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:          corev1alpha1.TaskPhaseSucceeded,
			CompletionTime: &completedAt,
		},
	}

	findings := security.FindingsArtifact{
		Version: 1,
		Repository: security.FindingsArtifactRepo{
			RepoURL: "https://github.com/example/repo",
			Branch:  "main",
			HeadSHA: "abc123",
			BaseSHA: "def456",
		},
		Scan: security.FindingsArtifactScan{
			Mode:    "initial",
			Summary: "No findings",
		},
		Findings: []security.FindingsArtifactFinding{},
	}
	findingsJSON, err := json.Marshal(findings)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if err := store.SaveArtifact(ctx, task.Namespace, task.Name, security.ArtifactFindings, "application/json", findingsJSON); err != nil {
		t.Fatalf("SaveArtifact(findings) error = %v", err)
	}
	if err := store.SaveArtifact(ctx, task.Namespace, task.Name, security.ArtifactThreatModel, "text/plain", []byte("threat model")); err != nil {
		t.Fatalf("SaveArtifact(threat-model) error = %v", err)
	}

	if err := r.ingestScanTask(ctx, scan, task, true); err != nil {
		t.Fatalf("ingestScanTask() error = %v", err)
	}

	current := &corev1alpha1.RepositoryScan{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		t.Fatalf("cl.Get() error = %v", err)
	}
	if current.Status.LastScanAt == nil || !current.Status.LastScanAt.Time.Equal(completedAt.Time) {
		t.Fatalf("LastScanAt = %v, want %v", current.Status.LastScanAt, completedAt.Time)
	}
	if current.Status.LastSuccessfulScanAt == nil || !current.Status.LastSuccessfulScanAt.Time.Equal(completedAt.Time) {
		t.Fatalf("LastSuccessfulScanAt = %v, want %v", current.Status.LastSuccessfulScanAt, completedAt.Time)
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
