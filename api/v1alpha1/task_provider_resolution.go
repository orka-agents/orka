/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import "strings"

// AgentProvidersResolvedInTaskNamespace lists the Providers that an ai Task's
// Agent from another namespace makes the Task resolve in the Task's own
// namespace: the Agent's providerRef when it has no namespace and the Task
// sets none of its own, and every model fallback. The Agent's author named
// them for the Agent's namespace, so whether they may be used in the Task's
// namespace was never decided by anyone. It returns nil for a same-namespace
// Agent and for a Task that does not run on a Provider.
func AgentProvidersResolvedInTaskNamespace(task *Task, agent *Agent) []string {
	if task == nil || agent == nil || task.Spec.Type != TaskTypeAI || agent.Namespace == task.Namespace {
		return nil
	}
	var names []string
	taskProvider := task.Spec.AI != nil && task.Spec.AI.ProviderRef != nil
	if ref := agent.Spec.ProviderRef; !taskProvider && ref != nil && ref.Namespace == "" && strings.TrimSpace(ref.Name) != "" {
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
