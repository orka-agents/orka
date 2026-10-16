package controller

import (
	"fmt"
	"slices"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const ceilingToolBash = "Bash"

// validateAgentRuntimeCeiling rejects a Task whose resolved tool policy or
// iteration budget exceeds the Agent's runtime ceiling. The resolved request is
// the Task override when present and the Agent default otherwise, so Agent
// defaults that exceed the ceiling fail as well. A nil task checks only the
// defaults the Agent sets explicitly, because Tasks may still supply their own
// policy within the ceiling. Orka-injected coordination tools are not counted.
func validateAgentRuntimeCeiling(task *corev1alpha1.Task, agent *corev1alpha1.Agent) error {
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
		// Registered external runtimes fix the iteration budget in their profile
		// and reject maxTurns overrides, so the ceiling cannot be exceeded there.
		if turns := effectiveACPMaxTurns(task, agent); turns > *ceiling.MaxTurns {
			return fmt.Errorf("agent %q ceiling allows at most %d turns, but the task resolves to %d; set maxTurns or the Agent defaultMaxTurns within the ceiling",
				agent.Name, *ceiling.MaxTurns, turns)
		}
	}

	if !checkTools || (ceiling.AllowedTools == nil && (ceiling.AllowBash == nil || *ceiling.AllowBash)) {
		return nil
	}
	tools, bash := resolvedCeilingToolRequest(task, agent, runtimeRef)
	// Registered external runtimes fix the Bash gate in their profile, so only
	// the tool allowlist applies to them.
	if ceiling.AllowBash != nil && !*ceiling.AllowBash && bash && !runtimeRef {
		return fmt.Errorf("agent %q ceiling forbids Bash, but the task resolves to a policy that allows it; set allowBash: false or omit Bash from the allowed tools", agent.Name)
	}
	if ceiling.AllowedTools == nil || (task == nil && runtime.DefaultAllowedTools == nil) {
		return nil
	}
	if tools == nil {
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

// resolvedCeilingToolRequest returns the tools the Task can execute after the
// runtime's own normalization, deny list, and Bash gate, and whether that
// policy grants Bash. It starts from the same allowlist dispatch uses and
// then drops only the injected coordination tools. A nil tool list means the
// unrestricted native set.
func resolvedCeilingToolRequest(task *corev1alpha1.Task, agent *corev1alpha1.Agent, runtimeRef bool) ([]string, bool) {
	requested := requestedACPAllowedTools(task, agent)
	if runtimeRef {
		// External runtimes accept only an exact registered allowlist and
		// reject allowBash and disallowedTools overrides.
		tools := sortedUnique(requested)
		return tools, slices.ContainsFunc(tools, isCeilingBashTool)
	}
	if len(requested) > 0 && !slices.ContainsFunc(requested, func(tool string) bool { return strings.TrimSpace(tool) != "" }) {
		// A nonempty all-blank allowlist is the legacy unrestricted sentinel;
		// judge it as the widest grant so it can never slip past a ceiling.
		requested = nil
	} else {
		requested = effectiveACPAllowedTools(task, agent)
	}
	tools, bash := resolvedCeilingRuntimeTools(task, agent, requested)
	if tools != nil && injectsACPCoordinationTools(task, agent) {
		tools = slices.DeleteFunc(tools, func(tool string) bool { return slices.Contains(acpInjectedCoordinationTools, tool) })
	}
	return tools, bash
}

func resolvedCeilingRuntimeTools(task *corev1alpha1.Task, agent *corev1alpha1.Agent, requested []string) ([]string, bool) {
	var disallowed []string
	if task != nil && task.Spec.AgentRuntime != nil {
		disallowed = sortedUnique(task.Spec.AgentRuntime.DisallowedTools)
	}
	allowBash := effectiveACPAllowBash(task, agent)
	// Without a Task, judge the Agent defaults at their widest: write intent
	// lifts the read-only narrowing some runtimes apply.
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
	if allowed == nil {
		return nil, allowBash
	}
	// Judge exactly what dispatch grants: descriptors keep every allowed name
	// that MCPToolPolicy.Allows accepts, and its deny list matches names
	// exactly, so a differently cased deny entry does not narrow the request.
	policy := harnessv2.MCPToolPolicy{AllowedToolNames: allowed, DisallowedToolNames: disallowed, AllowBash: allowBash}
	tools := sortedUnique(slices.DeleteFunc(slices.Clone(allowed), func(tool string) bool { return !policy.Allows(tool) }))
	return tools, slices.ContainsFunc(tools, isCeilingBashTool)
}

// validateHarnessV1RuntimeRefCeiling enforces the turn ceiling for a runtimeRef
// Agent whose AgentRuntime resolved to harness v1. Unlike v2 registrations, v1
// freezes the Task or Agent maxTurns into the session configuration.
func validateHarnessV1RuntimeRefCeiling(task *corev1alpha1.Task, agent *corev1alpha1.Agent) error {
	if agent == nil || agent.Spec.Runtime == nil || agent.Spec.Runtime.Ceiling == nil || agent.Spec.Runtime.Ceiling.MaxTurns == nil ||
		agent.Spec.Runtime.RuntimeRef == nil || strings.TrimSpace(agent.Spec.Runtime.RuntimeRef.Name) == "" {
		return nil
	}
	limit := *agent.Spec.Runtime.Ceiling.MaxTurns
	if turns := effectiveACPMaxTurns(task, agent); turns > limit {
		return fmt.Errorf("agent %q ceiling allows at most %d turns, but the task resolves to %d; set maxTurns or the Agent defaultMaxTurns within the ceiling",
			agent.Name, limit, turns)
	}
	return nil
}

// ceilingAllowsTool reports whether a requested tool is within the ceiling.
// Tool names match without regard to case, but scopes must match exactly
// because they carry command or path text. A bare ceiling entry also covers
// scoped requests of the same tool; a scoped entry covers only its own scope.
func ceilingAllowsTool(ceiling []string, tool string) bool {
	tool = strings.TrimSpace(tool)
	if tool == "" {
		return true
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

// splitCeilingTool separates a tool name from its parenthesized scope.
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
