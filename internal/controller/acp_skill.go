package controller

import (
	"fmt"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// acpSkillRuntimeSupport lists the built-in runtimes whose native skills
// folder the supervisor projects Agent Skills into, limited to exactly the
// Orka-provided skills where the runtime allows it.
var acpSkillRuntimeSupport = map[corev1alpha1.AgentRuntimeType]bool{
	corev1alpha1.AgentRuntimeCodex:    true,
	corev1alpha1.AgentRuntimeClaude:   true,
	corev1alpha1.AgentRuntimeOpencode: true,
}

// acpSkillRuntimeUnsupportedReasons explains why a built-in runtime can't
// receive skills. Every runtime is rejected at admission and planning instead
// of silently ignoring its skills.
var acpSkillRuntimeUnsupportedReasons = map[corev1alpha1.AgentRuntimeType]string{
	// Copilot CLI 1.0.96 in ACP mode keeps loading repository skills
	// (.github/skills, .agents/skills, .claude/skills) whatever its settings
	// say, so enabling its skill tool would also hand the model untrusted
	// repository skills, whose allowed-tools can add permission rules.
	corev1alpha1.AgentRuntimeCopilot: "Copilot CLI cannot limit skill discovery to Orka-provided skills in ACP mode, so enabling skills would also enable untrusted repository skills",
}

func acpSkillRuntimeUnsupportedReason(runtimeType corev1alpha1.AgentRuntimeType) string {
	if acpSkillRuntimeSupport[runtimeType] {
		return ""
	}
	if reason := acpSkillRuntimeUnsupportedReasons[runtimeType]; reason != "" {
		return fmt.Sprintf("runtime.skills are not supported by runtime %q: %s", runtimeType, reason)
	}
	return fmt.Sprintf("runtime.skills are not supported by runtime %q: it has no native skills folder Orka can project skills into", runtimeType)
}

const (
	acpSkillsExternalRuntimeMessage  = "runtime.skills are supported only on built-in runtime types; external runtimeRef runtimes cannot receive skills"
	acpSkillsExecutionWorkspaceError = "runtime.skills are not supported with Task.spec.execution.workspace; execution workspaces (including Agent Sandbox and Substrate) cannot mount skills yet"
)

// agentSkills returns the Agent's declared runtime skills, or nil.
func agentSkills(agent *corev1alpha1.Agent) []corev1alpha1.AgentRuntimeSkill {
	if agent == nil || agent.Spec.Runtime == nil {
		return nil
	}
	return agent.Spec.Runtime.Skills
}

// RuntimeSkillsFromAgent converts Agent skills into the profile shape,
// preserving declared order and resolving the default source path so the
// frozen profile names the exact folder that is read.
func RuntimeSkillsFromAgent(skills []corev1alpha1.AgentRuntimeSkill) []harnessv2.RuntimeSkill {
	if len(skills) == 0 {
		return nil
	}
	result := make([]harnessv2.RuntimeSkill, len(skills))
	for i := range skills {
		name := strings.TrimSpace(skills[i].Name)
		source := strings.TrimSpace(skills[i].Path)
		if source == "" {
			source = harnessv2.DefaultRuntimeSkillPath(name)
		}
		result[i] = harnessv2.RuntimeSkill{Name: name, Image: strings.TrimSpace(skills[i].Image), Path: source}
	}
	return result
}

func runtimePoolSkillsFromProfile(skills []harnessv2.RuntimeSkill) []corev1alpha1.RuntimePoolSkill {
	if len(skills) == 0 {
		return nil
	}
	result := make([]corev1alpha1.RuntimePoolSkill, len(skills))
	for i := range skills {
		result[i] = corev1alpha1.RuntimePoolSkill{Name: skills[i].Name, Image: skills[i].Image, Path: skills[i].Path}
	}
	return result
}

func runtimeSkillsFromPool(skills []corev1alpha1.RuntimePoolSkill) []harnessv2.RuntimeSkill {
	if len(skills) == 0 {
		return nil
	}
	result := make([]harnessv2.RuntimeSkill, len(skills))
	for i := range skills {
		result[i] = harnessv2.RuntimeSkill{
			Name:  strings.TrimSpace(skills[i].Name),
			Image: strings.TrimSpace(skills[i].Image),
			Path:  strings.TrimSpace(skills[i].Path),
		}
	}
	return result
}

// ValidateAgentSkills applies the full skill policy to an Agent: feature
// gates, image-delivery availability, built-in runtime support, shape rules,
// and the registry allowlist. An Agent without skills always passes.
func (p ACPToolboxPolicy) ValidateAgentSkills(agent *corev1alpha1.Agent) error {
	skills := agentSkills(agent)
	if len(skills) == 0 {
		return nil
	}
	if !p.Enabled || !p.SkillsEnabled {
		return fmt.Errorf("runtime.skills are disabled on this controller; enable --acp-skills-enabled and --acp-toolboxes-enabled and allow the skill registries")
	}
	if reason := strings.TrimSpace(p.UnavailableReason); reason != "" {
		return fmt.Errorf("%s%s", acpToolboxUnavailablePrefix, reason)
	}
	if agent.Spec.Runtime.RuntimeRef != nil && strings.TrimSpace(agent.Spec.Runtime.RuntimeRef.Name) != "" {
		return fmt.Errorf("%s", acpSkillsExternalRuntimeMessage)
	}
	if agent.Spec.Runtime.Type == "" {
		return fmt.Errorf("runtime.skills require a built-in runtime.type")
	}
	if reason := acpSkillRuntimeUnsupportedReason(agent.Spec.Runtime.Type); reason != "" {
		return fmt.Errorf("%s", reason)
	}
	if contract := agent.BuiltInContractVersion(); contract != "" && contract != corev1alpha1.AgentRuntimeContractHarnessV2 {
		return fmt.Errorf("runtime.skills require the orka.harness.v2 contract")
	}
	converted := RuntimeSkillsFromAgent(skills)
	if err := harnessv2.ValidateRuntimeSkills(converted); err != nil {
		return fmt.Errorf("runtime.%w", err)
	}
	for i := range converted {
		if !ToolboxImageAllowed(converted[i].Image, p.AllowedRegistries) {
			return fmt.Errorf("runtime.skills[%d] image %q is not from an allowed toolbox registry", i, converted[i].Image)
		}
	}
	return nil
}

// agentSkillPlanRejection repeats the admission checks at planning time so a
// Task never reaches a RuntimePool with skills it cannot bind, even if the
// Agent was admitted under a different policy.
func (p ACPToolboxPolicy) agentSkillPlanRejection(task *corev1alpha1.Task, agent *corev1alpha1.Agent, external bool) string {
	if len(agentSkills(agent)) == 0 {
		return ""
	}
	switch {
	case external:
		return acpSkillsExternalRuntimeMessage
	case taskRequestsExecutionWorkspace(task):
		return acpSkillsExecutionWorkspaceError
	}
	if err := p.ValidateAgentSkills(agent); err != nil {
		return err.Error()
	}
	return ""
}
