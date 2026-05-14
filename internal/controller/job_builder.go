/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"maps"
	"path"
	"reflect"
	"slices"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/metrics"
	"github.com/sozercan/orka/internal/taskmeta"
	"github.com/sozercan/orka/internal/workerenv"
)

const (
	// DefaultAIWorkerImage is the default image for AI tasks
	DefaultAIWorkerImage = "ghcr.io/sozercan/orka/ai-worker:latest"

	// DefaultGeneralWorkerImage is the default image for container tasks
	DefaultGeneralWorkerImage = "ghcr.io/sozercan/orka/general-worker:latest"

	// DefaultCopilotWorkerImage is the default image for Copilot agent tasks
	DefaultCopilotWorkerImage = "ghcr.io/sozercan/orka/agent-worker-copilot:latest"

	// DefaultClaudeWorkerImage is the default image for Claude agent tasks
	DefaultClaudeWorkerImage = "ghcr.io/sozercan/orka/agent-worker-claude:latest"

	// DefaultCodexWorkerImage is the default image for Codex agent tasks
	DefaultCodexWorkerImage = "ghcr.io/sozercan/orka/agent-worker-codex:latest"

	// DefaultInitImage is the default image for init containers
	DefaultInitImage = "busybox:1.37"

	// ResultEndpointEnvVar is the env var for the result submission URL
	ResultEndpointEnvVar = workerenv.ResultEndpoint

	// ControllerURLEnvVar is the env var for the controller base URL
	ControllerURLEnvVar = workerenv.ControllerURL

	// TaskNameEnvVar is the env var for the task name
	TaskNameEnvVar = workerenv.TaskName

	// TaskNamespaceEnvVar is the env var for the task namespace
	TaskNamespaceEnvVar = workerenv.TaskNamespace

	// defaultSecretKey is the default key name in provider secrets
	defaultSecretKey = "api-key"

	// Kubernetes Job names end up mirrored into pod labels like `job-name`,
	// which are capped at 63 characters.
	maxJobNameLength = 63
)

// JobBuilder builds Kubernetes Jobs for Tasks
type JobBuilder struct {
	client.Client
	AIWorkerImage                string
	GeneralWorkerImage           string
	CopilotWorkerImage           string
	ClaudeWorkerImage            string
	CodexWorkerImage             string
	CodexSandboxMode             string
	InitImage                    string
	ControllerURL                string // e.g. http://orka-controller.orka-system.svc:8080
	ContextTokenTTSURL           string
	ContextTokenSubjectTokenType string
	ContextTokenChildScope       string
	ContextTokenOutboundScope    string
}

// NewJobBuilder creates a new JobBuilder
func NewJobBuilder(c client.Client) *JobBuilder {
	return &JobBuilder{
		Client:             c,
		AIWorkerImage:      DefaultAIWorkerImage,
		GeneralWorkerImage: DefaultGeneralWorkerImage,
		CopilotWorkerImage: DefaultCopilotWorkerImage,
		ClaudeWorkerImage:  DefaultClaudeWorkerImage,
		CodexWorkerImage:   DefaultCodexWorkerImage,
		InitImage:          DefaultInitImage,
	}
}

func buildTaskJobName(task *corev1alpha1.Task) string {
	uidPrefix := string(task.UID)
	if len(uidPrefix) > 8 {
		uidPrefix = uidPrefix[:8]
	}
	suffix := fmt.Sprintf("-job-%s-%d", uidPrefix, task.Status.Attempts)
	maxPrefixLength := max(1, maxJobNameLength-len(suffix))

	prefix := task.Name
	if len(prefix) > maxPrefixLength {
		prefix = strings.Trim(prefix[:maxPrefixLength], "-")
		if prefix == "" {
			prefix = "task"
		}
	}

	return prefix + suffix
}

// Build creates a Job for the given Task
func (b *JobBuilder) Build(ctx context.Context, task *corev1alpha1.Task, agent *corev1alpha1.Agent, provider *corev1alpha1.Provider) (*batchv1.Job, error) {
	jobName := buildTaskJobName(task)
	execution := resolveExecution(task, agent)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: task.Namespace,
			Labels: map[string]string{
				labels.LabelTask:     labels.SelectorValue(task.Name),
				labels.LabelTaskType: string(task.Spec.Type),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(0)), // No retries at Job level, we handle retries in the controller
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						labels.LabelTask:     labels.SelectorValue(task.Name),
						labels.LabelTaskType: string(task.Spec.Type),
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: "orka-worker",
					SecurityContext:    b.buildPodSecurityContext(),
					Containers: []corev1.Container{
						b.buildContainer(ctx, task, agent, provider),
					},
				},
			},
		},
	}

	taskmeta.ApplyTransactionMetadata(&job.ObjectMeta, task.Spec.Transaction)
	taskmeta.ApplyTransactionMetadata(&job.Spec.Template.ObjectMeta, task.Spec.Transaction)

	applyExecution(job, execution)

	// Always add tmp volume for read-only root filesystem
	job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
		Name: "tmp",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	})

	b.addTransactionTokenSecret(job, task)

	// Add workspace/home volumes for tasks that need a git workspace.
	if taskNeedsWorkspace(task) {
		b.addWorkspaceVolumes(job, task)
	}

	if task.Spec.Type == corev1alpha1.TaskTypeContainer && effectiveWorkspace(task) != nil && task.Spec.Image != "" {
		b.addWorkspaceInitContainer(job, task)
	}

	// Add skill volumes — read Skill CRs, create ConfigMap, mount at /workspace/.skills/
	if err := b.addSkillVolumes(ctx, job, task, agent); err != nil {
		return nil, fmt.Errorf("failed to add skill volumes: %w", err)
	}

	// Add secret volumes if needed
	if task.Spec.SecretRef != nil || (agent != nil && agent.Spec.SecretRef != nil) || provider != nil {
		b.addSecretVolumes(ctx, job, task, agent, provider)
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
func (b *JobBuilder) buildContainer(ctx context.Context, task *corev1alpha1.Task, agent *corev1alpha1.Agent, provider *corev1alpha1.Provider) corev1.Container {
	container := corev1.Container{
		Name:            "worker",
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: b.buildContainerSecurityContext(),
		Resources:       b.buildResources(task, agent),
		Env:             b.buildEnvVars(ctx, task, agent, provider),
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
			if effectiveWorkspace(task) != nil {
				container.WorkingDir = workspaceWorkingDir(task)
				if !envVarExists(container.Env, "HOME") {
					container.Env = append(container.Env, corev1.EnvVar{Name: "HOME", Value: "/home/worker"})
				}
			}
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
			workerArgs := make([]string, 0, len(task.Spec.Command)+len(task.Spec.Args))
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

func resolveExecution(task *corev1alpha1.Task, agent *corev1alpha1.Agent) *corev1alpha1.ExecutionSpec {
	var effective corev1alpha1.ExecutionSpec

	if agent != nil && agent.Spec.Execution != nil {
		effective.RuntimeClassName = agent.Spec.Execution.RuntimeClassName
		effective.NodeSelector = copyNodeSelector(agent.Spec.Execution.NodeSelector)
		effective.Tolerations = copyTolerations(agent.Spec.Execution.Tolerations)
		if agent.Spec.Execution.Affinity != nil {
			effective.Affinity = agent.Spec.Execution.Affinity.DeepCopy()
		}
	}

	if task != nil && task.Spec.Execution != nil {
		if task.Spec.Execution.RuntimeClassName != "" {
			effective.RuntimeClassName = task.Spec.Execution.RuntimeClassName
		}
		if task.Spec.Execution.NodeSelector != nil {
			effective.NodeSelector = copyNodeSelector(task.Spec.Execution.NodeSelector)
		}
		if task.Spec.Execution.Tolerations != nil {
			effective.Tolerations = copyTolerations(task.Spec.Execution.Tolerations)
		}
		if task.Spec.Execution.Affinity != nil {
			effective.Affinity = task.Spec.Execution.Affinity.DeepCopy()
		}
	}

	if effective.RuntimeClassName == "" && len(effective.NodeSelector) == 0 && len(effective.Tolerations) == 0 && effective.Affinity == nil {
		return nil
	}

	return &effective
}

func applyExecution(job *batchv1.Job, execution *corev1alpha1.ExecutionSpec) {
	if job == nil || execution == nil {
		return
	}

	if execution.RuntimeClassName != "" {
		job.Spec.Template.Spec.RuntimeClassName = ptr.To(execution.RuntimeClassName)
	}
	if len(execution.NodeSelector) > 0 {
		job.Spec.Template.Spec.NodeSelector = copyNodeSelector(execution.NodeSelector)
	}
	if len(execution.Tolerations) > 0 {
		job.Spec.Template.Spec.Tolerations = copyTolerations(execution.Tolerations)
	}
	if execution.Affinity != nil {
		job.Spec.Template.Spec.Affinity = execution.Affinity.DeepCopy()
	}
}

func copyNodeSelector(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}

	return maps.Clone(in)
}

func copyTolerations(in []corev1.Toleration) []corev1.Toleration {
	if in == nil {
		return nil
	}

	out := make([]corev1.Toleration, len(in))
	copy(out, in)

	return out
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
func (b *JobBuilder) buildEnvVars(ctx context.Context, task *corev1alpha1.Task, agent *corev1alpha1.Agent, provider *corev1alpha1.Provider) []corev1.EnvVar {
	envVars := workerenv.BaseEnv{
		TaskName:       task.Name,
		TaskNamespace:  task.Namespace,
		ResultEndpoint: fmt.Sprintf("%s/internal/v1/results/%s/%s", b.ControllerURL, task.Namespace, task.Name),
		ControllerURL:  b.ControllerURL,
	}.EnvVars()

	// Add task-level env vars
	envVars = append(envVars, task.Spec.Env...)
	envVars = addTransactionEnvVars(envVars, task.Spec.Transaction)

	// Add prior task env vars for iterative coordination
	if task.Spec.PriorTaskRef != nil {
		envVars = append(envVars,
			corev1.EnvVar{Name: workerenv.PriorTask, Value: task.Spec.PriorTaskRef.Name},
		)
		priorNS := task.Spec.PriorTaskRef.Namespace
		if priorNS == "" {
			priorNS = task.Namespace
		}
		envVars = append(envVars,
			corev1.EnvVar{Name: workerenv.PriorTaskNamespace, Value: priorNS},
		)
	}

	// Add parent task env var for inter-agent messaging
	if parentTask := labels.ParentTaskName(task.Labels, task.Annotations); parentTask != "" {
		envVars = append(envVars,
			corev1.EnvVar{Name: workerenv.ParentTask, Value: parentTask},
		)
	}

	// Add AI-specific env vars
	if task.Spec.Type == corev1alpha1.TaskTypeAI {
		envVars = b.addAIEnvVars(ctx, envVars, task, agent, provider)
	}

	// Add agent-specific env vars
	if task.Spec.Type == corev1alpha1.TaskTypeAgent {
		envVars = b.addAgentEnvVars(ctx, envVars, task, agent)
		envVars = b.addCodexSandboxEnvVars(envVars, agent)
	}

	if task.Spec.Type == corev1alpha1.TaskTypeContainer {
		envVars = b.addWorkspaceEnvVars(envVars, task)
	}

	return envVars
}

func addTransactionEnvVars(envVars []corev1.EnvVar, tx *corev1alpha1.TaskTransaction) []corev1.EnvVar {
	if tx == nil {
		return envVars
	}
	envVars = workerenv.AppendIfSet(envVars, workerenv.TransactionID, tx.ID)
	envVars = workerenv.AppendIfSet(envVars, workerenv.TransactionProfile, tx.Profile)
	envVars = workerenv.AppendIfSet(envVars, workerenv.TransactionIssuer, tx.Issuer)
	envVars = workerenv.AppendIfSet(envVars, workerenv.TransactionSubject, tx.Subject)
	envVars = workerenv.AppendIfSet(envVars, workerenv.TransactionRequestingWorkload, tx.RequestingWorkload)
	envVars = workerenv.AppendIfSet(envVars, workerenv.TransactionScope, tx.Scope)
	envVars = workerenv.AppendIfSet(envVars, workerenv.TransactionScopes, workerenv.JoinCSV(tx.Scopes))
	envVars = workerenv.AppendIfSet(envVars, workerenv.TransactionContextDigest, tx.ContextDigest)
	envVars = workerenv.AppendIfSet(envVars, workerenv.TransactionRequesterContextDigest, tx.RequesterContextDigest)
	return envVars
}

// aiConfig holds resolved AI configuration from provider, agent, and task.
type aiConfig struct {
	providerType    string
	model           string
	prompt          string
	systemPrompt    string
	baseURL         string
	azureAPIVersion string
	tools           []string
}

// resolveAIConfig merges AI configuration from provider, agent, and task (in priority order).
func resolveAIConfig(task *corev1alpha1.Task, agent *corev1alpha1.Agent, providerCRD *corev1alpha1.Provider) aiConfig {
	var cfg aiConfig

	// Get values from Provider CRD if present (lowest priority - defaults)
	if providerCRD != nil {
		cfg.providerType = string(providerCRD.Spec.Type)
		cfg.model = providerCRD.Spec.DefaultModel
		cfg.baseURL = providerCRD.Spec.BaseURL
		if providerCRD.Spec.Azure != nil {
			cfg.azureAPIVersion = providerCRD.Spec.Azure.APIVersion
		}
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
	agentNames := make([]string, 0, len(agent.Spec.Coordination.AllowedAgents))
	for _, a := range agent.Spec.Coordination.AllowedAgents {
		agentNames = append(agentNames, a.Name)
	}

	// Current depth (0 for top-level coordinator)
	depth := "0"
	if d, ok := task.Annotations[labels.AnnotationCoordinationDepth]; ok {
		depth = d
	}

	return append(envVars, workerenv.CoordinationEnv{
		Enabled:                 true,
		MaxDepth:                int(agent.Spec.Coordination.MaxDepth),
		MaxChildren:             int(agent.Spec.Coordination.MaxConcurrentChildren),
		AllowedAgents:           agentNames,
		Depth:                   depth,
		AutonomousMode:          agent.Spec.Coordination.Autonomous,
		AutonomousIteration:     int(task.Status.Iteration),
		AutonomousMaxIterations: int(agent.Spec.Coordination.MaxIterations),
	}.EnvVars()...)
}

// addAIEnvVars adds AI-specific environment variables
func (b *JobBuilder) addAIEnvVars(ctx context.Context, //nolint:gocyclo
	envVars []corev1.EnvVar, task *corev1alpha1.Task, agent *corev1alpha1.Agent, providerCRD *corev1alpha1.Provider) []corev1.EnvVar {
	cfg := resolveAIConfig(task, agent, providerCRD)

	// Resolve system prompt from ConfigMapRef if not already set inline
	if cfg.systemPrompt == "" && agent != nil && agent.Spec.SystemPrompt != nil && agent.Spec.SystemPrompt.ConfigMapRef != nil {
		cfg.systemPrompt = b.resolveConfigMapValue(ctx, agent.Namespace, agent.Spec.SystemPrompt.ConfigMapRef)
	}

	envVars = append(envVars, workerenv.AIWorkerEnv{
		Provider:        cfg.providerType,
		Model:           cfg.model,
		Prompt:          cfg.prompt,
		SystemPrompt:    cfg.systemPrompt,
		BaseURL:         cfg.baseURL,
		AzureAPIVersion: cfg.azureAPIVersion,
	}.EnvVars()...)

	// Auto-inject coordination tools when coordination is enabled
	if agent != nil && agent.Spec.Coordination != nil && agent.Spec.Coordination.Enabled {
		for _, ct := range []string{
			"delegate_task",
			"wait_for_tasks",
			"create_container_task",
			"cancel_task",
			"send_message",
			"check_messages",
			"recall_memory",
			"remember",
			"propose_memory",
			"search_transcript",
			"create_pull_request",
			"check_pull_request_ci",
			"merge_pull_request",
			"auto_merge_pull_request",
			"review_pull_request",
			"post_review_comment",
			"create_agent",
			"delete_agent",
			"update_plan",
		} {
			if !slices.Contains(cfg.tools, ct) {
				cfg.tools = append(cfg.tools, ct)
			}
		}
	}

	// Auto-inject messaging tools for child tasks (tasks delegated by a coordinator)
	// so they can communicate with sibling tasks via send_message/check_messages
	_, isChildTask := task.Labels[labels.LabelParentTask]
	if isChildTask {
		for _, ct := range []string{"send_message", "check_messages"} {
			if !slices.Contains(cfg.tools, ct) {
				cfg.tools = append(cfg.tools, ct)
			}
		}
	}

	if len(cfg.tools) > 0 {
		envVars = append(envVars, corev1.EnvVar{Name: workerenv.AITools, Value: strings.Join(cfg.tools, ",")})
	}

	if agent != nil && agent.Spec.Coordination != nil && agent.Spec.Coordination.Enabled {
		envVars = b.addCoordinationEnvVars(envVars, task, agent)
	}

	// Enable coordination in worker for child tasks so messaging tools are registered
	if isChildTask && (agent == nil || agent.Spec.Coordination == nil || !agent.Spec.Coordination.Enabled) {
		envVars = append(envVars, corev1.EnvVar{Name: workerenv.CoordinationEnabled, Value: "true"})
	}

	// Add fallback provider environment variables
	if agent != nil && agent.Spec.Model != nil && len(agent.Spec.Model.Fallbacks) > 0 {
		envVars = append(envVars, corev1.EnvVar{
			Name:  workerenv.AIFallbackCount,
			Value: fmt.Sprintf("%d", len(agent.Spec.Model.Fallbacks)),
		})
		for i, fb := range agent.Spec.Model.Fallbacks {
			// Resolve the fallback Provider CRD
			fbProvider := &corev1alpha1.Provider{}
			if err := b.Get(ctx, client.ObjectKey{
				Namespace: task.Namespace,
				Name:      fb.ProviderRef,
			}, fbProvider); err != nil {
				continue // skip unresolvable fallbacks
			}
			fallbackEnv := workerenv.FallbackProviderEnv{
				Provider: string(fbProvider.Spec.Type),
				Model:    fb.Model,
				BaseURL:  fbProvider.Spec.BaseURL,
			}
			if fbProvider.Spec.Azure != nil {
				fallbackEnv.AzureAPIVersion = fbProvider.Spec.Azure.APIVersion
			}
			envVars = append(envVars, fallbackEnv.EnvVars(i)...)

		}
	}

	// AllowBash: task override > agent default > true
	allowBash := true
	if agent != nil && agent.Spec.Runtime != nil && agent.Spec.Runtime.DefaultAllowBash != nil {
		allowBash = *agent.Spec.Runtime.DefaultAllowBash
	}
	if task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.AllowBash != nil {
		allowBash = *task.Spec.AgentRuntime.AllowBash
	}
	if allowBash {
		envVars = append(envVars, corev1.EnvVar{
			Name: workerenv.AllowBash, Value: "true",
		})
	}

	return envVars
}

func (b *JobBuilder) addTransactionTokenSecret(job *batchv1.Job, task *corev1alpha1.Task) {
	if job == nil || len(job.Spec.Template.Spec.Containers) == 0 {
		return
	}
	container := &job.Spec.Template.Spec.Containers[0]
	container.Env = appendEnvIfSetMissing(container.Env, workerenv.ContextTokenTTSURL, b.ContextTokenTTSURL)
	container.Env = appendEnvIfSetMissing(container.Env, workerenv.ContextTokenSubjectTokenType, b.ContextTokenSubjectTokenType)
	container.Env = appendEnvIfSetMissing(container.Env, workerenv.ContextTokenChildScope, b.ContextTokenChildScope)
	container.Env = appendEnvIfSetMissing(container.Env, workerenv.ContextTokenOutboundScope, b.ContextTokenOutboundScope)

	if task == nil || task.Annotations == nil {
		return
	}
	secretName := strings.TrimSpace(task.Annotations[labels.AnnotationTransactionTokenSecret])
	if secretName == "" {
		return
	}
	const (
		volumeName = "transaction-token"
		mountPath  = "/var/run/orka/transaction-token"
		tokenPath  = mountPath + "/token"
	)
	defaultMode := int32(0400)
	job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
		Name: volumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  secretName,
				DefaultMode: &defaultMode,
				Items: []corev1.KeyToPath{{
					Key:  "token",
					Path: "token",
				}},
			},
		},
	})
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
		Name:      volumeName,
		MountPath: mountPath,
		ReadOnly:  true,
	})
	container.Env = appendEnvIfMissing(container.Env, corev1.EnvVar{Name: workerenv.TransactionTokenFile, Value: tokenPath})
	container.Env = appendEnvIfMissing(container.Env, corev1.EnvVar{Name: workerenv.ContextTokenSubjectTokenFile, Value: tokenPath})
}

// addSecretVolumes adds secret volumes to the Job
func (b *JobBuilder) addSecretVolumes(ctx context.Context, job *batchv1.Job, task *corev1alpha1.Task, agent *corev1alpha1.Agent, provider *corev1alpha1.Provider) {
	// Add provider secret (mounted as environment variable source)
	if provider != nil {
		secretName := provider.Spec.SecretRef.Name
		secretKey := provider.Spec.SecretRef.Key
		if secretKey == "" {
			secretKey = defaultSecretKey
		}

		// Determine the env var name based on provider type
		envVarName := workerenv.AnthropicAPIKey
		if provider.Spec.Type == corev1alpha1.ProviderTypeOpenAI || provider.Spec.Type == corev1alpha1.ProviderTypeAzureOpenAI {
			envVarName = workerenv.OpenAIAPIKey
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

		// Set base URL so agent CLIs route through the provider's upstream
		if provider.Spec.BaseURL != "" {
			baseURLEnvVar := workerenv.AnthropicBaseURL
			if provider.Spec.Type == corev1alpha1.ProviderTypeOpenAI || provider.Spec.Type == corev1alpha1.ProviderTypeAzureOpenAI {
				baseURLEnvVar = workerenv.OpenAIBaseURL
			}
			job.Spec.Template.Spec.Containers[0].Env = append(
				job.Spec.Template.Spec.Containers[0].Env,
				corev1.EnvVar{Name: baseURLEnvVar, Value: provider.Spec.BaseURL},
			)
		}
	}

	// Add fallback provider secrets
	if agent != nil && agent.Spec.Model != nil {
		for i, fb := range agent.Spec.Model.Fallbacks {
			fbProvider := &corev1alpha1.Provider{}
			if err := b.Get(ctx, client.ObjectKey{
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
			envVarName := workerenv.FallbackAPIKeyKey(i)
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
		corev1.EnvVar{Name: workerenv.SessionName, Value: sessionName},
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
	case corev1alpha1.AgentRuntimeCodex:
		return b.CodexWorkerImage
	default:
		return b.ClaudeWorkerImage
	}
}

func appendEnvIfSetMissing(envVars []corev1.EnvVar, name, value string) []corev1.EnvVar {
	if value == "" || envVarExists(envVars, name) {
		return envVars
	}
	return append(envVars, corev1.EnvVar{Name: name, Value: value})
}

func appendEnvIfMissing(envVars []corev1.EnvVar, envVar corev1.EnvVar) []corev1.EnvVar {
	if envVarExists(envVars, envVar.Name) {
		return envVars
	}
	return append(envVars, envVar)
}

func envVarExists(envVars []corev1.EnvVar, name string) bool {
	for _, envVar := range envVars {
		if envVar.Name == name {
			return true
		}
	}
	return false
}

func isCodexAgent(agent *corev1alpha1.Agent) bool {
	return agent != nil && agent.Spec.Runtime != nil && agent.Spec.Runtime.Type == corev1alpha1.AgentRuntimeCodex
}

// addCodexSandboxEnvVars injects controller-configured Codex sandbox mode when set.
func (b *JobBuilder) addCodexSandboxEnvVars(envVars []corev1.EnvVar, agent *corev1alpha1.Agent) []corev1.EnvVar {
	if b.CodexSandboxMode == "" || !isCodexAgent(agent) || envVarExists(envVars, workerenv.CodexSandboxMode) {
		return envVars
	}

	return append(envVars, corev1.EnvVar{Name: workerenv.CodexSandboxMode, Value: b.CodexSandboxMode})
}

// addAgentEnvVars adds agent-runtime-specific environment variables
func (b *JobBuilder) addAgentEnvVars(ctx context.Context, envVars []corev1.EnvVar, task *corev1alpha1.Task, agent *corev1alpha1.Agent) []corev1.EnvVar {
	// Prompt (required)
	prompt := task.Spec.Prompt
	if prompt == "" && task.Spec.AI != nil {
		prompt = task.Spec.AI.Prompt
	}
	envVars = append(envVars, corev1.EnvVar{Name: workerenv.Prompt, Value: prompt})

	envVars = b.addAgentModelEnvVars(ctx, envVars, agent)
	envVars = b.addAgentToolsEnvVars(envVars, task, agent)
	envVars = b.addWorkspaceEnvVars(envVars, task)

	// Timeout (task level)
	if task.Spec.Timeout != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  workerenv.TimeoutSeconds,
			Value: fmt.Sprintf("%d", int64(task.Spec.Timeout.Seconds())),
		})
	}

	return envVars
}

// addAgentModelEnvVars adds model and system prompt env vars from the Agent.
// If the agent doesn't specify a model, it falls back to the default provider's defaultModel.
func (b *JobBuilder) addAgentModelEnvVars(ctx context.Context, envVars []corev1.EnvVar, agent *corev1alpha1.Agent) []corev1.EnvVar {
	if agent == nil {
		return envVars
	}

	model := ""
	if agent.Spec.Model != nil && agent.Spec.Model.Name != "" {
		model = agent.Spec.Model.Name
	}

	// Fall back to the default provider's model if the agent doesn't specify one
	if model == "" {
		defaultProvider := &corev1alpha1.Provider{}
		if err := b.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: "default"}, defaultProvider); err == nil {
			model = defaultProvider.Spec.DefaultModel
		}
	}

	if model != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name: workerenv.Model, Value: model,
		})
	}

	if agent.Spec.SystemPrompt != nil {
		var systemPrompt string
		if agent.Spec.SystemPrompt.Inline != "" {
			systemPrompt = agent.Spec.SystemPrompt.Inline
		} else if agent.Spec.SystemPrompt.ConfigMapRef != nil {
			systemPrompt = b.resolveConfigMapValue(ctx, agent.Namespace, agent.Spec.SystemPrompt.ConfigMapRef)
		}
		if systemPrompt != "" {
			envVars = append(envVars, corev1.EnvVar{
				Name: workerenv.SystemPrompt, Value: systemPrompt,
			})
		}
	}
	return envVars
}

// resolveConfigMapValue reads a value from a ConfigMap key.
func (b *JobBuilder) resolveConfigMapValue(ctx context.Context, namespace string, ref *corev1alpha1.ConfigMapKeySelector) string {
	cm := &corev1.ConfigMap{}
	if err := b.Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: namespace}, cm); err != nil {
		log.FromContext(ctx).Error(err, "failed to resolve ConfigMap for system prompt",
			"configMap", ref.Name, "namespace", namespace, "key", ref.Key)
		return ""
	}
	return cm.Data[ref.Key]
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
		Name: workerenv.MaxTurns, Value: fmt.Sprintf("%d", maxTurns),
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
			Name: workerenv.AllowedTools, Value: joinStrings(allowedTools),
		})
	}

	// DisallowedTools (task only)
	if task.Spec.AgentRuntime != nil && len(task.Spec.AgentRuntime.DisallowedTools) > 0 {
		envVars = append(envVars, corev1.EnvVar{
			Name:  workerenv.DisallowedTools,
			Value: joinStrings(task.Spec.AgentRuntime.DisallowedTools),
		})
	}

	// AllowBash: task override > agent default > true
	allowBash := true
	if agent != nil && agent.Spec.Runtime != nil && agent.Spec.Runtime.DefaultAllowBash != nil {
		allowBash = *agent.Spec.Runtime.DefaultAllowBash
	}
	if task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.AllowBash != nil {
		allowBash = *task.Spec.AgentRuntime.AllowBash
	}
	if allowBash {
		envVars = append(envVars, corev1.EnvVar{
			Name: workerenv.AllowBash, Value: "true",
		})
	}

	return envVars
}

// addWorkspaceEnvVars adds workspace-related env vars from the task.
func (b *JobBuilder) addWorkspaceEnvVars(
	envVars []corev1.EnvVar,
	task *corev1alpha1.Task,
) []corev1.EnvVar {
	ws := effectiveWorkspace(task)
	if ws == nil {
		return envVars
	}
	if ws.GitRepo != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name: workerenv.GitRepo, Value: ws.GitRepo,
		})
	}
	envVars = append(envVars,
		corev1.EnvVar{Name: workerenv.GitConfigCount, Value: "1"},
		corev1.EnvVar{Name: workerenv.GitConfigKey0, Value: "safe.directory"},
		corev1.EnvVar{Name: workerenv.GitConfigValue0, Value: "/workspace"},
	)
	if ws.Branch != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name: workerenv.GitBranch, Value: ws.Branch,
		})
	}
	if ws.Ref != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name: workerenv.GitRef, Value: ws.Ref,
		})
	}
	if ws.SubPath != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name: workerenv.WorkspaceSubpath, Value: ws.SubPath,
		})
	}
	if ws.ForkRepo != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name: workerenv.ForkRepo, Value: ws.ForkRepo,
		})
	}
	if ws.PRBaseBranch != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name: workerenv.PRBaseBranch, Value: ws.PRBaseBranch,
		})
	}
	if ws.PushBranch != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name: workerenv.PushBranch, Value: ws.PushBranch,
		})
		envVars = append(envVars, corev1.EnvVar{
			Name: workerenv.RequirePushBranch, Value: "true",
		})
	}
	return envVars
}

// addAgentWorkspaceEnvVars adds workspace-related env vars from the task.
// Deprecated: use addWorkspaceEnvVars.
func (b *JobBuilder) addAgentWorkspaceEnvVars(envVars []corev1.EnvVar, task *corev1alpha1.Task) []corev1.EnvVar {
	return b.addWorkspaceEnvVars(envVars, task)
}

// addWorkspaceVolumes adds workspace-specific volumes to the Job (workspace, home)
func (b *JobBuilder) addWorkspaceVolumes(job *batchv1.Job, task *corev1alpha1.Task) {
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

	// Git secret volume if explicitly referenced
	ws := effectiveWorkspace(task)
	if ws != nil && ws.GitSecretRef != nil {
		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: "git-credentials",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: ws.GitSecretRef.Name,
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

func taskNeedsWorkspace(task *corev1alpha1.Task) bool {
	return task != nil && (task.Spec.Type == corev1alpha1.TaskTypeAgent || effectiveWorkspace(task) != nil)
}

func effectiveWorkspace(task *corev1alpha1.Task) *corev1alpha1.WorkspaceConfig {
	if task == nil {
		return nil
	}
	if task.Spec.Workspace != nil {
		return task.Spec.Workspace
	}
	if task.Spec.AgentRuntime != nil {
		return task.Spec.AgentRuntime.Workspace
	}
	return nil
}

func (b *JobBuilder) addWorkspaceInitContainer(job *batchv1.Job, task *corev1alpha1.Task) {
	initContainer := corev1.Container{
		Name:            "prepare-workspace",
		Image:           b.GeneralWorkerImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: b.buildContainerSecurityContext(),
		Command:         []string{"/worker"},
		Args:            []string{"--prepare-workspace-only"},
		Env:             b.workspaceInitEnvVars(task),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "workspace", MountPath: "/workspace"},
			{Name: "home", MountPath: "/home/worker"},
			{Name: "tmp", MountPath: "/tmp"},
		},
	}
	if effectiveWorkspace(task).GitSecretRef != nil {
		initContainer.VolumeMounts = append(initContainer.VolumeMounts, corev1.VolumeMount{
			Name:      "git-credentials",
			MountPath: "/secrets/git",
			ReadOnly:  true,
		})
	}
	job.Spec.Template.Spec.InitContainers = append(job.Spec.Template.Spec.InitContainers, initContainer)
}

func (b *JobBuilder) workspaceInitEnvVars(task *corev1alpha1.Task) []corev1.EnvVar {
	envVars := []corev1.EnvVar{
		{Name: TaskNameEnvVar, Value: task.Name},
		{Name: TaskNamespaceEnvVar, Value: task.Namespace},
		{Name: ControllerURLEnvVar, Value: b.ControllerURL},
	}
	envVars = append(envVars, task.Spec.Env...)
	if task.Spec.PriorTaskRef != nil {
		envVars = append(envVars, corev1.EnvVar{Name: workerenv.PriorTask, Value: task.Spec.PriorTaskRef.Name})
		priorNS := task.Spec.PriorTaskRef.Namespace
		if priorNS == "" {
			priorNS = task.Namespace
		}
		envVars = append(envVars, corev1.EnvVar{Name: workerenv.PriorTaskNamespace, Value: priorNS})
	}
	return b.addWorkspaceEnvVars(envVars, task)
}

func workspaceWorkingDir(task *corev1alpha1.Task) string {
	ws := effectiveWorkspace(task)
	if ws != nil && ws.SubPath != "" {
		return path.Join("/workspace", ws.SubPath)
	}
	return "/workspace"
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

// addSkillVolumes reads Skill CRs referenced by the agent and task, creates a ConfigMap
// with concatenated skill content, and mounts it at /workspace/.skills/.
func (b *JobBuilder) addSkillVolumes(ctx context.Context, job *batchv1.Job, task *corev1alpha1.Task, agent *corev1alpha1.Agent) error {
	logger := log.FromContext(ctx)

	// Collect skill references: agent-level first, then task-level
	var skillRefs []corev1alpha1.SkillReference
	if agent != nil {
		skillRefs = append(skillRefs, agent.Spec.Skills...)
	}
	if task.Spec.AI != nil {
		skillRefs = append(skillRefs, task.Spec.AI.Skills...)
	}

	// Deduplicate skill references by resolved identifier
	seen := make(map[string]bool)
	deduped := make([]corev1alpha1.SkillReference, 0, len(skillRefs))
	for _, ref := range skillRefs {
		var key string
		switch {
		case ref.Name != "":
			key = "skill:" + ref.Name
		case ref.ConfigMapRef != nil:
			key = "configmap:" + ref.ConfigMapRef.Name + "/" + ref.ConfigMapRef.Key
		}
		if key != "" && !seen[key] {
			seen[key] = true
			deduped = append(deduped, ref)
		}
	}
	skillRefs = deduped

	if len(skillRefs) == 0 {
		return nil
	}

	// Read Skill CRs and build ConfigMap data.
	// "PROMPT.md" is the only file injected into the model system prompt.
	cmData := make(map[string]string)
	items := make([]corev1.KeyToPath, 0, len(skillRefs)+1)
	promptParts := make([]string, 0, len(skillRefs))

	for idx, ref := range skillRefs {
		switch {
		case ref.Name != "":
			skill := &corev1alpha1.Skill{}
			skillName := ref.Name
			if err := b.Get(ctx, client.ObjectKey{Name: skillName, Namespace: task.Namespace}, skill); err != nil {
				return fmt.Errorf("failed to get Skill %q: %w", skillName, err)
			}

			metrics.SkillsLoaded.WithLabelValues(skill.Name, task.Namespace).Inc()

			promptParts = append(promptParts, strings.TrimSpace(skill.Spec.Content.Inline))

			inlineKey := fmt.Sprintf("skill-%d-inline", idx)
			cmData[inlineKey] = skill.Spec.Content.Inline
			items = append(items, corev1.KeyToPath{
				Key:  inlineKey,
				Path: path.Join(skillName, "SKILL.md"),
			})

			filePaths := make([]string, 0, len(skill.Spec.Content.Files))
			for filePath := range skill.Spec.Content.Files {
				filePaths = append(filePaths, filePath)
			}
			sort.Strings(filePaths)
			for i, filePath := range filePaths {
				fileKey := fmt.Sprintf("skill-%d-file-%d", idx, i)
				cmData[fileKey] = skill.Spec.Content.Files[filePath]
				items = append(items, corev1.KeyToPath{
					Key:  fileKey,
					Path: path.Join(skillName, filePath),
				})
			}
		case ref.ConfigMapRef != nil:
			cm := &corev1.ConfigMap{}
			if err := b.Get(ctx, client.ObjectKey{Name: ref.ConfigMapRef.Name, Namespace: task.Namespace}, cm); err != nil {
				return fmt.Errorf("failed to get skill ConfigMap %q: %w", ref.ConfigMapRef.Name, err)
			}
			content, ok := cm.Data[ref.ConfigMapRef.Key]
			if !ok {
				return fmt.Errorf("key %q not found in skill ConfigMap %q", ref.ConfigMapRef.Key, ref.ConfigMapRef.Name)
			}

			metrics.SkillsLoaded.WithLabelValues(ref.ConfigMapRef.Name, task.Namespace).Inc()

			promptParts = append(promptParts, strings.TrimSpace(content))

			inlineKey := fmt.Sprintf("skill-%d-inline", idx)
			cmData[inlineKey] = content
			items = append(items, corev1.KeyToPath{
				Key:  inlineKey,
				Path: path.Join(ref.ConfigMapRef.Name+"-"+ref.ConfigMapRef.Key, "SKILL.md"),
			})
		default:
			return fmt.Errorf("skill reference must set either name or configMapRef")
		}
	}

	prompt := strings.TrimSpace(strings.Join(promptParts, "\n\n"))
	if prompt == "" {
		return nil
	}
	cmData["system-prompt"] = prompt
	items = append(items, corev1.KeyToPath{Key: "system-prompt", Path: "PROMPT.md"})

	// Create a ConfigMap for skill content owned by the Job
	skillCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Name + "-skills",
			Namespace: job.Namespace,
			Labels: map[string]string{
				labels.LabelTask:    labels.SelectorValue(task.Name),
				labels.LabelPurpose: "skills",
				labels.LabelManaged: "true",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(task, corev1alpha1.GroupVersion.WithKind("Task")),
			},
		},
		Data: cmData,
	}

	if err := b.Create(ctx, skillCM); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create skill ConfigMap: %w", err)
		}
		existing := &corev1.ConfigMap{}
		if getErr := b.Get(ctx, client.ObjectKey{Name: skillCM.Name, Namespace: skillCM.Namespace}, existing); getErr != nil {
			return fmt.Errorf("failed to get existing skill ConfigMap: %w", getErr)
		}
		if !reflect.DeepEqual(existing.Data, cmData) {
			existing.Data = cmData
			if updateErr := b.Update(ctx, existing); updateErr != nil {
				return fmt.Errorf("failed to update existing skill ConfigMap: %w", updateErr)
			}
		}
	} else {
		logger.Info("Created skill ConfigMap", "configmap", skillCM.Name, "skills", len(skillRefs))
	}

	// Mount the ConfigMap into the worker pod
	job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
		Name: "skills",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: skillCM.Name,
				},
				Items: items,
			},
		},
	})
	job.Spec.Template.Spec.Containers[0].VolumeMounts = append(
		job.Spec.Template.Spec.Containers[0].VolumeMounts,
		corev1.VolumeMount{
			Name:      "skills",
			MountPath: "/workspace/.skills",
			ReadOnly:  true,
		},
	)

	return nil
}
