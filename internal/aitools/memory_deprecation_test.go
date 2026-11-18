/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package aitools

import (
	"slices"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

func TestDeprecatedMemoryToolNames(t *testing.T) {
	disabled := false
	coordinator := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{
		Coordination: &corev1alpha1.CoordinationConfig{Enabled: true},
	}}
	for _, tc := range []struct {
		name  string
		task  *corev1alpha1.Task
		agent *corev1alpha1.Agent
		want  []string
	}{
		{name: "no opt-in", task: aiToolTask(nil, nil, nil), want: MemoryToolNames()},
		{name: "coordination", task: aiToolTask(nil, nil, nil), agent: coordinator},
		{name: "explicit task tool", task: aiToolTask(nil, nil, []string{" recall_memory "}),
			want: []string{"remember", "propose_memory", "search_transcript"}},
		{name: "explicit agent tool", task: aiToolTask(nil, nil, nil),
			agent: &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Tools: []corev1alpha1.ToolReference{{Name: " remember "}}}},
			want:  []string{"recall_memory", "propose_memory", "search_transcript"}},
		{name: "disabled agent selection", task: aiToolTask(nil, nil, nil),
			agent: &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Tools: []corev1alpha1.ToolReference{{Name: "remember", Enabled: &disabled}}}},
			want:  MemoryToolNames()},
		{name: "child registry is not coordination opt-in", task: aiToolTask(map[string]string{labels.LabelParentTask: "parent"}, nil, nil),
			want: MemoryToolNames()},
		{name: "disabled coordination injection preserves explicit selection",
			task:  aiToolTask(nil, map[string]string{labels.AnnotationDisableCoordinationToolInject: "true"}, []string{"propose_memory"}),
			agent: coordinator, want: []string{"recall_memory", "remember", "search_transcript"}},
		{name: "no task", agent: coordinator},
		{name: "container", task: &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer}}},
		{name: "agent task", task: &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent}}},
		{name: "runtime agent", task: aiToolTask(nil, nil, nil), agent: &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "external"}},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DeprecatedMemoryToolNames(tc.task, tc.agent)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("DeprecatedMemoryToolNames() = %#v, want %#v", got, tc.want)
			}
			resolved := Resolve(tc.task, tc.agent)
			for _, name := range got {
				if !slices.Contains(resolved, name) || !IsImplicitTool(tc.task, tc.agent, name) {
					t.Fatalf("deprecated tool %q is not a resolved platform tool", name)
				}
			}
		})
	}
}
