/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

func TestJobBuilder_Build_AgentLabels(t *testing.T) {
	const agentNameLabel = "orka.ai/agent-name"
	const agentUIDLabel = "orka.ai/agent-uid"
	longName := strings.Repeat("agent-", 12) + "long"
	tests := []struct {
		name       string
		agentName  string
		agentNS    string
		agentUID   types.UID
		container  bool
		validation bool
		wantName   string
		wantUID    string
	}{
		{name: "resolved agent", agentName: "agent-a", agentUID: "agent-a-uid", wantName: "agent-a", wantUID: "agent-a-uid"},
		{name: "different agent", agentName: "agent-b", agentUID: "agent-b-uid", wantName: "agent-b", wantUID: "agent-b-uid"},
		{name: "recreated agent", agentName: "agent-a", agentUID: "replacement-uid", wantName: "agent-a", wantUID: "replacement-uid"},
		{name: "long name", agentName: longName, agentUID: "long-name-uid", wantName: labels.SelectorValue(longName), wantUID: "long-name-uid"},
		{name: "foreign namespace", agentName: "agent-a", agentNS: "other", agentUID: "foreign-uid", wantUID: "foreign-uid"},
		{name: "no agent"},
		{name: "agent without UID", agentName: "agent-a", wantName: "agent-a"},
		{name: "container job", agentName: "agent-a", agentUID: "agent-a-uid", container: true},
		{name: "repository validation", agentName: "agent-a", agentUID: "agent-a-uid", container: true, validation: true},
		{name: "AI validation option", agentName: "agent-a", agentUID: "agent-a-uid", validation: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := setupJobBuilder()
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: testTask, Namespace: defaultNS, Labels: map[string]string{agentNameLabel: "caller-supplied", agentUIDLabel: "caller-supplied"}},
				Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, AI: &corev1alpha1.AISpec{
					Provider: "openai", Model: "test-model", Prompt: "test prompt",
				}},
			}
			if tt.container {
				task.Spec.Type, task.Spec.AI, task.Spec.Image = corev1alpha1.TaskTypeContainer, nil, testBusyboxImage
			}
			if tt.validation {
				task.UID = "validation-task-uid"
				task.Annotations = map[string]string{labels.AnnotationRepositoryValidationCommandDigest: "sha256:" + strings.Repeat("b", 64)}
				task.Spec.Workspace = &corev1alpha1.WorkspaceConfig{GitRepo: "https://github.com/example/repo.git"}
				task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "test-session"}
			}
			var agent *corev1alpha1.Agent
			if tt.agentName != "" {
				agent = &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: tt.agentName, Namespace: defaultNS, UID: tt.agentUID}}
				if tt.agentNS != "" {
					agent.Namespace = tt.agentNS
				}
			}
			job, err := builder.BuildWithOptions(context.Background(), task, agent, nil, JobBuildOptions{RepositoryMonitorValidation: tt.validation})
			if err != nil {
				t.Fatalf("BuildWithOptions() error = %v", err)
			}
			for name, metadata := range map[string]metav1.ObjectMeta{"job": job.ObjectMeta, "pod": job.Spec.Template.ObjectMeta} {
				for key, want := range map[string]string{agentNameLabel: tt.wantName, agentUIDLabel: tt.wantUID} {
					got, present := metadata.Labels[key]
					if got != want || present != (want != "") {
						t.Errorf("%s label %s = %q, present=%v; want %q, present=%v", name, key, got, present, want, want != "")
					}
				}
				if metadata.Labels[labels.LabelTask] != labels.SelectorValue(task.Name) || metadata.Labels[labels.LabelTaskType] != string(task.Spec.Type) {
					t.Errorf("%s Task labels changed: %v", name, metadata.Labels)
				}
			}
		})
	}
}
