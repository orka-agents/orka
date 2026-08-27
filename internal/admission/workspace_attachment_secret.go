/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"context"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/orka-agents/orka/internal/labels"
)

const (
	WorkspaceAttachmentSecretWebhookPath = "/validate-v1-secret-workspace-attachment"

	genericGarbageCollectorUsername = "system:serviceaccount:kube-system:generic-garbage-collector"
	garbageCollectorUsername        = "system:serviceaccount:kube-system:garbage-collector"
	namespaceControllerUsername     = "system:serviceaccount:kube-system:namespace-controller"
	kubeControllerManagerUsername   = "system:kube-controller-manager"
)

// WorkspaceAttachmentSecretValidator reserves attachment credential writes
// for exact controller identities. Namespace workers may read and manage other
// Secrets, but cannot forge or mutate the bearer authority recovered by core.
// Kubernetes cleanup controllers may delete it during garbage collection or
// namespace teardown.
type WorkspaceAttachmentSecretValidator struct {
	decoder admission.Decoder
	config  ExecutionModeConfig
}

func (v *WorkspaceAttachmentSecretValidator) Handle(_ context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update && req.Operation != admissionv1.Delete {
		return admission.Allowed("not a workspace attachment Secret write")
	}

	var object *corev1.Secret
	if req.Operation != admissionv1.Delete {
		object = &corev1.Secret{}
		if err := v.decoder.Decode(req, object); err != nil {
			return admission.Errored(http.StatusBadRequest, fmt.Errorf("decode workspace attachment Secret: %w", err))
		}
	}

	var oldObject *corev1.Secret
	if req.Operation == admissionv1.Update || req.Operation == admissionv1.Delete {
		oldObject = &corev1.Secret{}
		if err := v.decoder.DecodeRaw(req.OldObject, oldObject); err != nil {
			return admission.Errored(http.StatusBadRequest, fmt.Errorf("decode old workspace attachment Secret: %w", err))
		}
	}

	if !isWorkspaceAttachmentSecret(object) && !isWorkspaceAttachmentSecret(oldObject) {
		return admission.Allowed("Secret is not a workspace attachment credential")
	}
	if req.Operation == admissionv1.Delete && isKubernetesCleanupController(req.UserInfo.Username) {
		return admission.Allowed("Kubernetes cleanup controller may delete the workspace attachment Secret")
	}
	if !v.config.controller(req.UserInfo.Username) {
		return admission.Denied("only an authorized controller identity may create, update, or delete workspace attachment Secrets")
	}
	return admission.Allowed("authorized controller owns the workspace attachment Secret write")
}

func isKubernetesCleanupController(username string) bool {
	switch username {
	case genericGarbageCollectorUsername,
		garbageCollectorUsername,
		namespaceControllerUsername,
		kubeControllerManagerUsername:
		return true
	default:
		return false
	}
}

func isWorkspaceAttachmentSecret(secret *corev1.Secret) bool {
	if secret == nil {
		return false
	}
	_, protected := secret.Labels[labels.LabelWorkspaceAttachment]
	return protected
}
