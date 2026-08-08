/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestTaskExecutionAuthorityValidatorRestrictsStatusWriters(t *testing.T) {
	validator := newTestTaskExecutionAuthorityValidator(t)

	tests := []struct {
		name   string
		mutate func(*corev1alpha1.Task)
	}{
		{
			name: "phase",
			mutate: func(task *corev1alpha1.Task) {
				task.Status.Phase = corev1alpha1.TaskPhaseRunning
			},
		},
		{
			name: "harness runtime",
			mutate: func(task *corev1alpha1.Task) {
				task.Status.HarnessRuntime = &corev1alpha1.HarnessRuntimeStatus{
					ContractVersion: "orka.harness.v1",
				}
			},
		},
		{
			name: "execution",
			mutate: func(task *corev1alpha1.Task) {
				task.Status.Execution = &corev1alpha1.TaskExecutionStatus{
					State: corev1alpha1.TaskExecutionStateRunning,
				}
			},
		},
		{
			name: "delivery",
			mutate: func(task *corev1alpha1.Task) {
				task.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{
					State: corev1alpha1.TaskDeliveryStatePreparing,
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldTask := newAdmissionTestTask()
			oldTask.Status.AgentExecutionBinding = &corev1alpha1.AgentExecutionBinding{}
			statusUpdate := oldTask.DeepCopy()
			tt.mutate(statusUpdate)

			response := validator.Handle(context.Background(), admissionRequest(
				t, admissionv1.Update, untrustedUsername, statusUpdate, oldTask, statusSubresource,
			))
			require.False(t, response.Allowed)
			require.Contains(t, response.Result.Message, "only an authorized controller identity may update Task status")
		})
	}
}

func TestTaskExecutionAuthorityValidatorAllowsControllerStatusUpdate(t *testing.T) {
	validator := newTestTaskExecutionAuthorityValidator(t)
	oldTask := newAdmissionTestTask()
	oldTask.Status.AgentExecutionBinding = &corev1alpha1.AgentExecutionBinding{}
	statusUpdate := oldTask.DeepCopy()
	statusUpdate.Status.Phase = corev1alpha1.TaskPhaseRunning

	response := validator.Handle(context.Background(), admissionRequest(
		t, admissionv1.Update, trustedControllerUser, statusUpdate, oldTask, statusSubresource,
	))
	require.True(t, response.Allowed, response.Result.Message)
}

func TestTaskExecutionAuthorityValidatorAllowsUnchangedStatus(t *testing.T) {
	validator := newTestTaskExecutionAuthorityValidator(t)
	oldTask := newAdmissionTestTask()

	t.Run("spec update", func(t *testing.T) {
		specUpdate := withImage(oldTask.DeepCopy(), "alpine")
		response := validator.Handle(context.Background(), admissionRequest(
			t, admissionv1.Update, untrustedUsername, specUpdate, oldTask, "",
		))
		require.True(t, response.Allowed, response.Result.Message)
	})

	t.Run("no-op status update", func(t *testing.T) {
		response := validator.Handle(context.Background(), admissionRequest(
			t, admissionv1.Update, untrustedUsername, oldTask.DeepCopy(), oldTask, statusSubresource,
		))
		require.True(t, response.Allowed, response.Result.Message)
	})
}

func newTestTaskExecutionAuthorityValidator(t *testing.T) *TaskExecutionAuthorityValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	return &TaskExecutionAuthorityValidator{
		decoder: ctrladmission.NewDecoder(scheme),
		config:  ExecutionModeConfig{ControllerUsernames: []string{trustedControllerUser}}.normalized(),
	}
}
