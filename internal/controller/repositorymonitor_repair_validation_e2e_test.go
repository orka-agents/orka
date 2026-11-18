/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// TestRepositoryMonitorRepairValidationFakeGitHubE2E drives the full monitor
// reconcile loop against a fake GitHub for a monitor with
// spec.validation.repairCommand: a failing CI check starts an automatic
// repair, and the dispatcher's pre-push gate outcome (simulated on the repair
// Task) decides whether the repair is pushed, retried, or failed.
//
//nolint:gocyclo // One fixture exercises each scenario end to end.
func TestRepositoryMonitorRepairValidationFakeGitHubE2E(t *testing.T) {
	const (
		failed      = repositoryMonitorRepairValidationFailedDeliveryReason
		unavailable = repositoryMonitorRepairValidationUnavailableDeliveryReason
		passed      = ""
	)
	tests := []struct {
		name       string
		maxRetries *int32
		// outcomes are the gate results of consecutive repair attempts.
		outcomes  []string
		wantPhase string
		wantError string
		wantTasks int
	}{
		{name: "pass", outcomes: []string{passed}, wantPhase: repositoryMonitorRepairPhaseSucceeded, wantTasks: 1},
		{name: "fail then pass", outcomes: []string{failed, passed}, wantPhase: repositoryMonitorRepairPhaseSucceeded, wantTasks: 2},
		{name: "exhausted", maxRetries: new(int32(1)), outcomes: []string{failed, failed}, wantPhase: repositoryMonitorRepairPhaseFailed, wantError: repositoryMonitorRepairValidationFailedError, wantTasks: 2},
		{name: "unavailable", outcomes: []string{unavailable}, wantPhase: repositoryMonitorRepairPhaseFailed, wantError: repositoryMonitorRepairValidationUnavailableError, wantTasks: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			monitorStore := setupControllerSQLiteStore(t)
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			server := newRepositoryMonitorSinglePullRequestServerWithBodyAndAuth(t, 31, `{"number":31,"title":"Fix me","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base31","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"feature-fix","sha":"head31","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[]}`, "Bearer forge-token")
			original := server.Config.Handler
			server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if strings.HasSuffix(req.URL.Path, "/check-runs") {
					conclusion := "success"
					if strings.Contains(req.URL.Path, "/commits/head31/") {
						conclusion = "failure"
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"id":10,"name":"tests","status":"completed","conclusion":%q}]}`, conclusion)
					return
				}
				if strings.HasSuffix(req.URL.Path, "/status") {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"state":"success","total_count":0,"statuses":[]}`))
					return
				}
				original.ServeHTTP(w, req)
			})
			t.Cleanup(server.Close)

			monitor, secret := repositoryMonitorInventoryTestObjects("pr-validated-fix")
			monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
			monitor.Spec.Repair.Enabled = true
			monitor.Spec.Repair.MaxValidationRetries = tt.maxRetries
			monitor.Spec.Validation.Image = "ghcr.io/orka-agents/validation@sha256:" + strings.Repeat("b", 64)
			monitor.Spec.Validation.RepairCommand = repairValidationTestCommand
			configureRepositoryMonitorTestWriteCredentials(monitor)
			cl := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}, &corev1alpha1.Task{}).
				WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
			reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, GitHubAPIBaseURL: server.URL}
			if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: "run-fix-31", MonitorNamespace: "default", MonitorName: monitor.Name, Trigger: "workflow", TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 31, TargetSHA: "head31", Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
				t.Fatal(err)
			}
			reconcile := func() {
				t.Helper()
				if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
			}
			// The first pass queues the policy command; the second starts it.
			reconcile()
			reconcile()
			jobs, _, err := monitorStore.ListRepairJobs(ctx, store.RepairJobFilter{Namespace: "default", MonitorName: monitor.Name, PRNumber: 31, Limit: 10})
			if err != nil || len(jobs) != 1 || jobs[0].TaskName == "" {
				t.Fatalf("repair jobs = %+v, %v", jobs, err)
			}
			jobID := jobs[0].ID

			for attempt, outcome := range tt.outcomes {
				job, err := monitorStore.GetRepairJob(ctx, "default", jobID)
				if err != nil {
					t.Fatal(err)
				}
				if job.Phase != repositoryMonitorRepairPhaseQueued {
					t.Fatalf("attempt %d: job = %+v, want queued", attempt, job)
				}
				if outcome == passed {
					markRepositoryMonitorTestTaskDelivered(t, ctx, cl, job.TaskName, "feature-fix", "pushed31")
				} else {
					var task corev1alpha1.Task
					if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: job.TaskName}, &task); err != nil {
						t.Fatal(err)
					}
					task.Status.Phase = corev1alpha1.TaskPhaseFailed
					task.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{
						State: corev1alpha1.TaskDeliveryStateDeliveryConflict, Outcome: corev1alpha1.TaskDeliveryOutcomeDeliveryConflict,
						Reason: corev1alpha1.TaskDeliveryReason(outcome), Message: "Validation command exited with status 2.",
					}
					if err := cl.Status().Update(ctx, &task); err != nil {
						t.Fatal(err)
					}
				}
				reconcile()
			}

			job, err := monitorStore.GetRepairJob(ctx, "default", jobID)
			if err != nil {
				t.Fatal(err)
			}
			if job.Phase != tt.wantPhase || job.LastError != tt.wantError {
				t.Fatalf("job = phase %q error %q, want phase %q error %q", job.Phase, job.LastError, tt.wantPhase, tt.wantError)
			}
			wantPushed := ""
			if tt.wantPhase == repositoryMonitorRepairPhaseSucceeded {
				wantPushed = "pushed31"
			}
			if job.PushedSHA != wantPushed {
				t.Fatalf("pushed SHA = %q, want %q", job.PushedSHA, wantPushed)
			}
			var tasks corev1alpha1.TaskList
			if err := cl.List(ctx, &tasks); err != nil {
				t.Fatal(err)
			}
			repairTasks := 0
			for i := range tasks.Items {
				if tasks.Items[i].Annotations[repositoryMonitorIssueAnnotationCommandID] != "" {
					repairTasks++
				}
			}
			// A failed repair does not start another automatic attempt for
			// the same job; any further policy repair gets its own job.
			if repairTasks < tt.wantTasks {
				t.Fatalf("repair tasks = %d, want at least %d", repairTasks, tt.wantTasks)
			}
			retries := 0
			for i := range tasks.Items {
				if tasks.Items[i].Annotations[repositoryMonitorRepairValidationOriginalTaskAnnotation] != "" {
					retries++
				}
			}
			if retries != tt.wantTasks-1 {
				t.Fatalf("validation retry tasks = %d, want %d", retries, tt.wantTasks-1)
			}
		})
	}
}
