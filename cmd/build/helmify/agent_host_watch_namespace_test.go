package main

import (
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/yaml"
)

func TestSplitNamespaceAgentHostAdmissionUsesWatchedNamespace(t *testing.T) {
	args := append(agentHostsEnabled(), "--set-string", "controller.watchNamespace=orka-tasks")
	rendered := requireHelmRender(t, append(args, "--show-only", "templates/controller-validating-webhook.yaml")...)
	var configuration admissionv1.ValidatingWebhookConfiguration
	require.NoError(t, yaml.Unmarshal([]byte(rendered), &configuration))
	found := false
	for _, webhook := range configuration.Webhooks {
		if webhook.Name != "agenthost.harness-v2.orka.ai" {
			continue
		}
		found = true
		require.NotNil(t, webhook.NamespaceSelector)
		require.Equal(t, "orka-tasks", webhook.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
		require.NotNil(t, webhook.FailurePolicy)
		require.Equal(t, admissionv1.Fail, *webhook.FailurePolicy)
		require.NotNil(t, webhook.ClientConfig.Service)
		require.Equal(t, staticChartTestNamespace, webhook.ClientConfig.Service.Namespace)
	}
	require.True(t, found, "AgentHost admission webhook must remain enabled")
	rendered = requireHelmRender(t, append(args, "--show-only", "templates/workspace-class-use-policy.yaml")...)
	var policy admissionv1.ValidatingAdmissionPolicy
	require.NoError(t, yaml.Unmarshal([]byte(requireRenderedObject(t, rendered,
		"ValidatingAdmissionPolicy", "test-orka-agenthost-template-use", "")), &policy))
	require.NotNil(t, policy.Spec.FailurePolicy)
	require.Equal(t, admissionv1.Fail, *policy.Spec.FailurePolicy)
	require.NotNil(t, policy.Spec.MatchConstraints.NamespaceSelector)
	require.Equal(t, "orka-tasks",
		policy.Spec.MatchConstraints.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
	var binding admissionv1.ValidatingAdmissionPolicyBinding
	require.NoError(t, yaml.Unmarshal([]byte(requireRenderedObject(t, rendered,
		"ValidatingAdmissionPolicyBinding", policy.Name, "")), &binding))
	require.Equal(t, policy.Name, binding.Spec.PolicyName)
	require.Equal(t, []admissionv1.ValidationAction{admissionv1.Deny}, binding.Spec.ValidationActions)
}
