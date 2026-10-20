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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func TestRepositoryMonitorNeedsChangesReadinessReflectsRepairEligibility(t *testing.T) {
	for _, tc := range []struct {
		name          string
		repairEnabled bool
		repairer      bool
		repairable    bool
		active        bool
		exhausted     bool
		ciFailed      bool
		conflicted    bool
		want          string
		wantIntent    string
	}{
		{name: "repair disabled", repairer: true, repairable: true, want: repositoryMonitorStatusFailure},
		{name: "missing repairer", repairEnabled: true, repairable: true, want: repositoryMonitorStatusFailure},
		{name: "nonrepairable review", repairEnabled: true, repairer: true, want: repositoryMonitorStatusFailure},
		{name: "repair budget exhausted", repairEnabled: true, repairer: true, repairable: true, exhausted: true, want: repositoryMonitorStatusFailure},
		{name: "active repair action", repairable: true, active: true, want: repositoryMonitorStatusPending},
		{name: "eligible review repair", repairEnabled: true, repairer: true, repairable: true, want: repositoryMonitorStatusPending, wantIntent: repositoryMonitorCommandIntentFix},
		{name: "eligible CI repair", repairEnabled: true, repairer: true, ciFailed: true, want: repositoryMonitorStatusPending, wantIntent: repositoryMonitorCommandIntentFixCI},
		{name: "eligible agentless conflict repair", repairEnabled: true, conflicted: true, want: repositoryMonitorStatusPending, wantIntent: repositoryMonitorCommandIntentUpdateBranch},
		{name: "conflict repair disabled", conflicted: true, want: repositoryMonitorStatusFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupControllerSQLiteStore(t)
			monitor, secret := repositoryMonitorInventoryTestObjects("needs-changes-readiness")
			monitor.Spec.Review.Publish.Enabled = true
			monitor.Spec.Repair.Enabled = tc.repairEnabled
			configureRepositoryMonitorTestWriteCredentials(monitor)
			if tc.repairer {
				monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
			}
			if tc.exhausted {
				zero := int32(0)
				monitor.Spec.Repair.MaxRepairsPerHead = &zero
			}
			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))
			cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(monitor).
				WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
			review := &store.ReviewRecord{
				ID: "current-review", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name,
				Kind: repositoryMonitorPullRequestKind, Number: 1, HeadSHA: repositoryMonitorTestHeadSHA,
				Verdict: repositoryMonitorReviewVerdictNeedsChanges, Repairable: tc.repairable,
				ValidationStatus: repositoryMonitorValidationStatusNotRun,
			}
			require.NoError(t, db.CreateReviewRecord(t.Context(), review))
			require.NoError(t, db.UpsertMonitorItem(t.Context(), &store.MonitorItem{
				MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: review.Kind, ItemKey: "1", Number: 1,
				State: repositoryMonitorItemStateOpen, HeadSHA: review.HeadSHA, BaseBranch: repositoryMonitorTestDefaultBranch,
				LastReviewID: review.ID, LastReviewedHeadSHA: review.HeadSHA, LastVerdict: review.Verdict,
			}))
			if tc.active {
				require.NoError(t, db.CreateWorkAction(t.Context(), &store.WorkAction{
					ID: "active-fix", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name,
					TargetKind: review.Kind, TargetNumber: review.Number, TargetSHA: review.HeadSHA,
					DesiredAction: repositoryMonitorCommandIntentFix, Status: repositoryMonitorWorkActionStatusRunning,
				}))
			}
			const prBody = `{"number":1,"title":"Needs changes","state":"open","draft":false,"mergeable_state":"clean","base":{"ref":"main","sha":"base1","repo":{"full_name":"orka-agents/orka"}},"head":{"ref":"feature","sha":"sha1","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[]}`
			body := prBody
			if tc.conflicted {
				body = strings.Replace(body, `"mergeable_state":"clean"`, `"mergeable_state":"dirty"`, 1)
			}
			server := newRepositoryMonitorSinglePullRequestServerWithBodyAndAuth(t, 1, body, "Bearer forge-token")
			t.Cleanup(server.Close)
			original := server.Config.Handler
			var statuses []repositoryMonitorCommitStatus
			server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls"):
					_, _ = fmt.Fprintf(w, "[%s]", body)
				case strings.HasSuffix(req.URL.Path, "/check-runs"):
					conclusion := "success"
					if tc.ciFailed {
						conclusion = "failure"
					}
					_, _ = fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"id":10,"name":"tests","status":"completed","conclusion":%q}]}`, conclusion)
				case strings.HasSuffix(req.URL.Path, "/status"):
					_ = json.NewEncoder(w).Encode(map[string]any{"state": "success", "total_count": len(statuses), "statuses": statuses})
				case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/statuses/"):
					var status repositoryMonitorCommitStatus
					if err := json.NewDecoder(req.Body).Decode(&status); err != nil {
						t.Error(err)
					}
					status.ID = int64(100 + len(statuses))
					statuses = append(statuses, status)
					_ = json.NewEncoder(w).Encode(status)
				default:
					original.ServeHTTP(w, req)
				}
			})
			r := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: db, GitHubAPIBaseURL: server.URL}
			require.NoError(t, db.CreateMonitorRun(t.Context(), &store.MonitorRun{
				ID: "inventory", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name,
				TargetKind: review.Kind, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute),
			}))
			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: monitor.Namespace, Name: monitor.Name}})
			require.NoError(t, err)
			require.NotEmpty(t, statuses)
			require.Equal(t, tc.want, statuses[len(statuses)-1].State)
			commands, _, err := db.ListCommandEvents(t.Context(), store.CommandEventFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name})
			require.NoError(t, err)
			if tc.wantIntent == "" {
				require.Empty(t, commands)
			} else {
				require.Len(t, commands, 1)
				require.Equal(t, tc.wantIntent, commands[0].Intent)
				require.Equal(t, review.HeadSHA, commands[0].HeadSHA)
			}
			var tasks corev1alpha1.TaskList
			require.NoError(t, cl.List(t.Context(), &tasks))
			require.Empty(t, tasks.Items, "fresh current-head reviews must not dispatch redundant review Tasks")
		})
	}
}
