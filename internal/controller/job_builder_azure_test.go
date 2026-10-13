/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestJobBuilderAzureDeploymentEnv(t *testing.T) {
	for _, deployment := range []string{"primary-slot", ""} {
		t.Run("deployment="+deployment, func(t *testing.T) {
			primary := &corev1alpha1.Provider{Spec: corev1alpha1.ProviderSpec{
				Type: corev1alpha1.ProviderTypeAzureOpenAI, DefaultModel: "provider-model",
				Azure: &corev1alpha1.AzureConfig{DeploymentName: deployment, APIVersion: "2024-06-01"},
			}}
			fallback := &corev1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "fallback", Namespace: "default"}, Spec: corev1alpha1.ProviderSpec{
				Type:  corev1alpha1.ProviderTypeAzureOpenAI,
				Azure: &corev1alpha1.AzureConfig{DeploymentName: "fallback-slot", APIVersion: "2024-10-21"},
			}}
			builder := setupJobBuilder()
			builder.Client = fake.NewClientBuilder().WithScheme(builder.Scheme()).WithObjects(fallback).Build()
			agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Model: &corev1alpha1.ModelConfig{
				Name: "agent-model", Fallbacks: []corev1alpha1.ModelFallback{{ProviderRef: "fallback", Model: "fallback-model"}},
			}}}
			task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default"}, Spec: corev1alpha1.TaskSpec{
				Type: corev1alpha1.TaskTypeAI, AI: &corev1alpha1.AISpec{Model: "task-model", Prompt: "hello"},
			}}
			job, err := builder.Build(t.Context(), task, agent, primary)
			require.NoError(t, err)
			values := map[string]string{}
			for _, envVar := range job.Spec.Template.Spec.Containers[0].Env {
				values[envVar.Name] = envVar.Value
			}
			assert.Equal(t, "task-model", values["ORKA_AI_MODEL"])
			assert.Equal(t, "fallback-model", values["ORKA_AI_FALLBACK_0_MODEL"])
			assert.Equal(t, deployment, values["ORKA_AI_AZURE_DEPLOYMENT_NAME"])
			assert.Equal(t, "fallback-slot", values["ORKA_AI_FALLBACK_0_AZURE_DEPLOYMENT_NAME"])
			assert.Equal(t, "2024-06-01", values["ORKA_AI_AZURE_API_VERSION"])
			assert.Equal(t, "2024-10-21", values["ORKA_AI_FALLBACK_0_AZURE_API_VERSION"])
			assert.Contains(t, values, "ORKA_AI_AZURE_DEPLOYMENT_NAME")
		})
	}
}

func TestJobBuilderAzureDeploymentEnvControllerOwned(t *testing.T) {
	for _, tt := range []struct {
		name string
		spec *corev1alpha1.ProviderSpec
		want string
	}{
		{
			name: "configured Azure deployment",
			spec: &corev1alpha1.ProviderSpec{
				Type:  corev1alpha1.ProviderTypeAzureOpenAI,
				Azure: &corev1alpha1.AzureConfig{DeploymentName: "configured-slot"},
			},
			want: "configured-slot",
		},
		{
			name: "omitted Azure deployment",
			spec: &corev1alpha1.ProviderSpec{
				Type:  corev1alpha1.ProviderTypeAzureOpenAI,
				Azure: &corev1alpha1.AzureConfig{APIVersion: "2024-06-01"},
			},
		},
		{
			name: "explicit empty Azure deployment",
			spec: &corev1alpha1.ProviderSpec{
				Type:  corev1alpha1.ProviderTypeAzureOpenAI,
				Azure: &corev1alpha1.AzureConfig{DeploymentName: ""},
			},
		},
		{
			name: "missing Azure config",
			spec: &corev1alpha1.ProviderSpec{Type: corev1alpha1.ProviderTypeAzureOpenAI},
		},
		{
			name: "non-Azure provider",
			spec: &corev1alpha1.ProviderSpec{Type: corev1alpha1.ProviderTypeOpenAI},
		},
		{name: "missing primary and unresolvable fallback providers"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, source := range []string{"none", "Task literal", "Task ValueFrom", "Task duplicates", "Agent Secret EnvFrom"} {
				t.Run(source, func(t *testing.T) {
					builder := setupJobBuilder()
					var primary *corev1alpha1.Provider
					if tt.spec != nil {
						primary = &corev1alpha1.Provider{Spec: *tt.spec}
						first := &corev1alpha1.Provider{
							ObjectMeta: metav1.ObjectMeta{Name: "fallback-first", Namespace: defaultNS},
							Spec:       *tt.spec,
						}
						second := &corev1alpha1.Provider{
							ObjectMeta: metav1.ObjectMeta{Name: "fallback-second", Namespace: defaultNS},
							Spec:       *tt.spec,
						}
						builder.Client = fake.NewClientBuilder().WithScheme(builder.Scheme()).WithObjects(first, second).Build()
					}
					agent := &corev1alpha1.Agent{
						ObjectMeta: metav1.ObjectMeta{Name: "deployment-agent", Namespace: defaultNS},
						Spec: corev1alpha1.AgentSpec{Model: &corev1alpha1.ModelConfig{
							Name: "agent-model",
							Fallbacks: []corev1alpha1.ModelFallback{
								{ProviderRef: "fallback-first", Model: "first-model"},
								{ProviderRef: "fallback-second", Model: "second-model"},
							},
						}},
					}
					task := &corev1alpha1.Task{
						ObjectMeta: metav1.ObjectMeta{Name: testTask, Namespace: defaultNS, UID: "deployment-task"},
						Spec: corev1alpha1.TaskSpec{
							Type: corev1alpha1.TaskTypeAI,
							AI:   &corev1alpha1.AISpec{Provider: "azure-openai", Model: "task-model", Prompt: "hello"},
						},
					}
					keys := []string{
						"ORKA_AI_AZURE_DEPLOYMENT_NAME",
						"ORKA_AI_FALLBACK_0_AZURE_DEPLOYMENT_NAME",
						"ORKA_AI_FALLBACK_1_AZURE_DEPLOYMENT_NAME",
					}
					for _, key := range keys {
						if source == "Task literal" || source == "Task duplicates" {
							task.Spec.Env = append(task.Spec.Env, corev1.EnvVar{Name: key, Value: "spoofed-slot"})
						}
						if source == "Task ValueFrom" || source == "Task duplicates" {
							task.Spec.Env = append(task.Spec.Env, corev1.EnvVar{
								Name: key,
								ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "task-deployments"}, Key: key,
								}},
							})
						}
					}
					if source == "Agent Secret EnvFrom" {
						agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: testAgentSecretName}
					}

					job, err := builder.Build(t.Context(), task, agent, primary)
					require.NoError(t, err)
					container := job.Spec.Template.Spec.Containers[0]
					if source == "Agent Secret EnvFrom" {
						require.True(t, hasAgentEnvFromSecret(container.EnvFrom), "missing direct Agent Secret EnvFrom")
					}
					// Explicit Env entries override same-named keys in Agent Secret EnvFrom.
					for _, key := range keys {
						var entries []corev1.EnvVar
						for _, envVar := range container.Env {
							if envVar.Name == key {
								entries = append(entries, envVar)
							}
						}
						assert.Equal(t, []corev1.EnvVar{{Name: key, Value: tt.want}}, entries, key)
					}
					assert.Contains(t, container.Env, corev1.EnvVar{Name: "ORKA_AI_MODEL", Value: "task-model"})
					if tt.spec != nil {
						assert.Contains(t, container.Env, corev1.EnvVar{Name: "ORKA_AI_FALLBACK_0_MODEL", Value: "first-model"})
						assert.Contains(t, container.Env, corev1.EnvVar{Name: "ORKA_AI_FALLBACK_1_MODEL", Value: "second-model"})
					}
				})
			}
		})
	}
}
