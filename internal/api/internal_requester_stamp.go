/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"strings"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/labels"
)

// SealChildRequesterStamp seals the requester stamp onto a child Task the
// calling worker created for its own Task. The link is authenticated end to
// end: the caller must be the parent Task's current worker Pod, the parent
// must itself carry a valid stamp, the child must be controller-owned by the
// parent, and the child's requester must equal the parent's. A child sealed
// this way inherits connector authority; one merely planted with the parent's
// UID as owner never does, because nothing seals it.
func (h *InternalHandlers) SealChildRequesterStamp(c fiber.Ctx) error {
	namespace := strings.TrimSpace(c.Params("namespace"))
	taskName := strings.TrimSpace(c.Params("taskName"))
	childName := strings.TrimSpace(c.Params("child"))
	if namespace == "" || taskName == "" || childName == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace, taskName, and child are required")
	}
	if len(requesterStampKey) == 0 {
		return fiber.NewError(fiber.StatusNotImplemented, "requester stamps are not enabled on this controller")
	}
	parent, err := h.internalCallerAuthorizer().verifyTaskCaller(c, namespace, taskName)
	if err != nil {
		return err
	}
	if !connectors.RequesterStampValid(requesterStampKey, parent) {
		return fiber.NewError(fiber.StatusForbidden, "the parent task carries no verified requester")
	}
	ctx := c.Context()
	reader := h.apiReader
	if reader == nil {
		reader = h.k8sClient
	}
	child := &corev1alpha1.Task{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: childName}, child); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "child task not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, "failed to read child task")
	}
	owner := metav1.GetControllerOf(child)
	if owner == nil || owner.UID != parent.UID || owner.Kind != "Task" || owner.APIVersion != corev1alpha1.GroupVersion.String() {
		return fiber.NewError(fiber.StatusForbidden, "the child task is not controller-owned by the calling task")
	}
	if child.Spec.RequestedBy == nil || parent.Spec.RequestedBy == nil ||
		child.Spec.RequestedBy.Issuer != parent.Spec.RequestedBy.Issuer || child.Spec.RequestedBy.Subject != parent.Spec.RequestedBy.Subject {
		return fiber.NewError(fiber.StatusForbidden, "the child task names a different requester than its parent")
	}
	// The stamp is computed from the identity that was just validated (the
	// parent's) and written together with the source annotation in one
	// patch fenced on the child's resource version, so a requester swapped
	// in between validation and sealing makes the write conflict instead of
	// being signed. Only this authenticated path seals a child.
	stamp := connectors.RequesterStamp(requesterStampKey, child.UID, parent.Spec.RequestedBy.Issuer, parent.Spec.RequestedBy.Subject)
	if stamp == "" {
		return fiber.NewError(fiber.StatusInternalServerError, "the child task has no UID to bind the requester stamp to")
	}
	original := child.DeepCopy()
	if child.Annotations == nil {
		child.Annotations = map[string]string{}
	}
	child.Annotations[labels.AnnotationRequestedBySource] = labels.RequestedBySourceAPI
	child.Annotations[labels.AnnotationRequestedByStamp] = stamp
	if err := h.k8sClient.Patch(ctx, child, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) {
			return fiber.NewError(fiber.StatusConflict, "the child task changed while it was being sealed; retry")
		}
		return fiber.NewError(fiber.StatusInternalServerError, "failed to seal the child task")
	}
	return c.JSON(fiber.Map{"sealed": true})
}
