/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/orka-agents/orka/internal/labels"
)

func TestWorkspaceAttachmentSecretValidatorProtectsCredentialLifecycle(t *testing.T) {
	validator := newTestWorkspaceAttachmentSecretValidator(t)
	unprotected := admissionSecret("ordinary")
	protected := admissionSecret("attachment")
	protected.Labels[labels.LabelWorkspaceAttachment] = "workspace-uid"

	tests := []struct {
		name      string
		operation admissionv1.Operation
		username  string
		object    *corev1.Secret
		oldObject *corev1.Secret
		allowed   bool
	}{
		{name: "unprotected create", operation: admissionv1.Create, username: untrustedUsername, object: unprotected, allowed: true},
		{name: "forged create", operation: admissionv1.Create, username: untrustedUsername, object: protected},
		{name: "controller create", operation: admissionv1.Create, username: trustedControllerUser, object: protected, allowed: true},
		{
			name: "add protected label", operation: admissionv1.Update, username: untrustedUsername,
			oldObject: unprotected, object: protected,
		},
		{
			name: "mutate protected bearer", operation: admissionv1.Update, username: untrustedUsername,
			oldObject: protected, object: mutateAdmissionSecret(protected, func(secret *corev1.Secret) {
				secret.Data["token"] = []byte("attacker-selected")
			}),
		},
		{
			name: "remove protected label", operation: admissionv1.Update, username: untrustedUsername,
			oldObject: protected, object: mutateAdmissionSecret(protected, func(secret *corev1.Secret) {
				delete(secret.Labels, labels.LabelWorkspaceAttachment)
			}),
		},
		{
			name: "controller update", operation: admissionv1.Update, username: trustedControllerUser,
			oldObject: protected, object: mutateAdmissionSecret(protected, func(secret *corev1.Secret) {
				secret.Data["token"] = []byte("rotated")
			}), allowed: true,
		},
		{name: "unprotected delete", operation: admissionv1.Delete, username: untrustedUsername, oldObject: unprotected, allowed: true},
		{name: "worker delete", operation: admissionv1.Delete, username: untrustedUsername, oldObject: protected},
		{name: "controller delete", operation: admissionv1.Delete, username: trustedControllerUser, oldObject: protected, allowed: true},
		{name: "generic garbage collector delete", operation: admissionv1.Delete, username: genericGarbageCollectorUsername, oldObject: protected, allowed: true},
		{name: "garbage collector delete", operation: admissionv1.Delete, username: garbageCollectorUsername, oldObject: protected, allowed: true},
		{name: "namespace controller delete", operation: admissionv1.Delete, username: namespaceControllerUsername, oldObject: protected, allowed: true},
		{name: "kube controller manager delete", operation: admissionv1.Delete, username: kubeControllerManagerUsername, oldObject: protected, allowed: true},
		{name: "cleanup controller create", operation: admissionv1.Create, username: kubeControllerManagerUsername, object: protected},
		{name: "cleanup controller update", operation: admissionv1.Update, username: kubeControllerManagerUsername, oldObject: protected, object: protected},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := validator.Handle(context.Background(), secretAdmissionRequest(
				t, tt.operation, tt.username, tt.object, tt.oldObject,
			))
			require.Equal(t, tt.allowed, response.Allowed, response.Result.Message)
			if !tt.allowed {
				require.Contains(t, response.Result.Message, "only an authorized controller identity")
			}
		})
	}
}

func newTestWorkspaceAttachmentSecretValidator(t *testing.T) *WorkspaceAttachmentSecretValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	return &WorkspaceAttachmentSecretValidator{
		decoder: ctrladmission.NewDecoder(scheme),
		config:  ExecutionModeConfig{ControllerUsernames: []string{trustedControllerUser}}.normalized(),
	}
}

func admissionSecret(name string) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: admissionTestNamespace,
			Labels:    map[string]string{},
		},
		Data: map[string][]byte{"token": []byte("controller-generated")},
	}
}

func mutateAdmissionSecret(secret *corev1.Secret, mutate func(*corev1.Secret)) *corev1.Secret {
	copy := secret.DeepCopy()
	mutate(copy)
	return copy
}

func secretAdmissionRequest(
	t *testing.T,
	operation admissionv1.Operation,
	username string,
	object *corev1.Secret,
	oldObject *corev1.Secret,
) ctrladmission.Request {
	t.Helper()
	request := ctrladmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: operation,
		Namespace: admissionTestNamespace,
		UserInfo:  authenticationv1.UserInfo{Username: username},
	}}
	if object != nil {
		request.Object = runtime.RawExtension{Raw: mustMarshalAdmissionSecret(t, object)}
	}
	if oldObject != nil {
		request.OldObject = runtime.RawExtension{Raw: mustMarshalAdmissionSecret(t, oldObject)}
	}
	return request
}

func mustMarshalAdmissionSecret(t *testing.T, secret *corev1.Secret) []byte {
	t.Helper()
	data, err := json.Marshal(secret)
	require.NoError(t, err)
	return data
}
