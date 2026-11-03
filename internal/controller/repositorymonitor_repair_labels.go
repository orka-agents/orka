package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// repositoryMonitorRepairSkipLabelActorNotPermitted records that the account
// that added a repair label lacks the permission label commands require.
const repositoryMonitorRepairSkipLabelActorNotPermitted = "repair_label_actor_not_permitted"

// repositoryMonitorRepairSkipScopeWithdrawn records that a queued policy
// repair lost its repair scope, such as a removed repair label, before it ran.
const repositoryMonitorRepairSkipScopeWithdrawn = "repair_scope_withdrawn"

// repositoryMonitorRepairSkipNoIntroducedCIFailure records that a queued
// policy CI repair found no failing check the pull request introduced.
const repositoryMonitorRepairSkipNoIntroducedCIFailure = "no_introduced_ci_failure"

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
	broke bool
	// incomplete means GitHub returned partial CI results for the head or
	// its base, so the failures cannot be classified yet.
	incomplete bool
	introduced []repositoryMonitorCIFailure
	inherited  []repositoryMonitorCIFailure
}

// errRepositoryMonitorCIIncomplete defers work that needs complete CI results.
var errRepositoryMonitorCIIncomplete = errors.New("CI results for the pull request head or its base are incomplete")

// repositoryMonitorBaseCICacheKey scopes base-commit CI results to one
// inventory run.
type repositoryMonitorBaseCICacheKey struct{}

type repositoryMonitorBaseCICache struct {
	mu      sync.Mutex
	results map[string]repositoryMonitorCIResult
}

// withRepositoryMonitorBaseCICache lets every pull request in one inventory
// run share the CI result of a base commit. Base results only classify head
// failures as introduced or inherited; head results are never cached.
func withRepositoryMonitorBaseCICache(ctx context.Context) context.Context {
	if _, ok := ctx.Value(repositoryMonitorBaseCICacheKey{}).(*repositoryMonitorBaseCICache); ok {
		return ctx
	}
	return context.WithValue(ctx, repositoryMonitorBaseCICacheKey{}, &repositoryMonitorBaseCICache{results: map[string]repositoryMonitorCIResult{}})
}

func (r *RepositoryMonitorReconciler) repositoryMonitorBaseCI(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, sha string) (repositoryMonitorCIResult, error) {
	cache, _ := ctx.Value(repositoryMonitorBaseCICacheKey{}).(*repositoryMonitorBaseCICache)
	key := strings.ToLower(strings.TrimSpace(monitor.Spec.RepoURL)) + "|" + sha
	if cache != nil {
		cache.mu.Lock()
		result, ok := cache.results[key]
		cache.mu.Unlock()
		if ok {
			return result, nil
		}
	}
	result, err := r.repositoryMonitorCheckCI(ctx, monitor, sha)
	if err == nil && cache != nil {
		cache.mu.Lock()
		cache.results[key] = result
		cache.mu.Unlock()
	}
	return result, err
}

// repositoryMonitorCIFailureNames names failures for a repair prompt. A name
// that a failure in other also carries is qualified with its source, so a
// check run and a status context sharing a name stay distinguishable.
func repositoryMonitorCIFailureNames(failures, other []repositoryMonitorCIFailure) []string {
	otherNames := make(map[string]struct{}, len(other))
	for _, failure := range other {
		otherNames[strings.ToLower(strings.TrimSpace(failure.name))] = struct{}{}
	}
	names := make([]string, 0, len(failures))
	for _, failure := range failures {
		name := failure.name
		if _, shared := otherNames[strings.ToLower(strings.TrimSpace(failure.name))]; shared {
			name = fmt.Sprintf("%s (%s)", failure.name, repositoryMonitorCIFailureSourceLabel(failure.source))
		}
		names = append(names, name)
	}
	return names
}

func repositoryMonitorCIFailureSourceLabel(source string) string {
	if app, ok := strings.CutPrefix(source, "check_run:"); ok {
		return "check run from GitHub App " + app
	}
	if source == repositoryMonitorCIFailureSourceStatus {
		return "commit status"
	}
	return source
}

func (r *RepositoryMonitorReconciler) repositoryMonitorPullRequestCIFailures(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, pr repositoryMonitorPullRequest) (repositoryMonitorCIFailures, error) {
	head, err := r.repositoryMonitorCheckCI(ctx, monitor, pr.HeadSHA)
	if err != nil {
		return repositoryMonitorCIFailures{}, err
	}
	if head.reason == repositoryMonitorCIChecksIncomplete {
		return repositoryMonitorCIFailures{incomplete: true}, nil
	}
	if head.reason != repositoryMonitorCINotGreen {
		return repositoryMonitorCIFailures{}, nil
	}
	if strings.TrimSpace(pr.BaseSHA) == "" || len(head.failed) == 0 {
		failures := repositoryMonitorCIFailures{broke: true}
		failures.introduced = append(failures.introduced, head.failed...)
		return failures, nil
	}
	base, err := r.repositoryMonitorBaseCI(ctx, monitor, pr.BaseSHA)
	if err != nil {
		return repositoryMonitorCIFailures{}, err
	}
	// A partial base result could hide a failure the head inherits, which
	// would then count as introduced; wait for complete results.
	if base.reason == repositoryMonitorCIChecksIncomplete || base.incomplete {
		return repositoryMonitorCIFailures{incomplete: true}, nil
	}
	baseFailed := make(map[string]struct{}, len(base.failed))
	for _, failure := range base.failed {
		baseFailed[failure.key()] = struct{}{}
	}
	var failures repositoryMonitorCIFailures
	for _, failure := range head.failed {
		if _, ok := baseFailed[failure.key()]; ok {
			failures.inherited = append(failures.inherited, failure)
		} else {
			failures.introduced = append(failures.introduced, failure)
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

// repositoryMonitorPolicyRepairWithdrawnReason rechecks a queued controller
// policy repair before it creates a Task or updates the branch, since its
// grant can change after acceptance: the repair scope must still cover the
// intent, the account behind the current repair label must still be
// permitted, and a CI fix must still have a failure the pull request
// introduced. Explicit commands keep their own authorization.
func (r *RepositoryMonitorReconciler) repositoryMonitorPolicyRepairWithdrawnReason(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, command *store.CommandEvent, owner, repo string, pr repositoryMonitorPullRequest) (string, error) {
	if command == nil || command.Source != repositoryMonitorControllerPolicySource {
		return "", nil
	}
	full, ciOnly, labelled := repositoryMonitorPullRequestRepairScope(monitor.Spec, pr)
	if !full && (!ciOnly || command.Intent != repositoryMonitorCommandIntentFixCI) {
		return repositoryMonitorRepairSkipScopeWithdrawn, nil
	}
	if labelled {
		permitted, err := r.repositoryMonitorRepairLabelActorPermitted(ctx, monitor, owner, repo, pr, repositoryMonitorRepairLabels(monitor.Spec, full))
		if err != nil {
			return "", err
		}
		if !permitted {
			return repositoryMonitorRepairSkipLabelActorNotPermitted, nil
		}
	}
	if command.Intent == repositoryMonitorCommandIntentFixCI {
		// A lookup error or partial results defer the command rather than
		// repairing blind or dropping it.
		failures, err := r.repositoryMonitorPullRequestCIFailures(ctx, monitor, pr)
		if err != nil {
			return "", err
		}
		if failures.incomplete {
			return "", errRepositoryMonitorCIIncomplete
		}
		if !failures.broke {
			return repositoryMonitorRepairSkipNoIntroducedCIFailure, nil
		}
	}
	return "", nil
}
