package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/workerenv"
)

const (
	repositoryMonitorDefaultGitHubAPIBaseURL = "https://api.github.com"
	repositoryMonitorPullRequestKind         = "pull_request"
	repositoryMonitorTokenKey                = "token"
	repositoryMonitorPasswordKey             = "password"
	repositoryMonitorGitHubPerPage           = 50
	repositoryMonitorGitHubResponseLimit     = 10 << 20

	repositoryMonitorItemStateOpen       = "open"
	repositoryMonitorItemStateOutOfScope = "out_of_scope"
	repositoryMonitorVerdictSkipped      = "skipped"
	repositoryMonitorSkipReasonMissing   = "not_open_or_base_branch_changed"
	repositoryMonitorSkipReasonReviewed  = "already_reviewed"
	repositoryMonitorSkipReasonPending   = "review_pending"
	repositoryMonitorSkipReasonOverLimit = "over_limit"
	repositoryMonitorReviewTaskTimeout   = 2 * time.Hour

	repositoryMonitorReviewTaskStateMissing   = "missing"
	repositoryMonitorReviewTaskStatePending   = "pending"
	repositoryMonitorReviewTaskStateSucceeded = "succeeded"
	repositoryMonitorReviewTaskStateRetryable = "retryable"
)

type repositoryMonitorPullRequest struct {
	Number         int64
	Title          string
	Author         string
	State          string
	Labels         []string
	BaseBranch     string
	HeadBranch     string
	HeadRepo       string
	HeadRepoURL    string
	BaseSHA        string
	HeadSHA        string
	Draft          bool
	MergeableState string
}

func (r *RepositoryMonitorReconciler) processPullRequestInventoryRun(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, run *store.MonitorRun, owner, repository string) (int, int, int, error) {
	if err := validateRepositoryMonitorRunTargetKind(run); err != nil {
		return 0, 0, 0, err
	}
	if !repositoryMonitorPullRequestsEnabled(monitor.Spec) {
		return 0, 0, 0, r.createMonitorEvent(ctx, monitor, run.ID, repositoryMonitorPullRequestKind, 0, "", "inventory_skipped", "Pull request monitoring is disabled", nil)
	}

	token, err := r.repositoryMonitorGitHubToken(ctx, monitor)
	if err != nil {
		return 0, 0, 0, err
	}

	baseBranch := effectiveRepositoryMonitorBranch(monitor)
	pullRequests, err := r.listRepositoryMonitorPullRequestsForRun(ctx, owner, repository, token, baseBranch, run)
	if err != nil {
		return 0, 0, 0, err
	}
	pullRequests = filterRepositoryMonitorBasePullRequests(pullRequests, baseBranch)
	seenPullRequestKeys := repositoryMonitorPullRequestKeys(pullRequests)
	pullRequests = filterRepositoryMonitorTargetPullRequests(pullRequests, run)
	slices.SortFunc(pullRequests, func(a, b repositoryMonitorPullRequest) int {
		return int(a.Number - b.Number)
	})

	maxPerRun := repositoryMonitorMaxPullRequestsPerRun(monitor.Spec)
	includeDrafts := monitor.Spec.Targets.PullRequests.IncludeDrafts
	selected := 0
	createdTasks := 0
	skipped := 0

	for _, pr := range pullRequests {
		existing, err := r.Store.GetMonitorItem(ctx, monitor.Namespace, monitor.Name, repositoryMonitorPullRequestKind, fmt.Sprintf("%d", pr.Number))
		if err != nil && !errorsIsStoreNotFound(err) {
			return selected, createdTasks, skipped, err
		}
		item := repositoryMonitorItemFromPullRequest(monitor, pr, existing)
		if handled, created, err := r.tryProcessPullRequestCommandRun(ctx, monitor, run, owner, repository, pr, item); err != nil {
			return selected, createdTasks, skipped, err
		} else if handled {
			selected++
			createdTasks += created
			continue
		}
		skipExisting := existing
		if repositoryMonitorPendingReviewCandidate(existing, pr) {
			taskState, err := r.repositoryMonitorReviewTaskState(ctx, monitor.Namespace, existing.LastReviewID)
			if err != nil {
				return selected, createdTasks, skipped, err
			}
			switch taskState {
			case repositoryMonitorReviewTaskStateMissing, repositoryMonitorReviewTaskStateRetryable:
				existingWithoutTask := *existing
				existingWithoutTask.LastReviewID = ""
				skipExisting = &existingWithoutTask
				item.LastReviewID = ""
			}
		}

		skipReason := repositoryMonitorPullRequestSkipReason(monitor.Spec, pr, skipExisting, includeDrafts, selected, maxPerRun, run)
		if skipReason == repositoryMonitorSkipReasonReviewed {
			fresh, err := r.repositoryMonitorReviewedHeadFresh(ctx, monitor, existing, pr.HeadSHA)
			if err != nil {
				return selected, createdTasks, skipped, err
			}
			if !fresh {
				if selected >= maxPerRun {
					skipReason = repositoryMonitorSkipReasonOverLimit
				} else {
					skipReason = ""
				}
			}
		}
		if skipReason != "" {
			skipped++
			switch skipReason {
			case repositoryMonitorSkipReasonPending:
				item.LastVerdict = repositoryMonitorRunPhaseQueued
			case repositoryMonitorSkipReasonReviewed:
				verdict, err := r.repositoryMonitorExistingReviewVerdict(ctx, monitor, existing, pr.HeadSHA)
				if err != nil {
					return selected, createdTasks, skipped, err
				}
				item.LastVerdict = verdict
			default:
				item.LastVerdict = repositoryMonitorVerdictSkipped
			}
			item.SkipReason = skipReason
			if err := r.Store.UpsertMonitorItem(ctx, item); err != nil {
				return selected, createdTasks, skipped, err
			}
			if err := r.createMonitorEvent(ctx, monitor, run.ID, repositoryMonitorPullRequestKind, pr.Number, pr.HeadSHA, "item_skipped", fmt.Sprintf("Pull request #%d skipped: %s", pr.Number, skipReason), map[string]any{
				"reason": skipReason,
				"labels": pr.Labels,
			}); err != nil {
				return selected, createdTasks, skipped, err
			}
			continue
		}

		selected++
		taskName, created, err := r.createRepositoryMonitorReviewTask(ctx, monitor, run, owner, repository, pr)
		if err != nil {
			return selected, createdTasks, skipped, err
		}
		if created {
			createdTasks++
		}
		item.LastVerdict = "queued"
		item.LastReviewID = taskName
		item.SkipReason = ""
		if err := r.Store.UpsertMonitorItem(ctx, item); err != nil {
			return selected, createdTasks, skipped, err
		}
		if err := r.createMonitorEvent(ctx, monitor, run.ID, repositoryMonitorPullRequestKind, pr.Number, pr.HeadSHA, "review_task_created", fmt.Sprintf("Pull request #%d review task queued", pr.Number), map[string]any{
			"taskName": taskName,
			"created":  created,
		}); err != nil {
			return selected, createdTasks, skipped, err
		}
		if err := r.createMonitorEvent(ctx, monitor, run.ID, repositoryMonitorPullRequestKind, pr.Number, pr.HeadSHA, "item_selected", fmt.Sprintf("Pull request #%d selected for review", pr.Number), nil); err != nil {
			return selected, createdTasks, skipped, err
		}
	}

	if repositoryMonitorRunCoversFullInventory(run) {
		if err := r.retireMissingRepositoryMonitorPullRequests(ctx, monitor, run, seenPullRequestKeys); err != nil {
			return selected, createdTasks, skipped, err
		}
	}

	return selected, createdTasks, skipped, nil
}

func repositoryMonitorPullRequestKeys(pullRequests []repositoryMonitorPullRequest) map[string]struct{} {
	keys := make(map[string]struct{}, len(pullRequests))
	for _, pr := range pullRequests {
		keys[fmt.Sprintf("%d", pr.Number)] = struct{}{}
	}
	return keys
}

func repositoryMonitorRunCoversFullInventory(run *store.MonitorRun) bool {
	return run == nil || (run.TargetNumber == 0 && strings.TrimSpace(run.TargetSHA) == "")
}

func (r *RepositoryMonitorReconciler) createRepositoryMonitorReviewTask(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, run *store.MonitorRun, owner, repository string, pr repositoryMonitorPullRequest) (string, bool, error) {
	taskName := repositoryMonitorReviewTaskName(monitor, run, pr)
	timeout := metav1.Duration{Duration: repositoryMonitorReviewTaskTimeout}
	priority := repositoryMonitorReviewTaskPriority(run)
	reviewer := *monitor.Spec.Agents.Reviewer
	repoFullName := owner + "/" + repository
	prNumber := strconv.FormatInt(pr.Number, 10)
	workspaceRepo, gitSecretRef := repositoryMonitorReviewTaskGitSource(monitor, owner, repository, pr)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskName,
			Namespace: monitor.Namespace,
			Labels: map[string]string{
				labels.LabelManaged:           "true",
				labels.LabelCreatedBy:         "repository-monitor",
				labels.LabelRepositoryMonitor: labels.SelectorValue(monitor.Name),
				labels.LabelMonitorRun:        labels.SelectorValue(run.ID),
				labels.LabelGitHubRepository:  labels.SelectorValue(repoFullName),
				labels.LabelGitHubTarget:      labels.SelectorValue(repositoryMonitorPullRequestKind),
				labels.LabelGitHubNumber:      labels.SelectorValue(prNumber),
			},
			Annotations: map[string]string{
				labels.AnnotationRepositoryMonitorName:  monitor.Name,
				labels.AnnotationMonitorRunID:           run.ID,
				labels.AnnotationMonitorItemKind:        repositoryMonitorPullRequestKind,
				labels.AnnotationMonitorItemNumber:      prNumber,
				labels.AnnotationMonitorHeadSHA:         pr.HeadSHA,
				labels.AnnotationGitHubRepository:       repoFullName,
				labels.AnnotationAgentReadOnly:          "true",
				labels.AnnotationWorkspaceInitContainer: "true",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAgent,
			AgentRef: &reviewer,
			Prompt:   buildRepositoryMonitorReviewPrompt(monitor, owner, repository, pr),
			Timeout:  &timeout,
			Priority: &priority,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				AllowedTools: readOnlyAgentAllowedTools(),
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo:      workspaceRepo,
					Ref:          pr.HeadSHA,
					GitSecretRef: gitSecretRef,
					PRBaseBranch: pr.BaseBranch,
				},
			},
			Env: []corev1.EnvVar{
				{Name: workerenv.PRBaseRepo, Value: repositoryMonitorHTTPSCloneURL(owner, repository)},
				{Name: workerenv.PRBaseSHA, Value: pr.BaseSHA},
			},
		},
	}
	if err := controllerutil.SetControllerReference(monitor, task, r.Scheme); err != nil {
		return "", false, err
	}
	if err := r.Create(ctx, task); err != nil {
		if apierrors.IsAlreadyExists(err) {
			var existing corev1alpha1.Task
			if getErr := r.Get(ctx, types.NamespacedName{Namespace: monitor.Namespace, Name: taskName}, &existing); getErr != nil {
				return "", false, getErr
			}
			if bindingErr := validateRepositoryMonitorReviewTaskMatchesExpected(&existing, task, monitor, run, repoFullName, pr); bindingErr != nil {
				return "", false, bindingErr
			}
			return taskName, false, nil
		}
		return "", false, err
	}
	return taskName, true, nil
}

func validateRepositoryMonitorReviewTaskMatchesExpected(existing, expected *corev1alpha1.Task, monitor *corev1alpha1.RepositoryMonitor, run *store.MonitorRun, repoFullName string, pr repositoryMonitorPullRequest) error {
	if existing == nil || expected == nil {
		return fmt.Errorf("repository monitor review task is required")
	}
	if err := validateRepositoryMonitorReviewTaskRunBinding(existing, monitor, run, repoFullName, pr); err != nil {
		return err
	}
	if existing.Name != expected.Name || existing.Namespace != expected.Namespace {
		return fmt.Errorf("existing review task %s/%s does not match expected task %s/%s", existing.Namespace, existing.Name, expected.Namespace, expected.Name)
	}
	if !reflect.DeepEqual(existing.Labels, expected.Labels) {
		return fmt.Errorf("existing review task %s/%s labels do not match the expected repository monitor review task", existing.Namespace, existing.Name)
	}
	if !reflect.DeepEqual(existing.Annotations, expected.Annotations) {
		return fmt.Errorf("existing review task %s/%s annotations do not match the expected repository monitor review task", existing.Namespace, existing.Name)
	}
	if !reflect.DeepEqual(existing.OwnerReferences, expected.OwnerReferences) {
		return fmt.Errorf("existing review task %s/%s owner references do not match the expected repository monitor review task", existing.Namespace, existing.Name)
	}
	if !reflect.DeepEqual(repositoryMonitorComparableReviewTaskSpec(existing.Spec), repositoryMonitorComparableReviewTaskSpec(expected.Spec)) {
		return fmt.Errorf("existing review task %s/%s spec does not match the expected repository monitor review task", existing.Namespace, existing.Name)
	}
	return nil
}

func repositoryMonitorComparableReviewTaskSpec(spec corev1alpha1.TaskSpec) corev1alpha1.TaskSpec {
	spec.ConcurrencyPolicy = ""
	spec.StartingDeadlineSeconds = nil
	spec.SuccessfulRunsHistoryLimit = nil
	spec.FailedRunsHistoryLimit = nil
	return spec
}

func validateRepositoryMonitorReviewTaskRunBinding(task *corev1alpha1.Task, monitor *corev1alpha1.RepositoryMonitor, run *store.MonitorRun, repoFullName string, pr repositoryMonitorPullRequest) error {
	if task == nil {
		return fmt.Errorf("repository monitor review task is required")
	}
	if err := validateRepositoryMonitorReviewTaskItemBinding(task, monitor, repositoryMonitorPullRequestKind, pr.Number); err != nil {
		return err
	}
	if got, want := strings.TrimSpace(task.Annotations[labels.AnnotationMonitorRunID]), strings.TrimSpace(run.ID); got != want {
		return fmt.Errorf("existing review task %s/%s has monitor run %q, want %q", task.Namespace, task.Name, got, want)
	}
	if got, want := strings.TrimSpace(task.Annotations[labels.AnnotationMonitorHeadSHA]), strings.TrimSpace(pr.HeadSHA); got != want {
		return fmt.Errorf("existing review task %s/%s has head SHA %q, want %q", task.Namespace, task.Name, got, want)
	}
	if got, want := strings.TrimSpace(task.Annotations[labels.AnnotationGitHubRepository]), strings.TrimSpace(repoFullName); !strings.EqualFold(got, want) {
		return fmt.Errorf("existing review task %s/%s has repository %q, want %q", task.Namespace, task.Name, got, want)
	}
	return nil
}

func validateRepositoryMonitorReviewTaskItemBinding(task *corev1alpha1.Task, monitor *corev1alpha1.RepositoryMonitor, itemKind string, itemNumber int64) error {
	if task == nil {
		return fmt.Errorf("repository monitor review task is required")
	}
	if monitor == nil {
		return fmt.Errorf("repository monitor is required")
	}
	if task.Namespace != monitor.Namespace {
		return fmt.Errorf("review task %s/%s is not in monitor namespace %q", task.Namespace, task.Name, monitor.Namespace)
	}
	if got, want := strings.TrimSpace(task.Annotations[labels.AnnotationRepositoryMonitorName]), monitor.Name; got != want {
		return fmt.Errorf("review task %s/%s has monitor %q, want %q", task.Namespace, task.Name, got, want)
	}
	if got, want := strings.TrimSpace(task.Annotations[labels.AnnotationMonitorItemKind]), itemKind; got != want {
		return fmt.Errorf("review task %s/%s has item kind %q, want %q", task.Namespace, task.Name, got, want)
	}
	if got, want := strings.TrimSpace(task.Annotations[labels.AnnotationMonitorItemNumber]), strconv.FormatInt(itemNumber, 10); got != want {
		return fmt.Errorf("review task %s/%s has item number %q, want %q", task.Namespace, task.Name, got, want)
	}
	if !metav1.IsControlledBy(task, monitor) {
		return fmt.Errorf("review task %s/%s is not controlled by repository monitor %s/%s", task.Namespace, task.Name, monitor.Namespace, monitor.Name)
	}
	return nil
}

func (r *RepositoryMonitorReconciler) repositoryMonitorReviewTaskState(ctx context.Context, namespace, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return repositoryMonitorReviewTaskStateMissing, nil
	}
	var task corev1alpha1.Task
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &task); err != nil {
		if apierrors.IsNotFound(err) {
			return repositoryMonitorReviewTaskStateMissing, nil
		}
		return "", err
	}
	switch task.Status.Phase {
	case "", corev1alpha1.TaskPhasePending, corev1alpha1.TaskPhaseRunning, corev1alpha1.TaskPhaseScheduled:
		return repositoryMonitorReviewTaskStatePending, nil
	case corev1alpha1.TaskPhaseSucceeded:
		return repositoryMonitorReviewTaskStateSucceeded, nil
	default:
		return repositoryMonitorReviewTaskStateRetryable, nil
	}
}

func repositoryMonitorPendingReviewCandidate(existing *store.MonitorItem, pr repositoryMonitorPullRequest) bool {
	return existing != nil &&
		existing.LastVerdict == repositoryMonitorRunPhaseQueued &&
		existing.HeadSHA != "" &&
		existing.HeadSHA == pr.HeadSHA &&
		existing.LastReviewID != ""
}

func repositoryMonitorReviewTaskGitSource(monitor *corev1alpha1.RepositoryMonitor, owner, repository string, pr repositoryMonitorPullRequest) (string, *corev1.LocalObjectReference) {
	monitoredRepo := strings.ToLower(owner + "/" + repository)
	if strings.EqualFold(strings.TrimSpace(pr.HeadRepo), monitoredRepo) {
		return repositoryMonitorHTTPSCloneURL(owner, repository), monitor.Spec.GitSecretRef
	}
	if strings.TrimSpace(pr.HeadRepoURL) == "" {
		return repositoryMonitorHTTPSCloneURL(owner, repository), nil
	}
	return pr.HeadRepoURL, nil
}

func repositoryMonitorHTTPSCloneURL(owner, repository string) string {
	return fmt.Sprintf("https://github.com/%s/%s", strings.TrimSpace(owner), strings.TrimSpace(repository))
}

func repositoryMonitorReviewTaskPriority(run *store.MonitorRun) int32 {
	if run != nil && (run.TargetNumber != 0 || strings.TrimSpace(run.TargetSHA) != "") {
		return 800
	}
	return 700
}

func repositoryMonitorReviewTaskName(monitor *corev1alpha1.RepositoryMonitor, run *store.MonitorRun, pr repositoryMonitorPullRequest) string {
	return repositoryMonitorBoundedDNSName(fmt.Sprintf("monrev-%s-%d-%s-%s", monitor.Name, pr.Number, pr.HeadSHA, run.ID), 63)
}

func buildRepositoryMonitorReviewPrompt(monitor *corev1alpha1.RepositoryMonitor, owner, repository string, pr repositoryMonitorPullRequest) string {
	payload := map[string]any{
		"schemaVersion":  "orka.prReview.input.v1",
		"repoURL":        monitor.Spec.RepoURL,
		"repo":           owner + "/" + repository,
		"prNumber":       pr.Number,
		"title":          pr.Title,
		"author":         pr.Author,
		"baseBranch":     pr.BaseBranch,
		"baseSHA":        pr.BaseSHA,
		"headBranch":     pr.HeadBranch,
		"headRepo":       pr.HeadRepo,
		"headRepoURL":    pr.HeadRepoURL,
		"headSHA":        pr.HeadSHA,
		"labels":         pr.Labels,
		"draft":          pr.Draft,
		"mergeableState": pr.MergeableState,
		"review": map[string]any{
			"event": monitor.Spec.Review.Event,
		},
		"policy": map[string]any{
			"protectedLabels": monitor.Spec.Policy.ProtectedLabels,
			"pauseLabels":     monitor.Spec.Policy.PauseLabels,
		},
		"validation": map[string]any{
			"mode":     monitor.Spec.Validation.Mode,
			"commands": monitor.Spec.Validation.Commands,
		},
	}
	payloadJSON, _ := json.MarshalIndent(payload, "", "  ")
	return fmt.Sprintf(`Review this exact pull request head for correctness, tests, security, and maintainability.

Do not post comments, push commits, merge, close, label, or otherwise mutate GitHub. Produce only the JSON review result described below.

The workspace is checked out at the pull request head SHA. Review the generated diff context first:
- /workspace/.git/orka/pr-review.md
- /workspace/.git/orka/pr-review.files
- /workspace/.git/orka/pr-review.diff

Input:
%s

Return one JSON object with this shape:
{
  "schemaVersion": "orka.prReview.v1",
  "repo": %q,
  "prNumber": %d,
  "headSHA": %q,
  "verdict": "passed|needs_changes|needs_human|security_sensitive|skipped",
  "confidence": "low|medium|high",
  "repairable": false,
  "summary": "Short maintainer-facing summary.",
  "findings": [
    {
      "priority": "P0|P1|P2|P3",
      "confidence": "low|medium|high",
      "file": "path/to/file",
      "line": 1,
      "title": "Finding title",
      "body": "Why this is a bug.",
      "recommendation": "What should change."
    }
  ],
  "security": {
    "status": "clear|needs_human|security_sensitive",
    "notes": ""
  },
  "tests": {
    "status": "not_run|passed|failed",
    "evidence": ""
  },
  "suggestedComment": "Draft prose. Orka may render or ignore this."
}

The headSHA in the output must be exactly %q. If you cannot evaluate this exact head, return verdict "skipped" and explain why in summary.
`, string(payloadJSON), owner+"/"+repository, pr.Number, pr.HeadSHA, pr.HeadSHA)
}

func repositoryMonitorBoundedDNSName(value string, maxLength int) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	lastHyphen := false
	for _, r := range value {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if valid {
			builder.WriteRune(r)
			lastHyphen = false
			continue
		}
		if !lastHyphen {
			builder.WriteByte('-')
			lastHyphen = true
		}
	}
	normalized := strings.Trim(builder.String(), "-")
	if normalized == "" {
		normalized = "monitor"
	}
	if len(normalized) <= maxLength {
		return normalized
	}
	hash := repositoryMonitorShortHash(value)
	maxPrefix := max(1, maxLength-len(hash)-1)
	prefix := strings.Trim(normalized[:maxPrefix], "-")
	if prefix == "" {
		prefix = "monitor"
	}
	return prefix + "-" + hash
}

func repositoryMonitorShortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:10]
}

func (r *RepositoryMonitorReconciler) listRepositoryMonitorPullRequestItems(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor) ([]store.MonitorItem, error) {
	var allItems []store.MonitorItem
	cursor := ""
	for {
		items, next, err := r.Store.ListMonitorItems(ctx, store.MonitorItemFilter{
			Namespace:   monitor.Namespace,
			MonitorName: monitor.Name,
			Kind:        repositoryMonitorPullRequestKind,
			Limit:       200,
			Cursor:      cursor,
		})
		if err != nil {
			return nil, err
		}
		allItems = append(allItems, items...)
		if next == "" {
			return allItems, nil
		}
		cursor = next
	}
}

func (r *RepositoryMonitorReconciler) retireMissingRepositoryMonitorPullRequests(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, run *store.MonitorRun, seenKeys map[string]struct{}) error {
	items, err := r.listRepositoryMonitorPullRequestItems(ctx, monitor)
	if err != nil {
		return err
	}
	for i := range items {
		item := items[i]
		if _, ok := seenKeys[item.ItemKey]; ok {
			continue
		}
		if item.State == repositoryMonitorItemStateOutOfScope && item.SkipReason == repositoryMonitorSkipReasonMissing {
			continue
		}
		item.State = repositoryMonitorItemStateOutOfScope
		item.LastVerdict = repositoryMonitorVerdictSkipped
		item.SkipReason = repositoryMonitorSkipReasonMissing
		if err := r.Store.UpsertMonitorItem(ctx, &item); err != nil {
			return err
		}
		if err := r.createMonitorEvent(ctx, monitor, run.ID, repositoryMonitorPullRequestKind, item.Number, item.HeadSHA, "item_retired", fmt.Sprintf("Pull request #%d is no longer in the open base-branch inventory", item.Number), map[string]any{
			"reason": repositoryMonitorSkipReasonMissing,
			"state":  item.State,
		}); err != nil {
			return err
		}
	}
	return nil
}

func validateRepositoryMonitorSupportedTargets(spec corev1alpha1.RepositoryMonitorSpec) error {
	if spec.Targets.Commits.Enabled {
		return fmt.Errorf("spec.targets.commits is not supported; only pull request and issue monitoring are supported")
	}
	if !repositoryMonitorPullRequestsEnabled(spec) && !spec.Targets.Issues.Enabled {
		return fmt.Errorf("at least one repository monitor target must be enabled")
	}
	if spec.Review.RequireGreenCI {
		return fmt.Errorf("spec.review.requireGreenCI is not supported until repository monitor CI state collection is available")
	}
	return nil
}

func validateRepositoryMonitorRunTargetKind(run *store.MonitorRun) error {
	if run == nil {
		return nil
	}
	switch strings.TrimSpace(run.TargetKind) {
	case "", repositoryMonitorPullRequestKind, repositoryMonitorIssueKind:
		return nil
	default:
		return fmt.Errorf("targetKind %q is not supported; supported values are pull_request and issue", run.TargetKind)
	}
}

func repositoryMonitorPullRequestsEnabled(spec corev1alpha1.RepositoryMonitorSpec) bool {
	return spec.Targets.PullRequests.Enabled == nil || *spec.Targets.PullRequests.Enabled
}

func repositoryMonitorMaxPullRequestsPerRun(spec corev1alpha1.RepositoryMonitorSpec) int {
	if spec.Targets.PullRequests.MaxPerRun == nil || *spec.Targets.PullRequests.MaxPerRun <= 0 {
		return 20
	}
	return int(*spec.Targets.PullRequests.MaxPerRun)
}

func filterRepositoryMonitorBasePullRequests(pullRequests []repositoryMonitorPullRequest, baseBranch string) []repositoryMonitorPullRequest {
	baseBranch = strings.TrimSpace(baseBranch)
	if baseBranch == "" {
		return pullRequests
	}
	filtered := make([]repositoryMonitorPullRequest, 0, len(pullRequests))
	for _, pr := range pullRequests {
		if pr.BaseBranch == baseBranch {
			filtered = append(filtered, pr)
		}
	}
	return filtered
}

func filterRepositoryMonitorTargetPullRequests(pullRequests []repositoryMonitorPullRequest, run *store.MonitorRun) []repositoryMonitorPullRequest {
	targetSHA := ""
	if run != nil {
		targetSHA = strings.TrimSpace(run.TargetSHA)
	}
	if run == nil || (run.TargetNumber == 0 && targetSHA == "") {
		return pullRequests
	}
	filtered := make([]repositoryMonitorPullRequest, 0, len(pullRequests))
	for _, pr := range pullRequests {
		if run.TargetNumber != 0 {
			if pr.Number != run.TargetNumber {
				continue
			}
			if targetSHA == "" || pr.HeadSHA == targetSHA {
				filtered = append(filtered, pr)
			}
			break
		}
		if targetSHA != "" && pr.HeadSHA == targetSHA {
			filtered = append(filtered, pr)
		}
	}
	return filtered
}

func repositoryMonitorPullRequestSkipReason(spec corev1alpha1.RepositoryMonitorSpec, pr repositoryMonitorPullRequest, existing *store.MonitorItem, includeDrafts bool, selected, maxPerRun int, run *store.MonitorRun) string {
	if !includeDrafts && pr.Draft {
		return "draft"
	}
	if strings.TrimSpace(pr.HeadSHA) == "" {
		return "missing_head_sha"
	}
	if run != nil && run.TargetSHA != "" && pr.HeadSHA != "" && run.TargetSHA != pr.HeadSHA {
		return "head_sha_mismatch"
	}
	if blockedLabel := repositoryMonitorBlockedLabel(spec, pr.Labels); blockedLabel != "" {
		return "blocked_label"
	}
	if existing != nil && existing.HeadSHA != "" && existing.HeadSHA == pr.HeadSHA && existing.LastReviewID != "" {
		if existing.LastVerdict == repositoryMonitorRunPhaseQueued {
			return repositoryMonitorSkipReasonPending
		}
	}
	if existing != nil && existing.LastReviewedHeadSHA != "" && existing.LastReviewedHeadSHA == pr.HeadSHA {
		return repositoryMonitorSkipReasonReviewed
	}
	if selected >= maxPerRun {
		return repositoryMonitorSkipReasonOverLimit
	}
	return ""
}

func (r *RepositoryMonitorReconciler) repositoryMonitorReviewedHeadFresh(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, existing *store.MonitorItem, headSHA string) (bool, error) {
	if existing == nil || strings.TrimSpace(existing.LastReviewedHeadSHA) == "" || existing.LastReviewedHeadSHA != headSHA {
		return false, nil
	}
	ttl := monitor.Spec.Review.StaleReviewTTL
	if ttl == nil || ttl.Duration <= 0 {
		return true, nil
	}
	records, _, err := r.Store.ListReviewRecords(ctx, store.ReviewRecordFilter{
		Namespace:   monitor.Namespace,
		MonitorName: monitor.Name,
		Kind:        repositoryMonitorPullRequestKind,
		Number:      existing.Number,
		HeadSHA:     headSHA,
		Limit:       25,
	})
	if err != nil {
		return false, err
	}
	var freshSince time.Time
	for _, record := range records {
		if repositoryMonitorReviewVerdictMarksHeadFresh(record.Verdict) && !record.CreatedAt.IsZero() {
			freshSince = record.CreatedAt
			break
		}
	}
	if freshSince.IsZero() {
		return false, nil
	}
	return time.Since(freshSince) < ttl.Duration, nil
}

func (r *RepositoryMonitorReconciler) repositoryMonitorExistingReviewVerdict(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, existing *store.MonitorItem, headSHA string) (string, error) {
	if existing == nil {
		return repositoryMonitorVerdictSkipped, nil
	}
	records, _, err := r.Store.ListReviewRecords(ctx, store.ReviewRecordFilter{
		Namespace:   monitor.Namespace,
		MonitorName: monitor.Name,
		Kind:        repositoryMonitorPullRequestKind,
		Number:      existing.Number,
		HeadSHA:     headSHA,
		Limit:       1,
	})
	if err != nil {
		return "", err
	}
	if len(records) > 0 {
		if verdict := strings.TrimSpace(records[0].Verdict); verdict != "" {
			return verdict, nil
		}
	}
	if verdict := strings.TrimSpace(existing.LastVerdict); verdict != "" {
		return verdict, nil
	}
	return repositoryMonitorVerdictSkipped, nil
}

func repositoryMonitorBlockedLabel(spec corev1alpha1.RepositoryMonitorSpec, prLabels []string) string {
	blocked := map[string]struct{}{}
	for _, label := range append(spec.Policy.ProtectedLabels, spec.Policy.PauseLabels...) {
		label = strings.ToLower(strings.TrimSpace(label))
		if label != "" {
			blocked[label] = struct{}{}
		}
	}
	for _, label := range prLabels {
		if _, ok := blocked[strings.ToLower(strings.TrimSpace(label))]; ok {
			return label
		}
	}
	return ""
}

func repositoryMonitorItemFromPullRequest(monitor *corev1alpha1.RepositoryMonitor, pr repositoryMonitorPullRequest, existing *store.MonitorItem) *store.MonitorItem {
	labelsJSON, _ := json.Marshal(pr.Labels)
	item := &store.MonitorItem{
		MonitorNamespace: monitor.Namespace,
		MonitorName:      monitor.Name,
		Kind:             repositoryMonitorPullRequestKind,
		ItemKey:          fmt.Sprintf("%d", pr.Number),
		Number:           pr.Number,
		Title:            pr.Title,
		Body:             "",
		HTMLURL:          "",
		Author:           pr.Author,
		State:            pr.State,
		LabelsJSON:       string(labelsJSON),
		BaseBranch:       pr.BaseBranch,
		HeadBranch:       pr.HeadBranch,
		HeadSHA:          pr.HeadSHA,
		BaseSHA:          pr.BaseSHA,
		Draft:            pr.Draft,
		MergeableState:   pr.MergeableState,
		CIState:          "unknown",
	}
	if existing != nil {
		item.LastReviewID = existing.LastReviewID
		item.LastReviewedHeadSHA = existing.LastReviewedHeadSHA
		item.RepairState = existing.RepairState
		item.AutomergeState = existing.AutomergeState
		item.StatusCommentID = existing.StatusCommentID
		item.StatusCommentURL = existing.StatusCommentURL
		if strings.TrimSpace(existing.HeadSHA) != "" && existing.HeadSHA == pr.HeadSHA {
			item.LastPublishID = existing.LastPublishID
			item.LastPublishPhase = existing.LastPublishPhase
			item.LastPublishReason = existing.LastPublishReason
			item.LastPublishURL = existing.LastPublishURL
		}
	}
	return item
}

func (r *RepositoryMonitorReconciler) repositoryMonitorGitHubToken(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor) (string, error) {
	if monitor.Spec.GitSecretRef == nil || strings.TrimSpace(monitor.Spec.GitSecretRef.Name) == "" {
		return "", nil
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: monitor.Spec.GitSecretRef.Name, Namespace: monitor.Namespace}, &secret); err != nil {
		return "", fmt.Errorf("failed to get repository monitor git secret %q: %w", monitor.Spec.GitSecretRef.Name, err)
	}
	for _, key := range []string{repositoryMonitorTokenKey, repositoryMonitorPasswordKey, workerenv.GitHubToken} {
		if value := strings.TrimSpace(string(secret.Data[key])); value != "" {
			return value, nil
		}
	}
	return "", fmt.Errorf("repository monitor git secret %q must contain a token, password, or %s key", monitor.Spec.GitSecretRef.Name, workerenv.GitHubToken)
}

func (r *RepositoryMonitorReconciler) listRepositoryMonitorPullRequests(ctx context.Context, owner, repository, token, baseBranch string) ([]repositoryMonitorPullRequest, error) {
	var pullRequests []repositoryMonitorPullRequest
	for page := 1; ; page++ {
		pageItems, err := r.fetchRepositoryMonitorPullRequestPage(ctx, owner, repository, token, baseBranch, page)
		if err != nil {
			return nil, err
		}
		pullRequests = append(pullRequests, pageItems...)
		if len(pageItems) < repositoryMonitorGitHubPerPage {
			break
		}
	}
	return pullRequests, nil
}

func (r *RepositoryMonitorReconciler) listRepositoryMonitorPullRequestsForRun(ctx context.Context, owner, repository, token, baseBranch string, run *store.MonitorRun) ([]repositoryMonitorPullRequest, error) {
	if run != nil && run.TargetNumber > 0 {
		pr, err := r.fetchRepositoryMonitorPullRequest(ctx, owner, repository, token, run.TargetNumber)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(strings.TrimSpace(pr.State), "open") {
			return nil, nil
		}
		return []repositoryMonitorPullRequest{*pr}, nil
	}
	return r.listRepositoryMonitorPullRequests(ctx, owner, repository, token, baseBranch)
}

func (r *RepositoryMonitorReconciler) fetchRepositoryMonitorPullRequestPage(ctx context.Context, owner, repository, token, baseBranch string, page int) ([]repositoryMonitorPullRequest, error) {
	baseURL := strings.TrimRight(r.GitHubAPIBaseURL, "/")
	if baseURL == "" {
		baseURL = repositoryMonitorDefaultGitHubAPIBaseURL
	}
	query := url.Values{}
	query.Set("state", "open")
	query.Set("per_page", strconv.Itoa(repositoryMonitorGitHubPerPage))
	query.Set("page", strconv.Itoa(page))
	if strings.TrimSpace(baseBranch) != "" {
		query.Set("base", strings.TrimSpace(baseBranch))
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls?%s", baseURL, url.PathEscape(owner), url.PathEscape(repository), query.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	httpClient := r.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub pull request inventory request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	respBody, err := readRepositoryMonitorGitHubResponse(resp.Body, repositoryMonitorGitHubResponseLimit)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &repositoryMonitorGitHubAPIError{Operation: "pull request inventory", StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	var response []repositoryMonitorPullRequestResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to parse GitHub pull request inventory response: %w", err)
	}
	return repositoryMonitorPullRequestsFromGitHub(response), nil
}

func (r *RepositoryMonitorReconciler) fetchRepositoryMonitorPullRequest(ctx context.Context, owner, repository, token string, number int64) (*repositoryMonitorPullRequest, error) {
	baseURL := strings.TrimRight(r.GitHubAPIBaseURL, "/")
	if baseURL == "" {
		baseURL = repositoryMonitorDefaultGitHubAPIBaseURL
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", baseURL, url.PathEscape(owner), url.PathEscape(repository), number)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	httpClient := r.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub pull request request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	respBody, err := readRepositoryMonitorGitHubResponse(resp.Body, repositoryMonitorGitHubResponseLimit)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &repositoryMonitorGitHubAPIError{Operation: "pull request request", StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	var response repositoryMonitorPullRequestResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to parse GitHub pull request response: %w", err)
	}
	pr := repositoryMonitorPullRequestFromGitHub(response)
	return &pr, nil
}

type repositoryMonitorPullRequestResponse struct {
	Number         int64  `json:"number"`
	Title          string `json:"title"`
	State          string `json:"state"`
	Draft          bool   `json:"draft"`
	MergeableState string `json:"mergeable_state"`
	User           struct {
		Login string `json:"login"`
	} `json:"user"`
	Base struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
			CloneURL string `json:"clone_url"`
		} `json:"repo"`
	} `json:"base"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
			CloneURL string `json:"clone_url"`
		} `json:"repo"`
	} `json:"head"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func repositoryMonitorPullRequestsFromGitHub(response []repositoryMonitorPullRequestResponse) []repositoryMonitorPullRequest {
	items := make([]repositoryMonitorPullRequest, 0, len(response))
	for _, pr := range response {
		items = append(items, repositoryMonitorPullRequestFromGitHub(pr))
	}
	return items
}

func repositoryMonitorPullRequestFromGitHub(pr repositoryMonitorPullRequestResponse) repositoryMonitorPullRequest {
	labelNames := make([]string, 0, len(pr.Labels))
	for _, label := range pr.Labels {
		labelNames = append(labelNames, label.Name)
	}
	return repositoryMonitorPullRequest{
		Number:         pr.Number,
		Title:          pr.Title,
		Author:         pr.User.Login,
		State:          pr.State,
		Labels:         labelNames,
		BaseBranch:     pr.Base.Ref,
		HeadBranch:     pr.Head.Ref,
		HeadRepo:       pr.Head.Repo.FullName,
		HeadRepoURL:    pr.Head.Repo.CloneURL,
		BaseSHA:        pr.Base.SHA,
		HeadSHA:        pr.Head.SHA,
		Draft:          pr.Draft,
		MergeableState: pr.MergeableState,
	}
}

func readRepositoryMonitorGitHubResponse(body io.Reader, limit int64) ([]byte, error) {
	respBody, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read GitHub pull request inventory response: %w", err)
	}
	if int64(len(respBody)) > limit {
		return nil, fmt.Errorf("GitHub pull request inventory response exceeded %d bytes", limit)
	}
	return respBody, nil
}

func (r *RepositoryMonitorReconciler) createMonitorEvent(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, runID, itemKind string, itemNumber int64, itemSHA, eventType, summary string, metadata map[string]any) error {
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	return r.Store.CreateMonitorEvent(ctx, &store.MonitorEvent{
		ID:               "mevt-" + uuid.NewString(),
		MonitorNamespace: monitor.Namespace,
		MonitorName:      monitor.Name,
		RunID:            runID,
		ItemKind:         itemKind,
		ItemNumber:       itemNumber,
		ItemSHA:          itemSHA,
		EventType:        eventType,
		Actor:            "controller",
		Summary:          summary,
		MetadataJSON:     string(metadataJSON),
	})
}
