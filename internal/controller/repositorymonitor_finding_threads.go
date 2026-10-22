package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// repositoryMonitorAddressedPrefix opens an inline comment whose finding a
// later review found fixed.
const repositoryMonitorAddressedPrefix = "✅ Addressed in `"

// repositoryMonitorGraphQLNumberVariable names the pull request number in
// GitHub GraphQL queries.
const repositoryMonitorGraphQLNumberVariable = "number"

// repositoryMonitorInlineFindingMarker matches the finding ID in the hidden
// marker Orka appends to its inline comments.
var repositoryMonitorInlineFindingMarker = regexp.MustCompile(`<!-- orka:repo-monitor-inline review=\S+ finding=(f-[0-9a-f]+) -->`)

type repositoryMonitorReviewComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

// markRepositoryMonitorFixedFindingThreads marks the inline comments of the
// findings record found fixed: each comment gets an "Addressed in" note and
// its thread is resolved. Only comments on reviews Orka published are
// touched, so a comment that copies Orka's marker is ignored, and each
// finding is marked once.
func (r *RepositoryMonitorReconciler) markRepositoryMonitorFixedFindingThreads(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, item *store.MonitorItem, record *store.ReviewRecord) error {
	if r.Store == nil || !monitor.Spec.Review.Publish.Enabled || record == nil {
		return nil
	}
	findings, err := repositoryMonitorReviewFindingsFromRecord(record)
	if err != nil {
		return nil //nolint:nilerr // An unreadable record has no findings to mark.
	}
	fixed := map[string]bool{}
	for _, finding := range findings {
		if finding.Status == repositoryMonitorFindingStatusFixed && finding.ResolvedHead == record.HeadSHA && finding.ID != "" {
			fixed[finding.ID] = true
		}
	}
	if len(fixed) == 0 {
		return nil
	}
	token, err := r.repositoryMonitorGitHubToken(ctx, monitor)
	if err != nil {
		return err
	}
	owner, repository, err := security.ParseGitHubRepositoryURL(monitor.Spec.RepoURL)
	if err != nil {
		return err
	}
	targets, err := r.repositoryMonitorFixedFindingComments(ctx, monitor, owner, repository, token, item.Number, fixed)
	if err != nil || len(targets) == 0 {
		return err
	}
	threads, err := r.listRepositoryMonitorReviewThreads(ctx, owner, repository, token, item.Number)
	if err != nil {
		return err
	}
	note := repositoryMonitorAddressedPrefix + shortRepositoryMonitorHead(record.HeadSHA) + "`.\n\n"
	for _, target := range targets {
		if err := r.markRepositoryMonitorFindingAddressed(ctx, monitor, item.Number, record, target, threads, owner, repository, token, note); err != nil {
			return err
		}
	}
	return nil
}

// repositoryMonitorFindingComment is an inline comment Orka posted for a
// finding.
type repositoryMonitorFindingComment struct {
	findingID string
	comment   repositoryMonitorReviewComment
}

// repositoryMonitorFixedFindingComments returns the inline comments, on
// reviews Orka published for the pull request, of the fixed findings.
func (r *RepositoryMonitorReconciler) repositoryMonitorFixedFindingComments(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, owner, repository, token string, number int64, fixed map[string]bool) ([]repositoryMonitorFindingComment, error) {
	publishes, _, err := r.Store.ListReviewPublishRecords(ctx, store.ReviewPublishRecordFilter{
		Namespace: monitor.Namespace, MonitorName: monitor.Name, ItemKind: repositoryMonitorPullRequestKind, ItemNumber: number, Phase: repositoryMonitorPublishPhaseSucceeded, Limit: 100,
	})
	if err != nil {
		return nil, err
	}
	var targets []repositoryMonitorFindingComment
	for _, publish := range publishes {
		reviewID, err := strconv.ParseInt(strings.TrimSpace(publish.GitHubReviewID), 10, 64)
		if err != nil || reviewID <= 0 {
			continue
		}
		comments, err := r.listRepositoryMonitorReviewComments(ctx, owner, repository, token, number, reviewID)
		if err != nil {
			return nil, err
		}
		for _, comment := range comments {
			if match := repositoryMonitorInlineFindingMarker.FindStringSubmatch(comment.Body); match != nil && fixed[match[1]] {
				targets = append(targets, repositoryMonitorFindingComment{findingID: match[1], comment: comment})
			}
		}
	}
	return targets, nil
}

// markRepositoryMonitorFindingAddressed notes the fix on one comment and
// resolves its thread, once per finding and review.
func (r *RepositoryMonitorReconciler) markRepositoryMonitorFindingAddressed(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, number int64, record *store.ReviewRecord, target repositoryMonitorFindingComment, threads map[int64]repositoryMonitorReviewThread, owner, repository, token, note string) error {
	mutationID := "ghmut-" + repositoryMonitorShortHash(record.ID+"|"+target.findingID+"|addressed")
	mutation, err := r.Store.GetGitHubMutationRecord(ctx, monitor.Namespace, mutationID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		mutation = &store.GitHubMutationRecord{ID: mutationID, Operation: "mark_finding_addressed", TargetKind: repositoryMonitorPullRequestKind, TargetNumber: number, TargetSHA: record.HeadSHA, Reason: target.findingID, ExternalID: strconv.FormatInt(target.comment.ID, 10), Status: repositoryMonitorAutomergeStateStarted}
		if err := r.recordRepositoryMonitorGitHubMutation(ctx, monitor, mutation); err != nil {
			return err
		}
	case err != nil:
		return err
	case mutation.Status == repositoryMonitorRunPhaseSucceeded:
		return nil
	}
	markErr := func() error {
		if !strings.HasPrefix(target.comment.Body, repositoryMonitorAddressedPrefix) {
			if err := r.updateRepositoryMonitorReviewComment(ctx, owner, repository, token, target.comment.ID, note+target.comment.Body); err != nil {
				return err
			}
		}
		if thread, ok := threads[target.comment.ID]; ok && !thread.resolved {
			return r.resolveRepositoryMonitorReviewThread(ctx, token, thread.id)
		}
		return nil
	}()
	mutation.Status, mutation.Error = repositoryMonitorRunPhaseSucceeded, ""
	if markErr != nil {
		mutation.Status, mutation.Error = repositoryMonitorRunPhaseFailed, markErr.Error()
	}
	if err := r.updateRepositoryMonitorGitHubMutation(ctx, monitor, mutation); err != nil {
		return err
	}
	return markErr
}

func (r *RepositoryMonitorReconciler) listRepositoryMonitorReviewComments(ctx context.Context, owner, repository, token string, number, reviewID int64) ([]repositoryMonitorReviewComment, error) {
	var all []repositoryMonitorReviewComment
	baseURL := repositoryMonitorGitHubBaseURL(r)
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/reviews/%d/comments?per_page=%d&page=%d", baseURL, url.PathEscape(owner), url.PathEscape(repository), number, reviewID, repositoryMonitorGitHubPerPage, page)
		var comments []repositoryMonitorReviewComment
		if err := r.fetchRepositoryMonitorAuthorizedJSON(ctx, endpoint, token, &comments); err != nil {
			return nil, err
		}
		all = append(all, comments...)
		if len(comments) < repositoryMonitorGitHubPerPage {
			return all, nil
		}
	}
}

func (r *RepositoryMonitorReconciler) updateRepositoryMonitorReviewComment(ctx context.Context, owner, repository, token string, commentID int64, body string) error {
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls/comments/%d", repositoryMonitorGitHubBaseURL(r), url.PathEscape(owner), url.PathEscape(repository), commentID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	repositoryMonitorSetGitHubHeaders(req, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := repositoryMonitorHTTPClient(r).Do(req)
	if err != nil {
		return fmt.Errorf("GitHub review comment update request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	respBody, err := readRepositoryMonitorGitHubResponse(resp.Body, repositoryMonitorGitHubResponseLimit)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &repositoryMonitorGitHubAPIError{Operation: "review comment update request", StatusCode: resp.StatusCode, Body: string(respBody)}
	}
	return nil
}

type repositoryMonitorReviewThread struct {
	id       string
	resolved bool
}

// listRepositoryMonitorReviewThreads maps each thread's first comment ID to
// the thread.
func (r *RepositoryMonitorReconciler) listRepositoryMonitorReviewThreads(ctx context.Context, owner, repository, token string, number int64) (map[int64]repositoryMonitorReviewThread, error) {
	const query = `query($owner:String!,$name:String!,$number:Int!,$after:String) {
 repository(owner:$owner,name:$name) { pullRequest(number:$number) {
  reviewThreads(first:100, after:$after) { nodes { id isResolved comments(first:1) { nodes { databaseId } } } pageInfo { hasNextPage endCursor } }
 } }
}`
	threads := map[int64]repositoryMonitorReviewThread{}
	var after any
	for {
		var response struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						Nodes []struct {
							ID         string `json:"id"`
							IsResolved bool   `json:"isResolved"`
							Comments   struct {
								Nodes []struct {
									DatabaseID int64 `json:"databaseId"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := r.repositoryMonitorGitHubGraphQL(ctx, token, query, map[string]any{"owner": owner, "name": repository, repositoryMonitorGraphQLNumberVariable: number, "after": after}, &response); err != nil {
			return nil, err
		}
		page := response.Repository.PullRequest.ReviewThreads
		for _, node := range page.Nodes {
			if len(node.Comments.Nodes) > 0 {
				threads[node.Comments.Nodes[0].DatabaseID] = repositoryMonitorReviewThread{id: node.ID, resolved: node.IsResolved}
			}
		}
		if !page.PageInfo.HasNextPage || page.PageInfo.EndCursor == "" {
			return threads, nil
		}
		after = page.PageInfo.EndCursor
	}
}

func (r *RepositoryMonitorReconciler) resolveRepositoryMonitorReviewThread(ctx context.Context, token, threadID string) error {
	const mutation = `mutation($thread:ID!) { resolveReviewThread(input:{threadId:$thread}) { thread { id isResolved } } }`
	var response struct {
		ResolveReviewThread struct {
			Thread struct {
				IsResolved bool `json:"isResolved"`
			} `json:"thread"`
		} `json:"resolveReviewThread"`
	}
	if err := r.repositoryMonitorGitHubGraphQL(ctx, token, mutation, map[string]any{"thread": threadID}, &response); err != nil {
		return err
	}
	if !response.ResolveReviewThread.Thread.IsResolved {
		return fmt.Errorf("GitHub did not resolve review thread %s", threadID)
	}
	return nil
}

// repositoryMonitorGitHubGraphQL runs one GitHub GraphQL operation and
// decodes its data into out.
func (r *RepositoryMonitorReconciler) repositoryMonitorGitHubGraphQL(ctx context.Context, token, query string, variables map[string]any, out any) error {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, repositoryMonitorGitHubBaseURL(r)+"/graphql", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	repositoryMonitorSetGitHubHeaders(req, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := repositoryMonitorHTTPClient(r).Do(req)
	if err != nil {
		return fmt.Errorf("GitHub GraphQL request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	body, err := readRepositoryMonitorGitHubResponse(resp.Body, repositoryMonitorGitHubResponseLimit)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &repositoryMonitorGitHubAPIError{Operation: "GraphQL request", StatusCode: resp.StatusCode, Body: string(body)}
	}
	var decoded struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return fmt.Errorf("failed to parse GitHub GraphQL response: %w", err)
	}
	if len(decoded.Errors) > 0 {
		return fmt.Errorf("GitHub GraphQL error: %s", decoded.Errors[0].Message)
	}
	return json.Unmarshal(decoded.Data, out)
}

func repositoryMonitorGitHubBaseURL(r *RepositoryMonitorReconciler) string {
	if baseURL := strings.TrimRight(r.GitHubAPIBaseURL, "/"); baseURL != "" {
		return baseURL
	}
	return repositoryMonitorDefaultGitHubAPIBaseURL
}

// markRepositoryMonitorFixedFindingThreadsBestEffort keeps a GitHub failure
// from blocking review ingestion; marking threads only tidies the PR.
func (r *RepositoryMonitorReconciler) markRepositoryMonitorFixedFindingThreadsBestEffort(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, item *store.MonitorItem, record *store.ReviewRecord) {
	if err := r.markRepositoryMonitorFixedFindingThreads(ctx, monitor, item, record); err != nil {
		log.FromContext(ctx).Info("could not mark fixed finding threads", "pr", item.Number, "review", record.ID, "error", err.Error())
	}
}
