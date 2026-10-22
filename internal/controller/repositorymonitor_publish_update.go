package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// repositoryMonitorUpdatedReviewNote opens a rewritten review summary.
const repositoryMonitorUpdatedReviewNote = "_Updated after a later review of this commit. Inline comments are from the earlier review._\n\n"

// updatePublishedRepositoryMonitorReview rewrites the summary of the review
// Orka already published for record's head when record reaches a different
// verdict, so GitHub shows the current verdict while keeping one Orka review
// per head. It reports false when the published review needs no change.
func (r *RepositoryMonitorReconciler) updatePublishedRepositoryMonitorReview(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, item *store.MonitorItem, task *corev1alpha1.Task, record *store.ReviewRecord, published *store.ReviewPublishRecord, token, owner, repository string) (bool, error) {
	shownID, err := r.repositoryMonitorShownReviewRecordID(ctx, monitor, item, published)
	if err != nil || shownID == record.ID {
		return false, err
	}
	shown, err := r.Store.GetReviewRecord(ctx, monitor.Namespace, shownID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	if shown != nil && strings.TrimSpace(shown.Verdict) == strings.TrimSpace(record.Verdict) {
		return false, nil
	}
	reviewID, err := strconv.ParseInt(strings.TrimSpace(published.GitHubReviewID), 10, 64)
	if err != nil || reviewID <= 0 {
		return false, nil
	}
	body, _, err := r.renderRepositoryMonitorGitHubReview(ctx, monitor, item, task, record, published.ID, token, owner, repository)
	if err != nil {
		return false, err
	}
	body = repositoryMonitorUpdatedReviewNote + body
	if len([]byte(body)) > repositoryMonitorReviewBodyMaxBytes {
		return false, nil
	}
	mutationID := "ghmut-" + repositoryMonitorShortHash(record.ID+"-update-review")
	mutation, err := r.Store.GetGitHubMutationRecord(ctx, monitor.Namespace, mutationID)
	if errors.Is(err, store.ErrNotFound) {
		mutation = &store.GitHubMutationRecord{ID: mutationID, RunID: repositoryMonitorReviewRunID(task), Operation: "update_review", TargetKind: repositoryMonitorPullRequestKind, TargetNumber: item.Number, TargetSHA: published.HeadSHA, Reason: repositoryMonitorPublishReviewReason, ExternalID: published.GitHubReviewID, Status: repositoryMonitorAutomergeStateStarted}
		if err := r.recordRepositoryMonitorGitHubMutation(ctx, monitor, mutation); err != nil {
			return false, err
		}
	} else if err != nil {
		return false, err
	}
	if mutation.Status != repositoryMonitorRunPhaseSucceeded {
		response, err := r.updateRepositoryMonitorPullRequestReview(ctx, owner, repository, token, item.Number, reviewID, body)
		if err != nil {
			mutation.Status = repositoryMonitorRunPhaseFailed
			mutation.Error = err.Error()
			if auditErr := r.updateRepositoryMonitorGitHubMutation(ctx, monitor, mutation); auditErr != nil {
				return false, fmt.Errorf("update review failed: %w; additionally failed to update mutation audit: %v", err, auditErr)
			}
			return false, err
		}
		mutation.Status = repositoryMonitorRunPhaseSucceeded
		mutation.Error = ""
		mutation.GitHubURL = response.HTMLURL
		if err := r.updateRepositoryMonitorGitHubMutation(ctx, monitor, mutation); err != nil {
			return false, err
		}
	}
	// The original publish record keeps its review binding; this review gets
	// its own terminal record pointing at the review it rewrote.
	update := repositoryMonitorBasePublishRecord(monitor, item, task, record)
	update.Phase = repositoryMonitorPublishPhaseSkipped
	update.SkipReason = repositoryMonitorPublishSkipUpdatedPublishedReview
	update.GitHubReviewID = published.GitHubReviewID
	update.GitHubReviewURL = published.GitHubReviewURL
	update.BodyDigest = repositoryMonitorBodyDigest(body)
	update.InlineCommentCount = published.InlineCommentCount
	return true, r.finishRepositoryMonitorReviewPublish(ctx, monitor, item, update, "review_publish_updated", fmt.Sprintf("Pull request #%d review updated on GitHub with verdict %s", item.Number, record.Verdict), map[string]any{
		eventReviewIDField: record.ID,
		"previousReviewID": shownID,
		"githubReviewID":   published.GitHubReviewID,
		"githubReviewURL":  published.GitHubReviewURL,
		eventVerdictField:  record.Verdict,
	})
}

// repositoryMonitorShownReviewRecordID returns the review whose summary the
// published review for published's head currently shows: the newest update,
// or the review first published.
func (r *RepositoryMonitorReconciler) repositoryMonitorShownReviewRecordID(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, item *store.MonitorItem, published *store.ReviewPublishRecord) (string, error) {
	records, _, err := r.Store.ListReviewPublishRecords(ctx, store.ReviewPublishRecordFilter{
		Namespace:   monitor.Namespace,
		MonitorName: monitor.Name,
		ItemKind:    repositoryMonitorPullRequestKind,
		ItemNumber:  item.Number,
		HeadSHA:     published.HeadSHA,
		Phase:       repositoryMonitorPublishPhaseSkipped,
		Limit:       100,
	})
	if err != nil {
		return "", err
	}
	shown, at := published.ReviewRecordID, published.CreatedAt
	for _, candidate := range records {
		if candidate.SkipReason == repositoryMonitorPublishSkipUpdatedPublishedReview && candidate.CreatedAt.After(at) {
			shown, at = candidate.ReviewRecordID, candidate.CreatedAt
		}
	}
	return shown, nil
}

func (r *RepositoryMonitorReconciler) updateRepositoryMonitorPullRequestReview(ctx context.Context, owner, repository, token string, number, reviewID int64, body string) (*repositoryMonitorPullRequestReviewResponse, error) {
	baseURL := strings.TrimRight(r.GitHubAPIBaseURL, "/")
	if baseURL == "" {
		baseURL = repositoryMonitorDefaultGitHubAPIBaseURL
	}
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/reviews/%d", baseURL, url.PathEscape(owner), url.PathEscape(repository), number, reviewID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	repositoryMonitorSetGitHubHeaders(req, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := repositoryMonitorHTTPClient(r).Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub pull request review update request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	respBody, err := readRepositoryMonitorGitHubResponse(resp.Body, repositoryMonitorGitHubResponseLimit)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &repositoryMonitorGitHubAPIError{Operation: "pull request review update request", StatusCode: resp.StatusCode, Body: string(respBody)}
	}
	var response repositoryMonitorPullRequestReviewResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to parse GitHub pull request review update response: %w", err)
	}
	return &response, nil
}
