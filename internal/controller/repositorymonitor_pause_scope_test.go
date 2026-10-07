package controller

import (
	"errors"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"k8s.io/apimachinery/pkg/types"
)

func TestRepositoryMonitorPauseScopeRejectionSettlesCompletedImplementation(t *testing.T) {
	for _, policy := range []string{"include", "exclude"} {
		for _, failure := range []string{"", "job", "action", "command", "item"} {
			t.Run(policy+"/"+failure, func(t *testing.T) {
				f := newRepositoryMonitorPausedAuthFixture(t)
				f.reconcile(t)
				original := f.item(t)
				var current corev1alpha1.RepositoryMonitor
				if err := f.r.Get(t.Context(), types.NamespacedName{Namespace: f.monitor.Namespace, Name: f.monitor.Name}, &current); err != nil {
					t.Fatal(err)
				}
				maxActive := int32(1)
				current.Spec.IssueWorkflow.Implementation.MaxActive = &maxActive
				reason := repositoryMonitorSkipReasonExcluded
				if policy == "include" {
					current.Spec.Targets.Issues.IncludeLabels = []string{"approved"}
					reason = repositoryMonitorSkipReasonMissingLabel
				} else {
					current.Spec.Targets.Issues.ExcludeLabels = []string{"bug"}
				}
				if err := f.r.Update(t.Context(), &current); err != nil {
					t.Fatal(err)
				}
				f.monitor = &current
				// Policy changes do not change the issue digest. While pause is
				// present, the completed result and its active budget stay parked.
				f.inventory(t, "policy-while-paused")
				parked := f.item(t)
				if parked.WorkflowPhase != repositoryMonitorIssuePhasePaused || parked.SnapshotDigest != original.SnapshotDigest || parked.LastActionID != original.LastActionID {
					t.Fatal("policy change discarded the paused result")
				}
				if block, err := f.r.issueImplementationBudgetBlockReason(t.Context(), f.monitor, &store.MonitorItem{Number: 43}, ""); err != nil || block != repositoryMonitorImplementationActiveBudget {
					t.Fatalf("paused attempt did not retain active budget: reason=%q err=%v", block, err)
				}
				f.paused.Store(false)
				originalStore := f.r.Store
				if failure == "item" {
					f.r.Store = pausedAuthItemWriteErrorStore{RepositoryMonitorStore: originalStore, phase: repositoryMonitorIssuePhaseBlocked}
				} else if failure != "" {
					f.r.Store = supersededPauseSettlementErrorStore{RepositoryMonitorStore: originalStore, projection: failure, err: errors.New("scope settlement unavailable")}
				}
				f.inventory(t, "scope-rejection")
				if failure != "" {
					retained := f.item(t)
					if retained.WorkflowPhase != repositoryMonitorIssuePhasePaused || retained.SnapshotDigest != original.SnapshotDigest || retained.LastActionID != original.LastActionID || retained.LastActionTaskName != original.LastActionTaskName {
						t.Fatal("failed settlement discarded the paused identity")
					}
					f.assertSnapshot(t, true)
					f.r.Store = originalStore
					f.inventory(t, "scope-rejection-retry")
				}
				assertRepositoryMonitorPausedScopeSettled(t, f, original.SnapshotDigest, reason)
			})
		}
	}
}

func assertRepositoryMonitorPausedScopeSettled(t *testing.T, f *repositoryMonitorPausedAuthFixture, digest, reason string) {
	t.Helper()
	item := f.item(t)
	if item.WorkflowPhase != repositoryMonitorIssuePhaseBlocked || item.SkipReason != reason || item.SnapshotDigest != digest {
		t.Fatalf("unchanged issue was not rejected by policy: phase=%q reason=%q digest=%q", item.WorkflowPhase, item.SkipReason, item.SnapshotDigest)
	}
	commandID := f.task.Annotations[repositoryMonitorIssueAnnotationCommandID]
	command, err := f.r.Store.GetCommandEvent(t.Context(), f.monitor.Namespace, commandID)
	if err != nil || command.Status != repositoryMonitorCommandProcessed || command.ProcessedAt == nil || command.Error != reason {
		t.Fatalf("original command stayed active: %+v err=%v", command, err)
	}
	actionID := store.RepositoryMonitorWorkActionID(commandID, repositoryMonitorCommandIntentImplement)
	action, err := f.r.Store.GetWorkAction(t.Context(), f.monitor.Namespace, actionID)
	if err != nil || action.Status != repositoryMonitorWorkActionStatusFailed || action.CompletedAt == nil || action.Error != reason {
		t.Fatalf("original action stayed active: %+v err=%v", action, err)
	}
	job, err := f.r.Store.GetImplementationJob(t.Context(), f.monitor.Namespace, repositoryMonitorImplementationJobID(f.task.Name))
	if err != nil || repositoryMonitorImplementationJobActive(job.Phase) || job.CompletedAt == nil || job.Error != reason {
		t.Fatalf("original implementation job stayed active: %+v err=%v", job, err)
	}
	if block, err := f.r.issueImplementationBudgetBlockReason(t.Context(), f.monitor, &store.MonitorItem{Number: 43}, ""); err != nil || block != "" {
		t.Fatalf("rejected attempt still consumes active budget: reason=%q err=%v", block, err)
	}
	if f.publications.Load() != 0 {
		t.Fatal("out-of-scope implementation was published")
	}
	f.reconcile(t)
	f.assertSnapshot(t, false)
	var task corev1alpha1.Task
	if err := f.r.Get(t.Context(), types.NamespacedName{Namespace: f.task.Namespace, Name: f.task.Name}, &task); err != nil || task.Labels[labels.LabelCreatedBy] != repositoryMonitorTaskCreatedBy {
		t.Fatalf("settlement lost the completed Task: err=%v", err)
	}
}
