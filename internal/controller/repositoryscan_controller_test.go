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
