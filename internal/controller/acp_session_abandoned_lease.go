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
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// SettleAbandonedSessionLease finishes a Session mutation lease whose holder
// Task is already terminal, so Session deletion can proceed once no turn is
// running. Only an exact holder qualifies: the Task the lease names, at the
// leased attempt and prompt, terminal, not executing in this dispatcher, and
// with a PromptAttempt that ended Cancelled, Failed or OutcomeUnknown. Its open
// SessionTurn is finalized by the same recovery settlement a terminal Task's
// turn always gets (which also clears any ADR 0059 resident record); a lease
// whose turn was never persisted is released. Anything else is left for the
// caller's ordinary conflict.
func (d *ACPDispatcher) SettleAbandonedSessionLease(ctx context.Context, namespace, sessionName string) error {
	if d == nil || d.Store == nil || d.Sessions == nil || d.Epochs == nil {
		return nil
	}
	control, err := d.Store.GetSessionControl(ctx, namespace, sessionName)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	lease := control.Lease
	if lease == nil {
		return nil
	}
	task, err := d.sessionLeaseHolderTask(ctx, namespace, sessionName, lease)
	if err != nil || task == nil {
		return err
	}
	execution := task.Status.Execution
	if !task.Status.Phase.IsTerminal() || d.isActive(task.UID) || execution == nil ||
		int64(execution.Attempt) != lease.Attempt || execution.PromptID != lease.PromptID {
		return nil
	}
	attemptID, err := promptAttemptIDFromTask(task)
	if err != nil {
		return err
	}
	attempt, err := d.Store.GetPromptAttempt(ctx, attemptID)
	if err != nil {
		return err
	}
	switch attempt.ExecutionState {
	case store.PromptExecutionCancelled, store.PromptExecutionFailed, store.PromptExecutionOutcomeUnknown:
	default:
		return nil
	}
	fence, err := d.Epochs.CurrentFence(ctx)
	if err != nil {
		return err
	}
	session, err := d.recoveredTaskSession(ctx, task, attempt)
	switch {
	case err == nil && session != nil && session.Turn != nil && session.Turn.Turn.State == store.SessionTurnOpen:
		cause := fmt.Errorf("session lease of terminal task %s/%s settled for Session deletion", task.Namespace, task.Name)
		if err := d.reconcileUnfinalizedTaskSession(ctx, task, fence, session, cause); err != nil {
			return err
		}
		return nil
	case err == nil:
		return nil
	case !errors.Is(err, store.ErrNotFound):
		return err
	}
	// The holder's turn was never persisted: release its pre-prompt lease.
	key, ok := acpAttemptSessionTurnKey(control, attempt)
	if !ok {
		return nil
	}
	if err := validateACPSessionLease(control, key); err != nil {
		return err
	}
	_, err = d.Sessions.ReleaseMutationLease(ctx, ACPReleaseSessionLeaseRequest{
		Lease: ACPSessionLease{Session: *control, Key: key}, Fence: fence, ReleasedAt: time.Now().UTC(),
	})
	return err
}

// sessionLeaseHolderTask finds the Task a Session lease names, by UID, among
// the namespace's Tasks that reference the Session.
func (d *ACPDispatcher) sessionLeaseHolderTask(
	ctx context.Context, namespace, sessionName string, lease *store.SessionMutationLease,
) (*corev1alpha1.Task, error) {
	tasks := &corev1alpha1.TaskList{}
	if err := uncachedReader(d.APIReader, d.Client).List(ctx, tasks, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range tasks.Items {
		task := &tasks.Items[i]
		if string(task.UID) != lease.TaskUID {
			continue
		}
		if task.Spec.SessionRef == nil || strings.TrimSpace(task.Spec.SessionRef.Name) != sessionName {
			return nil, nil
		}
		return task, nil
	}
	return nil, nil
}
