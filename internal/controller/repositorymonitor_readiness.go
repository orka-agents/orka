package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

const (
	repositoryMonitorStatusPending       = "pending"
	repositoryMonitorStatusSuccess       = "success"
	repositoryMonitorStatusFailure       = "failure"
	repositoryMonitorStatusSubmitting    = "submitting"
	repositoryMonitorMergeableStateDirty = "dirty"
	repositoryMonitorReadinessOperation  = "readiness_status"
	repositoryMonitorReadinessTargetKind = "commit"
	repositoryMonitorReadinessSuccess    = "Orka workflow evidence is current. GitHub controls required approvals and merging."
)

func repositoryMonitorReadinessContext(monitor *corev1alpha1.RepositoryMonitor) string {
	return "orka/" + monitor.Namespace + "/" + labels.SelectorValue(monitor.Name) + "/ready"
}

type repositoryMonitorCIResult struct {
	passed     bool
	reason     string
	failed     []repositoryMonitorCIFailure
	pending    []repositoryMonitorCIFailure
	incomplete bool
}

const repositoryMonitorCIFailureSourceStatus = "commit_status"

type repositoryMonitorCIFailure struct {
	name            string
	source          string
	id              int64
	appSlug         string
	headSHA         string
	status          string
	conclusion      string
	url             string
	title           string
	summary         string
	annotationCount int
	description     string
}

func (f repositoryMonitorCIFailure) key() string {
	return f.source + "|" + strings.ToLower(strings.TrimSpace(f.name))
}

type repositoryMonitorCommitStatus struct {
	ID          int64  `json:"id,omitempty"`
	Context     string `json:"context"`
	State       string `json:"state"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url,omitempty"`
	TargetURL   string `json:"target_url,omitempty"`
}

func repositoryMonitorPauseLabels(spec corev1alpha1.RepositoryMonitorSpec) []string {
	if spec.Policy.PauseLabels == nil {
		return []string{"orka:pause"}
	}
	return spec.Policy.PauseLabels
}

func (r *RepositoryMonitorReconciler) repositoryMonitorOwnedReadinessStatuses(ctx context.Context, sha string) (map[int64]struct{}, error) {
	ids := map[int64]struct{}{}
	if r.Store == nil {
		return ids, nil
	}
	cursor := ""
	for {
		records, next, err := r.Store.ListGitHubMutationRecords(ctx, store.GitHubMutationRecordFilter{AllNamespaces: true, Operation: repositoryMonitorReadinessOperation, TargetSHA: sha, Limit: 100, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, record := range records {
			if record.TargetSHA == sha {
				if id, err := strconv.ParseInt(record.ExternalID, 10, 64); err == nil && id > 0 {
					ids[id] = struct{}{}
				}
			}
		}
		if next == "" {
			return ids, nil
		}
		cursor = next
	}
}

const repositoryMonitorReadinessCIAwaitingApproval = "Waiting for a maintainer to approve the CI workflow runs for this commit."

func (r *RepositoryMonitorReconciler) repositoryMonitorReadyOutcome(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, pr repositoryMonitorPullRequest, item *store.MonitorItem) (string, string, error) {
	if repositoryMonitorBlockedLabel(monitor.Spec, pr.Labels) != "" || item.SkipReason == repositoryMonitorIssueSkipStoppedByCommand {
		return repositoryMonitorStatusFailure, "Automation is paused or blocked by repository policy.", nil
	}
	if pr.Draft {
		return repositoryMonitorStatusFailure, "The pull request is a draft.", nil
	}
	for _, phase := range store.RepositoryMonitorWorkActionActiveStatuses() {
		actions, _, err := r.Store.ListWorkActions(ctx, store.WorkActionFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: pr.Number, TargetSHA: pr.HeadSHA, Status: phase, Limit: 1})
		if err != nil {
			return "", "", err
		}
		if len(actions) > 0 {
			return repositoryMonitorStatusPending, "Waiting for current-head workflow actions to finish.", nil
		}
	}
	if item.RepairState == repositoryMonitorRepairPhaseFailed {
		return repositoryMonitorStatusFailure, "Repair did not complete successfully.", nil
	}
	if repositoryMonitorAutomergeRepairStateBlocks(item.RepairState) {
		return repositoryMonitorStatusPending, "Repair is in progress.", nil
	}
	if item.LastVerdict == repositoryMonitorReviewVerdictNeedsChanges && item.LastReviewedHeadSHA == pr.HeadSHA {
		eligible, err := r.repositoryMonitorNeedsChangesRepairEligible(ctx, monitor, pr, item)
		if err != nil {
			return "", "", err
		}
		if !eligible {
			return repositoryMonitorStatusFailure, "Review requires changes that automation cannot repair.", nil
		}
	}
	if pr.MergeableState == repositoryMonitorMergeableStateDirty {
		return repositoryMonitorStatusPending, "Waiting for pull request merge conflicts to be resolved.", nil
	}
	if item.LastVerdict != repositoryMonitorReviewVerdictPassed || item.LastReviewedHeadSHA != pr.HeadSHA {
		if item.LastReviewedHeadSHA == pr.HeadSHA && (item.LastVerdict == repositoryMonitorReviewVerdictFailed || item.LastVerdict == repositoryMonitorReviewVerdictNeedsHuman || item.LastVerdict == repositoryMonitorReviewVerdictSecuritySensitive) {
			return repositoryMonitorStatusFailure, "Review did not establish readiness.", nil
		}
		description, err := r.repositoryMonitorAwaitingReviewDescription(ctx, monitor, pr, item)
		if err != nil {
			return "", "", err
		}
		return repositoryMonitorStatusPending, description, nil
	}
	valid, err := r.repositoryMonitorValidationAllowsAutomerge(ctx, monitor, item, pr.HeadSHA)
	if err != nil {
		return "", "", err
	}
	if !valid {
		return repositoryMonitorStatusFailure, "Required validation evidence is unavailable or stale.", nil
	}
	fresh, err := r.repositoryMonitorReviewedHeadFresh(ctx, monitor, item, pr.HeadSHA)
	if err != nil {
		return "", "", err
	}
	if !fresh {
		return repositoryMonitorStatusPending, "Waiting for fresh review evidence.", nil
	}
	ci, err := r.repositoryMonitorCheckCI(ctx, monitor, pr.HeadSHA)
	if err != nil {
		return "", "", err
	}
	if ci.reason == repositoryMonitorCIAwaitingApproval {
		return repositoryMonitorStatusPending, repositoryMonitorReadinessCIAwaitingApproval, nil
	}
	if !ci.passed {
		return repositoryMonitorStatusPending, "Waiting for repository CI to pass.", nil
	}
	return repositoryMonitorStatusSuccess, repositoryMonitorReadinessSuccess, nil
}

func (r *RepositoryMonitorReconciler) repositoryMonitorAwaitingReviewDescription(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, pr repositoryMonitorPullRequest, item *store.MonitorItem) (string, error) {
	if monitor.Spec.Review.RequireGreenCI && item.LastReviewedHeadSHA != pr.HeadSHA {
		ci, err := r.repositoryMonitorCheckCI(ctx, monitor, pr.HeadSHA)
		if err != nil {
			return "", err
		}
		if ci.reason == repositoryMonitorCIAwaitingApproval {
			return repositoryMonitorReadinessCIAwaitingApproval, nil
		}
	}
	return "Waiting for a clean review of this commit.", nil
}

func (r *RepositoryMonitorReconciler) repositoryMonitorNeedsChangesRepairEligible(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, pr repositoryMonitorPullRequest, item *store.MonitorItem) (bool, error) {
	if full, _, _ := repositoryMonitorPullRequestRepairScope(monitor.Spec, pr); !full {
		return false, nil
	}
	owner, repo, err := security.ParseGitHubRepositoryURL(monitor.Spec.RepoURL)
	if err != nil {
		return false, err
	}
	intent := repositoryMonitorCommandIntentFix
	if pr.MergeableState == repositoryMonitorMergeableStateDirty {
		intent = repositoryMonitorCommandIntentUpdateBranch
	}
	reason, _, _, err := r.repositoryMonitorRepairPolicy(ctx, monitor, owner+"/"+repo, pr, "", intent)
	if err != nil || reason != "" {
		return false, err
	}
	if intent == repositoryMonitorCommandIntentUpdateBranch {
		return true, nil
	}
	broke, err := r.repositoryMonitorPullRequestBrokeCI(ctx, monitor, pr)
	if err != nil {
		return false, err
	}
	if broke {
		return true, nil
	}
	if item.LastReviewID == "" {
		return false, nil
	}
	review, err := r.Store.GetReviewRecord(ctx, monitor.Namespace, item.LastReviewID)
	if err != nil {
		return false, err
	}
	return review.HeadSHA == pr.HeadSHA && review.Repairable, nil
}

func repositoryMonitorPullRequestInScope(monitor *corev1alpha1.RepositoryMonitor, pr repositoryMonitorPullRequest) bool {
	return pr.State == repositoryMonitorItemStateOpen && pr.BaseBranch == effectiveRepositoryMonitorBranch(monitor) && repositoryMonitorPullRequestReviewed(monitor.Spec, pr)
}

func (r *RepositoryMonitorReconciler) repositoryMonitorSharedHeadReadiness(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, pr repositoryMonitorPullRequest, currentItem *store.MonitorItem, peers []repositoryMonitorPullRequest, endpoint, token string) (string, string, error) {
	result, resultDescription, err := r.repositoryMonitorReadyOutcome(ctx, monitor, pr, currentItem)
	if err != nil {
		return "", "", err
	}
	for _, peer := range peers {
		if peer.Number == pr.Number || peer.HeadSHA != pr.HeadSHA || !repositoryMonitorPullRequestInScope(monitor, peer) {
			continue
		}
		peer, err = r.refreshRepositoryMonitorReadinessPullRequest(ctx, peer, endpoint, token)
		if err != nil {
			return "", "", err
		}
		if peer.HeadSHA != pr.HeadSHA || !repositoryMonitorPullRequestInScope(monitor, peer) {
			continue
		}
		item, err := r.repositoryMonitorSharedHeadItem(ctx, monitor, peer)
		if err != nil {
			return "", "", err
		}
		state, description, err := r.repositoryMonitorReadyOutcome(ctx, monitor, peer, item)
		if err != nil {
			return "", "", err
		}
		if (state == repositoryMonitorStatusFailure && result != repositoryMonitorStatusFailure) ||
			(result == repositoryMonitorStatusSuccess && state != repositoryMonitorStatusSuccess) ||
			(state == result && description < resultDescription) {
			result, resultDescription = state, description
		}
	}
	return result, resultDescription, nil
}

func (r *RepositoryMonitorReconciler) refreshRepositoryMonitorReadinessPullRequest(ctx context.Context, pr repositoryMonitorPullRequest, endpoint, token string) (repositoryMonitorPullRequest, error) {
	var current repositoryMonitorPullRequestResponse
	if err := r.fetchRepositoryMonitorAuthorizedJSON(ctx, fmt.Sprintf("%s/pulls/%d", endpoint, pr.Number), token, &current); err != nil {
		return pr, err
	}
	pr.State, pr.HeadSHA, pr.BaseBranch = current.State, current.Head.SHA, current.Base.Ref
	pr.MergeableState, pr.Draft = current.MergeableState, current.Draft
	pr.MaintainerCanModify = current.MaintainerCanModify
	pr.HeadBranch, pr.HeadRepo, pr.HeadRepoURL = current.Head.Ref, current.Head.Repo.FullName, current.Head.Repo.CloneURL
	pr.Labels = nil
	for _, label := range current.Labels {
		pr.Labels = append(pr.Labels, label.Name)
	}
	return pr, nil
}

func (r *RepositoryMonitorReconciler) repositoryMonitorSharedHeadItem(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, pr repositoryMonitorPullRequest) (*store.MonitorItem, error) {
	existing, err := r.Store.GetMonitorItem(ctx, monitor.Namespace, monitor.Name, repositoryMonitorPullRequestKind, strconv.FormatInt(pr.Number, 10))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	item := repositoryMonitorItemFromPullRequest(monitor, pr, existing)
	if item.SkipReason != repositoryMonitorIssueSkipStoppedByCommand {
		item.RepairState, err = r.repositoryMonitorRepairStateForHead(ctx, monitor, pr.Number, pr.HeadSHA)
	}
	return item, err
}

func (r *RepositoryMonitorReconciler) reconcileRepositoryMonitorReadiness(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, pr *repositoryMonitorPullRequest, item *store.MonitorItem, inventory ...[]repositoryMonitorPullRequest) error {
	if !monitor.Spec.Review.Publish.Enabled || !repositoryMonitorManagedWorkflow(monitor) || r.Store == nil {
		return nil
	}
	endpoint, token, err := r.repositoryMonitorReadinessEndpoint(ctx, monitor)
	if err != nil {
		return err
	}
	expectedHead := pr.HeadSHA
	*pr, err = r.refreshRepositoryMonitorReadinessPullRequest(ctx, *pr, endpoint, token)
	if err != nil {
		return err
	}
	outOfScope := !repositoryMonitorPullRequestInScope(monitor, *pr)
	previous, err := r.Store.GetMonitorItem(ctx, monitor.Namespace, monitor.Name, repositoryMonitorPullRequestKind, strconv.FormatInt(pr.Number, 10))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if previous != nil && previous.HeadSHA != "" && previous.HeadSHA != expectedHead && (previous.HeadSHA != pr.HeadSHA || outOfScope) {
		if err := r.reconcileRepositoryMonitorDepartedHead(ctx, monitor, pr.Number, previous.HeadSHA, endpoint, token); err != nil {
			return err
		}
	}
	if outOfScope {
		return r.reconcileRepositoryMonitorDepartedHead(ctx, monitor, pr.Number, expectedHead, endpoint, token)
	}
	if pr.HeadSHA != expectedHead {
		if err := r.reconcileRepositoryMonitorDepartedHead(ctx, monitor, pr.Number, expectedHead, endpoint, token); err != nil {
			return err
		}
		if item.SkipReason != repositoryMonitorIssueSkipStoppedByCommand {
			return nil
		}
	}
	item.Draft = pr.Draft
	labelsJSON, err := json.Marshal(pr.Labels)
	if err != nil {
		return err
	}
	item.LabelsJSON = string(labelsJSON)
	mutation, err := r.ensureRepositoryMonitorReadinessStatus(ctx, monitor, *pr, endpoint, token)
	if err != nil {
		return err
	}
	var peers []repositoryMonitorPullRequest
	if len(inventory) > 0 {
		peers = inventory[0]
	} else {
		owner, repo, err := security.ParseGitHubRepositoryURL(monitor.Spec.RepoURL)
		if err != nil {
			return err
		}
		peers, err = r.listRepositoryMonitorPullRequests(ctx, owner, repo, token, effectiveRepositoryMonitorBranch(monitor))
		if err != nil {
			return err
		}
	}
	state, description, err := r.repositoryMonitorSharedHeadReadiness(ctx, monitor, *pr, item, peers, endpoint, token)
	if err != nil {
		return err
	}
	if err := r.publishRepositoryMonitorReadinessStatus(ctx, monitor, mutation, endpoint, token, state, description); err != nil {
		return err
	}
	want := repositoryMonitorAutomergeStatePending
	switch state {
	case repositoryMonitorStatusSuccess:
		want = repositoryMonitorAutomergeStateMergeReady
	case repositoryMonitorStatusFailure:
		want = repositoryMonitorAutomergeStateBlocked
	}
	if item.AutomergeState == want {
		return nil
	}
	item.AutomergeState = want
	return r.Store.UpsertMonitorItem(ctx, item)
}

func (r *RepositoryMonitorReconciler) reconcileRepositoryMonitorDepartedHead(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, departingNumber int64, sha, endpoint, token string) error {
	if r.Store != nil {
		if _, err := r.Store.GetGitHubMutationRecord(ctx, monitor.Namespace, repositoryMonitorReadinessMutationID(monitor, sha)); errors.Is(err, store.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
	}
	peers, err := r.listRepositoryMonitorReadinessPeers(ctx, monitor, token)
	if err != nil {
		return err
	}
	return r.reconcileRepositoryMonitorDepartedHeadWithPeers(ctx, monitor, departingNumber, sha, endpoint, token, peers)
}

func (r *RepositoryMonitorReconciler) listRepositoryMonitorReadinessPeers(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, token string) ([]repositoryMonitorPullRequest, error) {
	owner, repo, err := security.ParseGitHubRepositoryURL(monitor.Spec.RepoURL)
	if err != nil {
		return nil, err
	}
	return r.listRepositoryMonitorPullRequests(ctx, owner, repo, token, effectiveRepositoryMonitorBranch(monitor))
}

func (r *RepositoryMonitorReconciler) reconcileRepositoryMonitorDepartedHeadWithPeers(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, departingNumber int64, sha, endpoint, token string, peers []repositoryMonitorPullRequest) error {
	peers = slices.DeleteFunc(slices.Clone(peers), func(peer repositoryMonitorPullRequest) bool { return peer.Number == departingNumber })
	var err error
	for _, peer := range peers {
		if peer.HeadSHA != sha || !repositoryMonitorPullRequestInScope(monitor, peer) {
			continue
		}
		peer, err = r.refreshRepositoryMonitorReadinessPullRequest(ctx, peer, endpoint, token)
		if err != nil {
			return err
		}
		if peer.HeadSHA != sha || !repositoryMonitorPullRequestInScope(monitor, peer) {
			continue
		}
		item, err := r.repositoryMonitorSharedHeadItem(ctx, monitor, peer)
		if err != nil {
			return err
		}
		mutation, err := r.ensureRepositoryMonitorReadinessStatus(ctx, monitor, peer, endpoint, token)
		if err != nil {
			return err
		}
		state, description, err := r.repositoryMonitorSharedHeadReadiness(ctx, monitor, peer, item, peers, endpoint, token)
		if err != nil {
			return err
		}
		return r.publishRepositoryMonitorReadinessStatus(ctx, monitor, mutation, endpoint, token, state, description)
	}
	return r.revokeRepositoryMonitorReadiness(ctx, monitor, sha)
}

func (r *RepositoryMonitorReconciler) repositoryMonitorReadinessEndpoint(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor) (string, string, error) {
	owner, repo, err := security.ParseGitHubRepositoryURL(monitor.Spec.RepoURL)
	if err != nil {
		return "", "", err
	}
	token, err := r.repositoryMonitorForgeToken(ctx, monitor)
	if err != nil {
		return "", "", err
	}
	base := strings.TrimRight(r.GitHubAPIBaseURL, "/")
	if base == "" {
		base = repositoryMonitorDefaultGitHubAPIBaseURL
	}
	return fmt.Sprintf("%s/repos/%s/%s", base, url.PathEscape(owner), url.PathEscape(repo)), token, nil
}

func repositoryMonitorReadinessMutationID(monitor *corev1alpha1.RepositoryMonitor, sha string) string {
	identity := fmt.Sprintf("orka-ready:%s:%s:%s", monitor.UID, strings.ToLower(monitor.Spec.RepoURL), sha)
	return "ghmut-" + repositoryMonitorShortHash(identity)
}

func (r *RepositoryMonitorReconciler) revokeRepositoryMonitorReadiness(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, sha string) error {
	if !monitor.Spec.Review.Publish.Enabled || !repositoryMonitorManagedWorkflow(monitor) || r.Store == nil || sha == "" {
		return nil
	}
	mutation, err := r.Store.GetGitHubMutationRecord(ctx, monitor.Namespace, repositoryMonitorReadinessMutationID(monitor, sha))
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if mutation.Operation != repositoryMonitorReadinessOperation || mutation.TargetKind != repositoryMonitorReadinessTargetKind || mutation.TargetNumber != 0 || mutation.TargetSHA != sha {
		return fmt.Errorf("readiness audit identity does not match the commit")
	}
	endpoint, token, err := r.repositoryMonitorReadinessEndpoint(ctx, monitor)
	if err != nil {
		return err
	}
	return r.publishRepositoryMonitorReadinessStatus(ctx, monitor, mutation, endpoint, token, repositoryMonitorStatusFailure, "A pull request left this monitor's open base-branch inventory.")
}

func (r *RepositoryMonitorReconciler) ensureRepositoryMonitorReadinessStatus(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, pr repositoryMonitorPullRequest, endpoint, token string) (*store.GitHubMutationRecord, error) {
	mutationID := repositoryMonitorReadinessMutationID(monitor, pr.HeadSHA)
	mutation, err := r.Store.GetGitHubMutationRecord(ctx, monitor.Namespace, mutationID)
	if errors.Is(err, store.ErrNotFound) {
		mutation = &store.GitHubMutationRecord{ID: mutationID, Operation: repositoryMonitorReadinessOperation, TargetKind: repositoryMonitorReadinessTargetKind, TargetSHA: pr.HeadSHA, Status: "started"}
		if err := r.recordRepositoryMonitorGitHubMutation(ctx, monitor, mutation); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if mutation.Operation != repositoryMonitorReadinessOperation || mutation.TargetKind != repositoryMonitorReadinessTargetKind || mutation.TargetNumber != 0 || mutation.TargetSHA != pr.HeadSHA {
		return nil, fmt.Errorf("readiness audit identity does not match the commit")
	}
	if mutation.ExternalID == "" || mutation.Status == repositoryMonitorStatusSubmitting {
		if err := r.publishRepositoryMonitorReadinessStatus(ctx, monitor, mutation, endpoint, token, repositoryMonitorStatusPending, "Waiting for verified workflow completion."); err != nil {
			return nil, err
		}
	}
	return mutation, nil
}

func (r *RepositoryMonitorReconciler) publishRepositoryMonitorReadinessStatus(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, mutation *store.GitHubMutationRecord, endpoint, token, state, description string) error {
	digest := repositoryMonitorShortHash(state + "|" + description)
	if mutation.Status == repositoryMonitorRunPhaseSucceeded && mutation.RequestDigest == digest {
		return nil
	}
	mutation.Status = repositoryMonitorStatusSubmitting
	if err := r.updateRepositoryMonitorGitHubMutation(ctx, monitor, mutation); err != nil {
		return err
	}
	payload := repositoryMonitorCommitStatus{Context: repositoryMonitorReadinessContext(monitor), State: state, Description: description}
	result, err := r.writeRepositoryMonitorCommitStatus(ctx, endpoint+"/statuses/"+url.PathEscape(mutation.TargetSHA), token, payload)
	if err != nil {
		return err
	}
	if result.ID <= 0 || result.Context != payload.Context || result.State != state {
		return fmt.Errorf("GitHub returned an invalid readiness status identity")
	}
	mutation.ExternalID = strconv.FormatInt(result.ID, 10)
	mutation.GitHubURL = result.URL
	mutation.RequestDigest = digest
	mutation.Reason = description
	mutation.Status = repositoryMonitorRunPhaseSucceeded
	return r.updateRepositoryMonitorGitHubMutation(ctx, monitor, mutation)
}

func (r *RepositoryMonitorReconciler) writeRepositoryMonitorCommitStatus(ctx context.Context, endpoint, token string, payload repositoryMonitorCommitStatus) (repositoryMonitorCommitStatus, error) {
	var result repositoryMonitorCommitStatus
	body, err := json.Marshal(payload)
	if err != nil {
		return result, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	repositoryMonitorSetGitHubHeaders(req, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := repositoryMonitorHTTPClient(r).Do(req)
	if err != nil {
		return result, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := readRepositoryMonitorGitHubResponse(resp.Body, repositoryMonitorGitHubResponseLimit)
	if err != nil {
		return result, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := ""
		if repositoryMonitorGitHubErrorLooksRateLimited(string(data)) {
			detail = "rate limit"
		}
		return result, &repositoryMonitorGitHubAPIError{Operation: "readiness status", StatusCode: resp.StatusCode, Body: detail}
	}
	err = json.Unmarshal(data, &result)
	return result, err
}
