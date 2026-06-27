/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TaskType defines the type of task
// +kubebuilder:validation:Enum=container;ai;agent
type TaskType string

const (
	// TaskTypeContainer runs arbitrary container commands
	TaskTypeContainer TaskType = "container"
	// TaskTypeAI runs AI agent tasks with LLM integration
	TaskTypeAI TaskType = "ai"
	// TaskTypeAgent runs external agent CLI runtimes (e.g., Copilot CLI, Claude Code CLI, Codex CLI)
	TaskTypeAgent TaskType = "agent"
)

// TaskPhase defines the phase of task execution
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Scheduled;Cancelled
type TaskPhase string

const (
	TaskPhasePending   TaskPhase = "Pending"
	TaskPhaseRunning   TaskPhase = "Running"
	TaskPhaseSucceeded TaskPhase = "Succeeded"
	TaskPhaseFailed    TaskPhase = "Failed"
	TaskPhaseScheduled TaskPhase = "Scheduled"
	TaskPhaseCancelled TaskPhase = "Cancelled"
)

// ConcurrencyPolicy describes how the controller will handle concurrent scheduled runs.
// +kubebuilder:validation:Enum=Allow;Forbid
type ConcurrencyPolicy string

const (
	// AllowConcurrent allows child tasks to run concurrently.
	AllowConcurrent ConcurrencyPolicy = "Allow"
	// ForbidConcurrent skips the new run if a previous run is still active.
	ForbidConcurrent ConcurrencyPolicy = "Forbid"
)

// RequestedBy records the verified identity that requested a task.
type RequestedBy struct {
	// Subject is the verified subject claim.
	// +optional
	Subject string `json:"subject,omitempty"`

	// Issuer is the token issuer that authenticated the requester.
	// +optional
	Issuer string `json:"issuer,omitempty"`

	// Username is the verified username, if present.
	// +optional
	Username string `json:"username,omitempty"`

	// Email is the email claim, if present.
	// +optional
	Email string `json:"email,omitempty"`

	// Groups are verified group values, if present.
	// +optional
	Groups []string `json:"groups,omitempty"`

	// Roles are verified role or scope values, if present.
	// +optional
	Roles []string `json:"roles,omitempty"`
}

// TaskTransaction records safe, verified transaction-token metadata for audit correlation.
type TaskTransaction struct {
	// Profile is the context-token profile that authenticated the request.
	// +optional
	Profile string `json:"profile,omitempty"`

	// ID is the verified transaction identifier claim.
	// +optional
	ID string `json:"id,omitempty"`

	// Issuer is the token issuer that authenticated the transaction.
	// +optional
	Issuer string `json:"issuer,omitempty"`

	// Audience lists the verified token audience values.
	// +optional
	Audience []string `json:"audience,omitempty"`

	// Subject is the verified subject claim.
	// +optional
	Subject string `json:"subject,omitempty"`

	// RequestingWorkload is the verified workload that requested the transaction.
	// +optional
	RequestingWorkload string `json:"requestingWorkload,omitempty"`

	// Scope is the original verified scope string.
	// +optional
	Scope string `json:"scope,omitempty"`

	// Scopes lists parsed scope values from the verified scope string.
	// +optional
	Scopes []string `json:"scopes,omitempty"`

	// ContextDigest is a SHA256 digest of the full transaction context.
	// +optional
	ContextDigest string `json:"contextDigest,omitempty"`

	// RequesterContextDigest is a SHA256 digest of the full requester context.
	// +optional
	RequesterContextDigest string `json:"requesterContextDigest,omitempty"`

	// Context contains allowlisted, non-sensitive transaction context fields for audit.
	// +optional
	Context map[string]string `json:"context,omitempty"`
}

// TaskSpec defines the desired state of Task
// +kubebuilder:validation:XValidation:rule="has(self.requestedBy) == has(oldSelf.requestedBy) && (!has(self.requestedBy) || self.requestedBy == oldSelf.requestedBy)",message="requestedBy is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.transaction) == has(oldSelf.transaction) && (!has(self.transaction) || self.transaction == oldSelf.transaction)",message="transaction is immutable"
type TaskSpec struct {
	// Type specifies the task type: "container" or "ai"
	// +kubebuilder:validation:Required
	Type TaskType `json:"type"`

	// Image is the container image to run for the task
	// +optional
	Image string `json:"image,omitempty"`

	// Command is the command to run in the container
	// +optional
	Command []string `json:"command,omitempty"`

	// Args are the arguments to pass to the command
	// +optional
	Args []string `json:"args,omitempty"`

	// Env is a list of environment variables to set in the container
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Timeout is the maximum duration for the task
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// Priority is the queue priority (0-1000, higher = more urgent)
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	// +kubebuilder:default=500
	// +optional
	Priority *int32 `json:"priority,omitempty"`

	// RetryPolicy defines the retry behavior for failed tasks
	// +optional
	RetryPolicy *RetryPolicy `json:"retryPolicy,omitempty"`

	// WebhookURL is the URL to call when the task completes
	// +optional
	WebhookURL string `json:"webhookURL,omitempty"`

	// SecretRef references a Kubernetes Secret containing credentials
	// +optional
	SecretRef *SecretReference `json:"secretRef,omitempty"`

	// SessionRef references a session for conversation continuity
	// +optional
	SessionRef *SessionReference `json:"sessionRef,omitempty"`

	// Resources defines the compute resources for the task
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Execution defines worker pod runtime and placement settings.
	// +optional
	Execution *ExecutionSpec `json:"execution,omitempty"`

	// Schedule is a cron expression for recurring tasks (e.g., "0 */6 * * *").
	// When set, the controller creates child Task CRs on each cron tick.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// TimeZone is the IANA time zone for the schedule (e.g., "America/New_York").
	// Defaults to UTC if not set.
	// +optional
	TimeZone *string `json:"timeZone,omitempty"`

	// ConcurrencyPolicy specifies how to treat concurrent runs (Allow or Forbid).
	// +kubebuilder:default="Forbid"
	// +optional
	ConcurrencyPolicy ConcurrencyPolicy `json:"concurrencyPolicy,omitempty"`

	// StartingDeadlineSeconds is the deadline in seconds for starting a missed scheduled run.
	// If the schedule is missed by more than this many seconds, the run is skipped.
	// +kubebuilder:default=100
	// +optional
	StartingDeadlineSeconds *int64 `json:"startingDeadlineSeconds,omitempty"`

	// SuccessfulRunsHistoryLimit is the number of successful child tasks to retain.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=3
	// +optional
	SuccessfulRunsHistoryLimit *int32 `json:"successfulRunsHistoryLimit,omitempty"`

	// FailedRunsHistoryLimit is the number of failed child tasks to retain.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	FailedRunsHistoryLimit *int32 `json:"failedRunsHistoryLimit,omitempty"`

	// Suspend tells the controller to suspend subsequent scheduled runs.
	// It does not apply to already started child tasks. Defaults to false.
	// +optional
	Suspend *bool `json:"suspend,omitempty"`

	// AI contains AI-specific configuration (when type is "ai")
	// +optional
	AI *AISpec `json:"ai,omitempty"`

	// AgentRef references an Agent CRD for configuration
	// +optional
	AgentRef *AgentReference `json:"agentRef,omitempty"`

	// Prompt is the task-specific prompt (used with agentRef)
	// +optional
	Prompt string `json:"prompt,omitempty"`

	// AgentRuntime contains task-level overrides for agent runtime configuration (when type is "agent")
	// +optional
	AgentRuntime *AgentRuntimeSpec `json:"agentRuntime,omitempty"`

	// Workspace defines repository checkout and push settings for tasks that need
	// a git workspace. Agent tasks can continue to use agentRuntime.workspace for
	// compatibility; this top-level field is used by container tasks as well.
	// +optional
	Workspace *WorkspaceConfig `json:"workspace,omitempty"`

	// PriorTaskRef references a previously completed task whose diff should be
	// applied to the workspace before this task begins execution.
	// +optional
	PriorTaskRef *PriorTaskReference `json:"priorTaskRef,omitempty"`

	// RequestedBy records the verified identity that created the task.
	// This field is populated by the API server and is immutable.
	// +optional
	RequestedBy *RequestedBy `json:"requestedBy,omitempty"`

	// Transaction records verified transaction-token metadata for audit correlation.
	// This field is populated by the API server and is immutable.
	// +optional
	Transaction *TaskTransaction `json:"transaction,omitempty"`
}

// RetryPolicy defines retry behavior for failed tasks
type RetryPolicy struct {
	// MaxRetries is the maximum number of retry attempts
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	// +optional
	MaxRetries int32 `json:"maxRetries,omitempty"`

	// BackoffMultiplier is the exponential backoff multiplier
	// +kubebuilder:default=2
	// +optional
	BackoffMultiplier float64 `json:"backoffMultiplier,omitempty"`

	// InitialDelay is the initial delay before the first retry
	// +optional
	InitialDelay *metav1.Duration `json:"initialDelay,omitempty"`
}

// SecretReference references a Kubernetes Secret
type SecretReference struct {
	// Name is the name of the Secret
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace is the namespace of the Secret (defaults to Task namespace)
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// SessionReference references a session for conversation continuity
type SessionReference struct {
	// Name is the session identifier (ConfigMap: session-<name>)
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Create indicates whether to create the session if it doesn't exist
	// +kubebuilder:default=false
	// +optional
	Create bool `json:"create,omitempty"`

	// Append indicates whether to append task messages to the session transcript
	// +kubebuilder:default=true
	// +optional
	Append bool `json:"append"`

	// MaxMessages is the maximum number of messages to load from session
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=50
	// +optional
	MaxMessages int32 `json:"maxMessages,omitempty"`
}

// AgentReference references an Agent CRD
type AgentReference struct {
	// Name is the name of the Agent
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace is the namespace of the Agent (defaults to Task namespace)
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// PriorTaskReference references a previously completed task whose diff should be
// applied to the workspace before this task begins execution.
type PriorTaskReference struct {
	// Name is the name of the prior task
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace is the namespace of the prior task (defaults to Task namespace)
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// ProviderReference references a Provider CRD
type ProviderReference struct {
	// Name is the name of the Provider
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace is the namespace of the Provider (defaults to Task namespace)
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// AISpec defines AI-specific configuration
type AISpec struct {
	// ProviderRef references a Provider CRD for LLM configuration
	// If set, provider and model fields are optional (defaults from Provider)
	// +optional
	ProviderRef *ProviderReference `json:"providerRef,omitempty"`

	// Provider is the LLM provider (anthropic, openai) - required if providerRef not set
	// +kubebuilder:validation:Enum=anthropic;openai
	// +optional
	Provider string `json:"provider,omitempty"`

	// Model is the model identifier - required if providerRef not set
	// +optional
	Model string `json:"model,omitempty"`

	// Prompt is the user prompt for the AI task
	// +optional
	Prompt string `json:"prompt,omitempty"`

	// SystemPrompt is an optional system prompt
	// +optional
	SystemPrompt string `json:"systemPrompt,omitempty"`

	// Temperature controls randomness in generation
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=2
	// +optional
	Temperature *float64 `json:"temperature,omitempty"`

	// MaxTokens limits the response length
	// +optional
	MaxTokens *int32 `json:"maxTokens,omitempty"`

	// Skills references Skill CRDs to inject into the agent's system prompt
	// +optional
	Skills []SkillReference `json:"skills,omitempty"`

	// Tools lists the tools available for this task
	// +optional
	Tools []string `json:"tools,omitempty"`
}

// SkillReference references a Skill CRD by name or inline skill content from a ConfigMap key.
type SkillReference struct {
	// Name references a Skill CR by name
	// +optional
	Name string `json:"name,omitempty"`

	// ConfigMapRef references a ConfigMap key containing skill text
	// +optional
	ConfigMapRef *ConfigMapKeySelector `json:"configMapRef,omitempty"`
}

// TaskStatus defines the observed state of Task
type TaskStatus struct {
	// Phase is the current phase of the task
	// +optional
	Phase TaskPhase `json:"phase,omitempty"`

	// StartTime is when the task started running
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the task completed
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Attempts is the number of attempts made
	// +optional
	Attempts int32 `json:"attempts,omitempty"`

	// Iteration is the current autonomous loop iteration (0-based).
	// Only used when the task's coordination config has autonomous mode enabled.
	// +optional
	Iteration int32 `json:"iteration,omitempty"`

	// JobName is the name of the Kubernetes Job running the task
	// +optional
	JobName string `json:"jobName,omitempty"`

	// ResultRef indicates whether a result is available
	// +optional
	ResultRef *ResultReference `json:"resultRef,omitempty"`

	// ExecutionWorkspace reports the provider-neutral lifecycle state for a
	// requested execution workspace. Provider-native identifiers and credentials
	// are intentionally omitted.
	// +optional
	ExecutionWorkspace *ExecutionWorkspaceStatus `json:"executionWorkspace,omitempty"`

	// HarnessRuntime records the controller-resolved harness runtime target for an
	// in-flight agent turn. It intentionally stores only non-secret routing metadata
	// and Secret references, never bearer values.
	// +optional
	HarnessRuntime *HarnessRuntimeStatus `json:"harnessRuntime,omitempty"`

	// WebhookDelivered indicates whether the webhook was successfully called
	// +optional
	WebhookDelivered bool `json:"webhookDelivered,omitempty"`

	// Message provides additional status information
	// +optional
	Message string `json:"message,omitempty"`

	// ChildTasks tracks delegated child tasks (for coordinator agents)
	// +optional
	ChildTasks []ChildTaskStatus `json:"childTasks,omitempty"`

	// Conditions represent the current state of the Task
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastScheduleTime is the last time a child task was created for a scheduled run.
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`

	// NextScheduleTime is the next time a child task will be created.
	// +optional
	NextScheduleTime *metav1.Time `json:"nextScheduleTime,omitempty"`
}

// HarnessRuntimeStatus records the resolved harness runtime selected by the controller.
type HarnessRuntimeStatus struct {
	// RuntimeRefName is the AgentRuntime name for custom runtimeRef turns. Empty means built-in CLI wrapper.
	// +optional
	RuntimeRefName string `json:"runtimeRefName,omitempty"`

	// RuntimeName is the runtime name advertised by the harness capabilities and sent in turn metadata.
	// +optional
	RuntimeName string `json:"runtimeName,omitempty"`

	// ContractVersion is the Orka harness contract version used for the turn.
	// +optional
	ContractVersion string `json:"contractVersion,omitempty"`

	// Endpoint is the non-secret harness base URL selected when the turn started.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// RuntimeGeneration is the AgentRuntime generation selected when the turn started.
	// +optional
	RuntimeGeneration int64 `json:"runtimeGeneration,omitempty"`

	// AuthRefName is the Secret name selected when the turn started.
	// +optional
	AuthRefName string `json:"authRefName,omitempty"`

	// AuthRefField is the Secret data field selected when the turn started.
	// +optional
	AuthRefField string `json:"authRefField,omitempty"`
}

// ResultReference indicates whether a result is available for the task
type ResultReference struct {
	// Available indicates whether a result has been stored for this task
	Available bool `json:"available"`
}

// ExecutionWorkspaceStatus is the safe status surface for execution workspace lifecycle.
type ExecutionWorkspaceStatus struct {
	// Provider is the resolved workspace backend.
	// +optional
	Provider WorkspaceProvider `json:"provider,omitempty"`

	// TemplateRef is the resolved workspace template.
	// +optional
	TemplateRef *WorkspaceTemplateReference `json:"templateRef,omitempty"`

	// Phase is the provider-neutral lifecycle phase.
	// +optional
	Phase ExecutionWorkspacePhase `json:"phase,omitempty"`

	// Reason is the provider-neutral lifecycle reason.
	// +optional
	Reason ExecutionWorkspaceReason `json:"reason,omitempty"`

	// ReusePolicy is the resolved reuse policy.
	// +optional
	ReusePolicy WorkspaceReusePolicy `json:"reusePolicy,omitempty"`

	// CleanupPolicy is the resolved cleanup policy.
	// +optional
	CleanupPolicy WorkspaceCleanupPolicy `json:"cleanupPolicy,omitempty"`

	// Reused reports whether an existing workspace was reattached.
	// +optional
	Reused bool `json:"reused,omitempty"`

	// Placement reports non-secret runtime placement metadata for the workspace.
	// +optional
	Placement *ExecutionWorkspacePlacementStatus `json:"placement,omitempty"`

	// Density reports non-secret actor and worker counts for the workspace provider.
	// +optional
	Density *ExecutionWorkspaceDensityStatus `json:"density,omitempty"`

	// ResumeLatency is the observed time spent resuming the workspace until it was ready.
	// +optional
	ResumeLatency *metav1.Duration `json:"resumeLatency,omitempty"`

	// Message contains sanitized lifecycle context.
	// +optional
	Message string `json:"message,omitempty"`

	// LastUpdateTime is the last time workspace status was updated.
	// +optional
	LastUpdateTime *metav1.Time `json:"lastUpdateTime,omitempty"`
}

// ExecutionWorkspacePlacementStatus is the safe placement surface for an execution workspace.
type ExecutionWorkspacePlacementStatus struct {
	// WorkerNamespace is the namespace containing the selected worker pod.
	// +optional
	WorkerNamespace string `json:"workerNamespace,omitempty"`

	// WorkerPool is the provider's worker-pool name when available.
	// +optional
	WorkerPool string `json:"workerPool,omitempty"`

	// WorkerPodName is the selected worker pod name when available.
	// +optional
	WorkerPodName string `json:"workerPodName,omitempty"`
}

// ExecutionWorkspaceDensityStatus reports provider-level actor density.
type ExecutionWorkspaceDensityStatus struct {
	// WorkerCount is the number of workers reported by the provider.
	// +optional
	WorkerCount int32 `json:"workerCount,omitempty"`

	// ActorCount is the number of actors reported by the provider.
	// +optional
	ActorCount int32 `json:"actorCount,omitempty"`

	// RunningActorCount is the number of actors currently running on workers.
	// +optional
	RunningActorCount int32 `json:"runningActorCount,omitempty"`

	// SuspendedActorCount is the number of actors currently suspended.
	// +optional
	SuspendedActorCount int32 `json:"suspendedActorCount,omitempty"`

	// ActorsPerWorker is ActorCount divided by WorkerCount, formatted as a decimal string.
	// +optional
	ActorsPerWorker string `json:"actorsPerWorker,omitempty"`
}

// ChildTaskStatus tracks the status of a delegated child task
type ChildTaskStatus struct {
	// Name is the name of the child task
	Name string `json:"name"`

	// Agent is the agent handling the child task
	Agent string `json:"agent"`

	// Phase is the current phase of the child task
	Phase TaskPhase `json:"phase"`

	// Result is the result from the child task (if completed)
	// +optional
	Result string `json:"result,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Priority",type=integer,JSONPath=`.spec.priority`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Task is the Schema for the tasks API
type Task struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TaskSpec   `json:"spec,omitempty"`
	Status TaskStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TaskList contains a list of Task
type TaskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Task `json:"items"`
}

// AgentRuntimeType defines the agent runtime to use
// +kubebuilder:validation:Enum=copilot;claude;codex
type AgentRuntimeType string

const (
	// AgentRuntimeCopilot uses GitHub Copilot CLI as the agent runtime
	AgentRuntimeCopilot AgentRuntimeType = "copilot"
	// AgentRuntimeClaude uses Claude Code CLI as the agent runtime
	AgentRuntimeClaude AgentRuntimeType = "claude"
	// AgentRuntimeCodex uses OpenAI Codex CLI as the agent runtime
	AgentRuntimeCodex AgentRuntimeType = "codex"
)

// AgentRuntimeSpec defines task-level overrides for agent runtime configuration.
// Runtime type and credentials come from the referenced Agent CRD.
type AgentRuntimeSpec struct {
	// Workspace defines the working directory configuration
	// +optional
	Workspace *WorkspaceConfig `json:"workspace,omitempty"`

	// MaxTurns limits the number of agent loop iterations
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	// +optional
	MaxTurns *int32 `json:"maxTurns,omitempty"`

	// AllowedTools lists the tools the agent is allowed to use (overrides Agent defaults)
	// +optional
	AllowedTools []string `json:"allowedTools,omitempty"`

	// DisallowedTools lists tools the agent is not allowed to use
	// +optional
	DisallowedTools []string `json:"disallowedTools,omitempty"`

	// AllowBash enables the agent to run bash commands (overrides Agent default)
	// +optional
	AllowBash *bool `json:"allowBash,omitempty"`
}

// WorkspaceConfig defines workspace setup for agent tasks
type WorkspaceConfig struct {
	// GitRepo is the repository URL to clone
	// +optional
	GitRepo string `json:"gitRepo,omitempty"`

	// Branch is the git branch to checkout
	// +optional
	Branch string `json:"branch,omitempty"`

	// Ref is a specific git ref (commit SHA, tag) to checkout
	// +optional
	Ref string `json:"ref,omitempty"`

	// GitSecretRef references a Secret containing git credentials
	// +optional
	GitSecretRef *corev1.LocalObjectReference `json:"gitSecretRef,omitempty"`

	// SubPath is a subdirectory within the repo to use as workspace root
	// +optional
	SubPath string `json:"subPath,omitempty"`

	// ForkRepo is the writable fork repository URL for pushing changes
	// +optional
	ForkRepo string `json:"forkRepo,omitempty"`

	// PRBaseBranch is the upstream branch to target for pull requests
	// +optional
	PRBaseBranch string `json:"prBaseBranch,omitempty"`

	// PushBranch is the remote branch name to push changes to after the agent completes.
	// When set, FinalizeResult will commit and push changes to this branch.
	// +optional
	PushBranch string `json:"pushBranch,omitempty"`
}

func init() {
	SchemeBuilder.Register(&Task{}, &TaskList{})
}
