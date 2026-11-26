package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// Monitor audit decisions for completed repairs and the reason codes their
// failures record.
const (
	repositoryMonitorEventRepairSucceeded = "repair_succeeded"
	repositoryMonitorEventRepairFailed    = "repair_failed"

	repositoryMonitorRepairTaskNotSucceededReason = "repair_task_not_succeeded"
	repositoryMonitorRepairBranchMismatchReason   = "repair_delivery_branch_mismatch"
	repositoryMonitorRepairDeliveryFailedReason   = "repair_delivery_failed"
	repositoryMonitorUpdateBranchBaseMissing      = "update_branch_base_missing"
	repositoryMonitorRepairNotCompletedReason     = "repair_not_completed"

	repositoryMonitorEventMutationIDField = "mutationID"

	repositoryMonitorSkipReasonDraft          = "draft"
	repositoryMonitorForkPRNotWritableReason  = "fork_pr_repair_not_writable"
	repositoryMonitorIssuePhaseDisabledReason = "issue_workflow_phase_disabled"
	repositoryMonitorMissingAgentReasonPrefix = "missing_agent_"

	// repositoryMonitorRepositoryControlPathsPolicy names the fixed deny list
	// of repository control paths that no monitor setting can lift.
	repositoryMonitorRepositoryControlPathsPolicy = "builtin:repository-control-paths"
)

// recordRepositoryMonitorRepairOutcome records the decision a finished
// repair Task reached: the push it delivered, or why it delivered none. It
// runs before the repair job is settled, and its event ID is derived from
// the job and outcome, so a retried ingestion records it once.
func (r *RepositoryMonitorReconciler) recordRepositoryMonitorRepairOutcome(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, job *store.RepairJob, task *corev1alpha1.Task, commandID, mutationID string) error {
	metadata := map[string]any{
		"repairJobID":                         job.ID,
		intentField:                           job.Intent,
		eventTaskNameField:                    job.TaskName,
		repositoryMonitorEventMutationIDField: mutationID,
	}
	if commandID = strings.TrimSpace(commandID); commandID != "" {
		metadata[commandEventIDField] = commandID
	}
	runID := repositoryMonitorReviewRunID(task)
	if job.Phase == repositoryMonitorRepairPhaseSucceeded {
		metadata[repositoryMonitorFieldHeadSHA] = job.PushedSHA
		summary := fmt.Sprintf("Pull request #%d %s repair delivered %s", job.PRNumber, job.Intent, repositoryMonitorShortSHA(job.PushedSHA))
		return r.createMonitorEvent(ctx, monitor, runID, repositoryMonitorPullRequestKind, job.PRNumber, job.HeadSHA, repositoryMonitorEventRepairSucceeded, summary, metadata)
	}
	reason := repositoryMonitorRepairFailureReason(job, task)
	metadata[eventReasonField] = reason
	if reason == repositoryMonitorRepairTaskNotSucceededReason {
		metadata["taskPhase"] = string(task.Status.Phase)
	}
	summary := fmt.Sprintf("Pull request #%d %s repair failed: %s", job.PRNumber, job.Intent, reason)
	return r.createMonitorEvent(ctx, monitor, runID, repositoryMonitorPullRequestKind, job.PRNumber, job.HeadSHA, repositoryMonitorEventRepairFailed, summary, metadata)
}

// repositoryMonitorRepairFailureReason classifies why a finished repair
// delivered no push. It reads only controller-authored state, never Task or
// runtime text, so the reason is safe to show.
func repositoryMonitorRepairFailureReason(job *store.RepairJob, task *corev1alpha1.Task) string {
	if reason := repositoryMonitorWorkspacePathRejectionReason(task); reason != "" {
		return reason
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		return repositoryMonitorRepairTaskNotSucceededReason
	}
	switch lastError := strings.TrimSpace(job.LastError); {
	case strings.HasPrefix(lastError, "repair delivery branch did not match"):
		return repositoryMonitorRepairBranchMismatchReason
	case strings.HasPrefix(lastError, "repair delivery "):
		return repositoryMonitorRepairDeliveryFailedReason
	case strings.HasPrefix(lastError, "PR head does not contain the requested base revision"):
		return repositoryMonitorUpdateBranchBaseMissing
	default:
		return repositoryMonitorRepairNotCompletedReason
	}
}

// recordRepositoryMonitorUpdateBranchFailure records that a branch update
// ended without updating the branch. The event keeps a reason code; the
// mutation record it names keeps the failure detail. A mutation fails once,
// so the event is keyed by the mutation and every path that ends it, or
// replays its end, records one event.
func (r *RepositoryMonitorReconciler) recordRepositoryMonitorUpdateBranchFailure(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, run *store.MonitorRun, command *store.CommandEvent, mutation *store.GitHubMutationRecord, detail string) error {
	reason := repositoryMonitorUpdateBranchFailure
	if detail == repositoryMonitorUpdateBranchTimeoutReason {
		reason = "update_branch_timeout"
	}
	runID := ""
	if run != nil {
		runID = run.ID
	}
	metadata := map[string]any{eventReasonField: reason, repositoryMonitorEventMutationIDField: mutation.ID}
	if command != nil && strings.TrimSpace(command.ID) != "" {
		metadata[commandEventIDField] = command.ID
	}
	event, err := store.NewRepositoryMonitorEvent(store.RepositoryMonitorEventInput{
		ID: "mevt-" + mutation.ID + "-failed", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, RunID: runID,
		ItemKind: repositoryMonitorPullRequestKind, ItemNumber: mutation.TargetNumber, ItemSHA: mutation.TargetSHA,
		EventType: repositoryMonitorUpdateBranchFailure, Actor: controllerNameValue,
		Summary:  fmt.Sprintf("Pull request #%d branch update failed: %s", mutation.TargetNumber, reason),
		Metadata: metadata,
	})
	if err != nil {
		return err
	}
	return store.RecordRepositoryMonitorEvent(ctx, r.Store, event)
}

// saveBlockedRepositoryMonitorIssueImplementation saves an issue whose
// implementation was blocked before a Task existed and records the block.
func (r *RepositoryMonitorReconciler) saveBlockedRepositoryMonitorIssueImplementation(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, run *store.MonitorRun, command *store.CommandEvent, item *store.MonitorItem) error {
	if err := r.Store.UpsertMonitorItem(ctx, item); err != nil {
		return err
	}
	metadata := map[string]any{
		repositoryMonitorEventActionKindKey: repositoryMonitorIssueActionImplementation,
		eventReasonField:                    item.SkipReason,
	}
	if command != nil {
		metadata[commandEventIDField] = command.ID
	}
	runID := ""
	if run != nil {
		runID = run.ID
	}
	return r.createMonitorEvent(ctx, monitor, runID, repositoryMonitorIssueKind, item.Number, item.SnapshotDigest, "issue_action_blocked", fmt.Sprintf("Issue #%d implementation blocked: %s", item.Number, item.SkipReason), metadata)
}

func repositoryMonitorShortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// repositoryMonitorRunAttemptField names the metadata that tells the
// attempts of a retried run apart: each attempt starts the run anew.
const repositoryMonitorRunAttemptField = "attemptStartedAt"

func repositoryMonitorRunAttempt(run *store.MonitorRun) string {
	return run.StartedAt.UTC().Format(time.RFC3339Nano)
}

// repositoryMonitorEventPolicy names the RepositoryMonitor spec clause behind
// the reason in an event's metadata, with its effective value when that value
// is a scalar. It returns "" when no monitor setting decided, for example
// when GitHub state or a runtime failure did.
//
//nolint:gocyclo // One flat table keeps every reason-to-clause mapping reviewable in one place.
func repositoryMonitorEventPolicy(monitor *corev1alpha1.RepositoryMonitor, itemKind string, metadata map[string]any) string {
	if monitor == nil {
		return ""
	}
	reason, _ := metadata[eventReasonField].(string)
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ""
	}
	spec := monitor.Spec
	// The CI gate records the CI state as the skip reason.
	if ciState, _ := metadata["ciState"].(string); ciState == reason && spec.Review.RequireGreenCI {
		return "spec.review.requireGreenCI=true"
	}
	if actionKind, ok := strings.CutPrefix(reason, repositoryMonitorMissingAgentReasonPrefix); ok {
		return repositoryMonitorAgentPolicy(actionKind)
	}
	switch reason {
	case "repair_disabled":
		return "spec.repair.enabled=false"
	case "missing_repairer_agent":
		return "spec.agents.repairer"
	case repositoryMonitorForkPRNotWritableReason:
		if !spec.Repair.AllowForkPullRequests {
			return "spec.repair.allowForkPullRequests=false"
		}
	case repositoryMonitorRepairPRBudgetReason:
		return fmt.Sprintf("spec.repair.maxRepairsPerPR=%d", repositoryMonitorMaxRepairsPerPR(spec))
	case "repair_head_budget_exhausted":
		return fmt.Sprintf("spec.repair.maxRepairsPerHead=%d", repositoryMonitorMaxRepairsPerHead(spec))
	case repositoryMonitorValidationRetryExhaustedReason:
		return fmt.Sprintf("spec.repair.maxValidationRetries=%d", repositoryMonitorMaxValidationRetries(spec))
	case repositoryMonitorRepairSkipLabelActorNotPermitted:
		return "spec.triggers.github.labels.requireActorPermission=" + repositoryMonitorRequiredActorPermission(spec)
	case repositoryMonitorSkipReasonDraft:
		return "spec.targets.pullRequests.includeDrafts=false"
	case repositoryMonitorSkipReasonBlockedLabel:
		if repositoryMonitorMatchingLabel(spec.Policy.ProtectedLabels, repositoryMonitorEventLabels(metadata)) != "" {
			return "spec.policy.protectedLabels"
		}
		return "spec.policy.pauseLabels"
	case repositoryMonitorSkipReasonOverLimit:
		if itemKind == repositoryMonitorIssueKind {
			return fmt.Sprintf("spec.targets.issues.maxPerRun=%d", repositoryMonitorMaxIssuesPerRun(spec))
		}
		return fmt.Sprintf("spec.targets.pullRequests.maxPerRun=%d", repositoryMonitorMaxPullRequestsPerRun(spec))
	case repositoryMonitorSkipReasonNotIncluded:
		return "spec.targets.pullRequests.includeLabels"
	case repositoryMonitorSkipReasonCIOnly:
		return "spec.repair.ciOnlyLabels"
	case repositoryMonitorSkipReasonExcluded:
		return "spec.targets.issues.excludeLabels"
	case repositoryMonitorSkipReasonMissingLabel:
		return "spec.targets.issues.includeLabels"
	case repositoryMonitorIssuePhaseDisabledReason:
		actionKind, _ := metadata[repositoryMonitorEventActionKindKey].(string)
		return repositoryMonitorIssuePhasePolicy(actionKind)
	case "implementation_attempt_budget_exhausted":
		return fmt.Sprintf("spec.issueWorkflow.implementation.maxAttemptsPerIssue=%d", repositoryMonitorImplementationMaxAttemptsPerIssue(monitor))
	case repositoryMonitorImplementationActiveBudget:
		return fmt.Sprintf("spec.issueWorkflow.implementation.maxActive=%d", repositoryMonitorImplementationMaxActive(monitor))
	case repositoryMonitorReviewMaxRoundsReason:
		if spec.Review.MaxRounds != nil {
			return fmt.Sprintf("spec.review.maxRounds=%d", *spec.Review.MaxRounds)
		}
	case repositoryMonitorPatchPathDenied:
		return repositoryMonitorRepositoryControlPathsPolicy
	case repositoryMonitorPatchPathNotAllowed:
		return "spec.issueWorkflow.implementation.allowedPaths"
	case repositoryMonitorPublishSkipDisabled:
		return "spec.review.publish.enabled=false"
	case repositoryMonitorPublishSkipVerdictNotConfigured:
		verdict, _ := metadata[eventVerdictField].(string)
		return repositoryMonitorPublishVerdictPolicy(verdict)
	case repositoryMonitorPublishSkipSecuritySensitiveNotPublic:
		return "spec.review.publish.postSecuritySensitive=false"
	}
	return ""
}

// repositoryMonitorPublishVerdictPolicy names the publish flag that turned
// off posting a review with verdict.
func repositoryMonitorPublishVerdictPolicy(verdict string) string {
	switch strings.TrimSpace(verdict) {
	case repositoryMonitorReviewVerdictPassed:
		return "spec.review.publish.postPassed=false"
	case repositoryMonitorReviewVerdictNeedsChanges:
		return "spec.review.publish.postNeedsChanges=false"
	case repositoryMonitorReviewVerdictNeedsHuman:
		return "spec.review.publish.postNeedsHuman=false"
	default:
		return ""
	}
}

func repositoryMonitorAgentPolicy(actionKind string) string {
	switch actionKind {
	case repositoryMonitorIssueActionTriage:
		return "spec.agents.triager"
	case repositoryMonitorIssueActionResearch:
		return "spec.agents.researcher"
	case repositoryMonitorIssueActionPlan, repositoryMonitorIssueActionDecompose:
		return "spec.agents.planner"
	case repositoryMonitorIssueActionImplementation:
		return "spec.agents.implementer"
	default:
		return ""
	}
}

func repositoryMonitorIssuePhasePolicy(actionKind string) string {
	switch actionKind {
	case repositoryMonitorIssueActionTriage:
		return "spec.issueWorkflow.triage.enabled=false"
	case repositoryMonitorIssueActionResearch:
		return "spec.issueWorkflow.research.enabled=false"
	case repositoryMonitorIssueActionPlan, repositoryMonitorIssueActionDecompose:
		return "spec.issueWorkflow.planning.enabled=false"
	case repositoryMonitorIssueActionImplementation:
		return "spec.issueWorkflow.implementation.enabled=false"
	default:
		return ""
	}
}

func repositoryMonitorEventLabels(metadata map[string]any) []string {
	switch labels := metadata[objectLabelsField].(type) {
	case []string:
		return labels
	case []any:
		out := make([]string, 0, len(labels))
		for _, label := range labels {
			if value, ok := label.(string); ok {
				out = append(out, value)
			}
		}
		return out
	default:
		return nil
	}
}

func repositoryMonitorMaxRepairsPerPR(spec corev1alpha1.RepositoryMonitorSpec) int {
	if spec.Repair.MaxRepairsPerPR != nil {
		return int(*spec.Repair.MaxRepairsPerPR)
	}
	return 5
}

func repositoryMonitorMaxRepairsPerHead(spec corev1alpha1.RepositoryMonitorSpec) int {
	if spec.Repair.MaxRepairsPerHead != nil {
		return int(*spec.Repair.MaxRepairsPerHead)
	}
	return 2
}

func repositoryMonitorMaxValidationRetries(spec corev1alpha1.RepositoryMonitorSpec) int {
	if spec.Repair.MaxValidationRetries != nil {
		return int(*spec.Repair.MaxValidationRetries)
	}
	return 2
}

func repositoryMonitorRequiredActorPermission(spec corev1alpha1.RepositoryMonitorSpec) string {
	if permission := strings.ToLower(strings.TrimSpace(spec.Triggers.GitHub.Labels.RequireActorPermission)); permission != "" {
		return permission
	}
	return githubPermissionWrite
}
