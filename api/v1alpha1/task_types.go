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
	// TaskTypeAgent runs external agent CLI runtimes (e.g., Copilot CLI, Claude Code CLI)
	TaskTypeAgent TaskType = "agent"
)

// TaskPhase defines the phase of task execution
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Scheduled
type TaskPhase string

const (
	TaskPhasePending   TaskPhase = "Pending"
	TaskPhaseRunning   TaskPhase = "Running"
	TaskPhaseSucceeded TaskPhase = "Succeeded"
	TaskPhaseFailed    TaskPhase = "Failed"
	TaskPhaseScheduled TaskPhase = "Scheduled"
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

// TaskSpec defines the desired state of Task
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

	// PriorTaskRef references a previously completed task whose diff should be
	// applied to the workspace before this task begins execution.
	// +optional
	PriorTaskRef *PriorTaskReference `json:"priorTaskRef,omitempty"`
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
	Append bool `json:"append,omitempty"`

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

	// Skills references ConfigMaps containing skill definitions
	// +optional
	Skills []SkillReference `json:"skills,omitempty"`

	// Tools lists the tools available for this task
	// +optional
	Tools []string `json:"tools,omitempty"`
}

// SkillReference references a ConfigMap containing a skill definition
type SkillReference struct {
	// ConfigMapRef references the ConfigMap containing the skill
	ConfigMapRef ConfigMapReference `json:"configMapRef"`
}

// ConfigMapReference references a ConfigMap
type ConfigMapReference struct {
	// Name is the name of the ConfigMap
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Key is the key within the ConfigMap (defaults to "skill.md")
	// +kubebuilder:default="skill.md"
	// +optional
	Key string `json:"key,omitempty"`
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

	// JobName is the name of the Kubernetes Job running the task
	// +optional
	JobName string `json:"jobName,omitempty"`

	// ResultRef indicates whether a result is available
	// +optional
	ResultRef *ResultReference `json:"resultRef,omitempty"`

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

// ResultReference indicates whether a result is available for the task
type ResultReference struct {
	// Available indicates whether a result has been stored for this task
	Available bool `json:"available"`
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
// +kubebuilder:validation:Enum=copilot;claude
type AgentRuntimeType string

const (
	// AgentRuntimeCopilot uses GitHub Copilot CLI as the agent runtime
	AgentRuntimeCopilot AgentRuntimeType = "copilot"
	// AgentRuntimeClaude uses Claude Code CLI as the agent runtime
	AgentRuntimeClaude AgentRuntimeType = "claude"
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
