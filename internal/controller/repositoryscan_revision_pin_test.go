package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/security"
	storepkg "github.com/orka-agents/orka/internal/store"
)

const (
	pinnedScanHead       = "0123456789abcdef0123456789abcdef01234567"
	pinnedFindingCommit  = "fedcba9876543210fedcba9876543210fedcba98"
	pinnedScanRepository = "https://github.com/example/repo"
)

// A review examines the commit the mapper sliced, and its retry is rebuilt at
// that same commit. The branch can advance while the run is in flight; the
// pin must not change the target identity the result is bound to, which
// stays the scan's branch.
func TestPinnedReviewRetryKeepsTheRunCommitAndBranchTarget(t *testing.T) {
	fixture := newReviewResultRetryFixtureAt(t, pinnedScanHead)
	if err := fixture.reconciler.ingestScanTask(fixture.ctx, fixture.scan, fixture.sourceTask); err != nil {
		t.Fatalf("ingestScanTask(source) error = %v", err)
	}
	retryTask := fixture.getRetryTask(t)
	workspace := retryTask.Spec.Workspace
	if workspace == nil || workspace.Ref != pinnedScanHead || workspace.Branch != "main" || workspace.Intent != corev1alpha1.WorkspaceIntentRead {
		t.Fatalf("retry workspace = %#v, want read workspace on branch main pinned to %s", workspace, pinnedScanHead)
	}

	findings := security.FindingsV2Artifact{
		SchemaVersion: security.SchemaVersionFindingsV2,
		Repository: security.FindingsV2Repository{
			RepoURL: fixture.scan.Spec.RepoURL,
			Branch:  "main",
			HeadSHA: pinnedScanHead,
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
		t.Fatalf("run after pinned retry = %#v, want one succeeded review slice", run)
	}
}

func TestReviewTaskTargetWorkspaceUnpinsOnlyForTheAdmittingGeneration(t *testing.T) {
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "pin-target", Namespace: defaultNS, UID: types.UID("pin-target-uid"), Generation: 3},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: pinnedScanRepository, Branch: "release", AnalysisAgentRef: corev1alpha1.AgentReference{Name: "scan-reviewer"},
		},
	}
	run := &storepkg.ScanRun{
		ID: "scan_pin_target", Namespace: defaultNS, RepositoryScan: scan.Name,
		RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: scan.Generation, HeadCommit: pinnedScanHead,
	}
	task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
		Workspace: security.PinScanWorkspace(repositoryScanTaskWorkspace(scan, corev1alpha1.WorkspaceIntentRead), run.HeadCommit),
	}}

	repository, problem := trustedFindingsRepositoryForRunTask(scan, run, task)
	if problem != "" {
		t.Fatalf("trustedFindingsRepositoryForRunTask() problem = %q", problem)
	}
	want := security.FindingsV2Repository{RepoURL: pinnedScanRepository, Branch: "release", HeadSHA: pinnedScanHead}
	if repository != want {
		t.Fatalf("trusted repository = %#v, want %#v", repository, want)
	}
	if task.Spec.Workspace.Ref != pinnedScanHead {
		t.Fatalf("Task workspace ref = %q, want the pin left in place", task.Spec.Workspace.Ref)
	}

	// A later generation cannot say what the admitting ref was, so the pin
	// stays and the target no longer matches the scan's branch.
	edited := scan.DeepCopy()
	edited.Generation++
	repository, problem = trustedFindingsRepositoryForRunTask(edited, run, task)
	if problem != "" {
		t.Fatalf("trustedFindingsRepositoryForRunTask(edited) problem = %q", problem)
	}
	if repository.Branch != "ref:"+pinnedScanHead {
		t.Fatalf("trusted branch for another generation = %q, want the pinned commit", repository.Branch)
	}
}

func TestRepositoryScanValidationTaskPinsTheFindingCommit(t *testing.T) {
	for _, tt := range []struct {
		name      string
		commitSHA string
		wantRef   string
	}{
		{name: "occurrence commit", commitSHA: pinnedFindingCommit, wantRef: pinnedFindingCommit},
		{name: "no recorded commit", commitSHA: "", wantRef: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			securityStore := setupControllerSQLiteStore(t)
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatalf("AddToScheme() error = %v", err)
			}
			scan := &corev1alpha1.RepositoryScan{
				TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
				ObjectMeta: metav1.ObjectMeta{
					Name: "validation-pin", Namespace: defaultNS, UID: types.UID("validation-pin-uid"),
				},
				Spec: corev1alpha1.RepositoryScanSpec{
					RepoURL: pinnedScanRepository, AnalysisAgentRef: corev1alpha1.AgentReference{Name: "scan-reviewer"},
				},
			}
			analysisAgent, analysisRuntime := repositoryScanExternalRuntimeFixtures(scan.Spec.AnalysisAgentRef.Name, []string{})
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repositoryScanTestObjects(scan, analysisAgent, analysisRuntime)...).Build()
			reconciler := &RepositoryScanReconciler{Client: cl, Scheme: scheme, SecurityStore: securityStore}
			finding := &storepkg.Finding{
				ID: "finding-pin", Namespace: defaultNS, RepositoryScan: scan.Name, Severity: "high", Confidence: "high",
				CommitSHA: tt.commitSHA,
			}
			if err := reconciler.ensureValidationTask(ctx, scan, finding, nil); err != nil {
				t.Fatalf("ensureValidationTask() error = %v", err)
			}
			var tasks corev1alpha1.TaskList
			if err := cl.List(ctx, &tasks, client.InNamespace(defaultNS)); err != nil {
				t.Fatalf("List(Task) error = %v", err)
			}
			if len(tasks.Items) != 1 {
				t.Fatalf("validation tasks = %d, want 1", len(tasks.Items))
			}
			workspace := tasks.Items[0].Spec.Workspace
			if workspace == nil || workspace.Ref != tt.wantRef || workspace.Branch != "main" || workspace.Intent != corev1alpha1.WorkspaceIntentRead {
				t.Fatalf("validation workspace = %#v, want read workspace on main with ref %q", workspace, tt.wantRef)
			}
		})
	}
}
