package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

const (
	acpLegacyCleanupReason  = corev1alpha1.TaskExecutionReason("LegacyCleanupOnly")
	acpLegacyCleanupMessage = "legacy ACP state was retired without retry or continuation"
)

// reconcileLegacyCleanupACPTasks runs before ordinary recovery and admission.
// It never claims RuntimePool capacity or opens a supervisor prompt.
func (d *ACPDispatcher) reconcileLegacyCleanupACPTasks(
	ctx context.Context,
	tasks []corev1alpha1.Task,
) error {
	for i := range tasks {
		task := &tasks[i]
		if legacyCleanupBinding(task, corev1alpha1.AgentRuntimeContractHarnessV2) == nil {
			continue
		}
		if err := d.reconcileLegacyCleanupACPTask(ctx, task.DeepCopy()); err != nil {
			return fmt.Errorf("reconcile legacy cleanup-only ACP Task %s/%s: %w", task.Namespace, task.Name, err)
		}
	}
	return nil
}

func (d *ACPDispatcher) reconcileLegacyCleanupACPTask(
	ctx context.Context,
	task *corev1alpha1.Task,
) error {
	attempts, err := d.legacyCleanupPromptAttempts(ctx, task)
	if err != nil || len(attempts) == 0 {
		return err
	}
	fence, err := d.Epochs.CurrentFence(ctx)
	if err != nil {
		return err
	}

	for i := range attempts {
		attempt := attempts[i]
		if !store.IsTerminalPromptExecutionState(attempt.ExecutionState) {
			if err := d.settleLegacyCleanupACPAttempt(ctx, task, attempt, fence); err != nil {
				return err
			}
			attempt, err = d.Store.GetPromptAttempt(ctx, attempt.ID)
			if err != nil {
				return err
			}
			attempts[i] = attempt
		}
	}
	if err := d.releaseLegacyCleanupACPReservations(ctx, task); err != nil {
		return err
	}

	latest := attempts[len(attempts)-1]
	if !store.IsTerminalPromptExecutionState(latest.ExecutionState) {
		return fmt.Errorf("legacy cleanup ACP attempt %s did not reach a terminal state", latest.ID)
	}
	task, err = d.ensureLegacyCleanupACPTaskExecution(ctx, task, latest)
	if err != nil {
		return err
	}
	if err := d.projectLegacyCleanupACPAttempt(ctx, task, latest, fence); err != nil {
		return err
	}
	if task.Status.Execution != nil && strings.TrimSpace(task.Status.Execution.RuntimeSessionUID) != "" {
		refreshed := &corev1alpha1.Task{}
		if err := d.Client.Get(ctx, client.ObjectKeyFromObject(task), refreshed); err != nil {
			return err
		}
		task = refreshed
		complete, cleanupErr := d.cleanupRecoveredTaskScopedRuntimeSession(ctx, task)
		if cleanupErr != nil {
			return cleanupErr
		}
		if !complete {
			return nil
		}
	}
	return nil
}

func (d *ACPDispatcher) legacyCleanupPromptAttempts(
	ctx context.Context,
	task *corev1alpha1.Task,
) ([]*store.PromptAttempt, error) {
	ids := make(map[string]struct{})
	list := &corev1alpha1.PromptAttemptList{}
	if err := d.Client.List(ctx, list, client.InNamespace(task.Namespace)); err != nil {
		return nil, fmt.Errorf("list legacy cleanup PromptAttempts: %w", err)
	}
	for i := range list.Items {
		candidate := &list.Items[i]
		if candidate.Spec.TaskUID == string(task.UID) {
			ids[candidate.Spec.ID] = struct{}{}
		}
	}
	if task.Status.Execution != nil && task.Status.Execution.Attempt > 0 &&
		strings.TrimSpace(task.Status.Execution.PromptID) != "" {
		id, err := promptAttemptIDFromTask(task)
		if err != nil {
			return nil, err
		}
		ids[id] = struct{}{}
	}

	attempts := make([]*store.PromptAttempt, 0, len(ids))
	for id := range ids {
		attempt, err := d.Store.GetPromptAttempt(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("read legacy cleanup PromptAttempt %s: %w", id, err)
		}
		if attempt.ID != id || attempt.Key.Namespace != task.Namespace ||
			attempt.Key.TaskUID != string(task.UID) || attempt.Key.Attempt < 1 ||
			strings.TrimSpace(attempt.Key.PromptID) == "" {
			return nil, errors.New("legacy cleanup PromptAttempt identity does not exactly match the Task")
		}
		attempts = append(attempts, attempt)
	}
	sort.Slice(attempts, func(i, j int) bool {
		if attempts[i].Key.Attempt != attempts[j].Key.Attempt {
			return attempts[i].Key.Attempt < attempts[j].Key.Attempt
		}
		return attempts[i].ID < attempts[j].ID
	})
	for i := 1; i < len(attempts); i++ {
		if attempts[i-1].Key.Attempt == attempts[i].Key.Attempt {
			return nil, errors.New("legacy cleanup Task has duplicate PromptAttempt numbers")
		}
	}
	return attempts, nil
}

func (d *ACPDispatcher) settleLegacyCleanupACPAttempt(
	ctx context.Context,
	task *corev1alpha1.Task,
	attempt *store.PromptAttempt,
	fence store.ControllerEpochFence,
) error {
	switch attempt.ExecutionState {
	case store.PromptExecutionQueued, store.PromptExecutionReserved,
		store.PromptExecutionSessionStarting, store.PromptExecutionPlanned:
		target := store.PromptExecutionFailed
		if !task.DeletionTimestamp.IsZero() || task.Status.Phase == corev1alpha1.TaskPhaseCancelled {
			target = store.PromptExecutionCancelled
		}
		digest, err := acpDomainDigest("legacy-cleanup-attempt-transition", map[string]any{
			"id": attempt.ID, "version": attempt.Version, "from": attempt.ExecutionState,
			"to": target, "reason": acpLegacyCleanupReason,
		})
		if err != nil {
			return err
		}
		_, err = d.Store.TransitionPromptAttemptExecution(ctx, store.PromptAttemptExecutionTransition{
			ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState,
			NewState: target, OperationID: "legacy-cleanup-terminal-" + strconv.FormatInt(attempt.Version, 10),
			OperationDigest: digest, TerminalReason: string(acpLegacyCleanupReason), UpdatedAt: time.Now().UTC(),
		})
		return err
	case store.PromptExecutionSubmitting, store.PromptExecutionSubmittedUnknown,
		store.PromptExecutionAccepted, store.PromptExecutionRunning, store.PromptExecutionSettling:
		return d.persistOutcomeUnknown(ctx, attempt.ID, fence, string(acpLegacyCleanupReason), acpLegacyCleanupMessage)
	default:
		return fmt.Errorf("unsupported nonterminal legacy cleanup prompt state %q", attempt.ExecutionState)
	}
}

func (d *ACPDispatcher) releaseLegacyCleanupACPReservations(
	ctx context.Context,
	task *corev1alpha1.Task,
) error {
	pools := &corev1alpha1.RuntimePoolList{}
	if err := d.Client.List(ctx, pools, client.InNamespace(task.Namespace)); err != nil {
		return err
	}
	for i := range pools.Items {
		pool := &pools.Items[i]
		for j := range pool.Status.Capacity.Reservations {
			reservation := pool.Status.Capacity.Reservations[j]
			if reservation.TaskUID != string(task.UID) {
				continue
			}
			if err := d.releaseRuntimePoolReservation(ctx, acpRuntimePoolReservationIdentity{
				PoolKey: types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name},
				PoolUID: types.UID(reservation.PoolUID), TaskUID: task.UID,
				Attempt: reservation.Attempt, ControllerEpoch: reservation.ControllerEpoch,
				RuntimeInstanceID: reservation.RuntimeInstanceID,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *ACPDispatcher) ensureLegacyCleanupACPTaskExecution(
	ctx context.Context,
	task *corev1alpha1.Task,
	attempt *store.PromptAttempt,
) (*corev1alpha1.Task, error) {
	key := types.NamespacedName{Namespace: task.Namespace, Name: task.Name}
	var result *corev1alpha1.Task
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &corev1alpha1.Task{}
		if err := d.Client.Get(ctx, key, latest); err != nil {
			return err
		}
		if latest.UID != task.UID ||
			legacyCleanupBinding(latest, corev1alpha1.AgentRuntimeContractHarnessV2) == nil {
			return errors.New("task cleanup binding changed before ACP terminal settlement")
		}
		if latest.Status.Execution != nil && latest.Status.Execution.Attempt > int32(attempt.Key.Attempt) {
			return errors.New("task execution status is newer than the discovered cleanup attempt")
		}
		if latest.Status.Execution != nil && latest.Status.Execution.Attempt == int32(attempt.Key.Attempt) &&
			latest.Status.Execution.PromptID == attempt.Key.PromptID {
			result = latest
			return nil
		}
		base := latest.DeepCopy()
		now := metav1.Now()
		latest.Status.Execution = &corev1alpha1.TaskExecutionStatus{
			State:   taskExecutionStateForPromptAttempt(attempt.ExecutionState),
			Attempt: int32(attempt.Key.Attempt), PromptID: attempt.Key.PromptID,
			RequestDigest: attempt.RequestDigest, ControllerEpoch: attempt.ControllerEpoch,
			LastTransitionTime: &now,
		}
		if latest.Status.Delivery == nil {
			latest.Status.Delivery = deliveryStatusFromPromptState(attempt.DeliveryState)
		}
		if err := d.Client.Status().Patch(ctx, latest, client.MergeFrom(base)); err != nil {
			return err
		}
		result = latest
		return nil
	})
	return result, err
}

func taskExecutionStateForPromptAttempt(state store.PromptExecutionState) corev1alpha1.TaskExecutionState {
	switch state {
	case store.PromptExecutionQueued:
		return corev1alpha1.TaskExecutionStateQueued
	case store.PromptExecutionReserved, store.PromptExecutionSessionStarting, store.PromptExecutionPlanned:
		return corev1alpha1.TaskExecutionStateReserved
	case store.PromptExecutionSubmitting:
		return corev1alpha1.TaskExecutionStateSubmitting
	case store.PromptExecutionSubmittedUnknown:
		return corev1alpha1.TaskExecutionStateSubmittedUnknown
	case store.PromptExecutionAccepted:
		return corev1alpha1.TaskExecutionStateAccepted
	case store.PromptExecutionRunning:
		return corev1alpha1.TaskExecutionStateRunning
	case store.PromptExecutionSettling:
		return corev1alpha1.TaskExecutionStateSettling
	case store.PromptExecutionSucceeded:
		return corev1alpha1.TaskExecutionStateSucceeded
	case store.PromptExecutionCancelled:
		return corev1alpha1.TaskExecutionStateCancelled
	case store.PromptExecutionOutcomeUnknown:
		return corev1alpha1.TaskExecutionStateOutcomeUnknown
	default:
		return corev1alpha1.TaskExecutionStateFailed
	}
}

func (d *ACPDispatcher) projectLegacyCleanupACPAttempt(
	ctx context.Context,
	task *corev1alpha1.Task,
	attempt *store.PromptAttempt,
	fence store.ControllerEpochFence,
) error {
	bound, err := promptAttemptSessionBound(attempt)
	if err != nil {
		return err
	}
	if task.Spec.SessionRef != nil && bound {
		if err := d.finalizeRecoveredTerminalSession(ctx, task, attempt, fence); err != nil {
			return err
		}
	}
	project := d.failTask
	if task.Spec.SessionRef != nil && !bound {
		project = d.failTaskBeforeSessionBinding
	}
	switch attempt.ExecutionState {
	case store.PromptExecutionFailed:
		return project(ctx, task, corev1alpha1.TaskExecutionStateFailed,
			corev1alpha1.TaskExecutionOutcomeFailed, acpLegacyCleanupReason, acpLegacyCleanupMessage)
	case store.PromptExecutionCancelled:
		return project(ctx, task, corev1alpha1.TaskExecutionStateCancelled,
			corev1alpha1.TaskExecutionOutcomeCancelled, acpLegacyCleanupReason, acpLegacyCleanupMessage)
	case store.PromptExecutionOutcomeUnknown:
		return project(ctx, task, corev1alpha1.TaskExecutionStateOutcomeUnknown,
			corev1alpha1.TaskExecutionOutcomeOutcomeUnknown, acpLegacyCleanupReason, acpLegacyCleanupMessage)
	case store.PromptExecutionSucceeded:
		if !store.IsTerminalPromptDeliveryState(attempt.DeliveryState) {
			return fmt.Errorf("legacy cleanup succeeded attempt has nonterminal delivery state %s", attempt.DeliveryState)
		}
		return d.recoverSucceededTaskProjection(ctx, task, attempt, fence)
	default:
		return fmt.Errorf("legacy cleanup ACP projection requires a terminal attempt, got %s", attempt.ExecutionState)
	}
}
