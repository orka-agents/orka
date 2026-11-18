/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/executionmode"
)

// The controller writes the guide Agent itself, so the Agent it builds must
// pass the Agent contract webhook when created and when drift is reverted.
func TestAgentContractValidatorAdmitsGuideAgent(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "orka-system",
		Labels: map[string]string{executionmode.NamespaceLabel: string(executionmode.HarnessV2)},
	}}
	validator := &AgentContractValidator{
		decoder: ctrladmission.NewDecoder(scheme),
		reader:  fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace).Build(),
	}
	raw := func(agent *corev1alpha1.Agent) runtime.RawExtension {
		data, err := json.Marshal(agent)
		require.NoError(t, err)
		return runtime.RawExtension{Raw: data}
	}

	for _, runtimeType := range controller.GuideRuntimeTypes {
		t.Run(string(runtimeType), func(t *testing.T) {
			agent := controller.GuideAgentConfig{
				Enabled:         true,
				Namespace:       namespace.Name,
				Runtime:         runtimeType,
				Model:           "test-model",
				MaxTurns:        controller.DefaultGuideMaxTurns,
				DocsImage:       "ghcr.io/orka-agents/orka/guide-docs@sha256:" + strings.Repeat("a", 64),
				ContractVersion: executionmode.HarnessV2.ContractVersion(),
			}.DesiredAgent()
			agent.APIVersion = corev1alpha1.GroupVersion.String()
			agent.Kind = "Agent"

			edited := agent.DeepCopy()
			edited.Spec.Runtime.DefaultAllowBash = new(true)
			for _, req := range []admissionv1.AdmissionRequest{
				{Operation: admissionv1.Create, Namespace: namespace.Name, Object: raw(agent)},
				{Operation: admissionv1.Update, Namespace: namespace.Name, Object: raw(agent), OldObject: raw(edited)},
			} {
				response := validator.Handle(context.Background(), ctrladmission.Request{AdmissionRequest: req})
				require.True(t, response.Allowed, "%s: %s", req.Operation, response.Result.Message)
			}
		})
	}
}
