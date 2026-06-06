package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
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
	repositoryMonitorSkipReasonPending   = "review_pending"
)

type repositoryMonitorPullRequest struct {
	Number         int64
	Title          string
	Author         string
	State          string
	Labels         []string
	BaseBranch     string
	HeadBranch     string
	BaseSHA        string
	HeadSHA        string
	Draft          bool
	MergeableState string
}

func (r *RepositoryMonitorReconciler) processPullRequestInventoryRun(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, run *store.MonitorRun, owner, repository string) (int, int, error) {
	if err := validateRepositoryMonitorRunTargetKind(run); err != nil {
		return 0, 0, err
	}
	if !repositoryMonitorPullRequestsEnabled(monitor.Spec) {
		return 0, 0, r.createMonitorEvent(ctx, monitor, run.ID, repositoryMonitorPullRequestKind, 0, "", "inventory_skipped", "Pull request monitoring is disabled", nil)
	}

	token, err := r.repositoryMonitorGitHubToken(ctx, monitor)
	if err != nil {
		return 0, 0, err
	}

	baseBranch := effectiveRepositoryMonitorBranch(monitor)
	pullRequests, err := r.listRepositoryMonitorPullRequests(ctx, owner, repository, token, baseBranch)
	if err != nil {
		return 0, 0, err
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
	skipped := 0

	for _, pr := range pullRequests {
		existing, err := r.Store.GetMonitorItem(ctx, monitor.Namespace, monitor.Name, repositoryMonitorPullRequestKind, fmt.Sprintf("%d", pr.Number))
		if err != nil && !errorsIsStoreNotFound(err) {
			return selected, skipped, err
		}
		item := repositoryMonitorItemFromPullRequest(monitor, pr, existing)

		skipReason := repositoryMonitorPullRequestSkipReason(monitor.Spec, pr, existing, includeDrafts, selected, maxPerRun, run)
		if skipReason != "" {
			skipped++
			if skipReason == repositoryMonitorSkipReasonPending {
				item.LastVerdict = repositoryMonitorRunPhaseQueued
			} else {
				item.LastVerdict = repositoryMonitorVerdictSkipped
			}
			item.SkipReason = skipReason
			if err := r.Store.UpsertMonitorItem(ctx, item); err != nil {
				return selected, skipped, err
			}
			if err := r.createMonitorEvent(ctx, monitor, run.ID, repositoryMonitorPullRequestKind, pr.Number, pr.HeadSHA, "item_skipped", fmt.Sprintf("Pull request #%d skipped: %s", pr.Number, skipReason), map[string]any{
				"reason": skipReason,
				"labels": pr.Labels,
			}); err != nil {
				return selected, skipped, err
			}
			continue
		}

		selected++
		item.LastVerdict = "queued"
		item.SkipReason = ""
		if err := r.Store.UpsertMonitorItem(ctx, item); err != nil {
			return selected, skipped, err
		}
		if err := r.createMonitorEvent(ctx, monitor, run.ID, repositoryMonitorPullRequestKind, pr.Number, pr.HeadSHA, "item_selected", fmt.Sprintf("Pull request #%d selected for review", pr.Number), nil); err != nil {
			return selected, skipped, err
		}
	}

	if repositoryMonitorRunCoversFullInventory(run) {
		if err := r.retireMissingRepositoryMonitorPullRequests(ctx, monitor, run, seenPullRequestKeys); err != nil {
			return selected, skipped, err
		}
	}

	return selected, skipped, nil
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
	if spec.Targets.Issues.Enabled {
		return fmt.Errorf("spec.targets.issues is not supported; only pull request monitoring is supported")
	}
	if spec.Targets.Commits.Enabled {
		return fmt.Errorf("spec.targets.commits is not supported; only pull request monitoring is supported")
	}
	if !repositoryMonitorPullRequestsEnabled(spec) {
		return fmt.Errorf("spec.targets.pullRequests.enabled must be true; only pull request monitoring is supported")
	}
	if spec.Review.RequireGreenCI {
		return fmt.Errorf("spec.review.requireGreenCI is not supported until repository monitor CI state collection is available")
	}
	return nil
}

func validateRepositoryMonitorRunTargetKind(run *store.MonitorRun) error {
	if run == nil || strings.TrimSpace(run.TargetKind) == "" || run.TargetKind == repositoryMonitorPullRequestKind {
		return nil
	}
	return fmt.Errorf("targetKind %q is not supported; only pull_request monitor runs are supported", run.TargetKind)
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
	if run != nil && run.TargetSHA != "" && pr.HeadSHA != "" && run.TargetSHA != pr.HeadSHA {
		return "head_sha_mismatch"
	}
	if blockedLabel := repositoryMonitorBlockedLabel(spec, pr.Labels); blockedLabel != "" {
		return "blocked_label"
	}
	if existing != nil && existing.LastVerdict == repositoryMonitorRunPhaseQueued && existing.HeadSHA != "" && existing.HeadSHA == pr.HeadSHA {
		return repositoryMonitorSkipReasonPending
	}
	if existing != nil && existing.LastReviewedHeadSHA != "" && existing.LastReviewedHeadSHA == pr.HeadSHA {
		return "already_reviewed"
	}
	if selected >= maxPerRun {
		return "over_limit"
	}
	return ""
}

func repositoryMonitorBlockedLabel(spec corev1alpha1.RepositoryMonitorSpec, labels []string) string {
	blocked := map[string]struct{}{}
	for _, label := range append(spec.Policy.ProtectedLabels, spec.Policy.PauseLabels...) {
		label = strings.ToLower(strings.TrimSpace(label))
		if label != "" {
			blocked[label] = struct{}{}
		}
	}
	for _, label := range labels {
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
		return nil, fmt.Errorf("GitHub pull request inventory returned %d: %s", resp.StatusCode, string(respBody))
	}

	var response []struct {
		Number         int64  `json:"number"`
		Title          string `json:"title"`
		State          string `json:"state"`
		Draft          bool   `json:"draft"`
		MergeableState string `json:"mergeable_state"`
		User           struct {
			Login string `json:"login"`
		} `json:"user"`
		Base struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"base"`
		Head struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to parse GitHub pull request inventory response: %w", err)
	}

	items := make([]repositoryMonitorPullRequest, 0, len(response))
	for _, pr := range response {
		labels := make([]string, 0, len(pr.Labels))
		for _, label := range pr.Labels {
			labels = append(labels, label.Name)
		}
		items = append(items, repositoryMonitorPullRequest{
			Number:         pr.Number,
			Title:          pr.Title,
			Author:         pr.User.Login,
			State:          pr.State,
			Labels:         labels,
			BaseBranch:     pr.Base.Ref,
			HeadBranch:     pr.Head.Ref,
			BaseSHA:        pr.Base.SHA,
			HeadSHA:        pr.Head.SHA,
			Draft:          pr.Draft,
			MergeableState: pr.MergeableState,
		})
	}
	return items, nil
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
