/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

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
	MaxChainLength           = 32
	maxTaskNameLength        = 253
	continuationSuffixPrefix = "-resume-"
)

var ErrChainBroken = errors.New("task continuation chain is broken")

func Active(wait *corev1alpha1.TaskWaitStatus) bool {
	return wait != nil && !wait.State.IsTerminal()
}

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
		return MaxChainLength
	}
	return int32(value)
}

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

func Predecessor(ctx context.Context, reader client.Reader, task *corev1alpha1.Task) (*corev1alpha1.Task, bool, error) {
	if task == nil || reader == nil {
		return nil, false, nil
	}
	name := strings.TrimSpace(task.Annotations[labels.AnnotationContinuationOfName])
	uid := strings.TrimSpace(task.Annotations[labels.AnnotationContinuationOfUID])
	if name == "" || uid == "" {
		return nil, false, nil
	}
	previous := &corev1alpha1.Task{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: name}, previous); err != nil {
		return nil, false, client.IgnoreNotFound(err)
	}
	if !Continues(previous, task) {
		return nil, false, nil
	}
	return previous, true, nil
}

func Continues(previous, task *corev1alpha1.Task) bool {
	if previous == nil || task == nil || previous.UID == "" || task.UID == "" || previous.Namespace != task.Namespace {
		return false
	}
	wait := previous.Status.Wait
	return previous.Name == strings.TrimSpace(task.Annotations[labels.AnnotationContinuationOfName]) &&
		string(previous.UID) == strings.TrimSpace(task.Annotations[labels.AnnotationContinuationOfUID]) &&
		wait != nil && wait.State == corev1alpha1.TaskWaitStateResumed &&
		wait.ContinuationTaskName == task.Name && wait.ContinuationTaskUID == string(task.UID)
}

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

func TerminalPhase(phase corev1alpha1.TaskPhase) bool {
	switch phase {
	case corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled:
		return true
	default:
		return false
	}
}

func Settled(tail *corev1alpha1.Task) bool {
	return tail != nil && TerminalPhase(tail.Status.Phase) && !Active(tail.Status.Wait)
}
