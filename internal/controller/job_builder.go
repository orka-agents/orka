/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
)

const (
	// DefaultAIWorkerImage is the default image for AI tasks
	DefaultAIWorkerImage = "mercan-ai-worker:latest"

	// DefaultGeneralWorkerImage is the default image for container tasks
	DefaultGeneralWorkerImage = "mercan-general-worker:latest"

	// ResultConfigMapEnvVar is the env var for result ConfigMap name
	ResultConfigMapEnvVar = "MERCAN_RESULT_CONFIGMAP"

	// TaskNameEnvVar is the env var for the task name
	TaskNameEnvVar = "MERCAN_TASK_NAME"

	// TaskNamespaceEnvVar is the env var for the task namespace
	TaskNamespaceEnvVar = "MERCAN_TASK_NAMESPACE"
)

// JobBuilder builds Kubernetes Jobs for Tasks
type JobBuilder struct {
	client.Client
	AIWorkerImage      string
	GeneralWorkerImage string
}

// NewJobBuilder creates a new JobBuilder
func NewJobBuilder(c client.Client) *JobBuilder {
	return &JobBuilder{
		Client:             c,
		AIWorkerImage:      DefaultAIWorkerImage,
		GeneralWorkerImage: DefaultGeneralWorkerImage,
	}
}

// Build creates a Job for the given Task
func (b *JobBuilder) Build(ctx context.Context, task *corev1alpha1.Task, agent *corev1alpha1.Agent, provider *corev1alpha1.Provider) (*batchv1.Job, error) {
	jobName := fmt.Sprintf("%s-job-%s", task.Name, task.UID[:8])

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: task.Namespace,
			Labels: map[string]string{
				"mercan.ai/task":      task.Name,
				"mercan.ai/task-type": string(task.Spec.Type),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(0)), // No retries at Job level, we handle retries in the controller
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"mercan.ai/task":      task.Name,
						"mercan.ai/task-type": string(task.Spec.Type),
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: "mercan-worker",
					SecurityContext:    b.buildPodSecurityContext(),
					Containers: []corev1.Container{
						b.buildContainer(task, agent, provider),
					},
				},
			},
		},
	}

	// Always add tmp volume for read-only root filesystem
	job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
		Name: "tmp",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	})

	// Add secret volumes if needed
	if task.Spec.SecretRef != nil || (agent != nil && agent.Spec.SecretRef != nil) || provider != nil {
		b.addSecretVolumes(job, task, agent, provider)
	}

	// Add session volume if needed
	if task.Spec.SessionRef != nil {
		b.addSessionVolume(job, task)
	}

	// Set active deadline if timeout is specified
	if task.Spec.Timeout != nil {
		seconds := int64(task.Spec.Timeout.Duration.Seconds())
		job.Spec.ActiveDeadlineSeconds = &seconds
	}

	return job, nil
}

// buildPodSecurityContext builds a secure pod security context
func (b *JobBuilder) buildPodSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot: ptr.To(true),
		RunAsUser:    ptr.To(int64(1000)),
		RunAsGroup:   ptr.To(int64(1000)),
		FSGroup:      ptr.To(int64(1000)),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

// buildContainerSecurityContext builds a secure container security context
func (b *JobBuilder) buildContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem:   ptr.To(true),
		RunAsNonRoot:             ptr.To(true),
		RunAsUser:                ptr.To(int64(1000)),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
}

// buildContainer builds the main container for the Job
func (b *JobBuilder) buildContainer(task *corev1alpha1.Task, agent *corev1alpha1.Agent, provider *corev1alpha1.Provider) corev1.Container {
	container := corev1.Container{
		Name:            "worker",
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: b.buildContainerSecurityContext(),
		Resources:       b.buildResources(task, agent),
		Env:             b.buildEnvVars(task, agent, provider),
		VolumeMounts:    []corev1.VolumeMount{},
	}

	// Set image and command based on task type
	switch task.Spec.Type {
	case corev1alpha1.TaskTypeAI:
		container.Image = b.AIWorkerImage
		container.Command = []string{"/worker"}
		container.Args = []string{"--mode=ai"}
	case corev1alpha1.TaskTypeContainer:
		if task.Spec.Image != "" {
			container.Image = task.Spec.Image
		} else {
			container.Image = b.GeneralWorkerImage
		}
		if len(task.Spec.Command) > 0 {
			container.Command = task.Spec.Command
		}
		if len(task.Spec.Args) > 0 {
			container.Args = task.Spec.Args
		}
	}

	// Add tmp volume mount for read-only root filesystem
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
		Name:      "tmp",
		MountPath: "/tmp",
	})

	return container
}

// buildResources builds the resource requirements
func (b *JobBuilder) buildResources(task *corev1alpha1.Task, agent *corev1alpha1.Agent) corev1.ResourceRequirements {
	// Use task resources if specified
	if task.Spec.Resources.Limits != nil || task.Spec.Resources.Requests != nil {
		return task.Spec.Resources
	}

	// Use agent resources if specified
	if agent != nil && (agent.Spec.Resources.Limits != nil || agent.Spec.Resources.Requests != nil) {
		return agent.Spec.Resources
	}

	// Default resources
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
}

// buildEnvVars builds the environment variables for the container
func (b *JobBuilder) buildEnvVars(task *corev1alpha1.Task, agent *corev1alpha1.Agent, provider *corev1alpha1.Provider) []corev1.EnvVar {
	envVars := []corev1.EnvVar{
		{
			Name:  TaskNameEnvVar,
			Value: task.Name,
		},
		{
			Name:  TaskNamespaceEnvVar,
			Value: task.Namespace,
		},
		{
			Name:  ResultConfigMapEnvVar,
			Value: fmt.Sprintf("%s-result", task.Name),
		},
	}

	// Add task-level env vars
	envVars = append(envVars, task.Spec.Env...)

	// Add AI-specific env vars
	if task.Spec.Type == corev1alpha1.TaskTypeAI {
		envVars = b.addAIEnvVars(envVars, task, agent, provider)
	}

	return envVars
}

// addAIEnvVars adds AI-specific environment variables
func (b *JobBuilder) addAIEnvVars(envVars []corev1.EnvVar, task *corev1alpha1.Task, agent *corev1alpha1.Agent, providerCRD *corev1alpha1.Provider) []corev1.EnvVar {
	var providerType, model, prompt, systemPrompt, baseURL string
	var tools []string

	// Get values from Provider CRD if present (lowest priority - defaults)
	if providerCRD != nil {
		providerType = string(providerCRD.Spec.Type)
		model = providerCRD.Spec.DefaultModel
		baseURL = providerCRD.Spec.BaseURL
	}

	// Get values from agent if present (overrides provider defaults)
	if agent != nil {
		if agent.Spec.Model != nil {
			if agent.Spec.Model.Provider != "" {
				providerType = agent.Spec.Model.Provider
			}
			if agent.Spec.Model.Name != "" {
				model = agent.Spec.Model.Name
			}
		}
		if agent.Spec.SystemPrompt != nil {
			systemPrompt = agent.Spec.SystemPrompt.Inline
		}
		for _, t := range agent.Spec.Tools {
			if t.Enabled == nil || *t.Enabled {
				tools = append(tools, t.Name)
			}
		}
	}

	// Override with task values if present (highest priority)
	if task.Spec.AI != nil {
		if task.Spec.AI.Provider != "" {
			providerType = task.Spec.AI.Provider
		}
		if task.Spec.AI.Model != "" {
			model = task.Spec.AI.Model
		}
		if task.Spec.AI.Prompt != "" {
			prompt = task.Spec.AI.Prompt
		} else if task.Spec.Prompt != "" {
			prompt = task.Spec.Prompt
		}
		if task.Spec.AI.SystemPrompt != "" {
			systemPrompt = task.Spec.AI.SystemPrompt
		}
		if len(task.Spec.AI.Tools) > 0 {
			tools = append(tools, task.Spec.AI.Tools...)
		}
	}

	envVars = append(envVars,
		corev1.EnvVar{Name: "MERCAN_AI_PROVIDER", Value: providerType},
		corev1.EnvVar{Name: "MERCAN_AI_MODEL", Value: model},
		corev1.EnvVar{Name: "MERCAN_AI_PROMPT", Value: prompt},
		corev1.EnvVar{Name: "MERCAN_AI_SYSTEM_PROMPT", Value: systemPrompt},
	)

	// Add base URL if configured
	if baseURL != "" {
		envVars = append(envVars, corev1.EnvVar{Name: "MERCAN_AI_BASE_URL", Value: baseURL})
	}

	// Add tools as comma-separated list
	if len(tools) > 0 {
		toolsStr := ""
		for i, t := range tools {
			if i > 0 {
				toolsStr += ","
			}
			toolsStr += t
		}
		envVars = append(envVars, corev1.EnvVar{Name: "MERCAN_AI_TOOLS", Value: toolsStr})
	}

	return envVars
}

// addSecretVolumes adds secret volumes to the Job
func (b *JobBuilder) addSecretVolumes(job *batchv1.Job, task *corev1alpha1.Task, agent *corev1alpha1.Agent, provider *corev1alpha1.Provider) {
	// Add provider secret (mounted as environment variable source)
	if provider != nil {
		secretName := provider.Spec.SecretRef.Name
		secretKey := provider.Spec.SecretRef.Key
		if secretKey == "" {
			secretKey = "api-key"
		}

		// Determine the env var name based on provider type
		envVarName := "ANTHROPIC_API_KEY"
		if provider.Spec.Type == corev1alpha1.ProviderTypeOpenAI || provider.Spec.Type == corev1alpha1.ProviderTypeAzureOpenAI {
			envVarName = "OPENAI_API_KEY"
		}

		// Add API key as environment variable from secret
		job.Spec.Template.Spec.Containers[0].Env = append(
			job.Spec.Template.Spec.Containers[0].Env,
			corev1.EnvVar{
				Name: envVarName,
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: secretName,
						},
						Key: secretKey,
					},
				},
			},
		)
	}

	// Add task secret
	if task.Spec.SecretRef != nil {
		secretName := task.Spec.SecretRef.Name
		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: "task-secrets",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: secretName,
				},
			},
		})
		job.Spec.Template.Spec.Containers[0].VolumeMounts = append(
			job.Spec.Template.Spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{
				Name:      "task-secrets",
				MountPath: "/secrets/task",
				ReadOnly:  true,
			},
		)
	}

	// Add agent secret
	if agent != nil && agent.Spec.SecretRef != nil {
		secretName := agent.Spec.SecretRef.Name
		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: "agent-secrets",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: secretName,
				},
			},
		})
		job.Spec.Template.Spec.Containers[0].VolumeMounts = append(
			job.Spec.Template.Spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{
				Name:      "agent-secrets",
				MountPath: "/secrets/agent",
				ReadOnly:  true,
			},
		)
	}
}

// addSessionVolume adds session ConfigMap volume to the Job
func (b *JobBuilder) addSessionVolume(job *batchv1.Job, task *corev1alpha1.Task) {
	sessionCMName := fmt.Sprintf("session-%s", task.Spec.SessionRef.Name)

	job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
		Name: "session",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: sessionCMName,
				},
				Optional: ptr.To(true), // Session might not exist yet
			},
		},
	})

	job.Spec.Template.Spec.Containers[0].VolumeMounts = append(
		job.Spec.Template.Spec.Containers[0].VolumeMounts,
		corev1.VolumeMount{
			Name:      "session",
			MountPath: "/session",
			ReadOnly:  true,
		},
	)

	// Add session env vars
	job.Spec.Template.Spec.Containers[0].Env = append(
		job.Spec.Template.Spec.Containers[0].Env,
		corev1.EnvVar{Name: "MERCAN_SESSION_NAME", Value: task.Spec.SessionRef.Name},
		corev1.EnvVar{Name: "MERCAN_SESSION_CONFIGMAP", Value: sessionCMName},
	)
}
