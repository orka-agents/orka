/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestJobBuilder_AIModelSettingsEnv(t *testing.T) {
	const temperatureKey = "ORKA_AI_TEMPERATURE"
	const maxTokensKey = "ORKA_AI_MAX_TOKENS"

	for _, tt := range []struct {
		name        string
		noAgent     bool
		model       *corev1alpha1.ModelConfig
		temperature string
		maxTokens   string
	}{
		{name: "no Agent", noAgent: true},
		{name: "no model"},
		{name: "no settings", model: &corev1alpha1.ModelConfig{}},
		{
			name: "explicit zero and small cap",
			model: &corev1alpha1.ModelConfig{
				Temperature: new(0.0), MaxTokens: new(int32(256)),
			},
			temperature: "0", maxTokens: "256",
		},
		{
			name: "positive temperature and large cap",
			model: &corev1alpha1.ModelConfig{
				Temperature: new(0.123456789), MaxTokens: new(int32(8192)),
			},
			temperature: "0.123456789", maxTokens: "8192",
		},
		{
			name:        "temperature only",
			model:       &corev1alpha1.ModelConfig{Temperature: new(0.5)},
			temperature: "0.5",
		},
		{
			name:        "zero temperature only",
			model:       &corev1alpha1.ModelConfig{Temperature: new(0.0)},
			temperature: "0",
		},
		{
			name:      "maxTokens only",
			model:     &corev1alpha1.ModelConfig{MaxTokens: new(int32(256))},
			maxTokens: "256",
		},
		{
			name:      "zero maxTokens",
			model:     &corev1alpha1.ModelConfig{MaxTokens: new(int32(0))},
			maxTokens: "0",
		},
		{
			name:      "negative maxTokens",
			model:     &corev1alpha1.ModelConfig{MaxTokens: new(int32(-256))},
			maxTokens: "-256",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, source := range []string{"none", "Task literal", "Task ValueFrom", "Task duplicates", "Agent Secret EnvFrom"} {
				if tt.noAgent && source == "Agent Secret EnvFrom" {
					continue
				}
				t.Run(source, func(t *testing.T) {
					builder := setupJobBuilder()
					task := &corev1alpha1.Task{
						ObjectMeta: metav1.ObjectMeta{Name: testTask, Namespace: defaultNS, UID: "model-settings-task"},
						Spec: corev1alpha1.TaskSpec{
							Type: corev1alpha1.TaskTypeAI,
							AI:   &corev1alpha1.AISpec{Provider: "openai", Model: "task-model", Prompt: "hello"},
						},
					}
					var agent *corev1alpha1.Agent
					if !tt.noAgent {
						agent = &corev1alpha1.Agent{
							ObjectMeta: metav1.ObjectMeta{Name: "model-settings-agent", Namespace: defaultNS},
							Spec:       corev1alpha1.AgentSpec{Model: tt.model},
						}
					}
					for _, key := range []string{temperatureKey, maxTokensKey} {
						if source == "Task literal" || source == "Task duplicates" {
							task.Spec.Env = append(task.Spec.Env, corev1.EnvVar{Name: key, Value: "123"})
						}
						if source == "Task ValueFrom" || source == "Task duplicates" {
							task.Spec.Env = append(task.Spec.Env, corev1.EnvVar{
								Name: key,
								ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "task-settings"}, Key: key,
								}},
							})
						}
					}

					var envVars []corev1.EnvVar
					if source == "Agent Secret EnvFrom" {
						agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: testAgentSecretName}
						job, err := builder.Build(context.Background(), task, agent, nil)
						if err != nil {
							t.Fatalf("Build() error = %v", err)
						}
						container := job.Spec.Template.Spec.Containers[0]
						if !hasAgentEnvFromSecret(container.EnvFrom) {
							t.Fatal("missing direct Agent Secret EnvFrom")
						}
						// Explicit Env entries take precedence over any same-named Secret keys.
						envVars = container.Env
					} else {
						envVars = builder.buildEnvVars(context.Background(), task, agent, nil)
					}
					for key, want := range map[string]string{temperatureKey: tt.temperature, maxTokensKey: tt.maxTokens} {
						count := 0
						for _, envVar := range envVars {
							if envVar.Name != key {
								continue
							}
							count++
							if envVar.Value != want || envVar.ValueFrom != nil {
								t.Errorf("%s = %#v, want explicit controller-owned value %q", key, envVar, want)
							}
						}
						if count != 1 {
							t.Errorf("%s has %d entries, want exactly one", key, count)
						}
					}
				})
			}
		})
	}
}
