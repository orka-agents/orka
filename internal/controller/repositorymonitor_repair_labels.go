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
	"k8s.io/apimachinery/pkg/types"
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

// repositoryMonitorPermissionWaitingExpiry drops a waiting scope no poll
// has deferred for this long, such as a pull request that closed.
const repositoryMonitorPermissionWaitingExpiry = 15 * time.Minute

// repositoryMonitorPermissionCache caches collaborator permissions for one
// monitor poll, so every pull request, readiness, and repair label check
// in it looks each account up at most once. A failed lookup is cached too,
// so a throttled account is not asked again in the same poll. Permissions
// can change, so the cache lives only as long as the poll's context.
//
// The budget is shared fairly across polls by scope: the lookups one
// feedback snapshot of a pull request needs, or one account looked up on
// its own. A scope this poll's budget cut short waits, with every account it
// needs, and later polls reserve whole waiting scopes, the longest-waiting
// first, within half their budget. All of a reserved scope's accounts are
// looked up in the same poll, so its snapshot completes even when
// remembered permissions expire between polls.
type repositoryMonitorPermissionCache struct {
	mu      sync.Mutex
	entries map[string]repositoryMonitorPermissionResult
	lookups int
	// memory is the monitor's permission memory, nil outside a poll.
	memory *repositoryMonitorPermissionMemory
	// reserved are the accounts of the waiting scopes this poll reserves
	// that it has not looked up yet.
	reserved map[string]bool
	// scopes are the scopes this poll looked up accounts for.
	scopes map[string]*repositoryMonitorPermissionPollScope
}

type repositoryMonitorPermissionResult struct {
	permission string
	err        error
}

// repositoryMonitorPermissionPollScope is what one poll saw of a scope: the
// accounts it needed, and whether the budget refused any of them.
type repositoryMonitorPermissionPollScope struct {
	keys    map[string]bool
	refused bool
}

// repositoryMonitorPermissionMemory is what one monitor's polls carry
// forward: recent permission results, and the scopes waiting for budget.
type repositoryMonitorPermissionMemory struct {
	mu      sync.Mutex
	results map[string]repositoryMonitorRememberedPermission
	waiting map[string]*repositoryMonitorPermissionWaiting
	polls   int
	// used is when a poll last started.
	used time.Time
}

// repositoryMonitorPermissionMemoryIdleExpiry evicts the permission memory
// of a monitor no poll has used for this long, and
// repositoryMonitorPermissionMemoryLimit bounds how many monitors' memories
// the controller keeps, evicting the least recently used. Both back up the
// eviction of a deleted monitor's memory.
const (
	repositoryMonitorPermissionMemoryIdleExpiry = time.Hour
	repositoryMonitorPermissionMemoryLimit      = 1024
	repositoryMonitorPermissionSweepInterval    = time.Minute
)

// trackRepositoryMonitorUID records the UID that monitor's in-memory state
// belongs to. A monitor recreated under the same name has a new UID, so the
// state of the one it replaced is evicted.
func (r *RepositoryMonitorReconciler) trackRepositoryMonitorUID(monitor *corev1alpha1.RepositoryMonitor) {
	previous, loaded := r.monitorUIDs.Swap(monitor.Namespace+"/"+monitor.Name, monitor.UID)
	if loaded && previous != monitor.UID {
		r.evictRepositoryMonitorMemory(previous.(types.UID))
	}
}

// forgetRepositoryMonitor evicts the in-memory state of the deleted monitor
// with key namespace/name.
func (r *RepositoryMonitorReconciler) forgetRepositoryMonitor(key string) {
	if uid, loaded := r.monitorUIDs.LoadAndDelete(key); loaded {
		r.evictRepositoryMonitorMemory(uid.(types.UID))
	}
}

func (r *RepositoryMonitorReconciler) evictRepositoryMonitorMemory(uid types.UID) {
	r.permissionMemory.Delete(uid)
	r.feedbackVerifyCursors.Delete(uid)
}

// sweepRepositoryMonitorPermissionMemories evicts, at most once per sweep
// interval, the permission memories no poll used within the idle expiry, and
// then the least recently used ones beyond the limit.
func (r *RepositoryMonitorReconciler) sweepRepositoryMonitorPermissionMemories() {
	now := time.Now()
	r.permissionSweepMu.Lock()
	if now.Sub(r.permissionSweptAt) < repositoryMonitorPermissionSweepInterval {
		r.permissionSweepMu.Unlock()
		return
	}
	r.permissionSweptAt = now
	r.permissionSweepMu.Unlock()
	type entry struct {
		uid  types.UID
		used time.Time
	}
	var kept []entry
	r.permissionMemory.Range(func(key, value any) bool {
		memory := value.(*repositoryMonitorPermissionMemory)
		memory.mu.Lock()
		used := memory.used
		memory.mu.Unlock()
		if now.Sub(used) >= repositoryMonitorPermissionMemoryIdleExpiry {
			r.evictRepositoryMonitorMemory(key.(types.UID))
			return true
		}
		kept = append(kept, entry{uid: key.(types.UID), used: used})
		return true
	})
	if len(kept) <= repositoryMonitorPermissionMemoryLimit {
		return
	}
	slices.SortFunc(kept, func(a, b entry) int { return a.used.Compare(b.used) })
	for _, stale := range kept[:len(kept)-repositoryMonitorPermissionMemoryLimit] {
		r.evictRepositoryMonitorMemory(stale.uid)
	}
}

type repositoryMonitorRememberedPermission struct {
	permission string
	at         time.Time
}

// repositoryMonitorPermissionWaiting is a scope waiting for budget: the poll
// that first cut it short, the accounts it needs, and when a poll last did.
type repositoryMonitorPermissionWaiting struct {
	since int
	keys  map[string]bool
	seen  time.Time
}

type repositoryMonitorPermissionCacheKey struct{}

type repositoryMonitorFreshPermissionsKey struct{}

type repositoryMonitorPermissionScopeKey struct{}

// withRepositoryMonitorPermissionCache returns ctx with a new permission
// cache for one monitor poll that carries nothing between polls.
func withRepositoryMonitorPermissionCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, repositoryMonitorPermissionCacheKey{}, &repositoryMonitorPermissionCache{entries: map[string]repositoryMonitorPermissionResult{}, scopes: map[string]*repositoryMonitorPermissionPollScope{}})
}

// withRepositoryMonitorPermissionScope returns ctx whose permission lookups
// belong to scope, such as one pull request's feedback snapshot.
func withRepositoryMonitorPermissionScope(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, repositoryMonitorPermissionScopeKey{}, scope)
}

// withRepositoryMonitorPermissionPoll returns ctx with the permission cache
// of one poll of monitor, and a function that ends the poll. Ending it
// merges what the poll saw into the waiting scopes: a scope it cut short
// waits with the accounts it needs now, keeping when it first waited, one it
// completed stops waiting, and
// one it did not reach is left as it was, so a reconcile that looks nothing
// up changes nothing.
func (r *RepositoryMonitorReconciler) withRepositoryMonitorPermissionPoll(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor) (context.Context, func()) {
	r.trackRepositoryMonitorUID(monitor)
	r.sweepRepositoryMonitorPermissionMemories()
	value, _ := r.permissionMemory.LoadOrStore(monitor.UID, &repositoryMonitorPermissionMemory{
		results: map[string]repositoryMonitorRememberedPermission{}, waiting: map[string]*repositoryMonitorPermissionWaiting{},
	})
	memory := value.(*repositoryMonitorPermissionMemory)
	memory.mu.Lock()
	memory.polls++
	memory.used = time.Now()
	poll := memory.polls
	waiting := slices.Collect(maps.Keys(memory.waiting))
	slices.SortFunc(waiting, func(a, b string) int {
		if c := memory.waiting[a].since - memory.waiting[b].since; c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	// A scope that does not fit what is left of the reservation is passed
	// over for this poll, so it cannot keep later scopes from being served.
	// It is still served when the poll's budget allows, and a snapshot never
	// needs more lookups than its own limit, which fits the reservation.
	reserved := map[string]bool{}
	for _, scope := range waiting {
		keys := memory.waiting[scope].keys
		added := 0
		for key := range keys {
			if !reserved[key] {
				added++
			}
		}
		if len(reserved)+added > repositoryMonitorPermissionLookupBudget/2 {
			continue
		}
		maps.Copy(reserved, keys)
	}
	memory.mu.Unlock()
	cache := &repositoryMonitorPermissionCache{entries: map[string]repositoryMonitorPermissionResult{}, memory: memory, reserved: reserved, scopes: map[string]*repositoryMonitorPermissionPollScope{}}
	return context.WithValue(ctx, repositoryMonitorPermissionCacheKey{}, cache), func() {
		cache.mu.Lock()
		scopes := cache.scopes
		cache.scopes = map[string]*repositoryMonitorPermissionPollScope{}
		cache.mu.Unlock()
		now := time.Now()
		memory.mu.Lock()
		defer memory.mu.Unlock()
		for scope, seen := range scopes {
			if !seen.refused {
				delete(memory.waiting, scope)
				continue
			}
			// The scope now needs the accounts this poll saw, its snapshot's
			// current reviewers, not ones it needed before.
			entry := memory.waiting[scope]
			if entry == nil {
				entry = &repositoryMonitorPermissionWaiting{since: poll}
				memory.waiting[scope] = entry
			}
			entry.keys = maps.Clone(seen.keys)
			entry.seen = now
		}
		for scope, entry := range memory.waiting {
			if now.Sub(entry.seen) >= repositoryMonitorPermissionWaitingExpiry {
				delete(memory.waiting, scope)
			}
		}
		for key, remembered := range memory.results {
			if now.Sub(remembered.at) >= repositoryMonitorPermissionMemoryTTL {
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
	scope, _ := ctx.Value(repositoryMonitorPermissionScopeKey{}).(string)
	if scope == "" {
		scope = "account:" + key
	}
	cache.mu.Lock()
	seen := cache.scopes[scope]
	if seen == nil {
		seen = &repositoryMonitorPermissionPollScope{keys: map[string]bool{}}
		cache.scopes[scope] = seen
	}
	seen.keys[key] = true
	cache.mu.Unlock()
	if permission, ok := cache.remembered(ctx, key); ok {
		return permission, nil
	}
	cache.mu.Lock()
	if cached, ok := cache.entries[key]; ok {
		cache.mu.Unlock()
		return cached.permission, cached.err
	}
	// The accounts of reserved scopes keep budget of their own.
	reserved := len(cache.reserved)
	if cache.reserved[key] {
		reserved--
	}
	if cache.lookups+reserved >= repositoryMonitorPermissionLookupBudget {
		seen.refused = true
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
