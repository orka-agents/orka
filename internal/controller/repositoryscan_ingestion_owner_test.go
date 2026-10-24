package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/taskmeta"
)

type completedIngestionStore struct {
	store.SecurityStore
	completed []store.ScanTaskIdentity
}

func (s *completedIngestionStore) CompleteScanTaskIngestion(_ context.Context, identity store.ScanTaskIdentity) error {
	s.completed = append(s.completed, identity)
	return nil
}

// A restored scan Task settled on its source execution keeps its diagnostic
// artifacts under the source UID, where the artifact API and the Task
// finalizer address them, while the ingestion receipt keeps the incarnation
// UID it fences.
func TestScanIngestionFollowupStoresDiagnosticsUnderTheTaskDataUID(t *testing.T) {
	ctx := context.Background()
	artifacts := setupControllerSQLiteStore(t)
	securityStore := &completedIngestionStore{}
	reconciler := &RepositoryScanReconciler{SecurityStore: securityStore, ArtifactStore: artifacts}
	scan := &corev1alpha1.RepositoryScan{ObjectMeta: metav1.ObjectMeta{Name: "scan", Namespace: defaultNS}}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "scan-review", Namespace: defaultNS, UID: "restored-uid"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded,
			AgentExecutionBinding: &corev1alpha1.AgentExecutionBinding{
				ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV2,
				Task:            corev1alpha1.AgentExecutionBindingTaskRef{UID: "source-uid"},
			},
			Execution: &corev1alpha1.TaskExecutionStatus{
				State:   corev1alpha1.TaskExecutionStateSucceeded,
				Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
				Reason:  taskmeta.RestoreIdentityChangedReason,
			},
		},
	}
	ingestion := &store.ScanTaskIngestion{
		ScanTaskIdentity:    store.ScanTaskIdentity{Namespace: defaultNS, RepositoryScan: scan.Name, ScanRunID: "run", TaskName: task.Name, TaskUID: string(task.UID)},
		DroppedFindingsJSON: `{"dropped":[]}`,
	}

	require.NoError(t, reconciler.finishScanTaskIngestion(ctx, scan, task, ingestion))

	source := store.ArtifactOwner{Namespace: defaultNS, TaskName: task.Name, TaskUID: "source-uid"}
	data, _, err := artifacts.GetArtifact(ctx, store.ArtifactKey{Owner: source, Filename: security.ArtifactDroppedFindings})
	require.NoError(t, err)
	require.JSONEq(t, ingestion.DroppedFindingsJSON, string(data))
	incarnation := store.ArtifactOwner{Namespace: defaultNS, TaskName: task.Name, TaskUID: "restored-uid"}
	_, _, err = artifacts.GetArtifact(ctx, store.ArtifactKey{Owner: incarnation, Filename: security.ArtifactDroppedFindings})
	require.ErrorIs(t, err, store.ErrNotFound)
	require.Equal(t, []store.ScanTaskIdentity{ingestion.ScanTaskIdentity}, securityStore.completed,
		"the receipt keeps the incarnation UID it fences")
}
