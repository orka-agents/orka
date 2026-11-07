package admission

import (
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// TestTaskProvenanceValidatorKeepsWorkersFromRootTasksForIdentityAgents covers
// a worker, which holds Agent use broadly, creating a Task with no parent:
// it may not name an identity-bound Agent, which would otherwise bind as a
// root Task no use check covered. Other Agents and non-worker callers are
// unaffected.
func TestTaskProvenanceValidatorKeepsWorkersFromRootTasksForIdentityAgents(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	identityAgent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: admissionTestNamespace},
		Spec: corev1alpha1.AgentSpec{Identity: &corev1alpha1.AgentIdentity{
			Entra: &corev1alpha1.EntraAgentIdentity{AgentIdentityID: "33333333-3333-3333-3333-33333333aaaa"},
		}},
	}
	plainAgent := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "helper", Namespace: admissionTestNamespace}}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(identityAgent, plainAgent).Build()
	validator := NewTaskProvenanceValidator(scheme, NewTaskProvenanceConfig(true, "", "", "", "orka-system"), reader)
	task := func(agent string) *corev1alpha1.Task {
		created := newAdmissionTestTask()
		created.Spec.Type = corev1alpha1.TaskTypeAgent
		created.Spec.AgentRef = &corev1alpha1.AgentReference{Name: agent}
		return created
	}

	resp := validator.Handle(t.Context(), admissionRequest(t, admissionv1.Create, trustedWorkerUser, task("reviewer"), nil, ""))
	require.False(t, resp.Allowed, "a worker's parentless Task must not name an identity-bound Agent")
	require.Contains(t, resp.Result.Message, "identity-bound Agent")

	resp = validator.Handle(t.Context(), admissionRequest(t, admissionv1.Create, trustedWorkerUser, task("helper"), nil, ""))
	require.True(t, resp.Allowed, "a worker's parentless Task may name another Agent: %s", resp.Result.Message)

	resp = validator.Handle(t.Context(), admissionRequest(t, admissionv1.Create, untrustedUsername, task("reviewer"), nil, ""))
	require.True(t, resp.Allowed, "a non-worker caller is judged by the Agent use check, not here: %s", resp.Result.Message)
}

// TestWorkerRuleIgnoresTheProvenanceTrustOverride covers an operator who
// overrides the trusted worker ServiceAccounts and leaves a real worker out:
// that worker, still holding Agent use, is held to the rule all the same.
func TestWorkerRuleIgnoresTheProvenanceTrustOverride(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	identityAgent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: admissionTestNamespace},
		Spec: corev1alpha1.AgentSpec{Identity: &corev1alpha1.AgentIdentity{
			Entra: &corev1alpha1.EntraAgentIdentity{AgentIdentityID: "33333333-3333-3333-3333-33333333aaaa"},
		}},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(identityAgent).Build()
	config := NewTaskProvenanceConfig(true, "", "", "some-other-sa", "orka-system")
	require.NotContains(t, config.TrustedServiceAccountNames, "orka-ai-worker")
	require.Contains(t, config.WorkerServiceAccountNames, "orka-ai-worker")
	validator := NewTaskProvenanceValidator(scheme, config, reader)
	task := newAdmissionTestTask()
	task.Spec.Type = corev1alpha1.TaskTypeAgent
	task.Spec.AgentRef = &corev1alpha1.AgentReference{Name: "reviewer"}
	resp := validator.Handle(t.Context(), admissionRequest(t, admissionv1.Create, trustedWorkerUser, task, nil, ""))
	require.False(t, resp.Allowed, "an untrusted worker's parentless Task must not name an identity-bound Agent")
	require.Contains(t, resp.Result.Message, "identity-bound Agent")
	require.Equal(t, []string{"orka-ai-worker", "orka-vendor-worker", "custom-worker"},
		WorkerServiceAccountNames([]string{"custom-worker", "orka-ai-worker", " "}))
}
