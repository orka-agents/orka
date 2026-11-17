package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Every way a snapshot can be incomplete maps to one verdict: a transient
// failure is read again, a permanent limit asks for a person.
func TestRepositoryMonitorReviewFeedbackVerdict(t *testing.T) {
	permissions := map[string]string{"writer": "write", "outsider": ""}
	long := strings.Repeat("Please reconsider this part. ", 120)
	bigThreads := func(login string, total int) []repositoryMonitorFeedbackThreadNode {
		nodes := make([]repositoryMonitorFeedbackThreadNode, 0, 8)
		for i := range 8 {
			thread := repositoryMonitorFeedbackTestThread(fmt.Sprintf("T%d", i), 10+i, false,
				repositoryMonitorFeedbackTestComment(100+2*i, login, "User", long, i), repositoryMonitorFeedbackTestComment(101+2*i, login, "User", long, i))
			thread.Root.TotalCount = total
			nodes = append(nodes, thread)
		}
		return nodes
	}
	writerThread := []repositoryMonitorFeedbackThreadNode{repositoryMonitorFeedbackTestThread("T1", 10, false, repositoryMonitorFeedbackTestComment(101, "writer", "User", "Please fix.", 0))}
	manyReviewers := func() []repositoryMonitorFeedbackThreadNode {
		nodes := make([]repositoryMonitorFeedbackThreadNode, 0, repositoryMonitorFeedbackMaxPermissionLookups+1)
		for i := range repositoryMonitorFeedbackMaxPermissionLookups + 1 {
			login := fmt.Sprintf("user%d", i)
			permissions[login] = "write"
			nodes = append(nodes, repositoryMonitorFeedbackTestThread(fmt.Sprintf("U%d", i), 10, false, repositoryMonitorFeedbackTestComment(300+i, login, "User", "Fix it.", i)))
		}
		return nodes
	}()
	incompleteThread := repositoryMonitorFeedbackTestThread("T2", 10, false, repositoryMonitorFeedbackTestComment(201, "outsider", "User", "Nit.", 0))
	incompleteThread.Root.TotalCount = 30
	incompleteAuthorizedThread := repositoryMonitorFeedbackTestThread("T3", 10, false, repositoryMonitorFeedbackTestComment(202, "writer", "User", "Please fix.", 0))
	incompleteAuthorizedThread.Root.TotalCount = 30
	for _, tc := range []struct {
		name    string
		raw     repositoryMonitorRawReviewFeedback
		failing []string
		want    repositoryMonitorFeedbackVerdict
	}{
		{name: "complete", raw: repositoryMonitorRawReviewFeedback{threads: writerThread}, want: repositoryMonitorFeedbackComplete},
		{name: "evidence-only threads left out of the budget", raw: repositoryMonitorRawReviewFeedback{threads: bigThreads("outsider", 2)}, want: repositoryMonitorFeedbackComplete},
		{name: "failed GitHub read", raw: repositoryMonitorRawReviewFeedback{threads: writerThread, availability: repositoryMonitorEvidenceUnavailable, partial: true}, want: repositoryMonitorFeedbackIncompleteRetry},
		{name: "another head", raw: repositoryMonitorRawReviewFeedback{threads: writerThread, headSHA: "moved"}, want: repositoryMonitorFeedbackIncompleteRetry},
		{name: "failed permission lookup", raw: repositoryMonitorRawReviewFeedback{threads: writerThread}, failing: []string{"writer"}, want: repositoryMonitorFeedbackIncompleteRetry},
		{name: "page limit", raw: repositoryMonitorRawReviewFeedback{threads: writerThread, partial: true}, want: repositoryMonitorFeedbackNeedsPerson},
		{name: "page limit and byte budget", raw: repositoryMonitorRawReviewFeedback{threads: bigThreads("writer", 2), partial: true}, want: repositoryMonitorFeedbackNeedsPerson},
		{name: "more reviewers than one snapshot looks up", raw: repositoryMonitorRawReviewFeedback{threads: manyReviewers}, want: repositoryMonitorFeedbackNeedsPerson},
		{name: "thread with omitted replies", raw: repositoryMonitorRawReviewFeedback{threads: []repositoryMonitorFeedbackThreadNode{incompleteThread}}, want: repositoryMonitorFeedbackNeedsPerson},
		{name: "authorized thread with omitted replies", raw: repositoryMonitorRawReviewFeedback{threads: []repositoryMonitorFeedbackThreadNode{incompleteAuthorizedThread}}, want: repositoryMonitorFeedbackNeedsPerson},
		{name: "authorized thread left out of the budget", raw: repositoryMonitorRawReviewFeedback{threads: bigThreads("writer", 2)}, want: repositoryMonitorFeedbackNeedsPerson},
		{name: "thread with omitted replies left out of the budget", raw: repositoryMonitorRawReviewFeedback{threads: bigThreads("outsider", 5)}, want: repositoryMonitorFeedbackNeedsPerson},
	} {
		t.Run(tc.name, func(t *testing.T) {
			permissionOf, _ := repositoryMonitorFeedbackTestPermissions(permissions, tc.failing...)
			snapshot := repositoryMonitorFeedbackTestNormalize(tc.raw, nil, repositoryMonitorReviewFeedbackPolicy{MinPermission: "write"}, permissionOf)
			verdict, description := snapshot.verdict()
			if verdict != tc.want {
				t.Fatalf("verdict = %q (%s), want %q", verdict, description, tc.want)
			}
			if (verdict == repositoryMonitorFeedbackComplete) != (description == "") {
				t.Fatalf("description %q does not match verdict %q", description, verdict)
			}
		})
	}
}

// A poll looks each reviewer's permission up once across every pull request
// and readiness check, and stops at its lookup budget. A lookup the budget
// refuses, like a throttled one, leaves the snapshot to be read again rather
// than asking for a person.
func TestRepositoryMonitorReviewFeedbackPermissionLookupsPerPoll(t *testing.T) {
	f := newRepositoryMonitorFeedbackRepairFixture(t, nil)
	pr := repositoryMonitorFeedbackRepairTestPR()
	permissionRequests := func() int {
		count := 0
		for _, request := range f.github.requests {
			if strings.HasPrefix(request, "permission:") {
				count++
			}
		}
		return count
	}
	ctx := withRepositoryMonitorPermissionCache(context.Background())
	for range 3 {
		snapshot, err := f.reconciler.buildRepositoryMonitorReviewFeedback(ctx, f.monitor, pr, repositoryMonitorReviewFeedbackPolicyFor(f.monitor.Spec))
		if err != nil {
			t.Fatal(err)
		}
		if verdict, _ := snapshot.verdict(); verdict != repositoryMonitorFeedbackComplete {
			t.Fatalf("verdict = %q, want complete", verdict)
		}
		if _, _, _, err := f.reconciler.repositoryMonitorFeedbackReadiness(ctx, f.monitor, pr); err != nil {
			t.Fatal(err)
		}
	}
	// writer, outsider, and maintainer, each once.
	if got := permissionRequests(); got != 3 {
		t.Fatalf("permission requests = %d (%v), want one per reviewer", got, f.github.requests)
	}

	// A new poll whose budget is spent makes no request and reads again.
	spent := withRepositoryMonitorPermissionCache(context.Background())
	spent.Value(repositoryMonitorPermissionCacheKey{}).(*repositoryMonitorPermissionCache).lookups = repositoryMonitorPermissionLookupBudget
	before := permissionRequests()
	snapshot, err := f.reconciler.buildRepositoryMonitorReviewFeedback(spent, f.monitor, pr, repositoryMonitorReviewFeedbackPolicyFor(f.monitor.Spec))
	if err != nil {
		t.Fatal(err)
	}
	if verdict, _ := snapshot.verdict(); verdict != repositoryMonitorFeedbackIncompleteRetry || permissionRequests() != before {
		t.Fatalf("verdict = %q requests %d→%d, want a retry without requests", verdict, before, permissionRequests())
	}
	if _, err := f.reconciler.repositoryMonitorCollaboratorPermission(spent, "https://api.github.com", "orka-agents", "orka", "token", "someone"); !errors.Is(err, errRepositoryMonitorPermissionLookupBudget) {
		t.Fatalf("lookup past the budget err=%v", err)
	}
	// A throttled lookup is cached for the poll, so it is not retried in
	// it, and the snapshot is read again later.
	f.github.failPermission = map[string]int{"writer": 429}
	throttled := withRepositoryMonitorPermissionCache(context.Background())
	before = permissionRequests()
	for range 2 {
		snapshot, err = f.reconciler.buildRepositoryMonitorReviewFeedback(throttled, f.monitor, pr, repositoryMonitorReviewFeedbackPolicyFor(f.monitor.Spec))
		if err != nil {
			t.Fatal(err)
		}
		if verdict, _ := snapshot.verdict(); verdict != repositoryMonitorFeedbackIncompleteRetry {
			t.Fatalf("throttled verdict = %q, want incomplete_retry", verdict)
		}
	}
	if got := permissionRequests() - before; got != 3 {
		t.Fatalf("throttled poll made %d permission requests, want one per reviewer", got)
	}
}

// A thread page over the GitHub response limit is read again in smaller
// pages, so a too-large page alone is not incomplete. A single thread that is
// still too large cannot be read by reading again, so a person must look.
func TestRepositoryMonitorReviewFeedbackVerdictForTooLargeResponses(t *testing.T) {
	ctx := context.Background()
	pr := repositoryMonitorPullRequest{Number: 1, HeadSHA: repositoryMonitorFeedbackTestHead}
	for _, tc := range []struct {
		name    string
		maxSize int
		want    repositoryMonitorFeedbackVerdict
	}{
		{name: "smaller pages fit", maxSize: 12, want: repositoryMonitorFeedbackComplete},
		{name: "one thread is too large", maxSize: -1, want: repositoryMonitorFeedbackNeedsPerson},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := &repositoryMonitorFeedbackGitHub{t: t, threadPages: []string{"[" + repositoryMonitorFeedbackTestThreadJSON("T1", 11, "writer", "Fix one.") + "]"},
				permissions: map[string]string{"writer": "write"}, maxThreadPageSize: tc.maxSize}
			r, monitor := repositoryMonitorFeedbackTestReconciler(t, github)
			snapshot, err := r.buildRepositoryMonitorReviewFeedback(ctx, monitor, pr, repositoryMonitorDefaultReviewFeedbackPolicy(monitor.Spec))
			if err != nil {
				t.Fatal(err)
			}
			if verdict, description := snapshot.verdict(); verdict != tc.want {
				t.Fatalf("verdict = %q (%s), availability %q reason %q, want %q", verdict, description, snapshot.Availability, snapshot.Reason, tc.want)
			}
		})
	}
}
