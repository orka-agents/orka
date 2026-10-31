package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// repositoryMonitorRepairSkipLabelActorNotPermitted records that the account
// that added a repair label lacks the permission label commands require.
const repositoryMonitorRepairSkipLabelActorNotPermitted = "repair_label_actor_not_permitted"

// repositoryMonitorIssueEventPageLimit bounds the issue event scan. A pull
// request with a longer history fails closed rather than trusting a partial log.
const repositoryMonitorIssueEventPageLimit = 10

// repositoryMonitorPullRequestBrokeCI reports whether a failing CI check on
// the pull request head is the pull request's own. A check that also fails on
// the base commit was already broken and is left alone; with no base result
// to compare, every failure counts.
func (r *RepositoryMonitorReconciler) repositoryMonitorPullRequestBrokeCI(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, pr repositoryMonitorPullRequest) (bool, error) {
	failures, err := r.repositoryMonitorPullRequestCIFailures(ctx, monitor, pr)
	return failures.broke, err
}

// repositoryMonitorCIFailures splits the failing checks on a pull request
// head into those the pull request introduced and those also failing on its
// base commit.
type repositoryMonitorCIFailures struct {
	broke      bool
	introduced []string
	inherited  []string
}

func (r *RepositoryMonitorReconciler) repositoryMonitorPullRequestCIFailures(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, pr repositoryMonitorPullRequest) (repositoryMonitorCIFailures, error) {
	head, err := r.repositoryMonitorCheckCI(ctx, monitor, pr.HeadSHA)
	if err != nil || head.reason != repositoryMonitorCINotGreen {
		return repositoryMonitorCIFailures{}, err
	}
	if strings.TrimSpace(pr.BaseSHA) == "" || len(head.failed) == 0 {
		return repositoryMonitorCIFailures{broke: true, introduced: head.failed}, nil
	}
	base, err := r.repositoryMonitorCheckCI(ctx, monitor, pr.BaseSHA)
	if err != nil {
		return repositoryMonitorCIFailures{}, err
	}
	baseFailed := make(map[string]struct{}, len(base.failed))
	for _, name := range base.failed {
		baseFailed[strings.ToLower(strings.TrimSpace(name))] = struct{}{}
	}
	var failures repositoryMonitorCIFailures
	for _, name := range head.failed {
		if _, ok := baseFailed[strings.ToLower(strings.TrimSpace(name))]; ok {
			failures.inherited = append(failures.inherited, name)
		} else {
			failures.introduced = append(failures.introduced, name)
		}
	}
	failures.broke = len(failures.introduced) > 0
	return failures, nil
}

// repositoryMonitorRepairLabelActorPermitted reports whether an account that
// added one of labels to pr has the permission label commands require.
// Inventory sees labels but not who added them, so the issue event log names
// the account behind each current label. An unresolved account is refused.
func (r *RepositoryMonitorReconciler) repositoryMonitorRepairLabelActorPermitted(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, owner, repo string, pr repositoryMonitorPullRequest, labels []string) (bool, error) {
	present := map[string]struct{}{}
	for _, label := range pr.Labels {
		if repositoryMonitorMatchingLabel(labels, []string{label}) != "" {
			present[strings.ToLower(strings.TrimSpace(label))] = struct{}{}
		}
	}
	if len(present) == 0 {
		return false, nil
	}
	token, err := r.repositoryMonitorGitHubToken(ctx, monitor)
	if err != nil {
		return false, err
	}
	baseURL := strings.TrimRight(r.GitHubAPIBaseURL, "/")
	if baseURL == "" {
		baseURL = repositoryMonitorDefaultGitHubAPIBaseURL
	}
	// Events are oldest first, so the last labeled event added the current label.
	actors := map[string]string{}
	complete := false
	for page := 1; page <= repositoryMonitorIssueEventPageLimit; page++ {
		endpoint := fmt.Sprintf("%s/repos/%s/%s/issues/%d/events?per_page=%d&page=%d", baseURL, url.PathEscape(owner), url.PathEscape(repo), pr.Number, repositoryMonitorGitHubPerPage, page)
		var events []struct {
			Event string `json:"event"`
			Actor struct {
				Login string `json:"login"`
			} `json:"actor"`
			Label struct {
				Name string `json:"name"`
			} `json:"label"`
		}
		if err := r.fetchRepositoryMonitorAuthorizedJSON(ctx, endpoint, token, &events); err != nil {
			return false, err
		}
		for _, event := range events {
			name := strings.ToLower(strings.TrimSpace(event.Label.Name))
			if _, ok := present[name]; !ok {
				continue
			}
			// A removal seen after the snapshot revokes the earlier labeller,
			// so a label removed since the snapshot fails closed.
			switch event.Event {
			case "labeled":
				actors[name] = strings.TrimSpace(event.Actor.Login)
			case "unlabeled":
				delete(actors, name)
			}
		}
		if len(events) < repositoryMonitorGitHubPerPage {
			complete = true
			break
		}
	}
	if !complete {
		return false, nil
	}
	for _, actor := range actors {
		if actor == "" {
			continue
		}
		permission, err := r.repositoryMonitorCollaboratorPermission(ctx, baseURL, owner, repo, token, actor)
		if err != nil {
			return false, err
		}
		if repositoryMonitorRepairActorPermissionAllowed(monitor.Spec, permission) {
			return true, nil
		}
	}
	return false, nil
}

// repositoryMonitorCollaboratorPermission returns actor's repository role, or
// "" when actor is not a collaborator.
func (r *RepositoryMonitorReconciler) repositoryMonitorCollaboratorPermission(ctx context.Context, baseURL, owner, repo, token, actor string) (string, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/collaborators/%s/permission", baseURL, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(actor))
	var response struct {
		Permission string `json:"permission"`
		RoleName   string `json:"role_name"`
	}
	if err := r.fetchRepositoryMonitorAuthorizedJSON(ctx, endpoint, token, &response); err != nil {
		var apiErr *repositoryMonitorGitHubAPIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return "", nil
		}
		return "", err
	}
	// Custom roles report their base permission in permission.
	switch role := strings.ToLower(strings.TrimSpace(response.RoleName)); role {
	case "read", "triage", "write", githubPermissionMaintain, githubPermissionAdmin:
		return role, nil
	}
	return strings.ToLower(strings.TrimSpace(response.Permission)), nil
}

// repositoryMonitorRepairActorPermissionAllowed mirrors the permission check
// for GitHub label commands.
func repositoryMonitorRepairActorPermissionAllowed(spec corev1alpha1.RepositoryMonitorSpec, permission string) bool {
	allowed := []string{"write", githubPermissionMaintain, githubPermissionAdmin}
	switch strings.ToLower(strings.TrimSpace(spec.Triggers.GitHub.Labels.RequireActorPermission)) {
	case githubPermissionAdmin:
		allowed = []string{githubPermissionAdmin}
	case githubPermissionMaintain:
		allowed = []string{githubPermissionMaintain, githubPermissionAdmin}
	}
	if !repositoryMonitorPermissionListed(allowed, permission) {
		return false
	}
	policy := spec.Policy.AllowedRepositoryPermissions
	return len(policy) == 0 || repositoryMonitorPermissionListed(policy, permission)
}

func repositoryMonitorPermissionListed(permissions []string, permission string) bool {
	return slices.ContainsFunc(permissions, func(candidate string) bool {
		return strings.EqualFold(strings.TrimSpace(candidate), permission)
	})
}
