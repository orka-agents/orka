package controller

import (
	"context"
	"net/http"
	"slices"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestRepositoryMonitorRepairAppliesImplementationPathLimits covers a repair
// whose patch touches a repository control path, one outside the configured
// allowlist, and one inside it. The runtime enforces the limits on the
// workspace delta before publication; a rejection reaches the controller as
// a WorkspaceValidationFailed delivery with nothing pushed.
func TestRepositoryMonitorRepairAppliesImplementationPathLimits(t *testing.T) {
	controlPathRejection := acpWorkspaceValidationFailureMessage(&harnessv2.ClientError{StatusCode: http.StatusConflict, Code: harnessv2.ErrorCodeSessionPoisoned, Message: harnessv2.WorkspaceDeltaRepositoryControlPathMessage})
	disallowedPathRejection := acpWorkspaceValidationFailureMessage(&harnessv2.ClientError{StatusCode: http.StatusConflict, Code: harnessv2.ErrorCodeSessionPoisoned, Message: harnessv2.WorkspaceDeltaDisallowedPathMessage})
	tests := []struct {
		name          string
		rejection     string
		wantPhase     string
		wantError     string
		wantPushedSHA string
	}{
		{name: "repository control path", rejection: controlPathRejection, wantPhase: repositoryMonitorRepairPhaseFailed, wantError: repositoryMonitorPatchPathDenied},
		{name: "outside allowed paths", rejection: disallowedPathRejection, wantPhase: repositoryMonitorRepairPhaseFailed, wantError: repositoryMonitorPatchPathNotAllowed},
		{name: "inside allowed paths", wantPhase: repositoryMonitorRepairPhaseSucceeded, wantPushedSHA: "pushed31"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			monitorStore := setupControllerSQLiteStore(t)
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatalf("AddToScheme() error = %v", err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatalf("corev1 AddToScheme() error = %v", err)
			}
			monitor, _ := repositoryMonitorInventoryTestObjects("repair-path-limits")
			monitor.Spec.Repair.Enabled = true
			monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
			monitor.Spec.IssueWorkflow.Implementation.AllowedPaths = []string{"internal/**", "docs/**"}
			configureRepositoryMonitorTestWriteCredentials(monitor)
			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.Task{}).
				WithObjects(monitor, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: monitor.Namespace, UID: "namespace-uid"}}).
				Build()
			reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}
			command := &store.CommandEvent{ID: "cmd-repair-path-limits", Intent: repositoryMonitorCommandIntentFix, Source: "api"}
			pr := repositoryMonitorPullRequest{Number: 31, HeadSHA: "head31", BaseSHA: "base31", HeadBranch: "feature-fix", HeadRepo: "orka-agents/orka", BaseBranch: "main"}
			item := &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "31", Number: 31, HeadSHA: pr.HeadSHA}
			if created, err := reconciler.createRepositoryMonitorRepairTask(ctx, monitor, &store.MonitorRun{ID: "run-repair-path-limits"}, command, "orka-agents", "orka", pr, item, 1, 1); err != nil || created != 1 {
				t.Fatalf("createRepositoryMonitorRepairTask() = (%d, %v), want created", created, err)
			}

			jobID := "repair-" + repositoryMonitorShortHash(command.ID)
			job, err := monitorStore.GetRepairJob(ctx, monitor.Namespace, jobID)
			if err != nil {
				t.Fatalf("GetRepairJob() error = %v", err)
			}
			var task corev1alpha1.Task
			if err := cl.Get(ctx, types.NamespacedName{Namespace: monitor.Namespace, Name: job.TaskName}, &task); err != nil {
				t.Fatalf("Get repair task error = %v", err)
			}
			workspace := task.Spec.Workspace
			if workspace == nil || !workspace.DenyRepositoryControlPaths || !slices.Equal(workspace.AllowedPaths, []string{"internal/**", "docs/**"}) {
				t.Fatalf("repair workspace = %#v, want implementation path limits", workspace)
			}
			if limits := acpWorkspaceDeltaLimits(&task); !limits.DenyRepositoryControlPaths || !slices.Equal(limits.AllowedPaths, workspace.AllowedPaths) {
				t.Fatalf("runtime delta limits = %#v, want the repair path limits", limits)
			}

			if tt.rejection == "" {
				markRepositoryMonitorTestTaskDelivered(t, ctx, cl, task.Name, pr.HeadBranch, tt.wantPushedSHA)
			} else {
				task.Status.Phase = corev1alpha1.TaskPhaseFailed
				task.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{
					State: corev1alpha1.TaskDeliveryStateDeliveryConflict, Outcome: corev1alpha1.TaskDeliveryOutcomeDeliveryConflict,
					Reason: acpWorkspaceValidationFailedReason, Message: tt.rejection,
				}
				if err := cl.Status().Update(ctx, &task); err != nil {
					t.Fatalf("Status().Update() error = %v", err)
				}
			}
			if _, err := reconciler.ingestCompletedRepositoryMonitorRepairTasks(ctx, monitor); err != nil {
				t.Fatalf("ingestCompletedRepositoryMonitorRepairTasks() error = %v", err)
			}
			job, err = monitorStore.GetRepairJob(ctx, monitor.Namespace, jobID)
			if err != nil {
				t.Fatalf("GetRepairJob(after ingest) error = %v", err)
			}
			if job.Phase != tt.wantPhase || job.LastError != tt.wantError || job.PushedSHA != tt.wantPushedSHA {
				t.Fatalf("repair job = phase %q error %q pushed %q, want phase %q error %q pushed %q", job.Phase, job.LastError, job.PushedSHA, tt.wantPhase, tt.wantError, tt.wantPushedSHA)
			}
			pushMutation, err := monitorStore.GetGitHubMutationRecord(ctx, monitor.Namespace, "ghmut-"+repositoryMonitorShortHash(job.ID+"-push"))
			if err != nil {
				t.Fatalf("GetGitHubMutationRecord() error = %v", err)
			}
			wantStatus := repositoryMonitorRunPhaseSucceeded
			if tt.rejection != "" {
				wantStatus = repositoryMonitorRunPhaseFailed
			}
			if pushMutation.Status != wantStatus || pushMutation.ExternalID != tt.wantPushedSHA || pushMutation.Error != tt.wantError {
				t.Fatalf("push mutation = %#v, want status %q with pushed SHA %q and error %q", pushMutation, wantStatus, tt.wantPushedSHA, tt.wantError)
			}
		})
	}
}

func TestRepositoryMonitorRepairWithoutAllowedPathsStillDeniesControlPaths(t *testing.T) {
	monitor := &corev1alpha1.RepositoryMonitor{}
	workspace := &corev1alpha1.WorkspaceConfig{}
	repositoryMonitorApplyPatchPathLimits(monitor, workspace)
	if !workspace.DenyRepositoryControlPaths || len(workspace.AllowedPaths) != 0 {
		t.Fatalf("workspace = %#v, want control paths denied with no allowlist", workspace)
	}
}

func TestRepositoryMonitorWorkspacePathRejectionReasonIgnoresOtherFailures(t *testing.T) {
	for _, delivery := range []*corev1alpha1.TaskDeliveryStatus{
		nil,
		{Reason: acpWorkspaceValidationFailedReason, Message: acpWorkspaceValidationFailureMessage(&harnessv2.ClientError{Message: "workspace delta exceeds changed-file limit"})},
		{Reason: "PublicationFailed", Message: harnessv2.WorkspaceDeltaRepositoryControlPathMessage},
	} {
		task := &corev1alpha1.Task{Status: corev1alpha1.TaskStatus{Delivery: delivery}}
		if reason := repositoryMonitorWorkspacePathRejectionReason(task); reason != "" {
			t.Fatalf("repositoryMonitorWorkspacePathRejectionReason(%#v) = %q, want none", delivery, reason)
		}
	}
}
