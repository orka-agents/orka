package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/workerenv"
)

const (
	githubAPIBaseURLEnv      = "ORKA_GITHUB_API_BASE_URL"
	commandIntentStop        = finishReasonStop
	commandIntentResume      = "resume"
	commandIntentApprovePlan = "approve_plan"
)

func (h *Handlers) handleRepositoryMonitorLabelCommand(c fiber.Ctx, body []byte, delivery string, payload githubLabelWebhookPayload, target githubLabelTarget) (githubRepositoryMonitorEventResult, bool, error) {
	var result githubRepositoryMonitorEventResult
	if h.repositoryMonitorStore == nil {
		return result, false, nil
	}
	monitors := &corev1alpha1.RepositoryMonitorList{}
	if err := h.client.List(c.Context(), monitors, h.githubWebhookMonitorListOptions()...); err != nil {
		return result, false, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list repository monitors: %v", err))
	}
	if delivery == "" {
		delivery = githubWebhookReplayKey(body)
	}
	for i := range monitors.Items {
		monitor := &monitors.Items[i]
		intent, ok := repositoryMonitorCommandIntentForLabel(monitor, target, payload.Label.Name)
		if !ok || !repositoryMonitorAcceptsLabelCommand(monitor, payload.Repository, target) {
			continue
		}
		result.Matched++
		command, duplicate, err := h.recordRepositoryMonitorCommandEvent(c, monitor, payload, target, intent, delivery)
		if err != nil {
			return result, true, err
		}
		if duplicate {
			result.Duplicate++
			result.CommandIDs = append(result.CommandIDs, command.ID)
			continue
		}
		result.CommandIDs = append(result.CommandIDs, command.ID)
		if command.Status != githubCommandStatusAccepted || repositoryMonitorCommandDoesNotQueueRun(intent) {
			continue
		}
		run, queued, err := h.queueRepositoryMonitorCommandRun(c, monitor, command, target)
		if err != nil {
			return result, true, err
		}
		if queued {
			result.Queued++
			result.RunIDs = append(result.RunIDs, run.ID)
		} else {
			result.SkippedActive++
		}
	}
	return result, result.Matched > 0, nil
}

func repositoryMonitorAcceptsLabelCommand(monitor *corev1alpha1.RepositoryMonitor, repo githubWebhookRepository, target githubLabelTarget) bool {
	if monitor == nil || repositoryMonitorWebhookSuspended(monitor) || !monitor.Spec.Triggers.GitHub.Labels.Enabled {
		return false
	}
	owner, repository, err := parseRepositoryMonitorGitHubURL(monitor.Spec.RepoURL)
	if err != nil || !strings.EqualFold(strings.TrimSpace(repo.FullName), owner+"/"+repository) {
		return false
	}
	if target.IsPR {
		if !repositoryMonitorPullRequestsEnabled(monitor.Spec) {
			return false
		}
		return target.BaseBranch == "" || strings.EqualFold(target.BaseBranch, effectiveRepositoryMonitorBranch(monitor))
	}
	return target.Kind == repositoryMonitorTargetKindIssue && monitor.Spec.Targets.Issues.Enabled
}

func repositoryMonitorCommandIntentForLabel(monitor *corev1alpha1.RepositoryMonitor, target githubLabelTarget, label string) (string, bool) {
	label = strings.ToLower(strings.TrimSpace(label))
	if label == "" {
		return "", false
	}
	labels := monitor.Spec.Triggers.GitHub.Labels
	var configured map[string]string
	if target.IsPR {
		configured = map[string]string{
			"review":            labels.PullRequests.Review,
			"fix":               labels.PullRequests.Fix,
			"fix_ci":            labels.PullRequests.FixCI,
			"update_branch":     labels.PullRequests.UpdateBranch,
			commandIntentStop:   labels.PullRequests.Stop,
			commandIntentResume: labels.PullRequests.Resume,
		}
	} else {
		configured = map[string]string{
			"triage":                 labels.Issues.Triage,
			"research":               labels.Issues.Research,
			"plan":                   labels.Issues.Plan,
			commandIntentApprovePlan: labels.Issues.ApprovePlan,
			"implement":              labels.Issues.Implement,
			"decompose":              labels.Issues.Decompose,
			commandIntentStop:        labels.Issues.Stop,
			commandIntentResume:      labels.Issues.Resume,
		}
	}
	for intent, configuredLabel := range configured {
		if configuredLabel == "" {
			configuredLabel = repositoryMonitorDefaultCommandLabel(intent)
		}
		if strings.EqualFold(strings.TrimSpace(configuredLabel), label) {
			return intent, true
		}
	}
	return "", false
}

func repositoryMonitorDefaultCommandLabel(intent string) string {
	switch intent {
	case commandIntentApprovePlan:
		return "orka:approve-plan"
	case "fix_ci":
		return "orka:fix-ci"
	case "update_branch":
		return "orka:update-branch"
	case "decompose":
		return "orka:to-issues"
	default:
		return "orka:" + strings.ReplaceAll(intent, "_", "-")
	}
}

func (h *Handlers) recordRepositoryMonitorCommandEvent(c fiber.Ctx, monitor *corev1alpha1.RepositoryMonitor, payload githubLabelWebhookPayload, target githubLabelTarget, intent, delivery string) (*store.CommandEvent, bool, error) {
	permission, permissionErr := h.repositoryMonitorCommandActorPermission(c.Context(), monitor, payload.Repository, payload.Sender.Login)
	status := githubCommandStatusAccepted
	errorMessage := ""
	if permissionErr != nil {
		status = githubCommandStatusRejected
		errorMessage = permissionErr.Error()
	} else if !repositoryMonitorPermissionAllowed(monitor, permission) {
		status = githubCommandStatusRejected
		errorMessage = fmt.Sprintf("sender %q has GitHub permission %q, which is not allowed for RepositoryMonitor commands", payload.Sender.Login, permission)
	} else if guard := repositoryMonitorCommandGuardLabel(monitor, target.Labels); guard != "" {
		status = githubCommandStatusBlocked
		errorMessage = fmt.Sprintf("target has guard label %q", guard)
	}

	dedupe := repositoryMonitorCommandDedupeKey(monitor, target, payload.Label.Name, delivery)
	processedAt := time.Now()
	command := &store.CommandEvent{
		ID:                  repositoryMonitorCommandID(dedupe),
		MonitorNamespace:    monitor.Namespace,
		MonitorName:         monitor.Name,
		Repo:                payload.Repository.FullName,
		Kind:                target.Kind,
		Number:              int64(target.Number),
		Source:              githubCommandEventSourceLabel,
		DeliveryID:          delivery,
		Label:               payload.Label.Name,
		MonitorGeneration:   monitor.Generation,
		DedupeKey:           dedupe,
		IdempotencyKey:      dedupe,
		CommentID:           delivery,
		Author:              payload.Sender.Login,
		Permission:          permission,
		Command:             payload.Label.Name,
		Intent:              intent,
		HeadSHA:             target.HeadSHA,
		IssueSnapshotDigest: githubIssueSnapshotDigest(monitor, target),
		Status:              status,
		CreatedAt:           processedAt,
		ProcessedAt:         &processedAt,
		Error:               errorMessage,
	}
	if err := h.repositoryMonitorStore.CreateCommandEvent(c.Context(), command); err != nil {
		if existing, getErr := h.repositoryMonitorStore.GetCommandEvent(c.Context(), monitor.Namespace, command.ID); getErr == nil {
			return existing, true, nil
		}
		return nil, false, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to record repository monitor command: %v", err))
	}
	if command.Status == githubCommandStatusAccepted && monitor.Spec.Triggers.GitHub.Labels.ConsumeCommandLabels {
		if err := h.consumeRepositoryMonitorCommandLabel(c.Context(), monitor, payload.Repository, target, payload.Label.Name); err != nil {
			command.Error = fmt.Sprintf("accepted, but failed to consume command label: %v", err)
			_ = h.repositoryMonitorStore.UpdateCommandEvent(c.Context(), command)
		}
	}
	return command, false, nil
}

func (h *Handlers) consumeRepositoryMonitorCommandLabel(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, repo githubWebhookRepository, target githubLabelTarget, label string) error {
	token, err := h.repositoryMonitorGitHubToken(ctx, monitor)
	if err != nil {
		return err
	}
	owner, repository, err := parseRepositoryMonitorGitHubURL(monitor.Spec.RepoURL)
	if err != nil {
		return err
	}
	if strings.TrimSpace(repo.FullName) != "" {
		parts := strings.SplitN(repo.FullName, "/", 2)
		if len(parts) == 2 {
			owner, repository = parts[0], parts[1]
		}
	}
	baseURL := strings.TrimRight(osGetenv(githubAPIBaseURLEnv), "/")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/issues/%d/labels/%s", baseURL, url.PathEscape(owner), url.PathEscape(repository), target.Number, url.PathEscape(label))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub label removal failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GitHub label removal returned %d: %s", resp.StatusCode, string(bytes.TrimSpace(data)))
	}
	return nil
}

func (h *Handlers) queueRepositoryMonitorCommandRun(c fiber.Ctx, monitor *corev1alpha1.RepositoryMonitor, command *store.CommandEvent, target githubLabelTarget) (*store.MonitorRun, bool, error) {
	run := &store.MonitorRun{
		ID:               repositoryMonitorCommandRunID(command),
		MonitorNamespace: monitor.Namespace,
		MonitorName:      monitor.Name,
		Trigger:          githubMonitorTriggerLabelCommand,
		TargetKind:       target.Kind,
		TargetNumber:     int64(target.Number),
		TargetSHA:        target.HeadSHA,
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        time.Now(),
	}
	if err := h.repositoryMonitorStore.CreateMonitorRun(c.Context(), run); err != nil {
		if errors.Is(err, store.ErrConflict) {
			command.Status = githubCommandStatusBlocked
			command.Error = "repository monitor already has an active run"
			_ = h.repositoryMonitorStore.UpdateCommandEvent(c.Context(), command)
			return run, false, nil
		}
		if existing, getErr := h.repositoryMonitorStore.GetMonitorRun(c.Context(), run.MonitorNamespace, run.ID); getErr == nil {
			return existing, false, nil
		}
		return nil, false, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create repository monitor command run: %v", err))
	}
	if err := h.annotateRepositoryMonitorRunRequest(c, monitor, run); err != nil {
		if failErr := h.markRepositoryMonitorRunSignalFailed(c, run, err); failErr != nil {
			return nil, false, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("%v; additionally failed to mark monitor run failed: %v", err, failErr))
		}
		return nil, false, err
	}
	return run, true, nil
}

func repositoryMonitorCommandDoesNotQueueRun(intent string) bool {
	switch intent {
	case commandIntentStop, commandIntentResume, commandIntentApprovePlan:
		return true
	default:
		return false
	}
}

func repositoryMonitorCommandGuardLabel(monitor *corev1alpha1.RepositoryMonitor, labels []string) string {
	guards := append([]string{}, monitor.Spec.Policy.ProtectedLabels...)
	guards = append(guards, monitor.Spec.Policy.PauseLabels...)
	for _, label := range labels {
		for _, guard := range guards {
			if strings.EqualFold(strings.TrimSpace(label), strings.TrimSpace(guard)) && strings.TrimSpace(guard) != "" {
				return label
			}
		}
	}
	return ""
}

func (h *Handlers) repositoryMonitorCommandActorPermission(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, repo githubWebhookRepository, actor string) (string, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return "", fmt.Errorf("GitHub webhook sender is missing")
	}
	token, err := h.repositoryMonitorGitHubToken(ctx, monitor)
	if err != nil {
		return "", err
	}
	owner, repository, err := parseRepositoryMonitorGitHubURL(monitor.Spec.RepoURL)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(repo.FullName) != "" {
		parts := strings.SplitN(repo.FullName, "/", 2)
		if len(parts) == 2 {
			owner, repository = parts[0], parts[1]
		}
	}
	baseURL := strings.TrimRight(osGetenv(githubAPIBaseURLEnv), "/")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/collaborators/%s/permission", baseURL, url.PathEscape(owner), url.PathEscape(repository), url.PathEscape(actor))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("GitHub permission check failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GitHub permission check returned %d: %s", resp.StatusCode, string(bytes.TrimSpace(data)))
	}
	var parsed struct {
		Permission string `json:"permission"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("failed to parse GitHub permission response: %w", err)
	}
	if strings.TrimSpace(parsed.Permission) == "" {
		return "", fmt.Errorf("GitHub permission response did not include permission")
	}
	return strings.ToLower(strings.TrimSpace(parsed.Permission)), nil
}

func (h *Handlers) repositoryMonitorGitHubToken(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor) (string, error) {
	if monitor.Spec.GitSecretRef == nil || strings.TrimSpace(monitor.Spec.GitSecretRef.Name) == "" {
		return "", fmt.Errorf("spec.gitSecretRef is required for GitHub actor permission checks")
	}
	var secret corev1.Secret
	if err := h.client.Get(ctx, types.NamespacedName{Name: monitor.Spec.GitSecretRef.Name, Namespace: monitor.Namespace}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("spec.gitSecretRef %q not found in namespace %q", monitor.Spec.GitSecretRef.Name, monitor.Namespace)
		}
		return "", fmt.Errorf("failed to get spec.gitSecretRef %q: %w", monitor.Spec.GitSecretRef.Name, err)
	}
	for _, key := range []string{"token", "password", workerenv.GitHubToken} {
		if value := strings.TrimSpace(string(secret.Data[key])); value != "" {
			return value, nil
		}
	}
	return "", fmt.Errorf("spec.gitSecretRef %q must contain a token, password, or %s key", monitor.Spec.GitSecretRef.Name, workerenv.GitHubToken)
}

func repositoryMonitorPermissionAllowed(monitor *corev1alpha1.RepositoryMonitor, permission string) bool {
	permission = strings.ToLower(strings.TrimSpace(permission))
	allowed := monitor.Spec.Policy.AllowedRepositoryPermissions
	if len(allowed) == 0 {
		allowed = repositoryMonitorPermissionsAtLeast(monitor.Spec.Triggers.GitHub.Labels.RequireActorPermission)
	}
	for _, candidate := range allowed {
		if strings.EqualFold(strings.TrimSpace(candidate), permission) {
			return true
		}
	}
	return false
}

func repositoryMonitorPermissionsAtLeast(minimum string) []string {
	switch strings.ToLower(strings.TrimSpace(minimum)) {
	case "admin":
		return []string{"admin"}
	case "maintain":
		return []string{"maintain", "admin"}
	default:
		return []string{"write", "maintain", "admin"}
	}
}

func repositoryMonitorCommandDedupeKey(monitor *corev1alpha1.RepositoryMonitor, target githubLabelTarget, label, delivery string) string {
	parts := []string{monitor.Namespace, monitor.Name, delivery, strings.ToLower(strings.TrimSpace(label)), target.Kind, fmt.Sprintf("%d", target.Number), target.HeadSHA}
	return strings.Join(parts, "|")
}

func repositoryMonitorCommandID(dedupe string) string {
	return "cmd-" + githubReplayKeySuffix(githubWebhookReplayKey([]byte(dedupe)))
}

func repositoryMonitorCommandRunID(command *store.CommandEvent) string {
	return "monrun-" + githubReplayKeySuffix(githubWebhookReplayKey([]byte(command.ID+"|run")))
}

func githubIssueSnapshotDigest(monitor *corev1alpha1.RepositoryMonitor, target githubLabelTarget) string {
	if target.Kind != repositoryMonitorTargetKindIssue {
		return ""
	}
	ignored := map[string]struct{}{}
	if monitor != nil {
		configured := monitor.Spec.Triggers.GitHub.Labels.Issues
		for _, label := range []string{configured.Triage, configured.Research, configured.Plan, configured.ApprovePlan, configured.Implement, configured.Decompose, configured.Stop, configured.Resume} {
			if label = strings.ToLower(strings.TrimSpace(label)); label != "" {
				ignored[label] = struct{}{}
			}
		}
	}
	labels := make([]string, 0, len(target.Labels))
	for _, label := range target.Labels {
		trimmed := strings.TrimSpace(label)
		lower := strings.ToLower(trimmed)
		if _, ok := ignored[lower]; ok {
			continue
		}
		if trimmed == "" || strings.HasPrefix(lower, "orka:") || strings.HasPrefix(lower, "orka-state:") {
			continue
		}
		labels = append(labels, trimmed)
	}
	slicesSortStringsFold(labels)
	payload := map[string]any{"number": target.Number, "title": target.Title, "body": target.Body, "labels": labels}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func slicesSortStringsFold(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && strings.ToLower(values[j]) < strings.ToLower(values[j-1]); j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func osGetenv(key string) string { return strings.TrimSpace(os.Getenv(key)) }
