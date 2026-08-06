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
			want: append(append([]string{}, implicitCoordinationToolNames...), "request_approval"),
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
			name: "agent child retains configured tools without AI worker implicit tools",
			task: &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{labels.LabelParentTask: "parent"}},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AI: &corev1alpha1.AISpec{Tools: []string{"brokered"}}},
			},
			agent: &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{
				Tools:        []corev1alpha1.ToolReference{{Name: "agent_tool"}},
				Coordination: &corev1alpha1.CoordinationConfig{Enabled: true, Autonomous: true},
			}},
			want: []string{"agent_tool", "brokered"},
		},
		{
			name: "container task has no AI tools",
			task: &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer}},
			agent: &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{
				Tools:        []corev1alpha1.ToolReference{{Name: "agent_tool"}},
				Coordination: &corev1alpha1.CoordinationConfig{Enabled: true, Autonomous: true},
			}},
			want: nil,
		},
		{
			name: "disable annotation suppresses implicit tools",
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

func TestRegistersCoordinationTools(t *testing.T) {
	tests := []struct {
		name  string
		task  *corev1alpha1.Task
		agent *corev1alpha1.Agent
		want  bool
	}{
		{
			name:  "coordinator agent",
			task:  aiToolTask(nil, nil, nil),
			agent: &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Coordination: &corev1alpha1.CoordinationConfig{Enabled: true}}},
			want:  true,
		},
		{
			name: "child with implicit injection disabled",
			task: aiToolTask(
				map[string]string{labels.LabelParentTask: "parent"},
				map[string]string{labels.AnnotationDisableCoordinationToolInject: "true"},
				[]string{"send_message"},
			),
			want: true,
		},
		{
			name: "child identified by annotation",
			task: aiToolTask(nil, map[string]string{
				labels.AnnotationParentTaskName:                "long-parent-name",
				labels.AnnotationDisableCoordinationToolInject: "true",
			}, []string{"list_pull_requests"}),
			want: true,
		},
		{name: "standalone task", task: aiToolTask(nil, nil, []string{"send_message"})},
		{
			name: "agent child",
			task: &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{labels.LabelParentTask: "parent"}},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
			},
		},
		{
			name: "container child",
			task: &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{labels.LabelParentTask: "parent"}},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RegistersCoordinationTools(tt.task, tt.agent); got != tt.want {
				t.Fatalf("RegistersCoordinationTools() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestIsImplicitTool(t *testing.T) {
	child := aiToolTask(map[string]string{labels.LabelParentTask: "parent"}, nil, nil)
	if !IsImplicitTool(child, nil, "send_message") {
		t.Fatal("child messaging tool was not classified as implicit")
	}
	child.Annotations = map[string]string{labels.AnnotationDisableCoordinationToolInject: "true"}
	if IsImplicitTool(child, nil, "send_message") {
		t.Fatal("disabled child messaging tool was classified as implicit")
	}
	if IsImplicitTool(aiToolTask(nil, nil, []string{"list_issues"}), nil, "list_issues") {
		t.Fatal("explicit issue tool was classified as implicit")
	}
}

func TestToolNameAccessorsReturnCopies(t *testing.T) {
	const changed = "changed"
	coordination := CoordinationToolNames()
	memory := MemoryToolNames()
	coordination[0] = changed
	memory[0] = changed
	if CoordinationToolNames()[0] == changed || MemoryToolNames()[0] == changed {
		t.Fatal("tool name accessor exposed mutable package state")
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
