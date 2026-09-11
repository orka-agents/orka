package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/store"
)

// settleHarnessV1RuntimeSessionRecord owns the restored legacy runtime-session
// payload lifecycle. The durable v1 attempt and SessionTurn are already
// terminal before this method clears Task ownership or physically reclaims a
// task-scoped record.
func (d *HarnessV1Dispatcher) settleHarnessV1RuntimeSessionRecord(
	ctx context.Context,
	task *corev1alpha1.Task,
	attempt *store.HarnessV1Attempt,
) error {
	runtimes, ok := d.Attempts.(harness.RuntimeSessionStore)
	if !ok || attempt == nil || attempt.RuntimeSessionID == "" {
		return nil
	}
	id := harness.RuntimeSessionID(attempt.RuntimeSessionID)
	session, err := runtimes.GetRuntimeSession(ctx, task.Namespace, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if session.Owner.ActiveTask != "" && session.Owner.ActiveTask != task.Name {
		return fmt.Errorf("%w: runtime session %s is owned by Task %q", store.ErrConflict, id, session.Owner.ActiveTask)
	}
	sessionBacked := task.Spec.SessionRef != nil
	clearOwner := ""
	transition := func(target harness.RuntimeSessionState) error {
		updated, transitionErr := runtimes.TransitionRuntimeSession(ctx, harness.RuntimeSessionTransition{
			Namespace: task.Namespace, ID: id, From: session.State, To: target,
			ActiveTask: &clearOwner, UpdatedAt: time.Now().UTC(),
		})
		if transitionErr != nil {
			return transitionErr
		}
		session = updated
		return nil
	}

	for range 4 {
		if session.State == harness.RuntimeSessionStateDeleted {
			return runtimes.DeleteRuntimeSession(ctx, task.Namespace, id)
		}
		if sessionBacked {
			switch session.CleanupPolicy {
			case harness.RuntimeCleanupPolicyRetain:
				switch session.State {
				case harness.RuntimeSessionStateTurnRunning, harness.RuntimeSessionStateReady:
					if err := transition(harness.RuntimeSessionStateIdle); err != nil {
						return err
					}
					continue
				case harness.RuntimeSessionStateIdle:
					return transition(harness.RuntimeSessionStateRetained)
				case harness.RuntimeSessionStateRetained:
					return transition(harness.RuntimeSessionStateRetained)
				}
			case harness.RuntimeCleanupPolicySuspend:
				switch session.State {
				case harness.RuntimeSessionStateTurnRunning, harness.RuntimeSessionStateReady:
					if err := transition(harness.RuntimeSessionStateIdle); err != nil {
						return err
					}
					continue
				case harness.RuntimeSessionStateIdle:
					return transition(harness.RuntimeSessionStateSuspended)
				case harness.RuntimeSessionStateSuspended:
					return transition(harness.RuntimeSessionStateSuspended)
				}
			case harness.RuntimeCleanupPolicyDelete:
				// Continue into the deletion state machine below.
			}
		}
		switch session.State {
		case harness.RuntimeSessionStateDeleting:
			if err := transition(harness.RuntimeSessionStateDeleted); err != nil {
				return err
			}
		default:
			if !harness.RuntimeSessionTransitionAllowed(session.State, harness.RuntimeSessionStateDeleting) {
				return fmt.Errorf("%w: runtime session %s state %s cannot be reclaimed after terminal attempt",
					store.ErrConflict, id, session.State)
			}
			if err := transition(harness.RuntimeSessionStateDeleting); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("%w: runtime session %s did not reach a terminal cleanup state", store.ErrNotReady, id)
}
