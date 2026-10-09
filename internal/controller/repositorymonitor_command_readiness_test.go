package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func TestRepositoryMonitorRecoveredCommandRevokesReadinessBeforeDispatch(t *testing.T) {
	for _, queuedRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing_queued_run_%t", queuedRun), func(t *testing.T) {
			fixture := newRepositoryMonitorCommandReadinessFixture(t)
			if queuedRun {
				fixture.runID = "legacy-review-run"
				fixture.createRun(t, time.Now().Add(-time.Minute))
			}
			// Partial intake leaves only the command. Legacy recovery can also
			// find an already-queued run with no workflow action.
			fixture.reconcile(t)
			actionID := store.RepositoryMonitorWorkActionID(fixture.command.ID, repositoryMonitorCommandIntentReview)
			action, err := fixture.reconciler.Store.GetWorkAction(t.Context(), fixture.monitor.Namespace, actionID)
			require.NoError(t, err)
			require.Contains(t, []string{repositoryMonitorWorkActionStatusQueued, repositoryMonitorWorkActionStatusRunning}, action.Status)
			require.Equal(t, fixture.command.ID, action.CommandEventID)
			require.Equal(t, fixture.runID, action.RunID)
			require.Equal(t, fixture.command.HeadSHA, action.TargetSHA)
			require.Equal(t, fixture.command.Number, action.TargetNumber)
			require.Equal(t, fixture.command.Kind, action.TargetKind)
			fixture.reconcile(t)
			action, err = fixture.reconciler.Store.GetWorkAction(t.Context(), fixture.monitor.Namespace, actionID)
			require.NoError(t, err)
			require.Equal(t, repositoryMonitorWorkActionStatusRunning, action.Status)
			var task corev1alpha1.Task
			require.NoError(t, fixture.reconciler.Get(t.Context(), types.NamespacedName{Namespace: fixture.monitor.Namespace, Name: action.TaskName}, &task))
			require.NotNil(t, task.Spec.Workspace)
			require.Equal(t, fixture.command.HeadSHA, task.Spec.Workspace.Ref)
			require.NotEmpty(t, fixture.statuses)
			for _, status := range fixture.statuses {
				require.Equal(t, repositoryMonitorStatusPending, status.State, "accepted work must revoke old passed review evidence before dispatch")
			}
			require.NotEmpty(t, fixture.actionStatuses)
			for _, status := range fixture.actionStatuses {
				require.Equal(t, repositoryMonitorWorkActionStatusQueued, status, "the exact-head action must be durable before readiness publication")
			}
		})
	}
}

func TestRepositoryMonitorCommandRecoveryPreservesExistingActions(t *testing.T) {
	for _, status := range []string{repositoryMonitorWorkActionStatusRunning, repositoryMonitorWorkActionStatusSucceeded, repositoryMonitorWorkActionStatusFailed, repositoryMonitorWorkActionStatusBlocked, repositoryMonitorWorkActionStatusCancelled} {
		t.Run(status, func(t *testing.T) {
			fixture := newRepositoryMonitorCommandReadinessFixture(t)
			fixture.createRun(t, time.Now().Add(time.Hour))
			action := &store.WorkAction{
				ID:               store.RepositoryMonitorWorkActionID(fixture.command.ID, repositoryMonitorCommandIntentReview),
				MonitorNamespace: fixture.monitor.Namespace,
				MonitorName:      fixture.monitor.Name,
				RunID:            repositoryMonitorCommandRunIDFromCommand(fixture.command.ID),
				CommandEventID:   fixture.command.ID,
				TargetKind:       fixture.command.Kind,
				TargetNumber:     fixture.command.Number,
				TargetSHA:        fixture.command.HeadSHA,
				DesiredAction:    repositoryMonitorCommandIntentReview,
				Status:           status,
				Phase:            "existing-" + status,
				TaskName:         "existing-review-task",
			}
			require.NoError(t, fixture.reconciler.Store.CreateWorkAction(t.Context(), action))
			before, err := fixture.reconciler.Store.GetWorkAction(t.Context(), fixture.monitor.Namespace, action.ID)
			require.NoError(t, err)
			fixture.reconcile(t)
			after, err := fixture.reconciler.Store.GetWorkAction(t.Context(), fixture.monitor.Namespace, action.ID)
			require.NoError(t, err)
			require.Equal(t, before, after)
			command, err := fixture.reconciler.Store.GetCommandEvent(t.Context(), fixture.monitor.Namespace, fixture.command.ID)
			require.NoError(t, err)
			if status == repositoryMonitorWorkActionStatusRunning {
				require.Equal(t, repositoryMonitorCommandAccepted, command.Status)
			} else {
				require.Equal(t, repositoryMonitorCommandProcessed, command.Status)
			}
		})
	}
}

func TestRepositoryMonitorCommandRecoveryStopsWhenWorkActionCannotPersist(t *testing.T) {
	fixture := newRepositoryMonitorCommandReadinessFixture(t)
	fixture.reconciler.Store = failingUpdateBranchProjectionStore{
		RepositoryMonitorStore: fixture.reconciler.Store,
		projection:             "work action",
	}
	_, err := fixture.reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: fixture.monitor.Namespace, Name: fixture.monitor.Name}})
	require.ErrorContains(t, err, "work action projection unavailable")
	require.Empty(t, fixture.statuses)
	var tasks corev1alpha1.TaskList
	require.NoError(t, fixture.reconciler.List(t.Context(), &tasks))
	require.Empty(t, tasks.Items)
	command, err := fixture.reconciler.Store.GetCommandEvent(t.Context(), fixture.monitor.Namespace, fixture.command.ID)
	require.NoError(t, err)
	require.Equal(t, repositoryMonitorCommandAccepted, command.Status)
}

type repositoryMonitorCommandReadinessFixture struct {
	reconciler     *RepositoryMonitorReconciler
	monitor        *corev1alpha1.RepositoryMonitor
	command        *store.CommandEvent
	runID          string
	statuses       []repositoryMonitorCommitStatus
	actionStatuses []string
}

func newRepositoryMonitorCommandReadinessFixture(t *testing.T) *repositoryMonitorCommandReadinessFixture {
	t.Helper()
	monitor, secret := repositoryMonitorInventoryTestObjects("command-readiness")
	monitor.Spec.Review.Publish.Enabled = true
	monitor.Spec.Review.StaleReviewTTL = &metav1.Duration{Duration: time.Hour}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	db := setupControllerSQLiteStore(t)
	fixture := &repositoryMonitorCommandReadinessFixture{
		monitor: monitor,
		reconciler: &RepositoryMonitorReconciler{
			Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(monitor).WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build(),
			Scheme: scheme, Store: db,
		},
		command: &store.CommandEvent{
			ID: "recovered-review", CommentID: "recovered-review", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name,
			Kind: repositoryMonitorPullRequestKind, Number: 1, HeadSHA: repositoryMonitorTestHeadSHA,
			Intent: repositoryMonitorCommandIntentReview, Status: repositoryMonitorCommandAccepted, CreatedAt: time.Now(),
		},
	}
	reviewID := seedRepositoryMonitorAutomergeReview(t, t.Context(), db, monitor.Name, 1, fixture.command.HeadSHA)
	require.NoError(t, db.UpsertMonitorItem(t.Context(), &store.MonitorItem{
		MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: fixture.command.Kind, ItemKey: "1", Number: 1,
		State: repositoryMonitorItemStateOpen, HeadSHA: fixture.command.HeadSHA, BaseBranch: repositoryMonitorTestDefaultBranch,
		LastReviewID: reviewID, LastReviewedHeadSHA: fixture.command.HeadSHA, LastVerdict: repositoryMonitorReviewVerdictPassed,
	}))
	require.NoError(t, db.CreateCommandEvent(t.Context(), fixture.command))
	fixture.runID = repositoryMonitorCommandRunIDFromCommand(fixture.command.ID)
	const prBody = `{"number":1,"title":"Already reviewed","state":"open","draft":false,"mergeable_state":"clean","base":{"ref":"main","sha":"base1","repo":{"full_name":"orka-agents/orka"}},"head":{"ref":"feature","sha":"sha1","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[]}`
	server := newRepositoryMonitorSinglePullRequestServerWithBody(t, 1, prBody)
	t.Cleanup(server.Close)
	original := server.Config.Handler
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls"):
			_, _ = fmt.Fprintf(w, "[%s]", prBody)
		case strings.HasSuffix(req.URL.Path, "/check-runs"):
			_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"id":10,"name":"tests","status":"completed","conclusion":"success"}]}`))
		case strings.HasSuffix(req.URL.Path, "/status"):
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "success", "total_count": len(fixture.statuses), "statuses": fixture.statuses})
		case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/statuses/"):
			var status repositoryMonitorCommitStatus
			if err := json.NewDecoder(req.Body).Decode(&status); err != nil {
				t.Error(err)
			}
			status.ID = int64(100 + len(fixture.statuses))
			fixture.statuses = append(fixture.statuses, status)
			action, err := db.GetWorkAction(req.Context(), monitor.Namespace, store.RepositoryMonitorWorkActionID(fixture.command.ID, fixture.command.Intent))
			if err == nil {
				fixture.actionStatuses = append(fixture.actionStatuses, action.Status)
			} else {
				fixture.actionStatuses = append(fixture.actionStatuses, "missing")
			}
			_ = json.NewEncoder(w).Encode(status)
		default:
			original.ServeHTTP(w, req)
		}
	})
	fixture.reconciler.GitHubAPIBaseURL = server.URL
	return fixture
}

func (f *repositoryMonitorCommandReadinessFixture) createRun(t *testing.T, startedAt time.Time) {
	t.Helper()
	require.NoError(t, f.reconciler.Store.CreateMonitorRun(t.Context(), &store.MonitorRun{
		ID: f.runID, MonitorNamespace: f.monitor.Namespace, MonitorName: f.monitor.Name,
		CommandEventID: f.command.ID, TargetKind: f.command.Kind, TargetNumber: f.command.Number, TargetSHA: f.command.HeadSHA,
		Phase: repositoryMonitorRunPhaseQueued, StartedAt: startedAt,
	}))
}

func (f *repositoryMonitorCommandReadinessFixture) reconcile(t *testing.T) {
	t.Helper()
	_, err := f.reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: f.monitor.Namespace, Name: f.monitor.Name}})
	require.NoError(t, err)
}
