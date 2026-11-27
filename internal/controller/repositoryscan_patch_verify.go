/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
)

const (
	patchVerifyPhaseBase    = "base"
	patchVerifyPhasePatched = "patched"
	patchVerifyTimeout      = 30 * time.Minute
	patchVerifyStatusVerify = "verified"
	patchVerifyStatusFailed = "failed"
	patchVerifyShell        = "/bin/sh"
)

func repositoryScanPatchVerificationEnabled(scan *corev1alpha1.RepositoryScan) bool {
	if scan == nil || scan.Spec.PatchVerification == nil {
		return false
	}
	return tools.ValidRepositoryValidationImage(scan.Spec.PatchVerification.Image) &&
		strings.TrimSpace(scan.Spec.PatchVerification.Command) != ""
}

func patchVerifyBaseCommandExitedNonZero(task *corev1alpha1.Task) bool {
	if task == nil || task.Status.Phase != corev1alpha1.TaskPhaseFailed {
		return false
	}
	msg := strings.TrimSpace(task.Status.Message)
	if !strings.HasPrefix(msg, "job failed: container exited with code ") {
		return false
	}
	if strings.Contains(msg, "container exited with code 0 ") ||
		strings.Contains(msg, "container exited with code 137 ") ||
		strings.Contains(msg, "OOMKilled") {
		return false
	}
	return true
}

func patchVerifyTaskName(scanName, findingID, patchTaskName, phase, commitSHA, image, command string) string {
	scope := strings.Join([]string{
		findingID,
		patchTaskName,
		strings.TrimSpace(commitSHA),
		strings.TrimSpace(image),
		strings.TrimSpace(command),
	}, "\n")
	return security.ScanStageTaskName(scanName, security.StagePatchVerify, phase, scope)
}

func (r *RepositoryScanReconciler) evaluateControllerPatchVerification(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	patchTask *corev1alpha1.Task,
	finding *store.Finding,
	publication securityPatchPublicationReceipt,
	verified patchVerificationResult,
) (bool, string, error) {
	if !repositoryScanPatchVerificationEnabled(scan) {
		return false, "", nil
	}
	spec := scan.Spec.PatchVerification
	patchedCommit := ""
	if publication.publication != nil {
		patchedCommit = strings.TrimSpace(publication.publication.ExpectedCommitSHA)
	}
	if patchedCommit == "" || store.ValidateGitObjectID("patched commit", patchedCommit) != nil {
		return false, "controller patch verification requires a valid published commit SHA", nil
	}
	baseCommit := strings.TrimSpace(finding.CommitSHA)
	if spec.RequireFailBeforePass && (baseCommit == "" || store.ValidateGitObjectID("base commit", baseCommit) != nil) {
		return false, "controller fail->pass verification requires a pinned base commit SHA on the finding", nil
	}

	var baseTask *corev1alpha1.Task
	var basePassed *bool
	if spec.RequireFailBeforePass {
		var err error
		baseTask, err = r.ensurePatchVerifyTask(ctx, scan, patchTask, finding, patchVerifyPhaseBase, baseCommit, false)
		if err != nil {
			return false, "", err
		}
		if !baseTask.Status.Phase.IsTerminal() {
			return true, "", nil
		}
		if baseTask.Status.Phase == corev1alpha1.TaskPhaseCancelled {
			return false, "controller base verification task was cancelled", nil
		}
		passed := baseTask.Status.Phase == corev1alpha1.TaskPhaseSucceeded
		basePassed = &passed
		if passed {
			r.persistControllerVerificationSummary(ctx, patchTask, verified.summaryArtifact, &security.PatchControllerVerification{
				Status:                patchVerifyStatusFailed,
				Command:               strings.TrimSpace(spec.Command),
				RequireFailBeforePass: true,
				BaseCommit:            baseCommit,
				PatchedCommit:         patchedCommit,
				BaseTaskName:          baseTask.Name,
				BasePassed:            basePassed,
			})
			return false, "controller fail->pass verification rejected: command already passed on unpatched base commit", nil
		}
		if !patchVerifyBaseCommandExitedNonZero(baseTask) {
			r.persistControllerVerificationSummary(ctx, patchTask, verified.summaryArtifact, &security.PatchControllerVerification{
				Status:                patchVerifyStatusFailed,
				Command:               strings.TrimSpace(spec.Command),
				RequireFailBeforePass: true,
				BaseCommit:            baseCommit,
				PatchedCommit:         patchedCommit,
				BaseTaskName:          baseTask.Name,
			})
			return false, "controller fail->pass verification rejected: base task did not record a command failure exit code", nil
		}
	}

	patchedTask, err := r.ensurePatchVerifyTask(ctx, scan, patchTask, finding, patchVerifyPhasePatched, patchedCommit, true)
	if err != nil {
		return false, "", err
	}
	if !patchedTask.Status.Phase.IsTerminal() {
		return true, "", nil
	}
	if patchedTask.Status.Phase == corev1alpha1.TaskPhaseCancelled {
		return false, "controller patched verification task was cancelled", nil
	}
	patchedPassed := patchedTask.Status.Phase == corev1alpha1.TaskPhaseSucceeded
	verification := &security.PatchControllerVerification{
		Status:                patchVerifyStatusVerify,
		Command:               strings.TrimSpace(spec.Command),
		RequireFailBeforePass: spec.RequireFailBeforePass,
		BaseCommit:            baseCommit,
		PatchedCommit:         patchedCommit,
		PatchedTaskName:       patchedTask.Name,
		BasePassed:            basePassed,
		PatchedPassed:         &patchedPassed,
	}
	if baseTask != nil {
		verification.BaseTaskName = baseTask.Name
	}
	if !patchedPassed {
		verification.Status = patchVerifyStatusFailed
		r.persistControllerVerificationSummary(ctx, patchTask, verified.summaryArtifact, verification)
		return false, "controller patch verification failed on patched commit", nil
	}
	r.persistControllerVerificationSummary(ctx, patchTask, verified.summaryArtifact, verification)
	return false, "", nil
}

func (r *RepositoryScanReconciler) ensurePatchVerifyTask(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	patchTask *corev1alpha1.Task,
	finding *store.Finding,
	phase string,
	commitSHA string,
	useTargetRepo bool,
) (*corev1alpha1.Task, error) {
	taskName := patchVerifyTaskName(scan.Name, finding.ID, patchTask.Name, phase, commitSHA, scan.Spec.PatchVerification.Image, scan.Spec.PatchVerification.Command)
	existing := &corev1alpha1.Task{}
	err := r.Get(ctx, types.NamespacedName{Namespace: scan.Namespace, Name: taskName}, existing)
	if err == nil {
		return existing, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	workspace := repositoryScanTaskWorkspaceAtCommit(scan, corev1alpha1.WorkspaceIntentRead, commitSHA)
	if useTargetRepo && strings.TrimSpace(scan.Spec.ForkRepo) != "" {
		workspace.GitRepo = security.CanonicalRepositoryCloneURL(scan.Spec.ForkRepo)
		if scan.Spec.PublicationReadCredentialRef != nil {
			workspace.ReadCredentialRef = workspaceCredentialReference(scan.Spec.PublicationReadCredentialRef)
		}
	}
	timeout := metav1.Duration{Duration: patchVerifyTimeout}
	priority := int32(740)
	desired := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskName,
			Namespace: scan.Namespace,
			Labels: map[string]string{
				labels.LabelManaged:           scheduledRunLabelValue,
				labels.LabelCreatedBy:         repositorySecurityTaskCreator,
				labels.LabelSecurityTarget:    labels.SelectorValue(scan.Name),
				labels.LabelSecurityScanID:    finding.ScanRunID,
				labels.LabelSecurityMode:      phase,
				labels.LabelSecurityStage:     security.StagePatchVerify,
				labels.LabelSecurityFindingID: finding.ID,
			},
			Annotations: map[string]string{
				labels.AnnotationWorkspaceInitContainer: scheduledRunLabelValue,
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(scan, corev1alpha1.GroupVersion.WithKind("RepositoryScan"))},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:      corev1alpha1.TaskTypeContainer,
			Image:     strings.TrimSpace(scan.Spec.PatchVerification.Image),
			Command:   []string{patchVerifyShell, "-c"},
			Args:      []string{strings.TrimSpace(scan.Spec.PatchVerification.Command)},
			Timeout:   &timeout,
			Priority:  &priority,
			Workspace: workspace,
		},
	}
	if err := r.Create(ctx, desired); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if getErr := r.Get(ctx, types.NamespacedName{Namespace: scan.Namespace, Name: taskName}, existing); getErr == nil {
				return existing, nil
			}
		}
		return nil, err
	}
	return desired, nil
}

func (r *RepositoryScanReconciler) persistControllerVerificationSummary(
	ctx context.Context,
	patchTask *corev1alpha1.Task,
	summaryArtifact string,
	verification *security.PatchControllerVerification,
) {
	if r.ArtifactStore == nil || strings.TrimSpace(summaryArtifact) == "" || verification == nil {
		return
	}
	owner := store.ArtifactOwner{
		Namespace: patchTask.Namespace,
		TaskName:  patchTask.Name,
		TaskUID:   string(patchTask.UID),
	}
	data, _, err := r.ArtifactStore.GetArtifact(ctx, store.ArtifactKey{Owner: owner, Filename: summaryArtifact})
	if err != nil {
		return
	}
	var summary security.PatchSummaryArtifact
	if err := json.Unmarshal(data, &summary); err != nil {
		return
	}
	summary.ControllerVerification = verification
	updated, err := json.Marshal(&summary)
	if err != nil {
		return
	}
	_ = r.ArtifactStore.SaveArtifact(ctx, store.SaveArtifactRequest{
		Owner:       owner,
		Filename:    summaryArtifact,
		ContentType: artifactContentTypeJSON,
		Data:        updated,
	})
	switch verification.Status {
	case patchVerifyStatusVerify:
		metrics.RecordSecurityPatchVerification("controller_verified", "ok")
	case patchVerifyStatusFailed:
		metrics.RecordSecurityPatchVerification("controller_failed", "verification_failed")
	}
}
