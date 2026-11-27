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
	"k8s.io/apimachinery/pkg/types"

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
	assertPatchIngestState(t, fixture, scanRunPhasePending, findingStatePatchPending)

	baseTaskName := patchVerifyTaskName(
		fixture.scan.Name,
		fixture.finding.ID,
		task.Name,
		patchVerifyPhaseBase,
		fixture.finding.CommitSHA,
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
	if baseTask.Spec.Workspace == nil || baseTask.Spec.Workspace.Ref != fixture.finding.CommitSHA {
		t.Fatalf("base verification workspace ref = %#v, want %q", baseTask.Spec.Workspace, fixture.finding.CommitSHA)
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
		fixture.finding.CommitSHA,
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
		fixture.finding.CommitSHA,
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
