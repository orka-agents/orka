package supervisor

import (
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

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

	providerKindAgentKit       = "agentkit"
	EnvAgentKitAdapterDigest   = "ORKA_ACP_AGENTKIT_ADAPTER_DIGEST"
	agentKitAdapterName        = "agentkit-serve-acp"
	agentKitConfigPath         = "/agent/agent.yaml"
	agentKitProviderBaseURLEnv = "AGENTKIT_ACP_PROVIDER_BASE_URL"
	agentKitProviderTokenEnv   = "AGENTKIT_ACP_PROVIDER_TOKEN"
	agentKitModelEnv           = "AGENTKIT_ACP_MODEL"
	agentKitConfigDigestEnv    = "AGENTKIT_ACP_AGENT_CONFIGURATION_DIGEST"
	agentKitMCPTimeoutEnv      = "AGENTKIT_MCP_TIMEOUT"
)

// externalACPAdapter describes an out-of-tree ACP adapter that the supervisor
// launches with a frozen command line, a digest-pinned adapter identity, and a
// fixed provider environment contract. Every such adapter shares one
// profile, projection, and digest implementation; only the table differs.
type externalACPAdapter struct {
	kind        string
	digestEnv   string
	adapterName string
	command     string
	args        []string
	// label prefixes every user-visible error message.
	label string

	baseURLEnv      string
	tokenEnv        string
	modelEnv        string
	configDigestEnv string
}

var (
	foundryAdapter = externalACPAdapter{
		kind: providerKindFoundry, digestEnv: EnvFoundryAdapterDigest, adapterName: foundryAdapterName,
		command:    "/agent-runtime-foundry",
		args:       []string{"--protocol", acpCommandProtocol, "--config", foundryConfigPath},
		label:      "external Foundry",
		baseURLEnv: foundryProviderBaseURLEnv, tokenEnv: foundryProviderTokenEnv,
		modelEnv: foundryModelEnv, configDigestEnv: foundryConfigDigestEnv,
	}
	agentKitAdapter = externalACPAdapter{
		kind: providerKindAgentKit, digestEnv: EnvAgentKitAdapterDigest, adapterName: agentKitAdapterName,
		command:    "/opt/agentkit/bin/agentkit-serve",
		args:       []string{"--config", agentKitConfigPath, "--protocol", acpCommandProtocol},
		label:      "AgentKit",
		baseURLEnv: agentKitProviderBaseURLEnv, tokenEnv: agentKitProviderTokenEnv,
		modelEnv: agentKitModelEnv, configDigestEnv: agentKitConfigDigestEnv,
	}
)

func externalACPAdapterFor(kind string) (externalACPAdapter, bool) {
	switch kind {
	case providerKindFoundry:
		return foundryAdapter, true
	case providerKindAgentKit:
		return agentKitAdapter, true
	default:
		return externalACPAdapter{}, false
	}
}

func isExternalACPProvider(kind string) bool {
	_, ok := externalACPAdapterFor(kind)
	return ok
}

func (a externalACPAdapter) profile(model string) (ProviderProfile, error) {
	adapterDigest, err := a.digestFromEnv()
	if err != nil {
		return ProviderProfile{}, err
	}
	return ProviderProfile{
		Kind: a.kind, Model: model,
		Command:     a.command,
		Args:        slices.Clone(a.args),
		AdapterName: a.adapterName, AdapterDigest: adapterDigest,
		ProjectSession: func(request harnessv2.CreateRuntimeSessionRequest, _ acp.SessionPaths, _ ProviderProxyBinding) (ProviderSessionProjection, error) {
			return a.sessionProjection(request, model)
		},
		EnvironmentForSession: func(request harnessv2.CreateRuntimeSessionRequest, _ acp.SessionPaths, proxy ProviderProxyBinding) (map[string]string, error) {
			environment := map[string]string{
				a.baseURLEnv:      proxy.BaseURL,
				a.tokenEnv:        proxy.Credential,
				a.modelEnv:        model,
				a.configDigestEnv: requiredEnv(EnvAgentConfigurationDigest),
			}
			if a.kind == providerKindAgentKit && len(request.MCPConfiguration.ApprovalPolicy.RequiredTools) > 0 {
				environment[agentKitMCPTimeoutEnv] = strconv.Itoa(int(harnessv2.MCPApprovalCallTimeout / time.Second))
			}
			return environment, nil
		},
	}, nil
}

func (a externalACPAdapter) sessionProjection(request harnessv2.CreateRuntimeSessionRequest, model string) (ProviderSessionProjection, error) {
	if request.AgentConfiguration != nil {
		return ProviderSessionProjection{}, fmt.Errorf("%s ACP runtime does not support per-Task AgentConfiguration", a.label)
	}
	if request.Profile.ProviderKind != a.kind || request.Profile.Model != model {
		return ProviderSessionProjection{}, fmt.Errorf("%s session configuration does not match runtime profile", a.label)
	}
	if err := request.MCPConfiguration.ValidateProfile(request.Profile); err != nil {
		return ProviderSessionProjection{}, fmt.Errorf("%s MCP policy configuration: %w", a.label, err)
	}
	for _, descriptor := range request.MCPConfiguration.ToolPolicy.Tools {
		if !descriptor.Source.Brokered() {
			return ProviderSessionProjection{}, fmt.Errorf("%s ACP runtime forbids provider-native tool %q; tools must use the Orka session MCP server", a.label, descriptor.Name)
		}
	}
	return ProviderSessionProjection{}, nil
}

func (a externalACPAdapter) digestFromEnv() (string, error) {
	digest := strings.TrimSpace(os.Getenv(a.digestEnv))
	encoded, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(encoded) != 64 || encoded != strings.ToLower(encoded) {
		return "", fmt.Errorf("%s must be a sha256 digest", a.digestEnv)
	}
	if _, err := hex.DecodeString(encoded); err != nil {
		return "", fmt.Errorf("%s must be a sha256 digest", a.digestEnv)
	}
	return digest, nil
}

// adapterDigests returns nil when the digest is missing or malformed so the
// supervisor keeps its default-nil unknown-provider behavior.
func (a externalACPAdapter) adapterDigests() map[string]string {
	digest, err := a.digestFromEnv()
	if err != nil {
		return nil
	}
	return map[string]string{a.adapterName: digest}
}

func providerCapabilities(provider, model string) harnessv2.ProviderCapabilities {
	capabilities := harnessv2.ProviderCapabilities{
		ProviderKinds:             []string{provider},
		Models:                    []string{model},
		SupportsPermissions:       true,
		SupportsCancel:            true,
		SupportsTools:             true,
		SupportsImages:            true,
		SupportsEmbeddedResources: true,
	}
	if isExternalACPProvider(provider) {
		// These ACP adapters do not emit session/request_permission or
		// support rich prompt content.
		capabilities.SupportsPermissions = false
		capabilities.SupportsImages = false
		capabilities.SupportsAudio = false
		capabilities.SupportsEmbeddedResources = false
	}
	return capabilities
}
