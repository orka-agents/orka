package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestProvidersFromAgentInAnotherNamespace(t *testing.T) {
	task := func(namespace string, providerRef *ProviderReference) *Task {
		return &Task{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "work"},
			Spec:       TaskSpec{Type: TaskTypeAI, AI: &AISpec{ProviderRef: providerRef}},
		}
	}
	agent := &Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "reviewer"},
		Spec: AgentSpec{
			ProviderRef: &ProviderReference{Name: "primary"},
			Model:       &ModelConfig{Fallbacks: []ModelFallback{{ProviderRef: "backup"}}},
		},
	}

	require.Equal(t, []string{"primary", "backup"}, ProvidersFromAgentInAnotherNamespace(task("team-a", nil), agent))
	require.Equal(t, []string{"backup"}, ProvidersFromAgentInAnotherNamespace(task("team-a", &ProviderReference{Name: "mine"}), agent),
		"the Task's own Provider replaces the Agent's; its fallbacks still resolve in the Task namespace")
	require.Nil(t, ProvidersFromAgentInAnotherNamespace(task("agents", nil), agent), "a same-namespace Agent resolves where it was checked")

	agentTask := task("team-a", nil)
	agentTask.Spec.Type = TaskTypeAgent
	require.Nil(t, ProvidersFromAgentInAnotherNamespace(agentTask, agent), "an agent Task does not run on a Provider")

	pinned := agent.DeepCopy()
	pinned.Spec.ProviderRef.Namespace = "models"
	pinned.Spec.Model = nil
	require.Equal(t, []string{"primary"}, ProvidersFromAgentInAnotherNamespace(task("team-a", nil), pinned),
		"an explicit namespace does not help: the Agent's own namespace may be outside the policies that check it")
	require.Nil(t, ProvidersFromAgentInAnotherNamespace(task("team-a", &ProviderReference{Name: "mine"}), pinned))
}
