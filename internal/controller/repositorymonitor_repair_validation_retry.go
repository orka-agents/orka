/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

// Pre-push repair validation retries (ADR 0038). A repair whose patch fails
// spec.validation.repairCommand is never pushed; the monitor continues the
// repair session with a new Task that carries the failure evidence, bounded by
// spec.repair.maxValidationRetries. The same budget also counts review
// validation failures after a repair is pushed.
const (
	// repositoryMonitorRepairValidationAttemptAnnotation records how many
	// earlier attempts of the same repair job failed pre-push validation.
	// The first repair Task has none, which means zero.
	repositoryMonitorRepairValidationAttemptAnnotation = "orka.ai/repair-validation-attempt"
	// repositoryMonitorRepairValidationOriginalTaskAnnotation names the repair
	// job's first Task, from which every retry name is derived.
	repositoryMonitorRepairValidationOriginalTaskAnnotation = "orka.ai/repair-validation-original-task"

	repositoryMonitorRepairValidationFailedError      = "repair_validation_failed"
	repositoryMonitorRepairValidationUnavailableError = "repair_validation_unavailable"
	repositoryMonitorRepairValidationRetryEvent       = "repair_validation_retry"

	repositoryMonitorRepairValidationRetryNameSuffix = "-rv"
	repositoryMonitorRepairValidationFeedbackStart   = "\n\n[Orka pre-push validation feedback]\n"
	repositoryMonitorRepairValidationFeedbackEnd     = "\n[end of Orka pre-push validation feedback]"
	repositoryMonitorRepairValidationCommandLimit    = 1024
)

var repositoryMonitorRepairValidationRetryNamePattern = regexp.MustCompile(`-rv([1-9][0-9]*)$`)

func repositoryMonitorMaxValidationRetries(monitor *corev1alpha1.RepositoryMonitor) int {
	if monitor != nil && monitor.Spec.Repair.MaxValidationRetries != nil {
		return int(*monitor.Spec.Repair.MaxValidationRetries)
	}
	return 2
}

// repositoryMonitorRepairValidationDeliveryReason returns the pre-push gate
// reason a failed repair Task ended with, or "".
func repositoryMonitorRepairValidationDeliveryReason(task *corev1alpha1.Task) string {
	if task == nil || task.Status.Phase != corev1alpha1.TaskPhaseFailed || task.Status.Delivery == nil {
		return ""
	}
	switch reason := string(task.Status.Delivery.Reason); reason {
	case repositoryMonitorRepairValidationFailedDeliveryReason, repositoryMonitorRepairValidationUnavailableDeliveryReason:
		return reason
	default:
		return ""
	}
}

// repositoryMonitorRepairTaskPriorValidationFailures returns how many earlier
// attempts of task's repair job failed pre-push validation.
func repositoryMonitorRepairTaskPriorValidationFailures(task *corev1alpha1.Task) int {
	if task == nil {
		return 0
	}
	value, err := strconv.Atoi(strings.TrimSpace(task.Annotations[repositoryMonitorRepairValidationAttemptAnnotation]))
	if err != nil || value < 0 {
		return 0
	}
	return value
}

// repositoryMonitorRepairJobPrePushValidationFailures returns the pre-push
// validation failures recorded before the job's current Task. A retry name
// carries the same count, so the budget survives a deleted Task.
func (r *RepositoryMonitorReconciler) repositoryMonitorRepairJobPrePushValidationFailures(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, job *store.RepairJob) (int, error) {
	if job == nil || strings.TrimSpace(job.TaskName) == "" {
		return 0, nil
	}
	var task corev1alpha1.Task
	err := r.Get(ctx, types.NamespacedName{Namespace: monitor.Namespace, Name: job.TaskName}, &task)
	switch {
	case err == nil:
		if metav1.IsControlledBy(&task, monitor) {
			return repositoryMonitorRepairTaskPriorValidationFailures(&task), nil
		}
	case !apierrors.IsNotFound(err):
		return 0, err
	}
	if match := repositoryMonitorRepairValidationRetryNamePattern.FindStringSubmatch(job.TaskName); match != nil {
		value, convErr := strconv.Atoi(match[1])
		if convErr == nil {
			return value, nil
		}
	}
	return 0, nil
}

// repositoryMonitorRepairValidationRetryTaskName derives the deterministic
// Task name for the attempt that follows failures pre-push failures.
func repositoryMonitorRepairValidationRetryTaskName(original string, failures int) string {
	suffix := repositoryMonitorRepairValidationRetryNameSuffix + strconv.Itoa(failures)
	return repositoryMonitorBoundedDNSName(original, 63-len(suffix)) + suffix
}

// repositoryMonitorRepairPromptWithValidationFeedback replaces any earlier
// feedback section with one for this failure. The section sits before the
// prompt's input marker so the structured input stays parseable.
func repositoryMonitorRepairPromptWithValidationFeedback(prompt, command, evidence string, failures, maxRetries int) string {
	prompt = repositoryMonitorRepairPromptWithoutValidationFeedback(prompt)
	var section strings.Builder
	section.WriteString(repositoryMonitorRepairValidationFeedbackStart)
	fmt.Fprintf(&section, "Your previous repair attempt did not pass Orka's pre-push validation (failed attempt %d; at most %d retries). Nothing was pushed to the pull request.\n", failures, maxRetries)
	if command = strings.TrimSpace(command); command != "" {
		fmt.Fprintf(&section, "Orka ran the operator's validation command offline against the pull request head with your patch applied:\n%s\n", boundedString(command, repositoryMonitorRepairValidationCommandLimit))
	} else {
		section.WriteString("Orka ran the operator's validation command offline against the pull request head with your patch applied.\n")
	}
	if evidence = boundRepositoryMonitorValidationEvidence(evidence); evidence != "" {
		fmt.Fprintf(&section, "Validation evidence (command output is not shared):\n%s\n", evidence)
	}
	section.WriteString("Redo the repair from the current pull request head. Before you finish, make sure that command passes; the next attempt is validated the same way.")
	section.WriteString(repositoryMonitorRepairValidationFeedbackEnd)
	if preamble, payload, ok := strings.Cut(prompt, repositoryMonitorRepairPromptInputMarker); ok {
		return preamble + section.String() + repositoryMonitorRepairPromptInputMarker + payload
	}
	return prompt + section.String()
}

func repositoryMonitorRepairPromptWithoutValidationFeedback(prompt string) string {
	before, rest, ok := strings.Cut(prompt, repositoryMonitorRepairValidationFeedbackStart)
	if !ok {
		return prompt
	}
	_, after, ok := strings.Cut(rest, repositoryMonitorRepairValidationFeedbackEnd)
	if !ok {
		return before
	}
	return before + after
}

// retryRepositoryMonitorRepairAfterValidation continues a repair whose patch
// failed pre-push validation. It reports false when the budget is exhausted,
// leaving the caller to fail the job with repair_validation_failed.
func (r *RepositoryMonitorReconciler) retryRepositoryMonitorRepairAfterValidation(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, job *store.RepairJob, previous *corev1alpha1.Task) (bool, error) {
	failures := repositoryMonitorRepairTaskPriorValidationFailures(previous) + 1
	job.ValidationAttempts = failures
	maxRetries := repositoryMonitorMaxValidationRetries(monitor)
	if failures > maxRetries {
		return false, nil
	}
	original := strings.TrimSpace(previous.Annotations[repositoryMonitorRepairValidationOriginalTaskAnnotation])
	if original == "" {
		original = previous.Name
	}
	evidence := ""
	if previous.Status.Delivery != nil {
		evidence = previous.Status.Delivery.Message
	}
	annotations := maps.Clone(previous.Annotations)
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[repositoryMonitorRepairValidationAttemptAnnotation] = strconv.Itoa(failures)
	annotations[repositoryMonitorRepairValidationOriginalTaskAnnotation] = original
	retry := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:        repositoryMonitorRepairValidationRetryTaskName(original, failures),
			Namespace:   previous.Namespace,
			Labels:      maps.Clone(previous.Labels),
			Annotations: annotations,
		},
		Spec: *previous.Spec.DeepCopy(),
	}
	retry.Spec.Prompt = repositoryMonitorRepairPromptWithValidationFeedback(
		previous.Spec.Prompt, monitor.Spec.Validation.RepairCommand, evidence, failures, maxRetries,
	)
	if err := controllerutil.SetControllerReference(monitor, retry, r.Scheme); err != nil {
		return false, err
	}
	usageWorkID, err := r.prepareMonitorUsageWork(ctx, monitor, job.Repo, repositoryMonitorPullRequestKind, job.PRNumber)
	if err != nil {
		return false, err
	}
	if err := r.Create(ctx, retry); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return false, err
		}
		var existing corev1alpha1.Task
		if err := r.Get(ctx, types.NamespacedName{Namespace: retry.Namespace, Name: retry.Name}, &existing); err != nil {
			return false, err
		}
		if err := validateRepositoryMonitorRecoveredIssueActionTask(monitor, retry, &existing); err != nil {
			return false, err
		}
		retry = &existing
	}
	if err := r.retainMonitorUsageTask(ctx, retry, usageWorkID, "repair", job.PRNumber); err != nil {
		return false, err
	}
	job.TaskName = retry.Name
	job.Phase = repositoryMonitorRepairPhaseQueued
	job.LastError = ""
	job.CompletedAt = nil
	if err := r.Store.UpdateRepairJob(ctx, job); err != nil {
		return false, err
	}
	item, err := r.Store.GetMonitorItem(ctx, monitor.Namespace, monitor.Name, repositoryMonitorPullRequestKind, strconv.FormatInt(job.PRNumber, 10))
	switch {
	case err == nil:
		if item.HeadSHA == job.HeadSHA {
			item.RepairState = repositoryMonitorRepairPhaseQueued
			if err := r.Store.UpsertMonitorItem(ctx, item); err != nil {
				return false, err
			}
		}
	case !errorsIsStoreNotFound(err):
		return false, err
	}
	metadata := map[string]any{
		eventTaskNameField:     retry.Name,
		"previousTaskName":     previous.Name,
		"validationFailures":   failures,
		"maxValidationRetries": maxRetries,
		"evidence":             boundRepositoryMonitorValidationEvidence(evidence),
	}
	return true, r.createMonitorEvent(ctx, monitor, previous.Annotations[labels.AnnotationMonitorRunID], repositoryMonitorPullRequestKind, job.PRNumber, job.HeadSHA,
		repositoryMonitorRepairValidationRetryEvent,
		fmt.Sprintf("Pull request #%d repair failed pre-push validation; retry %d of %d queued", job.PRNumber, failures, maxRetries), metadata)
}

// repositoryMonitorRepairValidationReadinessDescription explains a failed
// pre-push repair gate for the current head, or returns "".
func (r *RepositoryMonitorReconciler) repositoryMonitorRepairValidationReadinessDescription(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, number int64, head string) (string, error) {
	if r == nil || r.Store == nil || monitor == nil {
		return "", nil
	}
	var latest *store.RepairJob
	cursor := ""
	for {
		jobs, next, err := r.Store.ListRepairJobs(ctx, store.RepairJobFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, PRNumber: number, Limit: 200, Cursor: cursor})
		if err != nil {
			return "", err
		}
		for i := range jobs {
			job := jobs[i]
			if job.HeadSHA != head || job.Phase != repositoryMonitorRepairPhaseFailed {
				continue
			}
			if latest == nil || job.CreatedAt.After(latest.CreatedAt) {
				latest = &job
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if latest == nil {
		return "", nil
	}
	switch latest.LastError {
	case repositoryMonitorRepairValidationFailedError:
		attempts := max(latest.ValidationAttempts, 1)
		noun := "attempts"
		if attempts == 1 {
			noun = "attempt"
		}
		return fmt.Sprintf("Repair did not pass spec.validation.repairCommand after %d %s; nothing was pushed.", attempts, noun), nil
	case repositoryMonitorRepairValidationUnavailableError:
		return "Repair validation could not run; nothing was pushed.", nil
	default:
		return "", nil
	}
}
