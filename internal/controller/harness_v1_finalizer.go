/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/store"
)

type legacyCleanupHarnessV1RuntimeSessionReclaimer interface {
	ReclaimRuntimeSessionsForTask(
		context.Context,
		string,
		string,
		[]harness.RuntimeSession,
	) error
}

// harnessV1TaskDeletionReady atomically re-proves every route-specific v1
// terminal and Session/outbox barrier before removing the attempt aggregates.
// Reclamation is the finalization proof: an active attempt or incomplete
// Session projection retains the Task finalizer for dispatcher recovery.
func (r *TaskReconciler) harnessV1TaskDeletionReady(
	ctx context.Context,
	task *corev1alpha1.Task,
) (bool, error) {
	if !taskManagedByHarnessV1(task) {
		return true, nil
	}
	binding := task.Status.AgentExecutionBinding
	if task.UID == "" || binding == nil {
		return false, fmt.Errorf("harness v1 Task identity and binding are required for deletion")
	}
	if r.HarnessV1Attempts == nil {
		return false, fmt.Errorf("harness v1 attempt store is required for deletion")
	}
	if r.ControllerEpochManager == nil {
		return false, fmt.Errorf("controller epoch manager is required for harness v1 deletion")
	}
	attempts, err := r.HarnessV1Attempts.ListHarnessV1AttemptsByTask(
		ctx, task.Namespace, string(task.UID),
	)
	if err != nil {
		return false, fmt.Errorf("list harness v1 attempts before Task deletion: %w", err)
	}
	cleanupBinding := legacyCleanupBinding(task, corev1alpha1.AgentRuntimeContractHarnessV1)
	if cleanupBinding != nil && len(attempts) == 0 {
		ready, err := r.reclaimLegacyCleanupHarnessV1RuntimeSessionsWithoutAttempt(ctx, task, cleanupBinding)
		if err != nil || !ready {
			return ready, err
		}
	}
	reclamationBindingDigest := binding.BindingDigest
	if cleanupBinding != nil && len(attempts) != 0 {
		// The migration binding is synthetic cleanup authority created after the
		// historical attempt. Its digest intentionally differs from the attempt's
		// immutable execution lineage, so derive the one admissible lineage from
		// the attempts themselves and reject any mixed set.
		reclamationBindingDigest = attempts[0].BindingDigest
		if err := store.ValidateCanonicalDigest(
			"legacy cleanup harness v1 binding digest",
			reclamationBindingDigest,
		); err != nil {
			return false, err
		}
	}
	allTerminal := true
	for i := range attempts {
		attempt := &attempts[i]
		if attempt.Namespace != task.Namespace || attempt.TaskName != task.Name ||
			attempt.TaskUID != string(task.UID) {
			return false, fmt.Errorf("harness v1 attempt identity changed before Task deletion")
		}
		if attempt.BindingDigest != reclamationBindingDigest {
			if cleanupBinding != nil {
				return false, errors.New("legacy cleanup harness v1 attempts span multiple binding lineages")
			}
			return false, errors.New("harness v1 attempt binding changed before Task deletion")
		}
		if !store.IsTerminalHarnessV1AttemptState(attempt.State) {
			allTerminal = false
		}
	}
	if !allTerminal {
		return false, nil
	}
	// A crash may occur after terminal attempt persistence but before ordinary
	// dispatcher settlement retires the legacy runtime_sessions row. Perform
	// the same idempotent settlement used by dispatcher and adjudicated cleanup
	// before the attempt aggregate can be reclaimed.
	runtimeSessions := HarnessV1Dispatcher{Attempts: r.HarnessV1Attempts}
	for i := range attempts {
		if err := runtimeSessions.settleHarnessV1RuntimeSessionRecord(ctx, task, &attempts[i]); err != nil {
			if errors.Is(err, store.ErrNotReady) {
				return false, nil
			}
			return false, fmt.Errorf("settle harness v1 runtime session before Task deletion: %w", err)
		}
	}
	fence, err := r.ControllerEpochManager.CurrentFence(ctx)
	if err != nil {
		return false, err
	}
	_, err = r.HarnessV1Attempts.ReclaimHarnessV1Attempts(ctx, store.ReclaimHarnessV1AttemptsRequest{
		Namespace:       task.Namespace,
		TaskUID:         string(task.UID),
		BindingDigest:   reclamationBindingDigest,
		SessionRequired: task.Spec.SessionRef != nil,
		Fence:           fence,
	})
	if errors.Is(err, store.ErrNotReady) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reclaim harness v1 attempts before Task deletion: %w", err)
	}
	return true, nil
}

// reclaimLegacyCleanupHarnessV1RuntimeSessionsWithoutAttempt closes the
// historical crash window in which the sealed migration inventory found a
// legacy runtime_sessions row but no durable attempt. The encrypted synthetic
// snapshot is the cleanup authority: only its exact LegacyRuntimeSession set
// may be reclaimed, and the store re-proves that set atomically before delete.
func (r *TaskReconciler) reclaimLegacyCleanupHarnessV1RuntimeSessionsWithoutAttempt(
	ctx context.Context,
	task *corev1alpha1.Task,
	binding *corev1alpha1.AgentExecutionBinding,
) (bool, error) {
	runtimes, ok := r.HarnessV1Attempts.(harness.RuntimeSessionStore)
	if !ok {
		return false, errors.New("harness v1 runtime session store is required for legacy cleanup deletion")
	}
	reclaimer, ok := r.HarnessV1Attempts.(legacyCleanupHarnessV1RuntimeSessionReclaimer)
	if !ok {
		return false, errors.New("atomic harness v1 runtime session reclamation is required for legacy cleanup deletion")
	}

	var sessions []harness.RuntimeSession
	cursor := ""
	for {
		page, next, err := runtimes.ListRuntimeSessions(ctx, harness.RuntimeSessionFilter{
			Namespace: task.Namespace, ActiveTask: task.Name, IncludeDeleted: true,
			Limit: 200, Cursor: cursor,
		})
		if err != nil {
			return false, fmt.Errorf("list no-attempt legacy cleanup harness v1 runtime sessions: %w", err)
		}
		sessions = append(sessions, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(sessions) == 0 {
		return true, nil
	}
	if r.AgentExecutionSnapshots == nil {
		return false, errors.New("encrypted snapshot store is required for no-attempt legacy cleanup deletion")
	}
	snapshot, err := r.AgentExecutionSnapshots.GetAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshotKey{
		TaskUID: string(task.UID), Digest: binding.Snapshot.Digest,
	})
	if err != nil {
		return false, fmt.Errorf("load legacy cleanup migration snapshot: %w", err)
	}
	if snapshot == nil || snapshot.TaskUID != string(task.UID) ||
		snapshot.Digest != binding.Snapshot.Digest ||
		snapshot.SchemaVersion != store.AgentExecutionSnapshotSchemaVersion ||
		store.CanonicalAgentExecutionSnapshotDigest(snapshot.Body) != binding.Snapshot.Digest {
		return false, errors.New("legacy cleanup migration snapshot identity or digest changed")
	}
	var body legacyCleanupSnapshotBody
	if err := json.Unmarshal(snapshot.Body, &body); err != nil {
		return false, fmt.Errorf("decode legacy cleanup migration snapshot: %w", err)
	}
	if body.SchemaVersion != store.AgentExecutionSnapshotSchemaVersion ||
		body.MigrationInventory != binding.MigrationInventoryID ||
		body.TaskNamespace != task.Namespace || body.TaskName != task.Name ||
		body.TaskUID != string(task.UID) ||
		body.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV1 ||
		len(body.V2Evidence) != 0 {
		return false, errors.New("legacy cleanup migration snapshot does not match the exact v1 Task binding")
	}

	expected := make([]agentExecutionClassificationEvidenceItem, 0, len(body.V1Evidence))
	for i := range body.V1Evidence {
		if body.V1Evidence[i].Kind == "LegacyRuntimeSession" {
			expected = append(expected, body.V1Evidence[i])
		}
	}
	actual := make([]agentExecutionClassificationEvidenceItem, 0, len(sessions))
	for i := range sessions {
		session := sessions[i]
		if session.Owner.Namespace != task.Namespace || session.Owner.ActiveTask != task.Name {
			return false, errors.New("legacy runtime session inventory escaped the exact Task filter")
		}
		actual = append(actual, classificationEvidenceItem(
			"LegacyRuntimeSession", task.Namespace, string(session.ID), "", "", session,
		))
	}
	sortClassificationEvidence(expected)
	sortClassificationEvidence(actual)
	if !slices.Equal(expected, actual) {
		return false, errors.New("live legacy runtime sessions do not match the encrypted migration inventory")
	}
	if err := reclaimer.ReclaimRuntimeSessionsForTask(ctx, task.Namespace, task.Name, sessions); err != nil {
		return false, fmt.Errorf("reclaim no-attempt legacy cleanup harness v1 runtime sessions: %w", err)
	}
	return true, nil
}
