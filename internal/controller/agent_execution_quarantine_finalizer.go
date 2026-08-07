/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/store"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// agentExecutionCleanupRoutes is the exact route set authorized by an Applied
// adjudication. It is deliberately not inferred from mutable Task or Agent
// configuration.
type agentExecutionCleanupRoutes struct {
	harnessV1 bool
	harnessV2 bool
}

type adjudicatedHarnessV1RuntimeSessionReclaimer interface {
	ReclaimRuntimeSessionsForTask(
		context.Context,
		string,
		string,
		[]harness.RuntimeSession,
	) error
}

// adjudicatedQuarantineCleanupRoutes verifies the live adjudication referenced
// by a quarantined Task. A subject-side reference is only a durable pointer;
// finalization must consume it only after the referenced record is Applied and
// every immutable identity and digest still matches.
func (r *TaskReconciler) adjudicatedQuarantineCleanupRoutes(
	ctx context.Context,
	task *corev1alpha1.Task,
) (agentExecutionCleanupRoutes, bool, error) {
	if task == nil || task.Status.AgentExecutionQuarantine == nil {
		return agentExecutionCleanupRoutes{}, false, errors.New("quarantined agent Task is required")
	}
	ref := task.Status.AgentExecutionResolutionRef
	if ref == nil {
		return agentExecutionCleanupRoutes{}, false, nil
	}
	if r.APIReader == nil {
		return agentExecutionCleanupRoutes{}, false, errors.New("uncached API reader is required to consume an adjudication")
	}
	if err := validateAgentExecutionResolutionRefDigest(task.Namespace, ref); err != nil {
		return agentExecutionCleanupRoutes{}, false, fmt.Errorf("validate task adjudication resolution reference: %w", err)
	}

	adjudication := &corev1alpha1.AgentExecutionAdjudication{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{
		Namespace: task.Namespace,
		Name:      ref.AdjudicationName,
	}, adjudication); err != nil {
		return agentExecutionCleanupRoutes{}, false, fmt.Errorf("read referenced task adjudication: %w", err)
	}
	if adjudication.Status.State != corev1alpha1.AgentExecutionAdjudicationApplied {
		switch adjudication.Status.State {
		case "", corev1alpha1.AgentExecutionAdjudicationPending,
			corev1alpha1.AgentExecutionAdjudicationApplying:
			// The controller writes the immutable subject reference before its
			// final Applied status update. Preserve every cleanup path across that
			// crash/retry boundary.
			return agentExecutionCleanupRoutes{}, false, nil
		default:
			return agentExecutionCleanupRoutes{}, false, fmt.Errorf(
				"task resolution references terminal non-Applied adjudication state %q",
				adjudication.Status.State,
			)
		}
	}
	if !resolutionRefMatchesAdjudication(ref, adjudication) ||
		adjudication.Status.ResolutionRefDigest != ref.ResolutionDigest {
		return agentExecutionCleanupRoutes{}, false, errors.New(
			"task resolution does not match the exact Applied adjudication",
		)
	}
	if adjudication.Status.OperationID != agentExecutionAdjudicationOperationID(adjudication) ||
		strings.TrimSpace(adjudication.Status.ResultingSubjectResourceVersion) == "" ||
		adjudication.Status.ObservedAt == nil ||
		!adjudication.Status.ObservedAt.Time.Equal(ref.AppliedAt.Time) {
		return agentExecutionCleanupRoutes{}, false, errors.New(
			"applied adjudication status is missing its exact subject-write receipt",
		)
	}
	if err := store.ValidateCanonicalDigest(
		"applied adjudication operation digest",
		adjudication.Status.OperationDigest,
	); err != nil {
		return agentExecutionCleanupRoutes{}, false, err
	}
	if adjudication.Spec.TaskRef.Name != task.Name || adjudication.Spec.TaskRef.UID != task.UID {
		return agentExecutionCleanupRoutes{}, false, errors.New(
			"applied adjudication does not name the exact quarantined task",
		)
	}
	quarantineDigest, err := canonicalAgentExecutionQuarantineDigest(task.Status.AgentExecutionQuarantine)
	if err != nil {
		return agentExecutionCleanupRoutes{}, false, err
	}
	if adjudication.Spec.QuarantineDigest != quarantineDigest {
		return agentExecutionCleanupRoutes{}, false, errors.New(
			"applied adjudication does not match the immutable task quarantine evidence",
		)
	}
	subject := &agentExecutionAdjudicationSubject{
		kind: agentExecutionAdjudicationSubjectTask,
		task: task,
	}
	if reason := validateAgentExecutionCleanupAction(adjudication, subject); reason != "" {
		return agentExecutionCleanupRoutes{}, false, errors.New(reason)
	}

	switch adjudication.Spec.Action {
	case corev1alpha1.AgentExecutionAdjudicationCleanupV1:
		return agentExecutionCleanupRoutes{harnessV1: true}, true, nil
	case corev1alpha1.AgentExecutionAdjudicationCleanupV2:
		return agentExecutionCleanupRoutes{harnessV2: true}, true, nil
	case corev1alpha1.AgentExecutionAdjudicationCleanupBoth:
		return agentExecutionCleanupRoutes{harnessV1: true, harnessV2: true}, true, nil
	default:
		return agentExecutionCleanupRoutes{}, false, fmt.Errorf(
			"applied adjudication action %q is not a supported cleanup action",
			adjudication.Spec.Action,
		)
	}
}

// quarantinedAgentTaskDeletionReady consumes an exact Applied cleanup
// resolution and drives every authorized route-specific finalizer. Common Task
// cleanup and finalizer removal remain the caller's responsibility.
func (r *TaskReconciler) quarantinedAgentTaskDeletionReady(
	ctx context.Context,
	task *corev1alpha1.Task,
) (bool, error) {
	routes, consumed, err := r.adjudicatedQuarantineCleanupRoutes(ctx, task)
	if err != nil || !consumed {
		return false, err
	}
	if routes.harnessV1 {
		ready, err := r.adjudicatedHarnessV1TaskDeletionReady(ctx, task)
		if err != nil || !ready {
			return false, err
		}
	}
	if routes.harnessV2 {
		if r.DurableControlStore == nil {
			return false, errors.New("durable ACP control store is required for adjudicated v2 cleanup")
		}
		ready, err := r.acpTaskDeletionReady(ctx, task)
		if err != nil || !ready {
			return false, err
		}
		reclaimed, err := r.reclaimACPTaskPublicationBundles(ctx, task)
		if err != nil || !reclaimed {
			return false, err
		}
		retired, err := r.retireACPArtifactIdentities(ctx, task)
		if err != nil || !retired {
			return false, err
		}
	}
	return true, nil
}

// adjudicatedHarnessV1TaskDeletionReady derives the frozen v1 binding digest
// from the durable attempts because quarantine and execution bindings are
// intentionally mutually exclusive. Reclamation still re-proves terminal and
// Session/outbox barriers in the attempt-store transaction.
func (r *TaskReconciler) adjudicatedHarnessV1TaskDeletionReady(
	ctx context.Context,
	task *corev1alpha1.Task,
) (bool, error) {
	if task == nil || task.UID == "" {
		return false, errors.New("quarantined harness v1 Task identity is required for deletion")
	}
	if r.HarnessV1Attempts == nil {
		return false, errors.New("harness v1 attempt store is required for adjudicated deletion")
	}
	attempts, err := r.HarnessV1Attempts.ListHarnessV1AttemptsByTask(
		ctx,
		task.Namespace,
		string(task.UID),
	)
	if err != nil {
		return false, fmt.Errorf("list adjudicated harness v1 attempts: %w", err)
	}
	if len(attempts) == 0 {
		return r.reclaimAdjudicatedHarnessV1RuntimeSessionsWithoutAttempt(ctx, task)
	}
	bindingDigest := attempts[0].BindingDigest
	if err := store.ValidateCanonicalDigest("adjudicated harness v1 binding digest", bindingDigest); err != nil {
		return false, err
	}
	for i := range attempts {
		attempt := &attempts[i]
		if attempt.Namespace != task.Namespace || attempt.TaskUID != string(task.UID) ||
			attempt.TaskName != task.Name {
			return false, errors.New("adjudicated harness v1 attempt identity does not match the task")
		}
		if attempt.BindingDigest != bindingDigest {
			return false, errors.New(
				"multiple harness v1 binding lineages require separate terminal settlement before reclamation",
			)
		}
		if !store.IsTerminalHarnessV1AttemptState(attempt.State) {
			return false, nil
		}
	}
	// The ordinary dispatcher performs this step during terminal settlement.
	// Quarantine prevents dispatcher routing, so an Applied cleanup resolution
	// must perform the same idempotent runtime-session retirement before attempt
	// reclamation can make the Task disappear.
	runtimeSessions := HarnessV1Dispatcher{Attempts: r.HarnessV1Attempts}
	for i := range attempts {
		if err := runtimeSessions.settleHarnessV1RuntimeSessionRecord(ctx, task, &attempts[i]); err != nil {
			if errors.Is(err, store.ErrNotReady) {
				return false, nil
			}
			return false, fmt.Errorf("settle adjudicated harness v1 runtime session: %w", err)
		}
	}
	if r.ControllerEpochManager == nil {
		return false, errors.New("controller epoch manager is required for adjudicated harness v1 deletion")
	}
	fence, err := r.ControllerEpochManager.CurrentFence(ctx)
	if err != nil {
		return false, err
	}
	_, err = r.HarnessV1Attempts.ReclaimHarnessV1Attempts(ctx, store.ReclaimHarnessV1AttemptsRequest{
		Namespace:       task.Namespace,
		TaskUID:         string(task.UID),
		BindingDigest:   bindingDigest,
		SessionRequired: task.Spec.SessionRef != nil,
		Fence:           fence,
	})
	if errors.Is(err, store.ErrNotReady) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reclaim adjudicated harness v1 attempts: %w", err)
	}
	return true, nil
}

// reclaimAdjudicatedHarnessV1RuntimeSessionsWithoutAttempt closes the legacy
// crash window in which inventory found a runtime_sessions row but no durable
// HarnessV1Attempt. The immutable quarantine digest is re-proved before the
// SQLite implementation atomically drives every exact row through
// Deleting/Deleted and physically removes it. A retry after commit observes no
// rows and converges without weakening the original evidence record.
func (r *TaskReconciler) reclaimAdjudicatedHarnessV1RuntimeSessionsWithoutAttempt(
	ctx context.Context,
	task *corev1alpha1.Task,
) (bool, error) {
	runtimes, ok := r.HarnessV1Attempts.(harness.RuntimeSessionStore)
	if !ok {
		return false, errors.New("harness v1 runtime session store is required for no-attempt adjudicated deletion")
	}
	reclaimer, ok := r.HarnessV1Attempts.(adjudicatedHarnessV1RuntimeSessionReclaimer)
	if !ok {
		return false, errors.New("atomic harness v1 runtime session reclamation is required for no-attempt adjudicated deletion")
	}

	var sessions []harness.RuntimeSession
	cursor := ""
	for {
		page, next, err := runtimes.ListRuntimeSessions(ctx, harness.RuntimeSessionFilter{
			Namespace: task.Namespace, ActiveTask: task.Name, IncludeDeleted: true,
			Limit: 200, Cursor: cursor,
		})
		if err != nil {
			return false, fmt.Errorf("list no-attempt harness v1 runtime sessions: %w", err)
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

	evidence := make([]agentExecutionClassificationEvidenceItem, 0, len(sessions))
	for i := range sessions {
		session := sessions[i]
		if session.Owner.Namespace != task.Namespace || session.Owner.ActiveTask != task.Name {
			return false, errors.New("legacy runtime session inventory escaped the exact Task filter")
		}
		evidence = append(evidence, classificationEvidenceItem(
			"LegacyRuntimeSession", task.Namespace, string(session.ID), "", "", session,
		))
	}
	sortClassificationEvidence(evidence)
	if task.Status.AgentExecutionQuarantine == nil ||
		evidenceItemsDigest(evidence) != task.Status.AgentExecutionQuarantine.V1EvidenceDigest {
		return false, errors.New("live legacy runtime sessions do not match the immutable v1 quarantine evidence")
	}
	if err := reclaimer.ReclaimRuntimeSessionsForTask(ctx, task.Namespace, task.Name, sessions); err != nil {
		return false, fmt.Errorf("reclaim no-attempt harness v1 runtime sessions: %w", err)
	}
	return true, nil
}
