package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/workers/common"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	repositoryMonitorReviewSchemaVersion = "orka.prReview.v1"

	repositoryMonitorReviewConfidenceLow    = "low"
	repositoryMonitorReviewConfidenceMedium = "medium"
	repositoryMonitorReviewConfidenceHigh   = "high"

	repositoryMonitorReviewVerdictPassed            = "passed"
	repositoryMonitorReviewVerdictNeedsChanges      = "needs_changes"
	repositoryMonitorReviewVerdictNeedsHuman        = "needs_human"
	repositoryMonitorReviewVerdictSecuritySensitive = "security_sensitive"
	repositoryMonitorReviewVerdictStale             = "stale"
	repositoryMonitorReviewVerdictFailed            = "failed"

	repositoryMonitorReviewSkipReasonFailed          = "review_failed"
	repositoryMonitorReviewSkipReasonMalformed       = "review_result_malformed"
	repositoryMonitorReviewSkipReasonMissingResult   = "review_result_missing"
	repositoryMonitorReviewSkipReasonStaleHead       = "stale_head_sha"
	repositoryMonitorReviewSkipReasonTaskMismatch    = "review_task_mismatch"
	repositoryMonitorReviewSkipReasonTaskFailed      = "review_task_failed"
	repositoryMonitorReviewSkipReasonTaskCancelled   = "review_task_cancelled"
	repositoryMonitorReviewSkipReasonTaskResultError = "review_result_error"
)

type repositoryMonitorReviewResult struct {
	SchemaVersion    string                            `json:"schemaVersion"`
	Repo             string                            `json:"repo"`
	PRNumber         int64                             `json:"prNumber"`
	HeadSHA          string                            `json:"headSHA"`
	Verdict          string                            `json:"verdict"`
	Confidence       string                            `json:"confidence"`
	Repairable       bool                              `json:"repairable"`
	Summary          string                            `json:"summary"`
	Findings         []repositoryMonitorReviewFinding  `json:"findings"`
	Security         repositoryMonitorReviewSecurity   `json:"security"`
	Tests            repositoryMonitorReviewTestStatus `json:"tests"`
	SuggestedComment string                            `json:"suggestedComment"`
}

type repositoryMonitorReviewFinding struct {
	Priority       string `json:"priority"`
	Confidence     string `json:"confidence"`
	File           string `json:"file"`
	Line           int64  `json:"line"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	Recommendation string `json:"recommendation"`
}

type repositoryMonitorReviewSecurity struct {
	Status string `json:"status"`
	Notes  string `json:"notes"`
}

type repositoryMonitorReviewTestStatus struct {
	Status   string `json:"status"`
	Evidence string `json:"evidence"`
}

func (r *RepositoryMonitorReconciler) ingestCompletedRepositoryMonitorReviewTasks(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor) (bool, error) {
	if r.ResultStore == nil {
		return false, nil
	}

	items, err := r.listRepositoryMonitorPullRequestItems(ctx, monitor)
	if err != nil {
		return false, err
	}

	ingested := false
	for i := range items {
		item := items[i]
		if item.LastVerdict != repositoryMonitorRunPhaseQueued || strings.TrimSpace(item.LastReviewID) == "" {
			continue
		}
		var task corev1alpha1.Task
		err := r.Get(ctx, types.NamespacedName{Namespace: monitor.Namespace, Name: item.LastReviewID}, &task)
		if err != nil {
			if client.IgnoreNotFound(err) == nil {
				continue
			}
			return ingested, err
		}
		if !repositoryMonitorReviewTaskTerminal(task.Status.Phase) {
			continue
		}
		handled, err := r.ingestCompletedRepositoryMonitorReviewTask(ctx, monitor, &item, &task)
		if err != nil {
			return ingested, err
		}
		ingested = ingested || handled
	}
	return ingested, nil
}

func repositoryMonitorReviewTaskTerminal(phase corev1alpha1.TaskPhase) bool {
	switch phase {
	case corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled:
		return true
	default:
		return false
	}
}

func (r *RepositoryMonitorReconciler) ingestCompletedRepositoryMonitorReviewTask(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, item *store.MonitorItem, task *corev1alpha1.Task) (bool, error) {
	recordID := repositoryMonitorReviewRecordID(task)
	if err := validateRepositoryMonitorReviewTaskItemBinding(task, monitor, repositoryMonitorPullRequestKind, item.Number); err != nil {
		return r.createRepositoryMonitorRejectedReviewRecord(ctx, monitor, item, task, recordID, repositoryMonitorReviewVerdictFailed, repositoryMonitorReviewSkipReasonTaskMismatch, err.Error())
	}
	if record, err := r.Store.GetReviewRecord(ctx, monitor.Namespace, recordID); err == nil {
		return r.applyRepositoryMonitorReviewRecord(ctx, monitor, item, record, task)
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}

	switch task.Status.Phase {
	case corev1alpha1.TaskPhaseFailed:
		return r.createRepositoryMonitorRejectedReviewRecord(ctx, monitor, item, task, recordID, repositoryMonitorReviewVerdictFailed, repositoryMonitorReviewSkipReasonTaskFailed, task.Status.Message)
	case corev1alpha1.TaskPhaseCancelled:
		return r.createRepositoryMonitorRejectedReviewRecord(ctx, monitor, item, task, recordID, repositoryMonitorReviewVerdictFailed, repositoryMonitorReviewSkipReasonTaskCancelled, task.Status.Message)
	}

	if task.Status.ResultRef == nil || !task.Status.ResultRef.Available {
		return r.createRepositoryMonitorRejectedReviewRecord(ctx, monitor, item, task, recordID, repositoryMonitorReviewVerdictFailed, repositoryMonitorReviewSkipReasonMissingResult, "review task completed without a stored result")
	}

	rawResult, err := r.ResultStore.GetResult(ctx, task.Namespace, task.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return r.createRepositoryMonitorRejectedReviewRecord(ctx, monitor, item, task, recordID, repositoryMonitorReviewVerdictFailed, repositoryMonitorReviewSkipReasonMissingResult, "review task result was not found")
		}
		return false, fmt.Errorf("get repository monitor review task result %s/%s: %w", task.Namespace, task.Name, err)
	}

	review, err := parseRepositoryMonitorReviewResult(rawResult)
	if err != nil {
		return r.createRepositoryMonitorRejectedReviewRecord(ctx, monitor, item, task, recordID, repositoryMonitorReviewVerdictFailed, repositoryMonitorReviewSkipReasonMalformed, err.Error())
	}
	if err := validateRepositoryMonitorReviewResult(review, item, task); err != nil {
		return r.createRepositoryMonitorRejectedReviewRecord(ctx, monitor, item, task, recordID, repositoryMonitorReviewVerdictFailed, repositoryMonitorReviewSkipReasonMalformed, err.Error())
	}
	if currentHead := strings.TrimSpace(item.HeadSHA); currentHead != "" && currentHead != expectedRepositoryMonitorReviewHeadSHA(item, task) {
		summary := fmt.Sprintf("review result applies to stale head %s; current head is %s", expectedRepositoryMonitorReviewHeadSHA(item, task), currentHead)
		return r.createRepositoryMonitorRejectedReviewRecord(ctx, monitor, item, task, recordID, repositoryMonitorReviewVerdictStale, repositoryMonitorReviewSkipReasonStaleHead, summary)
	}

	findingsJSON, err := repositoryMonitorReviewFindingsJSON(review.Findings)
	if err != nil {
		return false, err
	}
	record := &store.ReviewRecord{
		ID:               recordID,
		MonitorNamespace: monitor.Namespace,
		MonitorName:      monitor.Name,
		Kind:             repositoryMonitorPullRequestKind,
		Number:           item.Number,
		HeadSHA:          strings.TrimSpace(review.HeadSHA),
		TaskName:         task.Name,
		TaskNamespace:    task.Namespace,
		Verdict:          strings.TrimSpace(review.Verdict),
		Confidence:       strings.TrimSpace(review.Confidence),
		Repairable:       review.Repairable,
		SecurityStatus:   strings.TrimSpace(review.Security.Status),
		FindingsJSON:     findingsJSON,
		Summary:          strings.TrimSpace(review.Summary),
		SuggestedComment: strings.TrimSpace(review.SuggestedComment),
	}
	if err := r.Store.CreateReviewRecord(ctx, record); err != nil {
		return false, err
	}
	reason := ""
	if record.Verdict == repositoryMonitorVerdictSkipped {
		reason = repositoryMonitorVerdictSkipped
	}
	if err := r.applyRepositoryMonitorReviewRecordToItem(ctx, item, record, reason); err != nil {
		return false, err
	}
	if err := r.createMonitorEvent(ctx, monitor, "", repositoryMonitorPullRequestKind, item.Number, record.HeadSHA, "review_result_ingested", fmt.Sprintf("Pull request #%d review result ingested", item.Number), map[string]any{
		"reviewID":   record.ID,
		"taskName":   task.Name,
		"verdict":    record.Verdict,
		"headSHA":    record.HeadSHA,
		"confidence": record.Confidence,
	}); err != nil {
		return false, err
	}
	if err := r.publishRepositoryMonitorReview(ctx, monitor, item, task, record); err != nil {
		return false, err
	}
	return true, nil
}

func parseRepositoryMonitorReviewResult(raw []byte) (*repositoryMonitorReviewResult, error) {
	summary := strings.TrimSpace(common.ParseStructuredResult(string(raw)).Summary)
	if summary == "" {
		summary = strings.TrimSpace(string(raw))
	}
	payload, err := repositoryMonitorReviewJSONPayload(summary)
	if err != nil {
		return nil, err
	}
	var result repositoryMonitorReviewResult
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		return nil, fmt.Errorf("review result is not valid JSON: %w", err)
	}
	return &result, nil
}

func repositoryMonitorReviewJSONPayload(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("review result is empty")
	}
	if json.Valid([]byte(raw)) {
		return raw, nil
	}
	payload, ok := firstJSONObject(raw)
	if !ok {
		return "", fmt.Errorf("review result does not contain a JSON object")
	}
	if !json.Valid([]byte(payload)) {
		return "", fmt.Errorf("review result JSON object is invalid")
	}
	return payload, nil
}

func firstJSONObject(raw string) (string, bool) {
	start := strings.IndexByte(raw, '{')
	if start < 0 {
		return "", false
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(raw); i++ {
		ch := raw[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return raw[start : i+1], true
			}
		}
	}
	return "", false
}

func validateRepositoryMonitorReviewResult(result *repositoryMonitorReviewResult, item *store.MonitorItem, task *corev1alpha1.Task) error {
	if strings.TrimSpace(result.SchemaVersion) != repositoryMonitorReviewSchemaVersion {
		return fmt.Errorf("schemaVersion must be %q", repositoryMonitorReviewSchemaVersion)
	}
	if expectedRepo := strings.TrimSpace(task.Annotations[labels.AnnotationGitHubRepository]); expectedRepo != "" && !strings.EqualFold(strings.TrimSpace(result.Repo), expectedRepo) {
		return fmt.Errorf("repo %q does not match expected repository %q", result.Repo, expectedRepo)
	}
	if result.PRNumber != item.Number {
		return fmt.Errorf("prNumber %d does not match expected PR number %d", result.PRNumber, item.Number)
	}
	if expectedHead := expectedRepositoryMonitorReviewHeadSHA(item, task); strings.TrimSpace(result.HeadSHA) != expectedHead {
		return fmt.Errorf("headSHA %q does not match expected head %q", result.HeadSHA, expectedHead)
	}
	if !repositoryMonitorReviewVerdictAllowed(result.Verdict) {
		return fmt.Errorf("verdict %q is not allowed", result.Verdict)
	}
	if !repositoryMonitorReviewConfidenceAllowed(result.Confidence) {
		return fmt.Errorf("confidence %q is not allowed", result.Confidence)
	}
	if !repositoryMonitorReviewSecurityStatusAllowed(result.Security.Status) {
		return fmt.Errorf("security.status %q is not allowed", result.Security.Status)
	}
	return nil
}

func expectedRepositoryMonitorReviewHeadSHA(item *store.MonitorItem, task *corev1alpha1.Task) string {
	if task != nil {
		if headSHA := strings.TrimSpace(task.Annotations[labels.AnnotationMonitorHeadSHA]); headSHA != "" {
			return headSHA
		}
	}
	return strings.TrimSpace(item.HeadSHA)
}

func repositoryMonitorReviewVerdictAllowed(verdict string) bool {
	switch strings.TrimSpace(verdict) {
	case repositoryMonitorReviewVerdictPassed,
		repositoryMonitorReviewVerdictNeedsChanges,
		repositoryMonitorReviewVerdictNeedsHuman,
		repositoryMonitorReviewVerdictSecuritySensitive,
		repositoryMonitorVerdictSkipped:
		return true
	default:
		return false
	}
}

func repositoryMonitorReviewConfidenceAllowed(confidence string) bool {
	switch strings.TrimSpace(confidence) {
	case repositoryMonitorReviewConfidenceLow, repositoryMonitorReviewConfidenceMedium, repositoryMonitorReviewConfidenceHigh:
		return true
	default:
		return false
	}
}

func repositoryMonitorReviewSecurityStatusAllowed(status string) bool {
	switch strings.TrimSpace(status) {
	case "clear", "needs_human", "security_sensitive":
		return true
	default:
		return false
	}
}

func repositoryMonitorReviewFindingsJSON(findings []repositoryMonitorReviewFinding) (string, error) {
	if findings == nil {
		findings = []repositoryMonitorReviewFinding{}
	}
	data, err := json.Marshal(findings)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (r *RepositoryMonitorReconciler) createRepositoryMonitorRejectedReviewRecord(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, item *store.MonitorItem, task *corev1alpha1.Task, recordID, verdict, reason, summary string) (bool, error) {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		summary = reason
	}
	record := &store.ReviewRecord{
		ID:               recordID,
		MonitorNamespace: monitor.Namespace,
		MonitorName:      monitor.Name,
		Kind:             repositoryMonitorPullRequestKind,
		Number:           item.Number,
		HeadSHA:          expectedRepositoryMonitorReviewHeadSHA(item, task),
		TaskName:         task.Name,
		TaskNamespace:    task.Namespace,
		Verdict:          verdict,
		Confidence:       repositoryMonitorReviewConfidenceLow,
		SecurityStatus:   "unknown",
		FindingsJSON:     "[]",
		Summary:          summary,
	}
	if err := r.Store.CreateReviewRecord(ctx, record); err != nil {
		return false, err
	}
	if err := r.applyRepositoryMonitorReviewRecordToItem(ctx, item, record, reason); err != nil {
		return false, err
	}
	if err := r.createMonitorEvent(ctx, monitor, "", repositoryMonitorPullRequestKind, item.Number, record.HeadSHA, "review_result_rejected", fmt.Sprintf("Pull request #%d review result rejected: %s", item.Number, reason), map[string]any{
		"reviewID": record.ID,
		"taskName": task.Name,
		"verdict":  record.Verdict,
		"reason":   reason,
	}); err != nil {
		return false, err
	}
	if err := r.publishRepositoryMonitorReview(ctx, monitor, item, task, record); err != nil {
		return false, err
	}
	return true, nil
}

func (r *RepositoryMonitorReconciler) applyRepositoryMonitorReviewRecord(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, item *store.MonitorItem, record *store.ReviewRecord, task *corev1alpha1.Task) (bool, error) {
	if item.LastReviewID != task.Name || item.LastVerdict != repositoryMonitorRunPhaseQueued {
		return false, nil
	}
	reason := ""
	switch record.Verdict {
	case repositoryMonitorReviewVerdictFailed:
		reason = repositoryMonitorReviewSkipReasonFailed
	case repositoryMonitorReviewVerdictStale:
		reason = repositoryMonitorReviewSkipReasonStaleHead
	case repositoryMonitorVerdictSkipped:
		reason = repositoryMonitorVerdictSkipped
	}
	if err := r.applyRepositoryMonitorReviewRecordToItem(ctx, item, record, reason); err != nil {
		return false, err
	}
	if err := r.createMonitorEvent(ctx, monitor, "", repositoryMonitorPullRequestKind, item.Number, record.HeadSHA, "review_result_ingested", fmt.Sprintf("Pull request #%d review result ingested", item.Number), map[string]any{
		"reviewID": record.ID,
		"taskName": task.Name,
		"verdict":  record.Verdict,
		"headSHA":  record.HeadSHA,
	}); err != nil {
		return false, err
	}
	if err := r.publishRepositoryMonitorReview(ctx, monitor, item, task, record); err != nil {
		return false, err
	}
	return true, nil
}

func (r *RepositoryMonitorReconciler) applyRepositoryMonitorReviewRecordToItem(ctx context.Context, item *store.MonitorItem, record *store.ReviewRecord, reason string) error {
	item.LastReviewID = record.ID
	item.LastVerdict = record.Verdict
	item.SkipReason = reason
	if record.HeadSHA == item.HeadSHA {
		if reason == "" && repositoryMonitorReviewVerdictMarksHeadFresh(record.Verdict) {
			item.LastReviewedHeadSHA = record.HeadSHA
		} else {
			item.LastReviewedHeadSHA = ""
		}
	}
	if reason == "" && record.Verdict == repositoryMonitorReviewVerdictPassed {
		item.AutomergeState = "merge_ready"
	} else if record.Verdict != repositoryMonitorReviewVerdictPassed {
		item.AutomergeState = ""
	}
	return r.Store.UpsertMonitorItem(ctx, item)
}

func repositoryMonitorReviewVerdictMarksHeadFresh(verdict string) bool {
	switch strings.TrimSpace(verdict) {
	case repositoryMonitorReviewVerdictPassed,
		repositoryMonitorReviewVerdictNeedsChanges,
		repositoryMonitorReviewVerdictNeedsHuman,
		repositoryMonitorReviewVerdictSecuritySensitive:
		return true
	default:
		return false
	}
}

func repositoryMonitorReviewRecordID(task *corev1alpha1.Task) string {
	return repositoryMonitorBoundedDNSName("monreview-"+task.Namespace+"-"+task.Name, 120)
}
