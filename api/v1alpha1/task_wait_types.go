/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// TaskWaitKind names the external condition a parked agent turn waits on.
// +kubebuilder:validation:Enum=Approval;Tasks;Timer
type TaskWaitKind string

const (
	// TaskWaitKindApproval waits for a human decision on a parked sign-off
	// approval that the turn requested. It never authorizes a gated tool call.
	TaskWaitKindApproval TaskWaitKind = "Approval"
	// TaskWaitKindTasks waits until every referenced child Task, following
	// its own continuation chain, reaches a terminal phase.
	TaskWaitKindTasks TaskWaitKind = "Tasks"
	// TaskWaitKindTimer waits until a fixed time, for rate-limit backoff.
	TaskWaitKindTimer TaskWaitKind = "Timer"
)

// TaskWaitState is the durable lifecycle of a parked turn.
// +kubebuilder:validation:Enum=Requested;Waiting;Resuming;Resumed;Expired;Cancelled;Abandoned
type TaskWaitState string

const (
	// TaskWaitStateRequested records a wait the running turn asked for. It
	// takes effect only after that exact attempt settles Succeeded.
	TaskWaitStateRequested TaskWaitState = "Requested"
	// TaskWaitStateWaiting means the turn ended at an idle boundary, its
	// runtime capacity was released, and the awaited event has not arrived.
	TaskWaitStateWaiting TaskWaitState = "Waiting"
	// TaskWaitStateResuming durably reserves the continuation Task name
	// before it is created, so creation is exactly-once across restarts.
	TaskWaitStateResuming TaskWaitState = "Resuming"
	// TaskWaitStateResumed means the continuation Task exists.
	TaskWaitStateResumed TaskWaitState = "Resumed"
	// TaskWaitStateExpired means the wait deadline passed first.
	TaskWaitStateExpired TaskWaitState = "Expired"
	// TaskWaitStateCancelled means cancellation was requested while waiting.
	TaskWaitStateCancelled TaskWaitState = "Cancelled"
	// TaskWaitStateAbandoned means the requesting turn did not end in a
	// proven success, so the wait was never armed and nothing resumes.
	TaskWaitStateAbandoned TaskWaitState = "Abandoned"
)

// IsTerminal reports whether no further wait transition can occur.
func (s TaskWaitState) IsTerminal() bool {
	switch s {
	case TaskWaitStateResumed, TaskWaitStateExpired, TaskWaitStateCancelled, TaskWaitStateAbandoned:
		return true
	default:
		return false
	}
}

const (
	// TaskConditionWaiting is True while a parked turn waits for an external
	// event with its runtime capacity released.
	TaskConditionWaiting = "Waiting"
	// TaskConditionWaitingForCapacity is True while admission is blocked only
	// because runtime or provider capacity is exhausted. It is retryable and
	// bounded by the Task deadline.
	TaskConditionWaitingForCapacity = "WaitingForCapacity"

	// TaskWaitingForCapacityReasonRuntimePoolAtCapacity reports a full RuntimePool.
	TaskWaitingForCapacityReasonRuntimePoolAtCapacity = "RuntimePoolAtCapacity"
	// TaskWaitingForCapacityReasonWorkspaceCapacityUnavailable reports an
	// execution-workspace pool or provider with no admission capacity.
	TaskWaitingForCapacityReasonWorkspaceCapacityUnavailable = "WorkspaceCapacityUnavailable"
	// TaskWaitingForCapacityReasonCapacityAvailable clears the condition.
	TaskWaitingForCapacityReasonCapacityAvailable = "CapacityAvailable"
)

// MaxTaskWaitReferences bounds the references a single wait may carry.
const MaxTaskWaitReferences = 16

// TaskWaitStatus records a data-only park of an agent turn. The parked Task's
// turn has ended; nothing of the runtime process is preserved. A continuation
// Task on the same Session resumes the work from the Session transcript.
type TaskWaitStatus struct {
	// State is the current wait lifecycle state.
	State TaskWaitState `json:"state"`

	// Kind is the awaited condition.
	Kind TaskWaitKind `json:"kind"`

	// References identifies what is awaited: the approval ID for Approval and
	// child Task names for Tasks. Timer waits carry none.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=253
	// +optional
	References []string `json:"references,omitempty"`

	// ReferenceUIDs holds the UID of each child Task in References, in the
	// same order, recorded when the wait was authorized. A Tasks wait follows
	// only those exact incarnations, never a same-named replacement.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=253
	// +optional
	ReferenceUIDs []string `json:"referenceUIDs,omitempty"`

	// Reason is the agent's bounded, sanitized explanation of the wait.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Reason string `json:"reason,omitempty"`

	// Attempt and PromptID identify the exact turn that requested the wait.
	// The wait is armed only when that turn settled Succeeded.
	// +kubebuilder:validation:Minimum=1
	Attempt int32 `json:"attempt"`
	// +kubebuilder:validation:MaxLength=253
	PromptID string `json:"promptID"`

	// OperationDigest binds the brokered tool call that requested the wait, so
	// an exact replay is idempotent and a different call is rejected.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +optional
	OperationDigest string `json:"operationDigest,omitempty"`

	// RequestedAt is when the turn requested the wait.
	RequestedAt metav1.Time `json:"requestedAt"`

	// WaitingSince is when the wait was armed and capacity released.
	// +optional
	WaitingSince *metav1.Time `json:"waitingSince,omitempty"`

	// Deadline bounds the wait. It never exceeds the deadline of the chain's
	// first Task.
	Deadline metav1.Time `json:"deadline"`

	// ResumeAt is the wake time for Timer waits.
	// +optional
	ResumeAt *metav1.Time `json:"resumeAt,omitempty"`

	// ChainIndex is the number of continuations before this Task in its chain.
	// +kubebuilder:validation:Minimum=0
	// +optional
	ChainIndex int32 `json:"chainIndex,omitempty"`

	// ContinuationTaskName is the deterministic name reserved for the
	// continuation Task before it is created.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	ContinuationTaskName string `json:"continuationTaskName,omitempty"`

	// ContinuationTaskUID is the UID of the created continuation Task.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	ContinuationTaskUID string `json:"continuationTaskUID,omitempty"`

	// Message is a bounded, sanitized description of the current state.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Message string `json:"message,omitempty"`

	// LastTransitionTime is the last state transition time.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}
