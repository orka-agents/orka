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
	"github.com/orka-agents/orka/internal/labels"
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
// dispatcher: spec.timeout, or the harness v2 default, from creation, never
// later than the chain deadline a continuation inherited.
func Deadline(task *corev1alpha1.Task) time.Time {
	if task == nil {
		return time.Time{}
	}
	timeout := DefaultAgentTaskTimeout
	if task.Spec.Timeout != nil && task.Spec.Timeout.Duration > 0 {
		timeout = task.Spec.Timeout.Duration
	}
	return ClampToChainDeadline(task, task.CreationTimestamp.Add(timeout))
}

// ChainDeadline returns the absolute deadline a continuation inherited from
// the first Task of its chain. Its timeout alone is relative to its own
// creation, which happens after the remaining budget was computed.
func ChainDeadline(task *corev1alpha1.Task) (time.Time, bool) {
	if task == nil {
		return time.Time{}, false
	}
	raw := strings.TrimSpace(task.Annotations[labels.AnnotationContinuationChainDeadline])
	if raw == "" {
		return time.Time{}, false
	}
	deadline, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return deadline.UTC(), true
}

// ClampToChainDeadline returns deadline, or the inherited chain deadline when
// that is earlier. The annotation can only shorten a Task's deadline.
func ClampToChainDeadline(task *corev1alpha1.Task, deadline time.Time) time.Time {
	if chain, ok := ChainDeadline(task); ok && chain.Before(deadline) {
		return chain
	}
	return deadline
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
