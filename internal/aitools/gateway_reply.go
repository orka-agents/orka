package aitools

import (
	"encoding/json"
	"slices"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

const GatewayReplyToolName = "reply_in_conversation"

// ResolveWithGatewayReply is the native tool projection. Eligibility MUST come
// from the controller's durable event/TaskUID resolution, never Task metadata.
// Resolve without that grant strips even explicitly requested gateway-only tools.
func ResolveWithGatewayReply(task *corev1alpha1.Task, agent *corev1alpha1.Agent, eligible bool) []string {
	names := Resolve(task, agent)
	if !eligible || task == nil || task.Spec.Type != corev1alpha1.TaskTypeAI || !usesAIWorkerToolRegistry(task, agent) || labels.ParentTaskName(task.Labels, task.Annotations) != "" || !gatewayReplyPolicyAllows(task, agent) {
		return names
	}
	return append(names, GatewayReplyToolName)
}

// Explicit native policy remains authoritative even with a durable origin grant.
func gatewayReplyPolicyAllows(task *corev1alpha1.Task, agent *corev1alpha1.Agent) bool {
	contains := func(names []string) bool {
		return slices.ContainsFunc(names, func(name string) bool { return strings.TrimSpace(name) == GatewayReplyToolName })
	}
	if agent != nil && agent.Spec.Tools != nil {
		selected := false
		for _, tool := range agent.Spec.Tools {
			if strings.TrimSpace(tool.Name) == GatewayReplyToolName {
				if tool.Enabled != nil && !*tool.Enabled {
					return false
				}
				selected = true
			}
		}
		if !selected {
			return false
		}
	}
	if task.Spec.AI != nil && task.Spec.AI.Tools != nil && !contains(task.Spec.AI.Tools) {
		return false
	}
	if runtime := task.Spec.AgentRuntime; runtime != nil {
		if contains(runtime.DisallowedTools) || (runtime.AllowedTools != nil && !contains(runtime.AllowedTools)) {
			return false
		}
	}
	if task.Spec.Transaction != nil {
		if value, ok := task.Spec.Transaction.Context["allowedTools"]; ok {
			var allowed []string
			if json.Unmarshal([]byte(value), &allowed) != nil {
				allowed = strings.Split(value, ",")
			}
			if !contains(allowed) {
				return false
			}
		}
	}
	return true
}
