/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package taskwait holds the shared, side-effect-free rules for data-only
// parked agent turns: continuation naming and continuation-chain resolution.
//
// A parked Task's own turn has ended and its runtime capacity is released.
// status.wait is the only authority that links it to the continuation Task
// which resumes its Session. Readers follow that link to find the Task whose
// result represents the chain, and treat a chain as settled only when its
// newest Task is terminal and holds no active wait.
package taskwait

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

const (
	// MaxChainLength bounds how many continuations one chain may create.
	MaxChainLength = 32
	// maxTaskNameLength is the Kubernetes object name limit.
	maxTaskNameLength = 253
	// continuationSuffixPrefix separates the parked Task name from the
	// UID-derived suffix of its continuation.
	continuationSuffixPrefix = "-resume-"
)

// ErrChainBroken reports a continuation link that no longer resolves to the
// exact Task the controller created.
var ErrChainBroken = errors.New("task continuation chain is broken")

// Active reports whether a wait can still transition, so the Task's chain is
// not settled even when the Task phase is terminal.
func Active(wait *corev1alpha1.TaskWaitStatus) bool {
	return wait != nil && !wait.State.IsTerminal()
}

// ContinuationTaskName returns the deterministic continuation name for one
// parked Task incarnation. It is derived from the immutable UID, so a reused
// Task name cannot collide with an earlier incarnation's continuation.
func ContinuationTaskName(task *corev1alpha1.Task) string {
	if task == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(task.UID))
	suffix := continuationSuffixPrefix + hex.EncodeToString(sum[:])[:10]
	prefix := strings.TrimRight(task.Name, "-.")
	if limit := maxTaskNameLength - len(suffix); len(prefix) > limit {
		prefix = strings.TrimRight(prefix[:limit], "-.")
	}
	return prefix + suffix
}

// ChainIndex returns the number of continuations before task in its chain.
func ChainIndex(task *corev1alpha1.Task) int32 {
	if task == nil {
		return 0
	}
	raw := strings.TrimSpace(task.Annotations[labels.AnnotationContinuationIndex])
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || value < 0 {
		// A malformed index is treated as exhausted so it cannot extend a chain.
		return MaxChainLength
	}
	return int32(value)
}

// Continuation loads the continuation Task recorded by a resumed wait and
// verifies it is the exact incarnation the controller created.
func Continuation(ctx context.Context, reader client.Reader, task *corev1alpha1.Task) (*corev1alpha1.Task, error) {
	if task == nil || task.Status.Wait == nil || task.Status.Wait.State != corev1alpha1.TaskWaitStateResumed {
		return nil, nil
	}
	wait := task.Status.Wait
	name := strings.TrimSpace(wait.ContinuationTaskName)
	uid := strings.TrimSpace(wait.ContinuationTaskUID)
	if name == "" || uid == "" {
		return nil, fmt.Errorf("%w: task %s/%s resumed without a continuation identity", ErrChainBroken, task.Namespace, task.Name)
	}
	next := &corev1alpha1.Task{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: name}, next); err != nil {
		return nil, fmt.Errorf("load continuation of task %s/%s: %w", task.Namespace, task.Name, err)
	}
	if string(next.UID) != uid || next.Annotations[labels.AnnotationContinuationOfUID] != string(task.UID) {
		return nil, fmt.Errorf("%w: continuation %s/%s is not the Task created for %s", ErrChainBroken, task.Namespace, name, task.Name)
	}
	return next, nil
}

// ResolveTail follows resumed waits from task to the newest Task of its chain.
// It returns task itself when it has not resumed.
func ResolveTail(ctx context.Context, reader client.Reader, task *corev1alpha1.Task) (*corev1alpha1.Task, error) {
	current := task
	for range MaxChainLength + 1 {
		next, err := Continuation(ctx, reader, current)
		if err != nil {
			return nil, err
		}
		if next == nil {
			return current, nil
		}
		current = next
	}
	return nil, fmt.Errorf("%w: task %s/%s exceeds %d continuations", ErrChainBroken, task.Namespace, task.Name, MaxChainLength)
}

// TerminalPhase reports whether a Task phase can no longer change.
func TerminalPhase(phase corev1alpha1.TaskPhase) bool {
	switch phase {
	case corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled:
		return true
	default:
		return false
	}
}

// Settled reports whether the chain tail is finished: its phase is terminal and
// it holds no wait that could still start another turn.
func Settled(tail *corev1alpha1.Task) bool {
	return tail != nil && TerminalPhase(tail.Status.Phase) && !Active(tail.Status.Wait)
}
