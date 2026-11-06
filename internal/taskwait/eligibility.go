/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package taskwait

import (
	"errors"
	"fmt"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const (
	// DefaultAgentTaskTimeout is the deadline the ACP dispatcher applies to a
	// harness v2 Task without spec.timeout.
	DefaultAgentTaskTimeout = 30 * time.Minute
	// MinContinuationBudget is the least time a continuation turn must have
	// before the chain deadline. A wait that cannot leave this much expires.
	MinContinuationBudget = time.Minute
	// MaxReasonLength bounds the agent-supplied wait reason.
	MaxReasonLength = 512
)

// ErrIneligible reports a Task that cannot park its turn.
var ErrIneligible = errors.New("task cannot park its turn")

// Deadline returns the absolute deadline of an ACP Task. It matches the
// dispatcher: spec.timeout, or the harness v2 default, from creation.
func Deadline(task *corev1alpha1.Task) time.Time {
	if task == nil {
		return time.Time{}
	}
	timeout := DefaultAgentTaskTimeout
	if task.Spec.Timeout != nil && task.Spec.Timeout.Duration > 0 {
		timeout = task.Spec.Timeout.Duration
	}
	return task.CreationTimestamp.Add(timeout)
}

// Eligibility explains why a Task cannot park its turn, or returns nil. A
// continuation resumes from the Session transcript under the same frozen
// authority, so parking requires a Session that records this turn and
// rejects inputs that a later Task cannot safely inherit.
func Eligibility(task *corev1alpha1.Task) error {
	switch {
	case task == nil:
		return wrap("the Task is unavailable")
	case task.Spec.SessionRef == nil || strings.TrimSpace(task.Spec.SessionRef.Name) == "":
		return wrap("parking requires spec.sessionRef so the continuation can resume from the Session transcript")
	case !task.Spec.SessionRef.Append:
		return wrap("parking requires spec.sessionRef.append so this turn is recorded for the continuation")
	case task.Spec.SessionRef.PromptIncluded || strings.TrimSpace(task.Spec.SessionRef.ThroughMessageID) != "":
		return wrap("gateway conversation turns cannot park; the gateway starts the next turn")
	case task.Spec.Transaction != nil:
		return wrap("transaction-bound Tasks cannot park; a continuation cannot inherit a transaction token")
	case strings.TrimSpace(task.Spec.Schedule) != "":
		return wrap("scheduled Tasks cannot park")
	case task.Spec.PriorTaskRef != nil:
		return wrap("Tasks that apply a prior Task's diff cannot park")
	case ChainIndex(task) >= MaxChainLength:
		return wrap("this Task's continuation chain is at its limit")
	}
	return nil
}

func wrap(message string) error {
	return fmt.Errorf("%w: %s", ErrIneligible, message)
}
