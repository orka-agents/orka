package tools

import (
	"context"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

const (
	repositorySecurityCreatedBy     = "repository-security"
	harnessWrapperAttemptAnnotation = "orka.ai/harness-wrapper-attempt"
)

func toolTaskOutputAttempt(task *corev1alpha1.Task) int64 {
	if task == nil {
		return 0
	}
	attempt := int64(task.Status.Attempts)
	if task.Status.Phase == corev1alpha1.TaskPhasePending || task.Status.Phase == corev1alpha1.TaskPhaseScheduled {
		attempt++
	}
	if task.Annotations != nil {
		if planned, err := strconv.ParseInt(strings.TrimSpace(task.Annotations[harnessWrapperAttemptAnnotation]), 10, 64); err == nil &&
			planned > 0 && planned >= attempt {
			return planned
		}
	}
	return attempt
}

func toolTaskRequiresBoundOutput(task *corev1alpha1.Task) bool {
	if task == nil {
		return false
	}
	if strings.TrimSpace(task.Labels[labels.LabelCreatedBy]) == repositorySecurityCreatedBy {
		return true
	}
	owner := metav1.GetControllerOf(task)
	return owner != nil && owner.UID != "" && owner.Kind == "RepositoryScan" &&
		owner.APIVersion == corev1alpha1.GroupVersion.String()
}

func toolTaskResult(ctx context.Context, toolCtx *ToolContext, task *corev1alpha1.Task) ([]byte, error) {
	if toolCtx == nil || toolCtx.ResultStore == nil || task == nil {
		return nil, store.ErrNotFound
	}
	if toolTaskRequiresBoundOutput(task) {
		switch toolCtx.WorkerOutputBindingMode {
		case "", security.WorkerOutputBindingOff:
			return toolCtx.ResultStore.GetResult(ctx, task.Namespace, task.Name)
		case security.WorkerOutputBindingAudit:
			if result, err := toolBoundTaskResult(ctx, toolCtx, task); err == nil {
				return result, nil
			}
			return toolCtx.ResultStore.GetResult(ctx, task.Namespace, task.Name)
		case security.WorkerOutputBindingEnforce:
			return toolBoundTaskResult(ctx, toolCtx, task)
		default:
			return nil, store.ErrNotReady
		}
	}
	if result, err := toolBoundTaskResult(ctx, toolCtx, task); err == nil {
		return result, nil
	}
	return toolCtx.ResultStore.GetResult(ctx, task.Namespace, task.Name)
}

func toolBoundTaskResult(ctx context.Context, toolCtx *ToolContext, task *corev1alpha1.Task) ([]byte, error) {
	if task.UID == "" {
		return nil, store.ErrNotReady
	}
	bound, ok := any(toolCtx.ResultStore).(store.BoundOutputStore)
	if !ok {
		return nil, store.ErrNotReady
	}
	result, err := bound.GetBoundResult(ctx, task.Namespace, task.Name, string(task.UID), toolTaskOutputAttempt(task))
	if err != nil {
		return nil, err
	}
	return result.Data, nil
}
