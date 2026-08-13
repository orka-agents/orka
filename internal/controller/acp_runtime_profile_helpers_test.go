package controller

import (
	"fmt"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// PlanACPRuntime is a test-only convenience wrapper: production planning goes
// through PlanACPRuntimeWithConfiguration with a resolved session
// configuration, which this helper builds from the Agent's inline systemPrompt.
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
