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
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

const repositoryMonitorOperationUpdateReview = "update_review"

const repositoryMonitorUpdatedReviewNote = "_Updated after a later review of this commit. Inline comments are from the earlier review._\n\n"

func (r *RepositoryMonitorReconciler) updatePublishedRepositoryMonitorReview(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, item *store.MonitorItem, task *corev1alpha1.Task, record *store.ReviewRecord, published *store.ReviewPublishRecord, token, owner, repository string) (bool, error) {
	shownID, shownAt, err := r.repositoryMonitorShownReviewRecordID(ctx, monitor, item, published)
	if err != nil {
		return false, err
	}
	ambiguous, err := r.repositoryMonitorReviewUpdateUnrecorded(ctx, monitor, item, published.HeadSHA, shownAt)
	if err != nil {
		return false, err
	}
	if !ambiguous {
		if shownID == record.ID {
			return false, nil
		}
		shown, err := r.Store.GetReviewRecord(ctx, monitor.Namespace, shownID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
		if shown != nil && strings.TrimSpace(shown.Verdict) == strings.TrimSpace(record.Verdict) {
			return false, nil
		}
	}
	reviewID, err := strconv.ParseInt(strings.TrimSpace(published.GitHubReviewID), 10, 64)
	if err != nil || reviewID <= 0 {
		return false, nil
	}
	findings, err := repositoryMonitorReviewFindingsFromRecord(record)
	if err != nil {
		return false, err
	}
	body := repositoryMonitorUpdatedReviewNote + renderRepositoryMonitorReviewBody(monitor, item, task, record, published.ID, findings)
	if len([]byte(body)) > repositoryMonitorReviewBodyMaxBytes {
		return false, nil
	}
	mutationID := "ghmut-" + repositoryMonitorShortHash(record.ID+"-update-review")
	mutation, err := r.Store.GetGitHubMutationRecord(ctx, monitor.Namespace, mutationID)
	if errors.Is(err, store.ErrNotFound) {
		mutation = &store.GitHubMutationRecord{ID: mutationID, RunID: repositoryMonitorReviewRunID(task), Operation: repositoryMonitorOperationUpdateReview, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: item.Number, TargetSHA: published.HeadSHA, Reason: repositoryMonitorPublishReviewReason, ExternalID: published.GitHubReviewID, Status: repositoryMonitorAutomergeStateStarted}
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

func (r *RepositoryMonitorReconciler) repositoryMonitorShownReviewRecordID(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, item *store.MonitorItem, published *store.ReviewPublishRecord) (string, time.Time, error) {
	shown, at := published.ReviewRecordID, published.CreatedAt
	cursor := ""
	for {
		records, next, err := r.Store.ListReviewPublishRecords(ctx, store.ReviewPublishRecordFilter{
			Namespace:   monitor.Namespace,
			MonitorName: monitor.Name,
			ItemKind:    repositoryMonitorPullRequestKind,
			ItemNumber:  item.Number,
			HeadSHA:     published.HeadSHA,
			Phase:       repositoryMonitorPublishPhaseSkipped,
			Limit:       100,
			Cursor:      cursor,
		})
		if err != nil {
			return "", time.Time{}, err
		}
		for _, candidate := range records {
			if candidate.SkipReason == repositoryMonitorPublishSkipUpdatedPublishedReview && candidate.CreatedAt.After(at) {
				shown, at = candidate.ReviewRecordID, candidate.CreatedAt
			}
		}
		if next == "" {
			return shown, at, nil
		}
		cursor = next
	}
}

func (r *RepositoryMonitorReconciler) repositoryMonitorReviewUpdateUnrecorded(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, item *store.MonitorItem, headSHA string, shownAt time.Time) (bool, error) {
	cursor := ""
	for {
		mutations, next, err := r.Store.ListGitHubMutationRecords(ctx, store.GitHubMutationRecordFilter{
			Namespace: monitor.Namespace, MonitorName: monitor.Name, Operation: repositoryMonitorOperationUpdateReview,
			TargetKind: repositoryMonitorPullRequestKind, TargetNumber: item.Number, TargetSHA: headSHA, Limit: 100, Cursor: cursor,
		})
		if err != nil {
			return false, err
		}
		for _, mutation := range mutations {
			if mutation.CreatedAt.After(shownAt) {
				return true, nil
			}
		}
		if next == "" {
			return false, nil
		}
		cursor = next
	}
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
