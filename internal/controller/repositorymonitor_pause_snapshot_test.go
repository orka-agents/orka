package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

//nolint:gocyclo // Exercise pause, edited inventory, and unpause through the public reconciler.
func TestRepositoryMonitorPauseRediscoverEditsBeforeReplayingResults(t *testing.T) {
	for _, action := range []string{repositoryMonitorIssueActionPlan, repositoryMonitorIssueActionImplementation} {
		for _, edit := range []string{"text", "labels"} {
			t.Run(action+"/"+edit, func(t *testing.T) {
				db := setupControllerSQLiteStore(t)
				scheme := runtime.NewScheme()
				if err := corev1alpha1.AddToScheme(scheme); err != nil {
					t.Fatal(err)
				}
				if err := corev1.AddToScheme(scheme); err != nil {
					t.Fatal(err)
				}
				monitor, secret := repositoryMonitorInventoryTestObjects("pause-snapshot")
				prEnabled := false
				monitor.Spec.Targets.PullRequests.Enabled = &prEnabled
				monitor.Spec.Targets.Issues.Enabled = true
				monitor.Spec.Agents.Planner = &corev1alpha1.AgentReference{Name: "planner"}
				monitor.Spec.Agents.Implementer = &corev1alpha1.AgentReference{Name: "implementer"}
				configureRepositoryMonitorTestWriteCredentials(monitor)
				cl := fake.NewClientBuilder().WithScheme(scheme).
					WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}, &corev1alpha1.Task{}).
					WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
				original := repositoryMonitorIssue{Number: 42, Title: "Original title", Body: "Original requirements", State: "open", Labels: []string{"bug"}, UpdatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}
				changed := original
				changed.UpdatedAt = original.UpdatedAt.Add(time.Hour)
				if edit == "text" {
					changed.Title = "Edited title"
					changed.Body = "Edited requirements"
				} else {
					changed.Labels = []string{"bug", "priority"}
				}
				var live atomic.Value
				setLiveIssue := func(paused bool) {
					labels := make([]map[string]string, 0, len(changed.Labels)+1)
					for _, label := range changed.Labels {
						labels = append(labels, map[string]string{"name": label})
					}
					if paused {
						labels = append(labels, map[string]string{"name": "orka:pause"})
					}
					payload, err := json.Marshal(map[string]any{"number": changed.Number, "title": changed.Title, "body": changed.Body, "state": changed.State, "labels": labels, "updated_at": changed.UpdatedAt})
					if err != nil {
						t.Fatal(err)
					}
					live.Store(payload)
				}
				setLiveIssue(true)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if req.Method != http.MethodGet || req.URL.Path != "/repos/orka-agents/orka/issues/42" {
						t.Errorf("stale result attempted additional GitHub work: %s %s", req.Method, req.URL.Path)
						w.WriteHeader(http.StatusForbidden)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(live.Load().([]byte))
				}))
				t.Cleanup(server.Close)
				r := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: db, ResultStore: db, ArtifactStore: db, GitHubAPIBaseURL: server.URL}
				item := repositoryMonitorItemFromIssue(monitor, original, nil)
				item.WorkflowPhase = repositoryMonitorIssuePhasePaused
				item.LabelsJSON = `["bug","orka:pause"]`
				item.LastActionKind = action
				item.LastActionTaskName = "completed-action"
				item.LastCommandID = "original-implement"
				item.LastCommandIntent = repositoryMonitorCommandIntentImplement
				task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: item.LastActionTaskName, Namespace: monitor.Namespace, Annotations: map[string]string{repositoryMonitorIssueAnnotationActionKind: action, repositoryMonitorIssueAnnotationCommandID: item.LastCommandID}}, Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: monitor.Spec.Agents.Planner, Workspace: &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentRead, GitRepo: monitor.Spec.RepoURL}}}
				if action == repositoryMonitorIssueActionImplementation {
					task.Spec.AgentRef = monitor.Spec.Agents.Implementer
					task.Spec.Workspace.Intent = corev1alpha1.WorkspaceIntentWrite
					task.Spec.Workspace.PublicationGitRepo = monitor.Spec.RepoURL
					task.Spec.Workspace.PushBranch = "fix-42"
				}
				if err := cl.Create(t.Context(), task); err != nil {
					t.Fatal(err)
				}
				verdict := repositoryMonitorIssueVerdictReady
				schemaVersion := "orka.issuePlan.v1"
				if action == repositoryMonitorIssueActionImplementation {
					markRepositoryMonitorTestTaskDelivered(t, t.Context(), cl, task.Name, "fix-42", "head42")
					verdict = repositoryMonitorIssuePhasePatchReady
					schemaVersion = "orka.issueImplementation.v1"
				} else {
					markRepositoryMonitorTestTaskSucceeded(t, t.Context(), cl, task.Name)
				}
				payload, err := json.Marshal(map[string]any{"schemaVersion": schemaVersion, "issueNumber": item.Number, "snapshotDigest": item.SnapshotDigest, "status": verdict, "summary": "Completed original requirements", "risk": "low", "categories": []string{}})
				if err != nil {
					t.Fatal(err)
				}
				record := &store.ActionRecord{ID: repositoryMonitorIssueActionRecordID(task), MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: item.Number, ActionKind: action, TaskName: task.Name, CommandEventID: item.LastCommandID, SnapshotDigest: item.SnapshotDigest, Verdict: verdict, PayloadJSON: string(payload), CreatedAt: time.Now()}
				item.LastActionID = record.ID
				if err := db.CreateActionRecord(t.Context(), record); err != nil {
					t.Fatal(err)
				}
				if err := db.UpsertMonitorItem(t.Context(), item); err != nil {
					t.Fatal(err)
				}
				command := &store.CommandEvent{ID: item.LastCommandID, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: item.Number, Intent: item.LastCommandIntent, Status: "completed", CreatedAt: time.Now()}
				if err := db.CreateCommandEvent(t.Context(), command); err != nil {
					t.Fatal(err)
				}
				for _, phase := range []string{"paused", "unpaused"} {
					if phase == "unpaused" {
						setLiveIssue(false)
					}
					if err := db.CreateMonitorRun(t.Context(), &store.MonitorRun{ID: phase + "-inventory", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, TargetKind: repositoryMonitorIssueKind, TargetNumber: item.Number, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
						t.Fatal(err)
					}
					if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: monitor.Namespace, Name: monitor.Name}}); err != nil {
						t.Fatal(err)
					}
					stored, err := db.GetMonitorItem(t.Context(), monitor.Namespace, monitor.Name, repositoryMonitorIssueKind, "42")
					if err != nil {
						t.Fatal(err)
					}
					if phase == "paused" {
						if stored.WorkflowPhase != repositoryMonitorIssuePhasePaused || stored.SnapshotDigest != item.SnapshotDigest || stored.Title != original.Title || stored.Body != original.Body || !stored.GitHubUpdatedAt.Equal(original.UpdatedAt) || stored.LastActionID != record.ID {
							t.Fatalf("paused result lost its original snapshot: %+v", stored)
						}
					} else if stored.WorkflowPhase != repositoryMonitorIssuePhaseDiscovered || stored.SnapshotDigest == item.SnapshotDigest || stored.LastActionID != "" || stored.LastActionTaskName != "" {
						t.Fatalf("edited issue resumed the old result: %+v", stored)
					}
				}
				var tasks corev1alpha1.TaskList
				if err := cl.List(t.Context(), &tasks); err != nil || len(tasks.Items) != 1 {
					t.Fatalf("stale result queued new work: tasks=%d, err=%v", len(tasks.Items), err)
				}
			})
		}
	}
}
