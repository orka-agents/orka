package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

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
	// pending lists head check runs that had not completed.
	pending []repositoryMonitorCIFailure
	// headIncomplete means the failing head result came from partial check
	// runs or statuses, so introduced may not list every failure.
	headIncomplete bool
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
		failures := repositoryMonitorCIFailures{broke: true, pending: head.pending, headIncomplete: head.incomplete}
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
	failures := repositoryMonitorCIFailures{pending: head.pending, headIncomplete: head.incomplete}
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

// repositoryMonitorRepairLabelActorRefusal returns the reason a new
// automatic repair of a pull request whose repair scope comes from a repair
// label is refused because no account that added a current repair label has
// the permission label commands require. It returns "" when the pull request
// is not labelled into scope or the label's account is permitted.
func (r *RepositoryMonitorReconciler) repositoryMonitorRepairLabelActorRefusal(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, owner, repo string, pr repositoryMonitorPullRequest, full, labelled bool) (string, error) {
	if !labelled {
		return "", nil
	}
	permitted, err := r.repositoryMonitorRepairLabelActorPermitted(ctx, monitor, owner, repo, pr, repositoryMonitorRepairLabels(monitor.Spec, full))
	if err != nil {
		return "", err
	}
	if !permitted {
		return repositoryMonitorRepairSkipLabelActorNotPermitted, nil
	}
	return "", nil
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
// repositoryMonitorPermissionLookupBudget bounds the collaborator
// permission requests one monitor poll makes across every pull request,
// readiness, and repair label check, so a busy repository cannot spend
// GitHub's rate limit on permission lookups.
const repositoryMonitorPermissionLookupBudget = 60

// repositoryMonitorPermissionMemoryTTL is how long a permission one poll
// read answers later polls of the same monitor, so lookups a poll repeats do
// not spend its budget. A repair handoff, which creates a Task, never uses a
// remembered permission.
const repositoryMonitorPermissionMemoryTTL = 3 * time.Minute

// errRepositoryMonitorPermissionLookupBudget reports that this poll spent its
// permission lookup budget. Like any failed lookup it leaves the reviewer's
// authority unknown until a later poll checks it.
var errRepositoryMonitorPermissionLookupBudget = errors.New("repository permission lookups for this poll are spent")

// repositoryMonitorPermissionCache caches collaborator permissions for one
// monitor poll, so every pull request, readiness, and repair label check
// in it looks each account up at most once. A failed lookup is cached too,
// so a throttled account is not asked again in the same poll. Permissions
// can change, so the cache lives only as long as the poll's context.
//
// The budget is shared fairly across polls: accounts a poll could not look
// up because its budget was spent are reserved budget in the next poll, so
// pull requests visited late in every poll are not starved by early ones.
type repositoryMonitorPermissionCache struct {
	mu      sync.Mutex
	entries map[string]repositoryMonitorPermissionResult
	lookups int
	// memory is the monitor's permission memory, nil outside a poll.
	memory *repositoryMonitorPermissionMemory
	// reserved are the accounts the previous poll deferred that this poll
	// has not looked up yet; deferred are the ones this poll defers.
	reserved map[string]bool
	deferred map[string]bool
}

type repositoryMonitorPermissionResult struct {
	permission string
	err        error
}

// repositoryMonitorPermissionMemory is what one monitor's polls carry
// forward: recent permission results, and the accounts the last poll
// deferred.
type repositoryMonitorPermissionMemory struct {
	mu       sync.Mutex
	results  map[string]repositoryMonitorRememberedPermission
	deferred map[string]bool
}

type repositoryMonitorRememberedPermission struct {
	permission string
	at         time.Time
}

type repositoryMonitorPermissionCacheKey struct{}

type repositoryMonitorFreshPermissionsKey struct{}

// withRepositoryMonitorPermissionCache returns ctx with a new permission
// cache for one monitor poll that carries nothing between polls.
func withRepositoryMonitorPermissionCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, repositoryMonitorPermissionCacheKey{}, &repositoryMonitorPermissionCache{entries: map[string]repositoryMonitorPermissionResult{}, deferred: map[string]bool{}})
}

// withRepositoryMonitorPermissionPoll returns ctx with the permission cache
// of one poll of monitor, and a function that ends the poll, carrying the
// accounts it deferred to the next one.
func (r *RepositoryMonitorReconciler) withRepositoryMonitorPermissionPoll(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor) (context.Context, func()) {
	value, _ := r.permissionMemory.LoadOrStore(monitor.UID, &repositoryMonitorPermissionMemory{results: map[string]repositoryMonitorRememberedPermission{}})
	memory := value.(*repositoryMonitorPermissionMemory)
	memory.mu.Lock()
	reserved := maps.Clone(memory.deferred)
	memory.mu.Unlock()
	if reserved == nil {
		reserved = map[string]bool{}
	}
	cache := &repositoryMonitorPermissionCache{entries: map[string]repositoryMonitorPermissionResult{}, memory: memory, reserved: reserved, deferred: map[string]bool{}}
	return context.WithValue(ctx, repositoryMonitorPermissionCacheKey{}, cache), func() {
		cache.mu.Lock()
		deferred := maps.Clone(cache.deferred)
		cache.mu.Unlock()
		memory.mu.Lock()
		defer memory.mu.Unlock()
		memory.deferred = deferred
		for key, remembered := range memory.results {
			if time.Since(remembered.at) >= repositoryMonitorPermissionMemoryTTL {
				delete(memory.results, key)
			}
		}
	}
}

// withRepositoryMonitorFreshPermissions returns ctx whose permission
// lookups never use a permission an earlier poll read.
func withRepositoryMonitorFreshPermissions(ctx context.Context) context.Context {
	return context.WithValue(ctx, repositoryMonitorFreshPermissionsKey{}, true)
}

func (r *RepositoryMonitorReconciler) repositoryMonitorCollaboratorPermission(ctx context.Context, baseURL, owner, repo, token, actor string) (string, error) {
	cache, ok := ctx.Value(repositoryMonitorPermissionCacheKey{}).(*repositoryMonitorPermissionCache)
	if !ok {
		return r.lookUpRepositoryMonitorCollaboratorPermission(ctx, baseURL, owner, repo, token, actor)
	}
	key := strings.ToLower(baseURL + "|" + owner + "/" + repo + "|" + actor)
	if permission, ok := cache.remembered(ctx, key); ok {
		return permission, nil
	}
	cache.mu.Lock()
	if cached, ok := cache.entries[key]; ok {
		cache.mu.Unlock()
		return cached.permission, cached.err
	}
	// Accounts the previous poll deferred keep budget of their own.
	reserved := len(cache.reserved)
	if cache.reserved[key] {
		reserved--
	}
	if cache.lookups+reserved >= repositoryMonitorPermissionLookupBudget {
		cache.deferred[key] = true
		cache.mu.Unlock()
		return "", errRepositoryMonitorPermissionLookupBudget
	}
	cache.lookups++
	delete(cache.reserved, key)
	cache.mu.Unlock()
	permission, err := r.lookUpRepositoryMonitorCollaboratorPermission(ctx, baseURL, owner, repo, token, actor)
	cache.mu.Lock()
	cache.entries[key] = repositoryMonitorPermissionResult{permission: permission, err: err}
	cache.mu.Unlock()
	if err == nil && cache.memory != nil {
		cache.memory.mu.Lock()
		cache.memory.results[key] = repositoryMonitorRememberedPermission{permission: permission, at: time.Now()}
		cache.memory.mu.Unlock()
	}
	return permission, err
}

// remembered returns a permission an earlier poll of the monitor read within
// the memory TTL, unless ctx asks for fresh permissions.
func (c *repositoryMonitorPermissionCache) remembered(ctx context.Context, key string) (string, bool) {
	if c.memory == nil || ctx.Value(repositoryMonitorFreshPermissionsKey{}) != nil {
		return "", false
	}
	c.memory.mu.Lock()
	defer c.memory.mu.Unlock()
	remembered, ok := c.memory.results[key]
	if !ok || time.Since(remembered.at) >= repositoryMonitorPermissionMemoryTTL {
		return "", false
	}
	return remembered.permission, true
}

func (r *RepositoryMonitorReconciler) lookUpRepositoryMonitorCollaboratorPermission(ctx context.Context, baseURL, owner, repo, token, actor string) (string, error) {
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
	return repositoryMonitorPermissionMeets(spec.Triggers.GitHub.Labels.RequireActorPermission, spec.Policy.AllowedRepositoryPermissions, permission)
}

// repositoryMonitorPermissionMeets reports whether permission is at least
// minimum (write, maintain, or admin; anything else means write) and, when
// policy is set, listed in it.
func repositoryMonitorPermissionMeets(minimum string, policy []string, permission string) bool {
	allowed := []string{"write", githubPermissionMaintain, githubPermissionAdmin}
	switch strings.ToLower(strings.TrimSpace(minimum)) {
	case githubPermissionAdmin:
		allowed = []string{githubPermissionAdmin}
	case githubPermissionMaintain:
		allowed = []string{githubPermissionMaintain, githubPermissionAdmin}
	}
	if !repositoryMonitorPermissionListed(allowed, permission) {
		return false
	}
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
	if reason, err := r.repositoryMonitorRepairLabelActorRefusal(ctx, monitor, owner, repo, pr, full, labelled); err != nil || reason != "" {
		return reason, err
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
