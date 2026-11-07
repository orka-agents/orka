package aitools

import (
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// SubstrateMCPEgressCredentialSecretNames returns the Tool-namespace Secrets
// whose values the Substrate egress gateway injects into a Substrate MCP
// actor's outbound requests. Invoking such a Tool uses those credentials, so
// transaction authority must cover them exactly like spec.http.authSecretRef.
func SubstrateMCPEgressCredentialSecretNames(tool *corev1alpha1.Tool) []string {
	if tool == nil || tool.Spec.MCP == nil || tool.Spec.MCP.SubstrateActor == nil {
		return nil
	}
	var names []string
	for _, rule := range tool.Spec.MCP.SubstrateActor.Egress {
		if rule.Credential != nil {
			names = append(names, strings.TrimSpace(rule.Credential.SecretRef.Name))
		}
	}
	return names
}
