package aitools

import (
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/stretchr/testify/require"
)

func TestResolveGatewayReplyRequiresDurableGrantAndPolicy(t *testing.T) {
	task := aiToolTask(nil, nil, []string{"reply_in_conversation"})
	require.NotContains(t, Resolve(task, nil), "reply_in_conversation")
	require.NotContains(t, ResolveWithGatewayReply(task, nil, false), "reply_in_conversation")
	require.Contains(t, ResolveWithGatewayReply(task, nil, true), "reply_in_conversation")
	for _, tc := range []struct {
		name string
		edit func(*corev1alpha1.Task, *corev1alpha1.Agent)
	}{
		{"explicit AI list", func(task *corev1alpha1.Task, _ *corev1alpha1.Agent) { task.Spec.AI.Tools = []string{"web_search"} }},
		{"empty AI list", func(task *corev1alpha1.Task, _ *corev1alpha1.Agent) { task.Spec.AI.Tools = []string{} }},
		{"agent disabled", func(_ *corev1alpha1.Task, agent *corev1alpha1.Agent) {
			agent.Spec.Tools = []corev1alpha1.ToolReference{{Name: "reply_in_conversation", Enabled: new(false)}}
		}},
		{"agent explicit list", func(_ *corev1alpha1.Task, agent *corev1alpha1.Agent) {
			agent.Spec.Tools = []corev1alpha1.ToolReference{{Name: "web_search"}}
		}},
		{"disallowed", func(task *corev1alpha1.Task, _ *corev1alpha1.Agent) {
			task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{DisallowedTools: []string{"reply_in_conversation"}}
		}},
		{"transaction", func(task *corev1alpha1.Task, _ *corev1alpha1.Agent) {
			task.Spec.Transaction = &corev1alpha1.TaskTransaction{Context: map[string]string{"allowedTools": "[\"web_search\"]"}}
		}},
		{"empty transaction", func(task *corev1alpha1.Task, _ *corev1alpha1.Agent) {
			task.Spec.Transaction = &corev1alpha1.TaskTransaction{Context: map[string]string{"allowedTools": "[]"}}
		}},
		{"child", func(task *corev1alpha1.Task, _ *corev1alpha1.Agent) {
			task.Labels = map[string]string{labels.LabelParentTask: "parent"}
		}},
		{"container", func(task *corev1alpha1.Task, _ *corev1alpha1.Agent) { task.Spec.Type = corev1alpha1.TaskTypeContainer }},
		{"ACP", func(task *corev1alpha1.Task, _ *corev1alpha1.Agent) { task.Spec.Type = corev1alpha1.TaskTypeAgent }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := task.DeepCopy()
			agent := &corev1alpha1.Agent{}
			tc.edit(task, agent)
			require.NotContains(t, ResolveWithGatewayReply(task, agent, true), "reply_in_conversation")
		})
	}
	task.Spec.AI.Tools = nil
	got := ResolveWithGatewayReply(task, nil, true)
	require.Contains(t, got, "recall_memory")
	require.Contains(t, got, "reply_in_conversation")
}
