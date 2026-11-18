package controller

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// newRepositoryMonitorLabelledUpdateFixture returns a fixture whose pull
// request a writer labelled into repair scope, a branch update the policy
// queued for it, and a count of the branch update PUTs sent. The pull
// request's head does not contain its base, so an authorized update would
// be sent.
func newRepositoryMonitorLabelledUpdateFixture(t *testing.T) (*repositoryMonitorFeedbackRepairFixture, repositoryMonitorPullRequest, store.CommandEvent, *atomic.Int32) {
	t.Helper()
	f := newRepositoryMonitorFeedbackRepairFixture(t, func(monitor *corev1alpha1.RepositoryMonitor) {
		monitor.Spec.Repair.IncludeLabels = []string{"orka:fix"}
	})
	*f.labelEvents = `[{"event":"labeled","actor":{"login":"labeller"},"label":{"name":"orka:fix"}}]`
	f.github.permissions["labeller"] = "write"
	puts := &atomic.Int32{}
	f.handle = func(w http.ResponseWriter, req *http.Request) bool {
		switch {
		case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/repos/orka-agents/orka/compare/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"behind"}`))
			return true
		case req.Method == http.MethodPut && req.URL.Path == "/repos/orka-agents/orka/pulls/1/update-branch":
			puts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"message":"Updating pull request branch."}`))
			return true
		}
		return false
	}
	pr := repositoryMonitorFeedbackRepairTestPR()
	pr.Labels = []string{"orka:fix"}
	command := store.CommandEvent{ID: "policy-update", CommentID: "policy-update", DedupeKey: "policy-update", MonitorNamespace: "default", MonitorName: f.monitor.Name,
		Repo: "orka-agents/orka", Kind: repositoryMonitorPullRequestKind, Number: 1, Source: repositoryMonitorControllerPolicySource, Author: repositoryMonitorControllerActor,
		Permission: "repository_repair_policy", Intent: repositoryMonitorCommandIntentUpdateBranch, Command: repositoryMonitorCommandIntentUpdateBranch, HeadSHA: "head1111", Status: repositoryMonitorCommandAccepted}
	if err := f.store.CreateCommandEvent(context.Background(), &command); err != nil {
		t.Fatal(err)
	}
	return f, pr, command, puts
}

func (f *repositoryMonitorFeedbackRepairFixture) setPermission(login, permission string) {
	f.github.mu.Lock()
	f.github.permissions[login] = permission
	f.github.mu.Unlock()
}

// A labeller whose write access a poll's readiness check read, then lost in
// the same poll, does not authorize the branch update that poll hands off:
// authorization skips the poll's cache as well as the memory. (Fail-open
// check of f59e9b70e, finding 1.)
func TestRepositoryMonitorBranchUpdateRefusedForLabellerRevokedMidPoll(t *testing.T) {
	ctx := context.Background()
	f, pr, command, puts := newRepositoryMonitorLabelledUpdateFixture(t)
	poll, end := f.reconciler.withRepositoryMonitorPermissionPoll(ctx, f.monitor)
	defer end()
	// Readiness of a pull request sharing the head reads the labeller's
	// write access into the poll's cache.
	if reason, err := f.reconciler.repositoryMonitorRepairLabelActorRefusal(poll, repositoryMonitorPermissionForScheduling, f.monitor, "orka-agents", "orka", pr, true, true); err != nil || reason != "" {
		t.Fatalf("labeller refused before revocation: %q err=%v", reason, err)
	}
	f.setPermission("labeller", "read")
	item := repositoryMonitorItemFromPullRequest(f.monitor, pr, nil)
	if _, _, err := f.reconciler.tryProcessPullRequestCommandRun(poll, f.monitor, f.commandRun(command), "orka-agents", "orka", pr, item); err != nil {
		t.Fatal(err)
	}
	if got := puts.Load(); got != 0 {
		t.Fatalf("branch update sent %d times, want none for the revoked labeller", got)
	}
	action, err := f.store.GetWorkAction(ctx, "default", store.RepositoryMonitorWorkActionID(command.ID, store.RepositoryMonitorDesiredActionForIntent(command.Intent)))
	if err != nil || action.Status != repositoryMonitorWorkActionStatusBlocked || action.BlockedReason != repositoryMonitorRepairSkipLabelActorNotPermitted {
		t.Fatalf("action = %+v err=%v, want the branch update refused", action, err)
	}
}

// A branch update a crash left recorded as submitting, before its first PUT,
// is not sent once the labeller's access is revoked: recovery rechecks
// authorization, records the push failed, and never sends it later.
// (Fail-open check of f59e9b70e, finding 3.)
func TestRepositoryMonitorBranchUpdateRecoveryRefusedAfterRevocation(t *testing.T) {
	ctx := context.Background()
	f, pr, command, puts := newRepositoryMonitorLabelledUpdateFixture(t)
	submittingAt := time.Now()
	mutation := &store.GitHubMutationRecord{
		ID: repositoryMonitorUpdateBranchMutationID(command.ID), RunID: f.commandRun(command).ID, CommandEventID: command.ID,
		Operation: repositoryMonitorUpdateBranchOperation, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: pr.Number, TargetSHA: pr.HeadSHA,
		Reason: command.Intent, GitHubURL: pr.BaseBranch, ExternalID: pr.BaseSHA, Status: repositoryMonitorUpdateBranchSubmitting, PendingAt: &submittingAt,
	}
	if err := f.reconciler.recordRepositoryMonitorGitHubMutation(ctx, f.monitor, mutation); err != nil {
		t.Fatal(err)
	}
	f.setPermission("labeller", "read")
	for range 2 {
		poll, end := f.reconciler.withRepositoryMonitorPermissionPoll(ctx, f.monitor)
		item := repositoryMonitorItemFromPullRequest(f.monitor, pr, nil)
		_, _, err := f.reconciler.tryProcessPullRequestCommandRun(poll, f.monitor, f.commandRun(command), "orka-agents", "orka", pr, item)
		end()
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := puts.Load(); got != 0 {
		t.Fatalf("recovered branch update sent %d times, want none after revocation", got)
	}
	recorded, err := f.store.GetGitHubMutationRecord(ctx, "default", mutation.ID)
	if err != nil || recorded.Status != repositoryMonitorRunPhaseFailed || recorded.Error != repositoryMonitorRepairSkipLabelActorNotPermitted {
		t.Fatalf("mutation = %+v err=%v, want the push recorded refused", recorded, err)
	}
	action, err := f.store.GetWorkAction(ctx, "default", store.RepositoryMonitorWorkActionID(command.ID, store.RepositoryMonitorDesiredActionForIntent(command.Intent)))
	if err != nil || action.Status == repositoryMonitorWorkActionStatusSucceeded || action.Status == repositoryMonitorWorkActionStatusRunning {
		t.Fatalf("action = %+v err=%v, want the branch update not running or succeeded", action, err)
	}
}

// The same recovery still sends the branch update while the labeller keeps
// write access, so the recheck refuses only a revoked grant.
func TestRepositoryMonitorBranchUpdateRecoverySentWhileAuthorized(t *testing.T) {
	ctx := context.Background()
	f, pr, command, puts := newRepositoryMonitorLabelledUpdateFixture(t)
	submittingAt := time.Now()
	mutation := &store.GitHubMutationRecord{
		ID: repositoryMonitorUpdateBranchMutationID(command.ID), RunID: f.commandRun(command).ID, CommandEventID: command.ID,
		Operation: repositoryMonitorUpdateBranchOperation, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: pr.Number, TargetSHA: pr.HeadSHA,
		Reason: command.Intent, GitHubURL: pr.BaseBranch, ExternalID: pr.BaseSHA, Status: repositoryMonitorUpdateBranchSubmitting, PendingAt: &submittingAt,
	}
	if err := f.reconciler.recordRepositoryMonitorGitHubMutation(ctx, f.monitor, mutation); err != nil {
		t.Fatal(err)
	}
	poll, end := f.reconciler.withRepositoryMonitorPermissionPoll(ctx, f.monitor)
	defer end()
	item := repositoryMonitorItemFromPullRequest(f.monitor, pr, nil)
	if _, _, err := f.reconciler.tryProcessPullRequestCommandRun(poll, f.monitor, f.commandRun(command), "orka-agents", "orka", pr, item); err != nil {
		t.Fatal(err)
	}
	if got := puts.Load(); got != 1 {
		t.Fatalf("recovered branch update sent %d times, want once", got)
	}
}

// A reviewer an earlier poll read without authority, then granted write,
// holds readiness back: a remembered absence of authority never lets
// readiness clear. (Fail-open check of f59e9b70e, finding 2.)
func TestRepositoryMonitorReadinessHeldForReviewerGrantedWrite(t *testing.T) {
	ctx := context.Background()
	f := newRepositoryMonitorFeedbackRepairFixture(t, nil)
	f.github.mu.Lock()
	f.github.threadPages = []string{"[" + repositoryMonitorFeedbackTestThreadJSON("T2", 22, "outsider", "Please handle the nil case.") + "]"}
	f.github.reviewPages = []string{"[]"}
	f.github.permissions["outsider"] = "read"
	f.github.mu.Unlock()
	pr := repositoryMonitorFeedbackRepairTestPR()
	readiness := func() (string, bool) {
		t.Helper()
		poll, end := f.reconciler.withRepositoryMonitorPermissionPoll(ctx, f.monitor)
		defer end()
		state, _, held, err := f.reconciler.repositoryMonitorFeedbackReadiness(poll, f.monitor, pr)
		if err != nil {
			t.Fatal(err)
		}
		return state, held
	}
	if _, held := readiness(); held {
		t.Fatal("read-only reviewer's feedback held readiness, want it ignored")
	}
	// Within the memory's TTL the reviewer gains write.
	f.setPermission("outsider", "write")
	if state, held := readiness(); !held || state == repositoryMonitorStatusSuccess {
		t.Fatalf("readiness = %q held=%v, want the newly authorized feedback to hold the head", state, held)
	}
}

// Only the uncached authorization lookup and the scheduling lookup reach
// GitHub's permission API; only repositoryMonitorPermission dispatches to
// them; and only readiness and repair admission, which a write's own
// authorization follows, ask for scheduling permissions. A new caller must
// be added here deliberately.
func TestRepositoryMonitorWritePathsUseUncachedAuthorization(t *testing.T) {
	want := map[string][]string{
		"lookUpRepositoryMonitorCollaboratorPermission": {"repositoryMonitorAuthorizationPermission", "repositoryMonitorSchedulingPermission"},
		"repositoryMonitorSchedulingPermission":         {"repositoryMonitorPermission"},
		"repositoryMonitorAuthorizationPermission":      {"repositoryMonitorPermission"},
		"repositoryMonitorPermission":                   {"buildRepositoryMonitorReviewFeedback", "repositoryMonitorRepairLabelActorPermitted"},
		"repositoryMonitorPermissionForScheduling":      {"repositoryMonitorFeedbackReadiness", "repositoryMonitorPermission", "tryRepositoryMonitorFeedbackRepair"},
	}
	got := map[string]map[string]bool{}
	for name := range want {
		got[name] = map[string]bool{}
	}
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				var ident string
				switch n := node.(type) {
				case *ast.Ident:
					ident = n.Name
				case *ast.SelectorExpr:
					ident = n.Sel.Name
				default:
					return true
				}
				if callers, ok := got[ident]; ok {
					callers[fn.Name.Name] = true
				}
				return true
			})
		}
	}
	for name, callers := range want {
		if have := slices.Sorted(maps.Keys(got[name])); !slices.Equal(have, callers) {
			t.Errorf("%s is used by %v, want %v", name, have, callers)
		}
	}
}

// repositoryMonitorFeedbackMissingActionStore silently drops the creation of
// one work action, which then reads as missing.
type repositoryMonitorFeedbackMissingActionStore struct {
	store.RepositoryMonitorStore
	id string
}

func (s repositoryMonitorFeedbackMissingActionStore) GetWorkAction(ctx context.Context, namespace, id string) (*store.WorkAction, error) {
	if id == s.id {
		return nil, store.ErrNotFound
	}
	return s.RepositoryMonitorStore.GetWorkAction(ctx, namespace, id)
}

func (s repositoryMonitorFeedbackMissingActionStore) CreateWorkAction(ctx context.Context, action *store.WorkAction) error {
	if action.ID == s.id {
		return nil
	}
	return s.RepositoryMonitorStore.CreateWorkAction(ctx, action)
}

// A projection whose work action is missing, and whose write of it the
// store silently drops, is not marked projected. (Fail-open check of
// f59e9b70e, follow-up.)
func TestRepositoryMonitorReviewFeedbackMissingActionIsNotMarkedProjected(t *testing.T) {
	ctx := context.Background()
	l := newRepositoryMonitorFeedbackLifecycle(t)
	l.mustHandoff(t)
	task := l.f.repairTasks(t)[0]
	task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	task.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStateNoChange, Outcome: corev1alpha1.TaskDeliveryOutcomeNoChange, StartingSHA: "head1111"}
	if err := l.f.reconciler.Status().Update(ctx, &task); err != nil {
		t.Fatal(err)
	}
	original := l.f.reconciler.Store
	l.f.reconciler.Store = repositoryMonitorFeedbackMissingActionStore{RepositoryMonitorStore: original,
		id: store.RepositoryMonitorWorkActionID(l.command.ID, store.RepositoryMonitorDesiredActionForIntent(l.command.Intent))}
	_, err := l.f.reconciler.ingestCompletedRepositoryMonitorRepairTasks(ctx, l.f.monitor)
	l.f.reconciler.Store = original
	if !errors.Is(err, errRepositoryMonitorFeedbackProjectionContradicted) {
		t.Fatalf("ingestion err = %v, want the missing action reported", err)
	}
	if outcome := l.outcome(t); outcome.ProjectedAt != nil {
		t.Fatalf("outcome = %+v, want it not projected while its action is missing", outcome)
	}
	if _, err := l.f.reconciler.ingestCompletedRepositoryMonitorRepairTasks(ctx, l.f.monitor); err != nil {
		t.Fatal(err)
	}
	if l.outcome(t).ProjectedAt == nil || l.action(t).Status != repositoryMonitorWorkActionStatusSucceeded {
		t.Fatalf("outcome projected=%t action %s, want both once the action is written", l.outcome(t).ProjectedAt != nil, l.action(t).Status)
	}
}

// repositoryMonitorFeedbackBacklogStore lists a fixed backlog of unprojected
// outcomes, newest first and paged by offset as the store pages them, and
// records which outcomes each pass was given.
type repositoryMonitorFeedbackBacklogStore struct {
	store.RepositoryMonitorStore
	backlog []store.MonitorFeedbackOutcome
	listed  map[string]int
}

func (s *repositoryMonitorFeedbackBacklogStore) ListMonitorFeedbackOutcomes(_ context.Context, filter store.MonitorFeedbackOutcomeFilter) ([]store.MonitorFeedbackOutcome, string, error) {
	offset := 0
	if filter.Cursor != "" {
		offset, _ = strconv.Atoi(filter.Cursor)
	}
	end := min(offset+filter.Limit, len(s.backlog))
	page := s.backlog[offset:end]
	for _, outcome := range page {
		s.listed[outcome.CommandEventID]++
	}
	next := ""
	if end < len(s.backlog) {
		next = strconv.Itoa(end)
	}
	return page, next, nil
}

// A full batch of unprojected outcomes whose projection keeps failing does
// not keep older outcomes from being projected: each pass resumes where the
// last stopped and wraps around. (Fail-open check of f59e9b70e, follow-up.)
func TestRepositoryMonitorReviewFeedbackProjectionBacklogRotates(t *testing.T) {
	f := newRepositoryMonitorFeedbackRepairFixture(t, nil)
	backlog := &repositoryMonitorFeedbackBacklogStore{RepositoryMonitorStore: f.reconciler.Store, listed: map[string]int{}}
	// Outcomes that have not ended project nothing, so the passes only
	// show which outcomes each one reaches.
	for i := range repositoryMonitorFeedbackProjectionBatch + 20 {
		backlog.backlog = append(backlog.backlog, store.MonitorFeedbackOutcome{MonitorNamespace: "default", MonitorName: f.monitor.Name,
			CommandEventID: fmt.Sprintf("feedback-%03d", i), State: store.MonitorFeedbackOutcomeStarted})
	}
	f.reconciler.Store = backlog
	for range 3 {
		if _, err := f.reconciler.settleRepositoryMonitorFeedbackOutcomes(context.Background(), f.monitor); err != nil {
			t.Fatal(err)
		}
	}
	for _, outcome := range backlog.backlog {
		if backlog.listed[outcome.CommandEventID] == 0 {
			t.Fatalf("outcome %s never reached in 3 passes", outcome.CommandEventID)
		}
	}
	if backlog.listed["feedback-000"] != 2 || backlog.listed[fmt.Sprintf("feedback-%03d", repositoryMonitorFeedbackProjectionBatch)] != 1 {
		t.Fatalf("passes did not wrap around: %v", backlog.listed)
	}
}

// Reviewers without authority are looked up by each evaluation, so a waiting
// pull request reserves a lookup per evaluation: with more pull requests
// than the budget serves, each checked by repair admission and then
// readiness, every readiness evaluation still completes within a few polls
// of unchanged data. (Autoreview finding on d1ac73ed6.)
func TestRepositoryMonitorPermissionReservationsCoverEveryEvaluation(t *testing.T) {
	f := newRepositoryMonitorFeedbackRepairFixture(t, nil)
	f.github.mu.Lock()
	f.github.defaultPermission = "read"
	f.github.mu.Unlock()
	writer := func(permission string) bool { return repositoryMonitorPermissionMeets("write", nil, permission) }
	prs := repositoryMonitorPermissionLookupBudget/2 + 1
	readinessComplete := map[int]int{}
	for poll := 1; poll <= 8; poll++ {
		ctx, end := f.reconciler.withRepositoryMonitorPermissionPoll(context.Background(), f.monitor)
		for pr := range prs {
			scoped := withRepositoryMonitorPermissionScope(ctx, fmt.Sprintf("pull_request:%d", pr))
			login := fmt.Sprintf("pr%d-reader", pr)
			for evaluation := range 2 {
				_, err := f.reconciler.repositoryMonitorSchedulingPermission(scoped, f.reconciler.GitHubAPIBaseURL, "orka-agents", "orka", "forge-token", login, writer)
				switch {
				case err == nil && evaluation == 1:
					readinessComplete[pr] = poll
				case err != nil && !errors.Is(err, errRepositoryMonitorPermissionLookupBudget):
					t.Fatal(err)
				}
			}
		}
		end()
	}
	for pr := range prs {
		if readinessComplete[pr] < 5 {
			t.Errorf("pull request %d readiness last completed in poll %d of 8, want it served every few polls", pr, readinessComplete[pr])
		}
	}
}
