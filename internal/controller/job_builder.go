/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

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

	// DefaultCopilotWorkerImage is the default image for Copilot agent tasks
	DefaultCopilotWorkerImage = "mercan-agent-worker-copilot:latest"

	// DefaultClaudeWorkerImage is the default image for Claude agent tasks
	DefaultClaudeWorkerImage = "mercan-agent-worker-claude:latest"

	// DefaultInitImage is the default image for init containers
	DefaultInitImage = "busybox:1.37"

	// ResultEndpointEnvVar is the env var for the result submission URL
	ResultEndpointEnvVar = "MERCAN_RESULT_ENDPOINT"

	// ControllerURLEnvVar is the env var for the controller base URL
	ControllerURLEnvVar = "MERCAN_CONTROLLER_URL"

	// TaskNameEnvVar is the env var for the task name
	TaskNameEnvVar = "MERCAN_TASK_NAME"

	// TaskNamespaceEnvVar is the env var for the task namespace
	TaskNamespaceEnvVar = "MERCAN_TASK_NAMESPACE"

	// defaultSecretKey is the default key name in provider secrets
	defaultSecretKey = "api-key"
)

// JobBuilder builds Kubernetes Jobs for Tasks
type JobBuilder struct {
	client.Client
	AIWorkerImage      string
	GeneralWorkerImage string
	CopilotWorkerImage string
	ClaudeWorkerImage  string
	InitImage          string
	ControllerURL      string // e.g. http://mercan-controller.mercan-system.svc:8080
}

// NewJobBuilder creates a new JobBuilder
func NewJobBuilder(c client.Client) *JobBuilder {
	return &JobBuilder{
		Client:             c,
		AIWorkerImage:      DefaultAIWorkerImage,
		GeneralWorkerImage: DefaultGeneralWorkerImage,
		CopilotWorkerImage: DefaultCopilotWorkerImage,
		ClaudeWorkerImage:  DefaultClaudeWorkerImage,
		InitImage:          DefaultInitImage,
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

	// Add agent-specific volumes (workspace, home)
	if task.Spec.Type == corev1alpha1.TaskTypeAgent {
		b.addAgentVolumes(job, task)
	}

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
		seconds := int64(task.Spec.Timeout.Seconds())
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
			if len(task.Spec.Command) > 0 {
				container.Command = task.Spec.Command
			}
			if len(task.Spec.Args) > 0 {
				container.Args = task.Spec.Args
			}
		} else {
			container.Image = b.GeneralWorkerImage
			container.Command = []string{"/worker"}
			// Pass the user command as args to the worker binary
			var workerArgs []string
			workerArgs = append(workerArgs, task.Spec.Command...)
			workerArgs = append(workerArgs, task.Spec.Args...)
			container.Args = workerArgs
		}
	case corev1alpha1.TaskTypeAgent:
		container.Image = b.getAgentWorkerImage(agent)
		container.Command = []string{"/worker"}
		container.Args = []string{"--mode=agent"}
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
			Name:  ResultEndpointEnvVar,
			Value: fmt.Sprintf("%s/internal/v1/results/%s/%s", b.ControllerURL, task.Namespace, task.Name),
		},
		{
			Name:  ControllerURLEnvVar,
			Value: b.ControllerURL,
		},
	}

	// Add task-level env vars
	envVars = append(envVars, task.Spec.Env...)

	// Add prior task env vars for iterative coordination
	if task.Spec.PriorTaskRef != nil {
		envVars = append(envVars,
			corev1.EnvVar{Name: "MERCAN_PRIOR_TASK", Value: task.Spec.PriorTaskRef.Name},
		)
		priorNS := task.Spec.PriorTaskRef.Namespace
		if priorNS == "" {
			priorNS = task.Namespace
		}
		envVars = append(envVars,
			corev1.EnvVar{Name: "MERCAN_PRIOR_TASK_NAMESPACE", Value: priorNS},
		)
	}

	// Add AI-specific env vars
	if task.Spec.Type == corev1alpha1.TaskTypeAI {
		envVars = b.addAIEnvVars(envVars, task, agent, provider)
	}

	// Add agent-specific env vars
	if task.Spec.Type == corev1alpha1.TaskTypeAgent {
		envVars = b.addAgentEnvVars(envVars, task, agent)
	}

	return envVars
}

// aiConfig holds resolved AI configuration from provider, agent, and task.
type aiConfig struct {
	providerType string
	model        string
	prompt       string
	systemPrompt string
	baseURL      string
	tools        []string
}

// resolveAIConfig merges AI configuration from provider, agent, and task (in priority order).
func resolveAIConfig(task *corev1alpha1.Task, agent *corev1alpha1.Agent, providerCRD *corev1alpha1.Provider) aiConfig {
	var cfg aiConfig

	// Get values from Provider CRD if present (lowest priority - defaults)
	if providerCRD != nil {
		cfg.providerType = string(providerCRD.Spec.Type)
		cfg.model = providerCRD.Spec.DefaultModel
		cfg.baseURL = providerCRD.Spec.BaseURL
	}

	// Get values from agent if present (overrides provider defaults)
	if agent != nil {
		if agent.Spec.Model != nil {
			if agent.Spec.Model.Provider != "" {
				cfg.providerType = agent.Spec.Model.Provider
			}
			if agent.Spec.Model.Name != "" {
				cfg.model = agent.Spec.Model.Name
			}
		}
		if agent.Spec.SystemPrompt != nil {
			cfg.systemPrompt = agent.Spec.SystemPrompt.Inline
		}
		for _, t := range agent.Spec.Tools {
			if t.Enabled == nil || *t.Enabled {
				cfg.tools = append(cfg.tools, t.Name)
			}
		}
	}

	// Override with task values if present (highest priority)
	if task.Spec.AI != nil {
		if task.Spec.AI.Provider != "" {
			cfg.providerType = task.Spec.AI.Provider
		}
		if task.Spec.AI.Model != "" {
			cfg.model = task.Spec.AI.Model
		}
		if task.Spec.AI.Prompt != "" {
			cfg.prompt = task.Spec.AI.Prompt
		}
		if task.Spec.AI.SystemPrompt != "" {
			cfg.systemPrompt = task.Spec.AI.SystemPrompt
		}
		if len(task.Spec.AI.Tools) > 0 {
			cfg.tools = append(cfg.tools, task.Spec.AI.Tools...)
		}
	}

	// Check task.Spec.Prompt (used with agentRef pattern)
	if cfg.prompt == "" && task.Spec.Prompt != "" {
		cfg.prompt = task.Spec.Prompt
	}

	// Provider CRD type is authoritative when resolved via providerRef.
	// model.provider and task AI provider are hints for when no Provider CRD exists.
	if providerCRD != nil {
		cfg.providerType = string(providerCRD.Spec.Type)
	}

	return cfg
}

// addCoordinationEnvVars appends coordination-related environment variables.
func (b *JobBuilder) addCoordinationEnvVars(envVars []corev1.EnvVar, task *corev1alpha1.Task, agent *corev1alpha1.Agent) []corev1.EnvVar {
	envVars = append(envVars,
		corev1.EnvVar{Name: "MERCAN_COORDINATION_ENABLED", Value: "true"},
		corev1.EnvVar{Name: "MERCAN_COORDINATION_MAX_DEPTH",
			Value: fmt.Sprintf("%d", agent.Spec.Coordination.MaxDepth)},
		corev1.EnvVar{Name: "MERCAN_COORDINATION_MAX_CHILDREN",
			Value: fmt.Sprintf("%d", agent.Spec.Coordination.MaxConcurrentChildren)},
	)

	agentNames := make([]string, 0, len(agent.Spec.Coordination.AllowedAgents))
	for _, a := range agent.Spec.Coordination.AllowedAgents {
		agentNames = append(agentNames, a.Name)
	}
	envVars = append(envVars,
		corev1.EnvVar{Name: "MERCAN_COORDINATION_ALLOWED_AGENTS",
			Value: strings.Join(agentNames, ",")},
	)

	// Current depth (0 for top-level coordinator)
	depth := "0"
	if d, ok := task.Annotations["mercan.ai/coordination-depth"]; ok {
		depth = d
	}
	envVars = append(envVars,
		corev1.EnvVar{Name: "MERCAN_COORDINATION_DEPTH", Value: depth},
	)

	return envVars
}

// addAIEnvVars adds AI-specific environment variables
func (b *JobBuilder) addAIEnvVars(envVars []corev1.EnvVar, task *corev1alpha1.Task, agent *corev1alpha1.Agent, providerCRD *corev1alpha1.Provider) []corev1.EnvVar {
	cfg := resolveAIConfig(task, agent, providerCRD)

	envVars = append(envVars,
		corev1.EnvVar{Name: "MERCAN_AI_PROVIDER", Value: cfg.providerType},
		corev1.EnvVar{Name: "MERCAN_AI_MODEL", Value: cfg.model},
		corev1.EnvVar{Name: "MERCAN_AI_PROMPT", Value: cfg.prompt},
		corev1.EnvVar{Name: "MERCAN_AI_SYSTEM_PROMPT", Value: cfg.systemPrompt},
	)

	if cfg.baseURL != "" {
		envVars = append(envVars, corev1.EnvVar{Name: "MERCAN_AI_BASE_URL", Value: cfg.baseURL})
	}

	// Auto-inject coordination tools when coordination is enabled
	if agent != nil && agent.Spec.Coordination != nil && agent.Spec.Coordination.Enabled {
		for _, ct := range []string{"delegate_task", "wait_for_tasks", "create_pull_request", "merge_pull_request", "auto_merge_pull_request", "review_pull_request", "post_review_comment", "create_agent", "delete_agent"} {
			if !slices.Contains(cfg.tools, ct) {
				cfg.tools = append(cfg.tools, ct)
			}
		}
	}

	if len(cfg.tools) > 0 {
		envVars = append(envVars, corev1.EnvVar{Name: "MERCAN_AI_TOOLS", Value: strings.Join(cfg.tools, ",")})
	}

	if agent != nil && agent.Spec.Coordination != nil && agent.Spec.Coordination.Enabled {
		envVars = b.addCoordinationEnvVars(envVars, task, agent)
	}

	// Add fallback provider environment variables
	if agent != nil && agent.Spec.Model != nil && len(agent.Spec.Model.Fallbacks) > 0 {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "MERCAN_AI_FALLBACK_COUNT",
			Value: fmt.Sprintf("%d", len(agent.Spec.Model.Fallbacks)),
		})
		for i, fb := range agent.Spec.Model.Fallbacks {
			// Resolve the fallback Provider CRD
			fbProvider := &corev1alpha1.Provider{}
			if err := b.Get(context.TODO(), client.ObjectKey{
				Namespace: task.Namespace,
				Name:      fb.ProviderRef,
			}, fbProvider); err != nil {
				continue // skip unresolvable fallbacks
			}
			prefix := fmt.Sprintf("MERCAN_AI_FALLBACK_%d", i)
			envVars = append(envVars,
				corev1.EnvVar{Name: prefix + "_PROVIDER", Value: string(fbProvider.Spec.Type)},
				corev1.EnvVar{Name: prefix + "_MODEL", Value: fb.Model},
			)
			if fbProvider.Spec.BaseURL != "" {
				envVars = append(envVars, corev1.EnvVar{Name: prefix + "_BASE_URL", Value: fbProvider.Spec.BaseURL})
			}
			if fbProvider.Spec.Azure != nil {
				envVars = append(envVars, corev1.EnvVar{Name: prefix + "_AZURE_API_VERSION", Value: fbProvider.Spec.Azure.APIVersion})
			}
		}
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
			secretKey = defaultSecretKey
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

	// Add fallback provider secrets
	if agent != nil && agent.Spec.Model != nil {
		for i, fb := range agent.Spec.Model.Fallbacks {
			fbProvider := &corev1alpha1.Provider{}
			if err := b.Get(context.TODO(), client.ObjectKey{
				Namespace: task.Namespace,
				Name:      fb.ProviderRef,
			}, fbProvider); err != nil {
				continue
			}
			secretName := fbProvider.Spec.SecretRef.Name
			secretKey := fbProvider.Spec.SecretRef.Key
			if secretKey == "" {
				secretKey = defaultSecretKey
			}
			envVarName := fmt.Sprintf("MERCAN_AI_FALLBACK_%d_API_KEY", i)
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
		// Inject all secret keys as environment variables
		job.Spec.Template.Spec.Containers[0].EnvFrom = append(
			job.Spec.Template.Spec.Containers[0].EnvFrom,
			corev1.EnvFromSource{
				SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				},
			},
		)
		// Also mount as files for tools that read from filesystem
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

// addSessionVolume adds a session emptyDir volume and init container to the Job.
// The init container fetches the session transcript from the controller via HTTP
// and writes it to /session/transcript.jsonl for the main worker container.
func (b *JobBuilder) addSessionVolume(job *batchv1.Job, task *corev1alpha1.Task) {
	sessionName := task.Spec.SessionRef.Name

	// Add shared emptyDir volume for session data
	job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
		Name: "session-data",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	})

	// Mount in the main worker container
	job.Spec.Template.Spec.Containers[0].VolumeMounts = append(
		job.Spec.Template.Spec.Containers[0].VolumeMounts,
		corev1.VolumeMount{
			Name:      "session-data",
			MountPath: "/session",
			ReadOnly:  true,
		},
	)

	// Build the transcript fetch URL
	transcriptURL := fmt.Sprintf("%s/internal/v1/sessions/%s/%s/transcript",
		b.ControllerURL, task.Namespace, sessionName)

	// Add init container that fetches the transcript via HTTP
	initContainer := corev1.Container{
		Name:            "fetch-session",
		Image:           b.InitImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: b.buildContainerSecurityContext(),
		Command: []string{"sh", "-c", fmt.Sprintf(
			`TOKEN=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token) && `+
				`wget --header="Authorization: Bearer $TOKEN" -q -O /session/transcript.jsonl "%s" || `+
				`touch /session/transcript.jsonl`,
			transcriptURL,
		)},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "session-data",
				MountPath: "/session",
			},
		},
	}

	job.Spec.Template.Spec.InitContainers = append(job.Spec.Template.Spec.InitContainers, initContainer)

	// Add session env vars
	job.Spec.Template.Spec.Containers[0].Env = append(
		job.Spec.Template.Spec.Containers[0].Env,
		corev1.EnvVar{Name: "MERCAN_SESSION_NAME", Value: sessionName},
	)
}

// getAgentWorkerImage returns the worker image for the given agent runtime type
func (b *JobBuilder) getAgentWorkerImage(agent *corev1alpha1.Agent) string {
	if agent == nil || agent.Spec.Runtime == nil {
		return b.ClaudeWorkerImage // fallback
	}
	switch agent.Spec.Runtime.Type {
	case corev1alpha1.AgentRuntimeCopilot:
		return b.CopilotWorkerImage
	case corev1alpha1.AgentRuntimeClaude:
		return b.ClaudeWorkerImage
	default:
		return b.ClaudeWorkerImage
	}
}

// addAgentEnvVars adds agent-runtime-specific environment variables
func (b *JobBuilder) addAgentEnvVars(envVars []corev1.EnvVar, task *corev1alpha1.Task, agent *corev1alpha1.Agent) []corev1.EnvVar {
	// Prompt (required)
	prompt := task.Spec.Prompt
	if prompt == "" && task.Spec.AI != nil {
		prompt = task.Spec.AI.Prompt
	}
	envVars = append(envVars, corev1.EnvVar{Name: "MERCAN_PROMPT", Value: prompt})

	envVars = b.addAgentModelEnvVars(envVars, agent)
	envVars = b.addAgentToolsEnvVars(envVars, task, agent)
	envVars = b.addAgentWorkspaceEnvVars(envVars, task)

	// Timeout (task level)
	if task.Spec.Timeout != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "MERCAN_TIMEOUT_SECONDS",
			Value: fmt.Sprintf("%d", int64(task.Spec.Timeout.Seconds())),
		})
	}

	return envVars
}

// addAgentModelEnvVars adds model and system prompt env vars from the Agent.
func (b *JobBuilder) addAgentModelEnvVars(envVars []corev1.EnvVar, agent *corev1alpha1.Agent) []corev1.EnvVar {
	if agent == nil {
		return envVars
	}
	if agent.Spec.Model != nil && agent.Spec.Model.Name != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name: "MERCAN_MODEL", Value: agent.Spec.Model.Name,
		})
	}
	if agent.Spec.SystemPrompt != nil && agent.Spec.SystemPrompt.Inline != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name: "MERCAN_SYSTEM_PROMPT", Value: agent.Spec.SystemPrompt.Inline,
		})
	}
	return envVars
}

// addAgentToolsEnvVars adds max turns, allowed/disallowed tools, and bash permission env vars.
func (b *JobBuilder) addAgentToolsEnvVars(
	envVars []corev1.EnvVar,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
) []corev1.EnvVar {
	// MaxTurns: task override > agent default > 50
	maxTurns := int32(50)
	if agent != nil && agent.Spec.Runtime != nil && agent.Spec.Runtime.DefaultMaxTurns != nil {
		maxTurns = *agent.Spec.Runtime.DefaultMaxTurns
	}
	if task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.MaxTurns != nil {
		maxTurns = *task.Spec.AgentRuntime.MaxTurns
	}
	envVars = append(envVars, corev1.EnvVar{
		Name: "MERCAN_MAX_TURNS", Value: fmt.Sprintf("%d", maxTurns),
	})

	// AllowedTools: task override > agent default
	var allowedTools []string
	if agent != nil && agent.Spec.Runtime != nil {
		allowedTools = agent.Spec.Runtime.DefaultAllowedTools
	}
	if task.Spec.AgentRuntime != nil && len(task.Spec.AgentRuntime.AllowedTools) > 0 {
		allowedTools = task.Spec.AgentRuntime.AllowedTools
	}
	if len(allowedTools) > 0 {
		envVars = append(envVars, corev1.EnvVar{
			Name: "MERCAN_ALLOWED_TOOLS", Value: joinStrings(allowedTools),
		})
	}

	// DisallowedTools (task only)
	if task.Spec.AgentRuntime != nil && len(task.Spec.AgentRuntime.DisallowedTools) > 0 {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "MERCAN_DISALLOWED_TOOLS",
			Value: joinStrings(task.Spec.AgentRuntime.DisallowedTools),
		})
	}

	// AllowBash: task override > agent default > false
	allowBash := false
	if agent != nil && agent.Spec.Runtime != nil {
		allowBash = agent.Spec.Runtime.DefaultAllowBash
	}
	if task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.AllowBash != nil {
		allowBash = *task.Spec.AgentRuntime.AllowBash
	}
	if allowBash {
		envVars = append(envVars, corev1.EnvVar{
			Name: "MERCAN_ALLOW_BASH", Value: "true",
		})
	}

	return envVars
}

// addAgentWorkspaceEnvVars adds workspace-related env vars from the task.
func (b *JobBuilder) addAgentWorkspaceEnvVars(
	envVars []corev1.EnvVar,
	task *corev1alpha1.Task,
) []corev1.EnvVar {
	if task.Spec.AgentRuntime == nil || task.Spec.AgentRuntime.Workspace == nil {
		return envVars
	}
	ws := task.Spec.AgentRuntime.Workspace
	if ws.GitRepo != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name: "MERCAN_GIT_REPO", Value: ws.GitRepo,
		})
	}
	if ws.Branch != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name: "MERCAN_GIT_BRANCH", Value: ws.Branch,
		})
	}
	if ws.Ref != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name: "MERCAN_GIT_REF", Value: ws.Ref,
		})
	}
	if ws.SubPath != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name: "MERCAN_WORKSPACE_SUBPATH", Value: ws.SubPath,
		})
	}
	if ws.ForkRepo != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name: "MERCAN_FORK_REPO", Value: ws.ForkRepo,
		})
	}
	if ws.PRBaseBranch != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name: "MERCAN_PR_BASE_BRANCH", Value: ws.PRBaseBranch,
		})
	}
	if ws.PushBranch != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name: "MERCAN_PUSH_BRANCH", Value: ws.PushBranch,
		})
	}
	return envVars
}

// addAgentVolumes adds agent-specific volumes to the Job (workspace, home)
func (b *JobBuilder) addAgentVolumes(job *batchv1.Job, task *corev1alpha1.Task) {
	// /workspace emptyDir for git clone target
	job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
		Name: "workspace",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	})
	job.Spec.Template.Spec.Containers[0].VolumeMounts = append(
		job.Spec.Template.Spec.Containers[0].VolumeMounts,
		corev1.VolumeMount{
			Name:      "workspace",
			MountPath: "/workspace",
		},
	)

	// /home/worker emptyDir for writable home (CLI config/cache)
	job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
		Name: "home",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	})
	job.Spec.Template.Spec.Containers[0].VolumeMounts = append(
		job.Spec.Template.Spec.Containers[0].VolumeMounts,
		corev1.VolumeMount{
			Name:      "home",
			MountPath: "/home/worker",
		},
	)

	// Git secret volume if referenced
	if task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.Workspace != nil && task.Spec.AgentRuntime.Workspace.GitSecretRef != nil {
		secretName := task.Spec.AgentRuntime.Workspace.GitSecretRef.Name
		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: "git-credentials",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: secretName,
				},
			},
		})
		job.Spec.Template.Spec.Containers[0].VolumeMounts = append(
			job.Spec.Template.Spec.Containers[0].VolumeMounts,
			corev1.VolumeMount{
				Name:      "git-credentials",
				MountPath: "/secrets/git",
				ReadOnly:  true,
			},
		)
	}
}

// joinStrings joins a string slice with commas
func joinStrings(s []string) string {
	var result strings.Builder
	for i, v := range s {
		if i > 0 {
			result.WriteString(",")
		}
		result.WriteString(v)
	}
	return result.String()
}
