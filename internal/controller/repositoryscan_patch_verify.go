/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
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
	"github.com/orka-agents/orka/internal/workerenv"
)

const (
	patchVerifyPhaseBase        = "base"
	patchVerifyPhasePatched     = "patched"
	patchVerifyTimeout          = 30 * time.Minute
	patchVerifyStatusVerify     = "verified"
	patchVerifyStatusFailed     = "failed"
	patchVerifyShell            = "/bin/sh"
	patchVerifyArtifactSuffix   = ".controller-verification.json"
	patchVerifyMaxExitCode      = 125
	patchVerifyMaxTaskNameBytes = 63
)

func isRepositoryScanPatchVerifyTask(task *corev1alpha1.Task) bool {
	if task == nil || task.Spec.Type != corev1alpha1.TaskTypeContainer {
		return false
	}
	if task.Labels[labels.LabelCreatedBy] != repositorySecurityTaskCreator ||
		task.Labels[labels.LabelSecurityStage] != security.StagePatchVerify {
		return false
	}
	owner := metav1.GetControllerOf(task)
	return owner != nil &&
		owner.APIVersion == corev1alpha1.GroupVersion.String() &&
		owner.Kind == "RepositoryScan"
}

func validateRepositoryScanPatchVerificationSpec(spec *corev1alpha1.RepositoryScanPatchVerificationSpec) string {
	if spec == nil {
		return ""
	}
	if !tools.ValidRepositoryValidationImage(spec.Image) {
		return "controller patch verification policy is invalid: image must be a digest-pinned OCI reference"
	}
	if strings.TrimSpace(spec.Command) == "" {
		return "controller patch verification policy is invalid: command is required"
	}
	return ""
}

func patchVerifyBaseCommandExitedNonZero(task *corev1alpha1.Task) bool {
	if task == nil || task.Status.Phase != corev1alpha1.TaskPhaseFailed {
		return false
	}
	msg := strings.TrimSpace(task.Status.Message)
	const prefix = "job failed: container exited with code "
	if !strings.HasPrefix(msg, prefix) {
		return false
	}
	rest := strings.TrimPrefix(msg, prefix)
	codeText, _, _ := strings.Cut(rest, " ")
	code, err := strconv.Atoi(codeText)
	if err != nil {
		return false
	}
	return code >= 1 && code <= patchVerifyMaxExitCode && code != workerenv.RepositoryValidationUnavailableExitCode
}

func patchVerifyTaskName(scanName, findingID, patchTaskName, phase, commitSHA, image, command string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		scanName,
		findingID,
		patchTaskName,
		phase,
		strings.TrimSpace(commitSHA),
		strings.TrimSpace(image),
		strings.TrimSpace(command),
	}, "\n")))
	suffix := hex.EncodeToString(sum[:8])
	prefix := strings.ToLower(strings.TrimSpace(scanName)) + "-pv-" + phase + "-"
	maxPrefix := patchVerifyMaxTaskNameBytes - len(suffix)
	if len(prefix) > maxPrefix {
		prefix = strings.TrimRight(prefix[:maxPrefix], "-")
		if !strings.HasSuffix(prefix, "-") && len(prefix) < maxPrefix {
			prefix += "-"
		}
	}
	return prefix + suffix
}

func controllerVerificationArtifactName(summaryArtifact string) string {
	summaryArtifact = strings.TrimSpace(summaryArtifact)
	if summaryArtifact == "" {
		return ""
	}
	return strings.TrimSuffix(summaryArtifact, ".json") + patchVerifyArtifactSuffix
}

type patchVerificationStep struct {
	image                 string
	command               string
	requireFailBeforePass bool
	expectedBaseExitCode  int
	timeout               time.Duration
}

func effectivePatchVerificationPlans(scan *corev1alpha1.RepositoryScan, finding *store.Finding) ([]patchVerificationStep, string) {
	var plans []patchVerificationStep
	if scan != nil && scan.Spec.PatchVerification != nil {
		if errMsg := validateRepositoryScanPatchVerificationSpec(scan.Spec.PatchVerification); errMsg != "" {
			return nil, errMsg
		}
		plans = append(plans, patchVerificationStep{
			image:                 strings.TrimSpace(scan.Spec.PatchVerification.Image),
			command:               strings.TrimSpace(scan.Spec.PatchVerification.Command),
			requireFailBeforePass: scan.Spec.PatchVerification.RequireFailBeforePass,
			timeout:               patchVerifyTimeout,
		})
	}
	if pocCmd, expectedExitCode, ok := validatedFindingPoCCommand(finding); ok {
		img := effectivePoCVerificationImage(scan)
		if img == "" && scan != nil && scan.Spec.PatchVerification != nil {
			img = strings.TrimSpace(scan.Spec.PatchVerification.Image)
		}
		pocTimeout := effectivePoCVerificationTimeout(nil)
		if scan != nil && scan.Spec.PoCExecution != nil {
			pocTimeout = effectivePoCVerificationTimeout(scan.Spec.PoCExecution)
		}
		if tools.ValidRepositoryValidationImage(img) {
			duplicate := false
			for i := range plans {
				if plans[i].image == img && plans[i].command == pocCmd {
					plans[i].requireFailBeforePass = true
					plans[i].expectedBaseExitCode = expectedExitCode
					plans[i].timeout = pocTimeout
					duplicate = true
					break
				}
			}
			if !duplicate {
				plans = append(plans, patchVerificationStep{
					image:                 img,
					command:               pocCmd,
					requireFailBeforePass: true,
					expectedBaseExitCode:  expectedExitCode,
					timeout:               pocTimeout,
				})
			}
		}
	}
	return plans, ""
}

func (r *RepositoryScanReconciler) evaluateControllerPatchVerification(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	patchTask *corev1alpha1.Task,
	finding *store.Finding,
	publication securityPatchPublicationReceipt,
	verified patchVerificationResult,
) (bool, string, error) {
	plans, problem := effectivePatchVerificationPlans(scan, finding)
	if problem != "" {
		return false, problem, nil
	}
	if len(plans) == 0 {
		return false, "", nil
	}
	patchedCommit := ""
	baseCommit := ""
	if publication.publication != nil {
		patchedCommit = strings.TrimSpace(publication.publication.ExpectedCommitSHA)
		baseCommit = strings.TrimSpace(publication.publication.SourceBaselineSHA)
	}
	if patchedCommit == "" || store.ValidateGitObjectID("patched commit", patchedCommit) != nil {
		return false, "controller patch verification requires a valid published commit SHA", nil
	}
	var results []*security.PatchControllerVerification
	for _, plan := range plans {
		pending, stepVerification, stepProblem, err := r.evaluatePatchVerificationStep(ctx, scan, patchTask, finding, baseCommit, patchedCommit, plan)
		if err != nil {
			return false, "", err
		}
		if pending {
			return true, "", nil
		}
		if stepProblem != "" {
			if stepVerification != nil {
				if persistErr := r.persistControllerVerificationArtifact(ctx, patchTask, verified.summaryArtifact, stepVerification); persistErr != nil {
					return false, "", persistErr
				}
			}
			return false, stepProblem, nil
		}
		if stepVerification != nil {
			results = append(results, stepVerification)
		}
	}
	if len(results) > 0 {
		aggregate := aggregatePatchControllerVerifications(results)
		if err := r.persistControllerVerificationArtifact(ctx, patchTask, verified.summaryArtifact, aggregate); err != nil {
			return false, "", err
		}
	}
	return false, "", nil
}

func aggregatePatchControllerVerifications(results []*security.PatchControllerVerification) *security.PatchControllerVerification {
	if len(results) == 1 {
		return results[0]
	}
	last := results[len(results)-1]
	commands := make([]string, 0, len(results))
	requireFailBeforePass := false
	for _, item := range results {
		if cmd := strings.TrimSpace(item.Command); cmd != "" {
			commands = append(commands, cmd)
		}
		if item.RequireFailBeforePass {
			requireFailBeforePass = true
		}
	}
	return &security.PatchControllerVerification{
		Status:                patchVerifyStatusVerify,
		Command:               strings.Join(commands, " && "),
		RequireFailBeforePass: requireFailBeforePass,
		BaseCommit:            last.BaseCommit,
		PatchedCommit:         last.PatchedCommit,
		BaseTaskName:          last.BaseTaskName,
		PatchedTaskName:       last.PatchedTaskName,
		BasePassed:            last.BasePassed,
		PatchedPassed:         last.PatchedPassed,
	}
}

func (r *RepositoryScanReconciler) evaluatePatchVerificationStep(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	patchTask *corev1alpha1.Task,
	finding *store.Finding,
	baseCommit string,
	patchedCommit string,
	plan patchVerificationStep,
) (bool, *security.PatchControllerVerification, string, error) {
	if plan.requireFailBeforePass && (baseCommit == "" || store.ValidateGitObjectID("base commit", baseCommit) != nil) {
		return false, nil, "controller fail->pass verification requires a valid publication source baseline SHA", nil
	}

	var baseTask *corev1alpha1.Task
	var basePassed *bool
	if plan.requireFailBeforePass {
		var problem string
		var err error
		baseTask, problem, err = r.ensurePatchVerifyTask(ctx, scan, patchTask, finding, patchVerifyPhaseBase, baseCommit, plan.image, plan.command, plan.timeout, false)
		if err != nil {
			return false, nil, "", err
		}
		if problem != "" {
			return false, nil, problem, nil
		}
		if !baseTask.Status.Phase.IsTerminal() {
			return true, nil, "", nil
		}
		if baseTask.Status.Phase == corev1alpha1.TaskPhaseCancelled {
			return false, nil, "controller base verification task was cancelled", nil
		}
		passed := baseTask.Status.Phase == corev1alpha1.TaskPhaseSucceeded
		basePassed = &passed
		if passed {
			return false, &security.PatchControllerVerification{
				Status:                patchVerifyStatusFailed,
				Command:               plan.command,
				RequireFailBeforePass: true,
				BaseCommit:            baseCommit,
				PatchedCommit:         patchedCommit,
				BaseTaskName:          baseTask.Name,
				BasePassed:            basePassed,
			}, "controller fail->pass verification rejected: command already passed on unpatched base commit", nil
		}
		baseTriggered := patchVerifyBaseCommandExitedNonZero(baseTask)
		if plan.expectedBaseExitCode > 0 {
			baseTriggered = pocCommandTriggered(baseTask, plan.expectedBaseExitCode)
		}
		if !baseTriggered {
			failedVerification := &security.PatchControllerVerification{
				Status:                patchVerifyStatusFailed,
				Command:               plan.command,
				RequireFailBeforePass: true,
				BaseCommit:            baseCommit,
				PatchedCommit:         patchedCommit,
				BaseTaskName:          baseTask.Name,
			}
			if plan.expectedBaseExitCode > 0 {
				return false, failedVerification, fmt.Sprintf("controller fail->pass verification rejected: base task did not exit with expected PoC exit code %d", plan.expectedBaseExitCode), nil
			}
			return false, failedVerification, "controller fail->pass verification rejected: base task did not record a normal non-zero command exit code", nil
		}
	}

	patchedTask, problem, err := r.ensurePatchVerifyTask(ctx, scan, patchTask, finding, patchVerifyPhasePatched, patchedCommit, plan.image, plan.command, plan.timeout, true)
	if err != nil {
		return false, nil, "", err
	}
	if problem != "" {
		return false, nil, problem, nil
	}
	if !patchedTask.Status.Phase.IsTerminal() {
		return true, nil, "", nil
	}
	if patchedTask.Status.Phase == corev1alpha1.TaskPhaseCancelled {
		return false, nil, "controller patched verification task was cancelled", nil
	}
	patchedPassed := patchedTask.Status.Phase == corev1alpha1.TaskPhaseSucceeded
	verification := &security.PatchControllerVerification{
		Status:                patchVerifyStatusVerify,
		Command:               plan.command,
		RequireFailBeforePass: plan.requireFailBeforePass,
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
		return false, verification, "controller patch verification failed on patched commit", nil
	}
	return false, verification, "", nil
}

func buildDesiredPatchVerifyTask(
	scan *corev1alpha1.RepositoryScan,
	patchTask *corev1alpha1.Task,
	finding *store.Finding,
	phase string,
	commitSHA string,
	image string,
	command string,
	timeoutDuration time.Duration,
	useTargetRepo bool,
) *corev1alpha1.Task {
	if timeoutDuration <= 0 {
		timeoutDuration = patchVerifyTimeout
	}
	taskName := patchVerifyTaskName(scan.Name, finding.ID, patchTask.Name, phase, commitSHA, image, command)
	workspace := repositoryScanTaskWorkspaceAtCommit(scan, corev1alpha1.WorkspaceIntentRead, commitSHA)
	if useTargetRepo {
		if strings.TrimSpace(scan.Spec.ForkRepo) != "" {
			workspace.GitRepo = security.CanonicalRepositoryCloneURL(scan.Spec.ForkRepo)
			workspace.Branch = ""
		}
		if scan.Spec.PublicationReadCredentialRef != nil {
			workspace.ReadCredentialRef = workspaceCredentialReference(scan.Spec.PublicationReadCredentialRef)
		}
	}
	timeout := metav1.Duration{Duration: timeoutDuration}
	priority := int32(740)
	return &corev1alpha1.Task{
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
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(scan, corev1alpha1.GroupVersion.WithKind(repositoryScanResourceKind))},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:      corev1alpha1.TaskTypeContainer,
			Image:     strings.TrimSpace(image),
			Command:   []string{patchVerifyShell, "-c"},
			Args:      []string{strings.TrimSpace(command)},
			Timeout:   &timeout,
			Priority:  &priority,
			Workspace: workspace,
		},
	}
}

func patchVerifyTaskMatchesDesired(scan *corev1alpha1.RepositoryScan, existing, desired *corev1alpha1.Task) bool {
	if existing == nil || desired == nil || !metav1.IsControlledBy(existing, scan) {
		return false
	}
	for k, v := range desired.Labels {
		if existing.Labels[k] != v {
			return false
		}
	}
	if existing.Annotations[labels.AnnotationWorkspaceInitContainer] != scheduledRunLabelValue {
		return false
	}
	have := taskSpecWithServerDefaults(existing.Spec)
	want := taskSpecWithServerDefaults(desired.Spec)
	return reflect.DeepEqual(have, want)
}

func (r *RepositoryScanReconciler) ensurePatchVerifyTask(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	patchTask *corev1alpha1.Task,
	finding *store.Finding,
	phase string,
	commitSHA string,
	image string,
	command string,
	timeoutDuration time.Duration,
	useTargetRepo bool,
) (*corev1alpha1.Task, string, error) {
	desired := buildDesiredPatchVerifyTask(scan, patchTask, finding, phase, commitSHA, image, command, timeoutDuration, useTargetRepo)
	existing := &corev1alpha1.Task{}
	err := r.Get(ctx, types.NamespacedName{Namespace: scan.Namespace, Name: desired.Name}, existing)
	if err == nil {
		if !patchVerifyTaskMatchesDesired(scan, existing, desired) {
			return nil, fmt.Sprintf("controller patch verification task %s does not match expected owner or spec", desired.Name), nil
		}
		return existing, "", nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, "", err
	}
	if err := r.Create(ctx, desired); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if getErr := r.Get(ctx, types.NamespacedName{Namespace: scan.Namespace, Name: desired.Name}, existing); getErr == nil {
				if !patchVerifyTaskMatchesDesired(scan, existing, desired) {
					return nil, fmt.Sprintf("controller patch verification task %s does not match expected owner or spec", desired.Name), nil
				}
				return existing, "", nil
			}
		}
		return nil, "", err
	}
	return desired, "", nil
}

func (r *RepositoryScanReconciler) persistControllerVerificationArtifact(
	ctx context.Context,
	patchTask *corev1alpha1.Task,
	summaryArtifact string,
	verification *security.PatchControllerVerification,
) error {
	if verification == nil {
		return nil
	}
	artifactName := controllerVerificationArtifactName(summaryArtifact)
	if r.ArtifactStore != nil && artifactName != "" {
		owner := store.ArtifactOwner{
			Namespace: patchTask.Namespace,
			TaskName:  patchTask.Name,
			TaskUID:   string(patchTask.UID),
		}
		data, err := json.Marshal(verification)
		if err != nil {
			return err
		}
		if err := r.ArtifactStore.SaveArtifact(ctx, store.SaveArtifactRequest{
			Owner:       owner,
			Filename:    artifactName,
			ContentType: artifactContentTypeJSON,
			Data:        data,
		}); err != nil {
			return err
		}
	}
	switch verification.Status {
	case patchVerifyStatusVerify:
		metrics.RecordSecurityPatchVerification("controller_verified", "ok")
	case patchVerifyStatusFailed:
		metrics.RecordSecurityPatchVerification("controller_failed", "verification_failed")
	}
	return nil
}

func (r *RepositoryScanReconciler) loadControllerVerificationArtifact(
	ctx context.Context,
	patchTask *corev1alpha1.Task,
	summaryArtifact string,
) *security.PatchControllerVerification {
	artifactName := controllerVerificationArtifactName(summaryArtifact)
	if r.ArtifactStore == nil || artifactName == "" {
		return nil
	}
	owner := store.ArtifactOwner{
		Namespace: patchTask.Namespace,
		TaskName:  patchTask.Name,
		TaskUID:   string(patchTask.UID),
	}
	data, _, err := r.ArtifactStore.GetArtifact(ctx, store.ArtifactKey{Owner: owner, Filename: artifactName})
	if err != nil {
		return nil
	}
	var verification security.PatchControllerVerification
	if err := json.Unmarshal(data, &verification); err != nil {
		return nil
	}
	return &verification
}
