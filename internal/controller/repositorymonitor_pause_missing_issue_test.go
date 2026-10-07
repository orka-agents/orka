package controller

import (
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

//nolint:gocyclo // Exercise full inventory retirement and each durable settlement retry through Reconcile.
func TestRepositoryMonitorPauseMissingIssueSettlesAttempt(t *testing.T) {
	for _, projection := range []string{"", "job", "action", "command", "item"} {
		t.Run(projection, func(t *testing.T) {
			db := setupControllerSQLiteStore(t)
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			monitor, secret := repositoryMonitorInventoryTestObjects("pause-missing-issue")
			prEnabled := false
			monitor.Spec.Targets.PullRequests.Enabled = &prEnabled
			monitor.Spec.Targets.Issues.Enabled = true
			monitor.Spec.Agents.Implementer = &corev1alpha1.AgentReference{Name: "implementer"}
			maxActive := int32(1)
			monitor.Spec.IssueWorkflow.Implementation.MaxActive = &maxActive
			configureRepositoryMonitorTestWriteCredentials(monitor)
			cl := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}, &corev1alpha1.Task{}).
				WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodGet || req.URL.Path != "/repos/orka-agents/orka/issues" || req.URL.Query().Get("state") != "open" {
					t.Errorf("missing issue attempted additional GitHub work: %s %s", req.Method, req.URL.String())
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
			}))
			t.Cleanup(server.Close)
			r := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: db, ResultStore: db, ArtifactStore: db, GitHubAPIBaseURL: server.URL}
			issue := repositoryMonitorIssue{Number: 42, Title: "Completed issue", Body: "Original requirements", State: "open", Labels: []string{"bug"}}
			item := repositoryMonitorItemFromIssue(monitor, issue, nil)
			item.WorkflowPhase = repositoryMonitorIssuePhasePaused
			item.LabelsJSON = `["bug","orka:pause"]`
			item.LastActionKind = repositoryMonitorIssueActionImplementation
			item.LastActionTaskName = "completed-paused-implementation"
			item.LastCommandID = "original-implement"
			item.LastCommandIntent = repositoryMonitorCommandIntentImplement
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: item.LastActionTaskName, Namespace: monitor.Namespace, Annotations: map[string]string{repositoryMonitorIssueAnnotationActionKind: item.LastActionKind, repositoryMonitorIssueAnnotationCommandID: item.LastCommandID}},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: monitor.Spec.Agents.Implementer, Workspace: &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentWrite, GitRepo: monitor.Spec.RepoURL, PublicationGitRepo: monitor.Spec.RepoURL, PushBranch: "fix-42"}},
			}
			if err := cl.Create(t.Context(), task); err != nil {
				t.Fatal(err)
			}
			markRepositoryMonitorTestTaskDelivered(t, t.Context(), cl, task.Name, "fix-42", "head42")
			record := &store.ActionRecord{ID: repositoryMonitorIssueActionRecordID(task), MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: item.Kind, Number: item.Number, ActionKind: item.LastActionKind, TaskName: task.Name, CommandEventID: item.LastCommandID, SnapshotDigest: item.SnapshotDigest, Verdict: repositoryMonitorIssuePhasePatchReady, PayloadJSON: `{"status":"patch_ready"}`, CreatedAt: time.Now()}
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
			if err := r.recordRepositoryMonitorWorkActionState(t.Context(), monitor, nil, command, item.Kind, item.Number, "", item.SnapshotDigest, item.LastActionKind, repositoryMonitorWorkActionStatusRunning, repositoryMonitorIssuePhasePaused, task.Name, ""); err != nil {
				t.Fatal(err)
			}
			if err := r.recordImplementationJobQueued(t.Context(), monitor, command, item, task.Name, "fix-42", ""); err != nil {
				t.Fatal(err)
			}
			if reason, err := r.issueImplementationBudgetBlockReason(t.Context(), monitor, &store.MonitorItem{Number: 43}, ""); err != nil || reason != repositoryMonitorImplementationActiveBudget {
				t.Fatalf("paused job did not consume maxActive: reason=%q, err=%v", reason, err)
			}
			if err := db.CreateMonitorRun(t.Context(), &store.MonitorRun{ID: "missing-inventory", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("missing paused issue settlement unavailable")
			if projection != "" {
				r.Store = missingTaskPauseSettlementErrorStore{RepositoryMonitorStore: db, projection: projection, err: injected}
			}
			request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: monitor.Namespace, Name: monitor.Name}}
			if _, err := r.Reconcile(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			if projection != "" {
				failedRun, err := db.GetMonitorRun(t.Context(), monitor.Namespace, "missing-inventory")
				if err != nil || failedRun.Phase != repositoryMonitorRunPhaseFailed || !strings.Contains(failedRun.Error, injected.Error()) {
					t.Fatalf("retirement error was not recorded: %+v, err=%v", failedRun, err)
				}
				retained, err := db.GetMonitorItem(t.Context(), monitor.Namespace, monitor.Name, item.Kind, item.ItemKey)
				if err != nil || retained.State != repositoryMonitorItemStateOpen || retained.WorkflowPhase != repositoryMonitorIssuePhasePaused || retained.LastActionID != record.ID || retained.LastActionTaskName != task.Name {
					t.Fatalf("failed retirement lost paused identity: %+v, err=%v", retained, err)
				}
				r.Store = db
				if err := db.CreateMonitorRun(t.Context(), &store.MonitorRun{ID: "retry-inventory", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
					t.Fatal(err)
				}
				if _, err := r.Reconcile(t.Context(), request); err != nil {
					t.Fatal(err)
				}
			}
			settled, err := db.GetMonitorItem(t.Context(), monitor.Namespace, monitor.Name, item.Kind, item.ItemKey)
			if err != nil || settled.State != repositoryMonitorItemStateOutOfScope || settled.WorkflowPhase != repositoryMonitorIssuePhaseBlocked || settled.SkipReason != repositoryMonitorSkipReasonMissing || settled.LastActionID != record.ID || settled.LastActionTaskName != task.Name {
				t.Fatalf("missing issue did not retire: %+v, err=%v", settled, err)
			}
			settledCommand, err := db.GetCommandEvent(t.Context(), monitor.Namespace, command.ID)
			if err != nil || settledCommand.Status != repositoryMonitorCommandProcessed || settledCommand.ProcessedAt == nil || settledCommand.Error != repositoryMonitorSkipReasonMissing {
				t.Fatalf("missing issue command stayed active: %+v, err=%v", settledCommand, err)
			}
			action, err := db.GetWorkAction(t.Context(), monitor.Namespace, store.RepositoryMonitorWorkActionID(command.ID, repositoryMonitorCommandIntentImplement))
			if err != nil || action.Status != repositoryMonitorWorkActionStatusFailed || action.CompletedAt == nil || action.Error != repositoryMonitorSkipReasonMissing {
				t.Fatalf("missing issue action stayed active: %+v, err=%v", action, err)
			}
			job, err := db.GetImplementationJob(t.Context(), monitor.Namespace, repositoryMonitorImplementationJobID(task.Name))
			if err != nil || repositoryMonitorImplementationJobActive(job.Phase) || job.CompletedAt == nil || job.Error != repositoryMonitorSkipReasonMissing {
				t.Fatalf("missing issue job stayed active: %+v, err=%v", job, err)
			}
			if reason, err := r.issueImplementationBudgetBlockReason(t.Context(), monitor, &store.MonitorItem{Number: 43}, ""); err != nil || reason != "" {
				t.Fatalf("missing issue still consumes maxActive: reason=%q, err=%v", reason, err)
			}
			if _, err := r.Reconcile(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			var tasks corev1alpha1.TaskList
			if err := cl.List(t.Context(), &tasks); err != nil || len(tasks.Items) != 1 || tasks.Items[0].Name != task.Name {
				t.Fatalf("retired result created new work: %+v, err=%v", tasks.Items, err)
			}
		})
	}
}
