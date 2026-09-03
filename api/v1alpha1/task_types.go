/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	"encoding/json"

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
// +kubebuilder:validation:Enum=Pending;Running;Finalizing;Succeeded;Failed;Scheduled;Cancelled
type TaskPhase string

const (
	TaskPhasePending    TaskPhase = "Pending"
	TaskPhaseRunning    TaskPhase = "Running"
	TaskPhaseFinalizing TaskPhase = "Finalizing"
	TaskPhaseSucceeded  TaskPhase = "Succeeded"
	TaskPhaseFailed     TaskPhase = "Failed"
	TaskPhaseScheduled  TaskPhase = "Scheduled"
	TaskPhaseCancelled  TaskPhase = "Cancelled"
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
// +kubebuilder:validation:XValidation:rule="self.type == oldSelf.type",message="type is immutable"
// +kubebuilder:validation:XValidation:rule="(has(self.workspace) && has(self.workspace.intent) ? self.workspace.intent : (self.type == 'agent' ? 'read' : self.type)) == (has(oldSelf.workspace) && has(oldSelf.workspace.intent) ? oldSelf.workspace.intent : (oldSelf.type == 'agent' ? 'read' : oldSelf.type))",message="effective workspace intent is immutable"
// +kubebuilder:validation:XValidation:rule="self.type != 'agent' || (has(self.prompt) == has(oldSelf.prompt) && (!has(self.prompt) || self.prompt == oldSelf.prompt))",message="agent prompt is immutable"
// +kubebuilder:validation:XValidation:rule="self.type != 'agent' || (has(self.agentRef) == has(oldSelf.agentRef) && (!has(self.agentRef) || self.agentRef == oldSelf.agentRef))",message="agentRef is immutable for agent Tasks"
// +kubebuilder:validation:XValidation:rule="self.type != 'agent' || (has(self.agentRuntime) == has(oldSelf.agentRuntime) && (!has(self.agentRuntime) || self.agentRuntime == oldSelf.agentRuntime))",message="agentRuntime is immutable for agent Tasks"
// +kubebuilder:validation:XValidation:rule="self.type != 'agent' || (has(self.sessionRef) == has(oldSelf.sessionRef) && (!has(self.sessionRef) || self.sessionRef == oldSelf.sessionRef))",message="sessionRef is immutable for agent Tasks"
// +kubebuilder:validation:XValidation:rule="self.type != 'agent' || (has(self.workspace) == has(oldSelf.workspace) && (!has(self.workspace) || self.workspace == oldSelf.workspace))",message="workspace is immutable for agent Tasks"
// +kubebuilder:validation:XValidation:rule="self.type != 'agent' || (has(self.timeout) == has(oldSelf.timeout) && (!has(self.timeout) || self.timeout == oldSelf.timeout))",message="timeout is immutable for agent Tasks"
// +kubebuilder:validation:XValidation:rule="!has(self.execution) || !has(self.execution.workspace) || self.execution.workspace.reusePolicy != 'session' || has(self.sessionRef)",message="session workspace reuse requires spec.sessionRef"
// +kubebuilder:validation:XValidation:rule="self.type != 'container' || !has(self.workspace) || !has(self.workspace.expectedRemoteSHA)",message="container Tasks do not support workspace.expectedRemoteSHA"
// +kubebuilder:validation:XValidation:rule="self.type != 'container' || !has(self.workspace) || (!has(self.workspace.createPR) || !self.workspace.createPR)",message="container Tasks do not support workspace.createPR"
// +kubebuilder:validation:XValidation:rule="self.type != 'container' || !has(self.workspace) || (!has(self.workspace.maxChangedFiles) && (!has(self.workspace.allowedPaths) || self.workspace.allowedPaths.size() == 0) && (!has(self.workspace.denyRepositoryControlPaths) || !self.workspace.denyRepositoryControlPaths) && (!has(self.workspace.rejectBinaryFiles) || !self.workspace.rejectBinaryFiles) && (!has(self.workspace.rejectSecretLikeContent) || !self.workspace.rejectSecretLikeContent))",message="container Tasks do not support clean-room workspace publication policies"
// +kubebuilder:validation:XValidation:rule="self.type != 'container' || !has(self.workspace) || !has(self.workspace.pushBranch) || self.workspace.pushBranch.size() == 0 || !has(self.image) || self.image.size() == 0",message="custom-image container Tasks do not support workspace.pushBranch publication"
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

	// Workspace defines the canonical repository workspace, intent, credentials,
	// and publication request. Agent Tasks that omit intent are interpreted as
	// read by controller logic; an omitted intent preserves existing container behavior.
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

	// ThroughMessageID limits transcript loading to the logical history at and before this stable message ID.
	// Gateway-created Tasks use it so later queued user messages cannot enter an earlier turn.
	// +kubebuilder:validation:MaxLength=256
	// +optional
	ThroughMessageID string `json:"throughMessageId,omitempty"`

	// PromptIncluded reports that the current Task prompt is already the final user message in the bounded transcript.
	// Workers must not append prompt a second time when this is true.
	// +optional
	PromptIncluded bool `json:"promptIncluded,omitempty"`
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
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.executionOutcome) || self.executionOutcome == oldSelf.executionOutcome",message="executionOutcome is immutable once recorded"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.agentExecutionBinding) || (has(self.agentExecutionBinding) && self.agentExecutionBinding == oldSelf.agentExecutionBinding)",message="agentExecutionBinding is write-once and immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.agentExecutionBinding) || self.agentExecutionBinding.contractVersion != 'orka.harness.v1' || ((!has(self.execution) || (has(oldSelf.execution) && self.execution == oldSelf.execution)) && (!has(self.delivery) || (has(oldSelf.delivery) && self.delivery == oldSelf.delivery)))",message="a v1-bound Task cannot acquire new v2 execution or delivery state"
// +kubebuilder:validation:XValidation:rule="!has(self.agentExecutionBinding) || self.agentExecutionBinding.contractVersion != 'orka.harness.v2' || !has(self.harnessRuntime) || (has(oldSelf.harnessRuntime) && self.harnessRuntime == oldSelf.harnessRuntime)",message="a v2-bound Task cannot acquire new v1 harness state"
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

	// Execution reports the durable execution state and terminal outcome for the
	// current attempt. Phase remains the compatibility projection.
	// +optional
	Execution *TaskExecutionStatus `json:"execution,omitempty"`

	// Delivery reports trusted workspace validation and publication reconciliation.
	// +optional
	Delivery *TaskDeliveryStatus `json:"delivery,omitempty"`

	// HarnessRuntime records the controller-resolved harness v1 runtime target
	// for an in-flight agent turn. It intentionally stores only non-secret
	// routing metadata and Secret references, never bearer values. Compatibility
	// surface for harness v1 bindings.
	// +optional
	HarnessRuntime *HarnessRuntimeStatus `json:"harnessRuntime,omitempty"`

	// AgentExecutionBinding is the authoritative, write-once, immutable
	// execution route for this agent Task.
	// +optional
	AgentExecutionBinding *AgentExecutionBinding `json:"agentExecutionBinding,omitempty"`

	// ExecutionOutcome records the immutable outcome of a non-ACP workload before
	// provider-neutral execution-workspace finalization completes.
	// +optional
	ExecutionOutcome *TaskWorkloadExecutionOutcome `json:"executionOutcome,omitempty"`

	// ExecutionWorkspace reports the provider-neutral lifecycle state for a
	// requested execution workspace. Provider-native identifiers and credentials
	// are intentionally omitted.
	// +optional
	ExecutionWorkspace *ExecutionWorkspaceStatus `json:"executionWorkspace,omitempty"`

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

	// LastScheduleTime is the schedule's progress cursor: the last time a
	// child task was created, or — when a run was missed past
	// startingDeadlineSeconds (for example while suspended) — the time the
	// skipped window was re-anchored so the schedule resumes from the
	// present instead of replaying missed runs.
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`

	// NextScheduleTime is the next time a child task will be created.
	// +optional
	NextScheduleTime *metav1.Time `json:"nextScheduleTime,omitempty"`
}

// TaskWorkloadExecutionOutcome records the immutable result of non-ACP workload
// execution before provider-neutral workspace finalization. ACP agent attempts use
// TaskStatus.Execution, whose stronger fencing and OutcomeUnknown semantics are
// defined in task_runtime_types.go.
type TaskWorkloadExecutionOutcome struct {
	// Phase is the terminal workload execution phase.
	// +kubebuilder:validation:Enum=Succeeded;Failed;Cancelled
	Phase TaskPhase `json:"phase"`

	// Attempt is the Task attempt that produced this outcome.
	// +kubebuilder:validation:Minimum=1
	Attempt int32 `json:"attempt"`

	// ResultRef indicates whether the corresponding result was persisted.
	// +optional
	ResultRef *ResultReference `json:"resultRef,omitempty"`

	// RecordedAt is when Orka durably recorded the execution outcome.
	RecordedAt metav1.Time `json:"recordedAt"`

	// Message contains sanitized execution context.
	// +optional
	Message string `json:"message,omitempty"`
}

// ResultReference indicates whether a result is available for the task
type ResultReference struct {
	// Available indicates whether a result has been stored for this task
	Available bool `json:"available"`
}

// WorkspaceObjectReference identifies a concrete workspace without exposing provider-native IDs.
type WorkspaceObjectReference struct {
	// Name is the ExecutionWorkspace name.
	Name string `json:"name"`

	// UID is the immutable ExecutionWorkspace UID.
	// +optional
	UID string `json:"uid,omitempty"`
}

// ExecutionWorkspaceStatus is the safe status surface for execution workspace lifecycle.
type ExecutionWorkspaceStatus struct {
	// ClassRef is the provider-neutral class selected for controller-first execution.
	// +optional
	ClassRef *WorkspaceClassReference `json:"classRef,omitempty"`

	// WorkspaceRef identifies the concrete ExecutionWorkspace in the Task namespace.
	// +optional
	WorkspaceRef *WorkspaceObjectReference `json:"workspaceRef,omitempty"`

	// State is the provider-neutral concrete workspace state.
	// +optional
	State string `json:"state,omitempty"`

	// AttachedEpoch is the attachment epoch enforced for this Task.
	// +optional
	AttachedEpoch int64 `json:"attachedEpoch,omitempty"`

	// Conditions project generic workspace admission, readiness, attachment, and finalization state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Provider is the resolved legacy workspace backend.
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
// +kubebuilder:metadata:annotations=gateway.orka.ai/session-cutoff-schema=v1
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Priority",type=integer,JSONPath=`.spec.priority`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:selectablefield:JSONPath=.spec.sessionRef.name
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || (!has(oldSelf.status.agentExecutionBinding) || self.spec == oldSelf.spec)",message="Task spec is immutable after execution authority is recorded"

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
// +kubebuilder:validation:Enum=claude;codex;copilot;opencode
type AgentRuntimeType string

const (
	// AgentRuntimeCopilot uses GitHub Copilot CLI as the agent runtime.
	AgentRuntimeCopilot AgentRuntimeType = "copilot"
	// AgentRuntimeClaude uses Claude Code CLI as the agent runtime
	AgentRuntimeClaude AgentRuntimeType = "claude"
	// AgentRuntimeCodex uses OpenAI Codex CLI as the agent runtime
	AgentRuntimeCodex AgentRuntimeType = "codex"
	// AgentRuntimeOpencode uses OpenCode CLI's native ACP server as the agent runtime.
	AgentRuntimeOpencode AgentRuntimeType = "opencode"
)

// AgentRuntimeSpec defines task-level overrides for agent runtime configuration.
// Runtime type and credentials come from the referenced Agent CRD.
// +kubebuilder:validation:XValidation:rule="!has(self.workspace) || (oldSelf.hasValue() && has(oldSelf.value().workspace) && self.workspace == oldSelf.value().workspace)",optionalOldSelf=true,message="legacy agentRuntime.workspace is a preserved harness v1 compatibility surface; new Tasks must use spec.workspace"
type AgentRuntimeSpec struct {
	// Workspace is the legacy harness v1 agent workspace configuration at its
	// historical JSON path. It is a preserved compatibility read surface for
	// stored v1 Tasks only: it can never be introduced or changed, and it is
	// not an authority surface for new work.
	// +optional
	Workspace *LegacyAgentWorkspaceConfig `json:"workspace,omitempty"`

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

// MarshalJSON preserves the distinction between an omitted task tool override
// and an explicitly empty deny-all override.
func (in AgentRuntimeSpec) MarshalJSON() ([]byte, error) {
	type agentRuntimeSpecJSON struct {
		Workspace       *LegacyAgentWorkspaceConfig `json:"workspace,omitempty"`
		MaxTurns        *int32                      `json:"maxTurns,omitempty"`
		AllowedTools    *[]string                   `json:"allowedTools,omitempty"`
		DisallowedTools []string                    `json:"disallowedTools,omitempty"`
		AllowBash       *bool                       `json:"allowBash,omitempty"`
	}
	var allowedTools *[]string
	if in.AllowedTools != nil {
		tools := append([]string{}, in.AllowedTools...)
		allowedTools = &tools
	}
	return json.Marshal(agentRuntimeSpecJSON{
		Workspace:       in.Workspace,
		MaxTurns:        in.MaxTurns,
		AllowedTools:    allowedTools,
		DisallowedTools: in.DisallowedTools,
		AllowBash:       in.AllowBash,
	})
}

// LegacyAgentWorkspaceConfig is the harness v1 agent workspace shape preserved
// at the historical spec.agentRuntime.workspace JSON path. Historically valid
// stored values round-trip unchanged and are intentionally not subject to the
// v2 WorkspaceConfig URL and credential CEL policy; the shared schema keeps
// only protocol-neutral validation, and protocol-specific rules are enforced
// at binding resolution. These fields never enter new bindings: new v1
// workspaces are credential-free and public-read-only by policy.
type LegacyAgentWorkspaceConfig struct {
	// GitRepo is the repository URL to clone.
	// +optional
	GitRepo string `json:"gitRepo,omitempty"`

	// Branch is the git branch to checkout.
	// +optional
	Branch string `json:"branch,omitempty"`

	// Ref is a specific git ref (commit SHA, tag) to checkout.
	// +optional
	Ref string `json:"ref,omitempty"`

	// GitSecretRef references a Secret containing git credentials. Adopted
	// legacy bindings freeze the exact Secret identity; new bindings reject it.
	// +optional
	GitSecretRef *corev1.LocalObjectReference `json:"gitSecretRef,omitempty"`

	// SubPath is a subdirectory within the repo to use as workspace root.
	// +optional
	SubPath string `json:"subPath,omitempty"`

	// ForkRepo is the writable fork repository URL for pushing changes.
	// +optional
	ForkRepo string `json:"forkRepo,omitempty"`

	// PRBaseBranch is the upstream branch to target for pull requests.
	// +optional
	PRBaseBranch string `json:"prBaseBranch,omitempty"`

	// PushBranch is the remote branch name to push changes to after the agent
	// completes.
	// +optional
	PushBranch string `json:"pushBranch,omitempty"`
}

// HarnessRuntimeStatus records the resolved harness v1 runtime and its durable
// attempt projection. Harness v1 cannot write the v2-only execution/delivery
// surfaces, so terminal ambiguity is represented here without weakening route
// exclusivity.
// +kubebuilder:validation:XValidation:rule="!has(self.state) || !(self.state in ['Succeeded', 'Failed', 'Cancelled', 'OutcomeUnknown']) || has(self.outcome)",message="terminal harness state requires an outcome"
// +kubebuilder:validation:XValidation:rule="!has(self.outcome) || (has(self.state) && self.state in ['Succeeded', 'Failed', 'Cancelled', 'OutcomeUnknown'])",message="harness outcome requires a terminal state"
// +kubebuilder:validation:XValidation:rule="!has(self.state) || self.state != 'OutcomeUnknown' || (has(self.outcome) && self.outcome == 'OutcomeUnknown')",message="OutcomeUnknown harness state requires OutcomeUnknown outcome"
type HarnessRuntimeStatus struct {
	// RuntimeRefName is the AgentRuntime name for custom runtimeRef turns.
	// Empty means built-in CLI wrapper.
	// +optional
	RuntimeRefName string `json:"runtimeRefName,omitempty"`

	// RuntimeName is the runtime name advertised by the harness capabilities
	// and sent in turn metadata.
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

	// AuthRefResourceVersion is the auth Secret resourceVersion validated
	// before starting the turn.
	// +optional
	AuthRefResourceVersion string `json:"authRefResourceVersion,omitempty"`

	// Attempt is the durable harness v1 attempt number.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Attempt int32 `json:"attempt,omitempty"`

	// TurnID is the deterministic, non-secret harness turn identity.
	// +optional
	TurnID string `json:"turnID,omitempty"`

	// RuntimeSessionID is the deterministic, non-secret v1 runtime-session identity.
	// +optional
	RuntimeSessionID string `json:"runtimeSessionID,omitempty"`

	// State is the durable harness v1 attempt state projected for operators.
	// +optional
	State TaskExecutionState `json:"state,omitempty"`

	// Outcome is set only for a terminal harness v1 attempt.
	// +optional
	Outcome TaskExecutionOutcome `json:"outcome,omitempty"`

	// Reason is a bounded machine-readable terminal reason code.
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Reason string `json:"reason,omitempty"`

	// TerminalReceiptDigest identifies the authoritative terminal or unknown receipt.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +optional
	TerminalReceiptDigest string `json:"terminalReceiptDigest,omitempty"`

	// RequestDigest binds the canonical StartTurn request admitted by the
	// durable wrapper ledger.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +optional
	RequestDigest string `json:"requestDigest,omitempty"`

	// ControllerEpoch records the fenced controller epoch driving the attempt.
	// +kubebuilder:validation:Minimum=0
	// +optional
	ControllerEpoch int64 `json:"controllerEpoch,omitempty"`

	// LastEventSeq is the highest durably mapped harness frame sequence.
	// +kubebuilder:validation:Minimum=0
	// +optional
	LastEventSeq int64 `json:"lastEventSeq,omitempty"`

	// CancelRequestedAt records a durable cancellation request. Cancellation
	// remains nonterminal until a terminal frame or ledger receipt is observed.
	// +optional
	CancelRequestedAt *metav1.Time `json:"cancelRequestedAt,omitempty"`

	// Message is bounded, sanitized execution context.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Message string `json:"message,omitempty"`

	// LastTransitionTime is the last durable v1 attempt transition projected to
	// the Task.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}

// WorkspaceConfig defines repository workspace, validation, and publication intent.
// +kubebuilder:validation:XValidation:rule="!self.createPR || self.intent == 'write'",message="createPR requires write workspace intent"
// +kubebuilder:validation:XValidation:rule="!has(self.gitRepo) || (!self.gitRepo.matches('(?i)^[A-Za-z][A-Za-z0-9+.-]*://[^/]*@') && !self.gitRepo.contains('?') && !self.gitRepo.contains('#'))",message="gitRepo must not contain embedded credentials, query parameters, or fragments"
// +kubebuilder:validation:XValidation:rule="!has(self.publicationGitRepo) || (!self.publicationGitRepo.matches('(?i)^[A-Za-z][A-Za-z0-9+.-]*://[^/]*@') && !self.publicationGitRepo.contains('?') && !self.publicationGitRepo.contains('#'))",message="publicationGitRepo must not contain embedded credentials, query parameters, or fragments"
type WorkspaceConfig struct {
	// Intent declares whether the verified workspace must remain unchanged or may
	// produce a publication artifact. It is immutable for the lifetime of the Task.
	// Agent Tasks that omit intent are interpreted as read by controller logic;
	// omitted intent preserves the existing behavior of container Tasks.
	// +optional
	Intent WorkspaceIntent `json:"intent,omitempty"`

	// GitRepo is the source repository URL cloned by the clean-room workspace boundary.
	// Credentials must not be embedded in the URL.
	// +kubebuilder:validation:MaxLength=2048
	// +optional
	GitRepo string `json:"gitRepo,omitempty"`

	// SourceRepository is the optional URL-derived identity for GitRepo. When set,
	// it must match the normalized credential-free URL; for GitHub, use provider
	// "github" and ID "github.com/owner/repo".
	// +optional
	SourceRepository *RepositoryIdentity `json:"sourceRepository,omitempty"`

	// Branch is the source branch to check out.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	Branch string `json:"branch,omitempty"`

	// Ref is a specific source git ref, commit SHA, or tag to check out.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Ref string `json:"ref,omitempty"`

	// ReadCredentialRef references the one-operation clone/read credential Secret.
	// The Secret is resolved only by the clean-room workspace boundary.
	// +optional
	ReadCredentialRef *WorkspaceCredentialReference `json:"readCredentialRef,omitempty"`

	// PublicationGitRepo is the repository URL whose branch receives an exact CAS publication.
	// Credentials must not be embedded in the URL.
	// +kubebuilder:validation:MaxLength=2048
	// +optional
	PublicationGitRepo string `json:"publicationGitRepo,omitempty"`

	// PublicationRepository is the optional URL-derived identity for
	// PublicationGitRepo. When set, it must match the normalized credential-free
	// URL; for GitHub, use provider "github" and ID "github.com/owner/repo".
	// +optional
	PublicationRepository *RepositoryIdentity `json:"publicationRepository,omitempty"`

	// PublicationReadCredentialRef references the target-repository read
	// credential used only for preflight and independent post-push verification.
	// +optional
	PublicationReadCredentialRef *WorkspaceCredentialReference `json:"publicationReadCredentialRef,omitempty"`

	// PublicationCredentialRef references the target-repository write credential
	// used only for the exact CAS push. It is never used to clone the source.
	// +optional
	PublicationCredentialRef *WorkspaceCredentialReference `json:"publicationCredentialRef,omitempty"`

	// ForgeCredentialRef references the forge API credential used only for pull
	// request reconciliation when createPR=true.
	// +optional
	ForgeCredentialRef *WorkspaceCredentialReference `json:"forgeCredentialRef,omitempty"`

	// SubPath is a subdirectory within the source repository used as workspace root.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	SubPath string `json:"subPath,omitempty"`

	// PRBaseBranch is the upstream branch targeted when CreatePR is true.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	PRBaseBranch string `json:"prBaseBranch,omitempty"`

	// PushBranch is the publication branch. For write Tasks the controller derives
	// a full-entropy Task- or Session-owned branch when this is omitted.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	PushBranch string `json:"pushBranch,omitempty"`

	// ExpectedRemoteSHA requires the publication branch to exist at this exact
	// commit before publication. Empty means the branch must be absent. It is
	// supported only for agent Tasks using the trusted ACP publisher boundary.
	// +kubebuilder:validation:Pattern=`^([a-f0-9]{40}|[a-f0-9]{64})$`
	// +optional
	ExpectedRemoteSHA string `json:"expectedRemoteSHA,omitempty"`

	// MaxChangedFiles bounds the total changed, deleted, and symlink paths accepted
	// from the trusted supervisor before publication. Zero uses the runtime limit.
	// It is not supported for container Tasks.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxChangedFiles *int32 `json:"maxChangedFiles,omitempty"`

	// AllowedPaths restricts publishable workspace changes to these path globs or
	// directory prefixes ending in /**. Empty allows every otherwise-safe path.
	// It is not supported for container Tasks.
	// +kubebuilder:validation:MaxItems=256
	// +optional
	AllowedPaths []string `json:"allowedPaths,omitempty"`

	// DenyRepositoryControlPaths rejects workflow, RBAC, and chart-secret paths
	// before publication even when AllowedPaths is empty or otherwise matches.
	// It is not supported for container Tasks.
	// +optional
	DenyRepositoryControlPaths bool `json:"denyRepositoryControlPaths,omitempty"`

	// RejectBinaryFiles rejects changed file content that is not valid text. It is
	// not supported for container Tasks.
	// +optional
	RejectBinaryFiles bool `json:"rejectBinaryFiles,omitempty"`

	// RejectSecretLikeContent applies Orka's generic secret detector to changed
	// paths and file contents before publication. It is not supported for container Tasks.
	// +optional
	RejectSecretLikeContent bool `json:"rejectSecretLikeContent,omitempty"`

	// CreatePR explicitly requests pull request reconciliation after branch publication.
	// Branch push remains the minimum durable delivery when false. It is supported only
	// for agent Tasks using the trusted ACP publisher boundary.
	// +kubebuilder:default=false
	// +optional
	CreatePR bool `json:"createPR,omitempty"`
}

func init() {
	SchemeBuilder.Register(&Task{}, &TaskList{})
}
