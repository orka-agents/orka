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

func TestAgentContractValidatorDefaultsContractFromNamespaceMode(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	namespace := admissionNamespace(string(executionmode.HarnessV2))
	validator := &AgentContractValidator{
		decoder: ctrladmission.NewDecoder(scheme),
		reader:  fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace).Build(),
	}
	v2 := executionmode.HarnessV2.ContractVersion()
	v1 := executionmode.HarnessV1.ContractVersion()
	agent := func(contract *corev1alpha1.AgentRuntimeContractVersion) *corev1alpha1.Agent {
		return &corev1alpha1.Agent{
			TypeMeta:   metav1.TypeMeta{APIVersion: "core.orka.ai/v1alpha1", Kind: "Agent"},
			ObjectMeta: metav1.ObjectMeta{Name: "implementer", Namespace: namespace.Name},
			Spec: corev1alpha1.AgentSpec{
				Model:   &corev1alpha1.ModelConfig{Name: "claude-sonnet-4-20250514"},
				Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeClaude, ContractVersion: contract},
			},
		}
	}
	request := func(operation admissionv1.Operation, object, old *corev1alpha1.Agent) ctrladmission.Request {
		raw, err := json.Marshal(object)
		require.NoError(t, err)
		req := ctrladmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: operation,
			Namespace: namespace.Name,
			Object:    runtime.RawExtension{Raw: raw},
		}}
		if old != nil {
			oldRaw, err := json.Marshal(old)
			require.NoError(t, err)
			req.OldObject = runtime.RawExtension{Raw: oldRaw}
		}
		return req
	}

	tests := []struct {
		name        string
		operation   admissionv1.Operation
		object      *corev1alpha1.Agent
		old         *corev1alpha1.Agent
		allowed     bool
		messagePart string
	}{
		{name: "create without contract takes the namespace mode", operation: admissionv1.Create, object: agent(nil), allowed: true},
		{name: "create with the namespace contract", operation: admissionv1.Create, object: agent(&v2), allowed: true},
		{name: "create with the other contract", operation: admissionv1.Create, object: agent(&v1), messagePart: "must match namespace execution mode"},
		{name: "update keeps an omitted contract omitted", operation: admissionv1.Update, object: agent(nil), old: agent(nil), allowed: true},
		{name: "update may write the namespace contract", operation: admissionv1.Update, object: agent(&v2), old: agent(nil), allowed: true},
		{name: "update may not write the other contract", operation: admissionv1.Update, object: agent(&v1), old: agent(nil), messagePart: "must match namespace execution mode"},
		{name: "update may not remove a written contract", operation: admissionv1.Update, object: agent(nil), old: agent(&v2), messagePart: "immutable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := validator.Handle(context.Background(), request(tt.operation, tt.object, tt.old))
			require.Equal(t, tt.allowed, response.Allowed, response.Result.Message)
			if tt.messagePart != "" {
				require.Contains(t, response.Result.Message, tt.messagePart)
			}
		})
	}
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
