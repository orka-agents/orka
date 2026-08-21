/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"crypto/sha256"
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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/security"
	storepkg "github.com/orka-agents/orka/internal/store"
	sqlitestore "github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/workers/common"
)

const (
	readyReasonScanFailed      = "ScanFailed"
	repositoryScanTestNewRunID = "scan_new"
	testPatchDiffHeader        = "diff --git a/app.py b/app.py"
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

func TestApplyScanRunProgressPreservesTerminalErrorWithActiveTasks(t *testing.T) {
	completed := mustParseTime(t, "2026-05-04T03:02:01Z")
	run := &storepkg.ScanRun{Phase: scanRunPhaseFailed, ErrorMessage: "scanner policy digest changed", CompletedAt: &completed}

	applyScanRunProgress(run, scanRunProgress{hasActive: true})

	if run.Phase != scanRunPhaseFailed || run.Summary != run.ErrorMessage || run.CompletedAt == nil || !run.CompletedAt.Equal(completed) {
		t.Fatalf("run = %#v, want terminal failure preserved", run)
	}
}

func TestApplyScanRunProgressStampsTerminalErrorCompletion(t *testing.T) {
	completed := mustParseTime(t, "2026-05-04T03:02:01Z")
	run := &storepkg.ScanRun{ErrorMessage: "terminal result binding mismatch"}

	applyScanRunProgress(run, scanRunProgress{latestCompletion: &completed})

	if run.Phase != scanRunPhaseFailed || run.Summary != run.ErrorMessage || run.CompletedAt == nil || !run.CompletedAt.Equal(completed) {
		t.Fatalf("run = %#v, want terminal failure stamped with pipeline completion", run)
	}
}

func TestIngestMapperTaskSkipsFailedRun(t *testing.T) {
	reconciler := &RepositoryScanReconciler{}
	run := &storepkg.ScanRun{Phase: scanRunPhaseFailed, ErrorMessage: "scanner policy digest changed"}
	task := &corev1alpha1.Task{Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded}}

	if err := reconciler.ingestMapperTask(context.Background(), &corev1alpha1.RepositoryScan{}, task, run); err != nil {
		t.Fatalf("ingestMapperTask() error = %v", err)
	}
	if run.Phase != scanRunPhaseFailed || run.ErrorMessage == "" {
		t.Fatalf("run = %#v, want failed run unchanged", run)
	}
}

//nolint:gocyclo // This table-driven regression intentionally verifies terminal state across the run, scan, slice, and retry paths.
func TestRepositoryScanReconcileTreatsCancelledPipelineTasksAsTerminalFailures(t *testing.T) {
	tests := []struct {
		name        string
		stage       string
		sliceID     string
		wantMessage string
	}{
		{name: "threat model", stage: security.StageThreatModel, wantMessage: "threat model stage cancelled"},
		{name: "mapper", stage: security.StageMapper, wantMessage: "mapper stage cancelled"},
		{name: "review", stage: security.StageReview, sliceID: "slice_api", wantMessage: "review stage cancelled"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			securityStore := setupControllerSQLiteStore(t)
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatalf("AddToScheme() error = %v", err)
			}

			nameSuffix := strings.ReplaceAll(tt.stage, "-", "")
			scanName := "cancelled-" + nameSuffix
			runID := "scan_cancelled_" + nameSuffix
			taskName := scanName + "-" + tt.stage
			completed := metav1.NewTime(mustParseTime(t, "2026-05-08T03:04:05Z"))
			scan := &corev1alpha1.RepositoryScan{
				TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
				ObjectMeta: metav1.ObjectMeta{
					Name:       scanName,
					Namespace:  defaultNS,
					UID:        types.UID(scanName + "-uid"),
					Generation: 1,
				},
				Spec: corev1alpha1.RepositoryScanSpec{
					RepoURL:          "https://github.com/example/repo",
					AnalysisAgentRef: corev1alpha1.AgentReference{Name: "scan-reviewer"},
				},
				Status: corev1alpha1.RepositoryScanStatus{
					Phase:            repositoryScanPhaseScanning,
					LastScanID:       runID,
					LastScanTaskName: taskName,
				},
			}
			controllerRef := true
			taskLabels := map[string]string{
				labels.LabelSecurityTarget: labels.SelectorValue(scanName),
				labels.LabelSecurityScanID: runID,
				labels.LabelSecurityMode:   scanModeManual,
				labels.LabelSecurityStage:  tt.stage,
			}
			if tt.sliceID != "" {
				taskLabels[labels.LabelSecuritySliceID] = tt.sliceID
			}
			task := &corev1alpha1.Task{
				TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task"},
				ObjectMeta: metav1.ObjectMeta{
					Name:              taskName,
					Namespace:         defaultNS,
					CreationTimestamp: metav1.NewTime(completed.Add(-time.Minute)),
					Labels:            taskLabels,
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan",
						Name: scanName, UID: scan.UID, Controller: &controllerRef,
					}},
				},
				Status: corev1alpha1.TaskStatus{
					Phase:          corev1alpha1.TaskPhaseCancelled,
					CompletionTime: &completed,
				},
			}
			run := &storepkg.ScanRun{
				ID:             runID,
				Namespace:      defaultNS,
				RepositoryScan: scanName,
				TaskName:       taskName,
				Mode:           scanModeManual,
				Phase:          scanRunPhaseRunning,
				StartedAt:      completed.Add(-2 * time.Minute),
			}
			reserveScanRunForIngestionTest(t, ctx, securityStore, scan, run)
			if tt.sliceID != "" {
				if err := securityStore.UpsertReviewSlice(ctx, &storepkg.ReviewSlice{
					ID:             tt.sliceID,
					Namespace:      defaultNS,
					RepositoryScan: scanName,
					Status:         reviewSliceStatusPending,
					LastScanRunID:  runID,
				}); err != nil {
					t.Fatalf("UpsertReviewSlice() error = %v", err)
				}
			}

			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
				WithObjects(scan, task).
				Build()
			reconciler := &RepositoryScanReconciler{
				Client:        cl,
				Scheme:        scheme,
				SecurityStore: securityStore,
			}
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(scan)}

			assertTerminalFailure := func() (*storepkg.ScanRun, metav1.Condition) {
				t.Helper()
				storedRun, err := securityStore.GetScanRun(ctx, defaultNS, runID)
				if err != nil {
					t.Fatalf("GetScanRun() error = %v", err)
				}
				if storedRun.Phase != scanRunPhaseFailed || storedRun.ErrorMessage != tt.wantMessage || storedRun.Summary != tt.wantMessage {
					t.Fatalf("run phase/error/summary = %q/%q/%q, want failed/%q/%q", storedRun.Phase, storedRun.ErrorMessage, storedRun.Summary, tt.wantMessage, tt.wantMessage)
				}
				if storedRun.CompletedAt == nil || !storedRun.CompletedAt.Equal(completed.Time) {
					t.Fatalf("run.CompletedAt = %v, want %v", storedRun.CompletedAt, completed.Time)
				}

				current := &corev1alpha1.RepositoryScan{}
				if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
					t.Fatalf("Get(RepositoryScan) error = %v", err)
				}
				condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
				if current.Status.Phase != repositoryScanPhaseError || condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != readyReasonScanFailed || condition.Message != tt.wantMessage {
					t.Fatalf("scan status/condition = %q/%#v, want Error/ScanFailed %q", current.Status.Phase, condition, tt.wantMessage)
				}
				if current.Status.LastScanAt == nil || !current.Status.LastScanAt.Time.Equal(completed.Time) {
					t.Fatalf("scan.Status.LastScanAt = %v, want %v", current.Status.LastScanAt, completed.Time)
				}
				if current.Status.LastSuccessfulScanAt != nil {
					t.Fatalf("scan.Status.LastSuccessfulScanAt = %v, want nil", current.Status.LastSuccessfulScanAt)
				}
				if tt.sliceID != "" {
					reviewSlice, err := securityStore.GetReviewSlice(ctx, defaultNS, scanName, tt.sliceID)
					if err != nil {
						t.Fatalf("GetReviewSlice() error = %v", err)
					}
					if reviewSlice.Status != reviewSliceStatusFailed {
						t.Fatalf("review slice status = %q, want %q", reviewSlice.Status, reviewSliceStatusFailed)
					}
				}
				return storedRun, *condition
			}

			result, err := reconciler.Reconcile(ctx, req)
			if err != nil {
				t.Fatalf("first Reconcile() error = %v", err)
			}
			if result != (ctrl.Result{}) {
				t.Fatalf("first Reconcile() result = %#v, want no requeue", result)
			}
			firstRun, firstCondition := assertTerminalFailure()

			result, err = reconciler.Reconcile(ctx, req)
			if err != nil {
				t.Fatalf("second Reconcile() error = %v", err)
			}
			if result != (ctrl.Result{}) {
				t.Fatalf("second Reconcile() result = %#v, want no requeue", result)
			}
			secondRun, secondCondition := assertTerminalFailure()
			if secondRun.ErrorMessage != firstRun.ErrorMessage || secondRun.Summary != firstRun.Summary || secondRun.CompletedAt == nil || firstRun.CompletedAt == nil || !secondRun.CompletedAt.Equal(*firstRun.CompletedAt) {
				t.Fatalf("second reconcile changed terminal run: first=%#v second=%#v", firstRun, secondRun)
			}
			if !secondCondition.LastTransitionTime.Time.Equal(firstCondition.LastTransitionTime.Time) {
				t.Fatalf("Ready transition time changed across idempotent reconcile: first=%v second=%v", firstCondition.LastTransitionTime, secondCondition.LastTransitionTime)
			}

			var tasks corev1alpha1.TaskList
			if err := cl.List(ctx, &tasks, client.InNamespace(defaultNS), client.MatchingLabels(map[string]string{labels.LabelSecurityScanID: runID})); err != nil {
				t.Fatalf("List(Tasks) error = %v", err)
			}
			if len(tasks.Items) != 1 {
				t.Fatalf("len(tasks) = %d, want 1 after idempotent reconcile", len(tasks.Items))
			}
		})
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
					labels.LabelSecurityScanID: repositoryScanTestNewRunID,
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
					labels.LabelSecurityScanID:    repositoryScanTestNewRunID,
					labels.LabelSecurityStage:     security.StagePatch,
					labels.LabelSecurityFindingID: "f1",
				},
			},
		},
	}

	if got := latestOwnedScanPipelineRunID(tasks); got != repositoryScanTestNewRunID {
		t.Fatalf("latestOwnedScanPipelineRunID() = %q, want %q", got, repositoryScanTestNewRunID)
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

type ingestionReservationStore interface {
	storepkg.SecurityStore
	storepkg.SecurityRunTaskInputStore
}

func reserveScanRunForIngestionTest(
	t *testing.T,
	ctx context.Context,
	reservationStore ingestionReservationStore,
	scan *corev1alpha1.RepositoryScan,
	run *storepkg.ScanRun,
) {
	t.Helper()
	if scan == nil || scan.UID == "" || scan.Generation <= 0 {
		t.Fatalf("RepositoryScan identity = %q/%d, want immutable UID/generation", scan.UID, scan.Generation)
	}
	digest := sha256.Sum256([]byte(run.ID))
	run.RunUID = fmt.Sprintf("run_%x", digest)
	run.RepositoryScanUID = string(scan.UID)
	run.RepositoryScanGeneration = scan.Generation
	if run.RequestIdempotencyKey == "" && run.IdempotencyKey == "" {
		run.RequestIdempotencyKey = fmt.Sprintf("req_%x", digest)
		run.IdempotencyKey = run.RequestIdempotencyKey
	}
	desiredPhase := run.Phase
	desiredBundleStatus := run.Quality.BundleStatus
	run.Phase = scanRunPhasePending
	run.Quality.BundleStatus = storepkg.BundleStatusNotStarted
	input := &storepkg.SecurityRunTaskInput{
		RunUID: run.RunUID, Namespace: run.Namespace, RepositoryScan: run.RepositoryScan,
		ScanRunID: run.ID, Stage: security.StageThreatModel,
	}
	if err := reservationStore.CreateScanRunWithTaskInput(ctx, run, input); err != nil {
		t.Fatalf("CreateScanRunWithTaskInput() error = %v", err)
	}
	if desiredPhase != scanRunPhasePending || desiredBundleStatus != storepkg.BundleStatusNotStarted {
		run.Phase = desiredPhase
		run.Quality.BundleStatus = desiredBundleStatus
		if err := reservationStore.UpdateScanRun(ctx, run); err != nil {
			t.Fatalf("UpdateScanRun(fixture state) error = %v", err)
		}
	}
}

func TestIngestScanTaskIgnoresTerminalLegacyTaskWithoutReservedRun(t *testing.T) {
	ctx := context.Background()
	securityStore := setupControllerSQLiteStore(t)
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kaset", Namespace: defaultNS, UID: types.UID("kaset-uid"), Generation: 1,
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "legacy-terminal-mapper", Namespace: defaultNS,
			Labels: map[string]string{
				labels.LabelSecurityTarget: labels.SelectorValue(scan.Name),
				labels.LabelSecurityScanID: "scan_legacy_terminal",
				labels.LabelSecurityStage:  security.StageMapper,
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
	}
	reconciler := &RepositoryScanReconciler{SecurityStore: securityStore, ArtifactStore: securityStore}

	if err := reconciler.ingestScanTask(ctx, scan, task); err != nil {
		t.Fatalf("ingestScanTask() error = %v", err)
	}
	runs, _, err := securityStore.ListScanRuns(ctx, scan.Namespace, scan.Name, 10, "")
	if err != nil {
		t.Fatalf("ListScanRuns() error = %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("runs = %#v, want no fabricated row for terminal legacy Task", runs)
	}
}

func TestIngestOwnedTasksIgnoresTerminalLegacyTaskWithoutReservedRun(t *testing.T) {
	ctx := context.Background()
	securityStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "kaset", Namespace: defaultNS, UID: types.UID("kaset-uid"), Generation: 1,
		},
	}
	controller := true
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "legacy-terminal-review", Namespace: defaultNS,
			Labels: map[string]string{
				labels.LabelSecurityTarget: labels.SelectorValue(scan.Name),
				labels.LabelSecurityScanID: "scan_legacy_terminal",
				labels.LabelSecurityStage:  security.StageReview,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan", Name: scan.Name,
				UID: scan.UID, Controller: &controller,
			}},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(scan, task).Build()
	reconciler := &RepositoryScanReconciler{
		Client: cl, Scheme: scheme, SecurityStore: securityStore, ArtifactStore: securityStore,
	}

	if err := reconciler.ingestOwnedTasks(ctx, scan); err != nil {
		t.Fatalf("ingestOwnedTasks() error = %v", err)
	}
	runs, _, err := securityStore.ListScanRuns(ctx, scan.Namespace, scan.Name, 10, "")
	if err != nil {
		t.Fatalf("ListScanRuns() error = %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("runs = %#v, want no fabricated row for terminal legacy Task", runs)
	}
}

func TestIngestScanTaskRequiresCompleteImmutableRunBinding(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*storepkg.ScanRun)
	}{
		{name: "missing run UID", mutate: func(run *storepkg.ScanRun) { run.RunUID = "" }},
		{name: "missing RepositoryScan UID", mutate: func(run *storepkg.ScanRun) { run.RepositoryScanUID = "" }},
		{name: "missing RepositoryScan generation", mutate: func(run *storepkg.ScanRun) { run.RepositoryScanGeneration = 0 }},
		{name: "different RepositoryScan UID", mutate: func(run *storepkg.ScanRun) { run.RepositoryScanUID = "other-uid" }},
		{name: "different RepositoryScan generation", mutate: func(run *storepkg.ScanRun) { run.RepositoryScanGeneration++ }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			securityStore := setupControllerSQLiteStore(t)
			scan := &corev1alpha1.RepositoryScan{
				ObjectMeta: metav1.ObjectMeta{
					Name: "kaset", Namespace: defaultNS, UID: types.UID("kaset-uid"), Generation: 2,
				},
			}
			run := &storepkg.ScanRun{
				ID: "scan_incomplete_binding", RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Namespace: defaultNS, RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
				RepositoryScanGeneration: scan.Generation, TaskName: "terminal-mapper", Mode: "initial",
				Phase: scanRunPhaseRunning, Summary: "unchanged", StartedAt: time.Now(),
			}
			tt.mutate(run)
			if err := securityStore.CreateScanRun(ctx, run); err != nil {
				t.Fatalf("CreateScanRun() error = %v", err)
			}
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name: "terminal-mapper", Namespace: defaultNS,
					Labels: map[string]string{
						labels.LabelSecurityTarget: labels.SelectorValue(scan.Name),
						labels.LabelSecurityScanID: run.ID,
						labels.LabelSecurityStage:  security.StageMapper,
					},
				},
				Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
			}
			reconciler := &RepositoryScanReconciler{SecurityStore: securityStore, ArtifactStore: securityStore}

			if err := reconciler.ingestScanTask(ctx, scan, task); err != nil {
				t.Fatalf("ingestScanTask() error = %v", err)
			}
			stored, err := securityStore.GetScanRun(ctx, scan.Namespace, run.ID)
			if err != nil {
				t.Fatalf("GetScanRun() error = %v", err)
			}
			if stored.Phase != scanRunPhaseRunning || stored.Summary != "unchanged" {
				t.Fatalf("stored run = %#v, want incomplete binding ignored without writes", stored)
			}
		})
	}
}

func testReviewContext(t *testing.T, sliceID string, paths ...string) (security.ReviewContextManifest, string, string) {
	t.Helper()
	prompt := "Trusted mapper review context for " + sliceID + "\n"
	manifest := security.ReviewContextManifest{
		SchemaVersion:     security.SchemaVersionReviewContext,
		SliceID:           sliceID,
		PromptBytes:       len(prompt),
		ApproximateTokens: (len(prompt) + 3) / 4,
		Prompt:            prompt,
	}
	seen := map[string]struct{}{}
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		manifest.IncludedFiles = append(manifest.IncludedFiles, security.ReviewContextIncludedFile{
			Path:               path,
			Role:               "owned",
			IncludedLineRanges: []security.ReviewContextLineRange{{StartLine: 1, EndLine: 10000}},
			Readable:           true,
		})
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal(review context) error = %v", err)
	}
	parsed, digest, err := security.ParseTrustedReviewContextManifest(data)
	if err != nil {
		t.Fatalf("ParseTrustedReviewContextManifest() error = %v", err)
	}
	canonical, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("json.Marshal(canonical review context) error = %v", err)
	}
	return *parsed, string(canonical), digest
}

func reviewSlicePaths(slice storepkg.ReviewSlice) []string {
	paths := make([]string, 0, len(slice.OwnedFiles)+len(slice.ContextFiles)+len(slice.Tests))
	for _, file := range slice.OwnedFiles {
		paths = append(paths, file.Path)
	}
	for _, file := range slice.ContextFiles {
		paths = append(paths, file.Path)
	}
	for _, test := range slice.Tests {
		paths = append(paths, test.Path)
	}
	return paths
}

func saveMapperArtifactWithContexts(t *testing.T, store *sqlitestore.Store, task *corev1alpha1.Task, artifact security.ReviewSlicesArtifact) {
	t.Helper()
	ctx := context.Background()
	data, err := json.Marshal(artifact)
	if err != nil {
		t.Fatalf("json.Marshal(review slices) error = %v", err)
	}
	if err := store.SaveArtifact(ctx, task.Namespace, task.Name, security.ArtifactSlices, "application/json", data); err != nil {
		t.Fatalf("SaveArtifact(slices) error = %v", err)
	}
	for _, slice := range artifact.Slices {
		_, contextJSON, _ := testReviewContext(t, slice.ID, reviewSlicePaths(slice)...)
		if err := store.SaveArtifact(ctx, task.Namespace, task.Name, security.ReviewContextArtifactName(slice.ID), "application/json", []byte(contextJSON)); err != nil {
			t.Fatalf("SaveArtifact(review context %s) error = %v", slice.ID, err)
		}
	}
}

func bindReviewSliceContext(t *testing.T, slice *storepkg.ReviewSlice) security.ReviewContextManifest {
	t.Helper()
	manifest, contextJSON, digest := testReviewContext(t, slice.ID, reviewSlicePaths(*slice)...)
	slice.ReviewContextJSON = contextJSON
	slice.ReviewContextHash = digest
	return manifest
}

func saveFindingsTaskResult(
	t *testing.T,
	store *sqlitestore.Store,
	task *corev1alpha1.Task,
	repositoryScan, scanID, policyDigest, contextDigest, sliceID string,
	findings security.FindingsV2Artifact,
) {
	t.Helper()
	_ = repositoryScan
	_ = scanID
	_ = policyDigest
	_ = contextDigest
	_ = sliceID
	data, err := json.Marshal(findings)
	if err != nil {
		t.Fatalf("json.Marshal(findings artifact) error = %v", err)
	}
	if err := store.SaveArtifact(context.Background(), task.Namespace, task.Name, security.ArtifactFindingsV2, "application/json", data); err != nil {
		t.Fatalf("SaveArtifact(findings) error = %v", err)
	}
	task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
}

type reviewResultRetryFixture struct {
	ctx        context.Context
	store      *sqlitestore.Store
	client     client.Client
	reconciler *RepositoryScanReconciler
	scan       *corev1alpha1.RepositoryScan
	run        *storepkg.ScanRun
	slice      *storepkg.ReviewSlice
	sourceTask *corev1alpha1.Task
}

func newReviewResultRetryFixture(t *testing.T) *reviewResultRetryFixture {
	t.Helper()
	ctx := context.Background()
	securityStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{
			Name:       "retry-scan",
			Namespace:  defaultNS,
			UID:        types.UID("retry-scan-uid"),
			Generation: 1,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:          "https://github.com/example/repo",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "scan-reviewer"},
		},
		Status: corev1alpha1.RepositoryScanStatus{Phase: repositoryScanPhaseScanning, LastScanID: "scan_retry_result"},
	}
	policyDigest := security.ScannerPolicyDigest(security.ScannerPolicy{})
	run := &storepkg.ScanRun{
		ID:             "scan_retry_result",
		Namespace:      defaultNS,
		RepositoryScan: scan.Name,
		TaskName:       "retry-scan-initial-threat-model",
		Mode:           "initial",
		Phase:          scanRunPhaseRunning,
		PolicyDigest:   policyDigest,
		StartedAt:      time.Now().Add(-time.Minute),
	}
	reserveScanRunForIngestionTest(t, ctx, securityStore, scan, run)
	if err := securityStore.SaveThreatModel(ctx, &storepkg.ThreatModel{
		Namespace:       defaultNS,
		RepositoryScan:  scan.Name,
		Content:         "# Threat model\n\nReview authentication boundaries.",
		Source:          "generated",
		GeneratedByScan: run.ID,
	}); err != nil {
		t.Fatalf("SaveThreatModel() error = %v", err)
	}
	reviewSlice := &storepkg.ReviewSlice{
		SchemaVersion:  1,
		ID:             "slice_api",
		Namespace:      defaultNS,
		RepositoryScan: scan.Name,
		Source:         "deterministic-go-package",
		Title:          "Go package internal/api",
		Summary:        "API handlers",
		Kind:           "package",
		OwnedFiles:     []storepkg.ReviewSliceFile{{Path: "internal/api/security.go", Reason: "source"}},
		Confidence:     "high",
		Status:         reviewSliceStatusPending,
		LastScanRunID:  run.ID,
	}
	bindReviewSliceContext(t, reviewSlice)
	if err := securityStore.UpsertReviewSlice(ctx, reviewSlice); err != nil {
		t.Fatalf("UpsertReviewSlice() error = %v", err)
	}

	sourceTask := &corev1alpha1.Task{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "retry-scan-review-source",
			Namespace: defaultNS,
			Labels: map[string]string{
				labels.LabelManaged:         "true",
				labels.LabelCreatedBy:       "repository-security",
				labels.LabelSecurityTarget:  labels.SelectorValue(scan.Name),
				labels.LabelSecurityScanID:  run.ID,
				labels.LabelSecurityMode:    run.Mode,
				labels.LabelSecurityStage:   security.StageReview,
				labels.LabelSecuritySliceID: reviewSlice.ID,
			},
			Annotations: map[string]string{labels.AnnotationSecurityReviewAttempt: "0"},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAgent,
			AgentRef: &corev1alpha1.AgentReference{Name: "poison-source-agent"},
			Prompt:   "POISON SOURCE PROMPT MUST NOT BE COPIED",
		},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseSucceeded,
			ResultRef: &corev1alpha1.ResultReference{Available: true},
		},
	}
	if err := controllerutil.SetControllerReference(scan, sourceTask, scheme); err != nil {
		t.Fatalf("SetControllerReference() error = %v", err)
	}
	if err := securityStore.SaveArtifact(ctx, sourceTask.Namespace, sourceTask.Name, security.ArtifactFindingsV2, "application/json", []byte(`{"not":"a findings artifact"`)); err != nil {
		t.Fatalf("SaveArtifact(malformed) error = %v", err)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(scan, sourceTask).
		Build()
	reconciler := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: securityStore, ResultStore: securityStore, ArtifactStore: securityStore}

	return &reviewResultRetryFixture{
		ctx:        ctx,
		store:      securityStore,
		client:     cl,
		reconciler: reconciler,
		scan:       scan,
		run:        run,
		slice:      reviewSlice,
		sourceTask: sourceTask,
	}
}

func (f *reviewResultRetryFixture) retryTaskName() string {
	return security.ScanStageRetryTaskName(f.scan.Name, f.run.ID, security.StageReview, f.slice.ID, securityReviewRetryAttempt)
}

func (f *reviewResultRetryFixture) getRetryTask(t *testing.T) *corev1alpha1.Task {
	t.Helper()
	task := &corev1alpha1.Task{}
	if err := f.client.Get(f.ctx, client.ObjectKey{Namespace: defaultNS, Name: f.retryTaskName()}, task); err != nil {
		t.Fatalf("Get(retry Task) error = %v", err)
	}
	return task
}

func TestIngestReviewTaskCreatesOneControllerRebuiltRetry(t *testing.T) {
	fixture := newReviewResultRetryFixture(t)
	if err := fixture.reconciler.ingestScanTask(fixture.ctx, fixture.scan, fixture.sourceTask); err != nil {
		t.Fatalf("ingestScanTask(source) error = %v", err)
	}

	retryTask := fixture.getRetryTask(t)
	if retryTask.Annotations[labels.AnnotationSecurityReviewAttempt] != "1" {
		t.Fatalf("retry attempt annotation = %q, want 1", retryTask.Annotations[labels.AnnotationSecurityReviewAttempt])
	}
	if !metav1.IsControlledBy(retryTask, fixture.scan) {
		t.Fatalf("retry owner references = %#v, want RepositoryScan controller", retryTask.OwnerReferences)
	}
	if retryTask.Spec.AgentRef == nil || retryTask.Spec.AgentRef.Name != fixture.scan.Spec.AnalysisAgentRef.Name {
		t.Fatalf("retry AgentRef = %#v, want controller-rebuilt %q", retryTask.Spec.AgentRef, fixture.scan.Spec.AnalysisAgentRef.Name)
	}
	if strings.Contains(retryTask.Spec.Prompt, "POISON SOURCE PROMPT") ||
		!strings.Contains(retryTask.Spec.Prompt, "Trusted mapper review context for slice_api") ||
		!strings.Contains(retryTask.Spec.Prompt, "only automatic result retry") {
		t.Fatalf("retry prompt was not rebuilt from trusted context: %q", retryTask.Spec.Prompt)
	}
	if retryTask.Spec.Workspace == nil || retryTask.Spec.Workspace.Intent != corev1alpha1.WorkspaceIntentRead || len(retryTask.Spec.Env) != 0 {
		t.Fatalf("retry workspace/env = %#v/%#v, want read workspace and empty env", retryTask.Spec.Workspace, retryTask.Spec.Env)
	}

	run, err := fixture.store.GetScanRun(fixture.ctx, defaultNS, fixture.run.ID)
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if run.Phase != scanRunPhaseRunning || run.ErrorMessage != "" || !strings.Contains(run.Summary, "Retrying review slice") {
		t.Fatalf("run after retry creation = %#v, want running retry state", run)
	}
	reviewSlice, err := fixture.store.GetReviewSlice(fixture.ctx, defaultNS, fixture.scan.Name, fixture.slice.ID)
	if err != nil {
		t.Fatalf("GetReviewSlice() error = %v", err)
	}
	if reviewSlice.Status != reviewSliceStatusPending {
		t.Fatalf("review slice status = %q, want pending during retry", reviewSlice.Status)
	}

	if err := fixture.reconciler.ingestScanTask(fixture.ctx, fixture.scan, fixture.sourceTask); err != nil {
		t.Fatalf("second ingestScanTask(source) error = %v", err)
	}
	var tasks corev1alpha1.TaskList
	if err := fixture.client.List(fixture.ctx, &tasks, client.InNamespace(defaultNS)); err != nil {
		t.Fatalf("List(Tasks) error = %v", err)
	}
	if len(tasks.Items) != 2 {
		t.Fatalf("len(tasks) = %d, want source plus one deterministic retry", len(tasks.Items))
	}
	progress := fixture.reconciler.collectScanRunProgress(fixture.ctx, tasks.Items)
	if progress.reviewCount != 1 || progress.reviewSucceeded != 0 || !progress.hasActive {
		t.Fatalf("logical retry progress = %#v, want one active review slice", progress)
	}
}

func TestIngestReviewTaskAcceptsRetryOnceAndCountsOneLogicalSlice(t *testing.T) {
	fixture := newReviewResultRetryFixture(t)
	if err := fixture.reconciler.ingestScanTask(fixture.ctx, fixture.scan, fixture.sourceTask); err != nil {
		t.Fatalf("ingestScanTask(source) error = %v", err)
	}
	retryTask := fixture.getRetryTask(t)
	findings := security.FindingsV2Artifact{
		SchemaVersion: security.SchemaVersionFindingsV2,
		Repository: security.FindingsV2Repository{
			RepoURL: fixture.scan.Spec.RepoURL,
			Branch:  "main",
		},
		Scan:     security.FindingsV2Scan{Mode: fixture.run.Mode, SliceID: fixture.slice.ID, Summary: "retry completed"},
		Findings: []security.FindingsV2Finding{},
	}
	saveFindingsTaskResult(
		t, fixture.store, retryTask, fixture.scan.Name, fixture.run.ID, fixture.run.PolicyDigest,
		fixture.slice.ReviewContextHash, fixture.slice.ID, findings,
	)
	retryTask.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	retryTask.Status.CompletionTime = &metav1.Time{Time: time.Now()}
	if err := fixture.client.Update(fixture.ctx, retryTask); err != nil {
		t.Fatalf("Update(retry Task) error = %v", err)
	}

	if err := fixture.reconciler.ingestOwnedTasks(fixture.ctx, fixture.scan); err != nil {
		t.Fatalf("ingestOwnedTasks() error = %v", err)
	}
	run, err := fixture.store.GetScanRun(fixture.ctx, defaultNS, fixture.run.ID)
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if run.Phase != scanRunPhaseSucceeded || run.ReviewedSliceCount != 1 || run.ErrorMessage != "" {
		t.Fatalf("run after successful retry = %#v, want one succeeded review slice", run)
	}
	reviewSlice, err := fixture.store.GetReviewSlice(fixture.ctx, defaultNS, fixture.scan.Name, fixture.slice.ID)
	if err != nil {
		t.Fatalf("GetReviewSlice() error = %v", err)
	}
	if reviewSlice.Status != reviewSliceStatusReviewed {
		t.Fatalf("review slice status = %q, want reviewed", reviewSlice.Status)
	}

	if err := fixture.reconciler.ingestOwnedTasks(fixture.ctx, fixture.scan); err != nil {
		t.Fatalf("second ingestOwnedTasks() error = %v", err)
	}
	run, err = fixture.store.GetScanRun(fixture.ctx, defaultNS, fixture.run.ID)
	if err != nil {
		t.Fatalf("GetScanRun(second) error = %v", err)
	}
	if run.ReviewedSliceCount != 1 || run.Phase != scanRunPhaseSucceeded {
		t.Fatalf("idempotent run = %#v, want one succeeded review slice", run)
	}
	var tasks corev1alpha1.TaskList
	if err := fixture.client.List(fixture.ctx, &tasks, client.InNamespace(defaultNS)); err != nil {
		t.Fatalf("List(Tasks) error = %v", err)
	}
	progress := fixture.reconciler.collectScanRunProgress(fixture.ctx, tasks.Items)
	if progress.reviewCount != 1 || progress.reviewSucceeded != 1 {
		t.Fatalf("logical completed progress = %#v, want 1/1", progress)
	}
}

func TestIngestReviewTaskMalformedRetryExhaustsWithoutAttemptTwo(t *testing.T) {
	fixture := newReviewResultRetryFixture(t)
	if err := fixture.reconciler.ingestScanTask(fixture.ctx, fixture.scan, fixture.sourceTask); err != nil {
		t.Fatalf("ingestScanTask(source) error = %v", err)
	}
	retryTask := fixture.getRetryTask(t)
	if err := fixture.store.SaveArtifact(fixture.ctx, retryTask.Namespace, retryTask.Name, security.ArtifactFindingsV2, "application/json", []byte(`{"still":"invalid"`)); err != nil {
		t.Fatalf("SaveArtifact(retry malformed) error = %v", err)
	}
	retryTask.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	retryTask.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	retryTask.Status.CompletionTime = &metav1.Time{Time: time.Now()}
	if err := fixture.client.Update(fixture.ctx, retryTask); err != nil {
		t.Fatalf("Update(retry Task) error = %v", err)
	}

	if err := fixture.reconciler.ingestOwnedTasks(fixture.ctx, fixture.scan); err != nil {
		t.Fatalf("ingestOwnedTasks() error = %v", err)
	}
	run, err := fixture.store.GetScanRun(fixture.ctx, defaultNS, fixture.run.ID)
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if run.Phase != scanRunPhaseFailed || !strings.Contains(run.ErrorMessage, security.ArtifactFindingsV2) {
		t.Fatalf("run after malformed retry = %#v, want terminal parse failure", run)
	}
	reviewSlice, err := fixture.store.GetReviewSlice(fixture.ctx, defaultNS, fixture.scan.Name, fixture.slice.ID)
	if err != nil {
		t.Fatalf("GetReviewSlice() error = %v", err)
	}
	if reviewSlice.Status != reviewSliceStatusFailed {
		t.Fatalf("review slice status = %q, want failed", reviewSlice.Status)
	}
	var tasks corev1alpha1.TaskList
	if err := fixture.client.List(fixture.ctx, &tasks, client.InNamespace(defaultNS)); err != nil {
		t.Fatalf("List(Tasks) error = %v", err)
	}
	if len(tasks.Items) != 2 {
		t.Fatalf("len(tasks) = %d, want no attempt two", len(tasks.Items))
	}
	if err := fixture.reconciler.ingestOwnedTasks(fixture.ctx, fixture.scan); err != nil {
		t.Fatalf("second ingestOwnedTasks() error = %v", err)
	}
	if err := fixture.client.List(fixture.ctx, &tasks, client.InNamespace(defaultNS)); err != nil {
		t.Fatalf("List(Tasks second) error = %v", err)
	}
	if len(tasks.Items) != 2 {
		t.Fatalf("len(tasks after repeat) = %d, want exhausted retry budget", len(tasks.Items))
	}
}

func TestIngestReviewTaskRejectsConflictingDeterministicRetry(t *testing.T) {
	fixture := newReviewResultRetryFixture(t)
	conflict := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: fixture.retryTaskName(), Namespace: defaultNS}}
	if err := fixture.client.Create(fixture.ctx, conflict); err != nil {
		t.Fatalf("Create(conflicting retry) error = %v", err)
	}

	err := fixture.reconciler.ingestScanTask(fixture.ctx, fixture.scan, fixture.sourceTask)
	if err == nil || !strings.Contains(err.Error(), "conflicts with the expected retry identity") {
		t.Fatalf("ingestScanTask(source) error = %v, want deterministic-name conflict", err)
	}
	run, getErr := fixture.store.GetScanRun(fixture.ctx, defaultNS, fixture.run.ID)
	if getErr != nil {
		t.Fatalf("GetScanRun() error = %v", getErr)
	}
	if run.Phase != scanRunPhaseRunning || run.ErrorMessage != "" {
		t.Fatalf("run after collision = %#v, want unchanged active run", run)
	}
}

func TestIngestReviewTaskRejectsInvalidAttemptIdentityWithoutRetry(t *testing.T) {
	tests := []struct {
		name         string
		annotation   string
		useRetryName bool
	}{
		{name: "malformed", annotation: "not-a-number"},
		{name: "negative", annotation: "-1"},
		{name: "out of range", annotation: "2"},
		{name: "retry attempt on source name", annotation: "1"},
		{name: "initial attempt on retry name", annotation: "0", useRetryName: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newReviewResultRetryFixture(t)
			fixture.sourceTask.Annotations[labels.AnnotationSecurityReviewAttempt] = tt.annotation
			if tt.useRetryName {
				fixture.sourceTask.Name = fixture.retryTaskName()
			}

			if err := fixture.reconciler.ingestScanTask(fixture.ctx, fixture.scan, fixture.sourceTask); err != nil {
				t.Fatalf("ingestScanTask() error = %v", err)
			}
			run, err := fixture.store.GetScanRun(fixture.ctx, defaultNS, fixture.run.ID)
			if err != nil {
				t.Fatalf("GetScanRun() error = %v", err)
			}
			if run.Phase != scanRunPhaseFailed || !strings.Contains(run.ErrorMessage, "invalid security review attempt identity") {
				t.Fatalf("run = %#v, want invalid attempt failure", run)
			}
			var tasks corev1alpha1.TaskList
			if err := fixture.client.List(fixture.ctx, &tasks, client.InNamespace(defaultNS)); err != nil {
				t.Fatalf("List(Tasks) error = %v", err)
			}
			if len(tasks.Items) != 1 {
				t.Fatalf("len(tasks) = %d, want no retry", len(tasks.Items))
			}
		})
	}
}

func TestIngestReviewTaskResultRetryEligibility(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*reviewResultRetryFixture)
		wantRetry bool
		wantError string
	}{
		{
			name: "result reference unavailable",
			mutate: func(f *reviewResultRetryFixture) {
				f.sourceTask.Status.ResultRef = nil
			},
			wantRetry: true,
		},
		{
			name: "result missing after available reference",
			mutate: func(f *reviewResultRetryFixture) {
				f.sourceTask.Name = "retry-scan-review-result-missing"
			},
			wantRetry: true,
		},
		{
			name: "artifact store unavailable",
			mutate: func(f *reviewResultRetryFixture) {
				f.reconciler.ArtifactStore = nil
			},
			wantError: "artifact store is not configured",
		},
		{
			name: "trusted context corrupt",
			mutate: func(f *reviewResultRetryFixture) {
				f.slice.ReviewContextJSON = `{"schemaVersion":1,"sliceId":"wrong"}`
				f.slice.ReviewContextHash = "sha256:wrong"
				if err := f.store.UpsertReviewSlice(f.ctx, f.slice); err != nil {
					t.Fatalf("UpsertReviewSlice(corrupt) error = %v", err)
				}
			},
			wantError: "trusted review context",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newReviewResultRetryFixture(t)
			tt.mutate(fixture)
			if err := fixture.reconciler.ingestScanTask(fixture.ctx, fixture.scan, fixture.sourceTask); err != nil {
				t.Fatalf("ingestScanTask() error = %v", err)
			}

			var tasks corev1alpha1.TaskList
			if err := fixture.client.List(fixture.ctx, &tasks, client.InNamespace(defaultNS)); err != nil {
				t.Fatalf("List(Tasks) error = %v", err)
			}
			wantTasks := 1
			if tt.wantRetry {
				wantTasks = 2
			}
			if len(tasks.Items) != wantTasks {
				t.Fatalf("len(tasks) = %d, want %d", len(tasks.Items), wantTasks)
			}
			run, err := fixture.store.GetScanRun(fixture.ctx, defaultNS, fixture.run.ID)
			if err != nil {
				t.Fatalf("GetScanRun() error = %v", err)
			}
			if tt.wantRetry {
				if run.Phase != scanRunPhaseRunning || run.ErrorMessage != "" {
					t.Fatalf("retryable run = %#v, want active", run)
				}
				return
			}
			if run.Phase != scanRunPhaseFailed || !strings.Contains(run.ErrorMessage, tt.wantError) {
				t.Fatalf("non-retryable run = %#v, want failure containing %q", run, tt.wantError)
			}
		})
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
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS, UID: types.UID("kaset-uid"), Generation: 1},
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
	reserveScanRunForIngestionTest(t, ctx, store, scan, &storepkg.ScanRun{
		ID: "scan_mapper", Namespace: defaultNS, RepositoryScan: scan.Name,
		TaskName: task.Name, Mode: "initial", Phase: scanRunPhaseRunning, StartedAt: time.Now(),
	})
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
	saveMapperArtifactWithContexts(t, store, task, artifact)

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
	if got.ReviewContextJSON == "" || got.ReviewContextHash == "" {
		t.Fatalf("review slice context = %q/%q, want persisted mapper context", got.ReviewContextJSON, got.ReviewContextHash)
	}
	parsedContext, digest, err := security.ParseTrustedReviewContextManifest([]byte(got.ReviewContextJSON))
	if err != nil {
		t.Fatalf("ParseTrustedReviewContextManifest(stored) error = %v", err)
	}
	if parsedContext.SliceID != got.ID || digest != got.ReviewContextHash {
		t.Fatalf("stored review context = slice %q digest %q, want %q/%q", parsedContext.SliceID, digest, got.ID, got.ReviewContextHash)
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
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS, UID: types.UID("kaset-uid"), Generation: 1},
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
	saveMapperArtifactWithContexts(t, store, task, artifact)
	reserveScanRunForIngestionTest(t, ctx, store, scan, &storepkg.ScanRun{
		ID:             "scan_incremental_mapper",
		Namespace:      defaultNS,
		RepositoryScan: "kaset",
		TaskName:       task.Name,
		Mode:           "incremental",
		Phase:          scanRunPhaseRunning,
		BaseCommit:     "base123",
		IdempotencyKey: "original-active-key",
		StartedAt:      time.Now(),
	})

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
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS, UID: types.UID("kaset-uid"), Generation: 1},
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
	reserveScanRunForIngestionTest(t, ctx, store, scan, &storepkg.ScanRun{
		ID: "scan_mapper_reingest", Namespace: defaultNS, RepositoryScan: scan.Name,
		TaskName: mapperTask.Name, Mode: "initial", Phase: scanRunPhaseRunning, StartedAt: time.Now(),
	})
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
	saveMapperArtifactWithContexts(t, store, mapperTask, mapperArtifact)

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
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS, UID: types.UID("kaset-policy-uid"), Generation: 2},
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
	run := &storepkg.ScanRun{ID: "scan_policy", Namespace: defaultNS, RepositoryScan: "kaset", RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: scan.Generation, Mode: "initial", Phase: scanRunPhaseRunning}
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

func TestRepositoryScanIdempotencyRepairsPendingRunWithoutReplacement(t *testing.T) {
	ctx := context.Background()
	securityStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "kaset", Namespace: defaultNS, UID: types.UID("kaset-repair-uid"), Generation: 2,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "scan-reviewer"},
		},
	}
	policyDigest := security.ScannerPolicyDigest(security.ScannerPolicy{})
	requestKey := security.RequestIdempotencyKey(scan, scanModeIncremental, "base", "", policyDigest)
	runUID := "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	run := &storepkg.ScanRun{
		ID: security.PublicScanRunID(runUID), RunUID: runUID, Namespace: defaultNS, RepositoryScan: scan.Name,
		RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: scan.Generation,
		TaskName: security.ScanStageTaskNameForRun(scan.Name, scanModeIncremental, security.StageThreatModel, "", runUID),
		Mode:     scanModeIncremental, Phase: scanRunPhasePending, BaseCommit: "base",
		RequestIdempotencyKey: requestKey, IdempotencyKey: requestKey, PolicyDigest: policyDigest,
		Quality: initialScanQuality(scan, false), StartedAt: time.Now().UTC(),
	}
	if err := securityStore.CreateScanRun(ctx, run); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	if _, err := securityStore.SaveSecurityRunTaskInput(ctx, &storepkg.SecurityRunTaskInput{
		RunUID: run.RunUID, Namespace: run.Namespace, RepositoryScan: run.RepositoryScan,
		ScanRunID: run.ID, Stage: security.StageThreatModel,
	}); err != nil {
		t.Fatalf("SaveSecurityRunTaskInput() error = %v", err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan).Build()
	reconciler := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: securityStore, RunTaskInputStore: securityStore}
	if err := reconciler.createScanRun(ctx, scan, scanModeIncremental, "base", ""); err != nil {
		t.Fatalf("createScanRun() error = %v", err)
	}

	runs, _, err := securityStore.ListScanRuns(ctx, defaultNS, scan.Name, 10, "")
	if err != nil {
		t.Fatalf("ListScanRuns() error = %v", err)
	}
	if len(runs) != 1 || runs[0].ID != run.ID || runs[0].Phase != scanRunPhasePending {
		t.Fatalf("runs = %#v, want original pending run only", runs)
	}
	task := &corev1alpha1.Task{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: defaultNS, Name: run.TaskName}, task); err != nil {
		t.Fatalf("Get(repaired task) error = %v", err)
	}
	if task.Labels[labels.LabelSecurityScanID] != run.ID {
		t.Fatalf("task scan ID = %q, want %q", task.Labels[labels.LabelSecurityScanID], run.ID)
	}
}

func TestCreateScanRunLeavesPendingRunAfterTaskAdmissionError(t *testing.T) {
	ctx := context.Background()
	securityStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "task-admission-retry", Namespace: defaultNS, UID: types.UID("task-admission-retry-uid"), Generation: 1,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "scan-reviewer"},
		},
	}
	createCalls := 0
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(scan).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1alpha1.Task); !ok {
					return c.Create(ctx, obj, opts...)
				}
				createCalls++
				if createCalls == 1 {
					return apierrors.NewBadRequest("simulated task rejection")
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
	if err := securityStore.SaveThreatModel(ctx, &storepkg.ThreatModel{
		Namespace: scan.Namespace, RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
		RepositoryScanGeneration: scan.Generation, Content: "original controller threat model", Source: "user",
	}); err != nil {
		t.Fatalf("SaveThreatModel(original) error = %v", err)
	}
	reconciler := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: securityStore, RunTaskInputStore: securityStore}

	if err := reconciler.createScanRun(ctx, scan, "initial", "", ""); err == nil {
		t.Fatal("first createScanRun() error = nil, want task admission error")
	}
	runs, _, err := securityStore.ListScanRuns(ctx, defaultNS, scan.Name, 10, "")
	if err != nil {
		t.Fatalf("ListScanRuns(first) error = %v", err)
	}
	if len(runs) != 1 || runs[0].Phase != scanRunPhasePending || runs[0].CompletedAt != nil {
		t.Fatalf("runs after rejection = %#v, want one repairable pending run", runs)
	}
	originalRunID := runs[0].ID
	if err := securityStore.SaveThreatModel(ctx, &storepkg.ThreatModel{
		Namespace: scan.Namespace, RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
		RepositoryScanGeneration: scan.Generation, Content: "replacement controller threat model", Source: "user",
	}); err != nil {
		t.Fatalf("SaveThreatModel(replacement) error = %v", err)
	}

	if err := reconciler.createScanRun(ctx, scan, "initial", "", ""); err != nil {
		t.Fatalf("retry createScanRun() error = %v", err)
	}
	runs, _, err = securityStore.ListScanRuns(ctx, defaultNS, scan.Name, 10, "")
	if err != nil {
		t.Fatalf("ListScanRuns(retry) error = %v", err)
	}
	if len(runs) != 1 || runs[0].ID != originalRunID || runs[0].Phase != scanRunPhasePending {
		t.Fatalf("runs after retry = %#v, want original pending run", runs)
	}
	task := &corev1alpha1.Task{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: defaultNS, Name: runs[0].TaskName}, task); err != nil {
		t.Fatalf("Get(recovered task) error = %v", err)
	}
	if !strings.Contains(task.Spec.Prompt, "original controller threat model") ||
		strings.Contains(task.Spec.Prompt, "replacement controller threat model") {
		t.Fatalf("recovered task prompt did not retain immutable input snapshot: %q", task.Spec.Prompt)
	}
}

func TestCreateScanRunConcurrentReconcilesCreateOnePipeline(t *testing.T) {
	ctx := context.Background()
	securityStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{Name: "concurrent-security", Namespace: defaultNS, UID: types.UID("concurrent-security-uid"), Generation: 1},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:          "https://github.com/example/repo",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "scan-reviewer"},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan).Build()
	reconciler := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: securityStore}

	start := make(chan struct{})
	results := make(chan error, 4)
	for range 4 {
		go func() {
			<-start
			results <- reconciler.createScanRun(ctx, scan, "manual", "base", "")
		}()
	}
	close(start)
	for range 4 {
		if err := <-results; err != nil {
			t.Fatalf("createScanRun() error = %v", err)
		}
	}

	var tasks corev1alpha1.TaskList
	if err := cl.List(ctx, &tasks, client.InNamespace(defaultNS)); err != nil {
		t.Fatalf("List(Task) error = %v", err)
	}
	if len(tasks.Items) != 1 || taskSecurityStage(&tasks.Items[0]) != security.StageThreatModel {
		t.Fatalf("tasks = %#v, want one threat-model task", tasks.Items)
	}
	runs, _, err := securityStore.ListScanRuns(ctx, defaultNS, scan.Name, 10, "")
	if err != nil {
		t.Fatalf("ListScanRuns() error = %v", err)
	}
	if len(runs) != 1 || runs[0].Phase != scanRunPhasePending {
		t.Fatalf("runs = %#v, want one pending run", runs)
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
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS, UID: types.UID("kaset-review-uid"), Generation: 1},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:          "https://github.com/sozercan/kaset",
			Branch:           "main",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "scan-reviewer"},
		},
		Status: corev1alpha1.RepositoryScanStatus{LastScanID: "scan_review"},
	}
	threatTask := newSucceededSecurityTask("kaset-initial-threat", "scan_review", security.StageThreatModel, metav1.Now())
	mapperTask := newSucceededSecurityTask("kaset-initial-mapper", "scan_review", security.StageMapper, metav1.Now())
	for _, task := range []*corev1alpha1.Task{threatTask, mapperTask} {
		if err := controllerutil.SetControllerReference(scan, task, scheme); err != nil {
			t.Fatalf("SetControllerReference() error = %v", err)
		}
	}
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
		ID:                       "scan_review",
		Namespace:                defaultNS,
		RepositoryScan:           "kaset",
		RepositoryScanUID:        string(scan.UID),
		RepositoryScanGeneration: scan.Generation,
		TaskName:                 threatTask.Name,
		Mode:                     "initial",
		Phase:                    scanRunPhasePending,
		StartedAt:                time.Now(),
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
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS, UID: types.UID("kaset-partial-review-uid"), Generation: 1},
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
	for _, task := range []*corev1alpha1.Task{threatTask, mapperTask, reviewTask} {
		if err := controllerutil.SetControllerReference(scan, task, scheme); err != nil {
			t.Fatalf("SetControllerReference() error = %v", err)
		}
	}
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
		ID:                       "scan_partial_review",
		Namespace:                defaultNS,
		RepositoryScan:           "kaset",
		RepositoryScanUID:        string(scan.UID),
		RepositoryScanGeneration: scan.Generation,
		TaskName:                 threatTask.Name,
		Mode:                     "initial",
		Phase:                    scanRunPhaseRunning,
		SliceCount:               2,
		StartedAt:                time.Now(),
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
		bindReviewSliceContext(t, &slice)
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
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS, UID: types.UID("kaset-uid"), Generation: 3},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:          "https://github.com/sozercan/kaset",
			Branch:           "main",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "scan-reviewer"},
		},
		Status: corev1alpha1.RepositoryScanStatus{LastScanID: "scan_noop_incremental"},
	}
	threatTask := newSucceededSecurityTask("kaset-incremental-threat", "scan_noop_incremental", security.StageThreatModel, metav1.Now())
	mapperTask := newSucceededSecurityTask("kaset-incremental-mapper", "scan_noop_incremental", security.StageMapper, metav1.Now())
	controller := true
	owner := metav1.OwnerReference{
		APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan", Name: scan.Name,
		UID: scan.UID, Controller: &controller,
	}
	threatTask.OwnerReferences = []metav1.OwnerReference{owner}
	mapperTask.OwnerReferences = []metav1.OwnerReference{owner}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(scan, threatTask, mapperTask).
		Build()
	reconciler := &RepositoryScanReconciler{
		Client:         cl,
		Scheme:         scheme,
		SecurityStore:  store,
		IntegrityStore: store,
		IntegrityConfig: security.IntegrityConfig{
			QualityStateWritesEnabled: true,
			FindingObservationWrites:  true,
		},
	}
	quality := initialScanQuality(scan, false)
	quality.InventoryCoverageStatus = storepkg.CoverageStatusComplete
	quality.TargetVerification = storepkg.TargetVerificationUnverified
	quality.AnalysisAttestationLevel = storepkg.AnalysisAttestationToolObserved
	quality.IsolationStatus = storepkg.IsolationStatusHardened
	if err := store.CreateScanRun(ctx, &storepkg.ScanRun{
		ID:                       "scan_noop_incremental",
		RunUID:                   "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Namespace:                defaultNS,
		RepositoryScan:           "kaset",
		RepositoryScanUID:        string(scan.UID),
		RepositoryScanGeneration: scan.Generation,
		TaskName:                 threatTask.Name,
		Mode:                     "incremental",
		Phase:                    scanRunPhaseRunning,
		BaseCommit:               "base123",
		HeadCommit:               "head456",
		SliceCount:               2,
		SkippedSliceCount:        2,
		Summary:                  "Threat model generated; no review slices matched 1 changed files",
		Quality:                  quality,
		StartedAt:                time.Now(),
	}); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	if _, err := store.SaveSecurityRunThreatModel(ctx, &storepkg.SecurityRunThreatModel{
		RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Namespace: defaultNS,
		RepositoryScan: scan.Name, ScanRunID: "scan_noop_incremental", Version: 1, Content: "# Threat model",
	}); err != nil {
		t.Fatalf("SaveSecurityRunThreatModel() error = %v", err)
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
	if current.Status.Quality == nil || current.Status.Quality.CandidateCoverageStatus != string(storepkg.CoverageStatusComplete) ||
		current.Status.Quality.ValidationExecution != string(storepkg.QualityExecutionComplete) ||
		current.Status.Quality.AttackPathExecution != string(storepkg.QualityExecutionComplete) {
		t.Fatalf("Status.Quality = %#v, want terminal no-op quality", current.Status.Quality)
	}
	qualityReady := meta.FindStatusCondition(current.Status.Conditions, "QualityReady")
	if qualityReady == nil || qualityReady.Status != metav1.ConditionFalse || qualityReady.Reason != "QualityDegraded" {
		t.Fatalf("QualityReady = %#v, want False/QualityDegraded", qualityReady)
	}
}

func TestPersistNoopScanRunSkipsRewriteAfterBundleSeal(t *testing.T) {
	ctx := context.Background()
	securityStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "sealed-noop", Namespace: defaultNS, UID: types.UID("sealed-noop-uid"), Generation: 4,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/repo", Branch: "main",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
		Status: corev1alpha1.RepositoryScanStatus{Phase: repositoryScanPhaseScanning, LastScanID: "scan_sealed_noop"},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(scan).
		Build()
	completed := time.Now().UTC()
	run := &storepkg.ScanRun{
		ID: "scan_sealed_noop", RunUID: "run_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		Namespace: defaultNS, RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
		RepositoryScanGeneration: scan.Generation, TaskName: "sealed-noop-task", Mode: "manual",
		Phase: scanRunPhaseSucceeded, StartedAt: completed.Add(-time.Minute), CompletedAt: &completed,
		HeadCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Summary:    "Threat model generated; no reviewable security slices found",
		Quality: storepkg.ScanQuality{
			SchemaVersion: storepkg.SecurityQualitySchemaVersion, BundleStatus: storepkg.BundleStatusSealed,
			InventoryCoverageStatus: storepkg.CoverageStatusComplete, CandidateCoverageStatus: storepkg.CoverageStatusComplete,
			CoverageStatus: storepkg.CoverageStatusComplete, ValidationScope: storepkg.ValidationScopeOff,
			ValidationExecution: storepkg.QualityExecutionComplete, AttackPathExecution: storepkg.QualityExecutionDeferred,
		},
	}
	if err := securityStore.CreateScanRun(ctx, run); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	stale := *run
	stale.Summary = "stale post-seal rewrite"
	if err := securityStore.UpdateScanRun(ctx, &stale); err == nil {
		t.Fatal("UpdateScanRun() error = nil, want sealed-run conflict")
	}

	reconciler := &RepositoryScanReconciler{
		Client: cl, Scheme: scheme, SecurityStore: securityStore,
		IntegrityConfig: security.IntegrityConfig{QualityStateWritesEnabled: true},
	}
	if err := reconciler.persistNoopScanRun(ctx, &stale); err != nil {
		t.Fatalf("persistNoopScanRun() error = %v", err)
	}
	if stale.Summary != run.Summary {
		t.Fatalf("persistNoopScanRun() summary = %q, want stored sealed summary %q", stale.Summary, run.Summary)
	}
	if err := reconciler.updateNoopScanStatus(ctx, scan, &stale); err != nil {
		t.Fatalf("updateNoopScanStatus() error = %v", err)
	}
	current := &corev1alpha1.RepositoryScan{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		t.Fatalf("Get(scan) error = %v", err)
	}
	if current.Status.Phase != repositoryScanPhaseReady || current.Status.LastScanID != run.ID {
		t.Fatalf("scan status = %#v, want Ready for sealed no-op run", current.Status)
	}
	if current.Status.LastBundleSealedCommit != run.HeadCommit {
		t.Fatalf("LastBundleSealedCommit = %q, want %q", current.Status.LastBundleSealedCommit, run.HeadCommit)
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
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS, UID: types.UID("kaset-uid"), Generation: 1},
	}
	reserveScanRunForIngestionTest(t, ctx, store, scan, &storepkg.ScanRun{
		ID: "scan_mismatched_slice", Namespace: defaultNS, RepositoryScan: scan.Name,
		TaskName: "kaset-review-mismatched-slice", Mode: "initial", Phase: scanRunPhaseRunning, StartedAt: time.Now(),
	})
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
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS, UID: types.UID("kaset-uid"), Generation: 1},
		Spec: corev1alpha1.RepositoryScanSpec{
			MaxFindingsPerRun: &maxFindings,
		},
	}
	reserveScanRunForIngestionTest(t, ctx, store, scan, &storepkg.ScanRun{
		ID:             "scan_review_ingest",
		Namespace:      defaultNS,
		RepositoryScan: "kaset",
		TaskName:       "kaset-review-slice",
		Mode:           "initial",
		Phase:          scanRunPhaseRunning,
		BaseCommit:     "trusted-base",
		HeadCommit:     "trusted-head",
		StartedAt:      time.Now(),
	})
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
	scan := &corev1alpha1.RepositoryScan{ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS, UID: types.UID("kaset-uid"), Generation: 1}, Spec: corev1alpha1.RepositoryScanSpec{MaxFindingsPerRun: &maxFindings}}
	reserveScanRunForIngestionTest(t, ctx, store, scan, &storepkg.ScanRun{ID: "scan_review_filter", Namespace: defaultNS, RepositoryScan: "kaset", TaskName: "kaset-review-filter", Mode: "initial", Phase: scanRunPhaseRunning, HeadCommit: "trusted-head", StartedAt: time.Now()})
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
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS, UID: types.UID("kaset-uid"), Generation: 1},
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
	reserveScanRunForIngestionTest(t, ctx, store, scan, run)
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
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS, UID: types.UID("kaset-uid"), Generation: 1},
	}
	reserveScanRunForIngestionTest(t, ctx, store, scan, &storepkg.ScanRun{
		ID:             "scan_old_review",
		Namespace:      defaultNS,
		RepositoryScan: "kaset",
		TaskName:       "kaset-review-slice-old",
		Mode:           "initial",
		Phase:          scanRunPhaseSucceeded,
		BaseCommit:     "old-base",
		HeadCommit:     "old-head",
		StartedAt:      time.Now().Add(-1 * time.Hour),
	})
	reserveScanRunForIngestionTest(t, ctx, store, scan, &storepkg.ScanRun{
		ID:             "scan_new_review",
		Namespace:      defaultNS,
		RepositoryScan: "kaset",
		Mode:           scanModeIncremental,
		Phase:          scanRunPhaseRunning,
		BaseCommit:     "new-base",
		HeadCommit:     "new-head",
		StartedAt:      time.Now(),
	})
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
		repositoryScanTestNewRunID,
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
	if latest.GeneratedByScan != repositoryScanTestNewRunID {
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

	content, _, validationProblem, err := reconciler.loadThreatModelArtifact(ctx, task)
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

func TestIngestReservedScanTaskSkipsFrozenBundle(t *testing.T) {
	for _, bundleStatus := range []storepkg.BundleStatus{storepkg.BundleStatusSealing, storepkg.BundleStatusSealed} {
		for _, stage := range []string{security.StageThreatModel, security.StageMapper, security.StageReview} {
			t.Run(string(bundleStatus)+"/"+stage, func(t *testing.T) {
				ctx := context.Background()
				securityStore := setupControllerSQLiteStore(t)
				scan := &corev1alpha1.RepositoryScan{
					ObjectMeta: metav1.ObjectMeta{
						Name: "frozen-ingestion", Namespace: defaultNS,
						UID: types.UID("frozen-ingestion-uid"), Generation: 3,
					},
				}
				completed := time.Now().UTC()
				run := &storepkg.ScanRun{
					ID: "scan_frozen_ingestion", RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					Namespace: scan.Namespace, RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
					RepositoryScanGeneration: scan.Generation, TaskName: "original-task", Mode: "manual",
					Phase: scanRunPhaseSucceeded, Summary: "frozen summary", StartedAt: completed.Add(-time.Minute), CompletedAt: &completed,
					Quality: storepkg.LegacyScanQuality(),
				}
				run.Quality.BundleStatus = bundleStatus
				if err := securityStore.CreateScanRun(ctx, run); err != nil {
					t.Fatalf("CreateScanRun() error = %v", err)
				}
				task := &corev1alpha1.Task{
					ObjectMeta: metav1.ObjectMeta{
						Name: "late-" + stage, Namespace: scan.Namespace,
						Labels: map[string]string{
							labels.LabelSecurityScanID: run.ID,
							labels.LabelSecurityStage:  stage,
						},
					},
					Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseFailed, Message: "late failure"},
				}
				r := &RepositoryScanReconciler{SecurityStore: securityStore}

				if _, err := r.ingestReservedScanTask(ctx, scan, task); err != nil {
					t.Fatalf("ingestReservedScanTask() error = %v", err)
				}
				stored, err := securityStore.GetScanRun(ctx, run.Namespace, run.ID)
				if err != nil {
					t.Fatalf("GetScanRun() error = %v", err)
				}
				if stored.Phase != scanRunPhaseSucceeded || stored.TaskName != run.TaskName ||
					stored.Summary != run.Summary || stored.ErrorMessage != "" || stored.Quality.BundleStatus != bundleStatus {
					t.Fatalf("stored run = %#v, want frozen run unchanged", stored)
				}
			})
		}
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

func TestIngestValidationTaskWithBindingIgnoresMissingSourceRun(t *testing.T) {
	ctx := context.Background()
	securityStore := setupControllerSQLiteStore(t)
	reconciler := &RepositoryScanReconciler{SecurityStore: securityStore, ArtifactStore: securityStore}
	scan := &corev1alpha1.RepositoryScan{ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS}}
	finding := &storepkg.Finding{
		ID: "fnd_bound_missing_run", Namespace: defaultNS, RepositoryScan: scan.Name,
		ScanRunID: "scan_missing", Fingerprint: "fp-bound", Title: "Bound validation",
		Summary: "candidate", Severity: "high", Confidence: "high",
		ValidationStatus: "unvalidated", State: findingStateOpen,
	}
	if err := securityStore.UpsertFinding(ctx, finding); err != nil {
		t.Fatalf("UpsertFinding() error = %v", err)
	}
	artifact, err := json.Marshal(security.ValidationArtifact{
		Version: 1, FindingID: finding.ID, ScanRunID: finding.ScanRunID,
		Status: findingValidationStatusValidated, Summary: "must not be accepted",
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "bound-validation", Namespace: defaultNS,
			Annotations: map[string]string{security.AnnotationValidationBindingVersion: security.ValidationBindingVersion},
			Labels: map[string]string{
				labels.LabelSecurityFindingID: finding.ID, labels.LabelSecurityScanID: finding.ScanRunID,
				labels.LabelSecurityStage: security.StageValidation,
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
	}
	if err := securityStore.SaveArtifact(ctx, task.Namespace, task.Name, security.ArtifactValidation, "application/json", artifact); err != nil {
		t.Fatalf("SaveArtifact() error = %v", err)
	}
	if err := reconciler.ingestValidationTask(ctx, scan, task); err != nil {
		t.Fatalf("ingestValidationTask() error = %v", err)
	}
	got, err := securityStore.GetFinding(ctx, defaultNS, finding.ID)
	if err != nil {
		t.Fatalf("GetFinding() error = %v", err)
	}
	if got.ValidationStatus != "unvalidated" || got.ValidationJSON != "" {
		t.Fatalf("finding validation = %q/%q, want missing bound run rejected without mutation", got.ValidationStatus, got.ValidationJSON)
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
				labels.LabelSecurityScanID: repositoryScanTestNewRunID,
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
				labels.LabelSecurityScanID: repositoryScanTestNewRunID,
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
		ID:             repositoryScanTestNewRunID,
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

	run, err := store.GetScanRun(ctx, defaultNS, repositoryScanTestNewRunID)
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if run.Phase != scanRunPhaseSucceeded || run.CompletedAt == nil {
		t.Fatalf("run phase/completedAt = %q/%v, want succeeded with completion", run.Phase, run.CompletedAt)
	}
}

func TestCreateScanRunTreatsLegacyTaskAsActiveBlocker(t *testing.T) {
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
			Name: "demo-security-repository-20260425175643", Namespace: defaultNS, UID: types.UID("scan-idempotent-uid"),
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
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan", Name: scan.Name,
				UID: scan.UID, Controller: new(true),
			}},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAgent,
			AgentRef: &scan.Spec.AnalysisAgentRef,
			Prompt:   security.BuildThreatModelResultPrompt(scan, "initial", "", "", "", security.AgentResultBinding{RepositoryScan: scan.Name, ScanID: scanID}),
			Timeout:  &timeout,
			Priority: &priority,
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
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

	runs, _, err := store.ListScanRuns(ctx, scan.Namespace, scan.Name, 10, "")
	if err != nil {
		t.Fatalf("ListScanRuns() error = %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("runs = %#v, want no synthesized run for unverifiable legacy Task", runs)
	}
	current := &corev1alpha1.RepositoryScan{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		t.Fatalf("Get(scan) error = %v", err)
	}
	if current.Status.Phase != repositoryScanPhasePending || current.Status.LastScanID != "" || current.Status.LastScanTaskName != "" {
		t.Fatalf("scan.Status = %#v, want unchanged pending state", current.Status)
	}
}

type conflictInjectingSecurityStore struct {
	storepkg.SecurityStore
	storepkg.SecurityRunTaskInputStore
	injected        bool
	injectedRun     *storepkg.ScanRun
	mutateCompeting func(*storepkg.ScanRun)
}

func (s *conflictInjectingSecurityStore) CreateScanRun(ctx context.Context, requested *storepkg.ScanRun) error {
	if s.injected {
		return s.SecurityStore.CreateScanRun(ctx, requested)
	}
	s.injected = true
	runUID := "run_9999999999999999999999999999999999999999999999999999999999999999"
	competing := *requested
	competing.ID = security.PublicScanRunID(runUID)
	competing.RunUID = runUID
	competing.TaskName = security.ScanStageTaskNameForRun(
		requested.RepositoryScan, requested.Mode, security.StageThreatModel, "", runUID,
	)
	if s.mutateCompeting != nil {
		s.mutateCompeting(&competing)
	}
	if err := s.SecurityStore.CreateScanRun(ctx, &competing); err != nil {
		return err
	}
	s.injectedRun = &competing
	return fmt.Errorf("%w: injected concurrent request", storepkg.ErrConflict)
}

func (s *conflictInjectingSecurityStore) CreateScanRunWithTaskInput(
	ctx context.Context,
	requested *storepkg.ScanRun,
	input *storepkg.SecurityRunTaskInput,
) error {
	if s.injected {
		return s.SecurityRunTaskInputStore.CreateScanRunWithTaskInput(ctx, requested, input)
	}
	s.injected = true
	runUID := "run_9999999999999999999999999999999999999999999999999999999999999999"
	competing := *requested
	competing.ID = security.PublicScanRunID(runUID)
	competing.RunUID = runUID
	competing.TaskName = security.ScanStageTaskNameForRun(
		requested.RepositoryScan, requested.Mode, security.StageThreatModel, "", runUID,
	)
	if s.mutateCompeting != nil {
		s.mutateCompeting(&competing)
	}
	competingInput := *input
	competingInput.RunUID = runUID
	competingInput.ScanRunID = competing.ID
	if err := s.SecurityRunTaskInputStore.CreateScanRunWithTaskInput(ctx, &competing, &competingInput); err != nil {
		return err
	}
	s.injectedRun = &competing
	return fmt.Errorf("%w: injected concurrent request", storepkg.ErrConflict)
}

func TestCreateScanRunRepairsRunCreatedByIdempotencyConflict(t *testing.T) {
	ctx := context.Background()
	baseStore := setupControllerSQLiteStore(t)
	securityStore := &conflictInjectingSecurityStore{SecurityStore: baseStore, SecurityRunTaskInputStore: baseStore}
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "conflict-repair", Namespace: defaultNS, UID: types.UID("conflict-repair-uid"), Generation: 3,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan).Build()
	reconciler := &RepositoryScanReconciler{
		Client: cl, Scheme: scheme, SecurityStore: securityStore, RunTaskInputStore: securityStore,
	}

	if err := reconciler.createScanRun(ctx, scan, "initial", "", ""); err != nil {
		t.Fatalf("createScanRun() error = %v", err)
	}
	if securityStore.injectedRun == nil {
		t.Fatal("expected a competing scan run to be injected")
	}
	run, err := baseStore.GetScanRun(ctx, scan.Namespace, securityStore.injectedRun.ID)
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if run.Phase != scanRunPhasePending {
		t.Fatalf("run.Phase = %q, want pending", run.Phase)
	}
	task := &corev1alpha1.Task{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: scan.Namespace, Name: run.TaskName}, task); err != nil {
		t.Fatalf("Get(repaired task) error = %v", err)
	}
	if task.Labels[labels.LabelSecurityScanID] != run.ID {
		t.Fatalf("task scan ID = %q, want %q", task.Labels[labels.LabelSecurityScanID], run.ID)
	}
	current := &corev1alpha1.RepositoryScan{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		t.Fatalf("Get(scan) error = %v", err)
	}
	if current.Status.LastScanID != run.ID || current.Status.LastScanTaskName != run.TaskName {
		t.Fatalf("scan status = %#v, want repaired run/task", current.Status)
	}
}

func TestCreateScanRunTreatsUnrelatedRepositoryReservationAsContention(t *testing.T) {
	ctx := context.Background()
	baseStore := setupControllerSQLiteStore(t)
	securityStore := &conflictInjectingSecurityStore{
		SecurityStore: baseStore, SecurityRunTaskInputStore: baseStore,
		mutateCompeting: func(run *storepkg.ScanRun) {
			run.RequestIdempotencyKey = "req_unrelated_repository_reservation"
			run.IdempotencyKey = run.RequestIdempotencyKey
		},
	}
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "conflict-unrelated", Namespace: defaultNS, UID: types.UID("conflict-unrelated-uid"), Generation: 2,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan).Build()
	reconciler := &RepositoryScanReconciler{
		Client: cl, Scheme: scheme, SecurityStore: securityStore, RunTaskInputStore: securityStore,
	}

	if err := reconciler.createScanRun(ctx, scan, "initial", "", ""); err != nil {
		t.Fatalf("createScanRun() error = %v, want expected contention", err)
	}
	if securityStore.injectedRun == nil {
		t.Fatal("expected an unrelated competing scan run")
	}
	stored, err := baseStore.GetScanRun(ctx, scan.Namespace, securityStore.injectedRun.ID)
	if err != nil {
		t.Fatalf("GetScanRun(competing) error = %v", err)
	}
	if stored.Phase != scanRunPhasePending || stored.RequestIdempotencyKey != "req_unrelated_repository_reservation" {
		t.Fatalf("competing run = %#v, want untouched pending reservation", stored)
	}
	task := &corev1alpha1.Task{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: scan.Namespace, Name: stored.TaskName}, task); !apierrors.IsNotFound(err) {
		t.Fatalf("Get(competing task) error = %v, want no task created by losing reconciler", err)
	}
	current := &corev1alpha1.RepositoryScan{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		t.Fatalf("Get(scan) error = %v", err)
	}
	if current.Status.LastScanID != "" || current.Status.LastScanTaskName != "" {
		t.Fatalf("scan status = %#v, want unchanged status after contention", current.Status)
	}
}

func TestCreateScanRunRejectsConflictingRunWithDifferentHeadCommit(t *testing.T) {
	ctx := context.Background()
	baseStore := setupControllerSQLiteStore(t)
	requestedHead := strings.Repeat("A", 40)
	conflictingHead := strings.Repeat("b", 40)
	securityStore := &conflictInjectingSecurityStore{
		SecurityStore: baseStore, SecurityRunTaskInputStore: baseStore,
		mutateCompeting: func(run *storepkg.ScanRun) {
			run.HeadCommit = conflictingHead
		},
	}
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "conflict-head", Namespace: defaultNS, UID: types.UID("conflict-head-uid"), Generation: 4,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan).Build()
	reconciler := &RepositoryScanReconciler{
		Client: cl, Scheme: scheme, SecurityStore: securityStore, RunTaskInputStore: securityStore,
	}

	err := reconciler.createScanRun(ctx, scan, "manual", "", requestedHead)
	if err == nil || !strings.Contains(err.Error(), "does not match the requested scan inputs") {
		t.Fatalf("createScanRun() error = %v, want conflicting head-commit rejection", err)
	}
	if securityStore.injectedRun == nil {
		t.Fatal("expected a competing scan run to be injected")
	}
	stored, getErr := baseStore.GetScanRun(ctx, scan.Namespace, securityStore.injectedRun.ID)
	if getErr != nil {
		t.Fatalf("GetScanRun() error = %v", getErr)
	}
	if stored.HeadCommit != conflictingHead || stored.Phase != scanRunPhaseFailed {
		t.Fatalf("conflicting run = %#v, want mismatched head marked failed", stored)
	}
}

type patchIngestFixture struct {
	store            *sqlitestore.Store
	publicationStore *patchPublicationStore
	reconciler       *RepositoryScanReconciler
	scan             *corev1alpha1.RepositoryScan
	finding          *storepkg.Finding
	proposal         *storepkg.PatchProposal
}

type patchPublicationStore struct {
	storepkg.PublicationStore
	publication *storepkg.Publication
	err         error
}

func (s *patchPublicationStore) GetPublication(_ context.Context, id string) (*storepkg.Publication, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.publication == nil || s.publication.ID != id {
		return nil, storepkg.ErrNotFound
	}
	copyValue := *s.publication
	return &copyValue, nil
}

func newPatchIngestFixture(t *testing.T, id string) patchIngestFixture {
	t.Helper()
	return newPatchIngestFixtureWithSourceRun(t, id, "scan_patch")
}

func newPatchIngestFixtureWithSourceRun(t *testing.T, id, sourceScanRunID string) patchIngestFixture {
	t.Helper()
	ctx := context.Background()
	securityStore := setupControllerSQLiteStore(t)

	findingID := "fnd_patch_" + id
	taskName := "kaset-patch-" + id
	branch := "orka/security/" + findingID
	publicationStore := &patchPublicationStore{}
	fixture := patchIngestFixture{
		store:            securityStore,
		publicationStore: publicationStore,
		reconciler: &RepositoryScanReconciler{
			SecurityStore:    securityStore,
			ArtifactStore:    securityStore,
			ResultStore:      securityStore,
			PublicationStore: publicationStore,
		},
		scan: &corev1alpha1.RepositoryScan{
			ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS},
			Spec: corev1alpha1.RepositoryScanSpec{
				RepoURL:      "https://github.com/example/kaset",
				ForkRepo:     "https://github.com/example/kaset",
				Branch:       "main",
				PRBaseBranch: "main",
			},
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
			ID:              "patch_" + id,
			Namespace:       defaultNS,
			RepositoryScan:  "kaset",
			FindingID:       findingID,
			SourceScanRunID: sourceScanRunID,
			TaskName:        taskName,
			Branch:          branch,
			Status:          scanRunPhasePending,
			CreatedAt:       time.Now(),
			UpdatedAt:       time.Now(),
		},
	}
	fixture.finding.PatchProposalID = fixture.proposal.ID
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
	remoteBeforeSHA := ""
	headSHA := strings.Repeat("b", 40)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fixture.proposal.TaskName,
			Namespace: fixture.proposal.Namespace,
			UID:       types.UID("uid-" + fixture.proposal.TaskName),
			Labels: map[string]string{
				labels.LabelSecurityTarget:    fixture.scan.Name,
				labels.LabelSecurityFindingID: fixture.finding.ID,
				labels.LabelSecurityStage:     security.StagePatch,
				labels.LabelSecurityMode:      security.StagePatch,
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: fmt.Sprintf("REQUIRED_SECURITY_ARTIFACTS: %s, %s\n", diffName, summaryName),
			Workspace: &corev1alpha1.WorkspaceConfig{
				Intent:             corev1alpha1.WorkspaceIntentWrite,
				GitRepo:            fixture.scan.Spec.RepoURL,
				PublicationGitRepo: fixture.scan.Spec.ForkRepo,
				PushBranch:         fixture.proposal.Branch,
				ExpectedRemoteSHA:  remoteBeforeSHA,
				PRBaseBranch:       fixture.scan.Spec.PRBaseBranch,
				CreatePR:           true,
			},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseSucceeded,
			ResultRef: &corev1alpha1.ResultReference{Available: resultAvailable},
			Execution: &corev1alpha1.TaskExecutionStatus{Attempt: 1, PromptID: "prompt-" + fixture.proposal.TaskName},
			Delivery: &corev1alpha1.TaskDeliveryStatus{
				State:         corev1alpha1.TaskDeliveryStateVerifiedExact,
				Outcome:       corev1alpha1.TaskDeliveryOutcomeVerifiedExact,
				PublicationID: "publication-" + fixture.proposal.TaskName,
				PublicationRepository: &corev1alpha1.RepositoryIdentity{
					Provider: "github",
					ID:       "github.com/example/kaset",
				},
				Branch:            fixture.proposal.Branch,
				RemoteBeforeSHA:   &remoteBeforeSHA,
				ExpectedCommitSHA: headSHA,
				VerifiedRemoteSHA: headSHA,
				ArtifactDigest:    "sha256:" + strings.Repeat("a", 64),
				PRReceipt: &corev1alpha1.TaskPullRequestReceipt{
					ID:         "github:101:42",
					Number:     42,
					URL:        fixture.scan.Spec.RepoURL + "/pull/42",
					State:      "Open",
					BaseBranch: fixture.scan.Spec.PRBaseBranch,
					HeadBranch: fixture.proposal.Branch,
					HeadSHA:    headSHA,
				},
			},
		},
	}
	fixture.publicationStore.publication = patchPublicationForTask(fixture, task)
	return task
}

func patchPublicationForTask(fixture patchIngestFixture, task *corev1alpha1.Task) *storepkg.Publication {
	sourceSHA := strings.Repeat("1", 40)
	headSHA := strings.Repeat("b", 40)
	repositoryID := "github.com/example/kaset"
	targetRef := "refs/heads/" + fixture.proposal.Branch
	baseRef := "refs/heads/" + fixture.scan.Spec.PRBaseBranch
	now := time.Now().UTC()
	return &storepkg.Publication{
		ID:                  publicationIDForTask(task),
		Namespace:           task.Namespace,
		Generation:          1,
		TaskUID:             string(task.UID),
		Attempt:             int64(task.Status.Execution.Attempt),
		PromptID:            task.Status.Execution.PromptID,
		SourceRepositoryID:  repositoryID,
		SourceRef:           sourceSHA,
		SourceBaselineSHA:   sourceSHA,
		TargetRepositoryID:  repositoryID,
		TargetRef:           targetRef,
		Baseline:            storepkg.RemoteRefState{Absent: true},
		ArtifactDigest:      "sha256:" + strings.Repeat("a", 64),
		State:               storepkg.PublicationVerifiedExact,
		PreparedReceipt:     &storepkg.PreparedPublicationReceipt{CommitSHA: headSHA},
		PublishReceipt:      &storepkg.PublishOperationReceipt{TargetRepositoryID: repositoryID, TargetRef: targetRef, RemoteBefore: storepkg.RemoteRefState{Absent: true}, ExpectedCommitSHA: headSHA},
		VerificationReceipt: &storepkg.PublicationVerificationReceipt{Outcome: storepkg.PublicationVerifiedExact, ExpectedCommitSHA: headSHA, ObservedRemote: storepkg.RemoteRefState{SHA: headSHA}},
		PRIntent:            &storepkg.PullRequestIntent{BaseRepositoryID: repositoryID, BaseRef: baseRef, HeadRepositoryID: repositoryID, HeadRef: targetRef, PublicationGeneration: 1, ExpectedHeadSHA: headSHA},
		PullRequestReceipt:  &storepkg.PullRequestOperationReceipt{IntentKey: "sha256:" + strings.Repeat("c", 64), ForgeID: "github:101:42", URL: fixture.scan.Spec.RepoURL + "/pull/42", State: "Open", HeadSHA: headSHA, ReconciledAt: now},
		CreatedAt:           now,
		UpdatedAt:           now,
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

func TestIngestPatchTaskRejectsStaleSourceRunWithoutOccurrenceBindings(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixtureWithSourceRun(t, "stale-source-run", "scan_patch_old")
	diff := testPatchDiffHeader
	savePatchStructuredResult(t, fixture, &common.StructuredResult{
		Summary: "patched successfully", Diff: diff, Files: []string{"app.py"}, PushBranch: fixture.proposal.Branch,
	})
	savePatchArtifacts(t, fixture, diff, []string{"app.py"})

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, patchTaskForFixture(fixture, true)); err != nil {
		t.Fatalf("ingestPatchTask() error = %v", err)
	}
	assertPatchIngestState(t, fixture, "stale", findingStateOpen)
}

func TestIngestPatchTaskMarksPROpenAfterExactPublicationReceipt(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "ready")
	diff := testPatchDiffHeader
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
	assertPatchIngestState(t, fixture, patchProposalStatusPROpened, findingStatePROpen)
	proposals, err := fixture.store.ListPatchProposals(ctx, fixture.proposal.Namespace, fixture.finding.ID)
	if err != nil {
		t.Fatalf("ListPatchProposals() error = %v", err)
	}
	if len(proposals) != 1 || proposals[0].PRNumber == nil || *proposals[0].PRNumber != 42 || proposals[0].PRURL != fixture.scan.Spec.RepoURL+"/pull/42" {
		t.Fatalf("proposal publication receipt = %#v, want PR #42", proposals)
	}
	updatedFinding, err := fixture.store.GetFinding(ctx, fixture.proposal.Namespace, fixture.finding.ID)
	if err != nil {
		t.Fatalf("GetFinding() error = %v", err)
	}
	if updatedFinding.PRNumber == nil || *updatedFinding.PRNumber != 42 || updatedFinding.PRURL != fixture.scan.Spec.RepoURL+"/pull/42" {
		t.Fatalf("finding publication receipt = %#v, want PR #42", updatedFinding)
	}
}

func TestIngestPatchTaskAcceptsDiffArtifactWithDifferentIndexFormatting(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "diff-index-format")
	actualDiff := strings.Join([]string{
		testPatchDiffHeader,
		"index 1111111111111111111111111111111111111111..2222222222222222222222222222222222222222 100644",
		"--- a/app.py",
		"+++ b/app.py",
		"@@ -1 +1 @@",
		"-unsafe()",
		"+safe()",
		"",
	}, "\n")
	artifactDiff := strings.Join([]string{
		testPatchDiffHeader,
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
	assertPatchIngestState(t, fixture, patchProposalStatusPROpened, findingStatePROpen)
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
	assertPatchIngestState(t, fixture, patchProposalStatusPROpened, findingStatePROpen)
}

func TestIngestPatchTaskUsesDurablePublicationWhenTaskDeliveryReceiptIsMissing(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "task-receipt-missing")
	diff := testPatchDiffHeader
	savePatchArtifacts(t, fixture, diff, []string{"app.py"})
	task := patchTaskForFixture(fixture, true)
	task.Status.Delivery = nil

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, task); err != nil {
		t.Fatalf("ingestPatchTask() error = %v", err)
	}
	assertPatchIngestState(t, fixture, patchProposalStatusPROpened, findingStatePROpen)
}

func TestIngestPatchTaskRejectsMissingDurablePullRequestReceipt(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "durable-pr-receipt-missing")
	diff := testPatchDiffHeader
	savePatchArtifacts(t, fixture, diff, []string{"app.py"})
	task := patchTaskForFixture(fixture, true)
	fixture.publicationStore.publication.PullRequestReceipt = nil

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, task); err != nil {
		t.Fatalf("ingestPatchTask() error = %v", err)
	}
	assertPatchIngestState(t, fixture, scanRunPhaseFailed, findingStateOpen)
}

func TestVerifiedSecurityPatchPublicationRejectsDurableRecordDrift(t *testing.T) {
	const mismatchedBranchRef = "refs/heads/other"

	tests := []struct {
		name              string
		mutateTask        func(*corev1alpha1.Task)
		mutatePublication func(*storepkg.Publication)
	}{
		{name: "create PR not requested", mutateTask: func(task *corev1alpha1.Task) { task.Spec.Workspace.CreatePR = false }},
		{name: "task UID mismatch", mutatePublication: func(publication *storepkg.Publication) { publication.TaskUID = "mismatched-task-uid" }},
		{name: "publication not verified", mutatePublication: func(publication *storepkg.Publication) { publication.State = storepkg.PublicationVerifying }},
		{name: "source repository mismatch", mutatePublication: func(publication *storepkg.Publication) { publication.SourceRepositoryID = "github.com/example/other" }},
		{name: "source baseline mismatch", mutatePublication: func(publication *storepkg.Publication) { publication.SourceRef = strings.Repeat("2", 40) }},
		{name: "target repository mismatch", mutatePublication: func(publication *storepkg.Publication) { publication.TargetRepositoryID = "github.com/example/other" }},
		{name: "target ref mismatch", mutatePublication: func(publication *storepkg.Publication) { publication.TargetRef = mismatchedBranchRef }},
		{name: "target baseline mismatch", mutatePublication: func(publication *storepkg.Publication) {
			publication.Baseline = storepkg.RemoteRefState{SHA: strings.Repeat("2", 40)}
		}},
		{name: "prepared receipt missing", mutatePublication: func(publication *storepkg.Publication) { publication.PreparedReceipt = nil }},
		{name: "publish target mismatch", mutatePublication: func(publication *storepkg.Publication) { publication.PublishReceipt.TargetRef = mismatchedBranchRef }},
		{name: "verification head mismatch", mutatePublication: func(publication *storepkg.Publication) {
			publication.VerificationReceipt.ObservedRemote.SHA = strings.Repeat("c", 40)
		}},
		{name: "PR intent base mismatch", mutatePublication: func(publication *storepkg.Publication) { publication.PRIntent.BaseRef = "refs/heads/release" }},
		{name: "PR intent head mismatch", mutatePublication: func(publication *storepkg.Publication) { publication.PRIntent.HeadRef = mismatchedBranchRef }},
		{name: "PR receipt missing", mutatePublication: func(publication *storepkg.Publication) { publication.PullRequestReceipt = nil }},
		{name: "PR forge ID invalid", mutatePublication: func(publication *storepkg.Publication) {
			publication.PullRequestReceipt.ForgeID = "github:101:not-a-number"
		}},
		{name: "PR URL mismatch", mutatePublication: func(publication *storepkg.Publication) {
			publication.PullRequestReceipt.URL = "https://github.com/example/other/pull/42"
		}},
		{name: "PR not open", mutatePublication: func(publication *storepkg.Publication) { publication.PullRequestReceipt.State = "Closed" }},
		{name: "PR head mismatch", mutatePublication: func(publication *storepkg.Publication) {
			publication.PullRequestReceipt.HeadSHA = strings.Repeat("c", 40)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newPatchIngestFixture(t, strings.ReplaceAll(tt.name, " ", "-"))
			task := patchTaskForFixture(fixture, true)
			if tt.mutateTask != nil {
				tt.mutateTask(task)
			}
			if tt.mutatePublication != nil {
				tt.mutatePublication(fixture.publicationStore.publication)
			}
			if receipt, reason, err := fixture.reconciler.verifiedSecurityPatchPublication(context.Background(), fixture.scan, task, fixture.proposal.Branch); err != nil {
				t.Fatalf("verifiedSecurityPatchPublication() error = %v", err)
			} else if reason == "" {
				t.Fatalf("verifiedSecurityPatchPublication() receipt = %#v, want rejection", receipt)
			}
		})
	}
}

func TestIngestPatchTaskRejectsMissingDiffArtifact(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "missing-diff")
	savePatchStructuredResult(t, fixture, &common.StructuredResult{
		Summary:    "patched successfully",
		Diff:       testPatchDiffHeader,
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
		Diff:       testPatchDiffHeader,
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

func TestIngestPatchTaskIgnoresLegacyStructuredResultDiff(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "stale-diff")
	actualDiff := strings.Join([]string{
		testPatchDiffHeader,
		"--- a/app.py",
		"+++ b/app.py",
		"@@ -1 +1 @@",
		"-unsafe()",
		"+safe()",
		"",
	}, "\n")
	staleDiff := strings.Join([]string{
		testPatchDiffHeader,
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
	assertPatchIngestState(t, fixture, patchProposalStatusPROpened, findingStatePROpen)
}

func TestIngestPatchTaskRejectsConfirmedPushWithoutArtifactContract(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "no-artifacts")
	savePatchStructuredResult(t, fixture, &common.StructuredResult{
		Summary:    "patched successfully",
		Diff:       testPatchDiffHeader,
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
	diff := testPatchDiffHeader
	savePatchStructuredResult(t, fixture, &common.StructuredResult{
		Summary:    "patched successfully",
		Diff:       diff,
		Files:      []string{"app.py", "extra.py"},
		PushBranch: fixture.proposal.Branch,
	})
	savePatchArtifacts(t, fixture, diff, []string{"extra.py"})

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, patchTaskForFixture(fixture, true)); err != nil {
		t.Fatalf("ingestPatchTask() error = %v", err)
	}
	assertPatchIngestState(t, fixture, scanRunPhaseFailed, findingStateOpen)
}

func TestIngestPatchTaskIgnoresLegacyStructuredResultPushError(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "failed")
	savePatchStructuredResult(t, fixture, &common.StructuredResult{
		Summary:   "patch created but push failed",
		Diff:      testPatchDiffHeader,
		PushError: "git push failed: remote rejected",
	})
	savePatchArtifacts(t, fixture, testPatchDiffHeader, []string{"app.py"})

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, patchTaskForFixture(fixture, true)); err != nil {
		t.Fatalf("ingestPatchTask() error = %v", err)
	}
	assertPatchIngestState(t, fixture, patchProposalStatusPROpened, findingStatePROpen)
}

func TestIngestPatchTaskIgnoresLegacyStructuredResultWithoutPushBranch(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "missing-push")
	savePatchStructuredResult(t, fixture, &common.StructuredResult{
		Summary: "patch created without confirmed push",
		Diff:    testPatchDiffHeader,
	})
	savePatchArtifacts(t, fixture, testPatchDiffHeader, []string{"app.py"})

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, patchTaskForFixture(fixture, true)); err != nil {
		t.Fatalf("ingestPatchTask() error = %v", err)
	}
	assertPatchIngestState(t, fixture, patchProposalStatusPROpened, findingStatePROpen)
}

func TestIngestPatchTaskDoesNotRequireLegacyResultReference(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "pending-ref")
	savePatchArtifacts(t, fixture, testPatchDiffHeader, []string{"app.py"})

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, patchTaskForFixture(fixture, false)); err != nil {
		t.Fatalf("ingestPatchTask() error = %v", err)
	}
	assertPatchIngestState(t, fixture, patchProposalStatusPROpened, findingStatePROpen)
}

func TestIngestPatchTaskDoesNotRequireLegacyResultRecord(t *testing.T) {
	ctx := context.Background()
	fixture := newPatchIngestFixture(t, "pending-result")
	savePatchArtifacts(t, fixture, testPatchDiffHeader, []string{"app.py"})

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, patchTaskForFixture(fixture, true)); err != nil {
		t.Fatalf("ingestPatchTask() error = %v", err)
	}
	assertPatchIngestState(t, fixture, patchProposalStatusPROpened, findingStatePROpen)
}

func TestTerminalScanStatusRechecksLatestRunInsideStatusUpdate(t *testing.T) {
	tests := []struct {
		name   string
		update func(context.Context, *RepositoryScanReconciler, *corev1alpha1.RepositoryScan, *storepkg.ScanRun) error
	}{
		{
			name: "normal terminal update",
			update: func(ctx context.Context, reconciler *RepositoryScanReconciler, scan *corev1alpha1.RepositoryScan, run *storepkg.ScanRun) error {
				return reconciler.refreshScanRunStatus(ctx, scan, run, run.ID, true)
			},
		},
		{
			name: "no-op terminal update",
			update: func(ctx context.Context, reconciler *RepositoryScanReconciler, scan *corev1alpha1.RepositoryScan, run *storepkg.ScanRun) error {
				return reconciler.updateNoopScanStatus(ctx, scan, run)
			},
		},
		{
			name: "terminal failure update",
			update: func(ctx context.Context, reconciler *RepositoryScanReconciler, scan *corev1alpha1.RepositoryScan, run *storepkg.ScanRun) error {
				return reconciler.markScanRunTerminalError(ctx, scan, run, fmt.Errorf("older run failed"))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			securityStore := setupControllerSQLiteStore(t)
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatalf("AddToScheme() error = %v", err)
			}

			oldStarted := mustParseTime(t, "2026-08-02T08:00:00Z")
			oldCompleted := mustParseTime(t, "2026-08-02T08:05:00Z")
			newStarted := mustParseTime(t, "2026-08-02T08:06:00Z")
			scan := &corev1alpha1.RepositoryScan{
				TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
				ObjectMeta: metav1.ObjectMeta{
					Name: "status-race", Namespace: defaultNS, UID: types.UID("status-race-uid"), Generation: 4,
				},
				Spec: corev1alpha1.RepositoryScanSpec{
					RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
				},
				Status: corev1alpha1.RepositoryScanStatus{
					Phase: repositoryScanPhaseReady, LastScanID: "scan_older", LastScanTaskName: "task-older",
				},
			}
			oldRun := &storepkg.ScanRun{
				ID: "scan_older", RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Namespace: defaultNS, RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
				RepositoryScanGeneration: scan.Generation, TaskName: "task-older", Mode: "initial",
				Phase: scanRunPhaseSucceeded, StartedAt: oldStarted, CompletedAt: &oldCompleted, HeadCommit: "old-head",
			}
			newRun := &storepkg.ScanRun{
				ID: "scan_newer", RunUID: "run_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				Namespace: defaultNS, RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
				RepositoryScanGeneration: scan.Generation, TaskName: "task-newer", Mode: "manual",
				Phase: scanRunPhaseRunning, StartedAt: newStarted, HeadCommit: "new-head",
			}
			if err := securityStore.CreateScanRun(ctx, oldRun); err != nil {
				t.Fatalf("CreateScanRun(old) error = %v", err)
			}

			insertedNewRun := false
			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
				WithObjects(scan).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*corev1alpha1.RepositoryScan); ok && !insertedNewRun {
							insertedNewRun = true
							if err := securityStore.CreateScanRun(ctx, newRun); err != nil {
								return err
							}
							newerStatus := &corev1alpha1.RepositoryScan{}
							if err := c.Get(ctx, key, newerStatus, opts...); err != nil {
								return err
							}
							newerStatus.Status.Phase = repositoryScanPhaseScanning
							newerStatus.Status.LastScanID = newRun.ID
							newerStatus.Status.LastScanTaskName = newRun.TaskName
							if err := c.Status().Update(ctx, newerStatus); err != nil {
								return err
							}
						}
						return c.Get(ctx, key, obj, opts...)
					},
				}).
				Build()
			reconciler := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: securityStore}

			if err := tt.update(ctx, reconciler, scan, oldRun); err != nil {
				t.Fatalf("terminal status update error = %v", err)
			}
			if !insertedNewRun {
				t.Fatal("newer run was not inserted inside the status retry boundary")
			}

			current := &corev1alpha1.RepositoryScan{}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
				t.Fatalf("Get(scan) error = %v", err)
			}
			if current.Status.Phase != repositoryScanPhaseScanning || current.Status.LastScanID != newRun.ID ||
				current.Status.LastScanTaskName != newRun.TaskName {
				t.Fatalf("status = %#v, want newer Scanning state for %s", current.Status, newRun.ID)
			}
		})
	}
}

func TestTerminalScanStatusReloadsSameRunInsideStatusUpdate(t *testing.T) {
	tests := []struct {
		name   string
		update func(context.Context, *RepositoryScanReconciler, *corev1alpha1.RepositoryScan, *storepkg.ScanRun) error
	}{
		{
			name: "normal terminal update",
			update: func(ctx context.Context, reconciler *RepositoryScanReconciler, scan *corev1alpha1.RepositoryScan, run *storepkg.ScanRun) error {
				return reconciler.refreshScanRunStatus(ctx, scan, run, run.ID, true)
			},
		},
		{
			name: "no-op terminal update",
			update: func(ctx context.Context, reconciler *RepositoryScanReconciler, scan *corev1alpha1.RepositoryScan, run *storepkg.ScanRun) error {
				return reconciler.updateNoopScanStatus(ctx, scan, run)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			securityStore := setupControllerSQLiteStore(t)
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatalf("AddToScheme() error = %v", err)
			}

			started := mustParseTime(t, "2026-08-02T09:00:00Z")
			completed := mustParseTime(t, "2026-08-02T09:05:00Z")
			scan := &corev1alpha1.RepositoryScan{
				TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
				ObjectMeta: metav1.ObjectMeta{
					Name: "same-run-race", Namespace: defaultNS, UID: types.UID("same-run-race-uid"), Generation: 2,
				},
				Spec: corev1alpha1.RepositoryScanSpec{
					RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
				},
				Status: corev1alpha1.RepositoryScanStatus{
					Phase: repositoryScanPhaseReady, LastScanID: "scan_same", LastScanTaskName: "task-same",
				},
			}
			run := &storepkg.ScanRun{
				ID: "scan_same", RunUID: "run_cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
				Namespace: defaultNS, RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
				RepositoryScanGeneration: scan.Generation, TaskName: "task-same", Mode: "manual",
				Phase: scanRunPhaseSucceeded, StartedAt: started, CompletedAt: &completed, HeadCommit: "same-head",
			}
			if err := securityStore.CreateScanRun(ctx, run); err != nil {
				t.Fatalf("CreateScanRun() error = %v", err)
			}

			madeRunning := false
			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
				WithObjects(scan).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*corev1alpha1.RepositoryScan); ok && !madeRunning {
							madeRunning = true
							currentRun, err := securityStore.GetScanRun(ctx, run.Namespace, run.ID)
							if err != nil {
								return err
							}
							currentRun.Phase = scanRunPhaseRunning
							currentRun.CompletedAt = nil
							currentRun.Summary = "pending review work discovered"
							if err := securityStore.UpdateScanRun(ctx, currentRun); err != nil {
								return err
							}
							currentScan := &corev1alpha1.RepositoryScan{}
							if err := c.Get(ctx, key, currentScan, opts...); err != nil {
								return err
							}
							currentScan.Status.Phase = repositoryScanPhaseScanning
							if err := c.Status().Update(ctx, currentScan); err != nil {
								return err
							}
						}
						return c.Get(ctx, key, obj, opts...)
					},
				}).
				Build()
			reconciler := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: securityStore}

			if err := tt.update(ctx, reconciler, scan, run); err != nil {
				t.Fatalf("terminal status update error = %v", err)
			}
			current := &corev1alpha1.RepositoryScan{}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
				t.Fatalf("Get(scan) error = %v", err)
			}
			if current.Status.Phase != repositoryScanPhaseScanning || current.Status.LastScanID != run.ID {
				t.Fatalf("status = %#v, want same-run Scanning state", current.Status)
			}
		})
	}
}

func TestMarkScanRunTerminalErrorPreservesSealedRun(t *testing.T) {
	ctx := context.Background()
	securityStore := setupControllerSQLiteStore(t)
	completed := mustParseTime(t, "2026-08-02T09:30:00Z")
	stale := &storepkg.ScanRun{
		ID: "scan_sealed", Namespace: defaultNS, RepositoryScan: "sealed", TaskName: "task-sealed",
		Mode: "manual", Phase: scanRunPhaseSucceeded, StartedAt: completed.Add(-time.Minute), CompletedAt: &completed,
		Quality: storepkg.ScanQuality{BundleStatus: storepkg.BundleStatusDraft},
	}
	if err := securityStore.CreateScanRun(ctx, stale); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	sealed, err := securityStore.GetScanRun(ctx, defaultNS, stale.ID)
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	sealed.Quality.BundleStatus = storepkg.BundleStatusSealing
	if err := securityStore.UpdateScanRun(ctx, sealed); err != nil {
		t.Fatalf("UpdateScanRun(sealing) error = %v", err)
	}
	sealed.Quality.BundleStatus = storepkg.BundleStatusSealed
	if err := securityStore.UpdateScanRun(ctx, sealed); err != nil {
		t.Fatalf("UpdateScanRun(sealed) error = %v", err)
	}
	reconciler := &RepositoryScanReconciler{SecurityStore: securityStore}

	if err := reconciler.markScanRunTerminalError(ctx, nil, stale, fmt.Errorf("stale failure")); err != nil {
		t.Fatalf("markScanRunTerminalError() error = %v", err)
	}
	stored, err := securityStore.GetScanRun(ctx, defaultNS, stale.ID)
	if err != nil {
		t.Fatalf("GetScanRun(stored) error = %v", err)
	}
	if stored.Phase != scanRunPhaseSucceeded || stored.Quality.BundleStatus != storepkg.BundleStatusSealed {
		t.Fatalf("stored run = %#v, want sealed succeeded run preserved", stored)
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

func TestRefreshScanRunStatusAdvancesLegacyUnboundControlledRun(t *testing.T) {
	ctx := context.Background()
	securityStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "legacy-unbound", Namespace: defaultNS, UID: types.UID("legacy-unbound-uid"), Generation: 3,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
		Status: corev1alpha1.RepositoryScanStatus{Phase: repositoryScanPhaseScanning},
	}
	completed := metav1.NewTime(mustParseTime(t, "2026-08-02T12:00:00Z"))
	controller := true
	owner := metav1.OwnerReference{
		APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan", Name: scan.Name,
		UID: scan.UID, Controller: &controller,
	}
	newTask := func(name, stage string) *corev1alpha1.Task {
		return &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: defaultNS, OwnerReferences: []metav1.OwnerReference{owner},
				Labels: map[string]string{
					labels.LabelSecurityTarget: labels.SelectorValue(scan.Name),
					labels.LabelSecurityScanID: "scan_legacy_unbound",
					labels.LabelSecurityStage:  stage,
				},
			},
			Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded, CompletionTime: &completed},
		}
	}
	threatTask := newTask("legacy-threat", security.StageThreatModel)
	reviewTask := newTask("legacy-review", security.StageReview)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(scan, threatTask, reviewTask).Build()
	r := &RepositoryScanReconciler{
		Client: cl, Scheme: scheme, SecurityStore: securityStore,
		IntegrityConfig: security.IntegrityConfig{QualityStateWritesEnabled: true},
	}
	run := &storepkg.ScanRun{
		ID: "scan_legacy_unbound", Namespace: defaultNS, RepositoryScan: scan.Name,
		TaskName: threatTask.Name, Mode: "initial", Phase: scanRunPhaseRunning,
		HeadCommit: "0123456789012345678901234567890123456789", StartedAt: completed.Add(-time.Minute),
		Quality: storepkg.LegacyScanQuality(),
	}
	if err := securityStore.CreateScanRun(ctx, run); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}

	if err := r.refreshScanRunStatus(ctx, scan, run, run.ID, true); err != nil {
		t.Fatalf("refreshScanRunStatus() error = %v", err)
	}

	current := &corev1alpha1.RepositoryScan{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		t.Fatalf("Get(scan) error = %v", err)
	}
	if current.Status.Phase != repositoryScanPhaseReady || current.Status.LastScanID != run.ID ||
		current.Status.LastProcessedCommit != run.HeadCommit {
		t.Fatalf("status = %#v, want completed legacy discovery projection", current.Status)
	}
	if current.Status.Quality != nil {
		t.Fatalf("Status.Quality = %#v, want nil for unbound legacy run", current.Status.Quality)
	}
	qualityReady := meta.FindStatusCondition(current.Status.Conditions, "QualityReady")
	if qualityReady == nil || qualityReady.Status != metav1.ConditionUnknown || qualityReady.Reason != "QualityUnavailable" {
		t.Fatalf("QualityReady = %#v, want Unknown/QualityUnavailable", qualityReady)
	}
}

func TestUpdateStatusWithRetryClearsQualityWhenWritesDisabled(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "quality-disabled", Namespace: defaultNS},
		Status: corev1alpha1.RepositoryScanStatus{
			Phase:   repositoryScanPhaseReady,
			Quality: &corev1alpha1.RepositoryScanQualityStatus{SchemaVersion: 1},
			Conditions: []metav1.Condition{{
				Type: "QualityReady", Status: metav1.ConditionTrue, Reason: "QualityComplete",
			}},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan).Build()
	r := &RepositoryScanReconciler{Client: cl, Scheme: scheme}

	if err := r.updateStatusWithRetry(ctx, scan, func(current *corev1alpha1.RepositoryScan) {
		current.Status.Phase = repositoryScanPhasePending
	}); err != nil {
		t.Fatalf("updateStatusWithRetry() error = %v", err)
	}

	current := &corev1alpha1.RepositoryScan{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		t.Fatalf("Get(scan) error = %v", err)
	}
	if current.Status.Quality != nil {
		t.Fatalf("Status.Quality = %#v, want nil", current.Status.Quality)
	}
	if condition := meta.FindStatusCondition(current.Status.Conditions, "QualityReady"); condition != nil {
		t.Fatalf("QualityReady = %#v, want removed", condition)
	}
}

func TestUpdateStatusWithRetryCheckedClearsQualityOnNoopWhenWritesDisabled(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "quality-disabled-noop-mutation", Namespace: defaultNS},
		Status: corev1alpha1.RepositoryScanStatus{
			Phase:   repositoryScanPhaseReady,
			Quality: &corev1alpha1.RepositoryScanQualityStatus{SchemaVersion: 1},
			Conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready"},
				{Type: "QualityReady", Status: metav1.ConditionTrue, Reason: "QualityComplete"},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan).Build()
	r := &RepositoryScanReconciler{Client: cl, Scheme: scheme}

	if err := r.updateStatusWithRetryChecked(ctx, scan, func(current *corev1alpha1.RepositoryScan) (bool, error) {
		current.Status.Phase = repositoryScanPhasePending
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: "ShouldNotPersist",
		})
		return false, nil
	}); err != nil {
		t.Fatalf("updateStatusWithRetryChecked() error = %v", err)
	}

	current := &corev1alpha1.RepositoryScan{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		t.Fatalf("Get(scan) error = %v", err)
	}
	if current.Status.Quality != nil {
		t.Fatalf("Status.Quality = %#v, want nil", current.Status.Quality)
	}
	if condition := meta.FindStatusCondition(current.Status.Conditions, "QualityReady"); condition != nil {
		t.Fatalf("QualityReady = %#v, want removed", condition)
	}
	if current.Status.Phase != repositoryScanPhaseReady {
		t.Fatalf("Status.Phase = %q, want unchanged ready", current.Status.Phase)
	}
	ready := meta.FindStatusCondition(current.Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.Reason != "Ready" {
		t.Fatalf("Ready = %#v, want original condition unchanged", ready)
	}
}

func TestCreateScanRunClearsStaleQualityWhenWritesDisabled(t *testing.T) {
	ctx := context.Background()
	securityStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "quality-disabled-start", Namespace: defaultNS, UID: types.UID("quality-disabled-start-uid"), Generation: 2,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
		Status: staleRepositoryScanQualityStatus(),
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan).Build()
	r := &RepositoryScanReconciler{
		Client: cl, Scheme: scheme, SecurityStore: securityStore, RunTaskInputStore: securityStore,
	}

	if err := r.createScanRun(ctx, scan, "initial", "", ""); err != nil {
		t.Fatalf("createScanRun() error = %v", err)
	}
	assertRepositoryScanQualityCleared(t, ctx, cl, scan)
}

func TestRefreshScanRunStatusClearsStaleQualityWhenWritesDisabled(t *testing.T) {
	ctx := context.Background()
	securityStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "quality-disabled-refresh", Namespace: defaultNS, UID: types.UID("quality-disabled-refresh-uid"), Generation: 3,
		},
		Status: staleRepositoryScanQualityStatus(),
	}
	runUID := "run_7777777777777777777777777777777777777777777777777777777777777777"
	run := &storepkg.ScanRun{
		ID: security.PublicScanRunID(runUID), RunUID: runUID, Namespace: scan.Namespace, RepositoryScan: scan.Name,
		RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: scan.Generation,
		TaskName: "quality-disabled-refresh-task", Mode: "manual", Phase: scanRunPhasePending,
		Quality: initialScanQuality(scan, false), StartedAt: time.Now().UTC(),
	}
	if err := securityStore.CreateScanRun(ctx, run); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: run.TaskName, Namespace: scan.Namespace,
			Labels: map[string]string{
				labels.LabelSecurityTarget: labels.SelectorValue(scan.Name),
				labels.LabelSecurityScanID: run.ID,
				labels.LabelSecurityStage:  security.StageThreatModel,
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan, task).Build()
	r := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: securityStore}

	if err := r.refreshScanRunStatus(ctx, scan, run, run.ID, true); err != nil {
		t.Fatalf("refreshScanRunStatus() error = %v", err)
	}
	assertRepositoryScanQualityCleared(t, ctx, cl, scan)
}

func TestUpdateNoopScanStatusClearsStaleQualityWhenWritesDisabled(t *testing.T) {
	ctx := context.Background()
	securityStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "quality-disabled-noop", Namespace: defaultNS, UID: types.UID("quality-disabled-noop-uid"), Generation: 4,
		},
		Status: staleRepositoryScanQualityStatus(),
	}
	runUID := "run_8888888888888888888888888888888888888888888888888888888888888888"
	completedAt := time.Now().UTC()
	run := &storepkg.ScanRun{
		ID: security.PublicScanRunID(runUID), RunUID: runUID, Namespace: scan.Namespace, RepositoryScan: scan.Name,
		RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: scan.Generation,
		TaskName: "quality-disabled-noop-task", Mode: "incremental", Phase: scanRunPhaseSucceeded,
		Quality: initialScanQuality(scan, false), StartedAt: completedAt.Add(-time.Minute), CompletedAt: &completedAt,
	}
	if err := securityStore.CreateScanRun(ctx, run); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan).Build()
	r := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: securityStore}

	if err := r.updateNoopScanStatus(ctx, scan, run); err != nil {
		t.Fatalf("updateNoopScanStatus() error = %v", err)
	}
	assertRepositoryScanQualityCleared(t, ctx, cl, scan)
}

func staleRepositoryScanQualityStatus() corev1alpha1.RepositoryScanStatus {
	return corev1alpha1.RepositoryScanStatus{
		Phase:   repositoryScanPhaseReady,
		Quality: &corev1alpha1.RepositoryScanQualityStatus{SchemaVersion: 1},
		Conditions: []metav1.Condition{{
			Type: "QualityReady", Status: metav1.ConditionTrue, Reason: "QualityComplete",
		}},
	}
}

func assertRepositoryScanQualityCleared(
	t *testing.T,
	ctx context.Context,
	cl client.Client,
	scan *corev1alpha1.RepositoryScan,
) {
	t.Helper()
	current := &corev1alpha1.RepositoryScan{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		t.Fatalf("Get(scan) error = %v", err)
	}
	if current.Status.Quality != nil {
		t.Fatalf("Status.Quality = %#v, want nil", current.Status.Quality)
	}
	if condition := meta.FindStatusCondition(current.Status.Conditions, "QualityReady"); condition != nil {
		t.Fatalf("QualityReady = %#v, want removed", condition)
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
	if !reconciler.shouldAutoValidateFinding(scan, finding, 0) {
		t.Fatal("shouldAutoValidateFinding() = false, want true because full mode ignores sampling thresholds")
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

func TestScheduleFinalizedValidationDoesNotReusePreviousOccurrenceTask(t *testing.T) {
	ctx := context.Background()
	securityStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "validation-occurrence-scope", Namespace: defaultNS,
			UID: types.UID("validation-occurrence-scope-uid"), Generation: 1,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	run := &storepkg.ScanRun{
		ID: "scan-current-validation", RunUID: "run_6666666666666666666666666666666666666666666666666666666666666666",
		Namespace: scan.Namespace, RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
		RepositoryScanGeneration: scan.Generation, TaskName: "source-task", Mode: "manual", Phase: scanRunPhaseSucceeded,
		HeadCommit: strings.Repeat("d", 40), StartedAt: time.Now().UTC(), Quality: storepkg.LegacyScanQuality(),
	}
	if err := securityStore.CreateScanRun(ctx, run); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	finding := &storepkg.Finding{
		ID: "fnd_" + strings.Repeat("a", 64), Namespace: scan.Namespace, RepositoryScan: scan.Name,
		ScanRunID: run.ID, CurrentOccurrenceID: "occ_" + strings.Repeat("b", 64),
		Title: "Current occurrence", Severity: "high", Confidence: "high",
	}
	previousOccurrenceID := "occ_" + strings.Repeat("c", 64)
	previous := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "previous-occurrence-validation", Namespace: scan.Namespace,
			Labels: map[string]string{
				labels.LabelSecurityTarget:       labels.SelectorValue(scan.Name),
				labels.LabelSecurityFindingID:    labels.SelectorValue(finding.ID),
				labels.LabelSecurityScanID:       "scan-previous-validation",
				labels.LabelSecurityOccurrenceID: labels.SelectorValue(previousOccurrenceID),
				labels.LabelSecurityStage:        security.StageValidation,
			},
		},
		Spec: corev1alpha1.TaskSpec{Env: []corev1.EnvVar{
			{Name: security.EnvScanID, Value: "scan-previous-validation"},
			{Name: security.EnvFindingID, Value: finding.ID},
			{Name: security.EnvOccurrenceID, Value: previousOccurrenceID},
		}},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	if err := controllerutil.SetControllerReference(scan, previous, scheme); err != nil {
		t.Fatalf("SetControllerReference() error = %v", err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(scan, previous).Build()
	reconciler := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: securityStore}

	if err := reconciler.scheduleFinalizedValidation(ctx, scan, run, []*storepkg.Finding{finding}); err != nil {
		t.Fatalf("scheduleFinalizedValidation() error = %v", err)
	}
	currentTaskName := security.ScanStageTaskNameForRun(
		scan.Name, "validation", security.StageValidation, finding.CurrentOccurrenceID, run.RunUID,
	)
	current := &corev1alpha1.Task{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: scan.Namespace, Name: currentTaskName}, current); err != nil {
		t.Fatalf("Get(current occurrence validation task) error = %v", err)
	}
	if got, err := taskSecurityOccurrenceID(current); err != nil || got != finding.CurrentOccurrenceID {
		t.Fatalf("taskSecurityOccurrenceID() = (%q, %v), want current occurrence", got, err)
	}
}

func TestCreateValidationTaskUsesLabelSafeExactSecurityIDs(t *testing.T) {
	ctx := context.Background()
	securityStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "validation-labels", Namespace: defaultNS, UID: types.UID("validation-labels-uid"), Generation: 1,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(scan).Build()
	run := &storepkg.ScanRun{
		ID: "scan-validation-labels", RunUID: "run_7777777777777777777777777777777777777777777777777777777777777777",
		Namespace: scan.Namespace, RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
		RepositoryScanGeneration: scan.Generation, TaskName: "source-task", Mode: "manual", Phase: scanRunPhaseSucceeded,
		HeadCommit: strings.Repeat("d", 40), StartedAt: time.Now().UTC(), Quality: storepkg.LegacyScanQuality(),
	}
	if err := securityStore.CreateScanRun(ctx, run); err != nil {
		t.Fatalf("CreateScanRun() error = %v", err)
	}
	finding := &storepkg.Finding{
		ID: "fnd_" + strings.Repeat("a", 64), Namespace: scan.Namespace, RepositoryScan: scan.Name,
		ScanRunID: run.ID, CurrentOccurrenceID: "occ_" + strings.Repeat("b", 64),
		Title: "Full width finding", Severity: "high", Confidence: "high",
	}
	reconciler := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: securityStore}
	if err := reconciler.createValidationTask(ctx, scan, finding); err != nil {
		t.Fatalf("createValidationTask() error = %v", err)
	}
	taskName := security.ScanStageTaskNameForRun(
		scan.Name, "validation", security.StageValidation, finding.CurrentOccurrenceID, run.RunUID,
	)
	task := &corev1alpha1.Task{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: scan.Namespace, Name: taskName}, task); err != nil {
		t.Fatalf("Get(validation task) error = %v", err)
	}
	if task.Labels[labels.LabelSecurityFindingID] != labels.SelectorValue(finding.ID) ||
		task.Labels[labels.LabelSecurityOccurrenceID] != labels.SelectorValue(finding.CurrentOccurrenceID) {
		t.Fatalf("validation labels = %#v", task.Labels)
	}
	if len(task.Labels[labels.LabelSecurityFindingID]) > 63 || len(task.Labels[labels.LabelSecurityOccurrenceID]) > 63 {
		t.Fatalf("validation labels exceed Kubernetes limit: %#v", task.Labels)
	}
	if got, err := taskSecurityFindingID(task); err != nil || got != finding.ID {
		t.Fatalf("taskSecurityFindingID() = (%q, %v)", got, err)
	}
	if got, err := taskSecurityOccurrenceID(task); err != nil || got != finding.CurrentOccurrenceID {
		t.Fatalf("taskSecurityOccurrenceID() = (%q, %v)", got, err)
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
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS, UID: types.UID("kaset-policy-review-uid"), Generation: 1},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:                   "https://github.com/example/repo",
			AnalysisAgentRef:          corev1alpha1.AgentReference{Name: "scan-reviewer"},
			CustomScanInstructionsRef: &corev1alpha1.PolicyConfigMapKeyRef{Name: "scan-policy"},
		},
	}
	policyConfig := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "scan-policy", Namespace: defaultNS, Labels: map[string]string{security.PolicyConfigMapAllowedLabel: "true"}}, Data: map[string]string{"policy": "new policy text"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan, policyConfig).Build()
	reconciler := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: store}
	run := &storepkg.ScanRun{ID: "scan_policy", Namespace: defaultNS, RepositoryScan: "kaset", RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: scan.Generation, Mode: "initial", Phase: scanRunPhaseRunning, PolicyDigest: "sha256:old"}
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
		ObjectMeta: metav1.ObjectMeta{Name: "kaset", Namespace: defaultNS, UID: types.UID("kaset-policy-validation-uid"), Generation: 1},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:                   "https://github.com/example/repo",
			AnalysisAgentRef:          corev1alpha1.AgentReference{Name: "scan-reviewer"},
			CustomScanInstructionsRef: &corev1alpha1.PolicyConfigMapKeyRef{Name: "scan-policy"},
		},
	}
	policyConfig := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "scan-policy", Namespace: defaultNS, Labels: map[string]string{security.PolicyConfigMapAllowedLabel: "true"}}, Data: map[string]string{"policy": "new policy text"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan, policyConfig).Build()
	reconciler := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: store}
	run := &storepkg.ScanRun{ID: "scan_policy", Namespace: defaultNS, RepositoryScan: "kaset", RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: scan.Generation, Mode: "initial", Phase: scanRunPhaseRunning, PolicyDigest: "sha256:old"}
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

func TestProgressLatestScanRunRetiresStaleGenerationReservation(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	reservedScan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "stale-gen", Namespace: defaultNS, UID: types.UID("stale-gen-uid"), Generation: 1},
		Spec:       corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"}},
	}
	run := &storepkg.ScanRun{
		ID: "scan_stale_gen", Namespace: defaultNS, RepositoryScan: reservedScan.Name,
		TaskName: "stale-gen-initial-threat-model", Mode: "initial", Phase: scanRunPhaseRunning, StartedAt: time.Now(),
	}
	reserveScanRunForIngestionTest(t, ctx, store, reservedScan, run)

	currentScan := reservedScan.DeepCopy()
	currentScan.Generation = 2
	currentScan.Status.LastScanID = run.ID
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(currentScan).Build()
	reconciler := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: store}

	progressed, err := reconciler.progressLatestScanRun(ctx, currentScan)
	if err != nil {
		t.Fatalf("progressLatestScanRun() error = %v", err)
	}
	if !progressed {
		t.Fatal("progressLatestScanRun() = false, want stale reservation retirement")
	}
	updated, err := store.GetScanRun(ctx, defaultNS, run.ID)
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if updated.Phase != scanRunPhaseFailed || !strings.Contains(updated.ErrorMessage, "does not match the current repository scan identity") {
		t.Fatalf("stale run phase/error = %q/%q, want retired reservation", updated.Phase, updated.ErrorMessage)
	}
	var tasks corev1alpha1.TaskList
	if err := cl.List(ctx, &tasks, client.InNamespace(defaultNS)); err != nil {
		t.Fatalf("List(Tasks) error = %v", err)
	}
	if len(tasks.Items) != 0 {
		t.Fatalf("len(tasks) = %d, want no pipeline work for a stale-generation run", len(tasks.Items))
	}
	refreshed := &corev1alpha1.RepositoryScan{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: defaultNS, Name: currentScan.Name}, refreshed); err != nil {
		t.Fatalf("Get(RepositoryScan) error = %v", err)
	}
	if refreshed.Status.LastScanID != "" || refreshed.Status.LastScanTaskName != "" || refreshed.Status.Phase != repositoryScanPhasePending {
		t.Fatalf("status after retirement = %q/%q/%q, want cleared stale binding", refreshed.Status.Phase, refreshed.Status.LastScanID, refreshed.Status.LastScanTaskName)
	}
}

func TestRepositoryScanDeletionReleasesActiveRunReservation(t *testing.T) {
	ctx := context.Background()
	store := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{
			Name: "deleted-scan", Namespace: defaultNS, UID: types.UID("deleted-scan-uid"), Generation: 1,
			Finalizers: []string{repositoryScanRunFinalizer},
		},
		Spec:   corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/repo", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"}},
		Status: corev1alpha1.RepositoryScanStatus{Phase: repositoryScanPhaseScanning},
	}
	run := &storepkg.ScanRun{
		ID: "scan_deleted_owner", Namespace: defaultNS, RepositoryScan: scan.Name,
		TaskName: "deleted-scan-initial-threat-model", Mode: "initial", Phase: scanRunPhaseRunning, StartedAt: time.Now(),
	}
	reserveScanRunForIngestionTest(t, ctx, store, scan, run)

	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryScan{}).WithObjects(scan).Build()
	reconciler := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: store}
	if err := cl.Delete(ctx, scan); err != nil {
		t.Fatalf("Delete(RepositoryScan) error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: scan.Namespace, Name: scan.Name}}); err != nil {
		t.Fatalf("Reconcile(deleting) error = %v", err)
	}

	released, err := store.GetScanRun(ctx, defaultNS, run.ID)
	if err != nil {
		t.Fatalf("GetScanRun() error = %v", err)
	}
	if released.Phase != scanRunPhaseFailed || released.CompletedAt == nil ||
		!strings.Contains(released.ErrorMessage, "repository scan was deleted") {
		t.Fatalf("released run = %q/%q, want terminalized reservation", released.Phase, released.ErrorMessage)
	}
	remaining := &corev1alpha1.RepositoryScan{}
	if getErr := cl.Get(ctx, types.NamespacedName{Namespace: scan.Namespace, Name: scan.Name}, remaining); !apierrors.IsNotFound(getErr) {
		t.Fatalf("RepositoryScan after finalization = %v, want deleted", getErr)
	}

	recreated := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: scan.Name, Namespace: defaultNS, UID: types.UID("deleted-scan-uid-2"), Generation: 1},
		Spec:       scan.Spec,
	}
	newRun := &storepkg.ScanRun{
		ID: "scan_recreated_owner", Namespace: defaultNS, RepositoryScan: recreated.Name,
		TaskName: "deleted-scan-initial-threat-model-2", Mode: "initial", Phase: scanRunPhasePending, StartedAt: time.Now(),
	}
	reserveScanRunForIngestionTest(t, ctx, store, recreated, newRun)
}
