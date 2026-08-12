package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
)

type ACPRuntimeImages struct {
	Codex    string
	Claude   string
	Copilot  string
	Opencode string
}

type ACPRuntimePlan struct {
	PoolName string
	Image    string
	Profile  harnessv2.RuntimeProfile
	Digest   harnessv2.ProfileDigest
}

// validateACPRuntimePlanningAgent gates ACP planning on a complete built-in
// runtime with an explicit orka.harness.v2 classification and a valid v2
// OpenCode shape. A missing selector is never protocol evidence.
func validateACPRuntimePlanningAgent(task *corev1alpha1.Task, agent *corev1alpha1.Agent) error {
	if task == nil || agent == nil || agent.Spec.Runtime == nil || agent.Spec.Runtime.Type == "" {
		return fmt.Errorf("built-in agent runtime is required")
	}
	if agent.BuiltInContractVersion() != corev1alpha1.AgentRuntimeContractHarnessV2 {
		return fmt.Errorf("ACP runtime planning requires an Agent explicitly classified %s; a missing runtime.contractVersion selector is never protocol evidence", corev1alpha1.AgentRuntimeContractHarnessV2)
	}
	return ValidateOpenCodeAgentSpec(agent)
}

func PlanACPRuntime(task *corev1alpha1.Task, agent *corev1alpha1.Agent, images ACPRuntimeImages) (ACPRuntimePlan, error) {
	if err := ValidateOpenCodeAgentSpec(agent); err != nil {
		return ACPRuntimePlan{}, err
	}
	if agent != nil && agent.Spec.SystemPrompt != nil && agent.Spec.SystemPrompt.ConfigMapRef != nil {
		return ACPRuntimePlan{}, fmt.Errorf("PlanACPRuntime requires a resolved ConfigMap-backed Agent systemPrompt")
	}
	systemPrompt := ""
	if agent != nil && agent.Spec.SystemPrompt != nil {
		systemPrompt = agent.Spec.SystemPrompt.Inline
	}
	configuration, err := buildACPAgentSessionConfiguration(task, agent, systemPrompt)
	if err != nil {
		return ACPRuntimePlan{}, err
	}
	return PlanACPRuntimeWithConfiguration(task, agent, images, configuration)
}

func PlanACPRuntimeWithConfiguration(
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
	images ACPRuntimeImages,
	configuration harnessv2.AgentSessionConfiguration,
) (ACPRuntimePlan, error) {
	if err := validateACPRuntimePlanningAgent(task, agent); err != nil {
		return ACPRuntimePlan{}, err
	}
	provider := string(agent.Spec.Runtime.Type)
	model := ""
	if agent.Spec.Model != nil {
		model = strings.TrimSpace(agent.Spec.Model.Name)
	}
	if model == "" {
		return ACPRuntimePlan{}, fmt.Errorf("ACP runtime requires an explicit model")
	}
	if err := configuration.Validate(); err != nil {
		return ACPRuntimePlan{}, fmt.Errorf("ACP Agent session configuration: %w", err)
	}
	if configuration.ProviderKind != provider || configuration.Model != model ||
		configuration.MaxTurns != effectiveACPMaxTurns(task, agent) ||
		configuration.ReasoningEffort != effectiveACPReasoningEffort(agent) ||
		configuration.AgentUID != string(agent.UID) || configuration.AgentGeneration != agent.Generation {
		return ACPRuntimePlan{}, fmt.Errorf("resolved ACP Agent session configuration does not match Task and Agent")
	}
	intent := effectiveACPWorkspaceIntent(task)
	adapterDigests, image, err := acpRuntimeArtifacts(agent.Spec.Runtime.Type, images)
	if err != nil {
		return ACPRuntimePlan{}, err
	}
	if !ACPRuntimeImageAvailable(image) {
		return ACPRuntimePlan{}, fmt.Errorf("ACP runtime image for %s must be a configured digest-pinned image", provider)
	}
	allowed := effectiveACPAllowedTools(task, agent)
	disallowed := []string(nil)
	if task.Spec.AgentRuntime != nil {
		disallowed = sortedUnique(task.Spec.AgentRuntime.DisallowedTools)
	}
	allowBash := effectiveACPAllowBash(task, agent)
	allowed, disallowed, allowBash = normalizeACPRuntimeToolPolicy(provider, intent, allowed, disallowed, allowBash)
	if err := validateACPProviderNativePolicy(provider, intent, allowed, disallowed, allowBash); err != nil {
		return ACPRuntimePlan{}, err
	}
	if err := validateACPProviderSystemPrompt(provider, configuration); err != nil {
		return ACPRuntimePlan{}, err
	}
	for _, name := range allowed {
		if _, localOnly := controllerLocalOnlyTools[name]; localOnly {
			return ACPRuntimePlan{}, fmt.Errorf("built-in tool %q is local-process-only and cannot be exposed through the controller MCP broker", name)
		}
	}
	var modelLimits *harnessv2.ModelTokenLimits
	if agent.Spec.Runtime.Type == corev1alpha1.AgentRuntimeOpencode {
		modelLimits = &harnessv2.ModelTokenLimits{
			Context: int64(*agent.Spec.Model.ContextWindow),
			Output:  int64(*agent.Spec.Model.MaxTokens),
		}
	}
	agentDigest, err := harnessv2.CanonicalAgentConfigurationDigest(configuration)
	if err != nil {
		return ACPRuntimePlan{}, err
	}
	toolDigest, err := harnessv2.CanonicalRuntimeToolPolicyDigest(allowed, disallowed, allowBash)
	if err != nil {
		return ACPRuntimePlan{}, err
	}
	approvalTools := []string(nil)
	if agent.Spec.Coordination != nil {
		approvalTools = sortedUnique(agent.Spec.Coordination.ApprovalRequiredTools)
	}
	approvalDigest, err := harnessv2.CanonicalMCPApprovalPolicyDigest(harnessv2.MCPApprovalPolicy{RequiredTools: approvalTools})
	if err != nil {
		return ACPRuntimePlan{}, err
	}
	mcpDigest, err := harnessv2.CanonicalMCPConfigurationDigest(allowed)
	if err != nil {
		return ACPRuntimePlan{}, err
	}
	profile := harnessv2.RuntimeProfile{
		ACPProfile: harnessv2.ACPProfileV1, AdapterDigests: adapterDigests, ProviderKind: provider, Model: model,
		ModelLimits: modelLimits, AgentConfigurationDigest: agentDigest, ToolPolicyDigest: toolDigest, ApprovalPolicyDigest: approvalDigest,
		MCPConfigurationDigest: mcpDigest, WorkspaceIntent: harnessv2.WorkspaceIntent(intent),
		ProxyCredentialRole: "provider-inference", ProxyCredentialScope: "model:" + model, ResourceClass: "standard",
	}
	digest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		return ACPRuntimePlan{}, err
	}
	poolIdentityDigest, err := acpDomainDigest("runtime-pool-identity", map[string]string{
		"profileDigest": string(digest), "runtimeImage": image,
	})
	if err != nil {
		return ACPRuntimePlan{}, err
	}
	return ACPRuntimePlan{
		PoolName: acpRuntimePoolName(provider, harnessv2.ProfileDigest(poolIdentityDigest)),
		Image:    image, Profile: profile, Digest: digest,
	}, nil
}

func RuntimePoolProfileFromPlan(plan ACPRuntimePlan) corev1alpha1.RuntimePoolProfileSpec {
	var modelLimits *corev1alpha1.ModelTokenLimits
	if plan.Profile.ModelLimits != nil {
		modelLimits = &corev1alpha1.ModelTokenLimits{
			Context: plan.Profile.ModelLimits.Context,
			Output:  plan.Profile.ModelLimits.Output,
		}
	}
	return corev1alpha1.RuntimePoolProfileSpec{
		ProtocolVersion: corev1alpha1.RuntimePoolProtocolHarnessV2,
		Digest:          string(plan.Digest), DigestSchemaVersion: fmt.Sprintf("%d", harnessv2.ProfileDigestSchemaVersion),
		ACPProfile: plan.Profile.ACPProfile, AdapterDigests: cloneMap(plan.Profile.AdapterDigests),
		ProviderKind: plan.Profile.ProviderKind, Model: plan.Profile.Model, ModelLimits: modelLimits,
		AgentConfigurationDigest: plan.Profile.AgentConfigurationDigest, ToolPolicyDigest: plan.Profile.ToolPolicyDigest,
		ApprovalPolicyDigest: plan.Profile.ApprovalPolicyDigest, MCPConfigurationDigest: plan.Profile.MCPConfigurationDigest,
		WorkspaceIntent:     corev1alpha1.WorkspaceIntent(plan.Profile.WorkspaceIntent),
		ProxyCredentialRole: plan.Profile.ProxyCredentialRole, ProxyCredentialScope: plan.Profile.ProxyCredentialScope,
		ResourceClass: plan.Profile.ResourceClass,
	}
}

func acpRuntimePoolBindingMatches(status *corev1alpha1.TaskExecutionStatus, pool *corev1alpha1.RuntimePool) bool {
	return status != nil && pool != nil && pool.UID != "" &&
		strings.TrimSpace(status.RuntimePoolName) == pool.Name &&
		strings.TrimSpace(status.RuntimePoolUID) == string(pool.UID)
}

func effectiveACPWorkspaceIntent(task *corev1alpha1.Task) corev1alpha1.WorkspaceIntent {
	if task == nil {
		return corev1alpha1.WorkspaceIntentRead
	}
	workspace := task.Spec.Workspace
	if workspace != nil && workspace.Intent != "" {
		return workspace.Intent
	}
	return corev1alpha1.WorkspaceIntentRead
}

func effectiveACPAllowedTools(task *corev1alpha1.Task, agent *corev1alpha1.Agent) []string {
	var values []string
	if agent != nil && agent.Spec.Runtime != nil {
		runtime := agent.Spec.Runtime
		if runtime.Type == corev1alpha1.AgentRuntimeOpencode && runtime.DefaultAllowedTools == nil {
			values = acp.OpenCodeDefaultAllowedTools()
		} else if runtime.DefaultAllowedTools != nil {
			values = append([]string{}, runtime.DefaultAllowedTools...)
		}
	}
	if task != nil && task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.AllowedTools != nil {
		values = append([]string{}, task.Spec.AgentRuntime.AllowedTools...)
	}
	if taskRequestsReadOnlyAgent(task) && taskUsesReadOnlyAgentToolPreset(task) &&
		agent != nil && agent.Spec.Runtime != nil {
		// Repository-monitor presets use Claude-style path-scoped names, which
		// are not canonical provider-native descriptors, so translate the
		// preset into the exact read-only surface each runtime can enforce.
		switch agent.Spec.Runtime.Type {
		case corev1alpha1.AgentRuntimeOpencode:
			// OpenCode's Grep permission cannot carry the path-specific
			// secret-file exclusions applied to Read, so it stays disabled.
			values = []string{providerNativeToolRead, providerNativeToolGlob}
		case corev1alpha1.AgentRuntimeClaude:
			values = []string{providerNativeToolGlob, providerNativeToolGrep, providerNativeToolRead}
		case corev1alpha1.AgentRuntimeCodex:
			// Codex has no per-tool switches; this exact surface maps to its
			// native read-only agent mode, whose kernel-enforced sandbox
			// confines every command to reads with no network access.
			values = []string{providerNativeToolGlob, providerNativeToolGrep, providerNativeToolRead}
		}
	}
	if task != nil {
		_, delegatedChild := task.Labels[labels.LabelParentTask]
		disableCoordinationToolInjection := task.Annotations[labels.AnnotationDisableCoordinationToolInject] == scheduledRunLabelValue
		if delegatedChild && !disableCoordinationToolInjection {
			values = append(values, "send_message", "check_messages")
		}
	}
	return sortedUnique(values)
}

func taskUsesReadOnlyAgentToolPreset(task *corev1alpha1.Task) bool {
	return task != nil && task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.AllowedTools != nil &&
		slices.Equal(sortedUnique(task.Spec.AgentRuntime.AllowedTools), sortedUnique(readOnlyAgentAllowedTools()))
}

func normalizeACPRuntimeToolPolicy(
	provider string,
	intent corev1alpha1.WorkspaceIntent,
	allowed, disallowed []string,
	allowBash bool,
) ([]string, []string, bool) {
	allowed, disallowed, allowBash = normalizeACPProviderNativeToolPolicy(provider, allowed, disallowed, allowBash)
	if !strings.EqualFold(provider, string(corev1alpha1.AgentRuntimeOpencode)) {
		return allowed, disallowed, allowBash
	}
	return acp.NormalizeOpenCodeToolPolicy(intent == corev1alpha1.WorkspaceIntentRead, allowed, disallowed, allowBash)
}

func effectiveACPMaxTurns(task *corev1alpha1.Task, agent *corev1alpha1.Agent) int32 {
	if task != nil && task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.MaxTurns != nil {
		return *task.Spec.AgentRuntime.MaxTurns
	}
	if agent != nil && agent.Spec.Runtime != nil && agent.Spec.Runtime.DefaultMaxTurns != nil {
		return *agent.Spec.Runtime.DefaultMaxTurns
	}
	return 50
}

func acpRuntimeArtifacts(runtime corev1alpha1.AgentRuntimeType, images ACPRuntimeImages) (map[string]string, string, error) {
	switch runtime {
	case corev1alpha1.AgentRuntimeCodex:
		return map[string]string{
			"codex-acp":             "sha256:" + acp.CodexACPTarSHA256,
			"codex-acp-orka-patch":  "sha256:" + acp.CodexACPOrkaPatchSHA256,
			"codex-acp-orka-dist":   "sha256:" + acp.CodexACPOrkaDistSHA256,
			"codex-cli-linux-amd64": "sha256:" + acp.CodexCLILinuxX64SHA256,
			"codex-cli-linux-arm64": "sha256:" + acp.CodexCLILinuxARM64SHA256,
			"acp-schema":            "sha256:" + acp.ACPSchemaSHA256,
		}, strings.TrimSpace(images.Codex), nil
	case corev1alpha1.AgentRuntimeClaude:
		return map[string]string{
			"claude-agent-acp":        "sha256:" + acp.ClaudeACPTarSHA256,
			"claude-code-linux-amd64": "sha256:" + acp.ClaudeSDKLinuxX64SHA256,
			"claude-code-linux-arm64": "sha256:" + acp.ClaudeSDKLinuxARM64SHA256,
			"acp-schema":              "sha256:" + acp.ACPSchemaSHA256,
		}, strings.TrimSpace(images.Claude), nil
	case corev1alpha1.AgentRuntimeCopilot:
		return map[string]string{
			"copilot-cli-linux-amd64": "sha256:" + acp.CopilotCLILinuxX64SHA256,
			"copilot-cli-linux-arm64": "sha256:" + acp.CopilotCLILinuxARM64SHA256,
			"acp-schema":              "sha256:" + acp.ACPSchemaSHA256,
		}, strings.TrimSpace(images.Copilot), nil
	case corev1alpha1.AgentRuntimeOpencode:
		return map[string]string{
			"opencode-cli-linux-amd64":     "sha256:" + acp.OpenCodeLinuxX64BinarySHA256,
			"opencode-cli-linux-arm64":     "sha256:" + acp.OpenCodeLinuxARM64BinarySHA256,
			"opencode-ripgrep-linux-amd64": "sha256:" + acp.OpenCodeRipgrepLinuxX64BinarySHA256,
			"opencode-ripgrep-linux-arm64": "sha256:" + acp.OpenCodeRipgrepLinuxARM64BinarySHA256,
			"acp-schema":                   "sha256:" + acp.ACPSchemaSHA256,
		}, strings.TrimSpace(images.Opencode), nil
	default:
		return nil, "", fmt.Errorf("runtime %q is not supported by the ACP core pool", runtime)
	}
}

// ACPRuntimeImageAvailable reports whether a built-in runtime image is an
// immutable, non-placeholder reference suitable for task admission and chat.
func ACPRuntimeImageAvailable(image string) bool {
	image = strings.TrimSpace(image)
	if !digestPinnedImagePattern.MatchString(image) {
		return false
	}
	return !strings.HasSuffix(image, "@sha256:"+strings.Repeat("0", 64))
}

func acpDomainDigest(domain string, value any) (string, error) {
	canonical, err := harnessv2.CanonicalValue(value)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("orka.acp." + domain + "\x00"))
	_, _ = hash.Write(canonical)
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func acpRuntimePoolName(provider string, digest harnessv2.ProfileDigest) string {
	hexDigest := strings.TrimPrefix(string(digest), "sha256:")
	return fmt.Sprintf("acp-%s-%s", provider, hexDigest[:16])
}

func sortedUnique(values []string) []string {
	if values == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func cloneMap(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	maps.Copy(result, input)
	return result
}
