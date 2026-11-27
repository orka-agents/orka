package controller

import (
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const ceilingToolBash = "Bash"

func validateAgentRuntimeCeiling(task *corev1alpha1.Task, agent *corev1alpha1.Agent) error {
	return validateAgentRuntimeCeilingWithRegisteredPolicy(task, agent, nil)
}

func validateAgentRuntimeCeilingWithRegisteredPolicy(
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
	registered *corev1alpha1.AgentRuntimeMCPPolicySpec,
) error {
	if agent == nil || agent.Spec.Runtime == nil || agent.Spec.Runtime.Ceiling == nil {
		return nil
	}
	ceiling := agent.Spec.Runtime.Ceiling
	runtime := agent.Spec.Runtime
	runtimeRef := runtime.RuntimeRef != nil && strings.TrimSpace(runtime.RuntimeRef.Name) != ""
	checkTurns, checkTools := true, true
	if task == nil {
		checkTurns = runtime.DefaultMaxTurns != nil
		checkTools = !runtimeRef && (runtime.DefaultAllowedTools != nil || runtime.DefaultAllowBash != nil)
	}

	if ceiling.MaxTurns != nil && !runtimeRef && checkTurns {
		if turns := effectiveACPMaxTurns(task, agent); turns > *ceiling.MaxTurns {
			return fmt.Errorf("agent %q ceiling allows at most %d turns, but the task resolves to %d; set maxTurns or the Agent defaultMaxTurns within the ceiling",
				agent.Name, *ceiling.MaxTurns, turns)
		}
	}

	if !checkTools || (ceiling.AllowedTools == nil && (ceiling.AllowBash == nil || *ceiling.AllowBash)) {
		return nil
	}
	tools, bash := resolvedCeilingToolRequest(task, agent, runtimeRef, registered)
	if ceiling.AllowBash != nil && !*ceiling.AllowBash && bash && !runtimeRef {
		if runtime.Type == corev1alpha1.AgentRuntimeCodex {
			return fmt.Errorf("agent %q ceiling forbids Bash, but the codex runtime runs shell commands in every session, including read-only ones; use another runtime or allow Bash in the ceiling", agent.Name)
		}
		return fmt.Errorf("agent %q ceiling forbids Bash, but the task resolves to a policy that allows it; set allowBash: false or omit Bash from the allowed tools", agent.Name)
	}
	if ceiling.AllowedTools == nil || (task == nil && runtime.DefaultAllowedTools == nil) {
		return nil
	}
	if tools == nil {
		if runtimeRef {
			return fmt.Errorf("agent %q ceiling restricts tools, but the task does not set allowedTools; set allowedTools within the ceiling", agent.Name)
		}
		return fmt.Errorf("agent %q ceiling restricts tools, but the task resolves to the unrestricted native tool set; set allowedTools or the Agent defaultAllowedTools within the ceiling", agent.Name)
	}
	limit := ceiling.AllowedTools
	if runtime.Type == corev1alpha1.AgentRuntimeOpencode {
		limit = acp.NormalizeOpenCodeAuthorizationTools(limit)
	}
	for _, tool := range tools {
		if !ceilingAllowsTool(limit, tool) {
			return fmt.Errorf("agent %q ceiling does not allow tool %q", agent.Name, tool)
		}
	}
	return nil
}

func validateAgentRuntimeCeilingForRegistration(
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
	registration *corev1alpha1.AgentRuntime,
) error {
	var policy *corev1alpha1.AgentRuntimeMCPPolicySpec
	if registration != nil && registration.Spec.Capabilities != nil {
		policy = registration.Spec.Capabilities.MCPPolicy
	}
	return validateAgentRuntimeCeilingWithRegisteredPolicy(task, agent, policy)
}

func resolvedCeilingToolRequest(
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
	runtimeRef bool,
	registered *corev1alpha1.AgentRuntimeMCPPolicySpec,
) ([]string, bool) {
	requested := requestedACPAllowedTools(task, agent)
	if runtimeRef {
		tools := sortedUnique(requested)
		if tools != nil && registered != nil && slices.Equal(effectiveACPAllowedTools(task, agent), registered.AllowedTools) {
			policy := harnessv2.MCPToolPolicy{
				AllowedToolNames:    registered.AllowedTools,
				DisallowedToolNames: registered.DisallowedTools,
				AllowBash:           registered.AllowBash,
			}
			tools = slices.DeleteFunc(tools, func(tool string) bool { return !policy.Allows(tool) })
		}
		return tools, slices.ContainsFunc(tools, isCeilingBashTool)
	}
	if len(requested) > 0 && !slices.ContainsFunc(requested, func(tool string) bool { return strings.TrimSpace(tool) != "" }) {
		requested = nil
	} else {
		requested = effectiveACPAllowedTools(task, agent)
	}
	asked := requestedACPAllowedTools(task, agent)
	tools, bash := resolvedCeilingRuntimeTools(task, agent, requested)
	if tools != nil && injectsACPCoordinationTools(task, agent) {
		tools = slices.DeleteFunc(tools, func(tool string) bool {
			return slices.Contains(acpInjectedCoordinationTools, tool) &&
				!slices.ContainsFunc(asked, func(name string) bool { return strings.EqualFold(strings.TrimSpace(name), tool) })
		})
	}
	return tools, bash
}

func resolvedCeilingRuntimeTools(task *corev1alpha1.Task, agent *corev1alpha1.Agent, requested []string) ([]string, bool) {
	var disallowed []string
	if task != nil && task.Spec.AgentRuntime != nil {
		disallowed = sortedUnique(task.Spec.AgentRuntime.DisallowedTools)
	}
	allowBash := effectiveACPAllowBash(task, agent)
	intent := corev1alpha1.WorkspaceIntentWrite
	if task != nil {
		intent = effectiveACPWorkspaceIntent(task)
	}
	provider := string(agent.Spec.Runtime.Type)
	allowed, disallowed, allowBash := normalizeACPRuntimeToolPolicy(provider, intent, requested, disallowed, allowBash)
	if agent.Spec.Runtime.Type == corev1alpha1.AgentRuntimeOpencode {
		tools := acp.OpenCodeEffectiveAllowedTools(allowed, disallowed, allowBash)
		return tools, allowBash && slices.ContainsFunc(tools, isCeilingBashTool)
	}
	codexShell := agent.Spec.Runtime.Type == corev1alpha1.AgentRuntimeCodex
	if allowed == nil {
		return nil, allowBash || codexShell
	}
	policy := harnessv2.MCPToolPolicy{AllowedToolNames: allowed, DisallowedToolNames: disallowed, AllowBash: allowBash}
	tools := sortedUnique(slices.DeleteFunc(slices.Clone(allowed), func(tool string) bool { return !policy.Allows(tool) }))
	return tools, codexShell || slices.ContainsFunc(tools, isCeilingBashTool)
}

func ceilingAllowsTool(ceiling []string, tool string) bool {
	tool = strings.TrimSpace(tool)
	if tool == "" {
		return true
	}
	if parent, ok := strings.CutSuffix(tool, harnessv2.MCPForwardWriteSuffix); ok {
		if len(validation.IsDNS1123Subdomain(parent)) > 0 {
			return false
		}
		tool = parent
	}
	base, scope, scoped := splitCeilingTool(tool)
	return slices.ContainsFunc(ceiling, func(entry string) bool {
		entryBase, entryScope, entryScoped := splitCeilingTool(entry)
		if !strings.EqualFold(entryBase, base) {
			return false
		}
		return !entryScoped || (scoped && entryScope == scope)
	})
}

func splitCeilingTool(tool string) (base, scope string, scoped bool) {
	tool = strings.TrimSpace(tool)
	if before, after, ok := strings.Cut(tool, "("); ok {
		return strings.TrimSpace(before), after, true
	}
	return tool, "", false
}

func ceilingToolBaseName(tool string) string {
	base, _, _ := splitCeilingTool(tool)
	return base
}

func isCeilingBashTool(tool string) bool {
	return strings.EqualFold(ceilingToolBaseName(tool), ceilingToolBash)
}
