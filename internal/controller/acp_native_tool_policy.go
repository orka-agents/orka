package controller

import (
	"fmt"
	"slices"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func effectiveACPNativeToolPolicy(task *corev1alpha1.Task, agent *corev1alpha1.Agent) (harnessv2.MCPToolPolicy, error) {
	if task == nil || agent == nil || agent.Spec.Runtime == nil {
		return harnessv2.MCPToolPolicy{}, fmt.Errorf("task and built-in Agent are required for tool policy")
	}
	runtime := agent.Spec.Runtime
	mode := harnessv2.NativeToolPolicyMode(runtime.ToolPolicy)
	if mode != "" && (runtime.RuntimeRef != nil || agent.BuiltInContractVersion() != corev1alpha1.AgentRuntimeContractHarnessV2) {
		return harnessv2.MCPToolPolicy{}, fmt.Errorf("runtime.toolPolicy requires a built-in orka.harness.v2 Agent")
	}
	if mode == harnessv2.NativeToolPolicyFull && runtime.DefaultAllowBash != nil && !*runtime.DefaultAllowBash {
		return harnessv2.MCPToolPolicy{}, fmt.Errorf("full built-in tools are incompatible with Agent runtime.defaultAllowBash: false")
	}
	if mode == harnessv2.NativeToolPolicyFull && task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.AllowedTools != nil {
		return harnessv2.MCPToolPolicy{}, fmt.Errorf("task allowedTools cannot narrow full built-in tools; select a restricted Agent, or omit the Task allowlist and configure Orka tools in Agent runtime.defaultAllowedTools")
	}
	if mode == harnessv2.NativeToolPolicyRestricted && runtime.DefaultAllowedTools == nil &&
		(task.Spec.AgentRuntime == nil || task.Spec.AgentRuntime.AllowedTools == nil) {
		return harnessv2.MCPToolPolicy{}, fmt.Errorf("restricted tools require an explicit defaultAllowedTools or Task allowedTools list; [] denies all tools")
	}
	policy := harnessv2.MCPToolPolicy{
		NativeToolPolicy: mode, AllowedToolNames: effectiveACPAllowedTools(task, agent), AllowBash: effectiveACPAllowBash(task, agent),
	}
	if task.Spec.AgentRuntime != nil {
		policy.DisallowedToolNames = sortedUnique(task.Spec.AgentRuntime.DisallowedTools)
	}
	if mode == harnessv2.NativeToolPolicyRestricted {
		policy.AllowedToolNames = acp.NormalizeExplicitNativeToolNames(string(runtime.Type), policy.AllowedToolNames)
		policy.DisallowedToolNames = acp.NormalizeExplicitNativeToolNames(string(runtime.Type), policy.DisallowedToolNames)
	}
	intent := effectiveACPWorkspaceIntent(task)
	if err := acp.ValidateNativeToolPolicy(string(mode), string(runtime.Type), intent == corev1alpha1.WorkspaceIntentRead,
		policy.AllowedToolNames, policy.DisallowedToolNames, policy.AllowBash); err != nil {
		return harnessv2.MCPToolPolicy{}, err
	}
	if err := validateACPNativeToolSessionOwnership(task, policy); err != nil {
		return harnessv2.MCPToolPolicy{}, err
	}
	if mode == harnessv2.NativeToolPolicyFull {
		return policy, nil
	}
	policy.AllowedToolNames, policy.DisallowedToolNames, policy.AllowBash = normalizeACPRuntimeToolPolicy(
		string(runtime.Type), intent, policy.AllowedToolNames, policy.DisallowedToolNames, policy.AllowBash,
	)
	if mode != "" {
		effectiveAllowed := policy.AllowedToolNames
		if runtime.Type != corev1alpha1.AgentRuntimeOpencode {
			effectiveAllowed = acp.BuiltInRuntimeEffectiveAllowedTools(effectiveAllowed, policy.DisallowedToolNames, policy.AllowBash)
		}
		if err := validateACPProviderNativePolicy(string(runtime.Type), intent, effectiveAllowed, policy.DisallowedToolNames, policy.AllowBash); err != nil {
			return harnessv2.MCPToolPolicy{}, err
		}
	}
	return policy, nil
}

func validateACPNativeToolSessionOwnership(task *corev1alpha1.Task, policy harnessv2.MCPToolPolicy) error {
	if policy.NativeToolPolicy == "" {
		return nil
	}
	commands := policy.NativeToolPolicy == harnessv2.NativeToolPolicyFull || acp.BuiltInRuntimeEffectiveAllowBash(policy.AllowedToolNames, policy.DisallowedToolNames, policy.AllowBash)
	reusableWorkspace := task.Spec.Execution != nil && task.Spec.Execution.Workspace != nil && task.Spec.Execution.Workspace.ReusePolicy == corev1alpha1.WorkspaceReusePolicySession
	if commands && (task.Spec.SessionRef != nil || reusableWorkspace) {
		return fmt.Errorf("explicit tool policies with commands require a Task-owned Session; sessionRef and session workspace reuse are unsupported until background processes can be terminated between Tasks")
	}
	return nil
}

//nolint:goconst // Literal runner, feature, and state names keep the diagnostic table readable.
func acpTaskToolPolicyStatus(provider string, configuration harnessv2.MCPPolicyConfiguration) *corev1alpha1.TaskToolPolicyStatus {
	policy := configuration.ToolPolicy
	mode := string(policy.NativeToolPolicy)
	if mode == "" {
		mode = "legacy"
	}
	versions := map[string]string{
		"codex": acp.CodexCLIVersion, "claude": acp.ClaudeCodeVersion,
		"copilot": acp.CopilotCLIVersion, "opencode": acp.OpenCodeVersion,
	}
	result := &corev1alpha1.TaskToolPolicyStatus{
		Mode: mode, Runner: provider, RunnerVersion: versions[provider], Digest: configuration.ToolPolicyDigest,
	}
	allows := func(name string) bool {
		if policy.NativeToolPolicy == harnessv2.NativeToolPolicyFull {
			return true
		}
		if strings.EqualFold(name, "Bash") && !policy.AllowBash {
			return false
		}
		if slices.ContainsFunc(policy.DisallowedToolNames, func(item string) bool { return strings.EqualFold(item, name) }) {
			return false
		}
		return policy.AllowedToolNames == nil || slices.ContainsFunc(policy.Tools, func(tool harnessv2.MCPToolDescriptor) bool {
			return tool.Source == harnessv2.MCPToolSourceProviderNative && strings.EqualFold(tool.Name, name)
		})
	}
	for _, feature := range []struct {
		name  string
		tools []string
	}{
		{"file_read", []string{"Read"}}, {"file_write", []string{"Edit", "Write", "apply_patch"}}, {"commands", []string{"Bash"}},
		{"web_search", []string{"WebSearch"}}, {"web_fetch", []string{"WebFetch"}},
	} {
		status := corev1alpha1.TaskToolFeatureStatus{
			Name: feature.name, Source: "runner", State: "disabled", Reason: "Disabled by the frozen native tool policy.",
		}
		if slices.ContainsFunc(feature.tools, allows) {
			status.State, status.Reason = "ready", "Enabled in the pinned runner configuration; workspace, identity, and Task limits still apply."
			if strings.HasPrefix(feature.name, "web_") {
				status.State = "unverified"
				status.Reason = "Native tool enabled; provider support, remote service access, and egress have not been verified. RuntimePool egress is default-deny; full tools do not add network destinations or credentials."
			}
		}
		if provider == "opencode" && strings.HasPrefix(feature.name, "web_") {
			if mode == "legacy" {
				status.State, status.Reason = "disabled", "The legacy OpenCode integration denies native web tools; an operator must select an explicit tool policy."
			} else if status.State == "unverified" && feature.name == "web_search" {
				status.Reason = "Native Exa search is enabled. Access to https://mcp.exa.ai/mcp must be permitted by operator network policy; reachability has not been verified. No search API key is injected."
			}
		}
		if provider == "codex" && feature.name == "web_fetch" {
			status.State, status.Reason = "unsupported", "The pinned Codex integration has no standalone WebFetch tool. A shared Orka tool name does not establish native support."
		}
		result.Features = append(result.Features, status)
	}
	result.Features = append(result.Features,
		corev1alpha1.TaskToolFeatureStatus{Name: "native_helpers", Source: "runner", State: "unsupported", Reason: "Native helper agents and schedulers have no Orka child Task ownership contract and are disabled in explicit modes. Use authorized Orka delegate_task and wait_for_tasks tools."},
		corev1alpha1.TaskToolFeatureStatus{Name: "extensions", Source: "runner", State: "unsupported", Reason: "Task and repository configuration cannot install plugins, load additional MCP servers, or supply credentials. Full mode applies to approved built-in tools."},
	)
	for _, tool := range policy.Tools {
		if !tool.Source.Brokered() {
			continue
		}
		feature := corev1alpha1.TaskToolFeatureStatus{Name: tool.Name, Source: "orka", State: "ready", Reason: "Authorized through the prompt-scoped Orka broker; its tool, delegation, and publication checks remain in force."}
		if tool.Source == harnessv2.MCPToolSourceBrokeredCustom {
			feature.State, feature.Reason = "unverified", "Authorized custom Orka tool; endpoint reachability and required credentials are checked when invoked."
		}
		if configuration.ApprovalPolicy.Requires(tool.Name) {
			feature.State, feature.Reason = "unavailable", "Required Orka approval review is unavailable; the tool cannot execute."
		}
		result.Features = append(result.Features, feature)
	}
	return result
}
