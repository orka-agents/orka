package aitools

import (
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func SubstrateMCPEgressCredentialSecretNames(tool *corev1alpha1.Tool) []string {
	if tool == nil || tool.Spec.MCP == nil || tool.Spec.MCP.SubstrateActor == nil {
		return nil
	}
	var names []string
	seen := map[string]bool{}
	for _, rule := range tool.Spec.MCP.SubstrateActor.Egress {
		if rule.Credential == nil {
			continue
		}
		name := strings.TrimSpace(rule.Credential.SecretRef.Name)
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}
