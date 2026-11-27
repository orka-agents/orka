/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import "strings"

func ProvidersFromAgentInAnotherNamespace(task *Task, agent *Agent) []string {
	if task == nil || agent == nil || task.Spec.Type != TaskTypeAI || agent.Namespace == task.Namespace {
		return nil
	}
	var names []string
	taskProvider := task.Spec.AI != nil && task.Spec.AI.ProviderRef != nil
	if ref := agent.Spec.ProviderRef; !taskProvider && ref != nil && strings.TrimSpace(ref.Name) != "" {
		names = append(names, ref.Name)
	}
	if agent.Spec.Model != nil {
		for _, fallback := range agent.Spec.Model.Fallbacks {
			if strings.TrimSpace(fallback.ProviderRef) != "" {
				names = append(names, fallback.ProviderRef)
			}
		}
	}
	return names
}
