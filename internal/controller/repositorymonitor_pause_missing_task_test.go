package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

//nolint:gocyclo // Verify missing retained Tasks and durable settlement retries through Reconcile.
func TestRepositoryMonitorPauseMissingTaskSettlesAttempt(t *testing.T) {
	for _, action := range []string{repositoryMonitorIssueActionPlan, repositoryMonitorIssueActionImplementation} {
		for _, projection := range []string{"", "job", "action", "command", "item"} {
			if action == repositoryMonitorIssueActionPlan && projection == "job" {
				continue
			}
			t.Run(action+"/"+projection, func(t *testing.T) {
				db := setupControllerSQLiteStore(t)
				scheme := runtime.NewScheme()
				if err := corev1alpha1.AddToScheme(scheme); err != nil {
					t.Fatal(err)
				}
				if err := corev1.AddToScheme(scheme); err != nil {
					t.Fatal(err)
				}
				monitor, secret := repositoryMonitorInventoryTestObjects("pause-missing-task")
				prEnabled := false
				monitor.Spec.Targets.PullRequests.Enabled = &prEnabled
				monitor.Spec.Targets.Issues.Enabled = true
				monitor.Spec.Agents.Planner = &corev1alpha1.AgentReference{Name: "planner"}
				monitor.Spec.Agents.Implementer = &corev1alpha1.AgentReference{Name: "implementer"}
				maxActive := int32(1)
				monitor.Spec.IssueWorkflow.Implementation.MaxActive = &maxActive
				configureRepositoryMonitorTestWriteCredentials(monitor)
				cl := fake.NewClientBuilder().WithScheme(scheme).
					WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}, &corev1alpha1.Task{}).
					WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
				issue := repositoryMonitorIssue{Number: 42, Title: "Fix issue", Body: "Keep scope small", State: "open", Labels: []string{"bug"}}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if req.Method != http.MethodGet || req.URL.Path != "/repos/orka-agents/orka/issues/42" {
						t.Errorf("missing Task attempted GitHub publication: %s %s", req.Method, req.URL.Path)
						w.WriteHeader(http.StatusForbidden)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"number": issue.Number, "title": issue.Title, "body": issue.Body, "state": issue.State, "labels": []map[string]string{{"name": "bug"}}})
				}))
				t.Cleanup(server.Close)
				r := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: db, ResultStore: db, ArtifactStore: db, GitHubAPIBaseURL: server.URL}
				item := repositoryMonitorItemFromIssue(monitor, issue, nil)
				item.WorkflowPhase = repositoryMonitorIssuePhasePaused
				item.LabelsJSON = `["bug","orka:pause"]`
				item.LastActionKind = action
				item.LastActionTaskName = "completed-deleted-task"
				item.LastCommandID = "original-implement"
				item.LastCommandIntent = repositoryMonitorCommandIntentImplement
				task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: item.LastActionTaskName, Namespace: monitor.Namespace}}
				if err := cl.Create(t.Context(), task); err != nil {
					t.Fatal(err)
				}
				markRepositoryMonitorTestTaskSucceeded(t, t.Context(), cl, task.Name)
				if err := cl.Delete(t.Context(), task); err != nil {
					t.Fatal(err)
				}
				record := &store.ActionRecord{ID: repositoryMonitorIssueActionRecordID(task), MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: item.Kind, Number: item.Number, ActionKind: action, TaskName: task.Name, CommandEventID: item.LastCommandID, SnapshotDigest: item.SnapshotDigest, Verdict: repositoryMonitorIssueVerdictReady, PayloadJSON: "{}", CreatedAt: time.Now()}
				item.LastActionID = record.ID
				if err := db.CreateActionRecord(t.Context(), record); err != nil {
					t.Fatal(err)
				}
				if err := db.UpsertMonitorItem(t.Context(), item); err != nil {
					t.Fatal(err)
				}
				command := &store.CommandEvent{ID: item.LastCommandID, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: item.Kind, Number: item.Number, IssueSnapshotDigest: item.SnapshotDigest, Intent: item.LastCommandIntent, Status: "accepted", CreatedAt: time.Now()}
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
					if reason, err := r.issueImplementationBudgetBlockReason(t.Context(), monitor, &store.MonitorItem{Number: 43}, ""); err != nil || reason != repositoryMonitorImplementationActiveBudget {
						t.Fatalf("paused job did not consume maxActive: reason=%q, err=%v", reason, err)
					}
				}
				if err := db.CreateMonitorRun(t.Context(), &store.MonitorRun{ID: "unpause-inventory", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, TargetKind: item.Kind, TargetNumber: item.Number, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
					t.Fatal(err)
				}
				injected := errors.New("missing paused Task settlement unavailable")
				if projection != "" {
					r.Store = missingTaskPauseSettlementErrorStore{RepositoryMonitorStore: db, projection: projection, err: injected}
				}
				request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: monitor.Namespace, Name: monitor.Name}}
				if _, err := r.Reconcile(t.Context(), request); err != nil {
					t.Fatal(err)
				}
				if projection != "" {
					failedRun, err := db.GetMonitorRun(t.Context(), monitor.Namespace, "unpause-inventory")
					if err != nil || failedRun.Phase != repositoryMonitorRunPhaseFailed || !strings.Contains(failedRun.Error, injected.Error()) {
						t.Fatalf("settlement failure was not recorded: %+v, err=%v", failedRun, err)
					}
					retained, err := db.GetMonitorItem(t.Context(), monitor.Namespace, monitor.Name, item.Kind, item.ItemKey)
					if err != nil || retained.WorkflowPhase != repositoryMonitorIssuePhasePaused || retained.LastActionID != record.ID || retained.LastActionTaskName != task.Name || retained.SnapshotDigest != item.SnapshotDigest {
						t.Fatalf("failed settlement lost paused identity: %+v, err=%v", retained, err)
					}
					r.Store = db
					if err := db.CreateMonitorRun(t.Context(), &store.MonitorRun{ID: "retry-inventory", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, TargetKind: item.Kind, TargetNumber: item.Number, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
						t.Fatal(err)
					}
					if _, err := r.Reconcile(t.Context(), request); err != nil {
						t.Fatal(err)
					}
				}
				settled, err := db.GetMonitorItem(t.Context(), monitor.Namespace, monitor.Name, item.Kind, item.ItemKey)
				if err != nil || settled.WorkflowPhase != repositoryMonitorIssuePhaseBlocked || settled.SkipReason != repositoryMonitorIssuePausedTaskMissing || settled.LastActionID != record.ID || settled.LastActionTaskName != task.Name || strings.Contains(settled.LabelsJSON, "orka:pause") {
					t.Fatalf("missing Task did not block unverifiable result: %+v, err=%v", settled, err)
				}
				settledCommand, err := db.GetCommandEvent(t.Context(), monitor.Namespace, command.ID)
				if err != nil || settledCommand.Status != repositoryMonitorCommandProcessed || settledCommand.ProcessedAt == nil || settledCommand.Error != repositoryMonitorIssuePausedTaskMissing {
					t.Fatalf("missing Task command stayed active: %+v, err=%v", settledCommand, err)
				}
				actions, _, err := db.ListWorkActions(t.Context(), store.WorkActionFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, CommandEventID: command.ID, Limit: 10})
				if err != nil || len(actions) == 0 {
					t.Fatalf("missing settled actions: %+v, err=%v", actions, err)
				}
				for _, oldAction := range actions {
					if oldAction.Status != repositoryMonitorWorkActionStatusFailed || oldAction.CompletedAt == nil || oldAction.Error != repositoryMonitorIssuePausedTaskMissing {
						t.Fatalf("missing Task action stayed active: %+v", oldAction)
					}
				}
				if action == repositoryMonitorIssueActionImplementation {
					job, err := db.GetImplementationJob(t.Context(), monitor.Namespace, repositoryMonitorImplementationJobID(task.Name))
					if err != nil || repositoryMonitorImplementationJobActive(job.Phase) || job.CompletedAt == nil || job.Error != repositoryMonitorIssuePausedTaskMissing {
						t.Fatalf("missing Task job stayed active: %+v, err=%v", job, err)
					}
					if reason, err := r.issueImplementationBudgetBlockReason(t.Context(), monitor, &store.MonitorItem{Number: 43}, ""); err != nil || reason != "" {
						t.Fatalf("missing Task still consumes maxActive: reason=%q, err=%v", reason, err)
					}
				}
				var tasks corev1alpha1.TaskList
				if err := cl.List(t.Context(), &tasks); err != nil || len(tasks.Items) != 0 {
					t.Fatalf("missing Task result queued new work: %+v, err=%v", tasks.Items, err)
				}
			})
		}
	}
}

type missingTaskPauseSettlementErrorStore struct {
	store.RepositoryMonitorStore
	projection string
	err        error
}

func (s missingTaskPauseSettlementErrorStore) UpdateImplementationJob(ctx context.Context, job *store.ImplementationJob) error {
	if s.projection == "job" {
		return s.err
	}
	return s.RepositoryMonitorStore.UpdateImplementationJob(ctx, job)
}

func (s missingTaskPauseSettlementErrorStore) UpdateWorkAction(ctx context.Context, action *store.WorkAction) error {
	if s.projection == "action" {
		return s.err
	}
	return s.RepositoryMonitorStore.UpdateWorkAction(ctx, action)
}

func (s missingTaskPauseSettlementErrorStore) UpdateCommandEvent(ctx context.Context, command *store.CommandEvent) error {
	if s.projection == "command" {
		return s.err
	}
	return s.RepositoryMonitorStore.UpdateCommandEvent(ctx, command)
}

func (s missingTaskPauseSettlementErrorStore) UpsertMonitorItem(ctx context.Context, item *store.MonitorItem) error {
	if s.projection == "item" {
		return s.err
	}
	return s.RepositoryMonitorStore.UpsertMonitorItem(ctx, item)
}
