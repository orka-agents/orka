/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

func TestIngestScanTaskFailsSucceededTaskWithoutRequiredArtifacts(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)

	reconciler := &RepositoryScanReconciler{
		SecurityStore: store,
		ArtifactStore: store,
		ResultStore:   store,
	}
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: "default"},
	}
	completedAt := metav1.NewTime(mustParseTime(t, "2026-04-10T05:18:35Z"))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "kaset-manual-1",
			Namespace:         "default",
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
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: "default"},
	}
	completedAt := metav1.NewTime(mustParseTime(t, "2026-04-10T05:20:35Z"))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "kaset-manual-2",
			Namespace:         "default",
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

func TestIngestScanTaskPersistsThreatModelWhenFindingsAreInvalid(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)

	reconciler := &RepositoryScanReconciler{
		SecurityStore: store,
		ArtifactStore: store,
		ResultStore:   store,
	}
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: "default"},
	}
	completedAt := metav1.NewTime(mustParseTime(t, "2026-04-10T18:24:55Z"))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "kaset-manual-invalid-findings",
			Namespace:         "default",
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
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: "default"},
	}

	if err := store.SaveThreatModel(ctx, &storepkg.ThreatModel{
		Namespace:      "default",
		RepositoryScan: "kaset",
		Content:        "# Clean Threat Model\n\nCurated content",
		Source:         "cleaned",
	}); err != nil {
		t.Fatalf("SaveThreatModel() error = %v", err)
	}

	latest, err := store.GetLatestThreatModel(ctx, "default", "kaset")
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

	latest, err = store.GetLatestThreatModel(ctx, "default", "kaset")
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
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: "default"},
	}

	if err := store.SaveThreatModel(ctx, &storepkg.ThreatModel{
		Namespace:      "default",
		RepositoryScan: "kaset",
		Content:        "# Clean Threat Model\n\nCurated content",
		Source:         "cleaned",
	}); err != nil {
		t.Fatalf("SaveThreatModel() error = %v", err)
	}

	latest, err := store.GetLatestThreatModel(ctx, "default", "kaset")
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

	latest, err = store.GetLatestThreatModel(ctx, "default", "kaset")
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
			Namespace: "default",
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
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: "default"},
	}
	finding := &storepkg.Finding{
		ID:               "fnd_validate",
		Namespace:        "default",
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
			Namespace: "default",
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

	updated, err := store.GetFinding(ctx, "default", finding.ID)
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
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: "default"},
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
			Namespace:         "default",
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
			Namespace:         "default",
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

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(scan, oldTask, newTask).
		Build()

	reconciler := &RepositoryScanReconciler{
		Client:        cl,
		Scheme:        scheme,
		SecurityStore: store,
	}

	if err := store.CreateScanRun(ctx, &storepkg.ScanRun{
		ID:             "scan_new",
		Namespace:      "default",
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
		client.InNamespace("default"),
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

	run, err := store.GetScanRun(ctx, "default", "scan_new")
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
			Namespace: "default",
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
			ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: "default"},
		},
		finding: &storepkg.Finding{
			ID:               findingID,
			Namespace:        "default",
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
			Namespace:      "default",
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
	savePatchStructuredResult(t, fixture, &common.StructuredResult{
		Summary:    "patched successfully",
		Diff:       "diff --git a/app.py b/app.py",
		PushBranch: fixture.proposal.Branch,
	})

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, patchTaskForFixture(fixture, true)); err != nil {
		t.Fatalf("ingestPatchTask() error = %v", err)
	}
	assertPatchIngestState(t, fixture, scanRunPhaseSucceeded, findingStatePatchReady)
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
		ObjectMeta: metav1.ObjectMeta{Name: "ts-fail", Namespace: "default"},
		Spec:       corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "a"}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan).Build()
	r := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: secStore}

	completed := mustParseTime(t, "2026-05-07T22:41:22Z")
	run := &storepkg.ScanRun{ID: "scan_f", Namespace: "default", RepositoryScan: "ts-fail", TaskName: "t", Mode: "initial", Phase: scanRunPhaseFailed, StartedAt: completed, CompletedAt: &completed, ErrorMessage: "failed", HeadCommit: "abc"}
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
		ObjectMeta: metav1.ObjectMeta{Name: "ts-ok", Namespace: "default"},
		Spec:       corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "a"}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan).Build()
	r := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: secStore}

	completed := mustParseTime(t, "2026-05-07T23:00:00Z")
	run := &storepkg.ScanRun{ID: "scan_s", Namespace: "default", RepositoryScan: "ts-ok", TaskName: "t", Mode: "initial", Phase: scanRunPhaseSucceeded, StartedAt: completed, CompletedAt: &completed, HeadCommit: "def"}
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

func TestIngestCombinedScanTaskSetsLastScanAtOnFailure(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: "combined-fail", Namespace: "default"},
		Spec:       corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "a"}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan).Build()
	r := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: store, ArtifactStore: store, ResultStore: store}

	completedAt := metav1.NewTime(mustParseTime(t, "2026-05-08T01:00:00Z"))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "combined-fail-scan-1",
			Namespace:         "default",
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
		ObjectMeta: metav1.ObjectMeta{Name: "combined-ok", Namespace: "default"},
		Spec:       corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "a"}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan).Build()
	r := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: store, ArtifactStore: store, ResultStore: store}

	completedAt := metav1.NewTime(mustParseTime(t, "2026-05-08T02:00:00Z"))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "combined-ok-scan-1",
			Namespace:         "default",
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
