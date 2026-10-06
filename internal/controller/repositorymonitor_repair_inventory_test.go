package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestRepositoryMonitorReconcileRefreshesRepairInventoryWithoutPublication(t *testing.T) {
	for _, headChanged := range []bool{false, true} {
		t.Run(fmt.Sprintf("head_changed_%t", headChanged), func(t *testing.T) {
			ctx := t.Context()
			db := setupControllerSQLiteStore(t)
			monitor, secret := repositoryMonitorInventoryTestObjects("repair-without-publication")
			monitor.Spec.Repair.Enabled = true
			require.Nil(t, monitor.Spec.Agents.Repairer)
			require.False(t, monitor.Spec.Review.Publish.Enabled)
			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))
			cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(monitor).
				WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
			const listPR = `{"number":31,"title":"Conflicted","state":"open","draft":false,"base":{"ref":"main","sha":"base31","repo":{"full_name":"orka-agents/orka"}},"head":{"ref":"feature","sha":"head31","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[]}`
			var detail map[string]any
			require.NoError(t, json.Unmarshal([]byte(listPR), &detail))
			detail["mergeable_state"] = "dirty"
			if headChanged {
				detail["head"].(map[string]any)["sha"] = "head32"
			}
			detailReads, updates, readinessPosts := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if req.Header.Get("Authorization") != repositoryMonitorTestBearerHeader() {
					t.Errorf("request has unexpected authorization")
				}
				switch {
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls"):
					_, _ = fmt.Fprintf(w, "[%s]", listPR)
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls/31"):
					detailReads++
					_ = json.NewEncoder(w).Encode(detail)
				case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/compare/"):
					_, _ = w.Write([]byte(`{"status":"diverged","files":[]}`))
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/files"):
					_, _ = w.Write([]byte(`[]`))
				case req.Method == http.MethodPut && strings.HasSuffix(req.URL.Path, "/pulls/31/update-branch"):
					var payload map[string]string
					if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					if payload["expected_head_sha"] != "head31" {
						t.Errorf("branch update does not fence the inventoried head: %+v", payload)
					}
					updates++
					w.Header().Set("X-GitHub-Request-Id", "update-31")
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte(`{"message":"Update scheduled"}`))
				case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/statuses/"):
					readinessPosts++
					t.Error("publication-disabled monitor posted readiness")
					http.Error(w, "unexpected status", http.StatusInternalServerError)
				default:
					t.Errorf("unexpected GitHub request: %s %s", req.Method, req.URL.Path)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			}))
			t.Cleanup(server.Close)
			r := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: db, GitHubAPIBaseURL: server.URL, HTTPClient: server.Client()}
			require.NoError(t, db.CreateMonitorRun(ctx, &store.MonitorRun{
				ID: "full-inventory", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name,
				TargetKind: repositoryMonitorPullRequestKind, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute),
			}))
			key := types.NamespacedName{Namespace: monitor.Namespace, Name: monitor.Name}
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			require.NoError(t, err)
			require.Positive(t, detailReads)
			commands, _, err := db.ListCommandEvents(ctx, store.CommandEventFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name})
			require.NoError(t, err)
			if headChanged {
				require.Empty(t, commands, "a refreshed head must wait for a fresh inventory snapshot")
			} else {
				require.Len(t, commands, 1)
				command := commands[0]
				require.Equal(t, "controller_policy", command.Source)
				require.Equal(t, repositoryMonitorCommandIntentUpdateBranch, command.Intent)
				require.Equal(t, "head31", command.HeadSHA)
				require.Equal(t, int64(31), command.Number)
				require.Zero(t, updates, "policy selection must queue its canonical command before mutation")
				_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
				require.NoError(t, err)
				require.Equal(t, 1, updates)
				mutation, err := db.GetGitHubMutationRecord(ctx, monitor.Namespace, repositoryMonitorUpdateBranchMutationID(command.ID))
				require.NoError(t, err)
				require.Equal(t, repositoryMonitorUpdateBranchOperation, mutation.Operation)
				require.Equal(t, command.ID, mutation.CommandEventID)
				require.Equal(t, "head31", mutation.TargetSHA)
				require.Equal(t, int64(31), mutation.TargetNumber)
				require.Equal(t, repositoryMonitorAutomergeStatePending, mutation.Status)
				require.Equal(t, "update-31", mutation.GitHubRequestID)
				action, err := db.GetWorkAction(ctx, monitor.Namespace, store.RepositoryMonitorWorkActionID(command.ID, command.Intent))
				require.NoError(t, err)
				require.Equal(t, "head31", action.TargetSHA)
				require.Equal(t, repositoryMonitorWorkActionStatusRunning, action.Status)
			}
			var tasks corev1alpha1.TaskList
			require.NoError(t, cl.List(ctx, &tasks))
			require.Empty(t, tasks.Items, "controller-owned branch updates must not need an agent Task")
			require.Zero(t, readinessPosts)
		})
	}
}
