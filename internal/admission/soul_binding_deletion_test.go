package admission

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

func soulBoundAdmissionTask() *corev1alpha1.Task {
	task := newAdmissionTestTask()
	task.UID = "task-uid"
	task.Generation = 1
	task.Finalizers = []string{labels.TaskFinalizer}
	task.Spec.Type = corev1alpha1.TaskTypeAI
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	task.Status.SoulBinding = &corev1alpha1.TaskSoulBinding{
		TaskGeneration: 1, AgentUID: "agent-uid", AgentGeneration: 1,
		SoulDigest: digest, PromptDigest: digest,
	}
	return task
}

func TestTaskSoulBindingDeletionPreservesBoundGeneration(t *testing.T) {
	for _, tc := range []struct {
		name        string
		username    string
		subresource string
		mutate      func(*corev1alpha1.Task)
		allowed     bool
	}{
		{name: "controller removes finalizer", username: trustedControllerUser, mutate: func(task *corev1alpha1.Task) { task.Finalizers = nil }, allowed: true},
		{name: "controller settles status", username: trustedControllerUser, subresource: statusSubresource, mutate: func(task *corev1alpha1.Task) { task.Status.Phase = corev1alpha1.TaskPhaseCancelled }, allowed: true},
		{name: "untrusted finalizer removal", username: untrustedUsername, mutate: func(task *corev1alpha1.Task) { task.Finalizers = nil }},
		{name: "untrusted status write", username: untrustedUsername, subresource: statusSubresource, mutate: func(task *corev1alpha1.Task) { task.Status.Phase = corev1alpha1.TaskPhaseCancelled }},
		{name: "controller cannot rebind generation", username: trustedControllerUser, subresource: statusSubresource, mutate: func(task *corev1alpha1.Task) { task.Status.SoulBinding.TaskGeneration = task.Generation }},
		{name: "controller cannot remove binding", username: trustedControllerUser, subresource: statusSubresource, mutate: func(task *corev1alpha1.Task) { task.Status.SoulBinding = nil }},
		{name: "controller cannot edit bound spec", username: trustedControllerUser, mutate: func(task *corev1alpha1.Task) { task.Spec.Prompt = "different prompt" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validator := newTestTaskExecutionAuthorityValidator(t)
			old := soulBoundAdmissionTask()
			// The API server advances generation when deletion begins, without
			// changing the spec whose generation the soul binding records.
			old.Generation = 2
			now := metav1.Now()
			old.DeletionTimestamp = &now
			updated := old.DeepCopy()
			tc.mutate(updated)
			response := validator.Handle(context.Background(), admissionRequest(t, admissionv1.Update, tc.username, updated, old, tc.subresource))
			require.Equal(t, tc.allowed, response.Allowed, "%+v", response.Result)
			if tc.allowed {
				require.Equal(t, old.Spec, updated.Spec)
				require.Equal(t, old.Status.SoulBinding, updated.Status.SoulBinding)
			}
		})
	}
}

func TestTaskSoulBindingCreationStillRequiresCurrentIdentity(t *testing.T) {
	for _, scenario := range []string{"wrong generation", "deleting", "wrong task type"} {
		t.Run(scenario, func(t *testing.T) {
			validator := newTestTaskExecutionAuthorityValidator(t)
			updated := soulBoundAdmissionTask()
			switch scenario {
			case "wrong generation":
				updated.Generation = 2
			case "deleting":
				now := metav1.Now()
				updated.DeletionTimestamp = &now
			case "wrong task type":
				updated.Spec.Type = corev1alpha1.TaskTypeContainer
			}
			old := updated.DeepCopy()
			old.Status.SoulBinding = nil
			response := validator.Handle(context.Background(), admissionRequest(t, admissionv1.Update, trustedControllerUser, updated, old, statusSubresource))
			require.False(t, response.Allowed)
		})
	}
}

func TestTaskSoulBindingFinalizerStillRequiresDeletion(t *testing.T) {
	validator := newTestTaskExecutionAuthorityValidator(t)
	old := soulBoundAdmissionTask()
	updated := old.DeepCopy()
	updated.Finalizers = nil
	response := validator.Handle(context.Background(), admissionRequest(t, admissionv1.Update, trustedControllerUser, updated, old, ""))
	require.False(t, response.Allowed)
}
