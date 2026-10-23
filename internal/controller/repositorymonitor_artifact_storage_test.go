package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
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
