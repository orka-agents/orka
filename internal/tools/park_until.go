/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/redact"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/taskwait"
)

// ParkUntilToolName is the brokered tool an ACP agent calls to end its turn
// and release runtime capacity until an external event arrives.
const ParkUntilToolName = "park_until"

const (
	parkKindApproval = "approval"
	parkKindTasks    = "tasks"
	parkKindTimer    = "timer"

	// maxParkTimerDuration bounds a backoff timer. The Task deadline bounds it too.
	maxParkTimerDuration       = 24 * time.Hour
	parkStatusConflictAttempts = 4
)

// ParkUntilTool records a data-only park for the authenticated ACP turn. The
// turn ends normally; once it settles Succeeded the controller releases its
// runtime capacity and later starts a continuation Task on the same Session.
// No process memory is kept, and the parked prompt is never sent again.
type ParkUntilTool struct{}

// NewParkUntilTool creates the brokered park_until tool.
func NewParkUntilTool() *ParkUntilTool { return &ParkUntilTool{} }

// Name returns the tool name.
func (t *ParkUntilTool) Name() string { return ParkUntilToolName }

// Description returns the tool description.
func (t *ParkUntilTool) Description() string {
	return "Park this turn until an external event arrives, releasing your runtime while you wait. " +
		"After calling this, finish your turn immediately with a short status message and make no further tool calls. " +
		"Orka then starts a new turn in this same Session when the event arrives (an approval decision, the named child tasks finishing, " +
		"or a backoff timer), with a message describing what happened. Your conversation history is kept; running processes and " +
		"uncommitted in-memory state are not. Approval here is a sign-off checkpoint only: approval-required tools still need their own approval."
}

// Parameters returns the JSON schema.
func (t *ParkUntilTool) Parameters() json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
		"type": "object",
		"properties": {
			"kind": {"type": "string", "enum": ["approval", "tasks", "timer"], "description": "What to wait for"},
			"reason": {"type": "string", "maxLength": %d, "description": "Why you are waiting. For kind=approval this is the question the reviewer sees."},
			"tasks": {"type": "array", "items": {"type": "string"}, "maxItems": %d, "description": "kind=tasks: names of your child tasks to wait for"},
			"duration": {"type": "string", "description": "kind=timer: how long to back off, e.g. '10m' (max 24h)"},
			"timeout": {"type": "string", "description": "Optional maximum wait, e.g. '2h'. The Task deadline always applies."}
		},
		"required": ["kind", "reason"]
	}`, taskwait.MaxReasonLength, corev1alpha1.MaxTaskWaitReferences))
}

type parkUntilArgs struct {
	Kind     string   `json:"kind"`
	Reason   string   `json:"reason"`
	Tasks    []string `json:"tasks,omitempty"`
	Duration string   `json:"duration,omitempty"`
	Timeout  string   `json:"timeout,omitempty"`
}

// ParkUntilResult is the tool response.
type ParkUntilResult struct {
	Status      string `json:"status"`
	Kind        string `json:"kind"`
	ApprovalID  string `json:"approvalID,omitempty"`
	Deadline    string `json:"deadline"`
	ResumeAt    string `json:"resumeAt,omitempty"`
	Instruction string `json:"instruction"`
}

// Execute records the wait on the authenticated Task.
//
//nolint:gocyclo // Argument, identity, eligibility, and per-kind validation fail closed in one place.
func (t *ParkUntilTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	toolCtx := GetToolContext(ctx)
	if toolCtx == nil || !toolCtx.Brokered {
		return "", fmt.Errorf("%s is available only to ACP agent runtimes through the Orka broker", ParkUntilToolName)
	}
	if toolCtx.Client == nil || toolCtx.PolicyReader == nil {
		return "", fmt.Errorf("%s requires the authenticated broker context", ParkUntilToolName)
	}
	namespace := strings.TrimSpace(toolCtx.Namespace)
	taskName := strings.TrimSpace(toolCtx.TaskID)
	taskUID := strings.TrimSpace(toolCtx.TaskUID)
	operationID := strings.TrimSpace(toolCtx.OperationID)
	if namespace == "" || taskName == "" || taskUID == "" || operationID == "" {
		return "", fmt.Errorf("%s requires authenticated task and operation identity", ParkUntilToolName)
	}

	var args parkUntilArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	kind, err := parseParkKind(args.Kind)
	if err != nil {
		return "", err
	}
	reason := strings.TrimSpace(redact.SensitiveText(args.Reason))
	if reason == "" {
		return "", fmt.Errorf("reason is required")
	}
	if len(reason) > taskwait.MaxReasonLength {
		return "", fmt.Errorf("reason must be at most %d characters", taskwait.MaxReasonLength)
	}

	task := &corev1alpha1.Task{}
	if err := toolCtx.PolicyReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: taskName}, task); err != nil {
		return "", fmt.Errorf("load authenticated task: %w", err)
	}
	if string(task.UID) != taskUID {
		return "", fmt.Errorf("authenticated task identity no longer matches the current Task")
	}
	if err := taskwait.Eligibility(task); err != nil {
		return "", err
	}
	execution := task.Status.Execution
	if execution == nil || execution.Attempt < 1 || strings.TrimSpace(execution.PromptID) == "" ||
		(execution.State != corev1alpha1.TaskExecutionStateAccepted && execution.State != corev1alpha1.TaskExecutionStateRunning) {
		return "", fmt.Errorf("%s can only be called from the Task's running turn", ParkUntilToolName)
	}
	if strings.TrimSpace(execution.AgentRuntimeName) != "" {
		return "", fmt.Errorf("%s is available only to built-in ACP runtimes", ParkUntilToolName)
	}
	if !task.DeletionTimestamp.IsZero() {
		return "", fmt.Errorf("the Task is being deleted")
	}

	now := time.Now().UTC()
	deadline := taskwait.Deadline(task)
	if strings.TrimSpace(args.Timeout) != "" {
		timeout, parseErr := time.ParseDuration(strings.TrimSpace(args.Timeout))
		if parseErr != nil || timeout <= 0 {
			return "", fmt.Errorf("timeout must be a positive duration such as '2h'")
		}
		if requested := now.Add(timeout); requested.Before(deadline) {
			deadline = requested
		}
	}
	if deadline.Sub(now) < 2*taskwait.MinContinuationBudget {
		return "", fmt.Errorf("too little time remains before the Task deadline to park; finish the work in this turn instead")
	}

	references, resumeAt, err := t.kindInputs(ctx, toolCtx, task, kind, args, now, deadline)
	if err != nil {
		return "", err
	}

	digest := store.CanonicalBytesDigest(fmt.Appendf(nil, "park_until/v1\x00%s\x00%s\x00%d\x00%s\x00%s",
		namespace, taskUID, execution.Attempt, execution.PromptID, operationID))
	if kind == corev1alpha1.TaskWaitKindApproval {
		references = []string{taskwait.ParkedApprovalID(namespace, taskUID, execution.Attempt, execution.PromptID, digest)}
	}
	wait := &corev1alpha1.TaskWaitStatus{
		State: corev1alpha1.TaskWaitStateRequested, Kind: kind, References: references, Reason: reason,
		Attempt: execution.Attempt, PromptID: execution.PromptID, OperationDigest: digest,
		RequestedAt: metav1.NewTime(now), Deadline: metav1.NewTime(deadline.Truncate(time.Second)),
		ChainIndex: taskwait.ChainIndex(task), Message: "waiting for this turn to finish",
	}
	if resumeAt != nil {
		value := metav1.NewTime(resumeAt.Truncate(time.Second))
		wait.ResumeAt = &value
	}
	wait.LastTransitionTime = &wait.RequestedAt

	recorded, err := recordParkRequest(ctx, toolCtx, task, wait)
	if err != nil {
		return "", err
	}
	if recorded.Kind == corev1alpha1.TaskWaitKindApproval {
		if err := taskwait.EnsureParkedApprovalRequested(ctx, toolCtx.ExecutionEvents, task, recorded); err != nil {
			return "", fmt.Errorf("record parked approval request: %w", err)
		}
	}
	return parkUntilResponse(recorded)
}

func parseParkKind(raw string) (corev1alpha1.TaskWaitKind, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case parkKindApproval:
		return corev1alpha1.TaskWaitKindApproval, nil
	case parkKindTasks:
		return corev1alpha1.TaskWaitKindTasks, nil
	case parkKindTimer:
		return corev1alpha1.TaskWaitKindTimer, nil
	default:
		return "", fmt.Errorf("kind must be one of approval, tasks, or timer")
	}
}

func (t *ParkUntilTool) kindInputs(
	ctx context.Context,
	toolCtx *ToolContext,
	task *corev1alpha1.Task,
	kind corev1alpha1.TaskWaitKind,
	args parkUntilArgs,
	now, deadline time.Time,
) ([]string, *time.Time, error) {
	switch kind {
	// Fields that belong to another kind are ignored.
	case corev1alpha1.TaskWaitKindApproval:
		if toolCtx.ExecutionEvents == nil {
			return nil, nil, fmt.Errorf("parked approvals require execution event storage")
		}
		return nil, nil, nil
	case corev1alpha1.TaskWaitKindTimer:
		duration, err := time.ParseDuration(strings.TrimSpace(args.Duration))
		if err != nil || duration < time.Second || duration > maxParkTimerDuration {
			return nil, nil, fmt.Errorf("kind=timer requires duration between 1s and %s", maxParkTimerDuration)
		}
		resumeAt := now.Add(duration)
		if !resumeAt.Add(taskwait.MinContinuationBudget).Before(deadline) {
			return nil, nil, fmt.Errorf("the timer would end too close to the Task deadline; finish the work in this turn instead")
		}
		return nil, &resumeAt, nil
	case corev1alpha1.TaskWaitKindTasks:
		names := make([]string, 0, len(args.Tasks))
		for _, name := range args.Tasks {
			name = strings.TrimSpace(name)
			if name == "" || slices.Contains(names, name) {
				return nil, nil, fmt.Errorf("tasks must be distinct, non-empty child task names")
			}
			names = append(names, name)
		}
		if len(names) == 0 || len(names) > corev1alpha1.MaxTaskWaitReferences {
			return nil, nil, fmt.Errorf("kind=tasks requires between 1 and %d child task names", corev1alpha1.MaxTaskWaitReferences)
		}
		for _, name := range names {
			child := &corev1alpha1.Task{}
			if err := toolCtx.PolicyReader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, child); err != nil {
				if apierrors.IsNotFound(err) {
					return nil, nil, fmt.Errorf("task %q is not an authorized child of this task", name)
				}
				return nil, nil, fmt.Errorf("load child task %q: %w", name, err)
			}
			if err := validateBrokeredWaitTarget(ctx, toolCtx, task, child); err != nil {
				return nil, nil, fmt.Errorf("task %q: %w", name, err)
			}
		}
		return names, nil, nil
	default:
		return nil, nil, fmt.Errorf("unsupported wait kind %q", kind)
	}
}

// recordParkRequest stores the wait on the exact turn. An exact replay of the
// same tool call returns the recorded wait; a second, different request in
// the same turn is rejected.
func recordParkRequest(
	ctx context.Context,
	toolCtx *ToolContext,
	task *corev1alpha1.Task,
	wait *corev1alpha1.TaskWaitStatus,
) (*corev1alpha1.TaskWaitStatus, error) {
	current := task
	for attempt := range parkStatusConflictAttempts {
		if attempt > 0 {
			current = &corev1alpha1.Task{}
			if err := toolCtx.PolicyReader.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
				return nil, fmt.Errorf("reload task: %w", err)
			}
			if current.UID != task.UID {
				return nil, fmt.Errorf("authenticated task identity no longer matches the current Task")
			}
			execution := current.Status.Execution
			if execution == nil || execution.Attempt != wait.Attempt || execution.PromptID != wait.PromptID {
				return nil, fmt.Errorf("the turn that requested the wait is no longer active")
			}
		}
		if existing := current.Status.Wait; existing != nil {
			if existing.OperationDigest == wait.OperationDigest {
				return existing.DeepCopy(), nil
			}
			if taskwait.Active(existing) {
				return nil, fmt.Errorf("this task already has an active wait; finish your turn")
			}
		}
		base := current.DeepCopy()
		current.Status.Wait = wait.DeepCopy()
		err := toolCtx.Client.Status().Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		if err == nil {
			return wait, nil
		}
		if !apierrors.IsConflict(err) {
			return nil, fmt.Errorf("record wait: %w", err)
		}
	}
	return nil, errors.New("the task kept changing while the wait was recorded; retry")
}

func parkUntilResponse(wait *corev1alpha1.TaskWaitStatus) (string, error) {
	result := ParkUntilResult{
		Status:   "parked",
		Kind:     strings.ToLower(string(wait.Kind)),
		Deadline: wait.Deadline.UTC().Format(time.RFC3339),
		Instruction: "End your turn now with a brief status message. Do not call more tools. " +
			"Orka releases your runtime and starts a new turn in this Session when the event arrives or the wait deadline passes.",
	}
	if wait.Kind == corev1alpha1.TaskWaitKindApproval && len(wait.References) == 1 {
		result.ApprovalID = wait.References[0]
	}
	if wait.ResumeAt != nil {
		result.ResumeAt = wait.ResumeAt.UTC().Format(time.RFC3339)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
