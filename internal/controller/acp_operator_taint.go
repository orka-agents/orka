package controller

import (
	"context"
	"fmt"
	"strings"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/operatoraccess"
)

func acpTaskOperatorTainted(ctx context.Context, reader client.Reader, task *corev1alpha1.Task) (bool, error) {
	if task == nil {
		return false, nil
	}
	latest := &corev1alpha1.Task{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(task), latest); err != nil {
		return false, fmt.Errorf("read Task operator taint: %w", err)
	}
	if latest.UID != task.UID {
		return false, fmt.Errorf("read Task operator taint: Task identity changed")
	}
	if operatoraccess.Tainted(latest.Annotations) {
		return true, nil
	}
	name := strings.TrimSpace(latest.Labels[acpExecutionWorkspaceLinkLabel])
	uid := strings.TrimSpace(latest.Annotations[acpExecutionWorkspaceUIDAnnotation])
	if name == "" || uid == "" {
		return false, nil
	}
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: latest.Namespace, Name: name}, workspace); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("read ExecutionWorkspace operator taint: %w", err)
		}
	} else if string(workspace.UID) == uid && operatoraccess.Tainted(workspace.Annotations) {
		return true, nil
	}
	return acpIncarnationLinkedTaskTainted(ctx, reader, latest.Namespace, name, types.UID(uid))
}

func ACPTaskOperatorTainted(ctx context.Context, reader client.Reader, task *corev1alpha1.Task) (bool, error) {
	return acpTaskOperatorTainted(ctx, reader, task)
}

func acpWorkspaceOperatorTainted(
	ctx context.Context,
	reader client.Reader,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (bool, error) {
	if workspace == nil {
		return false, nil
	}
	if operatoraccess.Tainted(workspace.Annotations) {
		return true, nil
	}
	if workspace.UID == "" {
		return false, nil
	}
	return acpIncarnationLinkedTaskTainted(ctx, reader, workspace.Namespace, workspace.Name, workspace.UID)
}

func acpIncarnationLinkedTaskTainted(
	ctx context.Context,
	reader client.Reader,
	namespace, name string,
	uid types.UID,
) (bool, error) {
	tasks := &corev1alpha1.TaskList{}
	if err := reader.List(ctx, tasks, client.InNamespace(namespace),
		client.MatchingLabels{acpExecutionWorkspaceLinkLabel: name}); err != nil {
		return false, fmt.Errorf("read operator taint of Tasks linked to ExecutionWorkspace %s: %w", name, err)
	}
	for i := range tasks.Items {
		linked := &tasks.Items[i]
		if strings.TrimSpace(linked.Annotations[acpExecutionWorkspaceUIDAnnotation]) == string(uid) &&
			operatoraccess.Tainted(linked.Annotations) {
			return true, nil
		}
	}
	return false, nil
}

const operatorTaintedPublicationMessage = "publication refused: operator access (orka task exec or cp) tainted the workspace"

func (d *ACPDispatcher) taskPublicationRefused(ctx context.Context, task *corev1alpha1.Task) (refused bool, terminalReason string, err error) {
	cancelled, err := d.taskCancellationRequested(ctx, task)
	if err != nil || cancelled {
		return cancelled, "", err
	}
	tainted, err := acpTaskOperatorTainted(ctx, uncachedReader(d.APIReader, d.Client), task)
	if err != nil || !tainted {
		return false, "", err
	}
	log.FromContext(ctx).Info("refusing publication of an operator-tainted workspace", "task", task.Name, "namespace", task.Namespace)
	return true, operatorTaintedPublicationMessage, nil
}

func operatorTaintedDelivery(status corev1alpha1.TaskDeliveryStatus) bool {
	return status.Reason == corev1alpha1.TaskDeliveryReasonOperatorTainted
}
