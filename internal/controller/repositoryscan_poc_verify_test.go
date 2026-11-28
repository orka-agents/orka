/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

func TestEvaluateControllerPoCVerificationLifecycle(t *testing.T) {
	ctx := context.Background()
	scheme := repositoryMonitorValidationRuntimeScheme(t)
	commitSHA := strings.Repeat("d", 40)
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "poc-scan",
			Namespace: "default",
			UID:       types.UID("poc-scan-uid"),
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/orka-agents/example",
			Branch:  "main",
			PoCExecution: &corev1alpha1.RepositoryScanPoCExecutionSpec{
				Image:            testPatchVerificationImage,
				AllowedHarnesses: []string{security.PoCHarnessGoTest},
			},
		},
	}
	validationTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "poc-scan-val-1", Namespace: "default"},
	}
	finding := &store.Finding{
		ID:        "fnd-poc-1",
		ScanRunID: "scan-poc-1",
		CommitSHA: commitSHA,
	}
	artifact := &security.ValidationArtifact{
		Version:   1,
		FindingID: finding.ID,
		Status:    findingValidationStatusValidated,
		Summary:   "Confirmed path traversal.",
		PoC: &security.ValidationPoC{
			Harness:          security.PoCHarnessGoTest,
			Command:          "go test ./internal/archive -run TestTraversalPoC",
			ControlCommand:   "go test ./internal/archive -run TestSafeExtract",
			ExpectedExitCode: 1,
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(scan, validationTask).Build()
	reconciler := &RepositoryScanReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: scheme}

	pending, verification, err := reconciler.evaluateControllerPoCVerification(ctx, scan, validationTask, finding, artifact)
	if err != nil || !pending || verification != nil {
		t.Fatalf("first PoC eval = (pending=%v, verification=%#v, err=%v), want pending PoC task", pending, verification, err)
	}

	pocTaskName := pocVerifyTaskName(scan.Name, finding.ID, validationTask.Name, pocVerifyPhasePoC, commitSHA, testPatchVerificationImage, artifact.PoC.Command, pocVerifyDefaultTimeout)
	pocTask := &corev1alpha1.Task{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: scan.Namespace, Name: pocTaskName}, pocTask); err != nil {
		t.Fatalf("expected PoC task %s: %v", pocTaskName, err)
	}
	if !isRepositoryScanPoCVerifyTask(pocTask) {
		t.Fatal("expected created PoC task to satisfy isRepositoryScanPoCVerifyTask")
	}
	if !isConfinedVerificationTask(pocTask, false) {
		t.Fatal("expected PoC task to be classified as confined verification task")
	}

	pocTask.Status.Phase = corev1alpha1.TaskPhaseFailed
	pocTask.Status.Message = "job failed: container exited with code 1 (reason=Error)"
	if err := k8sClient.Update(ctx, pocTask); err != nil {
		t.Fatal(err)
	}

	pending, verification, err = reconciler.evaluateControllerPoCVerification(ctx, scan, validationTask, finding, artifact)
	if err != nil || !pending || verification != nil {
		t.Fatalf("second PoC eval = (pending=%v, verification=%#v, err=%v), want pending control task", pending, verification, err)
	}

	controlTaskName := pocVerifyTaskName(scan.Name, finding.ID, validationTask.Name, pocVerifyPhaseControl, commitSHA, testPatchVerificationImage, artifact.PoC.ControlCommand, pocVerifyDefaultTimeout)
	controlTask := &corev1alpha1.Task{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: scan.Namespace, Name: controlTaskName}, controlTask); err != nil {
		t.Fatalf("expected control task %s: %v", controlTaskName, err)
	}
	controlTask.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	if err := k8sClient.Update(ctx, controlTask); err != nil {
		t.Fatal(err)
	}

	pending, verification, err = reconciler.evaluateControllerPoCVerification(ctx, scan, validationTask, finding, artifact)
	if err != nil || pending || verification == nil {
		t.Fatalf("final PoC eval = (pending=%v, verification=%#v, err=%v), want verified result", pending, verification, err)
	}
	if verification.Status != pocVerifyStatusVerified || verification.PoCTriggered == nil || !*verification.PoCTriggered || verification.ControlPassed == nil || !*verification.ControlPassed {
		t.Fatalf("unexpected PoC verification result: %#v", verification)
	}
}

func TestEvaluateControllerPoCVerificationRejectsZeroExitAndDisallowedHarness(t *testing.T) {
	ctx := context.Background()
	scheme := repositoryMonitorValidationRuntimeScheme(t)
	commitSHA := strings.Repeat("e", 40)
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "poc-scan-reject",
			Namespace: "default",
			UID:       types.UID("poc-scan-reject-uid"),
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/orka-agents/example",
			Branch:  "main",
			PoCExecution: &corev1alpha1.RepositoryScanPoCExecutionSpec{
				Image:            testPatchVerificationImage,
				AllowedHarnesses: []string{security.PoCHarnessGoTest},
			},
		},
	}
	validationTask := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "poc-val-2", Namespace: "default"}}
	finding := &store.Finding{ID: "fnd-poc-2", ScanRunID: "scan-poc-2", CommitSHA: commitSHA}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(scan, validationTask).Build()
	reconciler := &RepositoryScanReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: scheme}

	disallowed := &security.ValidationArtifact{
		Version: 1, FindingID: finding.ID, Status: findingValidationStatusValidated, Summary: "Confirmed",
		PoC: &security.ValidationPoC{Harness: security.PoCHarnessShell, Command: "exit 1"},
	}
	pending, verification, err := reconciler.evaluateControllerPoCVerification(ctx, scan, validationTask, finding, disallowed)
	if err != nil || pending || verification == nil || verification.Status != pocVerifyStatusSkipped {
		t.Fatalf("disallowed harness = (pending=%v, verification=%#v, err=%v), want skipped", pending, verification, err)
	}

	allowed := &security.ValidationArtifact{
		Version: 1, FindingID: finding.ID, Status: findingValidationStatusValidated, Summary: "Confirmed",
		PoC: &security.ValidationPoC{Harness: security.PoCHarnessGoTest, Command: "go test ./..."},
	}
	pending, _, err = reconciler.evaluateControllerPoCVerification(ctx, scan, validationTask, finding, allowed)
	if err != nil || !pending {
		t.Fatalf("expected pending PoC task, got pending=%v err=%v", pending, err)
	}
	pocTaskName := pocVerifyTaskName(scan.Name, finding.ID, validationTask.Name, pocVerifyPhasePoC, commitSHA, testPatchVerificationImage, allowed.PoC.Command, pocVerifyDefaultTimeout)
	pocTask := &corev1alpha1.Task{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: scan.Namespace, Name: pocTaskName}, pocTask); err != nil {
		t.Fatal(err)
	}
	pocTask.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	if err := k8sClient.Update(ctx, pocTask); err != nil {
		t.Fatal(err)
	}
	pending, verification, err = reconciler.evaluateControllerPoCVerification(ctx, scan, validationTask, finding, allowed)
	if err != nil || pending || verification == nil || verification.Status != pocVerifyStatusFailed {
		t.Fatalf("zero-exit PoC = (pending=%v, verification=%#v, err=%v), want failed", pending, verification, err)
	}

	raw, err := json.Marshal(security.ValidationResultEnvelope{
		SchemaVersion:  security.AgentResultSchemaVersion,
		Kind:           security.AgentResultKindValidation,
		RepositoryScan: scan.Name,
		ScanID:         finding.ScanRunID,
		FindingID:      finding.ID,
		PolicyDigest:   "sha256:abc",
		Validation: security.ValidationArtifact{
			Version:         1,
			FindingID:       finding.ID,
			Status:          findingValidationStatusValidated,
			Summary:         "Agent tried to forge PoC verification",
			PoCVerification: &security.ValidationPoCVerification{Status: pocVerifyStatusVerified},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := security.ParseValidationResult(raw, security.ValidationResultExpectation{
		Binding: security.AgentResultBinding{RepositoryScan: scan.Name, ScanID: finding.ScanRunID, PolicyDigest: "sha256:abc"},
		Finding: finding,
	}); err == nil || !strings.Contains(err.Error(), "controller-owned") {
		t.Fatalf("expected agent-supplied poc_verification rejection, got %v", err)
	}
}

func TestRepositoryScanPoCVerifyJobUsesSharedConfinement(t *testing.T) {
	ctx := context.Background()
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "poc-job", Namespace: "default", UID: types.UID("poc-job-uid")},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:      "https://github.com/orka-agents/example",
			Branch:       "main",
			PoCExecution: &corev1alpha1.RepositoryScanPoCExecutionSpec{Image: testPatchVerificationImage},
		},
	}
	valTask := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "val-1", Namespace: "default"}}
	finding := &store.Finding{ID: "fnd-1", ScanRunID: "run-1"}
	task := buildDesiredPoCVerifyTask(scan, valTask, finding, pocVerifyPhasePoC, strings.Repeat("a", 40), testPatchVerificationImage, "go test ./...")
	task.UID = types.UID("poc-task-uid")
	task.Labels[labels.LabelSecurityStage] = security.StagePoCVerify

	job, err := setupJobBuilder().Build(ctx, task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if len(job.Spec.Template.Spec.InitContainers) != 3 {
		t.Fatalf("init containers = %#v, want 3 confinement init containers", job.Spec.Template.Spec.InitContainers)
	}
	worker := job.Spec.Template.Spec.Containers[0]
	if len(worker.Command) != 1 || !strings.HasSuffix(worker.Command[0], "/"+repositoryMonitorValidationNetworkSandboxBinary) {
		t.Fatalf("worker command = %#v, want network sandbox binary", worker.Command)
	}
	if _, ok := pocTaskCommandExitCode(&corev1alpha1.Task{
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseFailed,
			Message: "job failed: container exited with code 125 (reason=Error)",
		},
	}); ok {
		t.Fatal("expected reserved sandbox exit code 125 to be rejected")
	}
	if err := security.ValidateValidationPoC(&security.ValidationPoC{
		Harness:        security.PoCHarnessGoTest,
		Command:        "go test ./...",
		ExpectedSignal: "panic: index out of range",
	}); err == nil {
		t.Fatal("expected nonempty expected_signal to be rejected until supported")
	}
	_ = corev1.Pod{}
}

func TestPhase5cSandboxWorkspaceAndPoCHandoffToPatchVerify(t *testing.T) {
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "poc-handoff", Namespace: "default", UID: types.UID("poc-handoff-uid")},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/orka-agents/example",
			Branch:  "main",
			PoCExecution: &corev1alpha1.RepositoryScanPoCExecutionSpec{
				Image: testPatchVerificationImage,
				SandboxWorkspaceClassRef: &corev1alpha1.WorkspaceClassReference{
					Name: "gvisor-sandbox",
				},
			},
		},
	}
	execSpec := repositoryScanValidationExecution(scan)
	if execSpec == nil || execSpec.Workspace == nil || execSpec.Workspace.ClassRef == nil || execSpec.Workspace.ClassRef.Name != "gvisor-sandbox" {
		t.Fatalf("repositoryScanValidationExecution() = %#v, want gvisor-sandbox classRef", execSpec)
	}

	valJSON, err := json.Marshal(security.ValidationArtifact{
		Version:   1,
		FindingID: "fnd-1",
		Status:    findingValidationStatusValidated,
		Summary:   "Confirmed",
		PoC: &security.ValidationPoC{
			Harness:          security.PoCHarnessGoTest,
			Command:          "go test ./pkg/auth -run TestPoC",
			ExpectedExitCode: 42,
		},
		PoCVerification: &security.ValidationPoCVerification{
			Status:    pocVerifyStatusVerified,
			Harness:   security.PoCHarnessGoTest,
			CommitSHA: strings.Repeat("a", 40),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	finding := &store.Finding{ID: "fnd-1", ValidationJSON: string(valJSON)}
	plans, problem := effectivePatchVerificationPlans(scan, finding)
	if problem != "" || len(plans) != 1 || !plans[0].requireFailBeforePass || plans[0].image != testPatchVerificationImage || plans[0].command != "go test ./pkg/auth -run TestPoC" || plans[0].expectedBaseExitCode != 42 {
		t.Fatalf("effectivePatchVerificationPlans() = (%#v, problem=%q), want single PoC plan with expected exit 42", plans, problem)
	}

	scan.Spec.PatchVerification = &corev1alpha1.RepositoryScanPatchVerificationSpec{
		Image:                 testPatchVerificationImage,
		Command:               "go test ./...",
		RequireFailBeforePass: false,
	}
	plans, problem = effectivePatchVerificationPlans(scan, finding)
	if problem != "" || len(plans) != 2 {
		t.Fatalf("effectivePatchVerificationPlans(both) = (%#v, problem=%q), want both operator patchVerification and PoC plans", plans, problem)
	}
	if plans[0].command != "go test ./..." || plans[1].command != "go test ./pkg/auth -run TestPoC" || plans[1].expectedBaseExitCode != 42 {
		t.Fatalf("unexpected combined plans: %#v", plans)
	}

	whitespaceScan := scan.DeepCopy()
	whitespaceScan.Spec.PoCExecution.SandboxWorkspaceClassRef = &corev1alpha1.WorkspaceClassReference{Name: "   "}
	wsExec := repositoryScanValidationExecution(whitespaceScan)
	if wsExec == nil || wsExec.Workspace == nil || wsExec.Workspace.ClassRef == nil || wsExec.Workspace.ClassRef.Name != "" {
		t.Fatalf("expected whitespace sandboxWorkspaceClassRef to remain explicit so admission rejects it, got %#v", wsExec)
	}
}
