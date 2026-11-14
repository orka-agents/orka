package aitools

import (
	"slices"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestSubstrateMCPEgressCredentialSecretNamesAuthorizesEachSecretOnce(t *testing.T) {
	credential := func(name, key string) *corev1alpha1.SubstrateActorEgressCredential {
		return &corev1alpha1.SubstrateActorEgressCredential{SecretRef: corev1alpha1.SecretKeySelector{Name: name, Key: key}}
	}
	tool := &corev1alpha1.Tool{Spec: corev1alpha1.ToolSpec{MCP: &corev1alpha1.MCPToolServer{SubstrateActor: &corev1alpha1.SubstrateMCPActor{
		Egress: []corev1alpha1.SubstrateActorEgressRule{
			{Host: "a.example.com", Credential: credential("shared", "token")},
			{Host: "b.example.com"},
			{Host: "c.example.com", Credential: credential(" shared ", "other")},
			{Host: "d.example.com", Credential: credential("second", "token")},
			{Host: "e.example.com", Credential: credential("shared", "token")},
		},
	}}}}
	if got, want := SubstrateMCPEgressCredentialSecretNames(tool), []string{"shared", "second"}; !slices.Equal(got, want) {
		t.Fatalf("secret names = %q, want %q (each Secret once, in rule order)", got, want)
	}
	if got := SubstrateMCPEgressCredentialSecretNames(nil); got != nil {
		t.Fatalf("nil tool = %q", got)
	}
}
