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
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

const testPatchVerificationImage = "ghcr.io/example/patch-verify@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestIngestPatchTaskControllerVerificationFailBeforePass(t *testing.T) {
	ctx := context.Background()
	var seenToken string
	file := repositoryScanCommitFileResponse{
		Filename:  "app.py",
		Status:    "modified",
		Additions: 1,
		Deletions: 1,
		Patch:     "@@ -1 +1 @@\n-unsafe()\n+safe()",
	}
	server, pr := newPatchCommitServerWithPullRequest(t, []repositoryScanCommitFileResponse{file}, &seenToken)
	pr.commitDiff = testPatchFullDiff
	fixture := patchFixtureWithForgeSecret(t, "verify-fail-pass", server, true)
	fixture.scan.Spec.PublicationReadCredentialRef = &corev1.LocalObjectReference{Name: "pub-read-secret"}
	fixture.scan.Spec.PatchVerification = &corev1alpha1.RepositoryScanPatchVerificationSpec{
		Image:                 testPatchVerificationImage,
		Command:               "pytest tests/test_security.py",
		RequireFailBeforePass: true,
	}
	fixture.finding.CommitSHA = strings.Repeat("c", 40)
	if err := fixture.store.UpsertFinding(ctx, fixture.finding); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.SaveResult(ctx, fixture.proposal.Namespace, fixture.proposal.TaskName, repositoryScanPatchResultEnvelope(fixture, []string{"app.py"})); err != nil {
		t.Fatal(err)
	}
	task := patchTaskForFixture(fixture, true)

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, task); err != nil {
		t.Fatal(err)
	}
	assertPatchIngestState(t, fixture, scanRunPhasePending, findingStatePatchPending)

	sourceBaselineSHA := strings.Repeat("1", 40)
	baseTaskName := patchVerifyTaskName(
		fixture.scan.Name,
		fixture.finding.ID,
		task.Name,
		patchVerifyPhaseBase,
		sourceBaselineSHA,
		fixture.scan.Spec.PatchVerification.Image,
		fixture.scan.Spec.PatchVerification.Command,
	)
	baseTask := &corev1alpha1.Task{}
	if err := fixture.reconciler.Get(ctx, types.NamespacedName{Namespace: fixture.scan.Namespace, Name: baseTaskName}, baseTask); err != nil {
		t.Fatalf("expected base verification task %s: %v", baseTaskName, err)
	}
	if baseTask.Spec.Type != corev1alpha1.TaskTypeContainer || baseTask.Spec.Image != testPatchVerificationImage {
		t.Fatalf("unexpected base verification task spec: %#v", baseTask.Spec)
	}
	if baseTask.Spec.Workspace == nil || baseTask.Spec.Workspace.Ref != sourceBaselineSHA {
		t.Fatalf("base verification workspace ref = %#v, want %q", baseTask.Spec.Workspace, sourceBaselineSHA)
	}
	if baseTask.Labels[labels.LabelSecurityStage] != security.StagePatchVerify {
		t.Fatalf("base verification stage label = %q, want %q", baseTask.Labels[labels.LabelSecurityStage], security.StagePatchVerify)
	}

	baseTask.Status.Phase = corev1alpha1.TaskPhaseFailed
	baseTask.Status.Message = "job failed: container exited with code 1 (reason=Error)"
	if err := fixture.reconciler.Update(ctx, baseTask); err != nil {
		t.Fatal(err)
	}
	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, task); err != nil {
		t.Fatal(err)
	}
	assertPatchIngestState(t, fixture, scanRunPhasePending, findingStatePatchPending)

	patchedTaskName := patchVerifyTaskName(
		fixture.scan.Name,
		fixture.finding.ID,
		task.Name,
		patchVerifyPhasePatched,
		strings.Repeat("b", 40),
		fixture.scan.Spec.PatchVerification.Image,
		fixture.scan.Spec.PatchVerification.Command,
	)
	patchedTask := &corev1alpha1.Task{}
	if err := fixture.reconciler.Get(ctx, types.NamespacedName{Namespace: fixture.scan.Namespace, Name: patchedTaskName}, patchedTask); err != nil {
		t.Fatalf("expected patched verification task %s: %v", patchedTaskName, err)
	}
	if patchedTask.Spec.Workspace == nil || patchedTask.Spec.Workspace.Ref == "" {
		t.Fatalf("patched verification workspace missing commit pin: %#v", patchedTask.Spec.Workspace)
	}
	if patchedTask.Spec.Workspace.ReadCredentialRef == nil || patchedTask.Spec.Workspace.ReadCredentialRef.Name != fixture.scan.Spec.PublicationReadCredentialRef.Name {
		t.Fatalf("patched verification readCredentialRef = %#v, want %q", patchedTask.Spec.Workspace.ReadCredentialRef, fixture.scan.Spec.PublicationReadCredentialRef.Name)
	}
	patchedTask.Spec = taskSpecWithServerDefaults(patchedTask.Spec)
	desiredPatched := buildDesiredPatchVerifyTask(fixture.scan, task, fixture.finding, patchVerifyPhasePatched, strings.Repeat("b", 40), true)
	if !patchVerifyTaskMatchesDesired(fixture.scan, patchedTask, desiredPatched) {
		t.Fatal("expected patchVerifyTaskMatchesDesired to accept server-defaulted task spec")
	}
	tampered := patchedTask.DeepCopy()
	tampered.Spec.Env = []corev1.EnvVar{{Name: "GOFLAGS", Value: "-run=^$"}}
	if patchVerifyTaskMatchesDesired(fixture.scan, tampered, desiredPatched) {
		t.Fatal("expected patchVerifyTaskMatchesDesired to reject unexpected Spec.Env")
	}

	patchedTask.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	if err := fixture.reconciler.Update(ctx, patchedTask); err != nil {
		t.Fatal(err)
	}
	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, task); err != nil {
		t.Fatal(err)
	}
	assertPatchIngestState(t, fixture, patchProposalStatusPROpened, findingStatePROpen)

	_, summaryName := patchArtifactNames(fixture.finding.ID)
	data, _, err := fixture.store.GetArtifact(ctx, store.ArtifactKey{
		Owner:    store.ArtifactOwner{Namespace: task.Namespace, TaskName: task.Name, TaskUID: string(task.UID)},
		Filename: controllerVerificationArtifactName(summaryName),
	})
	if err != nil {
		t.Fatalf("load controller verification artifact: %v", err)
	}
	var verification security.PatchControllerVerification
	if err := json.Unmarshal(data, &verification); err != nil {
		t.Fatalf("unmarshal controller verification: %v", err)
	}
	if verification.Status != patchVerifyStatusVerify || !verification.RequireFailBeforePass {
		t.Fatalf("unexpected controller verification artifact: %#v", verification)
	}
	if pr.patched == nil || !strings.Contains(pr.body, "Controller-verified fix (fail->pass)") {
		t.Fatalf("PR body missing controller verification disclosure:\n%s", pr.body)
	}
}

func TestIngestPatchTaskControllerVerificationRejectsBasePass(t *testing.T) {
	ctx := context.Background()
	var seenToken string
	file := repositoryScanCommitFileResponse{
		Filename:  "app.py",
		Status:    "modified",
		Additions: 1,
		Deletions: 1,
		Patch:     "@@ -1 +1 @@\n-unsafe()\n+safe()",
	}
	server, pr := newPatchCommitServerWithPullRequest(t, []repositoryScanCommitFileResponse{file}, &seenToken)
	pr.commitDiff = testPatchFullDiff
	fixture := patchFixtureWithForgeSecret(t, "verify-base-pass", server, true)
	fixture.scan.Spec.PatchVerification = &corev1alpha1.RepositoryScanPatchVerificationSpec{
		Image:                 testPatchVerificationImage,
		Command:               "pytest tests/test_security.py",
		RequireFailBeforePass: true,
	}
	fixture.finding.CommitSHA = strings.Repeat("b", 40)
	if err := fixture.store.UpsertFinding(ctx, fixture.finding); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.SaveResult(ctx, fixture.proposal.Namespace, fixture.proposal.TaskName, repositoryScanPatchResultEnvelope(fixture, []string{"app.py"})); err != nil {
		t.Fatal(err)
	}
	task := patchTaskForFixture(fixture, true)

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, task); err != nil {
		t.Fatal(err)
	}
	baseTaskName := patchVerifyTaskName(
		fixture.scan.Name,
		fixture.finding.ID,
		task.Name,
		patchVerifyPhaseBase,
		strings.Repeat("1", 40),
		fixture.scan.Spec.PatchVerification.Image,
		fixture.scan.Spec.PatchVerification.Command,
	)
	baseTask := &corev1alpha1.Task{}
	if err := fixture.reconciler.Get(ctx, types.NamespacedName{Namespace: fixture.scan.Namespace, Name: baseTaskName}, baseTask); err != nil {
		t.Fatal(err)
	}
	baseTask.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	if err := fixture.reconciler.Update(ctx, baseTask); err != nil {
		t.Fatal(err)
	}
	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, task); err != nil {
		t.Fatal(err)
	}
	assertPatchIngestState(t, fixture, scanRunPhaseFailed, findingStateOpen)
}

func TestIngestPatchTaskControllerVerificationRejectsBaseOrchestrationFailure(t *testing.T) {
	ctx := context.Background()
	var seenToken string
	file := repositoryScanCommitFileResponse{
		Filename:  "app.py",
		Status:    "modified",
		Additions: 1,
		Deletions: 1,
		Patch:     "@@ -1 +1 @@\n-unsafe()\n+safe()",
	}
	server, pr := newPatchCommitServerWithPullRequest(t, []repositoryScanCommitFileResponse{file}, &seenToken)
	pr.commitDiff = testPatchFullDiff
	fixture := patchFixtureWithForgeSecret(t, "verify-base-infra-fail", server, true)
	fixture.scan.Spec.PatchVerification = &corev1alpha1.RepositoryScanPatchVerificationSpec{
		Image:                 testPatchVerificationImage,
		Command:               "pytest tests/test_security.py",
		RequireFailBeforePass: true,
	}
	fixture.finding.CommitSHA = strings.Repeat("b", 40)
	if err := fixture.store.UpsertFinding(ctx, fixture.finding); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.SaveResult(ctx, fixture.proposal.Namespace, fixture.proposal.TaskName, repositoryScanPatchResultEnvelope(fixture, []string{"app.py"})); err != nil {
		t.Fatal(err)
	}
	task := patchTaskForFixture(fixture, true)

	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, task); err != nil {
		t.Fatal(err)
	}
	baseTaskName := patchVerifyTaskName(
		fixture.scan.Name,
		fixture.finding.ID,
		task.Name,
		patchVerifyPhaseBase,
		strings.Repeat("1", 40),
		fixture.scan.Spec.PatchVerification.Image,
		fixture.scan.Spec.PatchVerification.Command,
	)
	baseTask := &corev1alpha1.Task{}
	if err := fixture.reconciler.Get(ctx, types.NamespacedName{Namespace: fixture.scan.Namespace, Name: baseTaskName}, baseTask); err != nil {
		t.Fatal(err)
	}
	baseTask.Status.Phase = corev1alpha1.TaskPhaseFailed
	baseTask.Status.Message = "job failed: container exited with code 143 (reason=Error)"
	if err := fixture.reconciler.Update(ctx, baseTask); err != nil {
		t.Fatal(err)
	}
	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, task); err != nil {
		t.Fatal(err)
	}
	assertPatchIngestState(t, fixture, scanRunPhaseFailed, findingStateOpen)
}

func TestIngestPatchTaskControllerVerificationFailsClosedOnInvalidSpecAndForeignTask(t *testing.T) {
	ctx := context.Background()
	var seenToken string
	file := repositoryScanCommitFileResponse{
		Filename:  "app.py",
		Status:    "modified",
		Additions: 1,
		Deletions: 1,
		Patch:     "@@ -1 +1 @@\n-unsafe()\n+safe()",
	}
	server, pr := newPatchCommitServerWithPullRequest(t, []repositoryScanCommitFileResponse{file}, &seenToken)
	pr.commitDiff = testPatchFullDiff
	fixture := patchFixtureWithForgeSecret(t, "verify-invalid-spec", server, true)
	fixture.scan.Spec.PatchVerification = &corev1alpha1.RepositoryScanPatchVerificationSpec{
		Image:   testPatchVerificationImage,
		Command: "   ",
	}
	if err := fixture.store.SaveResult(ctx, fixture.proposal.Namespace, fixture.proposal.TaskName, repositoryScanPatchResultEnvelope(fixture, []string{"app.py"})); err != nil {
		t.Fatal(err)
	}
	task := patchTaskForFixture(fixture, true)
	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, task); err != nil {
		t.Fatal(err)
	}
	assertPatchIngestState(t, fixture, scanRunPhaseFailed, findingStateOpen)
}

func TestRepositoryScanPatchVerifyJobAndConfinementLifecycle(t *testing.T) {
	ctx := context.Background()
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scan-confinement",
			Namespace: "default",
			UID:       types.UID("scan-confinement-uid"),
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/orka-agents/example",
			Branch:  "main",
			PatchVerification: &corev1alpha1.RepositoryScanPatchVerificationSpec{
				Image:   testPatchVerificationImage,
				Command: "go test ./...",
			},
		},
	}
	patchTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "patch-task-1", Namespace: "default"},
	}
	finding := &store.Finding{ID: "fnd-1", ScanRunID: "scan-run-1"}
	task := buildDesiredPatchVerifyTask(scan, patchTask, finding, patchVerifyPhasePatched, strings.Repeat("a", 40), true)
	task.UID = types.UID("patch-verify-task-uid")
	task.Annotations[labels.AnnotationTransactionTokenSecret] = "injected-token-secret"

	builder := setupJobBuilder()
	job, err := builder.Build(ctx, task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if len(job.Spec.Template.Spec.InitContainers) != 3 {
		t.Fatalf("init containers = %#v, want workspace preparation, network probe, and network gate", job.Spec.Template.Spec.InitContainers)
	}
	if job.Spec.Template.Spec.InitContainers[0].Name != workspacePreparationInitContainerName ||
		job.Spec.Template.Spec.InitContainers[1].Name != repositoryMonitorValidationNetworkProbeContainer ||
		job.Spec.Template.Spec.InitContainers[2].Name != repositoryMonitorValidationNetworkGateContainer {
		t.Fatalf("init container order = %q, %q, %q",
			job.Spec.Template.Spec.InitContainers[0].Name,
			job.Spec.Template.Spec.InitContainers[1].Name,
			job.Spec.Template.Spec.InitContainers[2].Name,
		)
	}
	worker := job.Spec.Template.Spec.Containers[0]
	if len(worker.Command) != 1 || !strings.HasSuffix(worker.Command[0], "/"+repositoryMonitorValidationNetworkSandboxBinary) {
		t.Fatalf("patch-verify worker command = %#v, want network sandbox binary", worker.Command)
	}
	if len(worker.Args) < 4 || worker.Args[0] != repositoryMonitorValidationSandboxWorkerMode || worker.Args[1] != patchVerifyShell || worker.Args[2] != "-c" || worker.Args[3] != "go test ./..." {
		t.Fatalf("patch-verify worker args = %#v, want sandbox wrapper around command", worker.Args)
	}
	for i := range job.Spec.Template.Spec.Volumes {
		if job.Spec.Template.Spec.Volumes[i].Secret != nil && job.Spec.Template.Spec.Volumes[i].Secret.SecretName == "injected-token-secret" {
			t.Fatal("patch-verify Pod mounted an injected transaction-token Secret")
		}
	}

	scheme := repositoryMonitorValidationRuntimeScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(scan, task).Build()
	builder.Client = k8sClient
	reconciler := &TaskReconciler{
		Client:     k8sClient,
		APIReader:  k8sClient,
		Scheme:     scheme,
		JobBuilder: builder,
	}

	ready, err := reconciler.ensureRepositoryMonitorValidationNetworkGateForTask(ctx, task, false)
	if err != nil || ready {
		t.Fatalf("first gate reconcile = (%v, %v), want created and pending", ready, err)
	}
	ready, err = reconciler.ensureRepositoryMonitorValidationNetworkGateForTask(ctx, task, false)
	if err != nil || !ready {
		t.Fatalf("second gate reconcile = (%v, %v), want ready for Job creation", ready, err)
	}

	job = repositoryMonitorValidationRuntimeJob(t, task, scheme, builder)
	task.Status.Attempts = 1
	pod := repositoryMonitorValidationRuntimePod(job)
	pod.Status = corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{
			{Name: workspacePreparationInitContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: repositoryMonitorValidationNetworkProbeContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		},
	}
	if err := k8sClient.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}

	if err := reconciler.reconcileRepositoryMonitorValidationConfinement(ctx, task, job); err != nil {
		t.Fatalf("first confinement reconcile error = %v", err)
	}
	policy := &networkingv1.NetworkPolicy{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, policy); err != nil {
		t.Fatalf("expected NetworkPolicy for patch-verify task: %v", err)
	}
	if err := reconciler.reconcileRepositoryMonitorValidationConfinement(ctx, task, job); err != nil {
		t.Fatalf("second confinement reconcile error = %v", err)
	}
	gate := &corev1.ConfigMap{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, gate); err != nil {
		t.Fatal(err)
	}
	if gate.Data[repositoryMonitorValidationNetworkGateKey] != repositoryMonitorValidationNetworkGateReady {
		t.Fatalf("gate state = %q, want %q", gate.Data[repositoryMonitorValidationNetworkGateKey], repositoryMonitorValidationNetworkGateReady)
	}
}
