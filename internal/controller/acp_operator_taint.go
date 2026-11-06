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

// acpTaskOperatorTainted reports whether operator access tainted the Task's
// RuntimeSession: the Task itself or its exact linked ExecutionWorkspace
// incarnation carries the durable marker. It reads the authoritative API so a
// marker written by orka-api moments earlier is never missed by a cache, and
// it fails closed on any read error other than a vanished workspace.
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
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("read ExecutionWorkspace operator taint: %w", err)
	}
	return string(workspace.UID) == uid && operatoraccess.Tainted(workspace.Annotations), nil
}

// operatorTaintedPublicationMessage is the bounded terminal message recorded
// on the publication, delivery status, and Task when operator access tainted
// the workspace, so the refusal is distinguishable from a user cancellation.
const operatorTaintedPublicationMessage = "publication refused: operator access (orka task exec or cp) tainted the workspace"

// taskPublicationRefused is the single pre-publishing decision shared by the
// fresh and recovery publication paths: a cancelled Task and a Task whose
// session was tainted by operator access both settle as cancelled before
// publish, so tainted workspace contents never reach a forge. terminalReason
// is empty for a user cancellation and operatorTaintedPublicationMessage for
// a taint refusal.
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

// operatorTaintedDelivery reports whether a delivery status records a taint
// refusal.
func operatorTaintedDelivery(status corev1alpha1.TaskDeliveryStatus) bool {
	return status.Reason == corev1alpha1.TaskDeliveryReasonOperatorTainted
}
