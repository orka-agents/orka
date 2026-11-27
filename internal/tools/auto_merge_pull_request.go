/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

type AutoMergePullRequestTool struct {
	k8sClient  client.Client
	apiBaseURL string
}

type AutoMergePullRequestArgs struct {
	TaskName      string `json:"task_name"`
	PRNumber      int    `json:"pr_number"`
	MergeMethod   string `json:"merge_method,omitempty"`
	CommitTitle   string `json:"commit_title,omitempty"`
	CommitMessage string `json:"commit_message,omitempty"`
	Timeout       string `json:"timeout,omitempty"`
}

type AutoMergePullRequestResult struct {
	Merged        bool   `json:"merged"`
	ChecksPassed  bool   `json:"checks_passed"`
	SHA           string `json:"sha,omitempty"`
	Message       string `json:"message"`
	Outcome       string `json:"outcome"`
	ChecksDetails string `json:"checks_details,omitempty"`
}

type CICheckResult struct {
	Passed  bool
	Failed  bool
	Pending bool
	Details string
}

var errNoCIChecksConfigured = errors.New("no CI checks configured for this PR")

func NewAutoMergePullRequestTool(k8sClient client.Client) *AutoMergePullRequestTool {
	return &AutoMergePullRequestTool{
		k8sClient: k8sClient,
	}
}

func (t *AutoMergePullRequestTool) Name() string {
	return autoMergePullRequestToolName
}

func (t *AutoMergePullRequestTool) Description() string {
	return "Poll GitHub CI checks and automatically merge a pull request when all checks pass. " +
		"Waits up to the specified timeout, retrying on transient errors."
}

func (t *AutoMergePullRequestTool) Parameters() json.RawMessage {
	schema := map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{taskNameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: childWorkspaceTaskDescription}, githubPRNumberField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeInteger, jsonSchemaDescriptionField: "GitHub pull request number to merge"}, mergeMethodField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Merge method: 'merge', 'squash', or 'rebase'. Defaults to 'squash'",
		jsonSchemaEnumField: []string{mergeMethodMerge, defaultMergeMethod, mergeMethodRebase},
	}, githubCommitTitleField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Custom merge commit title"},
		githubCommitMessageField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Custom merge commit message"}, timeoutField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Maximum time to wait for CI checks (e.g. '30m', '1h'). Defaults to '30m'"},
	}, jsonSchemaRequiredField: []string{taskNameField, githubPRNumberField},
	}
	data, _ := json.Marshal(schema)
	return data
}

func (t *AutoMergePullRequestTool) Execute(ctx context.Context, argsJSON json.RawMessage) (string, error) {
	var args AutoMergePullRequestArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	if args.TaskName == "" || args.PRNumber == 0 {
		return "", fmt.Errorf("task_name and pr_number are required")
	}

	if args.MergeMethod == "" {
		args.MergeMethod = defaultMergeMethod
	}
	if err := validateMergeMethod(args.MergeMethod); err != nil {
		return "", err
	}

	if args.Timeout == "" {
		args.Timeout = "30m"
	}

	timeout, err := time.ParseDuration(args.Timeout)
	if err != nil {
		return "", fmt.Errorf("invalid timeout %q: %w", args.Timeout, err)
	}

	ns := os.Getenv(envOrkaTaskNamespace)
	if ns == "" {
		ns = defaultNamespace
	}

	var task corev1alpha1.Task
	if err := t.k8sClient.Get(ctx, types.NamespacedName{Name: args.TaskName, Namespace: ns}, &task); err != nil {
		return "", fmt.Errorf("failed to get task %s: %w", args.TaskName, err)
	}

	ws := taskWorkspace(&task)
	if ws == nil {
		return "", fmt.Errorf("task %s does not have workspace configuration", args.TaskName)
	}

	repoURL := ws.GitRepo
	if repoURL == "" {
		return "", fmt.Errorf("task %s workspace has no gitRepo configured", args.TaskName)
	}

	owner, repo, err := parseGitHubRepo(repoURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse GitHub repo from %s: %w", repoURL, err)
	}

	if ws.ForgeCredentialRef == nil {
		return "", fmt.Errorf("task %s workspace has no forgeCredentialRef configured", args.TaskName)
	}

	token, err := resolveForgeCredentialToken(ctx, t.k8sClient, ns, ws.ForgeCredentialRef)
	if err != nil {
		return "", err
	}

	baseURL := githubAPIBaseURL
	if t.apiBaseURL != "" {
		baseURL = t.apiBaseURL
	}

	logger := log.FromContext(ctx)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	deadline := time.After(timeout)

	pc := &pollContext{
		token: token, owner: owner, repo: repo,
		prNumber: args.PRNumber, mergeMethod: args.MergeMethod,
		commitTitle: args.CommitTitle, commitMessage: args.CommitMessage,
		baseURL: baseURL, logger: logger,
	}

	for first := true; ; first = false {
		if !first {
			select {
			case <-ctx.Done():
				return marshalResult(AutoMergePullRequestResult{
					Message: "context cancelled while waiting for CI checks",
					Outcome: timeoutField,
				}), nil
			case <-deadline:
				return marshalResult(AutoMergePullRequestResult{
					Message: fmt.Sprintf("timed out after %s waiting for CI checks to pass", args.Timeout),
					Outcome: timeoutField,
				}), nil
			case <-ticker.C:
			}
		}

		result, done, err := pc.pollOnce(ctx)
		if err != nil {
			return "", err
		}
		if done {
			return marshalResult(*result), nil
		}
	}
}

type pollContext struct {
	token, owner, repo                      string
	prNumber                                int
	mergeMethod, commitTitle, commitMessage string
	baseURL                                 string
	logger                                  logr.Logger
}

func (pc *pollContext) pollOnce(ctx context.Context) (*AutoMergePullRequestResult, bool, error) {
	headSHA, state, merged, err := getGitHubPRDetails(ctx, pc.token, pc.owner, pc.repo, pc.prNumber, pc.baseURL)
	if err != nil {
		if isTransientHTTPError(err) {
			pc.logger.Info("transient GitHub API error, will retry", "error", err)
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to get PR details: %w", err)
	}

	if merged {
		return &AutoMergePullRequestResult{
			Merged: true, ChecksPassed: true,
			Message: "Pull request was already merged", Outcome: "already_merged",
		}, true, nil
	}

	if state == githubPRStateClosed {
		return &AutoMergePullRequestResult{
			Message: "Pull request is closed", Outcome: githubPRStateClosed,
		}, true, nil
	}

	ciResult, err := checkCIStatusDetailed(ctx, pc.token, pc.owner, pc.repo, headSHA, pc.baseURL)
	if err != nil {
		if isTransientHTTPError(err) {
			pc.logger.Info("transient GitHub API error, will retry", "error", err)
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to check CI status: %w", err)
	}

	if ciResult.Failed {
		return &AutoMergePullRequestResult{
			ChecksDetails: ciResult.Details,
			Message:       fmt.Sprintf("CI checks failed: %s", ciResult.Details),
			Outcome:       "ci_failed",
		}, true, nil
	}

	if ciResult.Passed {
		sha, err := mergeGitHubPR(ctx, pc.token, pc.owner, pc.repo, pc.prNumber, pc.mergeMethod, pc.commitTitle, pc.commitMessage, pc.baseURL)
		if err != nil {
			return nil, false, fmt.Errorf("failed to merge pull request: %w", err)
		}
		return &AutoMergePullRequestResult{
			Merged: true, ChecksPassed: true, SHA: sha,
			Message: pullRequestMergedMessage, Outcome: mergedStatusString,
		}, true, nil
	}

	pc.logger.Info("CI checks still pending, will retry", "pr", pc.prNumber, "details", ciResult.Details)
	return nil, false, nil
}

func marshalResult(r AutoMergePullRequestResult) string {
	data, _ := json.Marshal(r)
	return string(data)
}

func getGitHubPRDetails(ctx context.Context, token, owner, repo string, prNumber int, baseURL string) (headSHA, state string, merged bool, err error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", baseURL, owner, repo, prNumber)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", false, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", false, &githubAPIError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
		}
	}

	var prResp struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		State  string `json:"state"`
		Merged bool   `json:"merged"`
	}
	if err := json.Unmarshal(respBody, &prResp); err != nil {
		return "", "", false, fmt.Errorf("failed to parse GitHub response: %w", err)
	}

	return prResp.Head.SHA, prResp.State, prResp.Merged, nil
}

func checkCIStatusDetailed(ctx context.Context, token, owner, repo, sha, baseURL string) (CICheckResult, error) {
	var runs []checkRun
	total := 0
	for page := 1; page <= maxCheckRunPages; page++ {
		checkResp, err := fetchCheckRunsPage(ctx, token, owner, repo, sha, baseURL, page)
		if err != nil {
			return CICheckResult{}, err
		}
		total = checkResp.TotalCount
		runs = append(runs, checkResp.CheckRuns...)
		if len(checkResp.CheckRuns) < checkRunsPerPage || len(runs) >= total {
			break
		}
	}

	if total == 0 {
		return CICheckResult{}, errNoCIChecksConfigured
	}

	var failed, pending []string
	for _, check := range runs {
		if check.Status != "completed" {
			pending = append(pending, fmt.Sprintf("%s (status=%s)", check.Name, check.Status))
			continue
		}
		switch check.Conclusion {
		case successStatusString, "neutral", "skipped":
		case "failure", cancelledStatusString, "timed_out", "action_required", "stale":
			failed = append(failed, fmt.Sprintf("%s (conclusion=%s)", check.Name, check.Conclusion))
		default:
			failed = append(failed, fmt.Sprintf("%s (conclusion=%s)", check.Name, check.Conclusion))
		}
	}

	if len(failed) > 0 {
		return CICheckResult{
			Failed:  true,
			Details: strings.Join(failed, "; "),
		}, nil
	}

	if len(pending) > 0 {
		return CICheckResult{
			Pending: true,
			Details: strings.Join(pending, "; "),
		}, nil
	}

	if len(runs) < total {
		return CICheckResult{
			Pending: true,
			Details: fmt.Sprintf("%d of %d check runs examined; the rest could not be read, so CI is not reported as passed", len(runs), total),
		}, nil
	}

	return CICheckResult{Passed: true}, nil
}

const (
	checkRunsPerPage = 100
	maxCheckRunPages = 10
)

type checkRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type checkRunsPage struct {
	TotalCount int        `json:"total_count"`
	CheckRuns  []checkRun `json:"check_runs"`
}

const checkRunsErrorBodyLimit = 4 << 10

func fetchCheckRunsPage(ctx context.Context, token, owner, repo, sha, baseURL string, page int) (checkRunsPage, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/commits/%s/check-runs?per_page=%d&page=%d", baseURL, owner, repo, sha, checkRunsPerPage, page)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return checkRunsPage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return checkRunsPage{}, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, checkRunsErrorBodyLimit))
		return checkRunsPage{}, &githubAPIError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
		}
	}
	respBody, err := readGitHubResponse(resp.Body, 1<<20)
	if err != nil {
		return checkRunsPage{}, err
	}

	var checkResp checkRunsPage
	if err := json.Unmarshal(respBody, &checkResp); err != nil {
		return checkRunsPage{}, fmt.Errorf("failed to parse check runs response: %w", err)
	}
	return checkResp, nil
}

type githubAPIError struct {
	StatusCode int
	Body       string
}

func (e *githubAPIError) Error() string {
	return fmt.Sprintf("GitHub API returned %d: %s", e.StatusCode, e.Body)
}

func isTransientHTTPError(err error) bool {
	if apiErr, ok := errors.AsType[*githubAPIError](err); ok {
		return apiErr.StatusCode == 429 || apiErr.StatusCode >= 500
	}
	return false
}

var _ Tool = (*AutoMergePullRequestTool)(nil)
