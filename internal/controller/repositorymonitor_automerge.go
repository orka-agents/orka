package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/security"
	"github.com/sozercan/orka/internal/store"
)

const (
	repositoryMonitorActionAutomerge       = "pr_automerge"
	repositoryMonitorAutomergeStateMerged  = "merged"
	repositoryMonitorAutomergeStateBlocked = "blocked"
	repositoryMonitorAutomergeStateFailed  = "failed"
	repositoryMonitorAutomergeStateStarted = "started"
	repositoryMonitorAutomergeGateEnv      = "ORKA_REPOSITORY_MONITOR_AUTOMERGE_GATE"
	repositoryMonitorAutomergeMethodSquash = "squash"
)

func (r *RepositoryMonitorReconciler) tryProcessPullRequestAutomergeCommand(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, run *store.MonitorRun, command *store.CommandEvent, owner, repository string, pr repositoryMonitorPullRequest, item *store.MonitorItem) (bool, error) {
	if command.Intent != "automerge" {
		return false, nil
	}
	verdict, reason := r.repositoryMonitorAutomergeGate(ctx, monitor, command, pr, item)
	if verdict != repositoryMonitorIssueVerdictReady {
		item.AutomergeState = repositoryMonitorAutomergeStateBlocked
		item.SkipReason = reason
		if err := r.Store.UpsertMonitorItem(ctx, item); err != nil {
			return true, err
		}
		return true, r.createRepositoryMonitorAutomergeRecord(ctx, monitor, command, item, repositoryMonitorAutomergeStateBlocked, reason, map[string]any{"reason": reason})
	}
	if err := r.createRepositoryMonitorAutomergeRecord(ctx, monitor, command, item, repositoryMonitorAutomergeStateStarted, "merge attempt started", map[string]any{"headSHA": pr.HeadSHA}); err != nil {
		return true, err
	}
	method := repositoryMonitorAutomergeMethod(monitor)
	sha, err := r.mergeRepositoryMonitorPullRequest(ctx, monitor, owner, repository, pr.Number, method)
	if err != nil {
		item.AutomergeState = repositoryMonitorAutomergeStateFailed
		item.SkipReason = "automerge_failed"
		if updateErr := r.Store.UpsertMonitorItem(ctx, item); updateErr != nil {
			return true, updateErr
		}
		return true, r.createRepositoryMonitorAutomergeRecord(ctx, monitor, command, item, repositoryMonitorAutomergeStateFailed, err.Error(), map[string]any{"mergeMethod": method, "error": err.Error()})
	}
	item.AutomergeState = repositoryMonitorAutomergeStateMerged
	item.SkipReason = ""
	item.State = "merged"
	if err := r.Store.UpsertMonitorItem(ctx, item); err != nil {
		return true, err
	}
	if err := r.createRepositoryMonitorAutomergeRecord(ctx, monitor, command, item, repositoryMonitorAutomergeStateMerged, "pull request merged", map[string]any{"mergeMethod": method, "mergeSHA": sha}); err != nil {
		return true, err
	}
	return true, r.createMonitorEvent(ctx, monitor, run.ID, repositoryMonitorPullRequestKind, pr.Number, pr.HeadSHA, "automerge_succeeded", fmt.Sprintf("Pull request #%d automerged", pr.Number), map[string]any{"mergeSHA": sha, "mergeMethod": method})
}

func (r *RepositoryMonitorReconciler) repositoryMonitorAutomergeGate(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, command *store.CommandEvent, pr repositoryMonitorPullRequest, item *store.MonitorItem) (string, string) {
	switch {
	case !monitor.Spec.Automerge.Enabled:
		return repositoryMonitorIssuePhaseBlocked, "automerge_disabled"
	case repositoryMonitorAutomergeRequiresGlobalGate(monitor) && !strings.EqualFold(os.Getenv(repositoryMonitorAutomergeGateEnv), "true"):
		return repositoryMonitorIssuePhaseBlocked, "global_merge_gate_disabled"
	case !repositoryMonitorAutomergeActorAllowed(monitor, command.Permission):
		return repositoryMonitorIssuePhaseBlocked, "actor_permission_insufficient"
	case command.HeadSHA == "" || command.HeadSHA != pr.HeadSHA:
		return repositoryMonitorIssuePhaseBlocked, "stale_head_sha"
	case repositoryMonitorBlockedLabel(monitor.Spec, pr.Labels) != "":
		return repositoryMonitorIssuePhaseBlocked, repositoryMonitorSkipReasonBlockedLabel
	case item.LastVerdict != repositoryMonitorReviewVerdictPassed || item.LastReviewedHeadSHA != pr.HeadSHA:
		return repositoryMonitorIssuePhaseBlocked, "orka_review_not_passed"
	case item.RepairState != "":
		return repositoryMonitorIssuePhaseBlocked, "active_or_failed_repair_state"
	case !strings.EqualFold(pr.MergeableState, "clean"):
		return repositoryMonitorIssuePhaseBlocked, "pull_request_not_mergeable"
	}
	ci, err := r.repositoryMonitorCheckCI(ctx, monitor, pr.HeadSHA)
	if err != nil {
		return repositoryMonitorIssuePhaseBlocked, "ci_check_error"
	}
	if !ci.passed {
		return repositoryMonitorIssuePhaseBlocked, ci.reason
	}
	return repositoryMonitorIssueVerdictReady, ""
}

func repositoryMonitorAutomergeRequiresGlobalGate(monitor *corev1alpha1.RepositoryMonitor) bool {
	return monitor.Spec.Automerge.RequireGlobalMergeGate == nil || *monitor.Spec.Automerge.RequireGlobalMergeGate
}

func repositoryMonitorAutomergeActorAllowed(monitor *corev1alpha1.RepositoryMonitor, permission string) bool {
	permission = strings.ToLower(strings.TrimSpace(permission))
	allowed := []string{"maintain", "admin"}
	for _, policyPermission := range monitor.Spec.Policy.AllowedRepositoryPermissions {
		if strings.EqualFold(strings.TrimSpace(policyPermission), "write") {
			allowed = append(allowed, "write")
		}
	}
	return slices.Contains(allowed, permission)
}

func repositoryMonitorAutomergeMethod(monitor *corev1alpha1.RepositoryMonitor) string {
	allowed := monitor.Spec.Automerge.AllowedMergeMethods
	if len(allowed) == 0 {
		return repositoryMonitorAutomergeMethodSquash
	}
	for _, method := range allowed {
		switch strings.TrimSpace(method) {
		case repositoryMonitorAutomergeMethodSquash, "merge", "rebase":
			return strings.TrimSpace(method)
		}
	}
	return repositoryMonitorAutomergeMethodSquash
}

type repositoryMonitorCIResult struct {
	passed bool
	reason string
}

func (r *RepositoryMonitorReconciler) repositoryMonitorCheckCI(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, sha string) (repositoryMonitorCIResult, error) {
	token, err := r.repositoryMonitorGitHubToken(ctx, monitor)
	if err != nil {
		return repositoryMonitorCIResult{}, err
	}
	owner, repo, err := security.ParseGitHubRepositoryURL(monitor.Spec.RepoURL)
	if err != nil {
		return repositoryMonitorCIResult{}, err
	}
	baseURL := strings.TrimRight(r.GitHubAPIBaseURL, "/")
	if baseURL == "" {
		baseURL = repositoryMonitorDefaultGitHubAPIBaseURL
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/commits/%s/check-runs", baseURL, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(sha))
	var response struct {
		TotalCount int `json:"total_count"`
		CheckRuns  []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := r.fetchRepositoryMonitorAuthorizedJSON(ctx, endpoint, token, &response); err != nil {
		return repositoryMonitorCIResult{}, err
	}
	if response.TotalCount == 0 {
		return repositoryMonitorCIResult{reason: "ci_checks_missing"}, nil
	}
	var pendingOrFailed []string
	for _, check := range response.CheckRuns {
		if check.Status != "completed" || check.Conclusion != "success" {
			pendingOrFailed = append(pendingOrFailed, fmt.Sprintf("%s:%s/%s", check.Name, check.Status, check.Conclusion))
		}
	}
	if len(pendingOrFailed) > 0 {
		return repositoryMonitorCIResult{reason: "ci_not_green"}, nil
	}
	return repositoryMonitorCIResult{passed: true}, nil
}

func (r *RepositoryMonitorReconciler) fetchRepositoryMonitorAuthorizedJSON(ctx context.Context, endpoint, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", strings.Join([]string{"Bearer", token}, " "))
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	httpClient := r.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(resp.Body, repositoryMonitorGitHubResponseLimit))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &repositoryMonitorGitHubAPIError{Operation: "automerge gate", StatusCode: resp.StatusCode, Body: string(data)}
	}
	return json.Unmarshal(data, out)
}

func (r *RepositoryMonitorReconciler) mergeRepositoryMonitorPullRequest(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, owner, repository string, number int64, method string) (string, error) {
	token, err := r.repositoryMonitorGitHubToken(ctx, monitor)
	if err != nil {
		return "", err
	}
	baseURL := strings.TrimRight(r.GitHubAPIBaseURL, "/")
	if baseURL == "" {
		baseURL = repositoryMonitorDefaultGitHubAPIBaseURL
	}
	payload, _ := json.Marshal(map[string]any{"merge_method": method})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, fmt.Sprintf("%s/repos/%s/%s/pulls/%d/merge", baseURL, url.PathEscape(owner), url.PathEscape(repository), number), bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", strings.Join([]string{"Bearer", token}, " "))
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	httpClient := r.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(resp.Body, repositoryMonitorGitHubResponseLimit))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &repositoryMonitorGitHubAPIError{Operation: "automerge", StatusCode: resp.StatusCode, Body: string(data)}
	}
	var parsed struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", err
	}
	return parsed.SHA, nil
}

func (r *RepositoryMonitorReconciler) createRepositoryMonitorAutomergeRecord(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, command *store.CommandEvent, item *store.MonitorItem, verdict, summary string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["commandEventID"] = command.ID
	payload["headSHA"] = command.HeadSHA
	payloadJSON, _ := json.Marshal(payload)
	record := &store.ActionRecord{
		ID:                "act-" + repositoryMonitorShortHash(command.ID+"-automerge-"+verdict),
		MonitorNamespace:  monitor.Namespace,
		MonitorName:       monitor.Name,
		Kind:              repositoryMonitorPullRequestKind,
		Number:            item.Number,
		ActionKind:        repositoryMonitorActionAutomerge,
		HeadSHA:           command.HeadSHA,
		CommandEventID:    command.ID,
		MonitorGeneration: monitor.Generation,
		Verdict:           verdict,
		Summary:           boundedString(summary, repositoryMonitorReviewTextMaxRunes),
		PayloadJSON:       string(payloadJSON),
		CreatedAt:         time.Now(),
	}
	if err := r.Store.CreateActionRecord(ctx, record); err != nil && !strings.Contains(strings.ToLower(err.Error()), "constraint") {
		return err
	}
	return nil
}
