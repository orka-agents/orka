/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import "strings"

// ProvidersFromAgentInAnotherNamespace lists the Providers an ai Task takes
// from its Agent when that Agent is in another namespace: the Agent's
// providerRef, unless the Task sets its own, and every model fallback.
// Provider use authorization checks a Provider when an Agent names it, but
// only in namespaces its admission policies cover, and fallbacks and a
// namespace-less providerRef resolve in the Task's namespace rather than the
// Agent's. So a Task cannot rely on those checks for an Agent elsewhere. It
// returns nil for a same-namespace Agent and for a Task that does not run on
// a Provider.
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
