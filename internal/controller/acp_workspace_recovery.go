package controller

import (
	"context"
	"reflect"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

// failedACPWorkspaceHasNativeCheckpoint checks the exact retained reference,
// not just the durable-session marker. Imported checkpoints keep their original
// provenance, so ownership is proved by the current pool's catalog reference.
// Read errors preserve the source until its existing retention deadline rather
// than treating an unavailable ownership record as proof that no data remains.
func (r *TaskReconciler) failedACPWorkspaceHasNativeCheckpoint(ctx context.Context, workspace *workspacev1alpha1.ExecutionWorkspace) (bool, error) {
	if workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredDeleted {
		return false, nil
	}
	if remaining, bounded := acpWorkspaceMaxLifetimeRemaining(workspace, time.Now()); bounded && remaining <= 0 {
		return false, nil
	}
	adapter := &ACPExecutionWorkspaceAdapterReconciler{Client: r.Client, APIReader: r.APIReader}
	pool, foreign, err := adapter.linkedRuntimePool(ctx, workspace)
	if err != nil {
		return false, err
	}
	if pool == nil || foreign || !pool.DeletionTimestamp.IsZero() || !substrateRuntimePoolSuspendCapable(pool) ||
		pool.Annotations[substrateNativeJournalAnnotation] == "" {
		return false, nil
	}
	pools := &RuntimePoolReconciler{Client: r.Client, APIReader: r.APIReader, ControllerNamespace: r.ControllerNamespace}
	_, record, err := pools.readNativeSubstrateState(ctx, pool)
	if err != nil {
		return false, err
	}
	if record == nil || record.Failure == "" || record.Checkpoint == nil {
		return false, nil
	}
	cm, artifact, err := pools.readSubstrateCheckpointArtifact(ctx, record.Checkpoint.Digest)
	if err != nil {
		return false, err
	}
	return artifact != nil && !artifact.Deleting && cm.DeletionTimestamp.IsZero() &&
		artifact.Owners[substratePoolCheckpointOwner(pool)] && artifact.Namespace == workspace.Namespace &&
		artifact.Atespace == record.Atespace && reflect.DeepEqual(artifact.Checkpoint, *record.Checkpoint) &&
		artifact.ClassBinding == workspace.Spec.ClassBinding && artifact.ProviderBinding == workspace.Spec.ProviderBinding &&
		reflect.DeepEqual(artifact.Runtime, pool.Spec.Runtime), nil
}
