package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func TestRepositoryMonitorStatusCountsUsesReadinessState(t *testing.T) {
	for _, tc := range []struct {
		name       string
		state      string
		readiness  string
		verdict    string
		skipReason string
		wantOpen   int32
		wantReady  int32
	}{
		{name: "passed review pending CI", state: repositoryMonitorItemStateOpen, readiness: repositoryMonitorAutomergeStatePending, verdict: repositoryMonitorReviewVerdictPassed, wantOpen: 1},
		{name: "passed review blocked readiness", state: repositoryMonitorItemStateOpen, readiness: repositoryMonitorAutomergeStateBlocked, verdict: repositoryMonitorReviewVerdictPassed, wantOpen: 1},
		{name: "passed review without readiness", state: repositoryMonitorItemStateOpen, verdict: repositoryMonitorReviewVerdictPassed, wantOpen: 1},
		{name: "failed review", state: repositoryMonitorItemStateOpen, readiness: repositoryMonitorAutomergeStateBlocked, verdict: repositoryMonitorReviewVerdictFailed, wantOpen: 1},
		{name: "ready", state: repositoryMonitorItemStateOpen, readiness: repositoryMonitorAutomergeStateMergeReady, verdict: repositoryMonitorReviewVerdictPassed, wantOpen: 1, wantReady: 1},
		{name: "ready already reviewed", state: repositoryMonitorItemStateOpen, readiness: repositoryMonitorAutomergeStateMergeReady, verdict: repositoryMonitorReviewVerdictPassed, skipReason: repositoryMonitorSkipReasonReviewed, wantOpen: 1, wantReady: 1},
		{name: "stale already reviewed", state: repositoryMonitorItemStateOpen, readiness: repositoryMonitorAutomergeStatePending, verdict: repositoryMonitorReviewVerdictStale, skipReason: repositoryMonitorSkipReasonReviewed, wantOpen: 1},
		{name: "closed", state: "closed", readiness: repositoryMonitorAutomergeStateMergeReady, verdict: repositoryMonitorReviewVerdictPassed},
		{name: "out of scope", state: repositoryMonitorItemStateOutOfScope, readiness: repositoryMonitorAutomergeStateMergeReady, verdict: repositoryMonitorReviewVerdictPassed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupControllerSQLiteStore(t)
			monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "readiness-counts", Namespace: defaultNS}}
			require.NoError(t, db.UpsertMonitorItem(t.Context(), &store.MonitorItem{
				MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name,
				Kind: repositoryMonitorPullRequestKind, Number: 1, ItemKey: "1", State: tc.state,
				HeadSHA: "head", LastReviewedHeadSHA: "head", LastVerdict: tc.verdict,
				SkipReason: tc.skipReason, AutomergeState: tc.readiness,
			}))
			r := &RepositoryMonitorReconciler{Store: db}
			counts, err := r.repositoryMonitorStatusCounts(t.Context(), monitor)
			require.NoError(t, err)
			require.Equal(t, tc.wantOpen, counts.openPullRequests)
			require.Equal(t, tc.wantReady, counts.mergeReadyItems)
		})
	}
}

func TestRepositoryMonitorStatusCountsIncludesPausedIssues(t *testing.T) {
	for _, tc := range []struct {
		name        string
		state       string
		phase       string
		verdict     string
		wantOpen    int32
		wantBlocked int32
		wantPending int32
	}{
		{name: "paused ready result", state: repositoryMonitorItemStateOpen, phase: repositoryMonitorIssuePhasePaused, verdict: "ready", wantOpen: 1, wantBlocked: 1},
		{name: "paused successful result", state: repositoryMonitorItemStateOpen, phase: repositoryMonitorIssuePhasePaused, verdict: "success", wantOpen: 1, wantBlocked: 1},
		{name: "paused failed result counted once", state: repositoryMonitorItemStateOpen, phase: repositoryMonitorIssuePhasePaused, verdict: repositoryMonitorReviewVerdictFailed, wantOpen: 1, wantBlocked: 1},
		{name: "resumed discovery", state: repositoryMonitorItemStateOpen, phase: repositoryMonitorIssuePhaseDiscovered, verdict: "ready", wantOpen: 1},
		{name: "queued implementation", state: repositoryMonitorItemStateOpen, phase: repositoryMonitorIssuePhaseImplementationQueued, verdict: "ready", wantOpen: 1, wantPending: 1},
		{name: "closed paused result", state: "closed", phase: repositoryMonitorIssuePhasePaused, verdict: "success"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupControllerSQLiteStore(t)
			monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "paused-counts", Namespace: defaultNS}}
			require.NoError(t, db.UpsertMonitorItem(t.Context(), &store.MonitorItem{
				MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name,
				Kind: repositoryMonitorIssueKind, Number: 1, ItemKey: "1", State: tc.state,
				WorkflowPhase: tc.phase, LastVerdict: tc.verdict,
			}))
			r := &RepositoryMonitorReconciler{Store: db}
			counts, err := r.repositoryMonitorStatusCounts(t.Context(), monitor)
			require.NoError(t, err)
			require.Equal(t, tc.wantOpen, counts.openIssues)
			require.Equal(t, tc.wantBlocked, counts.blockedIssues)
			require.Equal(t, tc.wantPending, counts.pendingIssueActions)
		})
	}
}
