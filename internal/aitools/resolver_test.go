/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package aitools

import (
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

func TestResolve(t *testing.T) {
	enabled := true
	disabled := false
	tests := []struct {
		name  string
		task  *corev1alpha1.Task
		agent *corev1alpha1.Agent
		want  []string
	}{
		{
			name: "configured tools and memory are canonicalized",
			task: aiToolTask(nil, nil, []string{"task_tool", " agent_tool ", ""}),
			agent: &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Tools: []corev1alpha1.ToolReference{
				{Name: " agent_tool ", Enabled: &enabled},
				{Name: "disabled_tool", Enabled: &disabled},
			}}},
			want: []string{"agent_tool", "task_tool", "recall_memory", "remember", "propose_memory", "search_transcript"},
		},
		{
			name: "autonomous coordination adds approval",
			task: aiToolTask(nil, nil, nil),
			agent: &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Coordination: &corev1alpha1.CoordinationConfig{
				Enabled: true, Autonomous: true,
			}}},
			want: append(append([]string{}, coordinationToolNames...), "request_approval"),
		},
		{
			name: "parent label adds messaging",
			task: aiToolTask(map[string]string{labels.LabelParentTask: "parent"}, nil, nil),
			want: []string{"send_message", "check_messages", "recall_memory", "remember", "propose_memory", "search_transcript"},
		},
		{
			name: "parent annotation adds messaging",
			task: aiToolTask(nil, map[string]string{labels.AnnotationParentTaskName: "long-parent-name"}, nil),
			want: []string{"send_message", "check_messages", "recall_memory", "remember", "propose_memory", "search_transcript"},
		},
		{
			name: "non AI task has no AI tools",
			task: &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer}},
			agent: &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{
				Tools:        []corev1alpha1.ToolReference{{Name: "agent_tool"}},
				Coordination: &corev1alpha1.CoordinationConfig{Enabled: true, Autonomous: true},
			}},
			want: nil,
		},
		{
			name: "disable annotation suppresses all implicit coordination tools",
			task: aiToolTask(
				map[string]string{labels.LabelParentTask: "parent"},
				map[string]string{labels.AnnotationDisableCoordinationToolInject: "true"},
				[]string{"explicit_tool"},
			),
			agent: &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Coordination: &corev1alpha1.CoordinationConfig{
				Enabled: true, Autonomous: true,
			}}},
			want: []string{"explicit_tool", "recall_memory", "remember", "propose_memory", "search_transcript"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(tt.task, tt.agent)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("Resolve() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func aiToolTask(labelsMap, annotations map[string]string, tools []string) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Labels: labelsMap, Annotations: annotations},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI:   &corev1alpha1.AISpec{Tools: tools},
		},
	}
}
