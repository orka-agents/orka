package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

const (
	mergeMethodField = "mergeMethod"
)

const (
	githubPermissionAdmin    = "admin"
	githubPermissionMaintain = "maintain"
	bearerAuthScheme         = "Bearer"
)

const (
	repositoryMonitorAutomergeStateMerged     = "merged"
	repositoryMonitorAutomergeStateMergeReady = "merge_ready"
	repositoryMonitorAutomergeStateBlocked    = "blocked"
	repositoryMonitorAutomergeStateStarted    = "started"
	repositoryMonitorAutomergeStatePending    = repositoryMonitorReviewTaskStatePending
	repositoryMonitorAutomergeReasonCIPending = "ci_pending"
)

func (r *RepositoryMonitorReconciler) repositoryMonitorValidationAllowsAutomerge(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, item *store.MonitorItem, headSHA string) (bool, error) {
	if r.Store == nil || item == nil || strings.TrimSpace(item.LastReviewID) == "" {
		return false, nil
	}
	record, err := r.Store.GetReviewRecord(ctx, monitor.Namespace, item.LastReviewID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("load readiness validation review record: %w", err)
	}
	if record.MonitorName != monitor.Name || record.Kind != repositoryMonitorPullRequestKind ||
		record.Number != item.Number || record.HeadSHA != headSHA || record.Verdict != repositoryMonitorReviewVerdictPassed ||
		!repositoryMonitorReviewRecordAllowsAutomerge(monitor, record) {
		return false, nil
	}
	return true, nil
}

func repositoryMonitorAutomergeRepairStateBlocks(state string) bool {
	switch strings.TrimSpace(state) {
	case "", repositoryMonitorRepairPhaseSucceeded:
		return false
	default:
		return true
	}
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
	total := -1
	var checks []struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("%s/repos/%s/%s/commits/%s/check-runs?per_page=100&page=%d", baseURL, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(sha), page)
		var response struct {
			TotalCount int `json:"total_count"`
			CheckRuns  []struct {
				ID         int64  `json:"id"`
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"check_runs"`
		}
		if err := r.fetchRepositoryMonitorAuthorizedJSON(ctx, endpoint, token, &response); err != nil {
			return repositoryMonitorCIResult{}, err
		}
		if total < 0 {
			total = response.TotalCount
		}
		checks = append(checks, response.CheckRuns...)
		if len(checks) >= total || len(response.CheckRuns) == 0 {
			break
		}
	}
	if total <= 0 {
		return r.repositoryMonitorCheckCommitStatus(ctx, baseURL, owner, repo, token, sha)
	}
	if len(checks) < total {
		return repositoryMonitorCIResult{reason: "ci_checks_incomplete"}, nil
	}
	var pending, failed []string
	considered := 0
	for _, check := range checks {
		considered++
		if check.Status != "completed" {
			pending = append(pending, fmt.Sprintf("%s:%s/%s", check.Name, check.Status, check.Conclusion))
			continue
		}
		if !repositoryMonitorCheckRunConclusionPassing(check.Conclusion) {
			failed = append(failed, fmt.Sprintf("%s:%s/%s", check.Name, check.Status, check.Conclusion))
		}
	}
	if len(failed) > 0 {
		return repositoryMonitorCIResult{reason: repositoryMonitorCINotGreen}, nil
	}
	if len(pending) > 0 {
		return repositoryMonitorCIResult{reason: repositoryMonitorAutomergeReasonCIPending}, nil
	}
	status, err := r.repositoryMonitorCheckCommitStatus(ctx, baseURL, owner, repo, token, sha)
	if err != nil {
		return repositoryMonitorCIResult{}, err
	}
	if status.reason == "ci_checks_missing" && considered > 0 {
		return repositoryMonitorCIResult{passed: true}, nil
	}
	return status, nil
}

func repositoryMonitorCheckRunConclusionPassing(conclusion string) bool {
	switch strings.ToLower(strings.TrimSpace(conclusion)) {
	case "success", "neutral", "skipped":
		return true
	default:
		return false
	}
}

func (r *RepositoryMonitorReconciler) repositoryMonitorCheckCommitStatus(ctx context.Context, baseURL, owner, repo, token, sha string) (repositoryMonitorCIResult, error) {
	owned, err := r.repositoryMonitorOwnedReadinessStatuses(ctx, sha)
	if err != nil {
		return repositoryMonitorCIResult{}, err
	}
	considered, pending := 0, false
	// Combined status is paginated and contains only the latest status for each
	// context. Recompute after excluding audited IDs; its aggregate includes us.
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("%s/repos/%s/%s/commits/%s/status?per_page=100&page=%d", baseURL, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(sha), page)
		var response struct {
			TotalCount int                             `json:"total_count"`
			Statuses   []repositoryMonitorCommitStatus `json:"statuses"`
		}
		if err := r.fetchRepositoryMonitorAuthorizedJSON(ctx, endpoint, token, &response); err != nil {
			return repositoryMonitorCIResult{}, err
		}
		for _, status := range response.Statuses {
			if _, skip := owned[status.ID]; skip {
				continue
			}
			considered++
			switch status.State {
			case repositoryMonitorStatusSuccess:
			case repositoryMonitorStatusPending:
				pending = true
			default:
				return repositoryMonitorCIResult{reason: repositoryMonitorCINotGreen}, nil
			}
		}
		if len(response.Statuses) < 100 {
			if page*100-100+len(response.Statuses) < response.TotalCount {
				return repositoryMonitorCIResult{reason: "ci_checks_incomplete"}, nil
			}
			break
		}
	}
	if pending {
		return repositoryMonitorCIResult{reason: repositoryMonitorAutomergeReasonCIPending}, nil
	}
	if considered == 0 {
		return repositoryMonitorCIResult{reason: "ci_checks_missing"}, nil
	}
	return repositoryMonitorCIResult{passed: true}, nil
}

func (r *RepositoryMonitorReconciler) fetchRepositoryMonitorAuthorizedJSON(ctx context.Context, endpoint, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", strings.Join([]string{bearerAuthScheme, token}, " "))
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
