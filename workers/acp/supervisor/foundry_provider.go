package supervisor

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const (
	providerKindFoundry       = "foundry"
	EnvFoundryAdapterDigest   = "ORKA_ACP_FOUNDRY_ADAPTER_DIGEST"
	foundryAdapterName        = "foundry-serve-acp"
	foundryConfigPath         = "/agent/foundry.json"
	foundryProviderBaseURLEnv = "ORKA_FOUNDRY_ACP_PROVIDER_BASE_URL"
	foundryProviderTokenEnv   = "ORKA_FOUNDRY_ACP_PROVIDER_TOKEN"
	foundryModelEnv           = "ORKA_FOUNDRY_ACP_MODEL"
	foundryConfigDigestEnv    = "ORKA_FOUNDRY_ACP_AGENT_CONFIGURATION_DIGEST"
)

func foundryProviderProfile(model string) (ProviderProfile, error) {
	adapterDigest, err := foundryAdapterDigestFromEnv()
	if err != nil {
		return ProviderProfile{}, err
	}
	return ProviderProfile{
		Kind: providerKindFoundry, Model: model,
		Command:     "/agent-runtime-foundry",
		Args:        []string{"--protocol", acpCommandProtocol, "--config", foundryConfigPath},
		AdapterName: foundryAdapterName, AdapterDigest: adapterDigest,
		ProjectSession: func(request harnessv2.CreateRuntimeSessionRequest, _ acp.SessionPaths, _ ProviderProxyBinding) (ProviderSessionProjection, error) {
			return foundrySessionProjection(request, model)
		},
		EnvironmentForSession: func(_ harnessv2.CreateRuntimeSessionRequest, _ acp.SessionPaths, proxy ProviderProxyBinding) (map[string]string, error) {
			return map[string]string{
				foundryProviderBaseURLEnv: proxy.BaseURL,
				foundryProviderTokenEnv:   proxy.Credential,
				foundryModelEnv:           model,
				foundryConfigDigestEnv:    requiredEnv(EnvAgentConfigurationDigest),
			}, nil
		},
	}, nil
}

func foundrySessionProjection(request harnessv2.CreateRuntimeSessionRequest, model string) (ProviderSessionProjection, error) {
	if request.AgentConfiguration != nil {
		return ProviderSessionProjection{}, fmt.Errorf("external Foundry ACP runtime does not support per-Task AgentConfiguration")
	}
	if request.Profile.ProviderKind != providerKindFoundry || request.Profile.Model != model {
		return ProviderSessionProjection{}, fmt.Errorf("external Foundry session configuration does not match runtime profile")
	}
	if err := request.MCPConfiguration.ValidateProfile(request.Profile); err != nil {
		return ProviderSessionProjection{}, fmt.Errorf("external Foundry MCP policy configuration: %w", err)
	}
	if len(request.MCPConfiguration.ApprovalPolicy.RequiredTools) != 0 {
		return ProviderSessionProjection{}, fmt.Errorf("external Foundry ACP runtime does not support approval-required MCP tools")
	}
	for _, descriptor := range request.MCPConfiguration.ToolPolicy.Tools {
		if !descriptor.Source.Brokered() {
			return ProviderSessionProjection{}, fmt.Errorf("external Foundry ACP runtime forbids provider-native tool %q; tools must use the Orka session MCP server", descriptor.Name)
		}
	}
	return ProviderSessionProjection{}, nil
}

func foundryAdapterDigestFromEnv() (string, error) {
	digest := strings.TrimSpace(os.Getenv(EnvFoundryAdapterDigest))
	encoded, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(encoded) != 64 || encoded != strings.ToLower(encoded) {
		return "", fmt.Errorf("%s must be a sha256 digest", EnvFoundryAdapterDigest)
	}
	if _, err := hex.DecodeString(encoded); err != nil {
		return "", fmt.Errorf("%s must be a sha256 digest", EnvFoundryAdapterDigest)
	}
	return digest, nil
}
