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

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/workerenv"
)

const (
	githubAPIBaseURLEnv           = "ORKA_GITHUB_API_BASE_URL"
	commandIntentStop             = finishReasonStop
	commandIntentResume           = "resume"
	commandIntentApprovePlan      = "approve_plan"
	commandIntentDecompose        = "decompose"
	commandIntentPlan             = "plan"
	commandIntentFixCI            = "fix_ci"
	commandIntentUpdateBranch     = "update_branch"
	githubMutationStatusSucceeded = "succeeded"

	githubPermissionRead     = "read"
	githubPermissionTriage   = "triage"
	githubPermissionWrite    = "write"
	githubPermissionMaintain = "maintain"
	githubPermissionAdmin    = "admin"
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
		if !ok || !repositoryMonitorAcceptsLabelCommand(monitor, payload.Repository, target, intent) {
			continue
		}
		result.Matched++
		command, duplicate, err := h.recordRepositoryMonitorCommandEvent(c, monitor, payload, target, intent, delivery)
		if err != nil {
			return result, true, err
		}
		if duplicate {
			result.Duplicate++
		}
		result.CommandIDs = append(result.CommandIDs, command.ID)
		if command.Status != githubCommandStatusAccepted {
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

func repositoryMonitorAcceptsLabelCommand(monitor *corev1alpha1.RepositoryMonitor, repo githubWebhookRepository, target githubLabelTarget, intent string) bool {
	if monitor == nil || repositoryMonitorWebhookSuspended(monitor) || !monitor.Spec.Triggers.GitHub.Labels.Enabled {
		return false
	}
	owner, repository, err := parseRepositoryMonitorGitHubURL(monitor.Spec.RepoURL)
	if err != nil || !strings.EqualFold(strings.TrimSpace(repo.FullName), owner+"/"+repository) {
		return false
	}
	if intent == commandIntentStop {
		if target.IsPR {
			return repositoryMonitorPullRequestsEnabled(monitor.Spec)
		}
		return target.Kind == repositoryMonitorTargetKindIssue && monitor.Spec.Targets.Issues.Enabled
	}
	if strings.TrimSpace(target.State) != "" && !strings.EqualFold(strings.TrimSpace(target.State), "open") {
		return false
	}
	if target.IsPR {
		if !repositoryMonitorPullRequestsEnabled(monitor.Spec) {
			return false
		}
		if target.Draft && !monitor.Spec.Targets.PullRequests.IncludeDrafts {
			return false
		}
		return target.BaseBranch == "" || strings.TrimSpace(target.BaseBranch) == effectiveRepositoryMonitorBranch(monitor)
	}
	if target.Kind != repositoryMonitorTargetKindIssue || !monitor.Spec.Targets.Issues.Enabled {
		return false
	}
	return repositoryMonitorControlCommandIntent(intent) || repositoryMonitorCommandGuardLabel(monitor, target.Labels) != "" || repositoryMonitorWebhookIssueTargetLabelsAllowed(monitor.Spec, target.Labels)
}

func repositoryMonitorWebhookIssueTargetLabelsAllowed(spec corev1alpha1.RepositoryMonitorSpec, labels []string) bool {
	if repositoryMonitorWebhookMatchingLabel(spec.Targets.Issues.ExcludeLabels, labels) != "" {
		return false
	}
	return len(spec.Targets.Issues.IncludeLabels) == 0 || repositoryMonitorWebhookMatchingLabel(spec.Targets.Issues.IncludeLabels, labels) != ""
}

func repositoryMonitorWebhookMatchingLabel(policyLabels, itemLabels []string) string {
	wanted := map[string]struct{}{}
	for _, label := range policyLabels {
		label = strings.ToLower(strings.TrimSpace(label))
		if label != "" {
			wanted[label] = struct{}{}
		}
	}
	for _, label := range itemLabels {
		if _, ok := wanted[strings.ToLower(strings.TrimSpace(label))]; ok {
			return label
		}
	}
	return ""
}

func repositoryMonitorCommandIntentForLabel(monitor *corev1alpha1.RepositoryMonitor, target githubLabelTarget, label string) (string, bool) {
	label = strings.ToLower(strings.TrimSpace(label))
	if label == "" {
		return "", false
	}
	labels := monitor.Spec.Triggers.GitHub.Labels
	type commandLabel struct{ intent, label string }
	var configured []commandLabel
	if target.IsPR {
		configured = []commandLabel{
			{intent: "review", label: labels.PullRequests.Review},
			{intent: "fix", label: labels.PullRequests.Fix},
			{intent: commandIntentFixCI, label: labels.PullRequests.FixCI},
			{intent: commandIntentUpdateBranch, label: labels.PullRequests.UpdateBranch},
			{intent: "automerge", label: labels.PullRequests.Automerge},
			{intent: commandIntentStop, label: labels.PullRequests.Stop},
			{intent: commandIntentResume, label: labels.PullRequests.Resume},
		}
	} else {
		configured = []commandLabel{
			{intent: "triage", label: labels.Issues.Triage},
			{intent: "research", label: labels.Issues.Research},
			{intent: "plan", label: labels.Issues.Plan},
			{intent: commandIntentApprovePlan, label: labels.Issues.ApprovePlan},
			{intent: "implement", label: labels.Issues.Implement},
			{intent: "decompose", label: labels.Issues.Decompose},
			{intent: commandIntentStop, label: labels.Issues.Stop},
			{intent: commandIntentResume, label: labels.Issues.Resume},
		}
	}
	for _, entry := range configured {
		configuredLabel := entry.label
		if configuredLabel == "" {
			configuredLabel = repositoryMonitorDefaultCommandLabel(entry.intent)
		}
		if strings.EqualFold(strings.TrimSpace(configuredLabel), label) {
			return entry.intent, true
		}
	}
	return "", false
}

func repositoryMonitorDefaultCommandLabel(intent string) string {
	switch intent {
	case commandIntentApprovePlan:
		return "orka:approve-plan"
	case commandIntentFixCI:
		return "orka:fix-ci"
	case commandIntentUpdateBranch:
		return "orka:update-branch"
	case commandIntentDecompose:
		return "orka:to-issues"
	default:
		return "orka:" + strings.ReplaceAll(intent, "_", "-")
	}
}

func (h *Handlers) recordRepositoryMonitorCommandEvent(c fiber.Ctx, monitor *corev1alpha1.RepositoryMonitor, payload githubLabelWebhookPayload, target githubLabelTarget, intent, delivery string) (*store.CommandEvent, bool, error) {
	dedupe := repositoryMonitorCommandDedupeKey(monitor, target, payload.Label.Name, delivery)
	commandID := repositoryMonitorCommandID(dedupe)
	if existing, err := h.repositoryMonitorStore.GetCommandEvent(c.Context(), monitor.Namespace, commandID); err == nil {
		if err := h.ensureRepositoryMonitorCommandLabelConsumed(c, monitor, existing, payload, target); err != nil {
			return nil, true, err
		}
		return existing, true, nil
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, false, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to inspect repository monitor command: %v", err))
	}
	permission, permissionErr := h.repositoryMonitorCommandActorPermission(c.Context(), monitor, payload.Repository, payload.Sender.Login)
	status := githubCommandStatusAccepted
	errorMessage := ""
	if permissionErr != nil {
		return nil, false, fiber.NewError(fiber.StatusServiceUnavailable, fmt.Sprintf("failed to verify GitHub actor permission: %v", permissionErr))
	}
	if !repositoryMonitorPermissionAllowedForIntent(monitor, permission, intent) {
		status = githubCommandStatusRejected
		errorMessage = fmt.Sprintf("sender %q has GitHub permission %q, which is not allowed for RepositoryMonitor commands", payload.Sender.Login, permission)
	} else if !repositoryMonitorControlCommandIntent(intent) {
		if guard := repositoryMonitorCommandGuardLabel(monitor, target.Labels); guard != "" {
			status = githubCommandStatusBlocked
			errorMessage = fmt.Sprintf("target has guard label %q", guard)
		}
	}

	processedAt := time.Now()
	command := &store.CommandEvent{
		ID:                  commandID,
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
	metrics.RecordRepositoryMonitorCommand(command.Intent, command.Status)
	if err := h.ensureRepositoryMonitorCommandLabelConsumed(c, monitor, command, payload, target); err != nil {
		return nil, false, err
	}

	return command, false, nil
}

func (h *Handlers) ensureRepositoryMonitorCommandLabelConsumed(c fiber.Ctx, monitor *corev1alpha1.RepositoryMonitor, command *store.CommandEvent, payload githubLabelWebhookPayload, target githubLabelTarget) error {
	if command == nil || command.Status != githubCommandStatusAccepted || !monitor.Spec.Triggers.GitHub.Labels.ConsumeCommandLabels {
		return nil
	}
	mutationID := "ghmut-" + githubReplayKeySuffix(githubWebhookReplayKey([]byte(command.ID+"|remove_label")))
	mutation, err := h.repositoryMonitorStore.GetGitHubMutationRecord(c.Context(), monitor.Namespace, mutationID)
	if errors.Is(err, store.ErrNotFound) {
		mutation = &store.GitHubMutationRecord{ID: mutationID, CommandEventID: command.ID, Operation: "remove_label", TargetKind: target.Kind, TargetNumber: int64(target.Number), TargetSHA: target.HeadSHA, Reason: "consume_command_label", RequestDigest: "sha256:" + githubReplayKeySuffix(githubWebhookReplayKey([]byte(payload.Label.Name))), Status: "started"}
		if err := h.recordRepositoryMonitorGitHubMutation(c.Context(), monitor, mutation); err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to record command label mutation: %v", err))
		}
	} else if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to inspect command label mutation: %v", err))
	} else if mutation.Status == githubMutationStatusSucceeded {
		return nil
	} else {
		mutation.Status = "started"
		mutation.Error = ""
		if err := h.updateRepositoryMonitorGitHubMutation(c.Context(), monitor, mutation); err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to reset command label mutation: %v", err))
		}
	}
	if err := h.consumeRepositoryMonitorCommandLabel(c.Context(), monitor, payload.Repository, target, payload.Label.Name); err != nil {
		mutation.Status = repositoryMonitorRunPhaseFailed
		mutation.Error = err.Error()
		if auditErr := h.updateRepositoryMonitorGitHubMutation(c.Context(), monitor, mutation); auditErr != nil {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to consume command label: %v; audit update failed: %v", err, auditErr))
		}
		command.Error = fmt.Sprintf("accepted, but failed to consume command label: %v", err)
		if updateErr := h.repositoryMonitorStore.UpdateCommandEvent(c.Context(), command); updateErr != nil {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to update monitor command: %v", updateErr))
		}
		return nil
	}
	mutation.Status = githubMutationStatusSucceeded
	mutation.Error = ""
	if err := h.updateRepositoryMonitorGitHubMutation(c.Context(), monitor, mutation); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to finalize command label mutation: %v", err))
	}
	if command.Error != "" {
		command.Error = ""
		if err := h.repositoryMonitorStore.UpdateCommandEvent(c.Context(), command); err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to clear command error: %v", err))
		}
	}
	return nil
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
	req.Header.Set("Authorization", strings.Join([]string{"Bearer", token}, " "))
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
		if resp.StatusCode == http.StatusNotFound {
			return nil
		}
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
		CommandEventID:   command.ID,
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        time.Now(),
	}
	if err := h.upsertRepositoryMonitorCommandWorkAction(c.Context(), monitor, command, run.ID); err != nil {
		return nil, false, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to link monitor workflow action to run: %v", err))
	}
	if command.Status != githubCommandStatusAccepted {
		return run, false, nil
	}
	if err := h.repositoryMonitorStore.CreateMonitorRun(c.Context(), run); err != nil {
		if existing, getErr := h.repositoryMonitorStore.GetMonitorRun(c.Context(), run.MonitorNamespace, run.ID); getErr == nil {
			if existing.Phase == repositoryMonitorRunPhaseFailed && strings.Contains(existing.Error, "failed to signal repository monitor run") {
				existing.Phase = repositoryMonitorRunPhaseQueued
				existing.StartedAt = time.Now()
				existing.CompletedAt = nil
				existing.Error = ""
				if updateErr := h.repositoryMonitorStore.UpdateMonitorRun(c.Context(), existing); updateErr != nil {
					return nil, false, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to reset repository monitor command run: %v", updateErr))
				}
				if err := h.annotateRepositoryMonitorRunRequest(c, monitor, existing); err != nil {
					_ = h.failRepositoryMonitorCommandWorkAction(c.Context(), monitor, command, existing.ID, err)
					if failErr := h.markRepositoryMonitorRunSignalFailed(c, existing, err); failErr != nil {
						return nil, false, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("%v; additionally failed to mark monitor run failed: %v", err, failErr))
					}
					return nil, false, err
				}
				return existing, true, nil
			}
			if existing.Phase == repositoryMonitorRunPhaseQueued {
				if err := h.annotateRepositoryMonitorRunRequest(c, monitor, existing); err != nil {
					_ = h.failRepositoryMonitorCommandWorkAction(c.Context(), monitor, command, existing.ID, err)
					if failErr := h.markRepositoryMonitorRunSignalFailed(c, existing, err); failErr != nil {
						return nil, false, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("%v; additionally failed to mark monitor run failed: %v", err, failErr))
					}
					return nil, false, err
				}
				return existing, false, nil
			}
			return existing, false, nil
		}
		if errors.Is(err, store.ErrConflict) {
			return run, false, nil
		}
		return nil, false, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create repository monitor command run: %v", err))
	}
	if err := h.annotateRepositoryMonitorRunRequest(c, monitor, run); err != nil {
		_ = h.failRepositoryMonitorCommandWorkAction(c.Context(), monitor, command, run.ID, err)
		if failErr := h.markRepositoryMonitorRunSignalFailed(c, run, err); failErr != nil {
			return nil, false, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("%v; additionally failed to mark monitor run failed: %v", err, failErr))
		}
		return nil, false, err
	}
	return run, true, nil
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
	req.Header.Set("Authorization", strings.Join([]string{"Bearer", token}, " "))
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
		RoleName   string `json:"role_name"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("failed to parse GitHub permission response: %w", err)
	}
	permission := githubRepositoryPermissionFromResponse(parsed.RoleName, parsed.Permission)
	if strings.TrimSpace(permission) == "" {
		return "", fmt.Errorf("GitHub permission response did not include permission")
	}
	return strings.ToLower(strings.TrimSpace(permission)), nil
}

func githubRepositoryPermissionFromResponse(roleName, permission string) string {
	roleName = strings.TrimSpace(roleName)
	if githubBuiltInRepositoryPermission(roleName) {
		return roleName
	}
	return strings.TrimSpace(permission)
}

func githubBuiltInRepositoryPermission(permission string) bool {
	switch strings.ToLower(strings.TrimSpace(permission)) {
	case githubPermissionRead, githubPermissionTriage, githubPermissionWrite, githubPermissionMaintain, githubPermissionAdmin:
		return true
	default:
		return false
	}
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

func repositoryMonitorPermissionAllowedForIntent(monitor *corev1alpha1.RepositoryMonitor, permission, intent string) bool {
	if strings.TrimSpace(intent) == githubActionReview && monitor != nil && monitor.Spec.Review.Publish.Enabled {
		return repositoryMonitorPermissionAllowed(monitor, permission)
	}
	if !repositoryMonitorReadOnlyCommandIntent(intent) {
		return repositoryMonitorPermissionAllowed(monitor, permission)
	}
	permission = strings.ToLower(strings.TrimSpace(permission))
	if !repositoryMonitorPermissionInList(permission, []string{githubPermissionTriage, githubPermissionWrite, githubPermissionMaintain, githubPermissionAdmin}) {
		return false
	}
	policyAllowed := monitor.Spec.Policy.AllowedRepositoryPermissions
	return len(policyAllowed) == 0 || repositoryMonitorPermissionInList(permission, policyAllowed)
}

func repositoryMonitorControlCommandIntent(intent string) bool {
	switch strings.TrimSpace(intent) {
	case commandIntentStop:
		return true
	default:
		return false
	}
}

func repositoryMonitorReadOnlyCommandIntent(intent string) bool {
	switch strings.TrimSpace(intent) {
	case "triage", "research", commandIntentPlan, "review":
		return true
	default:
		return false
	}
}

func repositoryMonitorPermissionAllowed(monitor *corev1alpha1.RepositoryMonitor, permission string) bool {
	permission = strings.ToLower(strings.TrimSpace(permission))
	minimumAllowed := repositoryMonitorPermissionsAtLeast(monitor.Spec.Triggers.GitHub.Labels.RequireActorPermission)
	if !repositoryMonitorPermissionInList(permission, minimumAllowed) {
		return false
	}
	policyAllowed := monitor.Spec.Policy.AllowedRepositoryPermissions
	return len(policyAllowed) == 0 || repositoryMonitorPermissionInList(permission, policyAllowed)
}

func repositoryMonitorPermissionInList(permission string, allowed []string) bool {
	for _, candidate := range allowed {
		if strings.EqualFold(strings.TrimSpace(candidate), permission) {
			return true
		}
	}
	return false
}

func repositoryMonitorPermissionsAtLeast(minimum string) []string {
	switch strings.ToLower(strings.TrimSpace(minimum)) {
	case githubPermissionAdmin:
		return []string{githubPermissionAdmin}
	case githubPermissionMaintain:
		return []string{githubPermissionMaintain, githubPermissionAdmin}
	default:
		return []string{githubPermissionWrite, githubPermissionMaintain, githubPermissionAdmin}
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
	payload := struct {
		Number int      `json:"number"`
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}{Number: target.Number, Title: target.Title, Body: target.Body, Labels: labels}
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

//nolint:gocyclo // Durable command/action coalescing keeps each terminal and race outcome explicit.
func (h *Handlers) upsertRepositoryMonitorCommandWorkAction(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, command *store.CommandEvent, runID string) error {
	if monitor == nil || command == nil || h.repositoryMonitorStore == nil {
		return nil
	}
	desiredAction := store.RepositoryMonitorDesiredActionForIntent(command.Intent)
	if desiredAction == "" {
		return nil
	}
	var status string
	phase := desiredAction + "_queued"
	blockedReason := ""
	switch command.Status {
	case githubCommandStatusRejected:
		status = repositoryMonitorRunPhaseFailed
		phase = "rejected"
		blockedReason = command.Error
	case githubCommandStatusBlocked:
		status = githubCommandStatusBlocked
		phase = githubCommandStatusBlocked
		blockedReason = command.Error
	case githubCommandStatusAccepted, "":
		status = repositoryMonitorRunPhaseQueued
	default:
		status = command.Status
	}
	var completedAt *time.Time
	switch status {
	case repositoryMonitorRunPhaseFailed, githubCommandStatusBlocked, githubCommandStatusCompleted:
		now := time.Now()
		completedAt = &now
	}
	id := store.RepositoryMonitorWorkActionID(command.ID, desiredAction)
	if existing, err := h.repositoryMonitorStore.GetWorkAction(ctx, monitor.Namespace, id); err == nil {
		if existing.Status == "cancelled" && desiredAction != commandIntentStop && desiredAction != commandIntentResume {
			return nil
		}
		if runID != "" && existing.RunID == "" {
			existing.RunID = runID
		}
		if status == repositoryMonitorRunPhaseQueued && existing.Status == repositoryMonitorRunPhaseFailed && existing.BlockedReason == "run_signal_failed" {
			existing.Status = repositoryMonitorRunPhaseQueued
			existing.Phase = phase
			existing.BlockedReason = ""
			existing.Error = ""
			existing.CompletedAt = nil
		}
		if existing.Status == "queued" && status != "queued" {
			existing.Status = status
			existing.Phase = phase
			existing.BlockedReason = blockedReason
		}
		metrics.RecordRepositoryMonitorWorkAction(existing.DesiredAction, existing.Status)
		return h.repositoryMonitorStore.UpdateWorkAction(ctx, existing)
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	dedupe := store.RepositoryMonitorWorkActionDedupeKey(monitor.Namespace, monitor.Name, monitor.Generation, command.Kind, command.Number, command.HeadSHA, command.IssueSnapshotDigest, desiredAction)
	if command.Status == githubCommandStatusAccepted && desiredAction != commandIntentStop && desiredAction != commandIntentResume {
		active, _, err := h.repositoryMonitorStore.ListWorkActions(ctx, store.WorkActionFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, DedupeKey: dedupe, Limit: 5})
		if err != nil {
			return err
		}
		for _, candidate := range active {
			switch candidate.Status {
			case repositoryMonitorRunPhaseQueued, "leased", "running":
				command.Status = githubCommandStatusCompleted
				command.Error = "coalesced with active workflow action " + candidate.ID
				if err := h.repositoryMonitorStore.UpdateCommandEvent(ctx, command); err != nil {
					return err
				}
				return nil
			}
		}
	}
	metadata, _ := json.Marshal(map[string]any{"source": command.Source, "label": command.Label, "deliveryID": command.DeliveryID})
	metrics.RecordRepositoryMonitorWorkAction(desiredAction, status)
	if err := h.repositoryMonitorStore.CreateWorkAction(ctx, &store.WorkAction{
		ID:                   id,
		MonitorNamespace:     monitor.Namespace,
		MonitorName:          monitor.Name,
		RunID:                runID,
		CommandEventID:       command.ID,
		MonitorGeneration:    monitor.Generation,
		TargetKind:           command.Kind,
		TargetNumber:         command.Number,
		TargetSHA:            command.HeadSHA,
		TargetSnapshotDigest: command.IssueSnapshotDigest,
		Intent:               command.Intent,
		DesiredAction:        desiredAction,
		DedupeKey:            dedupe,
		IdempotencyKey:       command.IdempotencyKey,
		Status:               status,
		Phase:                phase,
		BlockedReason:        blockedReason,
		MetadataJSON:         string(metadata),
		CreatedAt:            command.CreatedAt,
		CompletedAt:          completedAt,
	}); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "constraint") {
			existing, getErr := h.repositoryMonitorStore.GetWorkAction(ctx, monitor.Namespace, id)
			if getErr == nil && existing.CommandEventID == command.ID && existing.DedupeKey == dedupe {
				return nil
			}
		}
		return err
	}
	return nil
}

func (h *Handlers) failRepositoryMonitorCommandWorkAction(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, command *store.CommandEvent, runID string, cause error) error {
	if monitor == nil || command == nil || h.repositoryMonitorStore == nil || cause == nil {
		return nil
	}
	desiredAction := store.RepositoryMonitorDesiredActionForIntent(command.Intent)
	if desiredAction == "" {
		return nil
	}
	action, err := h.repositoryMonitorStore.GetWorkAction(ctx, monitor.Namespace, store.RepositoryMonitorWorkActionID(command.ID, desiredAction))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	now := time.Now()
	action.Status = repositoryMonitorRunPhaseFailed
	action.Phase = repositoryMonitorRunPhaseFailed
	action.RunID = runID
	action.Error = cause.Error()
	action.BlockedReason = "run_signal_failed"
	action.CompletedAt = &now
	return h.repositoryMonitorStore.UpdateWorkAction(ctx, action)
}

func (h *Handlers) recordRepositoryMonitorGitHubMutation(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, record *store.GitHubMutationRecord) error {
	if monitor == nil || record == nil || h.repositoryMonitorStore == nil {
		return nil
	}
	if record.ID == "" {
		sum := sha256.Sum256([]byte(record.Operation + "|" + record.TargetKind + "|" + fmt.Sprint(record.TargetNumber) + "|" + record.TargetSHA + "|" + record.Reason + "|" + record.GitHubURL))
		record.ID = "ghmut-" + hex.EncodeToString(sum[:])[:16]
	}
	record.MonitorNamespace = monitor.Namespace
	record.MonitorName = monitor.Name
	record.MonitorGeneration = monitor.Generation
	if record.Actor == "" {
		record.Actor = "orka-controller"
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now()
	}
	if err := h.repositoryMonitorStore.CreateGitHubMutationRecord(ctx, record); err != nil && !strings.Contains(strings.ToLower(err.Error()), "constraint") {
		return err
	}
	metrics.RecordRepositoryMonitorGitHubMutation(record.Operation, record.Status)
	return nil
}

func (h *Handlers) updateRepositoryMonitorGitHubMutation(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, record *store.GitHubMutationRecord) error {
	if monitor == nil || record == nil || h.repositoryMonitorStore == nil {
		return nil
	}
	record.MonitorNamespace = monitor.Namespace
	record.MonitorName = monitor.Name
	record.MonitorGeneration = monitor.Generation
	if record.Actor == "" {
		record.Actor = "orka-controller"
	}
	if err := h.repositoryMonitorStore.UpdateGitHubMutationRecord(ctx, record); err != nil {
		return err
	}
	metrics.RecordRepositoryMonitorGitHubMutation(record.Operation, record.Status)
	return nil
}
