// Command profile renders the immutable inputs for one approval demo runtime.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func main() {
	provider := flag.String("provider", "", "agentkit or foundry")
	adapter := flag.String("adapter-digest", "", "digest of the source runtime image before supervisor composition")
	config := flag.String("config", "", "exact baked /agent/agent.yaml or /agent/foundry.json")
	flag.Parse()
	if err := run(*provider, *adapter, *config); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(provider, adapter, configPath string) error {
	if provider != "agentkit" && provider != "foundry" {
		return fmt.Errorf("--provider must be agentkit or foundry")
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read the exact baked configuration: %w", err)
	}
	var configuration map[string]any
	if err := yaml.UnmarshalStrict(raw, &configuration); err != nil {
		return fmt.Errorf("decode baked configuration: %w", err)
	}
	var model string
	if provider == "agentkit" {
		if configuration["abiVersion"] != "v0" {
			return fmt.Errorf("use the baked v0 agent.yaml, not the Agentkitfile build input")
		}
		value, _ := configuration["model"].(map[string]any)
		model, _ = value["name"].(string)
		for _, field := range []string{"tools", "brokeredTools"} {
			if values, _ := configuration[field].([]any); len(values) != 0 {
				return fmt.Errorf("direct AgentKit configuration must not contain %s", field)
			}
		}
	} else {
		model, _ = configuration["model"].(string)
		if configuration["toolSchemaMode"] != "provider-static" {
			return fmt.Errorf("foundry configuration must use toolSchemaMode provider-static")
		}
	}
	allowed, disallowed := []string{"create-work-order", "read-inventory"}, []string{}
	approval := harnessv2.MCPApprovalPolicy{RequiredTools: []string{"create-work-order"}}
	toolDigest, err := harnessv2.CanonicalRuntimeToolPolicyDigest(allowed, disallowed, false)
	if err != nil {
		return err
	}
	approvalDigest, err := harnessv2.CanonicalMCPApprovalPolicyDigest(approval)
	if err != nil {
		return err
	}
	mcpDigest, err := harnessv2.CanonicalMCPConfigurationDigest(allowed)
	if err != nil {
		return err
	}
	configHash := sha256.Sum256(raw)
	configDigest := "sha256:" + hex.EncodeToString(configHash[:])
	adapterName := provider + "-serve-acp"
	profile := harnessv2.RuntimeProfile{
		ACPProfile: harnessv2.ACPProfileV1, AdapterDigests: map[string]string{adapterName: adapter},
		ProviderKind: provider, Model: model, AgentConfigurationDigest: configDigest,
		ToolPolicyDigest: toolDigest, ApprovalPolicyDigest: approvalDigest, MCPConfigurationDigest: mcpDigest,
		WorkspaceIntent: harnessv2.WorkspaceIntentRead, ProxyCredentialRole: "operator-managed",
		ProxyCredentialScope: "external-runtime", ResourceClass: "external",
	}
	if err := profile.Validate(); err != nil {
		return err
	}
	digest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		return err
	}
	output := struct {
		Profile       corev1alpha1.AgentRuntimeProfileSpec   `json:"profile"`
		MCPPolicy     corev1alpha1.AgentRuntimeMCPPolicySpec `json:"mcpPolicy"`
		SupervisorEnv map[string]string                      `json:"supervisorEnv"`
	}{
		Profile: corev1alpha1.AgentRuntimeProfileSpec{
			Digest: string(digest), DigestSchemaVersion: int32(harnessv2.ProfileDigestSchemaVersion),
			ACPProfile: profile.ACPProfile, AdapterName: adapterName, AdapterDigest: adapter,
			ProviderKind: provider, Model: model, AgentConfigurationDigest: configDigest,
			ToolPolicyDigest: toolDigest, ApprovalPolicyDigest: approvalDigest, MCPConfigurationDigest: mcpDigest,
			WorkspaceIntent:      corev1alpha1.WorkspaceIntentRead,
			ProxyCredentialRole:  profile.ProxyCredentialRole,
			ProxyCredentialScope: profile.ProxyCredentialScope, ResourceClass: profile.ResourceClass,
		},
		MCPPolicy: corev1alpha1.AgentRuntimeMCPPolicySpec{
			AllowedTools: allowed, DisallowedTools: disallowed, ApprovalRequiredTools: approval.RequiredTools,
		},
		SupervisorEnv: map[string]string{
			"ORKA_ACP_PROVIDER":         provider,
			"ORKA_ACP_MODEL":            model,
			"ORKA_ACP_WORKSPACE_INTENT": "read",
			"ORKA_ACP_" + strings.ToUpper(provider) + "_ADAPTER_DIGEST": adapter,
			"ORKA_ACP_AGENT_CONFIGURATION_DIGEST":                       configDigest,
			"ORKA_ACP_TOOL_POLICY_DIGEST":                               toolDigest,
			"ORKA_ACP_APPROVAL_POLICY_DIGEST":                           approvalDigest,
			"ORKA_ACP_MCP_CONFIGURATION_DIGEST":                         mcpDigest,
			"ORKA_ACP_PROXY_CREDENTIAL_ROLE":                            profile.ProxyCredentialRole,
			"ORKA_ACP_PROXY_CREDENTIAL_SCOPE":                           profile.ProxyCredentialScope,
			"ORKA_ACP_RESOURCE_CLASS":                                   profile.ResourceClass,
		},
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(output)
}
