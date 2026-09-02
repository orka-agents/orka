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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/executionmode"
)

func TestNamespaceExecutionModeValidatorOnlyAcquiresClaimOnCreate(t *testing.T) {
	validator := newTestNamespaceExecutionModeValidator(t)

	tests := []struct {
		name        string
		operation   admissionv1.Operation
		oldMode     string
		newMode     string
		allowed     bool
		messagePart string
	}{
		{
			name:      "create claimed namespace",
			operation: admissionv1.Create,
			newMode:   string(executionmode.HarnessV1),
			allowed:   true,
		},
		{
			name:      "preserve absent claim",
			operation: admissionv1.Update,
			allowed:   true,
		},
		{
			name:        "reject claim on existing namespace",
			operation:   admissionv1.Update,
			newMode:     string(executionmode.HarnessV2),
			messagePart: "existing namespace cannot acquire",
		},
		{
			name:      "preserve existing claim",
			operation: admissionv1.Update,
			oldMode:   string(executionmode.HarnessV1),
			newMode:   string(executionmode.HarnessV1),
			allowed:   true,
		},
		{
			name:        "reject changed claim",
			operation:   admissionv1.Update,
			oldMode:     string(executionmode.HarnessV1),
			newMode:     string(executionmode.HarnessV2),
			messagePart: "claim is immutable",
		},
		{
			name:        "reject removed claim",
			operation:   admissionv1.Update,
			oldMode:     string(executionmode.HarnessV1),
			messagePart: "claim is immutable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			object := admissionNamespace(tt.newMode)
			var oldObject *corev1.Namespace
			if tt.operation == admissionv1.Update {
				oldObject = admissionNamespace(tt.oldMode)
			}
			response := validator.Handle(context.Background(), namespaceAdmissionRequest(
				t, tt.operation, object, oldObject,
			))
			require.Equal(t, tt.allowed, response.Allowed, response.Result.Message)
			if tt.messagePart != "" {
				require.Contains(t, response.Result.Message, tt.messagePart)
			}
		})
	}
}

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

func newTestNamespaceExecutionModeValidator(t *testing.T) *NamespaceExecutionModeValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	return &NamespaceExecutionModeValidator{decoder: ctrladmission.NewDecoder(scheme)}
}

func admissionNamespace(mode string) *corev1.Namespace {
	labels := map[string]string{}
	if mode != "" {
		labels[executionmode.NamespaceLabel] = mode
	}
	return &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   "orka-test",
			Labels: labels,
		},
	}
}

func namespaceAdmissionRequest(
	t *testing.T,
	operation admissionv1.Operation,
	object *corev1.Namespace,
	oldObject *corev1.Namespace,
) ctrladmission.Request {
	t.Helper()
	request := ctrladmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: operation,
		Object:    runtime.RawExtension{Raw: mustMarshalNamespace(t, object)},
	}}
	if oldObject != nil {
		request.OldObject = runtime.RawExtension{Raw: mustMarshalNamespace(t, oldObject)}
	}
	return request
}

func mustMarshalNamespace(t *testing.T, namespace *corev1.Namespace) []byte {
	t.Helper()
	data, err := json.Marshal(namespace)
	require.NoError(t, err)
	return data
}

func TestAgentContractValidatorRequiresModelForHarnessV2Agents(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	namespace := admissionNamespace(string(executionmode.HarnessV2))
	validator := &AgentContractValidator{
		decoder: ctrladmission.NewDecoder(scheme),
		reader:  fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace).Build(),
	}
	contract := executionmode.HarnessV2.ContractVersion()
	agent := func(model string) *corev1alpha1.Agent {
		object := &corev1alpha1.Agent{
			TypeMeta:   metav1.TypeMeta{APIVersion: "core.orka.ai/v1alpha1", Kind: "Agent"},
			ObjectMeta: metav1.ObjectMeta{Name: "implementer", Namespace: namespace.Name},
			Spec: corev1alpha1.AgentSpec{
				Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex, ContractVersion: &contract},
			},
		}
		if model != "" {
			object.Spec.Model = &corev1alpha1.ModelConfig{Name: model}
		}
		return object
	}
	request := func(object *corev1alpha1.Agent) ctrladmission.Request {
		raw, err := json.Marshal(object)
		require.NoError(t, err)
		return ctrladmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Namespace: namespace.Name,
			Object:    runtime.RawExtension{Raw: raw},
		}}
	}
	denied := validator.Handle(context.Background(), request(agent("")))
	require.False(t, denied.Allowed, denied.Result.Message)
	require.Contains(t, denied.Result.Message, "requires spec.model.name")
	allowed := validator.Handle(context.Background(), request(agent("gpt-5.6-sol")))
	require.True(t, allowed.Allowed, allowed.Result.Message)
}
