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
	DefaultAgentTaskTimeout = 30 * time.Minute
	MinContinuationBudget   = time.Minute
	MaxReasonLength         = 512
)

var ErrIneligible = errors.New("task cannot park its turn")

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

func ClampToChainDeadline(task *corev1alpha1.Task, deadline time.Time) time.Time {
	if chain, ok := ChainDeadline(task); ok && chain.Before(deadline) {
		return chain
	}
	return deadline
}

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
	case boundToAgentIdentity(task):
		return wrap("Tasks bound to an identity-bound Agent cannot park; a continuation cannot prove the identity delegation its parked turn was checked for")
	case ChainIndex(task) >= MaxChainLength:
		return wrap("this Task's continuation chain is at its limit")
	}
	return nil
}

func boundToAgentIdentity(task *corev1alpha1.Task) bool {
	binding := task.Status.AgentExecutionBinding
	return binding != nil && binding.Agent != nil && binding.Agent.Identity != nil
}

func wrap(message string) error {
	return fmt.Errorf("%w: %s", ErrIneligible, message)
}
