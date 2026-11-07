// Command profile renders the immutable AgentRuntime registration inputs for
// one wrapped kagent Agent: the v2 profile, its MCP policy, and the matching
// supervisor environment. Observed mode brokers no Orka tools; governed mode
// lists the Orka tools the Agent calls through the wrapper's relay.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const (
	providerKind = "kagent"
	adapterName  = "kagent-serve-acp"
)

func main() {
	adapter := flag.String("adapter-digest", "",
		"digest of the configured kagent source image before supervisor composition")
	configPath := flag.String("config", "", "exact baked /agent/kagent.json")
	allowedTools := flag.String("allowed-tools", "", "comma-separated Orka tools a governed Agent may call")
	approvalTools := flag.String("approval-required-tools", "", "comma-separated allowed tools that need an Orka approval")
	flag.Parse()
	if err := run(*adapter, *configPath, toolList(*allowedTools), toolList(*approvalTools)); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func toolList(value string) []string {
	tools := []string{}
	for tool := range strings.SplitSeq(value, ",") {
		if tool = strings.TrimSpace(tool); tool != "" {
			tools = append(tools, tool)
		}
	}
	slices.Sort(tools)
	return slices.Compact(tools)
}

func run(adapter, configPath string, allowed, approvalRequired []string) error {
	encoded, ok := strings.CutPrefix(adapter, "sha256:")
	if !ok || len(encoded) != 64 || strings.ToLower(encoded) != encoded {
		return fmt.Errorf("--adapter-digest must be the configured image's sha256 digest")
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read the exact baked configuration: %w", err)
	}
	var configuration struct {
		Model    string          `json:"model"`
		Governed json.RawMessage `json:"governed"`
	}
	if err := json.Unmarshal(raw, &configuration); err != nil || configuration.Model == "" {
		return fmt.Errorf("kagent configuration must name its profile model")
	}
	// A governed Agent may also have no tools: Orka then serves only its model.
	// The wrapper reads a null governed stanza as observed mode.
	governed := bytes.TrimSpace(configuration.Governed)
	if len(allowed) != 0 && (len(governed) == 0 || governed[0] != '{') {
		return fmt.Errorf("only a governed kagent configuration may list Orka tools")
	}
	for _, tool := range approvalRequired {
		if !slices.Contains(allowed, tool) {
			return fmt.Errorf("approval-required tool %q is not allowed", tool)
		}
	}
	// The CRD lists stay explicit, while the approval digest uses the empty
	// policy the controller derives from an empty approvalRequiredTools list.
	disallowed := []string{}
	approval := harnessv2.MCPApprovalPolicy{}
	if len(approvalRequired) != 0 {
		approval.RequiredTools = approvalRequired
	}
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
	sum := sha256.Sum256(raw)
	configDigest := "sha256:" + hex.EncodeToString(sum[:])
	profile := harnessv2.RuntimeProfile{
		ACPProfile: harnessv2.ACPProfileV1, AdapterDigests: map[string]string{adapterName: adapter},
		ProviderKind: providerKind, Model: configuration.Model, AgentConfigurationDigest: configDigest,
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
			ProviderKind: providerKind, Model: configuration.Model, AgentConfigurationDigest: configDigest,
			ToolPolicyDigest: toolDigest, ApprovalPolicyDigest: approvalDigest, MCPConfigurationDigest: mcpDigest,
			WorkspaceIntent: corev1alpha1.WorkspaceIntentRead, ProxyCredentialRole: profile.ProxyCredentialRole,
			ProxyCredentialScope: profile.ProxyCredentialScope, ResourceClass: profile.ResourceClass,
		},
		MCPPolicy: corev1alpha1.AgentRuntimeMCPPolicySpec{
			AllowedTools: allowed, DisallowedTools: disallowed, ApprovalRequiredTools: approvalRequired,
		},
		SupervisorEnv: map[string]string{
			"ORKA_ACP_PROVIDER":                   providerKind,
			"ORKA_ACP_MODEL":                      configuration.Model,
			"ORKA_ACP_WORKSPACE_INTENT":           "read",
			"ORKA_ACP_KAGENT_ADAPTER_DIGEST":      adapter,
			"ORKA_ACP_AGENT_CONFIGURATION_DIGEST": configDigest,
			"ORKA_ACP_TOOL_POLICY_DIGEST":         toolDigest,
			"ORKA_ACP_APPROVAL_POLICY_DIGEST":     approvalDigest,
			"ORKA_ACP_MCP_CONFIGURATION_DIGEST":   mcpDigest,
			"ORKA_ACP_PROXY_CREDENTIAL_ROLE":      profile.ProxyCredentialRole,
			"ORKA_ACP_PROXY_CREDENTIAL_SCOPE":     profile.ProxyCredentialScope,
			"ORKA_ACP_RESOURCE_CLASS":             profile.ResourceClass,
			"ORKA_ACP_PROVIDER_PROXY_BASE_URL":    "http://127.0.0.1:8091",
			// The kagent broker serves the identity and retire-boot recovery
			// endpoints, so the supervisor can retire a restarted boot.
			"ORKA_ACP_FOUNDRY_RECOVERY_PROFILE_DIGEST": string(digest),
		},
	}
	if len(approvalRequired) != 0 {
		// Approval-gated tools need the supervisor qualified for this exact profile.
		output.SupervisorEnv["ORKA_ACP_BROKERED_TOOL_APPROVAL_PROFILE_DIGEST"] = string(digest)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(output)
}
