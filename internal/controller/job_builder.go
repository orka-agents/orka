/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"net/url"
	"os"
	"path"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/contexttoken"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/taskmeta"
	"github.com/orka-agents/orka/internal/workerenv"
)

const (
	// DefaultAIWorkerImage is the default image for AI tasks
	DefaultAIWorkerImage = "ghcr.io/orka-agents/orka/ai-worker:latest"

	// DefaultGeneralWorkerImage is the default image for container tasks
	DefaultGeneralWorkerImage = "ghcr.io/orka-agents/orka/general-worker:latest"

	// DefaultInitImage is the default image for init containers
	DefaultInitImage = "busybox:1.37"

	// AIWorkerServiceAccount is the ServiceAccount used by trusted AI task workers.
	AIWorkerServiceAccount = "orka-ai-worker"

	// VendorWorkerServiceAccount is the ServiceAccount used by untrusted vendor/agent task workers.
	VendorWorkerServiceAccount = "orka-vendor-worker"

	// ContainerWorkerServiceAccount is the ServiceAccount used by untrusted container task workers.
	ContainerWorkerServiceAccount = "orka-container-worker"

	// directProviderSecretsEnvVar restores legacy direct provider API key/base URL injection for untrusted container pods.
	directProviderSecretsEnvVar = "ORKA_AGENT_DIRECT_PROVIDER_SECRETS"

	// directSecretMountsEnvVar restores legacy direct task/agent secret injection for untrusted container pods.
	directSecretMountsEnvVar = "ORKA_AGENT_DIRECT_SECRET_MOUNTS"

	// directGitCredentialsEnvVar restores legacy direct Git credential mounts for untrusted custom container pods.
	directGitCredentialsEnvVar = "ORKA_AGENT_DIRECT_GIT_CREDENTIALS"

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
	AIWorkerImage                              string
	GeneralWorkerImage                         string
	InitImage                                  string
	ControllerURL                              string // e.g. http://orka-controller.orka-system.svc:8080
	ContextTokenTTSEndpoint                    string
	ContextTokenTTSAudience                    string
	ContextTokenTTSTimeout                     string
	ContextTokenTTSTokenSource                 string
	ContextTokenSubjectTokenType               string
	ContextTokenChildScope                     string
	ContextTokenOutboundScope                  string
	ContextTokenChildTokenTTL                  string
	ContextTokenToolTokenTTL                   string
	TransactionCredentialReadScopes            []string
	OutboundAccessTrustedGatewayServices       string
	OutboundAccessTrustedTokenEndpointServices string
	EnableTelemetry                            bool
	directSecrets                              directRuntimeSecretPolicy
}

type directRuntimeSecretPolicy struct {
	providerSecrets bool
	secretMounts    bool
	gitCredentials  bool
}

// NewJobBuilder creates a new JobBuilder
func NewJobBuilder(c client.Client) *JobBuilder {
	return &JobBuilder{
		Client:             c,
		AIWorkerImage:      DefaultAIWorkerImage,
		GeneralWorkerImage: DefaultGeneralWorkerImage,
		InitImage:          DefaultInitImage,
		directSecrets: directRuntimeSecretPolicy{
			providerSecrets: envFlagEnabled(directProviderSecretsEnvVar),
			secretMounts:    envFlagEnabled(directSecretMountsEnvVar),
			gitCredentials:  envFlagEnabled(directGitCredentialsEnvVar),
		},
	}
}

func workerServiceAccountForTask(task *corev1alpha1.Task) string {
	if task == nil {
		return ContainerWorkerServiceAccount
	}

	switch task.Spec.Type {
	case corev1alpha1.TaskTypeAI:
		return AIWorkerServiceAccount
	case corev1alpha1.TaskTypeAgent:
		return VendorWorkerServiceAccount
	case corev1alpha1.TaskTypeContainer:
		return ContainerWorkerServiceAccount
	default:
		return ContainerWorkerServiceAccount
	}
}

func workerAutomountServiceAccountTokenWithOptions(task *corev1alpha1.Task, opts JobBuildOptions) *bool {
	return new(podShouldAutomountServiceAccountTokenWithOptions(task, opts))
}

func podShouldAutomountServiceAccountToken(task *corev1alpha1.Task) bool {
	if taskRequestsReadOnlyAgent(task) {
		return false
	}
	if task == nil || !isUntrustedComputeTask(task) {
		return true
	}

	return taskUsesManagedOrkaWorker(task)
}

func podShouldAutomountServiceAccountTokenWithOptions(task *corev1alpha1.Task, opts JobBuildOptions) bool {
	if taskRequestsReadOnlyAgent(task) {
		return opts.ExecutionWorkspace != nil || opts.AgentSandboxWorkspace != nil
	}
	return podShouldAutomountServiceAccountToken(task)
}

func taskUsesManagedOrkaWorker(task *corev1alpha1.Task) bool {
	if task == nil {
		return false
	}

	switch task.Spec.Type {
	case corev1alpha1.TaskTypeAI, corev1alpha1.TaskTypeAgent:
		return true
	case corev1alpha1.TaskTypeContainer:
		return task.Spec.Image == ""
	default:
		return false
	}
}

func isVendorAgentTask(task *corev1alpha1.Task) bool {
	return task != nil && task.Spec.Type == corev1alpha1.TaskTypeAgent
}

func isUntrustedComputeTask(task *corev1alpha1.Task) bool {
	if task == nil {
		return false
	}

	switch task.Spec.Type {
	case corev1alpha1.TaskTypeAgent, corev1alpha1.TaskTypeContainer:
		return true
	default:
		return false
	}
}

func (b *JobBuilder) directProviderSecretsAllowed(task *corev1alpha1.Task) bool {
	return taskAllowsDirectRuntimeSecrets(task) || (b != nil && b.directSecrets.providerSecrets)
}

func (b *JobBuilder) directSecretMountsAllowed(task *corev1alpha1.Task) bool {
	if taskRequestsReadOnlyAgent(task) {
		return false
	}
	return taskAllowsDirectRuntimeSecrets(task) || (b != nil && b.directSecrets.secretMounts)
}

func taskAllowsDirectRuntimeSecrets(task *corev1alpha1.Task) bool {
	return !isUntrustedComputeTask(task) || isVendorAgentTask(task)
}

func mainContainerNeedsGitCredentials(task *corev1alpha1.Task) bool {
	return taskUsesManagedOrkaWorker(task) && !taskRequestsReadOnlyAgent(task)
}

func (b *JobBuilder) directGitCredentialsAllowed(task *corev1alpha1.Task) bool {
	return !isUntrustedComputeTask(task) || mainContainerNeedsGitCredentials(task) || (b != nil && b.directSecrets.gitCredentials)
}

func envFlagEnabled(name string) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if enabled, err := strconv.ParseBool(value); err == nil {
		return enabled
	}

	switch strings.ToLower(value) {
	case "y", "yes", "on":
		return true
	case "n", "no", "off":
		return false
	default:
		return false
	}
}

func agentHasFallbackProviders(agent *corev1alpha1.Agent) bool {
	return agent != nil && agent.Spec.Model != nil && len(agent.Spec.Model.Fallbacks) > 0
}

var (
	defaultTaskResourceCPURequest    = *resource.NewMilliQuantity(100, resource.DecimalSI)
	defaultTaskResourceMemoryRequest = *resource.NewQuantity(512*1024*1024, resource.BinarySI)
	defaultTaskResourceCPULimit      = *resource.NewQuantity(1, resource.DecimalSI)
	defaultTaskResourceMemoryLimit   = *resource.NewQuantity(2*1024*1024*1024, resource.BinarySI)
)

func defaultTaskResourceRequirements() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    defaultTaskResourceCPURequest,
			corev1.ResourceMemory: defaultTaskResourceMemoryRequest,
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    defaultTaskResourceCPULimit,
			corev1.ResourceMemory: defaultTaskResourceMemoryLimit,
		},
	}
}

func (b *JobBuilder) needsSecretVolumes(task *corev1alpha1.Task, agent *corev1alpha1.Agent, provider *corev1alpha1.Provider) bool {
	if taskRequestsReadOnlyAgent(task) && agent != nil && agent.Spec.SecretRef != nil {
		return true
	}
	if b.directSecretMountsAllowed(task) {
		if task != nil && task.Spec.SecretRef != nil {
			return true
		}
		if agent != nil && agent.Spec.SecretRef != nil {
			return true
		}
	}

	return b.directProviderSecretsAllowed(task) && (provider != nil || agentHasFallbackProviders(agent))
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

// JobBuildOptions carries optional inputs that affect Job rendering while keeping
// the historical Build signature stable.
type JobBuildOptions struct {
	AgentSandboxWorkspace *AgentSandboxWorkspaceRequest
	ExecutionWorkspace    *ExecutionWorkspaceRequest
	ResolvedApprovalsJSON string
}

// Build creates a Job for the given Task.
func (b *JobBuilder) Build(ctx context.Context, task *corev1alpha1.Task, agent *corev1alpha1.Agent, provider *corev1alpha1.Provider) (*batchv1.Job, error) {
	return b.BuildWithOptions(ctx, task, agent, provider, JobBuildOptions{})
}

// BuildWithOptions creates a Job for the given Task using additional resolved options.
func (b *JobBuilder) BuildWithOptions(ctx context.Context, task *corev1alpha1.Task, agent *corev1alpha1.Agent, provider *corev1alpha1.Provider, opts JobBuildOptions) (*batchv1.Job, error) {
	if err := validateReadOnlyAgentRuntime(task, agent); err != nil {
		return nil, err
	}

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
			BackoffLimit: new(int32(0)), // No retries at Job level, we handle retries in the controller
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						labels.LabelTask:     labels.SelectorValue(task.Name),
						labels.LabelTaskType: string(task.Spec.Type),
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           workerServiceAccountForTask(task),
					AutomountServiceAccountToken: workerAutomountServiceAccountTokenWithOptions(task, opts),
					SecurityContext:              b.buildPodSecurityContext(),
					Containers: []corev1.Container{
						b.buildContainerWithOptions(ctx, task, agent, provider, opts),
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

	if effectiveWorkspace(task) != nil && (taskUsesWorkspaceInitContainer(task) || (task.Spec.Type == corev1alpha1.TaskTypeContainer && task.Spec.Image != "")) {
		b.addWorkspaceInitContainer(job, task)
	}

	// Add skill volumes — read Skill CRs, create ConfigMap, mount at /workspace/.skills/
	if err := b.addSkillVolumes(ctx, job, task, agent); err != nil {
		return nil, fmt.Errorf("failed to add skill volumes: %w", err)
	}

	// Add secret volumes if needed
	if b.needsSecretVolumes(task, agent, provider) {
		if err := b.addSecretVolumes(ctx, job, task, agent, provider); err != nil {
			return nil, fmt.Errorf("failed to add secret volumes: %w", err)
		}
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
		RunAsNonRoot: new(true),
		RunAsUser:    new(int64(1000)),
		RunAsGroup:   new(int64(1000)),
		FSGroup:      new(int64(1000)),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

// buildContainerSecurityContext builds a secure container security context
func (b *JobBuilder) buildContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: new(false),
		ReadOnlyRootFilesystem:   new(true),
		RunAsNonRoot:             new(true),
		RunAsUser:                new(int64(1000)),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
}

// buildContainer builds the main container for the Job
func (b *JobBuilder) buildContainer(ctx context.Context, task *corev1alpha1.Task, agent *corev1alpha1.Agent, provider *corev1alpha1.Provider) corev1.Container {
	return b.buildContainerWithOptions(ctx, task, agent, provider, JobBuildOptions{})
}

// buildContainerWithOptions builds the main container for the Job.
func (b *JobBuilder) buildContainerWithOptions(ctx context.Context, task *corev1alpha1.Task, agent *corev1alpha1.Agent, provider *corev1alpha1.Provider, opts JobBuildOptions) corev1.Container {
	container := corev1.Container{
		Name:            "worker",
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: b.buildContainerSecurityContext(),
		Resources:       b.buildResources(task, agent),
		Env:             b.buildEnvVarsWithOptions(ctx, task, agent, provider, opts),
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
		container.Image = b.AIWorkerImage
		container.Command = []string{"/worker"}
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
		job.Spec.Template.Spec.RuntimeClassName = new(execution.RuntimeClassName)
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

	// Default resources. Memory limit is sized for real Go/Node/Python
	// test suites — 512Mi was too small for `go test ./...` on medium repos
	// and silently OOMKilled workers. Agents/tasks can still override via
	// agent.spec.resources or task.spec.resources (checked above).
	return defaultTaskResourceRequirements()
}

// buildEnvVars builds the environment variables for the container
func (b *JobBuilder) buildEnvVars(ctx context.Context, task *corev1alpha1.Task, agent *corev1alpha1.Agent, provider *corev1alpha1.Provider) []corev1.EnvVar {
	return b.buildEnvVarsWithOptions(ctx, task, agent, provider, JobBuildOptions{})
}

// buildEnvVarsWithOptions builds the environment variables for the container using additional options.
func (b *JobBuilder) buildEnvVarsWithOptions(ctx context.Context, task *corev1alpha1.Task, agent *corev1alpha1.Agent, provider *corev1alpha1.Provider, opts JobBuildOptions) []corev1.EnvVar {
	baseEnv := workerenv.BaseEnv{
		TaskName:       task.Name,
		TaskNamespace:  task.Namespace,
		TaskUID:        string(task.UID),
		ResultEndpoint: fmt.Sprintf("%s/internal/v1/results/%s/%s", b.ControllerURL, task.Namespace, task.Name),
		ControllerURL:  b.ControllerURL,
	}
	if agent != nil {
		baseEnv.AgentName = agent.Name
	}
	envVars := baseEnv.EnvVars()
	if taskRequestsReadOnlyAgent(task) {
		envVars = setControllerEnv(envVars, workerenv.ResultStdout, scheduledRunLabelValue)
	}
	envVars = b.addTelemetryEnvVars(envVars, task)

	// Add task-level env vars. AI worker telemetry env vars are reserved for
	// controller injection so workload authors cannot bypass default-off telemetry
	// policy or redirect GenAI metadata to arbitrary collectors. Restore
	// controller-owned identity and approval env vars so task authors cannot
	// spoof execution identity or approval state.
	envVars = appendTaskEnvVars(envVars, task)
	envVars = setControllerEnv(envVars, workerenv.TaskName, task.Name)
	envVars = setControllerEnv(envVars, workerenv.TaskNamespace, task.Namespace)
	envVars = setControllerEnv(envVars, workerenv.TaskUID, string(task.UID))
	if agent != nil {
		envVars = setControllerEnv(envVars, workerenv.AgentName, agent.Name)
	}
	envVars = setControllerEnv(envVars, workerenv.ResultEndpoint, fmt.Sprintf("%s/internal/v1/results/%s/%s", b.ControllerURL, task.Namespace, task.Name))
	envVars = setControllerEnv(envVars, workerenv.ControllerURL, b.ControllerURL)
	envVars = setControllerEnvValue(envVars, workerenv.AITools, "")
	envVars = setControllerEnvValue(envVars, workerenv.CoordinationEnabled, "")
	envVars = setControllerEnvValue(envVars, workerenv.AutonomousMode, "")
	envVars = setControllerEnvValue(envVars, workerenv.ResolvedApprovals, "")
	envVars = setControllerEnvValue(envVars, workerenv.ApprovalRequiredTools, "")
	envVars = addTransactionEnvVars(envVars, task.Spec.Transaction, b.TransactionCredentialReadScopes)

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
		if taskUsesWorkspaceInitContainer(task) {
			envVars = setControllerEnv(envVars, workerenv.WorkspacePrepared, scheduledRunLabelValue)
		}
		workspaceRequest := opts.ExecutionWorkspace
		if workspaceRequest == nil {
			workspaceRequest = opts.AgentSandboxWorkspace
		}
		envVars = b.addExecutionWorkspaceEnvVars(envVars, task, workspaceRequest)
	}

	if task.Spec.Type == corev1alpha1.TaskTypeContainer {
		envVars = b.addWorkspaceEnvVars(envVars, task)
	}
	envVars = setControllerEnvValue(envVars, workerenv.ResolvedApprovals, opts.ResolvedApprovalsJSON)
	if taskRequestsReadOnlyAgent(task) {
		envVars = setControllerEnv(envVars, workerenv.AgentReadOnly, scheduledRunLabelValue)
		envVars = setControllerEnv(envVars, workerenv.ResultStdout, scheduledRunLabelValue)
	}

	return envVars
}

// addExecutionWorkspaceEnvVars injects resolved execution workspace settings for agent tasks.
func (b *JobBuilder) addExecutionWorkspaceEnvVars(envVars []corev1.EnvVar, task *corev1alpha1.Task, request *ExecutionWorkspaceRequest) []corev1.EnvVar {
	if request == nil {
		return envVars
	}

	envVars = append(envVars, workerenv.ExecutionWorkspaceEnv{
		Enabled:               true,
		Provider:              string(request.Provider),
		TemplateName:          request.TemplateName,
		TemplateNamespace:     request.TemplateNamespace,
		ClaimNamespace:        request.ClaimNamespace,
		ClaimName:             request.ClaimName,
		ReusePolicy:           string(request.ReusePolicy),
		ReuseKey:              request.ReuseKey,
		CleanupPolicy:         string(request.CleanupPolicy),
		Boot:                  request.Boot,
		PoolName:              request.PoolName,
		PoolNamespace:         request.PoolNamespace,
		SnapshotRestoreURI:    request.SnapshotRestoreURI,
		SnapshotCheckpointURI: request.SnapshotCheckpointURI,
		SnapshotOnRelease:     request.SnapshotOnRelease,
		ProcessMode:           string(request.ProcessMode),
		ResidentKey:           request.ResidentKey,
		ClaimTimeout:          request.ClaimTimeout,
		CommandTimeout:        request.CommandTimeout,
		StatusEndpoint:        fmt.Sprintf("%s/internal/v1/tasks/%s/%s/execution-workspace/status", b.ControllerURL, task.Namespace, task.Name),
		Depth:                 0,
	}.EnvVars()...)

	if request.Provider == corev1alpha1.WorkspaceProviderSubstrate {
		envVars = append(envVars, workerenv.SubstrateEnv{
			APIEndpoint:             request.SubstrateAPIEndpoint,
			APICAFile:               request.SubstrateAPICAFile,
			APIInsecureSkipVerify:   request.SubstrateAPIInsecureSkipVerify,
			RouterURL:               request.SubstrateRouterURL,
			ActorDNSSuffix:          request.SubstrateActorDNSSuffix,
			SessionIdentityRequired: request.SubstrateSessionIdentityRequired,
			SessionIdentityMintCert: request.SubstrateSessionIdentityMintCert,
			SessionIdentityAudience: request.SubstrateSessionIdentityAudience,
			SessionIdentityAppID:    request.SubstrateSessionIdentityAppID,
			SessionIdentityUserID:   request.SubstrateSessionIdentityUserID,
		}.EnvVars()...)
		if strings.TrimSpace(request.SubstrateBootstrapSecretName) != "" {
			envVars = append(envVars, corev1.EnvVar{
				Name: workerenv.WorkspaceBootstrapToken,
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: request.SubstrateBootstrapSecretName,
						},
						Key: request.SubstrateBootstrapSecretKey,
					},
				},
			})
		}
		if strings.TrimSpace(request.SubstrateSessionIdentitySecretName) != "" {
			envVars = append(envVars, corev1.EnvVar{
				Name: workerenv.SubstrateSessionIdentityToken,
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: request.SubstrateSessionIdentitySecretName,
						},
						Key: request.SubstrateSessionIdentitySecretKey,
					},
				},
			})
		}
		return envVars
	}

	// Render the legacy agent-sandbox env during the migration so existing
	// worker images and tests continue to work unchanged.
	return append(envVars, workerenv.AgentSandboxEnv{
		Enabled:           true,
		RouterURL:         request.RouterURL,
		TemplateName:      request.TemplateName,
		TemplateNamespace: request.TemplateNamespace,
		ClaimNamespace:    request.ClaimNamespace,
		ReusePolicy:       string(request.ReusePolicy),
		ReuseKey:          request.ReuseKey,
		CleanupPolicy:     string(request.CleanupPolicy),
		WarmPoolPolicy:    request.WarmPoolPolicy,
		NamespaceStrategy: request.NamespaceStrategy,
		ClaimTimeout:      request.ClaimTimeout,
		CommandTimeout:    request.CommandTimeout,
	}.EnvVars()...)
}

func (b *JobBuilder) addTelemetryEnvVars(envVars []corev1.EnvVar, task *corev1alpha1.Task) []corev1.EnvVar {
	if task != nil && task.Annotations != nil {
		envVars = setControllerEnv(envVars, workerenv.TraceParent, task.Annotations[labels.AnnotationTraceParent])
		envVars = setControllerEnv(envVars, workerenv.TraceState, task.Annotations[labels.AnnotationTraceState])
	}
	if !b.EnableTelemetry || task == nil || task.Spec.Type != corev1alpha1.TaskTypeAI {
		return envVars
	}
	if !workerReachableOTLPEndpointConfigured(os.Getenv) {
		return envVars
	}
	envVars = setControllerEnv(envVars, workerenv.EnableTelemetry, scheduledRunLabelValue)
	unreachableSignalOverrides := unreachableWorkerOTLPSignalEndpoints(os.Getenv)
	// Copy only non-secret scalar OTLP settings. Header env vars carry
	// credentials, and certificate env vars are file paths whose source files are
	// not mounted into worker Pods by the controller.
	for _, name := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_PROTOCOL",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
		"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
		"OTEL_EXPORTER_OTLP_INSECURE",
		"OTEL_EXPORTER_OTLP_TRACES_INSECURE",
		"OTEL_EXPORTER_OTLP_METRICS_INSECURE",
		"OTEL_EXPORTER_OTLP_TIMEOUT",
		"OTEL_EXPORTER_OTLP_TRACES_TIMEOUT",
		"OTEL_EXPORTER_OTLP_METRICS_TIMEOUT",
		"OTEL_EXPORTER_OTLP_COMPRESSION",
		"OTEL_EXPORTER_OTLP_TRACES_COMPRESSION",
		"OTEL_EXPORTER_OTLP_METRICS_COMPRESSION",
	} {
		if signal := otlpSignalFromEnvName(name); signal != "" && unreachableSignalOverrides[signal] {
			continue
		}
		value := os.Getenv(name)
		if strings.HasSuffix(name, "_ENDPOINT") && !isWorkerReachableOTLPEndpoint(value) {
			value = ""
		}
		envVars = setControllerEnv(envVars, name, safeWorkerOTLPEnvValue(name, value))
	}
	return envVars
}

func unreachableWorkerOTLPSignalEndpoints(getenv func(string) string) map[string]bool {
	out := map[string]bool{}
	for _, signal := range []string{"TRACES", "METRICS"} {
		name := "OTEL_EXPORTER_OTLP_" + signal + "_ENDPOINT"
		if strings.TrimSpace(getenv(name)) != "" && !isWorkerReachableOTLPEndpoint(getenv(name)) {
			out[signal] = true
		}
	}
	return out
}

func otlpSignalFromEnvName(name string) string {
	for _, signal := range []string{"TRACES", "METRICS"} {
		if strings.HasPrefix(name, "OTEL_EXPORTER_OTLP_"+signal+"_") {
			return signal
		}
	}
	return ""
}

func appendTaskEnvVars(envVars []corev1.EnvVar, task *corev1alpha1.Task) []corev1.EnvVar {
	if task == nil {
		return envVars
	}
	for _, envVar := range task.Spec.Env {
		if isReservedTaskTelemetryEnv(task, envVar.Name) {
			continue
		}
		envVars = append(envVars, envVar)
	}
	return envVars
}

func isReservedTaskTelemetryEnv(task *corev1alpha1.Task, name string) bool {
	if task == nil {
		return false
	}
	switch task.Spec.Type {
	case corev1alpha1.TaskTypeAI:
		return isReservedAIWorkerTelemetryEnv(name)
	case corev1alpha1.TaskTypeAgent:
		return isReservedTraceContextEnv(name)
	default:
		return false
	}
}

func isReservedAIWorkerTelemetryEnv(name string) bool {
	if isReservedTraceContextEnv(name) {
		return true
	}
	switch name {
	case workerenv.EnableTelemetry, "OTEL_RESOURCE_ATTRIBUTES":
		return true
	default:
		return strings.HasPrefix(name, "OTEL_EXPORTER_OTLP")
	}
}

func isReservedTraceContextEnv(name string) bool {
	switch name {
	case workerenv.TraceParent, workerenv.TraceState, workerenv.TraceBaggage:
		return true
	default:
		return false
	}
}

func addTransactionEnvVars(
	envVars []corev1.EnvVar,
	tx *corev1alpha1.TaskTransaction,
	credentialReadScopes []string,
) []corev1.EnvVar {
	if tx == nil {
		return envVars
	}
	envVars = setControllerEnv(envVars, workerenv.TransactionID, tx.ID)
	envVars = setControllerEnv(envVars, workerenv.TransactionProfile, tx.Profile)
	envVars = setControllerEnv(envVars, workerenv.TransactionIssuer, tx.Issuer)
	envVars = setControllerEnv(envVars, workerenv.TransactionSubject, tx.Subject)
	envVars = setControllerEnv(envVars, workerenv.TransactionRequestingWorkload, tx.RequestingWorkload)
	envVars = setControllerEnv(envVars, workerenv.TransactionScope, tx.Scope)
	envVars = setControllerEnv(envVars, workerenv.TransactionScopes, workerenv.JoinCSV(tx.Scopes))
	envVars = setControllerEnv(envVars, workerenv.TransactionContextDigest, tx.ContextDigest)
	envVars = setControllerEnv(envVars, workerenv.TransactionRequesterContextDigest, tx.RequesterContextDigest)
	envVars = setControllerEnv(envVars, workerenv.TransactionCredentialSecret, tx.Context["secret"])
	envVars = setControllerEnv(
		envVars,
		workerenv.TransactionCredentialReadScopes,
		workerenv.JoinCSV(credentialReadScopes),
	)
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

	for _, envVar := range (workerenv.CoordinationEnv{
		Enabled:                 true,
		MaxDepth:                int(agent.Spec.Coordination.MaxDepth),
		MaxChildren:             int(agent.Spec.Coordination.MaxConcurrentChildren),
		AllowedAgents:           agentNames,
		Depth:                   depth,
		AutonomousMode:          agent.Spec.Coordination.Autonomous,
		AutonomousIteration:     int(task.Status.Iteration),
		AutonomousMaxIterations: int(agent.Spec.Coordination.MaxIterations),
	}).EnvVars() {
		envVars = setControllerEnvValue(envVars, envVar.Name, envVar.Value)
	}
	return setControllerEnvValue(envVars, workerenv.ApprovalRequiredTools, workerenv.JoinCSV(agent.Spec.Coordination.ApprovalRequiredTools))
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

	disableCoordinationToolInjection := task.Annotations[labels.AnnotationDisableCoordinationToolInject] == scheduledRunLabelValue

	// Auto-inject coordination tools when coordination is enabled, unless the
	// task deliberately supplies a narrower explicit tool set.
	if agent != nil && agent.Spec.Coordination != nil && agent.Spec.Coordination.Enabled && !disableCoordinationToolInjection {
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
			"list_pull_requests",
			"check_pr_review_marker",
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
		if agent.Spec.Coordination.Autonomous && !slices.Contains(cfg.tools, "request_approval") {
			cfg.tools = append(cfg.tools, "request_approval")
		}
	}

	// Auto-inject messaging tools for child tasks (tasks delegated by a coordinator)
	// so they can communicate with sibling tasks via send_message/check_messages
	_, isChildTask := task.Labels[labels.LabelParentTask]
	if isChildTask && !disableCoordinationToolInjection {
		for _, ct := range []string{"send_message", "check_messages"} {
			if !slices.Contains(cfg.tools, ct) {
				cfg.tools = append(cfg.tools, ct)
			}
		}
	}

	if len(cfg.tools) > 0 {
		envVars = setControllerEnvValue(envVars, workerenv.AITools, strings.Join(cfg.tools, ","))
	}

	if agent != nil && agent.Spec.Coordination != nil && agent.Spec.Coordination.Enabled {
		envVars = b.addCoordinationEnvVars(envVars, task, agent)
	}

	// Enable coordination in worker for child tasks so messaging tools are registered
	if isChildTask && (agent == nil || agent.Spec.Coordination == nil || !agent.Spec.Coordination.Enabled) {
		envVars = append(envVars, corev1.EnvVar{Name: workerenv.CoordinationEnabled, Value: scheduledRunLabelValue})
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
			Name: workerenv.AllowBash, Value: scheduledRunLabelValue,
		})
	}

	return envVars
}

func (b *JobBuilder) addTransactionTokenSecret(job *batchv1.Job, task *corev1alpha1.Task) {
	if job == nil || len(job.Spec.Template.Spec.Containers) == 0 {
		return
	}
	secretName := ""
	if task != nil && task.Annotations != nil {
		secretName = strings.TrimSpace(task.Annotations[labels.AnnotationTransactionTokenSecret])
	}
	injectTTS := b.shouldInjectContextTokenTTS(task, secretName)
	for i := range job.Spec.Template.Spec.Containers {
		container := &job.Spec.Template.Spec.Containers[i]
		container.Env = setControllerEnv(container.Env, workerenv.OutboundAccessTrustedGatewayServices, b.OutboundAccessTrustedGatewayServices)
		container.Env = setControllerEnv(container.Env, workerenv.OutboundAccessTrustedTokenEndpointServices, b.OutboundAccessTrustedTokenEndpointServices)
		if injectTTS {
			container.Env = setControllerEnv(container.Env, workerenv.ContextTokenTTSEndpoint, b.ContextTokenTTSEndpoint)
			container.Env = setControllerEnv(container.Env, workerenv.ContextTokenTTSAudience, b.ContextTokenTTSAudience)
			container.Env = setControllerEnv(container.Env, workerenv.ContextTokenTTSTimeout, b.ContextTokenTTSTimeout)
			container.Env = setControllerEnv(container.Env, workerenv.ContextTokenTTSTokenSource, b.ContextTokenTTSTokenSource)
			container.Env = setControllerEnv(container.Env, workerenv.ContextTokenSubjectTokenType, b.ContextTokenSubjectTokenType)
			container.Env = setControllerEnv(container.Env, workerenv.ContextTokenChildScope, b.ContextTokenChildScope)
			container.Env = setControllerEnv(container.Env, workerenv.ContextTokenOutboundScope, b.ContextTokenOutboundScope)
			container.Env = setControllerEnv(container.Env, workerenv.ContextTokenChildTokenTTL, b.ContextTokenChildTokenTTL)
			container.Env = setControllerEnv(container.Env, workerenv.ContextTokenToolTokenTTL, b.ContextTokenToolTokenTTL)
		} else {
			for _, name := range contextTokenTTSEnvNames() {
				container.Env = removeControllerEnv(container.Env, name)
			}
		}
		container.Env = removeControllerEnv(container.Env, workerenv.TransactionTokenFile)
		container.Env = removeControllerEnv(container.Env, workerenv.ContextTokenSubjectTokenFile)
	}

	if secretName == "" {
		return
	}
	const (
		volumeName = "transaction-token"
		mountPath  = "/var/run/orka/transaction-token"
		tokenPath  = mountPath + "/token"
	)
	defaultMode := int32(0400)
	// The child transaction token is delegation authority. Add the Secret as a
	// pod volume and expose the mounted token-file path to every workload
	// container in the pod so secondary containers can make TTS-mediated calls.
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
	for i := range job.Spec.Template.Spec.Containers {
		container := &job.Spec.Template.Spec.Containers[i]
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      volumeName,
			MountPath: mountPath,
			ReadOnly:  true,
		})
		container.Env = setControllerEnv(container.Env, workerenv.TransactionTokenFile, tokenPath)
		if b.ContextTokenTTSTokenSource == contexttoken.TTSTokenSourceIncoming {
			container.Env = setControllerEnv(container.Env, workerenv.ContextTokenSubjectTokenFile, tokenPath)
		} else {
			container.Env = removeControllerEnv(container.Env, workerenv.ContextTokenSubjectTokenFile)
		}
	}
}

func (b *JobBuilder) shouldInjectContextTokenTTS(task *corev1alpha1.Task, secretName string) bool {
	if b.ContextTokenTTSEndpoint == "" {
		return false
	}
	if secretName != "" {
		return true
	}
	if task == nil || task.Spec.Transaction == nil {
		return false
	}
	return b.ContextTokenTTSTokenSource != contexttoken.TTSTokenSourceIncoming
}

func contextTokenTTSEnvNames() []string {
	return []string{
		workerenv.ContextTokenTTSEndpoint,
		workerenv.ContextTokenTTSAudience,
		workerenv.ContextTokenTTSTimeout,
		workerenv.ContextTokenTTSTokenSource,
		workerenv.ContextTokenSubjectTokenType,
		workerenv.ContextTokenChildScope,
		workerenv.ContextTokenOutboundScope,
		workerenv.ContextTokenChildTokenTTL,
		workerenv.ContextTokenToolTokenTTL,
	}
}

// addSecretVolumes adds secret volumes to the Job
func (b *JobBuilder) addSecretVolumes(ctx context.Context, job *batchv1.Job, task *corev1alpha1.Task, agent *corev1alpha1.Agent, provider *corev1alpha1.Provider) error {
	allowDirectProviderSecrets := b.directProviderSecretsAllowed(task)
	allowDirectSecretMounts := b.directSecretMountsAllowed(task)

	if taskRequestsReadOnlyAgent(task) {
		if err := b.addReadOnlyAgentRuntimeSecretEnv(ctx, job, task, agent); err != nil {
			return err
		}
	}

	// Add provider secret (mounted as environment variable source)
	if allowDirectProviderSecrets && provider != nil {
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
	if allowDirectProviderSecrets && agentHasFallbackProviders(agent) {
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
	if allowDirectSecretMounts && task.Spec.SecretRef != nil {
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
	if allowDirectSecretMounts && agent != nil && agent.Spec.SecretRef != nil {
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
		switch task.Spec.Type {
		case corev1alpha1.TaskTypeAI:
			job.Spec.Template.Spec.Containers[0].Env = reserveAIWorkerTelemetryEnvFromKeys(job.Spec.Template.Spec.Containers[0].Env)
		case corev1alpha1.TaskTypeAgent:
			job.Spec.Template.Spec.Containers[0].Env = reserveTraceContextEnvFromKeys(job.Spec.Template.Spec.Containers[0].Env)
		}
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

	return nil
}

func reserveTraceContextEnvFromKeys(envVars []corev1.EnvVar) []corev1.EnvVar {
	for _, name := range []string{workerenv.TraceParent, workerenv.TraceState, workerenv.TraceBaggage} {
		if !envVarExists(envVars, name) {
			envVars = append(envVars, corev1.EnvVar{Name: name})
		}
	}
	return envVars
}

func reserveAIWorkerTelemetryEnvFromKeys(envVars []corev1.EnvVar) []corev1.EnvVar {
	for _, name := range reservedAIWorkerTelemetryEnvNames() {
		if !envVarExists(envVars, name) {
			envVars = append(envVars, corev1.EnvVar{Name: name})
		}
	}
	return envVars
}

func reservedAIWorkerTelemetryEnvNames() []string {
	return []string{
		workerenv.EnableTelemetry,
		workerenv.TraceParent,
		workerenv.TraceState,
		workerenv.TraceBaggage,
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_PROTOCOL",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
		"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
		"OTEL_EXPORTER_OTLP_INSECURE",
		"OTEL_EXPORTER_OTLP_TRACES_INSECURE",
		"OTEL_EXPORTER_OTLP_METRICS_INSECURE",
		"OTEL_EXPORTER_OTLP_HEADERS",
		"OTEL_EXPORTER_OTLP_TRACES_HEADERS",
		"OTEL_EXPORTER_OTLP_METRICS_HEADERS",
		"OTEL_EXPORTER_OTLP_TIMEOUT",
		"OTEL_EXPORTER_OTLP_TRACES_TIMEOUT",
		"OTEL_EXPORTER_OTLP_METRICS_TIMEOUT",
		"OTEL_EXPORTER_OTLP_COMPRESSION",
		"OTEL_EXPORTER_OTLP_TRACES_COMPRESSION",
		"OTEL_EXPORTER_OTLP_METRICS_COMPRESSION",
		"OTEL_EXPORTER_OTLP_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_METRICS_CLIENT_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_CLIENT_KEY",
		"OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY",
		"OTEL_EXPORTER_OTLP_METRICS_CLIENT_KEY",
		"OTEL_RESOURCE_ATTRIBUTES",
	}
}

func validateReadOnlyAgentRuntime(task *corev1alpha1.Task, agent *corev1alpha1.Agent) error {
	if !taskRequestsReadOnlyAgent(task) || agent == nil || agent.Spec.Runtime == nil {
		return nil
	}
	if agent.Spec.Runtime.Type == corev1alpha1.AgentRuntimeCodex {
		return fmt.Errorf("read-only agent tasks do not support codex runtime because Codex requires shell access while model credentials are exposed as environment variables")
	}
	if agent.Spec.Runtime.Type == corev1alpha1.AgentRuntimeCopilot {
		return fmt.Errorf("read-only agent tasks do not support copilot runtime credentials because GITHUB_TOKEN can mutate GitHub")
	}
	return nil
}

func (b *JobBuilder) addReadOnlyAgentRuntimeSecretEnv(ctx context.Context, job *batchv1.Job, task *corev1alpha1.Task, agent *corev1alpha1.Agent) error {
	if agent == nil || agent.Spec.SecretRef == nil || strings.TrimSpace(agent.Spec.SecretRef.Name) == "" {
		return nil
	}
	keys, err := readOnlyAgentRuntimeSecretKeys(agent)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}

	secret := &corev1.Secret{}
	secretName := strings.TrimSpace(agent.Spec.SecretRef.Name)
	if err := b.Get(ctx, client.ObjectKey{Name: secretName, Namespace: task.Namespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("read-only agent runtime secret %q not found in namespace %q", secretName, task.Namespace)
		}
		return fmt.Errorf("failed to get read-only agent runtime secret %q: %w", secretName, err)
	}
	if !readOnlyAgentRuntimeSecretHasCredential(secret, agent) {
		return fmt.Errorf("read-only agent runtime secret %q contains no supported auth credential keys for runtime %q", secretName, readOnlyAgentRuntimeType(agent))
	}

	added := 0
	for _, key := range keys {
		if _, ok := secret.Data[key]; !ok {
			continue
		}
		job.Spec.Template.Spec.Containers[0].Env = append(job.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{
			Name: key,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  key,
				},
			},
		})
		added++
	}
	if added == 0 {
		return fmt.Errorf("read-only agent runtime secret %q contains no supported keys for runtime %q", secretName, readOnlyAgentRuntimeType(agent))
	}
	return nil
}

func readOnlyAgentRuntimeSecretHasCredential(secret *corev1.Secret, agent *corev1alpha1.Agent) bool {
	if secret == nil {
		return false
	}
	switch readOnlyAgentRuntimeType(agent) {
	case corev1alpha1.AgentRuntimeClaude:
		for _, key := range []string{workerenv.AnthropicAPIKey, "ANTHROPIC_FOUNDRY_API_KEY"} {
			if value := strings.TrimSpace(string(secret.Data[key])); value != "" {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func readOnlyAgentRuntimeSecretKeys(agent *corev1alpha1.Agent) ([]string, error) {
	switch readOnlyAgentRuntimeType(agent) {
	case corev1alpha1.AgentRuntimeCodex:
		return nil, fmt.Errorf("read-only agent tasks do not support codex runtime because Codex requires shell access while model credentials are exposed as environment variables")
	case corev1alpha1.AgentRuntimeClaude:
		return []string{
			workerenv.AnthropicAPIKey,
			workerenv.AnthropicBaseURL,
			"CLAUDE_CODE_USE_FOUNDRY",
			"ANTHROPIC_FOUNDRY_API_KEY",
			"ANTHROPIC_FOUNDRY_RESOURCE",
			"ANTHROPIC_DEFAULT_SONNET_MODEL",
			"ANTHROPIC_DEFAULT_HAIKU_MODEL",
			"ANTHROPIC_DEFAULT_OPUS_MODEL",
		}, nil
	case corev1alpha1.AgentRuntimeCopilot:
		return nil, fmt.Errorf("read-only agent tasks do not support copilot runtime credentials because GITHUB_TOKEN can mutate GitHub")
	default:
		return nil, nil
	}
}

func readOnlyAgentRuntimeType(agent *corev1alpha1.Agent) corev1alpha1.AgentRuntimeType {
	if agent == nil || agent.Spec.Runtime == nil {
		return corev1alpha1.AgentRuntimeClaude
	}
	switch agent.Spec.Runtime.Type {
	case corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude, corev1alpha1.AgentRuntimeCopilot:
		return agent.Spec.Runtime.Type
	default:
		return corev1alpha1.AgentRuntimeClaude
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

	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "session-data",
			MountPath: "/session",
		},
	}
	if !podShouldAutomountServiceAccountToken(task) {
		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: "session-token",
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{
						{
							ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
								Path:              "token",
								ExpirationSeconds: new(int64(3600)),
							},
						},
					},
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "session-token",
			MountPath: "/var/run/secrets/kubernetes.io/serviceaccount",
			ReadOnly:  true,
		})
	}

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
		VolumeMounts: volumeMounts,
	}

	job.Spec.Template.Spec.InitContainers = append(job.Spec.Template.Spec.InitContainers, initContainer)

	// Add session env vars
	job.Spec.Template.Spec.Containers[0].Env = append(
		job.Spec.Template.Spec.Containers[0].Env,
		corev1.EnvVar{Name: workerenv.SessionName, Value: sessionName},
	)
}

func workerReachableOTLPEndpointConfigured(getenv func(string) string) bool {
	for _, name := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
	} {
		if isWorkerReachableOTLPEndpoint(getenv(name)) {
			return true
		}
	}
	return false
}

func isWorkerReachableOTLPEndpoint(value string) bool {
	host := otlpEndpointHost(value)
	if host == "" {
		return false
	}
	if host == "localhost" {
		return false
	}
	hostWithoutZone, _, _ := strings.Cut(host, "%")
	if ip, err := netip.ParseAddr(hostWithoutZone); err == nil {
		ip = ip.Unmap()
		return !ip.IsLoopback() && !ip.IsUnspecified()
	}
	return true
}

func otlpEndpointHost(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parseValue := value
	if !strings.Contains(value, "://") {
		parseValue = "//" + value
	}
	parsed, err := url.Parse(parseValue)
	if err != nil {
		return ""
	}
	host := parsed.Hostname()
	return strings.ToLower(strings.Trim(host, "[]"))
}

func safeWorkerOTLPEnvValue(name, value string) string {
	if !strings.HasSuffix(name, "_ENDPOINT") {
		return value
	}
	value = strings.TrimSpace(value)
	parseValue := value
	schemeLess := !strings.Contains(value, "://")
	if schemeLess {
		parseValue = "//" + value
	}
	parsed, err := url.Parse(parseValue)
	if err != nil {
		return value
	}
	if parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" {
		return value
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	sanitized := parsed.String()
	if schemeLess {
		return strings.TrimPrefix(sanitized, "//")
	}
	return sanitized
}

func setControllerEnvValue(envVars []corev1.EnvVar, name, value string) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(envVars)+1)
	set := false
	for _, envVar := range envVars {
		if envVar.Name != name {
			out = append(out, envVar)
			continue
		}
		if !set {
			out = append(out, corev1.EnvVar{Name: name, Value: value})
			set = true
		}
	}
	if !set {
		out = append(out, corev1.EnvVar{Name: name, Value: value})
	}
	return out
}

func setControllerEnv(envVars []corev1.EnvVar, name, value string) []corev1.EnvVar {
	if value == "" {
		return removeControllerEnv(envVars, name)
	}
	out := make([]corev1.EnvVar, 0, len(envVars)+1)
	set := false
	for _, envVar := range envVars {
		if envVar.Name != name {
			out = append(out, envVar)
			continue
		}
		if !set {
			out = append(out, corev1.EnvVar{Name: name, Value: value})
			set = true
		}
	}
	if !set {
		out = append(out, corev1.EnvVar{Name: name, Value: value})
	}
	return out
}

func removeControllerEnv(envVars []corev1.EnvVar, name string) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(envVars))
	for _, envVar := range envVars {
		if envVar.Name != name {
			out = append(out, envVar)
		}
	}
	return out
}

func envVarExists(envVars []corev1.EnvVar, name string) bool {
	for _, envVar := range envVars {
		if envVar.Name == name {
			return true
		}
	}
	return false
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

	// AllowedTools: read-only task override > task override > agent default
	var allowedTools []string
	if agent != nil && agent.Spec.Runtime != nil {
		allowedTools = agent.Spec.Runtime.DefaultAllowedTools
	}
	if task.Spec.AgentRuntime != nil && len(task.Spec.AgentRuntime.AllowedTools) > 0 {
		allowedTools = task.Spec.AgentRuntime.AllowedTools
	}
	if taskRequestsReadOnlyAgent(task) {
		allowedTools = readOnlyAgentAllowedTools()
		envVars = setControllerEnv(envVars, workerenv.ClaudeBare, scheduledRunLabelValue)
		envVars = setControllerEnv(envVars, workerenv.ClaudeDisableSettingSources, scheduledRunLabelValue)
		envVars = setControllerEnv(envVars, workerenv.ClaudePermissionMode, "dontAsk")
		envVars = removeControllerEnv(envVars, workerenv.AllowedTools)
		envVars = removeControllerEnv(envVars, workerenv.DisallowedTools)
		envVars = removeControllerEnv(envVars, workerenv.AllowBash)
	}
	if len(allowedTools) > 0 {
		envVars = append(envVars, corev1.EnvVar{
			Name: workerenv.AllowedTools, Value: joinStrings(allowedTools),
		})
	}

	// DisallowedTools (task only, plus read-only guardrails)
	var disallowedTools []string
	if task.Spec.AgentRuntime != nil && len(task.Spec.AgentRuntime.DisallowedTools) > 0 {
		disallowedTools = task.Spec.AgentRuntime.DisallowedTools
	}
	if taskRequestsReadOnlyAgent(task) {
		disallowedTools = append(disallowedTools, readOnlyAgentDisallowedTools()...)
	}
	if len(disallowedTools) > 0 {
		envVars = append(envVars, corev1.EnvVar{
			Name:  workerenv.DisallowedTools,
			Value: joinStrings(disallowedTools),
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
	if taskRequestsReadOnlyAgent(task) {
		allowBash = false
	}
	if allowBash {
		envVars = append(envVars, corev1.EnvVar{
			Name: workerenv.AllowBash, Value: scheduledRunLabelValue,
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
			Name: workerenv.RequirePushBranch, Value: scheduledRunLabelValue,
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
		if b.directGitCredentialsAllowed(task) && !taskUsesWorkspaceInitContainer(task) {
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
}

func taskNeedsWorkspace(task *corev1alpha1.Task) bool {
	return task != nil && (task.Spec.Type == corev1alpha1.TaskTypeAgent || effectiveWorkspace(task) != nil)
}

func taskUsesWorkspaceInitContainer(task *corev1alpha1.Task) bool {
	return task != nil && task.Annotations[labels.AnnotationWorkspaceInitContainer] == scheduledRunLabelValue
}

func taskRequestsReadOnlyAgent(task *corev1alpha1.Task) bool {
	return task != nil && task.Annotations[labels.AnnotationAgentReadOnly] == scheduledRunLabelValue
}

func readOnlyAgentAllowedTools() []string {
	return []string{
		"Read(/workspace/**)",
		"Glob(/workspace/**)",
		"Grep(/workspace/**)",
		"LS(/workspace/**)",
	}
}

func readOnlyAgentDisallowedTools() []string {
	deniedReadPaths := []string{
		"/proc/**",
		"/var/run/secrets/**",
		"/secrets/**",
		"/home/worker/**",
	}
	disallowed := []string{"Bash", "Write", "Edit", "MultiEdit", "NotebookEdit", "WebFetch", "WebSearch"}
	for _, deniedPath := range deniedReadPaths {
		disallowed = append(disallowed,
			"Read("+deniedPath+")",
			"Glob("+deniedPath+")",
			"Grep("+deniedPath+")",
			"LS("+deniedPath+")",
		)
	}
	return disallowed
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
				labels.LabelManaged: scheduledRunLabelValue,
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
