package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/workers/common"
)

// unacceptedArtifactStore refuses artifacts, as the storage client does while
// the controller's fence is not open.
type unacceptedArtifactStore struct {
	store.ArtifactStore
	err error
}

func (s unacceptedArtifactStore) SaveArtifact(context.Context, store.SaveArtifactRequest) error {
	return s.err
}

// An implementation whose patch artifacts the store has not accepted yet is
// retried, not recorded as blocked, which would skip the action for good.
func TestRepositoryMonitorIssuePatchArtifactsRetryATemporaryStorageRefusal(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := repositoryMonitorReviewIngestTestMonitor("patch-temporary")
	monitor.Spec.Owner = "sozercan"
	monitor.Spec.Repository = "orka"
	item := &store.MonitorItem{MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: 56, SnapshotDigest: "sha256:test"}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "impl-temporary", Namespace: "default"}}
	record := &store.ActionRecord{ID: "act-temporary", CommandEventID: "cmd-temporary"}
	patch := &common.StructuredResult{
		BaseSHA: "base",
		Diff:    "diff --git a/docs/safe.md b/docs/safe.md\n--- a/docs/safe.md\n+++ b/docs/safe.md\n@@ -1 +1 @@\n-old\n+new\n",
		Files:   []string{"docs/safe.md"},
	}
	refusal := fmt.Errorf("save artifact: %w", store.ErrNotReady)
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore, ArtifactStore: unacceptedArtifactStore{ArtifactStore: monitorStore, err: refusal}}
	reason, err := reconciler.validateAndSaveIssuePatchArtifacts(ctx, monitor, item, record, task, patch)
	if !errors.Is(err, store.ErrNotReady) || reason != "" {
		t.Fatalf("patch artifacts under a temporary refusal = %q, %v; want the error so the action is retried", reason, err)
	}

	reconciler.ArtifactStore = unacceptedArtifactStore{ArtifactStore: monitorStore, err: errors.New("disk full")}
	if reason, err := reconciler.validateAndSaveIssuePatchArtifacts(ctx, monitor, item, record, task, patch); err != nil || reason != "patch_diff_artifact_save_failed" {
		t.Fatalf("patch artifacts under a definite failure = %q, %v; want the blocked reason", reason, err)
	}
}

// A delivered implementation whose summary artifact the store has not
// accepted yet is retried, not recorded as blocked.
func TestRepositoryMonitorDeliveredImplementationRetriesATemporaryStorageRefusal(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := repositoryMonitorReviewIngestTestMonitor("delivery-temporary")
	item := &store.MonitorItem{MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: 57, SnapshotDigest: "sha256:test"}
	record := &store.ActionRecord{ID: "act-delivery", CommandEventID: "cmd-delivery"}
	remoteBefore := "base-sha"
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "impl-delivery", Namespace: "default", UID: "impl-delivery-uid"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			Workspace: &corev1alpha1.WorkspaceConfig{
				Intent: corev1alpha1.WorkspaceIntentWrite, ExpectedRemoteSHA: remoteBefore,
				PublicationGitRepo: "https://github.com/orka-agents/orka.git", PushBranch: "orka/issue-57",
			},
		},
		Status: corev1alpha1.TaskStatus{Delivery: &corev1alpha1.TaskDeliveryStatus{
			State: corev1alpha1.TaskDeliveryStateVerifiedExact, Outcome: corev1alpha1.TaskDeliveryOutcomeVerifiedExact,
			PublicationID:         "publication-impl-delivery",
			PublicationRepository: &corev1alpha1.RepositoryIdentity{Provider: "github", ID: "github.com/orka-agents/orka"},
			Branch:                "orka/issue-57", RemoteBeforeSHA: &remoteBefore,
			ExpectedCommitSHA: "head-sha", VerifiedRemoteSHA: "head-sha",
			ArtifactDigest: "sha256:" + strings.Repeat("a", 64),
		}},
	}
	for name, refusal := range map[string]error{
		"not ready":                        fmt.Errorf("save artifact: %w", store.ErrNotReady),
		"outcome unknown":                  fmt.Errorf("save artifact: %w", store.ErrOutcomeUnknown),
		"aborted after transient failures": fmt.Errorf("save artifact: %w (%w)", storagev1.ErrOperationAborted, store.ErrUnavailable),
	} {
		t.Run(name, func(t *testing.T) {
			reconciler := &RepositoryMonitorReconciler{Store: monitorStore, ArtifactStore: unacceptedArtifactStore{ArtifactStore: monitorStore, err: refusal}}
			phase, mutationTask, reason, err := reconciler.finishIssueImplementation(ctx, monitor, item, record, task)
			if !errors.Is(err, refusal) || phase != "" || mutationTask != "" || reason != "" {
				t.Fatalf("finishIssueImplementation = %q, %q, %q, %v; want the error so the action is retried", phase, mutationTask, reason, err)
			}
		})
	}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore, ArtifactStore: unacceptedArtifactStore{ArtifactStore: monitorStore, err: errors.New("disk full")}}
	phase, _, reason, err := reconciler.finishIssueImplementation(ctx, monitor, item, record, task)
	if err != nil || phase != repositoryMonitorIssuePhaseBlocked || reason != "patch_summary_artifact_save_failed" {
		t.Fatalf("finishIssueImplementation under a definite failure = %q, %q, %v; want blocked", phase, reason, err)
	}
}
