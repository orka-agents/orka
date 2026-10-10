package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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
		for _, edit := range []string{"text", "labels", "run_limit"} {
			for _, projection := range []string{"", "job", "action", "command"} {
				if action == repositoryMonitorIssueActionPlan && projection != "" {
					continue
				}
				t.Run(action+"/"+edit+"/"+projection, func(t *testing.T) {
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
					maxActive := int32(1)
					monitor.Spec.IssueWorkflow.Implementation.MaxActive = &maxActive
					runTarget := int64(42)
					if edit == "run_limit" {
						maxPerRun := int32(1)
						monitor.Spec.Targets.Issues.MaxPerRun = &maxPerRun
						runTarget = 0
					}
					configureRepositoryMonitorTestWriteCredentials(monitor)
					cl := fake.NewClientBuilder().WithScheme(scheme).
						WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}, &corev1alpha1.Task{}).
						WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
					original := repositoryMonitorIssue{Number: 42, Title: "Original title", Body: "Original requirements", State: "open", Labels: []string{"bug"}, UpdatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}
					changed := original
					changed.UpdatedAt = original.UpdatedAt.Add(time.Hour)
					if edit != "labels" {
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
						issueJSON := map[string]any{"number": changed.Number, "title": changed.Title, "body": changed.Body, "state": changed.State, "labels": labels, "updated_at": changed.UpdatedAt}
						var response any = issueJSON
						if edit == "run_limit" {
							response = []any{
								map[string]any{"number": 1, "title": "Earlier eligible issue", "state": "open", "labels": []map[string]string{{"name": "bug"}}},
								map[string]any{"number": 2, "title": "Another eligible issue", "state": "open", "labels": []map[string]string{{"name": "bug"}}},
								issueJSON,
							}
						}
						payload, err := json.Marshal(response)
						if err != nil {
							t.Fatal(err)
						}
						live.Store(payload)
					}
					setLiveIssue(true)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
						expectedPath := "/repos/orka-agents/orka/issues/42"
						if edit == "run_limit" {
							expectedPath = "/repos/orka-agents/orka/issues"
						}
						if req.Method != http.MethodGet || req.URL.Path != expectedPath {
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
					command := &store.CommandEvent{ID: item.LastCommandID, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: item.Number, IssueSnapshotDigest: item.SnapshotDigest, Intent: item.LastCommandIntent, Status: "accepted", CreatedAt: time.Now()}
					if err := db.CreateCommandEvent(t.Context(), command); err != nil {
						t.Fatal(err)
					}
					if err := db.CreateMonitorRun(t.Context(), &store.MonitorRun{ID: repositoryMonitorCommandRunIDFromCommand(command.ID), MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, TargetKind: command.Kind, TargetNumber: command.Number, CommandEventID: command.ID, Phase: repositoryMonitorRunPhaseSucceeded, StartedAt: time.Now().Add(-time.Hour)}); err != nil {
						t.Fatal(err)
					}
					if err := r.recordRepositoryMonitorWorkActionState(t.Context(), monitor, nil, command, item.Kind, item.Number, "", item.SnapshotDigest, action, repositoryMonitorWorkActionStatusRunning, repositoryMonitorIssuePhasePaused, task.Name, ""); err != nil {
						t.Fatal(err)
					}
					if action == repositoryMonitorIssueActionPlan {
						if err := r.recordRepositoryMonitorPrerequisiteImplementState(t.Context(), monitor, nil, command, item, repositoryMonitorWorkActionStatusRunning, repositoryMonitorIssuePhasePlanQueued, task.Name, ""); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := r.recordImplementationJobQueued(t.Context(), monitor, command, item, task.Name, "fix-42", ""); err != nil {
							t.Fatal(err)
						}
						if reason, err := r.issueImplementationBudgetBlockReason(t.Context(), monitor, item, ""); err != nil || reason != repositoryMonitorImplementationActiveBudget {
							t.Fatalf("completed paused job did not hold active budget: reason=%q, err=%v", reason, err)
						}
					}
					for _, phase := range []string{"paused", "unpaused"} {
						if phase == "unpaused" {
							setLiveIssue(false)
						}
						if err := db.CreateMonitorRun(t.Context(), &store.MonitorRun{ID: phase + "-inventory", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, TargetKind: repositoryMonitorIssueKind, TargetNumber: runTarget, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
							t.Fatal(err)
						}
						injected := errors.New("superseded pause settlement unavailable")
						if phase == "unpaused" && projection != "" {
							r.Store = supersededPauseSettlementErrorStore{RepositoryMonitorStore: db, projection: projection, err: injected}
						}
						if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: monitor.Namespace, Name: monitor.Name}}); err != nil {
							t.Fatal(err)
						}
						if phase == "unpaused" && projection != "" {
							failedRun, err := db.GetMonitorRun(t.Context(), monitor.Namespace, phase+"-inventory")
							if err != nil || failedRun.Phase != repositoryMonitorRunPhaseFailed || !strings.Contains(failedRun.Error, injected.Error()) {
								t.Fatalf("settlement error did not fail inventory: %+v, err=%v", failedRun, err)
							}
							retained, err := db.GetMonitorItem(t.Context(), monitor.Namespace, monitor.Name, repositoryMonitorIssueKind, "42")
							if err != nil || retained.WorkflowPhase != repositoryMonitorIssuePhasePaused || retained.SnapshotDigest != item.SnapshotDigest || retained.LastActionID != record.ID || retained.LastActionTaskName != task.Name {
								t.Fatalf("failed settlement dropped the old identity: %+v, err=%v", retained, err)
							}
							r.Store = db
							if err := db.CreateMonitorRun(t.Context(), &store.MonitorRun{ID: "retry-inventory", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, TargetKind: repositoryMonitorIssueKind, TargetNumber: runTarget, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
								t.Fatal(err)
							}
							if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: monitor.Namespace, Name: monitor.Name}}); err != nil {
								t.Fatal(err)
							}
						}
						stored, err := db.GetMonitorItem(t.Context(), monitor.Namespace, monitor.Name, repositoryMonitorIssueKind, "42")
						if err != nil {
							t.Fatal(err)
						}
						if phase == "paused" {
							if stored.WorkflowPhase != repositoryMonitorIssuePhasePaused || stored.SnapshotDigest != item.SnapshotDigest || stored.Title != original.Title || stored.Body != original.Body || !stored.GitHubUpdatedAt.Equal(original.UpdatedAt) || stored.LastActionID != record.ID {
								t.Fatalf("paused result lost its original snapshot: %+v", stored)
							}
							originalCommand, err := db.GetCommandEvent(t.Context(), monitor.Namespace, command.ID)
							if err != nil || originalCommand.Status != "accepted" {
								t.Fatalf("pause settled the retained command early: %+v, err=%v", originalCommand, err)
							}
						} else {
							expectedPhase, expectedSkip := repositoryMonitorIssuePhaseDiscovered, ""
							if edit == "run_limit" {
								expectedPhase, expectedSkip = repositoryMonitorIssuePhaseBlocked, repositoryMonitorSkipReasonOverLimit
							}
							if stored.WorkflowPhase != expectedPhase || stored.SkipReason != expectedSkip || stored.SnapshotDigest != repositoryMonitorIssueContentDigest(changed, repositoryMonitorIssueCommandLabelNames(monitor)...) || stored.LastActionID != "" || stored.LastActionKind != "" || stored.LastActionTaskName != "" || strings.Contains(stored.LabelsJSON, "orka:pause") {
								t.Fatalf("edited issue resumed the old result: %+v", stored)
							}
						}
					}
					if edit == "run_limit" {
						runID := "unpaused-inventory"
						if projection != "" {
							runID = "retry-inventory"
						}
						inventoryRun, err := db.GetMonitorRun(t.Context(), monitor.Namespace, runID)
						if err != nil || inventoryRun.SelectedCount != 1 || inventoryRun.CreatedTaskCount != 0 {
							t.Fatalf("run did not exercise the selection cap: %+v, err=%v", inventoryRun, err)
						}
						earlier, err := db.GetMonitorItem(t.Context(), monitor.Namespace, monitor.Name, repositoryMonitorIssueKind, "1")
						if err != nil || earlier.WorkflowPhase != repositoryMonitorIssuePhaseDiscovered {
							t.Fatalf("earlier eligible issue did not take the selection slot: %+v, err=%v", earlier, err)
						}
					}
					originalCommand, err := db.GetCommandEvent(t.Context(), monitor.Namespace, command.ID)
					if err != nil || originalCommand.Status != repositoryMonitorCommandProcessed || originalCommand.ProcessedAt == nil || originalCommand.Error != repositoryMonitorIssueSnapshotSuperseded {
						t.Fatalf("superseded command stayed active: %+v, err=%v", originalCommand, err)
					}
					actions, _, err := db.ListWorkActions(t.Context(), store.WorkActionFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, CommandEventID: command.ID, Limit: 10})
					if err != nil || len(actions) == 0 {
						t.Fatalf("missing settled actions: %+v, err=%v", actions, err)
					}
					for _, oldAction := range actions {
						if oldAction.Status != repositoryMonitorWorkActionStatusFailed || oldAction.CompletedAt == nil || oldAction.Error != repositoryMonitorIssueSnapshotSuperseded {
							t.Fatalf("superseded action stayed active: %+v", oldAction)
						}
					}
					if action == repositoryMonitorIssueActionImplementation {
						job, err := db.GetImplementationJob(t.Context(), monitor.Namespace, repositoryMonitorImplementationJobID(task.Name))
						if err != nil || repositoryMonitorImplementationJobActive(job.Phase) || job.CompletedAt == nil || job.Error != repositoryMonitorIssueSnapshotSuperseded {
							t.Fatalf("superseded job stayed active: %+v, err=%v", job, err)
						}
						if reason, err := r.issueImplementationBudgetBlockReason(t.Context(), monitor, &store.MonitorItem{Number: 43}, ""); err != nil || reason != "" {
							t.Fatalf("superseded job still consumes maxActive: reason=%q, err=%v", reason, err)
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
}

type supersededPauseSettlementErrorStore struct {
	store.RepositoryMonitorStore
	projection string
	err        error
}

func (s supersededPauseSettlementErrorStore) UpdateImplementationJob(ctx context.Context, job *store.ImplementationJob) error {
	if s.projection == "job" {
		return s.err
	}
	return s.RepositoryMonitorStore.UpdateImplementationJob(ctx, job)
}

func (s supersededPauseSettlementErrorStore) UpdateWorkAction(ctx context.Context, action *store.WorkAction) error {
	if s.projection == "action" {
		return s.err
	}
	return s.RepositoryMonitorStore.UpdateWorkAction(ctx, action)
}

func (s supersededPauseSettlementErrorStore) UpdateCommandEvent(ctx context.Context, command *store.CommandEvent) error {
	if s.projection == "command" {
		return s.err
	}
	return s.RepositoryMonitorStore.UpdateCommandEvent(ctx, command)
}

func TestRepositoryMonitorPauseSupersessionPreservesTerminalStates(t *testing.T) {
	for _, state := range []string{repositoryMonitorWorkActionStatusSucceeded, repositoryMonitorWorkActionStatusFailed, repositoryMonitorWorkActionStatusBlocked, repositoryMonitorWorkActionStatusCancelled, repositoryMonitorIssueSkipStoppedByCommand} {
		t.Run(state, func(t *testing.T) {
			db := setupControllerSQLiteStore(t)
			monitor, _ := repositoryMonitorInventoryTestObjects("terminal-pause")
			r := &RepositoryMonitorReconciler{Store: db}
			completedAt := time.Now().Add(-time.Hour)
			command := &store.CommandEvent{ID: "original-command", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: 42, Intent: repositoryMonitorCommandIntentImplement, Status: repositoryMonitorCommandProcessed, Error: state, ProcessedAt: &completedAt}
			if err := db.CreateCommandEvent(t.Context(), command); err != nil {
				t.Fatal(err)
			}
			status := state
			if state == repositoryMonitorIssueSkipStoppedByCommand {
				status = repositoryMonitorWorkActionStatusCancelled
			}
			action := &store.WorkAction{ID: store.RepositoryMonitorWorkActionID(command.ID, repositoryMonitorCommandIntentImplement), MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, CommandEventID: command.ID, TargetKind: command.Kind, TargetNumber: command.Number, DesiredAction: command.Intent, Status: status, Error: state, TaskName: "completed-action", CompletedAt: &completedAt}
			if err := db.CreateWorkAction(t.Context(), action); err != nil {
				t.Fatal(err)
			}
			planAction := *action
			planAction.ID = store.RepositoryMonitorWorkActionID(command.ID, repositoryMonitorIssueActionPlan)
			planAction.DesiredAction = repositoryMonitorIssueActionPlan
			if err := db.CreateWorkAction(t.Context(), &planAction); err != nil {
				t.Fatal(err)
			}
			job := &store.ImplementationJob{ID: "terminal-job", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, IssueNumber: command.Number, TaskName: action.TaskName, CommandEventID: command.ID, WorkActionID: action.ID, Phase: status, Error: state, CompletedAt: &completedAt}
			if err := db.CreateImplementationJob(t.Context(), job); err != nil {
				t.Fatal(err)
			}
			record := &store.ActionRecord{ID: "terminal-result", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: command.Kind, Number: command.Number, ActionKind: repositoryMonitorIssueActionPlan, TaskName: action.TaskName, CommandEventID: command.ID, SnapshotDigest: "original", PayloadJSON: "{}"}
			if err := db.CreateActionRecord(t.Context(), record); err != nil {
				t.Fatal(err)
			}
			existing := &store.MonitorItem{Kind: command.Kind, Number: command.Number, WorkflowPhase: repositoryMonitorIssuePhasePaused, SnapshotDigest: record.SnapshotDigest, LastActionID: record.ID, LastActionTaskName: record.TaskName}
			if state == repositoryMonitorIssueSkipStoppedByCommand {
				existing.WorkflowPhase = repositoryMonitorIssuePhaseBlocked
				existing.SkipReason = state
			}
			oldJob, err := db.GetImplementationJob(t.Context(), monitor.Namespace, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			oldAction, err := db.GetWorkAction(t.Context(), monitor.Namespace, action.ID)
			if err != nil {
				t.Fatal(err)
			}
			oldPlanAction, err := db.GetWorkAction(t.Context(), monitor.Namespace, planAction.ID)
			if err != nil {
				t.Fatal(err)
			}
			oldCommand, err := db.GetCommandEvent(t.Context(), monitor.Namespace, command.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.settleRepositoryMonitorPausedIssue(t.Context(), monitor, existing, repositoryMonitorIssueSnapshotSuperseded); err != nil {
				t.Fatal(err)
			}
			newJob, err := db.GetImplementationJob(t.Context(), monitor.Namespace, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			newAction, err := db.GetWorkAction(t.Context(), monitor.Namespace, action.ID)
			if err != nil {
				t.Fatal(err)
			}
			newPlanAction, err := db.GetWorkAction(t.Context(), monitor.Namespace, planAction.ID)
			if err != nil {
				t.Fatal(err)
			}
			newCommand, err := db.GetCommandEvent(t.Context(), monitor.Namespace, command.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(oldJob, newJob) || !reflect.DeepEqual(oldAction, newAction) || !reflect.DeepEqual(oldPlanAction, newPlanAction) || !reflect.DeepEqual(oldCommand, newCommand) {
				t.Fatalf("supersession changed terminal %s state: job=%+v, action=%+v, command=%+v", state, newJob, newAction, newCommand)
			}
		})
	}
}
