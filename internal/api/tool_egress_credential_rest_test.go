package api

import (
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestToolRESTRejectsSubstrateEgressCredentials(t *testing.T) {
	spec := corev1alpha1.ToolSpec{MCP: &corev1alpha1.MCPToolServer{SubstrateActor: &corev1alpha1.SubstrateMCPActor{
		Egress: []corev1alpha1.SubstrateActorEgressRule{{Scheme: corev1alpha1.SubstrateActorEgressSchemeHTTPS, Host: "api.github.com",
			Credential: &corev1alpha1.SubstrateActorEgressCredential{SecretRef: corev1alpha1.SecretKeySelector{Name: "github", Key: "token"}}}},
	}}}
	if err := validateToolRESTMutation(spec); err == nil {
		t.Fatal("REST API accepted an egress credential Secret reference")
	}
	if !toolSpecHasProtectedAuth(spec) {
		t.Fatal("a Tool with an egress credential is not protected from REST updates")
	}
	spec.MCP.SubstrateActor.Egress[0].Credential = nil
	if err := validateToolRESTMutation(spec); err != nil || toolSpecHasProtectedAuth(spec) {
		t.Fatalf("plain egress rule was treated as protected: %v", err)
	}
}
