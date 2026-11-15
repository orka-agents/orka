/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/aitools"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/workerenv"
)

func TestAddAIEnvVars_MemoryDeprecationPolicy(t *testing.T) {
	for _, tc := range []struct {
		name         string
		coordination bool
		child        bool
		disable      bool
		explicit     []string
		want         []string
	}{
		{name: "legacy", want: aitools.MemoryToolNames()},
		{name: "coordinator", coordination: true},
		{name: "explicit", explicit: aitools.MemoryToolNames()},
		{name: "partial explicit", explicit: []string{"recall_memory"}, want: []string{"remember", "propose_memory", "search_transcript"}},
		{name: "child", child: true, want: aitools.MemoryToolNames()},
		{name: "disabled coordinator injection", coordination: true, disable: true, want: aitools.MemoryToolNames()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jb := setupJobBuilder()
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: testTask, Namespace: defaultNS},
				Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI,
					AI: &corev1alpha1.AISpec{Prompt: "test", Tools: tc.explicit}},
			}
			if tc.child {
				task.Labels = map[string]string{labels.LabelParentTask: "parent"}
			}
			if tc.disable {
				task.Annotations = map[string]string{labels.AnnotationDisableCoordinationToolInject: "true"}
			}
			var agent *corev1alpha1.Agent
			if tc.coordination {
				agent = &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{
					Coordination: &corev1alpha1.CoordinationConfig{Enabled: true},
				}}
			}
			// User env cannot hide warnings or falsely mark an opted-in tool.
			envVars, err := jb.addAIEnvVars(context.Background(), []corev1.EnvVar{
				{Name: workerenv.AIDeprecatedMemoryTools, Value: "spoofed"},
			}, task, agent, nil, JobBuildOptions{})
			if err != nil {
				t.Fatal(err)
			}
			values := map[string]string{}
			count := 0
			for _, env := range envVars {
				values[env.Name] = env.Value
				if env.Name == workerenv.AIDeprecatedMemoryTools {
					count++
				}
			}
			parsed := workerenv.ParseAIWorkerEnv(func(name string) string { return values[name] })
			if count != 1 || !slices.Equal(parsed.DeprecatedMemoryTools, tc.want) {
				t.Fatalf("deprecation env count=%d tools=%#v, want %#v", count, parsed.DeprecatedMemoryTools, tc.want)
			}
			for _, name := range aitools.MemoryToolNames() {
				if !slices.Contains(parsed.Tools, name) {
					t.Fatalf("compatibility release omitted %q from %#v", name, parsed.Tools)
				}
			}
		})
	}
}
