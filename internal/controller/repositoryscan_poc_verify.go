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
	"slices"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/internal/workerenv"
)

const (
	pocVerifyPhasePoC        = "poc"
	pocVerifyPhaseControl    = "control"
	pocVerifyDefaultTimeout  = 5 * time.Minute
	pocVerifyMaxTimeout      = 30 * time.Minute
	pocVerifyStatusVerified  = "poc_verified"
	pocVerifyStatusFailed    = "poc_failed"
	pocVerifyStatusSkipped   = "poc_skipped"
	pocVerifyMaxTaskNameSize = 63
)

func pocExecutionPolicyDigest(scan *corev1alpha1.RepositoryScan) string {
	if scan == nil || scan.Spec.PoCExecution == nil {
		return ""
	}
	allowed := make([]string, 0, len(scan.Spec.PoCExecution.AllowedHarnesses))
	for _, h := range scan.Spec.PoCExecution.AllowedHarnesses {
		if trimmed := strings.ToLower(strings.TrimSpace(h)); trimmed != "" {
			allowed = append(allowed, trimmed)
		}
	}
	slices.Sort(allowed)
	sum := sha256.Sum256([]byte(strings.Join([]string{
		effectivePoCVerificationImage(scan),
		effectivePoCVerificationTimeout(scan.Spec.PoCExecution).String(),
		strings.Join(allowed, ","),
	}, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:8])
}

func preservedValidationPoCVerification(scan *corev1alpha1.RepositoryScan, finding *store.Finding, artifact *security.ValidationArtifact) *security.ValidationPoCVerification {
	if finding == nil || artifact == nil || strings.TrimSpace(finding.ValidationJSON) == "" {
		return nil
	}
	var prior security.ValidationArtifact
	if err := json.Unmarshal([]byte(finding.ValidationJSON), &prior); err != nil {
		return nil
	}
	if prior.PoCVerification == nil || strings.TrimSpace(prior.PoCVerification.Status) == "" {
		return nil
	}
	if strings.TrimSpace(prior.FindingID) != strings.TrimSpace(artifact.FindingID) {
		return nil
	}
	if prior.PoCVerification.CommitSHA != "" && strings.TrimSpace(finding.CommitSHA) != "" &&
		strings.TrimSpace(prior.PoCVerification.CommitSHA) != strings.TrimSpace(finding.CommitSHA) {
		return nil
	}
	if !reflect.DeepEqual(prior.PoC, artifact.PoC) {
		return nil
	}
	if scan != nil && scan.Spec.PoCExecution != nil {
		currentDigest := pocExecutionPolicyDigest(scan)
		if prior.PoCVerification.PolicyDigest != "" && prior.PoCVerification.PolicyDigest != currentDigest {
			return nil
		}
		if prior.PoCVerification.Status == pocVerifyStatusSkipped && prior.PoCVerification.PolicyDigest != currentDigest {
			return nil
		}
	}
	return prior.PoCVerification
}

func isRepositoryScanPoCVerifyTask(task *corev1alpha1.Task) bool {
	if task == nil || task.Spec.Type != corev1alpha1.TaskTypeContainer {
		return false
	}
	if task.Labels[labels.LabelCreatedBy] != repositorySecurityTaskCreator ||
		task.Labels[labels.LabelSecurityStage] != security.StagePoCVerify {
		return false
	}
	owner := metav1.GetControllerOf(task)
	return owner != nil &&
		owner.APIVersion == corev1alpha1.GroupVersion.String() &&
		owner.Kind == repositoryScanResourceKind
}

func effectivePoCVerificationImage(scan *corev1alpha1.RepositoryScan) string {
	if scan == nil || scan.Spec.PoCExecution == nil {
		return ""
	}
	if img := strings.TrimSpace(scan.Spec.PoCExecution.Image); img != "" {
		return img
	}
	if scan.Spec.PatchVerification != nil {
		return strings.TrimSpace(scan.Spec.PatchVerification.Image)
	}
	return ""
}

func effectivePoCVerificationTimeout(spec *corev1alpha1.RepositoryScanPoCExecutionSpec) time.Duration {
	if spec != nil && spec.Timeout != nil && spec.Timeout.Duration > 0 {
		if spec.Timeout.Duration > pocVerifyMaxTimeout {
			return pocVerifyMaxTimeout
		}
		return spec.Timeout.Duration
	}
	return pocVerifyDefaultTimeout
}

func pocHarnessAllowed(spec *corev1alpha1.RepositoryScanPoCExecutionSpec, harness string) bool {
	harness = strings.ToLower(strings.TrimSpace(harness))
	if !security.ValidPoCHarness(harness) {
		return false
	}
	if spec == nil || len(spec.AllowedHarnesses) == 0 {
		return true
	}
	for _, allowed := range spec.AllowedHarnesses {
		if strings.ToLower(strings.TrimSpace(allowed)) == harness {
			return true
		}
	}
	return false
}

func validateRepositoryScanPoCExecutionSpec(scan *corev1alpha1.RepositoryScan) string {
	if scan == nil || scan.Spec.PoCExecution == nil {
		return ""
	}
	image := effectivePoCVerificationImage(scan)
	if !tools.ValidRepositoryValidationImage(image) {
		return "controller PoC verification policy is invalid: image must be a digest-pinned OCI reference"
	}
	for _, harness := range scan.Spec.PoCExecution.AllowedHarnesses {
		if !security.ValidPoCHarness(harness) {
			return fmt.Sprintf("controller PoC verification policy is invalid: unsupported harness %q", harness)
		}
	}
	if scan.Spec.PoCExecution.Timeout != nil && scan.Spec.PoCExecution.Timeout.Duration < time.Second {
		return "controller PoC verification policy is invalid: timeout must be at least 1s"
	}
	return ""
}

func pocCommandMatchesHarness(harness, command string) bool {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(harness)) {
	case security.PoCHarnessGoTest:
		if strings.ContainsAny(trimmed, ";|&`$()<>") {
			return false
		}
		fields := strings.Fields(trimmed)
		return len(fields) >= 2 && fields[0] == "go" && fields[1] == "test"
	case security.PoCHarnessPytest:
		if strings.ContainsAny(trimmed, ";|&`$()<>") {
			return false
		}
		fields := strings.Fields(trimmed)
		return (len(fields) >= 1 && fields[0] == "pytest") ||
			(len(fields) >= 3 && (fields[0] == "python" || fields[0] == "python3") && fields[1] == "-m" && fields[2] == "pytest")
	case security.PoCHarnessShell:
		return true
	default:
		return false
	}
}

func pocVerifyTaskName(scanName, findingID, validationTaskName, phase, commitSHA, image, command string, timeout time.Duration) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		scanName,
		findingID,
		validationTaskName,
		phase,
		strings.TrimSpace(commitSHA),
		strings.TrimSpace(image),
		strings.TrimSpace(command),
		timeout.String(),
	}, "\n")))
	suffix := hex.EncodeToString(sum[:8])
	prefix := strings.ToLower(strings.TrimSpace(scanName)) + "-poc-" + phase + "-"
	maxPrefix := pocVerifyMaxTaskNameSize - len(suffix)
	if len(prefix) > maxPrefix {
		prefix = strings.TrimRight(prefix[:maxPrefix], "-")
		if !strings.HasSuffix(prefix, "-") && len(prefix) < maxPrefix {
			prefix += "-"
		}
	}
	return prefix + suffix
}

func pocTaskCommandExitCode(task *corev1alpha1.Task) (int, bool) {
	if task == nil || task.Status.Phase != corev1alpha1.TaskPhaseFailed {
		return 0, false
	}
	msg := strings.TrimSpace(task.Status.Message)
	const prefix = "job failed: container exited with code "
	if !strings.HasPrefix(msg, prefix) {
		return 0, false
	}
	if strings.Contains(msg, "reason=StartError") || strings.Contains(msg, "reason=ContainerCannotRun") || strings.Contains(msg, "Evicted") {
		return 0, false
	}
	rest := strings.TrimPrefix(msg, prefix)
	codeText, _, _ := strings.Cut(rest, " ")
	code, err := strconv.Atoi(codeText)
	if err != nil || code < 1 || code > patchVerifyMaxExitCode || code == workerenv.RepositoryValidationUnavailableExitCode {
		return 0, false
	}
	return code, true
}

func (r *RepositoryScanReconciler) pocTaskWorkerPodExecuted(ctx context.Context, task *corev1alpha1.Task) bool {
	if task == nil || strings.TrimSpace(task.Status.JobName) == "" || r.Client == nil {
		return true
	}
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Status.JobName}, job); err != nil {
		return true
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(task.Namespace), client.MatchingLabels{
		batchv1.JobNameLabel: job.Name,
	}); err != nil || len(pods.Items) == 0 {
		return true
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !metav1.IsControlledBy(pod, job) {
			continue
		}
		if repositoryMonitorValidationPodDisrupted(pod) {
			return false
		}
		for j := range pod.Status.ContainerStatuses {
			status := &pod.Status.ContainerStatuses[j]
			if status.State.Terminated != nil {
				return repositoryMonitorValidationTerminationExecuted(status.State.Terminated)
			}
		}
	}
	return false
}

func pocCommandTriggered(task *corev1alpha1.Task, expectedExitCode int) bool {
	code, ok := pocTaskCommandExitCode(task)
	if !ok {
		return false
	}
	if expectedExitCode > 0 {
		return code == expectedExitCode
	}
	return true
}

func buildDesiredPoCVerifyTask(
	scan *corev1alpha1.RepositoryScan,
	validationTask *corev1alpha1.Task,
	finding *store.Finding,
	phase string,
	commitSHA string,
	image string,
	command string,
) *corev1alpha1.Task {
	timeoutDur := effectivePoCVerificationTimeout(scan.Spec.PoCExecution)
	taskName := pocVerifyTaskName(scan.Name, finding.ID, validationTask.Name, phase, commitSHA, image, command, timeoutDur)
	workspace := repositoryScanTaskWorkspaceAtCommit(scan, corev1alpha1.WorkspaceIntentRead, commitSHA)
	timeout := metav1.Duration{Duration: timeoutDur}
	priority := int32(730)
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
				labels.LabelSecurityStage:     security.StagePoCVerify,
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

func pocVerifyTaskMatchesDesired(scan *corev1alpha1.RepositoryScan, existing, desired *corev1alpha1.Task) bool {
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

func (r *RepositoryScanReconciler) ensurePoCVerifyTask(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	validationTask *corev1alpha1.Task,
	finding *store.Finding,
	phase string,
	commitSHA string,
	image string,
	command string,
) (*corev1alpha1.Task, string, error) {
	desired := buildDesiredPoCVerifyTask(scan, validationTask, finding, phase, commitSHA, image, command)
	existing := &corev1alpha1.Task{}
	err := r.Get(ctx, types.NamespacedName{Namespace: scan.Namespace, Name: desired.Name}, existing)
	if err == nil {
		if !pocVerifyTaskMatchesDesired(scan, existing, desired) {
			return nil, fmt.Sprintf("controller PoC verification task %s does not match expected owner or spec", desired.Name), nil
		}
		return existing, "", nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, "", err
	}
	if err := r.Create(ctx, desired); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if getErr := r.Get(ctx, types.NamespacedName{Namespace: scan.Namespace, Name: desired.Name}, existing); getErr == nil {
				if !pocVerifyTaskMatchesDesired(scan, existing, desired) {
					return nil, fmt.Sprintf("controller PoC verification task %s does not match expected owner or spec", desired.Name), nil
				}
				return existing, "", nil
			}
		}
		return nil, "", err
	}
	return desired, "", nil
}

func (r *RepositoryScanReconciler) evaluateControllerPoCVerification(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	validationTask *corev1alpha1.Task,
	finding *store.Finding,
	artifact *security.ValidationArtifact,
) (bool, *security.ValidationPoCVerification, error) {
	if scan == nil || scan.Spec.PoCExecution == nil || artifact == nil || artifact.PoC == nil {
		return false, nil, nil
	}
	if strings.TrimSpace(artifact.Status) != findingValidationStatusValidated {
		return false, nil, nil
	}
	poc := artifact.PoC
	policyDigest := pocExecutionPolicyDigest(scan)
	if problem := validateRepositoryScanPoCExecutionSpec(scan); problem != "" {
		return false, &security.ValidationPoCVerification{
			Status:       pocVerifyStatusSkipped,
			Harness:      poc.Harness,
			PolicyDigest: policyDigest,
			Reason:       problem,
		}, nil
	}
	if !pocHarnessAllowed(scan.Spec.PoCExecution, poc.Harness) {
		return false, &security.ValidationPoCVerification{
			Status:       pocVerifyStatusSkipped,
			Harness:      poc.Harness,
			PolicyDigest: policyDigest,
			Reason:       fmt.Sprintf("PoC harness %q is not permitted by spec.pocExecution.allowedHarnesses", poc.Harness),
		}, nil
	}
	if !pocCommandMatchesHarness(poc.Harness, poc.Command) ||
		(strings.TrimSpace(poc.ControlCommand) != "" && !pocCommandMatchesHarness(poc.Harness, poc.ControlCommand)) {
		return false, &security.ValidationPoCVerification{
			Status:       pocVerifyStatusFailed,
			Harness:      poc.Harness,
			PolicyDigest: policyDigest,
			Reason:       fmt.Sprintf("PoC command does not match declared harness %q", poc.Harness),
		}, nil
	}
	if poc.ExpectedExitCode <= 0 && strings.TrimSpace(poc.ControlCommand) == "" {
		return false, &security.ValidationPoCVerification{
			Status:       pocVerifyStatusFailed,
			Harness:      poc.Harness,
			PolicyDigest: policyDigest,
			Reason:       "PoC verification requires either expected_exit_code or control_command to avoid accepting arbitrary failures",
		}, nil
	}
	commitSHA := strings.TrimSpace(finding.CommitSHA)
	if commitSHA == "" || store.ValidateGitObjectID("finding commit", commitSHA) != nil {
		return false, &security.ValidationPoCVerification{
			Status:       pocVerifyStatusSkipped,
			Harness:      poc.Harness,
			PolicyDigest: policyDigest,
			Reason:       "controller PoC verification requires a valid finding commit SHA",
		}, nil
	}
	image := effectivePoCVerificationImage(scan)

	pocTask, problem, err := r.ensurePoCVerifyTask(ctx, scan, validationTask, finding, pocVerifyPhasePoC, commitSHA, image, poc.Command)
	if err != nil {
		return false, nil, err
	}
	if problem != "" {
		return false, &security.ValidationPoCVerification{
			Status:       pocVerifyStatusFailed,
			Harness:      poc.Harness,
			CommitSHA:    commitSHA,
			PolicyDigest: policyDigest,
			Reason:       problem,
		}, nil
	}
	if !pocTask.Status.Phase.IsTerminal() {
		return true, nil, nil
	}
	if pocTask.Status.Phase == corev1alpha1.TaskPhaseCancelled {
		return false, &security.ValidationPoCVerification{
			Status:       pocVerifyStatusFailed,
			Harness:      poc.Harness,
			CommitSHA:    commitSHA,
			PolicyDigest: policyDigest,
			PoCTaskName:  pocTask.Name,
			Reason:       "controller PoC verification task was cancelled",
		}, nil
	}
	triggered := pocCommandTriggered(pocTask, poc.ExpectedExitCode) && r.pocTaskWorkerPodExecuted(ctx, pocTask)
	result := &security.ValidationPoCVerification{
		Status:       pocVerifyStatusVerified,
		Harness:      poc.Harness,
		CommitSHA:    commitSHA,
		PolicyDigest: policyDigest,
		PoCTaskName:  pocTask.Name,
		PoCTriggered: &triggered,
	}
	if !triggered {
		result.Status = pocVerifyStatusFailed
		if pocTask.Status.Phase == corev1alpha1.TaskPhaseSucceeded {
			result.Reason = "PoC command exited 0 on vulnerable commit instead of reproducing the failure"
		} else {
			result.Reason = "PoC command did not exit with the expected non-zero code on vulnerable commit"
		}
		return false, result, nil
	}

	if strings.TrimSpace(poc.ControlCommand) != "" {
		controlTask, problem, err := r.ensurePoCVerifyTask(ctx, scan, validationTask, finding, pocVerifyPhaseControl, commitSHA, image, poc.ControlCommand)
		if err != nil {
			return false, nil, err
		}
		if problem != "" {
			result.Status = pocVerifyStatusFailed
			result.Reason = problem
			return false, result, nil
		}
		if !controlTask.Status.Phase.IsTerminal() {
			return true, nil, nil
		}
		result.ControlTaskName = controlTask.Name
		controlPassed := controlTask.Status.Phase == corev1alpha1.TaskPhaseSucceeded
		result.ControlPassed = &controlPassed
		if !controlPassed {
			result.Status = pocVerifyStatusFailed
			result.Reason = "PoC benign control command failed on the pinned commit"
			return false, result, nil
		}
	}

	return false, result, nil
}
