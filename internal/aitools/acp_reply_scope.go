package aitools

import (
	"encoding/json"
	"slices"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

// ACPGatewayReplyScopeAllows checks restrictions only, never origin or runtime
// authorization. Both frozen policy projection and live sender admission use it.
func ACPGatewayReplyScopeAllows(task *corev1alpha1.Task) bool {
	if task == nil || task.Spec.Type != corev1alpha1.TaskTypeAgent || labels.ParentTaskName(task.Labels, task.Annotations) != "" || task.Annotations[labels.AnnotationAgentReadOnly] == "true" {
		return false
	}
	if runtime := task.Spec.AgentRuntime; runtime != nil && slices.ContainsFunc(runtime.DisallowedTools, func(name string) bool { return strings.TrimSpace(name) == GatewayReplyToolName }) {
		return false
	}
	if task.Spec.Transaction != nil {
		if value, ok := task.Spec.Transaction.Context["allowedTools"]; ok {
			var names []string
			if json.Unmarshal([]byte(value), &names) != nil {
				names = strings.Split(value, ",")
			}
			if !slices.ContainsFunc(names, func(name string) bool { return strings.TrimSpace(name) == GatewayReplyToolName }) {
				return false
			}
		}
	}
	return true
}
