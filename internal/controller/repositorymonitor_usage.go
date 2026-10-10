package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

const (
	usageOutcomeRefreshInterval   = 5 * time.Minute
	usageOutcomeBacklogInterval   = 15 * time.Second
	usagePRCreatedMutationReason  = "issue_implementation_pr_created"
	usagePRAssistedMutationReason = "issue_implementation_pr_assisted"
)

func (r *RepositoryMonitorReconciler) usageOutcomeRequeueAfter(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor) (time.Duration, error) {
	usageStore, ok := r.Store.(store.UsageStore)
	if !ok || monitor.UID == "" {
		return 0, nil
	}
	now := time.Now().UTC()
	links, err := usageStore.ListUsagePullRequestLinks(ctx, monitor.Namespace, string(monitor.UID), now.Add(-usageOutcomeRefreshInterval), 1)
	if err != nil {
		return 0, err
	}
	if len(links) > 0 {
		return usageOutcomeBacklogInterval, nil
	}
	// Include recently observed links: they still need a timer for their next
	// refresh. Confirmed merges are excluded by the store.
	links, err = usageStore.ListUsagePullRequestLinks(ctx, monitor.Namespace, string(monitor.UID), now, 1)
	if len(links) > 0 {
		return usageOutcomeRefreshInterval, err
	}
	return 0, err
}

func validUsagePullRequestURL(rawURL, repository string, number int64) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil || number <= 0 || parsed.Scheme != urlSchemeHTTPS || !strings.EqualFold(parsed.Host, "github.com") ||
		parsed.User != nil || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return false
	}
	parts := strings.Split(parsed.Path, "/")
	return len(parts) == 5 && parts[0] == "" && parts[2] != "" && parts[3] == "pull" && parts[4] == fmt.Sprint(number) &&
		strings.EqualFold(parts[1]+"/"+parts[2], repository)
}

func (r *RepositoryMonitorReconciler) recordMonitorUsagePRLink(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, issue int64, task *corev1alpha1.Task, repository string, number int64, origin string) error {
	usageStore, ok := r.Store.(store.UsageStore)
	if !ok || monitor.UID == "" {
		return nil
	}
	workID, err := r.prepareMonitorUsageWork(ctx, monitor, repository, repositoryMonitorIssueKind, issue)
	if err != nil {
		return err
	}
	return usageStore.LinkUsagePullRequest(ctx, store.UsagePRLink{Namespace: monitor.Namespace, WorkID: workID,
		Repository: repository, Number: number, Origin: origin, EvidenceID: "publication/" + string(task.UID) + "/" + task.Name,
		LinkedAt: time.Now().UTC()})
}

// Refresh continues after Tasks finish and queries each linked PR directly,
// including closed PRs that no longer appear in the open-PR inventory.
func (r *RepositoryMonitorReconciler) refreshMonitorUsageOutcomes(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor) error {
	usageStore, ok := r.Store.(store.UsageStore)
	if !ok || monitor.UID == "" {
		return nil
	}
	links, err := usageStore.ListUsagePullRequestLinks(ctx, monitor.Namespace, string(monitor.UID), time.Now().UTC().Add(-usageOutcomeRefreshInterval), 20)
	if err != nil || len(links) == 0 {
		return err
	}
	token, tokenErr := r.repositoryMonitorGitHubToken(ctx, monitor)
	refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	seen := map[string]bool{}
	for _, link := range links {
		if refreshCtx.Err() != nil {
			break
		}
		key := fmt.Sprintf("%s#%d", link.Repository, link.Number)
		if seen[key] {
			continue
		}
		seen[key] = true
		observation := store.UsagePullRequest{Namespace: monitor.Namespace, NamespaceUID: link.NamespaceUID, Repository: link.Repository, Number: link.Number,
			URL: fmt.Sprintf("https://github.com/%s/pull/%d", link.Repository, link.Number), State: repositoryMonitorIssueUnknownValue,
			ReadinessReason: "GitHub state unavailable", ObservedAt: time.Now().UTC()}
		if tokenErr == nil {
			if current, err := r.fetchUsagePullRequest(refreshCtx, link.Repository, link.Number, token); err == nil {
				observation = current
				observation.Namespace = monitor.Namespace
				observation.NamespaceUID = link.NamespaceUID
			}
		}
		if err := usageStore.RecordUsagePullRequest(ctx, observation); err != nil {
			return err
		}
	}
	return nil
}

// GraphQL exposes GitHub's rules-aware merge state and review decision for the
// same head in one observation. Unknown decisions fail closed for readiness.
func (r *RepositoryMonitorReconciler) fetchUsagePullRequest(ctx context.Context, repository string, number int64, token string) (store.UsagePullRequest, error) {
	var result store.UsagePullRequest
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" {
		return result, fmt.Errorf("invalid usage PR repository")
	}
	query := `query($owner:String!,$name:String!,$number:Int!) {
	 repository(owner:$owner,name:$name) { nameWithOwner pullRequest(number:$number) {
	  id number url state createdAt mergedAt closedAt headRefOid isDraft mergeable mergeStateStatus reviewDecision
	 } }
	}`
	data, err := json.Marshal(map[string]any{"query": query, "variables": map[string]any{"owner": owner, "name": name, "number": number}})
	if err != nil {
		return result, err
	}
	baseURL := strings.TrimRight(r.GitHubAPIBaseURL, "/")
	if baseURL == "" {
		baseURL = repositoryMonitorDefaultGitHubAPIBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/graphql", bytes.NewReader(data))
	if err != nil {
		return result, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	client := r.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(req)
	if err != nil {
		return result, fmt.Errorf("GitHub usage outcome query failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return result, fmt.Errorf("GitHub usage outcome query returned HTTP %d", response.StatusCode)
	}
	body, err := readRepositoryMonitorGitHubResponse(response.Body, repositoryMonitorGitHubResponseLimit)
	if err != nil {
		return result, err
	}
	var decoded struct {
		Data struct {
			Repository *struct {
				NameWithOwner string `json:"nameWithOwner"`
				PullRequest   *struct {
					ID             string     `json:"id"`
					Number         int64      `json:"number"`
					URL            string     `json:"url"`
					State          string     `json:"state"`
					CreatedAt      *time.Time `json:"createdAt"`
					MergedAt       *time.Time `json:"mergedAt"`
					ClosedAt       *time.Time `json:"closedAt"`
					HeadSHA        string     `json:"headRefOid"`
					Draft          bool       `json:"isDraft"`
					Mergeable      string     `json:"mergeable"`
					MergeState     string     `json:"mergeStateStatus"`
					ReviewDecision *string    `json:"reviewDecision"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return result, fmt.Errorf("invalid GitHub usage outcome response")
	}
	repo := decoded.Data.Repository
	if len(decoded.Errors) > 0 || repo == nil || repo.PullRequest == nil || !strings.EqualFold(repo.NameWithOwner, repository) {
		return result, fmt.Errorf("GitHub did not confirm the requested repository and PR")
	}
	pr := repo.PullRequest
	if pr.ID == "" || pr.Number != number || !validUsagePullRequestURL(pr.URL, repository, number) {
		return result, fmt.Errorf("GitHub PR identity mismatch")
	}
	ready := pr.State == "OPEN" && !pr.Draft && pr.HeadSHA != "" && pr.Mergeable == "MERGEABLE" && pr.MergeState == "CLEAN" &&
		(pr.ReviewDecision == nil || *pr.ReviewDecision == "APPROVED")
	result = store.UsagePullRequest{Repository: strings.ToLower(repository), Number: number, GitHubID: pr.ID, URL: pr.URL,
		State: strings.ToLower(pr.State), HeadSHA: pr.HeadSHA, Ready: ready, CreatedAt: pr.CreatedAt, MergedAt: pr.MergedAt,
		ClosedAt: pr.ClosedAt, ObservedAt: time.Now().UTC()}
	if !ready && result.State == "open" {
		result.ReadinessReason = "Current head has not met GitHub checks, review or mergeability requirements"
	}
	return result, nil
}
