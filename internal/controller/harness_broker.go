package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/google/jsonschema-go/jsonschema"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/store"
	toolspkg "github.com/orka-agents/orka/internal/tools"
	worker "github.com/orka-agents/orka/internal/worker"
	"github.com/orka-agents/orka/internal/workerenv"
	corev1 "k8s.io/api/core/v1"
)

const (
	harnessWrapperBrokeredToolTimeout = 30 * time.Second
	harnessBrokeredToolDelegateTask   = "delegate_task"
	harnessBrokeredToolWaitForTasks   = "wait_for_tasks"
	harnessBrokeredToolCancelTask     = "cancel_task"
	harnessBrokeredToolSendMessage    = "send_message"
	harnessBrokeredToolCheckMessages  = "check_messages"
)

var (
	errHarnessBrokeredApprovalPending = errors.New("brokered tool call awaiting approval")
	harnessBrokeredCoordinationEnvMu  = sync.Mutex{}
)

type harnessBrokeredApprovalPendingError struct {
	approvalID string
	toolName   string
}

func (e harnessBrokeredApprovalPendingError) Error() string {
	if strings.TrimSpace(e.approvalID) == "" {
		return errHarnessBrokeredApprovalPending.Error()
	}
	return fmt.Sprintf("brokered tool call awaiting approval %s", e.approvalID)
}

func (e harnessBrokeredApprovalPendingError) Is(target error) bool {
	return target == errHarnessBrokeredApprovalPending
}

func harnessBrokeredPendingApproval(err error) (string, string, bool) {
	var pending harnessBrokeredApprovalPendingError
	if errors.As(err, &pending) {
		return strings.TrimSpace(pending.approvalID), strings.TrimSpace(pending.toolName), true
	}
	return "", "", false
}

func harnessWrapperBrokeredToolNames(task *corev1alpha1.Task) []string {
	if task != nil && task.Spec.AgentRuntime != nil && len(task.Spec.AgentRuntime.AllowedTools) > 0 {
		return normalizeToolNames(task.Spec.AgentRuntime.AllowedTools)
	}
	return nil
}

func normalizeToolNames(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func harnessWrapperToolExecutionMode(task *corev1alpha1.Task, agent *corev1alpha1.Agent) harness.ToolExecutionMode {
	runtimeRefName := agentHarnessRuntimeRefName(agent)
	if runtimeRefName == "" && task != nil && task.Status.HarnessRuntime != nil {
		runtimeRefName = strings.TrimSpace(task.Status.HarnessRuntime.RuntimeRefName)
	}
	if runtimeRefName == "" && task != nil && task.Annotations != nil {
		runtimeRefName = strings.TrimSpace(task.Annotations[harnessWrapperRuntimeRefAnno])
	}
	if runtimeRefName != "" && len(harnessWrapperBrokeredToolNames(task)) > 0 {
		return harness.ToolExecutionModeBrokered
	}
	return harness.ToolExecutionModeObserved
}

func isHarnessBrokeredCoordinationToolName(name string) bool {
	switch strings.TrimSpace(name) {
	case harnessBrokeredToolDelegateTask, harnessBrokeredToolWaitForTasks, harnessBrokeredToolCancelTask, harnessBrokeredToolSendMessage, harnessBrokeredToolCheckMessages:
		return true
	default:
		return false
	}
}

func (r *TaskReconciler) harnessBrokeredCoordinationTool(name string) toolspkg.Tool {
	switch strings.TrimSpace(name) {
	case harnessBrokeredToolDelegateTask:
		return toolspkg.NewDelegateTaskTool(r.Client)
	case harnessBrokeredToolWaitForTasks:
		return toolspkg.NewWaitForTasksTool(r.Client)
	case harnessBrokeredToolCancelTask:
		return toolspkg.NewCancelTaskTool(r.Client)
	case harnessBrokeredToolSendMessage:
		return toolspkg.NewSendMessageTool()
	case harnessBrokeredToolCheckMessages:
		return toolspkg.NewCheckMessagesTool()
	default:
		return nil
	}
}

func (r *TaskReconciler) executeHarnessBrokeredCoordinationTool(
	ctx context.Context,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
	frame harness.HarnessEventFrame,
	tool toolspkg.Tool,
) (string, error) {
	// Coordination tools still read some policy from the legacy worker env surface.
	// Serialize this narrow bridge so concurrent controller reconciles cannot leak
	// one task 's env into another in-process coordination call.
	harnessBrokeredCoordinationEnvMu.Lock()
	defer harnessBrokeredCoordinationEnvMu.Unlock()
	restore := setHarnessBrokeredCoordinationEnv(task, agent)
	defer restore()
	toolCtx := &toolspkg.ToolContext{
		Client:       r.Client,
		Namespace:    task.Namespace,
		Tenant:       task.Namespace,
		TaskID:       task.Name,
		TaskUID:      string(task.UID),
		ParentTaskID: harnessBrokeredParentTaskName(task),
		ToolCallID:   frame.ToolCallID,
		ResultStore:  r.ResultStore,
		MessageStore: r.MessageStore,
	}
	return tool.Execute(toolspkg.WithToolContext(ctx, toolCtx), argsOrEmptyObject(frame.Content))
}

func setHarnessBrokeredCoordinationEnv(task *corev1alpha1.Task, agent *corev1alpha1.Agent) func() {
	values := map[string]string{
		workerenv.TaskName:                  task.Name,
		workerenv.TaskNamespace:             task.Namespace,
		workerenv.ParentTask:                harnessBrokeredParentTaskName(task),
		workerenv.CoordinationAllowedAgents: "",
		workerenv.CoordinationDepth: func() string {
			if task.Annotations != nil {
				if depth := strings.TrimSpace(task.Annotations[labels.AnnotationCoordinationDepth]); depth != "" {
					return depth
				}
			}
			return "0"
		}(),
		workerenv.CoordinationMaxDepth: "3",
	}
	if agent != nil && agent.Spec.Coordination != nil {
		if agent.Spec.Coordination.MaxDepth > 0 {
			values[workerenv.CoordinationMaxDepth] = strconv.Itoa(int(agent.Spec.Coordination.MaxDepth))
		}
		if len(agent.Spec.Coordination.AllowedAgents) > 0 {
			allowed := make([]string, 0, len(agent.Spec.Coordination.AllowedAgents))
			for _, allowedAgent := range agent.Spec.Coordination.AllowedAgents {
				if strings.TrimSpace(allowedAgent.Name) != "" {
					if strings.TrimSpace(allowedAgent.Namespace) != "" {
						allowed = append(allowed, strings.TrimSpace(allowedAgent.Namespace)+"/"+strings.TrimSpace(allowedAgent.Name))
					} else {
						allowed = append(allowed, strings.TrimSpace(allowedAgent.Name))
					}
				}
			}
			values[workerenv.CoordinationAllowedAgents] = strings.Join(allowed, ",")
		}
	}
	previous := map[string]*string{}
	for key, value := range values {
		if current, ok := os.LookupEnv(key); ok {
			copyValue := current
			previous[key] = &copyValue
		} else {
			previous[key] = nil
		}
		_ = os.Setenv(key, value)
	}
	return func() {
		for key, value := range previous {
			if value == nil {
				_ = os.Unsetenv(key)
			} else {
				_ = os.Setenv(key, *value)
			}
		}
	}
}

func harnessBrokeredParentTaskName(task *corev1alpha1.Task) string {
	if task == nil {
		return ""
	}
	if parent := labels.ParentTaskName(task.Labels, task.Annotations); strings.TrimSpace(parent) != "" {
		return strings.TrimSpace(parent)
	}
	return strings.TrimSpace(task.Name)
}

func argsOrEmptyObject(args json.RawMessage) json.RawMessage {
	if len(args) == 0 {
		return json.RawMessage(`{}`)
	}
	return args
}

func (r *TaskReconciler) harnessBrokeredTransactionAuthority(ctx context.Context, task *corev1alpha1.Task) (string, []string, error) {
	if task == nil || task.Spec.Transaction == nil {
		return "", nil, nil
	}
	scopes := append([]string(nil), task.Spec.Transaction.Scopes...)
	if len(scopes) == 0 {
		scopes = strings.Fields(task.Spec.Transaction.Scope)
	}
	secretName := ""
	if task.Annotations != nil {
		secretName = strings.TrimSpace(task.Annotations[labels.AnnotationTransactionTokenSecret])
	}
	if secretName == "" {
		return "", scopes, nil
	}
	reader := ctrlclient.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, ctrlclient.ObjectKey{Namespace: task.Namespace, Name: secretName}, secret); err != nil {
		return "", nil, fmt.Errorf("read task transaction authority: %w", err)
	}
	owned := false
	for _, owner := range secret.OwnerReferences {
		if owner.Kind == "Task" && owner.Name == task.Name && owner.UID == task.UID {
			owned = true
			break
		}
	}
	if !owned {
		return "", nil, errors.New("task transaction authority Secret is not owned by the Task")
	}
	token := strings.TrimSpace(string(secret.Data["token"]))
	if token == "" {
		return "", nil, errors.New("task transaction authority Secret token is empty")
	}
	return token, scopes, nil
}

func (r *TaskReconciler) harnessBrokeredTransactionCredentialAuthority(task *corev1alpha1.Task) (bool, bool, string) {
	if task == nil || task.Spec.Transaction == nil {
		return false, false, ""
	}
	tx := task.Spec.Transaction
	requiredScopes := r.TransactionCredentialReadScopes
	if len(requiredScopes) == 0 {
		requiredScopes = []string{outboundaccess.DefaultCredentialReadScope}
	}
	allowed := false
	for _, scope := range requiredScopes {
		if toolspkg.TransactionHasScope(tx, scope) {
			allowed = true
			break
		}
	}
	constraint := ""
	if tx.Context != nil {
		constraint = strings.TrimSpace(tx.Context["secret"])
	}
	return strings.TrimSpace(tx.ID) != "", allowed, constraint
}

//nolint:gocyclo // Brokered tool handling is a compact policy/approval/idempotency state machine.
func (r *TaskReconciler) handleHarnessBrokeredToolCall(
	ctx context.Context,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
	frame harness.HarnessEventFrame,
) (harness.ToolCallResult, error) {
	toolName := strings.TrimSpace(frame.ToolName)
	toolCallID := strings.TrimSpace(frame.ToolCallID)
	frame.ToolName = toolName
	frame.ToolCallID = toolCallID
	idempotencyKey := harness.ToolRequestIdempotencyKey(frame.RuntimeSessionID, frame.TurnID, toolCallID)
	result := harness.ToolCallResult{
		Version:          harness.ProtocolVersion,
		RuntimeSessionID: frame.RuntimeSessionID,
		TurnID:           frame.TurnID,
		ToolCallID:       toolCallID,
		IdempotencyKey:   idempotencyKey,
		Approved:         true,
	}
	if toolName == "" || result.ToolCallID == "" {
		err := fmt.Errorf("brokered tool name and toolCallID are required")
		result.Error = brokeredToolError("invalid_tool_call", err)
		return result, r.recordHarnessBrokeredToolEvent(ctx, task, frame, events.ExecutionEventTypeToolCallFailed, err.Error(), brokeredToolEventContent(result, nil))
	}
	args := frame.Content
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	argsDigest, err := approvals.TargetArgsDigest(args)
	if err != nil {
		result.Error = brokeredToolError("invalid_tool_arguments", err)
		return result, r.recordHarnessBrokeredToolEvent(ctx, task, frame, events.ExecutionEventTypeToolCallFailed, err.Error(), brokeredToolEventContent(result, map[string]any{"targetArgsDigestError": true}))
	}
	if previous, ok, err := r.previousHarnessBrokeredToolResult(ctx, task, frame, idempotencyKey, argsDigest); err != nil {
		return result, err
	} else if ok {
		return previous, nil
	}
	plannedClasses, err := harnessWrapperPlannedBrokeredToolClassMap(task)
	if err != nil {
		result.Error = brokeredToolError("invalid_brokered_plan", err)
		return result, r.recordHarnessBrokeredToolEvent(ctx, task, frame, events.ExecutionEventTypeToolCallFailed, err.Error(), brokeredToolEventContent(result, nil))
	}
	frozenClass, allowed := plannedClasses[toolName]
	if !allowed && len(plannedClasses) == 0 {
		if slices.Contains(harnessWrapperBrokeredToolNames(task), toolName) {
			allowed = true
		}
	}
	if !allowed {
		err := fmt.Errorf("brokered tool %q is not allowed for this task", toolName)
		result.Error = brokeredToolError("tool_not_allowed", err)
		return result, r.recordHarnessBrokeredToolEvent(ctx, task, frame, events.ExecutionEventTypeToolCallFailed, err.Error(), brokeredToolEventContent(result, nil))
	}
	if tool := r.harnessBrokeredCoordinationTool(toolName); tool != nil {
		if frozenClass != "" && frozenClass != corev1alpha1.AgentRuntimeBrokeredToolClassCoordination {
			err := fmt.Errorf("brokered tool %q class changed from %q to %q", toolName, frozenClass, corev1alpha1.AgentRuntimeBrokeredToolClassCoordination)
			result.Error = brokeredToolError("tool_class_changed", err)
			return result, r.recordHarnessBrokeredToolEvent(ctx, task, frame, events.ExecutionEventTypeToolCallFailed, err.Error(), brokeredToolEventContent(result, nil))
		}
		if started, err := r.hasUnresolvedHarnessBrokeredToolExecution(ctx, task, frame, idempotencyKey, argsDigest); err != nil {
			return result, err
		} else if started {
			err := fmt.Errorf("brokered coordination tool %q has an unresolved prior execution ledger entry", toolName)
			result.Error = brokeredToolError("tool_execution_outcome_unknown", err)
			content := brokeredToolEventContent(result, map[string]any{
				"targetArgsDigest": argsDigest,
				"outcomeUnknown":   true,
			})
			return result, r.recordHarnessBrokeredToolEvent(ctx, task, frame, events.ExecutionEventTypeToolCallFailed, err.Error(), content)
		}
		startedContent := brokeredToolEventContent(result, map[string]any{
			"targetArgsDigest": argsDigest,
			"brokeredClass":    string(corev1alpha1.AgentRuntimeBrokeredToolClassCoordination),
			"executionState":   "started",
		})
		if err := r.recordHarnessBrokeredToolEvent(ctx, task, frame, events.ExecutionEventTypeToolCallStarted, "brokered coordination tool execution started", startedContent); err != nil {
			return result, err
		}
		execCtx, cancel := context.WithTimeout(ctx, harnessWrapperBrokeredToolTimeout)
		defer cancel()
		output, err := r.executeHarnessBrokeredCoordinationTool(execCtx, task, agent, frame, tool)
		if err != nil {
			result.Error = brokeredToolError("coordination_tool_failed", err)
			content := brokeredToolEventContent(result, map[string]any{"targetArgsDigest": argsDigest})
			return result, r.recordHarnessBrokeredToolEvent(ctx, task, frame, events.ExecutionEventTypeToolCallFailed, err.Error(), content)
		}
		result.Output = brokeredToolOutput(output)
		resultRef, err := r.saveHarnessBrokeredToolResult(ctx, task, idempotencyKey, result.Output)
		if err != nil {
			result.Error = brokeredToolError("tool_result_store_failed", err)
			result.Output = nil
			content := brokeredToolEventContent(result, map[string]any{"targetArgsDigest": argsDigest})
			return result, r.recordHarnessBrokeredToolEvent(ctx, task, frame, events.ExecutionEventTypeToolCallFailed, err.Error(), content)
		}
		content := brokeredToolEventContent(result, map[string]any{
			"toolResultRef":    resultRef,
			"targetArgsDigest": argsDigest,
			"resultLength":     len(output),
			"brokeredClass":    string(corev1alpha1.AgentRuntimeBrokeredToolClassCoordination),
		})
		return result, r.recordHarnessBrokeredToolEvent(ctx, task, frame, events.ExecutionEventTypeToolCallCompleted, "brokered coordination tool call completed", content)
	}
	tool := &corev1alpha1.Tool{}
	if err := r.Get(ctx, ctrlclient.ObjectKey{Namespace: task.Namespace, Name: toolName}, tool); err != nil {
		err = fmt.Errorf("read brokered tool %q: %w", toolName, err)
		result.Error = brokeredToolError("tool_not_found", err)
		return result, r.recordHarnessBrokeredToolEvent(ctx, task, frame, events.ExecutionEventTypeToolCallFailed, err.Error(), brokeredToolEventContent(result, nil))
	}
	if frozenClass != "" && tool.Spec.BrokeredToolClass != frozenClass {
		err := fmt.Errorf("brokered tool %q class changed from %q to %q", toolName, frozenClass, tool.Spec.BrokeredToolClass)
		result.Error = brokeredToolError("tool_class_changed", err)
		return result, r.recordHarnessBrokeredToolEvent(ctx, task, frame, events.ExecutionEventTypeToolCallFailed, err.Error(), brokeredToolEventContent(result, nil))
	}
	execIdempotencyKey := idempotencyKey
	approvalID := ""
	if err := validateBrokeredToolArguments(tool, args); err != nil {
		result.Error = brokeredToolError("invalid_tool_arguments", err)
		content := brokeredToolEventContent(result, map[string]any{"targetArgsDigest": argsDigest})
		return result, r.recordHarnessBrokeredToolEvent(ctx, task, frame, events.ExecutionEventTypeToolCallFailed, err.Error(), content)
	}
	transactionToken, transactionScopes, err := r.harnessBrokeredTransactionAuthority(ctx, task)
	if err != nil {
		result.Error = brokeredToolError("transaction_authority_unavailable", err)
		return result, r.recordHarnessBrokeredToolEvent(
			ctx, task, frame, events.ExecutionEventTypeToolCallFailed, err.Error(), brokeredToolEventContent(result, nil),
		)
	}
	credentialAuthorityEnforced, credentialScopeAllowed, credentialSecret :=
		r.harnessBrokeredTransactionCredentialAuthority(task)
	if tool.Spec.HTTP != nil && tool.Spec.HTTP.AuthSecretRef != nil {
		if err := outboundaccess.ValidateCredentialAuthority(
			credentialAuthorityEnforced,
			credentialScopeAllowed,
			credentialSecret,
			[]string{tool.Spec.HTTP.AuthSecretRef.Name},
			false,
		); err != nil {
			result.Error = brokeredToolError("credential_authority_unavailable", err)
			return result, r.recordHarnessBrokeredToolEvent(
				ctx,
				task,
				frame,
				events.ExecutionEventTypeToolCallFailed,
				err.Error(),
				brokeredToolEventContent(result, nil),
			)
		}
	}
	transactionIdentity, err := r.harnessBrokeredTransactionAuthorityIdentityFromSnapshot(
		ctx, task, transactionToken, transactionScopes,
	)
	if err != nil {
		result.Error = brokeredToolError("transaction_authority_unavailable", err)
		return result, r.recordHarnessBrokeredToolEvent(
			ctx, task, frame, events.ExecutionEventTypeToolCallFailed, err.Error(), brokeredToolEventContent(result, nil),
		)
	}
	authSecretValue, authSecretIdentity, err := r.harnessBrokeredAuthSecretSnapshot(ctx, tool)
	if err != nil {
		result.Error = brokeredToolError("credential_authority_unavailable", err)
		return result, r.recordHarnessBrokeredToolEvent(
			ctx, task, frame, events.ExecutionEventTypeToolCallFailed, err.Error(), brokeredToolEventContent(result, nil),
		)
	}
	switch tool.Spec.BrokeredToolClass {
	case corev1alpha1.AgentRuntimeBrokeredToolClassRead:
		// Read-only brokered tools execute immediately below.
	case corev1alpha1.AgentRuntimeBrokeredToolClassWrite:
		approved, decisionApprovalID, err := r.ensureHarnessBrokeredWriteApproval(ctx, task, frame, tool, args, transactionIdentity, authSecretIdentity)
		if err != nil {
			if errors.Is(err, errHarnessBrokeredApprovalPending) {
				return result, err
			}
			result.Error = brokeredToolError("approval_failed", err)
			return result, r.recordHarnessBrokeredToolEvent(ctx, task, frame, events.ExecutionEventTypeToolCallFailed, err.Error(), brokeredToolEventContent(result, nil))
		}
		if !approved {
			result.Approved = false
			result.Error = &harness.ErrorInfo{Code: "approval_declined", Message: "approval declined"}
			content := brokeredToolEventContent(result, map[string]any{"approvalID": decisionApprovalID, "targetArgsDigest": argsDigest, "approved": false})
			return result, r.recordHarnessBrokeredToolEvent(ctx, task, frame, events.ExecutionEventTypeToolCallFailed, "approval declined", content)
		}
		execIdempotencyKey = decisionApprovalID
		approvalID = decisionApprovalID
	default:
		err := fmt.Errorf("brokered tool %q is not classified as read or write", toolName)
		result.Error = brokeredToolError("tool_class_not_allowed", err)
		return result, r.recordHarnessBrokeredToolEvent(ctx, task, frame, events.ExecutionEventTypeToolCallFailed, err.Error(), brokeredToolEventContent(result, nil))
	}
	if tool.Spec.BrokeredToolClass == corev1alpha1.AgentRuntimeBrokeredToolClassWrite {
		if started, err := r.hasUnresolvedHarnessBrokeredToolExecution(ctx, task, frame, idempotencyKey, argsDigest); err != nil {
			return result, err
		} else if started {
			err := fmt.Errorf("brokered write tool %q has an unresolved prior execution ledger entry", toolName)
			result.Error = brokeredToolError("tool_execution_outcome_unknown", err)
			content := brokeredToolEventContent(result, map[string]any{
				"targetArgsDigest":        argsDigest,
				"approvalID":              approvalID,
				"executionIdempotencyKey": execIdempotencyKey,
				"outcomeUnknown":          true,
			})
			return result, r.recordHarnessBrokeredToolEvent(ctx, task, frame, events.ExecutionEventTypeToolCallFailed, err.Error(), content)
		}
		content := brokeredToolEventContent(result, map[string]any{
			"targetArgsDigest":        argsDigest,
			"approvalID":              approvalID,
			"executionIdempotencyKey": execIdempotencyKey,
			"brokeredClass":           string(tool.Spec.BrokeredToolClass),
			"executionState":          "started",
		})
		if err := r.recordHarnessBrokeredToolEvent(ctx, task, frame, events.ExecutionEventTypeToolCallStarted, "brokered write tool execution started", content); err != nil {
			return result, err
		}
	}
	execCtx, cancel := context.WithTimeout(ctx, harnessWrapperBrokeredToolTimeout)
	defer cancel()
	execCtx = worker.WithToolCallID(execCtx, frame.ToolCallID)
	execCtx = worker.WithToolIdempotencyKey(execCtx, execIdempotencyKey)
	executor := worker.NewToolExecutorForNamespace(task.Namespace, r.KubeClient, nil, r.OutboundAccessResolver)
	executor.SetTransactionAuthority(transactionToken, transactionScopes)
	executor.SetTransactionCredentialAuthority(credentialAuthorityEnforced, credentialScopeAllowed, credentialSecret)
	executor.SetTransactionExchangeConfig(r.BrokeredTransactionExchange)
	if tool.Spec.HTTP != nil && tool.Spec.HTTP.AuthSecretRef != nil {
		executor.SetAuthSecretValue(tool.Spec.HTTP.AuthSecretRef.Name, tool.Spec.HTTP.AuthSecretRef.Key, authSecretValue)
	}
	output, err := executor.Execute(execCtx, tool, args)
	if err != nil {
		result.Error = brokeredToolError("tool_execution_failed", err)
		content := brokeredToolEventContent(result, map[string]any{
			"targetArgsDigest":        argsDigest,
			"approvalID":              approvalID,
			"executionIdempotencyKey": execIdempotencyKey,
			"toolRequestAttempted":    worker.ToolRequestWasAttempted(err),
		})
		return result, r.recordHarnessBrokeredToolEvent(ctx, task, frame, events.ExecutionEventTypeToolCallFailed, err.Error(), content)
	}
	result.Output = brokeredToolOutput(output)
	resultRef, err := r.saveHarnessBrokeredToolResult(ctx, task, idempotencyKey, result.Output)
	if err != nil {
		result.Error = brokeredToolError("tool_result_store_failed", err)
		result.Output = nil
		content := brokeredToolEventContent(result, map[string]any{
			"targetArgsDigest":        argsDigest,
			"approvalID":              approvalID,
			"executionIdempotencyKey": execIdempotencyKey,
		})
		return result, r.recordHarnessBrokeredToolEvent(ctx, task, frame, events.ExecutionEventTypeToolCallFailed, err.Error(), content)
	}
	content := brokeredToolEventContent(result, map[string]any{
		"toolResultRef":           resultRef,
		"targetArgsDigest":        argsDigest,
		"executionIdempotencyKey": execIdempotencyKey,
		"resultLength":            len(output),
		"brokeredClass":           string(tool.Spec.BrokeredToolClass),
		"approvalID":              approvalID,
	})
	return result, r.recordHarnessBrokeredToolEvent(ctx, task, frame, events.ExecutionEventTypeToolCallCompleted, "brokered tool call completed", content)
}

func (r *TaskReconciler) ensureHarnessBrokeredWriteApproval(
	ctx context.Context,
	task *corev1alpha1.Task,
	frame harness.HarnessEventFrame,
	tool *corev1alpha1.Tool,
	args json.RawMessage,
	transactionIdentity map[string]any,
	authSecretIdentity map[string]string,
) (bool, string, error) {
	target, err := r.harnessBrokeredApprovalTarget(
		ctx, task, frame, tool, args, transactionIdentity, authSecretIdentity,
	)
	if err != nil {
		return false, "", err
	}
	approval, found, err := r.harnessBrokeredApprovalState(ctx, task, target.ApprovalID)
	if err != nil {
		return false, target.ApprovalID, err
	}
	if !found {
		if err := r.recordHarnessBrokeredApprovalRequested(ctx, task, frame, target); err != nil {
			return false, target.ApprovalID, err
		}
		return false, target.ApprovalID, harnessBrokeredApprovalPendingError{approvalID: target.ApprovalID, toolName: tool.Name}
	}
	if approval.TargetArgsDigest != target.TargetArgsDigest || approval.TargetSpecDigest != target.TargetSpecDigest {
		return false, target.ApprovalID, fmt.Errorf("approval target changed for %s", target.TargetTool)
	}
	switch approval.Status {
	case approvals.StatusApproved:
		return true, target.ApprovalID, nil
	case approvals.StatusDeclined, approvals.StatusExpired, approvals.StatusCancelled:
		return false, target.ApprovalID, nil
	default:
		return false, target.ApprovalID, harnessBrokeredApprovalPendingError{approvalID: target.ApprovalID, toolName: tool.Name}
	}
}

func (r *TaskReconciler) harnessBrokeredApprovalTarget(
	ctx context.Context,
	task *corev1alpha1.Task,
	frame harness.HarnessEventFrame,
	tool *corev1alpha1.Tool,
	args json.RawMessage,
	transactionIdentity map[string]any,
	authSecretIdentity map[string]string,
) (approvals.ApprovalTarget, error) {
	outboundPolicyIdentity, err := r.harnessBrokeredOutboundPolicyIdentity(ctx, tool)
	if err != nil {
		return approvals.ApprovalTarget{}, err
	}
	specDigest, err := approvals.TargetSpecDigest(map[string]any{
		"toolResourceVersion":  tool.ResourceVersion,
		"toolName":             tool.Name,
		"brokeredToolClass":    string(tool.Spec.BrokeredToolClass),
		"runtimeSessionID":     string(frame.RuntimeSessionID),
		"turnID":               string(frame.TurnID),
		"toolCallID":           frame.ToolCallID,
		"transactionAuthority": transactionIdentity,
		"authSecret":           authSecretIdentity,
		"outboundPolicy":       outboundPolicyIdentity,
	})
	if err != nil {
		return approvals.ApprovalTarget{}, err
	}
	return approvals.NewApprovalTarget(
		task.Namespace,
		task.Name,
		string(task.UID),
		tool.Name,
		args,
		fmt.Sprintf("Execute brokered write tool %s", tool.Name),
		"Remote runtime requested a consequential brokered tool call; Orka must approve exact arguments before execution.",
		"warning",
		specDigest,
	)
}

func (r *TaskReconciler) brokeredApprovalReader() ctrlclient.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *TaskReconciler) harnessBrokeredTransactionAuthorityIdentity(
	ctx context.Context,
	task *corev1alpha1.Task,
) (map[string]any, error) {
	token, scopes, err := r.harnessBrokeredTransactionAuthority(ctx, task)
	if err != nil {
		return nil, err
	}
	return r.harnessBrokeredTransactionAuthorityIdentityFromSnapshot(ctx, task, token, scopes)
}

func (r *TaskReconciler) harnessBrokeredTransactionAuthorityIdentityFromSnapshot(
	ctx context.Context,
	task *corev1alpha1.Task,
	token string,
	scopes []string,
) (map[string]any, error) {
	if task == nil || task.Spec.Transaction == nil {
		return nil, nil
	}
	identity := map[string]any{
		"transactionID":          task.Spec.Transaction.ID,
		"scope":                  task.Spec.Transaction.Scope,
		"scopes":                 scopes,
		"contextDigest":          task.Spec.Transaction.ContextDigest,
		"requesterContextDigest": task.Spec.Transaction.RequesterContextDigest,
	}
	if token != "" {
		sum := sha256.Sum256([]byte(token))
		identity["tokenDigest"] = hex.EncodeToString(sum[:])
	}
	if task.Annotations != nil {
		if secretName := strings.TrimSpace(task.Annotations[labels.AnnotationTransactionTokenSecret]); secretName != "" {
			secret := &corev1.Secret{}
			if err := r.brokeredApprovalReader().Get(
				ctx,
				ctrlclient.ObjectKey{Namespace: task.Namespace, Name: secretName},
				secret,
			); err != nil {
				return nil, fmt.Errorf("resolve task transaction authority approval identity: %w", err)
			}
			identity["secretUID"] = string(secret.UID)
			identity["secretResourceVersion"] = secret.ResourceVersion
		}
	}
	if config := r.BrokeredTransactionExchange; config != nil {
		endpointDigest := ""
		if config.TTS.Endpoint != "" {
			sum := sha256.Sum256([]byte(config.TTS.Endpoint))
			endpointDigest = hex.EncodeToString(sum[:])
		}
		identity["exchange"] = map[string]any{
			"endpointDigest": endpointDigest,
			"audience":       config.TTS.Audience,
			"timeout":        config.TTS.Timeout.String(),
			"tokenSource":    config.TTS.TokenSource,
			"toolTokenTTL":   config.TTS.ToolTokenTTL.String(),
			"subjectType":    config.SubjectTokenType,
			"outboundScope":  config.OutboundScope,
		}
	}
	return identity, nil
}

func (r *TaskReconciler) harnessBrokeredAuthSecretSnapshot(
	ctx context.Context,
	tool *corev1alpha1.Tool,
) (string, map[string]string, error) {
	if tool == nil || tool.Spec.HTTP == nil || tool.Spec.HTTP.AuthSecretRef == nil {
		return "", nil, nil
	}
	secret := &corev1.Secret{}
	ref := tool.Spec.HTTP.AuthSecretRef
	key := ctrlclient.ObjectKey{Namespace: tool.Namespace, Name: ref.Name}
	if err := r.brokeredApprovalReader().Get(ctx, key, secret); err != nil {
		return "", nil, fmt.Errorf("resolve Tool auth Secret approval identity: %w", err)
	}
	value := strings.TrimSpace(string(secret.Data[ref.Key]))
	if value == "" {
		return "", nil, errors.New("tool auth Secret key is missing or empty")
	}
	return value, map[string]string{
		"uid":             string(secret.UID),
		"resourceVersion": secret.ResourceVersion,
	}, nil
}

func (r *TaskReconciler) harnessBrokeredOutboundPolicyIdentity(ctx context.Context, tool *corev1alpha1.Tool) (map[string]any, error) {
	if tool == nil || tool.Spec.HTTP == nil || tool.Spec.HTTP.OutboundAccessPolicyRef == nil {
		return nil, nil
	}
	policy := &corev1alpha1.OutboundAccessPolicy{}
	key := ctrlclient.ObjectKey{Namespace: tool.Namespace, Name: tool.Spec.HTTP.OutboundAccessPolicyRef.Name}
	if err := r.brokeredApprovalReader().Get(ctx, key, policy); err != nil {
		return nil, fmt.Errorf("resolve outbound access policy approval identity: %w", err)
	}
	secretRefs := []*corev1alpha1.NamespacedSecretKeySelector{}
	appendTLS := func(config *corev1alpha1.OutboundTLSConfig) {
		if config != nil && config.CASecretRef != nil {
			secretRefs = append(secretRefs, config.CASecretRef)
		}
	}
	if direct := policy.Spec.Direct; direct != nil {
		secretRefs = append(secretRefs, direct.Subject.SecretRef)
		if direct.Actor != nil {
			secretRefs = append(secretRefs, direct.Actor.SecretRef)
		}
		if auth := direct.ClientAuthentication; auth != nil {
			secretRefs = append(secretRefs, auth.ClientSecretRef, auth.PrivateKeyRef)
		}
		appendTLS(direct.TokenEndpoint.TLS)
	}
	if gateway := policy.Spec.Gateway; gateway != nil {
		appendTLS(gateway.TLS)
	}
	serviceAccountNames := []string{}
	if direct := policy.Spec.Direct; direct != nil {
		appendSource := func(source *corev1alpha1.OutboundTokenSource) {
			if source == nil || source.ServiceAccountRef == nil {
				return
			}
			if name := strings.TrimSpace(source.ServiceAccountRef.Name); name != "" {
				serviceAccountNames = append(serviceAccountNames, name)
			}
		}
		appendSource(&direct.Subject)
		appendSource(direct.Actor)
	}
	serviceRefs := []corev1alpha1.OutboundServiceReference{}
	if policy.Spec.Direct != nil && policy.Spec.Direct.TokenEndpoint.ServiceRef != nil {
		serviceRefs = append(serviceRefs, *policy.Spec.Direct.TokenEndpoint.ServiceRef)
	}
	if policy.Spec.Gateway != nil {
		serviceRefs = append(serviceRefs, policy.Spec.Gateway.ServiceRef)
	}
	versions := make([]string, 0, len(secretRefs)+len(serviceAccountNames)+len(serviceRefs))
	for _, name := range serviceAccountNames {
		serviceAccount := &corev1.ServiceAccount{}
		if err := r.brokeredApprovalReader().Get(
			ctx,
			ctrlclient.ObjectKey{Namespace: policy.Namespace, Name: name},
			serviceAccount,
		); err != nil {
			return nil, fmt.Errorf("resolve outbound access ServiceAccount approval identity: %w", err)
		}
		versions = append(
			versions,
			policy.Namespace+"/"+name+"\x00"+string(serviceAccount.UID)+"\x00"+serviceAccount.ResourceVersion,
		)
	}
	for _, ref := range serviceRefs {
		serviceNamespace := strings.TrimSpace(ref.Namespace)
		if serviceNamespace == "" {
			serviceNamespace = policy.Namespace
		}
		service := &corev1.Service{}
		if err := r.brokeredApprovalReader().Get(ctx, ctrlclient.ObjectKey{Namespace: serviceNamespace, Name: ref.Name}, service); err != nil {
			return nil, fmt.Errorf("resolve outbound access Service approval identity: %w", err)
		}
		versions = append(versions, serviceNamespace+"/"+ref.Name+"\x00"+string(service.UID)+"\x00"+service.ResourceVersion)
	}
	for _, ref := range secretRefs {
		if ref == nil || strings.TrimSpace(ref.Name) == "" {
			continue
		}
		namespace := strings.TrimSpace(ref.Namespace)
		if namespace == "" {
			namespace = policy.Namespace
		}
		secret := &corev1.Secret{}
		if err := r.brokeredApprovalReader().Get(
			ctx,
			ctrlclient.ObjectKey{Namespace: namespace, Name: ref.Name},
			secret,
		); err != nil {
			return nil, fmt.Errorf("resolve outbound access credential approval identity: %w", err)
		}
		versions = append(versions, namespace+"/"+ref.Name+"\x00"+string(secret.UID)+"\x00"+secret.ResourceVersion)
	}
	slices.Sort(versions)
	secretDigest := ""
	if len(versions) > 0 {
		sum := sha256.Sum256([]byte(strings.Join(versions, "\n")))
		secretDigest = hex.EncodeToString(sum[:])
	}
	return map[string]any{
		"uid":             string(policy.UID),
		"generation":      policy.Generation,
		"resourceVersion": policy.ResourceVersion,
		"secretsDigest":   secretDigest,
	}, nil
}

func (r *TaskReconciler) harnessBrokeredApprovalState(
	ctx context.Context,
	task *corev1alpha1.Task,
	approvalID string,
) (approvals.Approval, bool, error) {
	listed, err := approvals.ListEvents(ctx, r.ExecutionEventStore, task.Namespace, task.Name)
	if err != nil {
		return approvals.Approval{}, false, err
	}
	for _, approval := range approvals.Derive(approvals.FilterEventsForTaskUID(listed, string(task.UID)), time.Now().UTC()) {
		if approval.ID == approvalID {
			return approval, true, nil
		}
	}
	return approvals.Approval{}, false, nil
}

func (r *TaskReconciler) recordHarnessBrokeredApprovalRequested(
	ctx context.Context,
	task *corev1alpha1.Task,
	frame harness.HarnessEventFrame,
	target approvals.ApprovalTarget,
) error {
	content, err := json.Marshal(map[string]any{
		"approvalID":        target.ApprovalID,
		"taskUID":           target.TaskUID,
		"targetTool":        target.TargetTool,
		"targetArgsDigest":  target.TargetArgsDigest,
		"targetSpecDigest":  target.TargetSpecDigest,
		"targetArgsPreview": target.TargetArgsPreview,
		"action":            target.Action,
		"riskSummary":       target.RiskSummary,
		"severity":          target.Severity,
		"toolCallID":        frame.ToolCallID,
		"runtimeSessionID":  string(frame.RuntimeSessionID),
		"turnID":            string(frame.TurnID),
		"correlationID":     frame.CorrelationID,
	})
	if err != nil {
		return err
	}
	_, err = r.ExecutionEventStore.AppendExecutionEvent(ctx, &store.ExecutionEvent{
		Namespace:   task.Namespace,
		StreamType:  store.ExecutionEventStreamTypeTask,
		StreamID:    task.Name,
		TaskName:    task.Name,
		SessionName: r.executionEventSessionName(ctx, task),
		AgentName:   harnessWrapperTaskAgentName(task),
		Type:        events.ExecutionEventTypeApprovalRequested,
		Severity:    events.ExecutionEventSeverityWarning,
		ToolName:    target.TargetTool,
		ToolCallID:  target.ApprovalID,
		Summary:     "approval requested for brokered tool call",
		Content:     content,
	})
	return err
}

func (r *TaskReconciler) saveHarnessBrokeredToolResult(ctx context.Context, task *corev1alpha1.Task, idempotencyKey string, output json.RawMessage) (string, error) {
	if len(output) == 0 {
		return "", nil
	}
	if r == nil || r.ResultStore == nil {
		return "", fmt.Errorf("result store is required for brokered tool result replay")
	}
	ref := harnessBrokeredToolResultRef(task, idempotencyKey)
	if err := r.ResultStore.SaveResult(ctx, task.Namespace, ref, []byte(output)); err != nil {
		return "", fmt.Errorf("store brokered tool result: %w", err)
	}
	return ref, nil
}

func harnessBrokeredToolResultRef(task *corev1alpha1.Task, idempotencyKey string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(idempotencyKey)))
	return strings.TrimSpace(task.Name) + "-brokered-" + hex.EncodeToString(sum[:])[:24]
}

func (r *TaskReconciler) hasUnresolvedHarnessBrokeredToolExecution(
	ctx context.Context,
	task *corev1alpha1.Task,
	frame harness.HarnessEventFrame,
	idempotencyKey string,
	argsDigest string,
) (bool, error) {
	if r == nil || r.ExecutionEventStore == nil || task == nil {
		return false, nil
	}
	listed, err := r.ExecutionEventStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
		Namespace:  task.Namespace,
		StreamID:   task.Name,
		EventTypes: []string{events.ExecutionEventTypeToolCallStarted, events.ExecutionEventTypeToolCallCompleted, events.ExecutionEventTypeToolCallFailed},
	})
	if err != nil {
		return false, fmt.Errorf("read brokered tool ledger: %w", err)
	}
	started := false
	for _, event := range listed {
		if event.ToolCallID != frame.ToolCallID {
			continue
		}
		var payload struct {
			Brokered         bool   `json:"brokered"`
			IdempotencyKey   string `json:"idempotencyKey"`
			TargetArgsDigest string `json:"targetArgsDigest,omitempty"`
			ExecutionState   string `json:"executionState,omitempty"`
		}
		if err := json.Unmarshal(event.Content, &payload); err != nil || !payload.Brokered || payload.IdempotencyKey != idempotencyKey {
			continue
		}
		if event.ToolName != frame.ToolName {
			return true, nil
		}
		if payload.TargetArgsDigest != "" && argsDigest != "" && payload.TargetArgsDigest != argsDigest {
			if event.Type == events.ExecutionEventTypeToolCallStarted && payload.ExecutionState == "started" {
				return true, nil
			}
			continue
		}
		switch event.Type {
		case events.ExecutionEventTypeToolCallStarted:
			if payload.ExecutionState == "started" {
				started = true
			}
		case events.ExecutionEventTypeToolCallCompleted, events.ExecutionEventTypeToolCallFailed:
			started = false
		}
	}
	return started, nil
}

func (r *TaskReconciler) previousHarnessBrokeredToolResult(
	ctx context.Context,
	task *corev1alpha1.Task,
	frame harness.HarnessEventFrame,
	idempotencyKey string,
	argsDigest string,
) (harness.ToolCallResult, bool, error) {
	if r == nil || r.ExecutionEventStore == nil || task == nil {
		return harness.ToolCallResult{}, false, nil
	}
	listed, err := r.ExecutionEventStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
		Namespace:  task.Namespace,
		StreamID:   task.Name,
		EventTypes: []string{events.ExecutionEventTypeToolCallCompleted, events.ExecutionEventTypeToolCallFailed},
	})
	if err != nil {
		return harness.ToolCallResult{}, false, fmt.Errorf("read brokered tool ledger: %w", err)
	}
	for i := len(listed) - 1; i >= 0; i-- {
		event := listed[i]
		if event.ToolCallID != frame.ToolCallID {
			continue
		}
		var payload struct {
			Brokered         bool               `json:"brokered"`
			IdempotencyKey   string             `json:"idempotencyKey"`
			TargetArgsDigest string             `json:"targetArgsDigest,omitempty"`
			Approved         *bool              `json:"approved,omitempty"`
			ToolResultRef    string             `json:"toolResultRef,omitempty"`
			ToolResult       json.RawMessage    `json:"toolResult,omitempty"`
			ToolError        *harness.ErrorInfo `json:"toolError,omitempty"`
		}
		if err := json.Unmarshal(event.Content, &payload); err != nil || !payload.Brokered || payload.IdempotencyKey != idempotencyKey {
			continue
		}
		if event.ToolName != frame.ToolName {
			result := harness.ToolCallResult{
				Version:          harness.ProtocolVersion,
				RuntimeSessionID: frame.RuntimeSessionID,
				TurnID:           frame.TurnID,
				ToolCallID:       strings.TrimSpace(frame.ToolCallID),
				IdempotencyKey:   idempotencyKey,
				Approved:         false,
				Error:            &harness.ErrorInfo{Code: "tool_call_id_reused", Message: "brokered tool call id was reused for a different tool"},
			}
			return result, true, nil
		}
		if payload.TargetArgsDigest != "" && argsDigest != "" && payload.TargetArgsDigest != argsDigest {
			result := harness.ToolCallResult{
				Version:          harness.ProtocolVersion,
				RuntimeSessionID: frame.RuntimeSessionID,
				TurnID:           frame.TurnID,
				ToolCallID:       strings.TrimSpace(frame.ToolCallID),
				IdempotencyKey:   idempotencyKey,
				Approved:         false,
				Error:            &harness.ErrorInfo{Code: "tool_call_arguments_changed", Message: "brokered tool call arguments changed after a result was recorded"},
			}
			return result, true, nil
		}
		approved := true
		if payload.Approved != nil {
			approved = *payload.Approved
		}
		if len(payload.ToolResult) == 0 && payload.ToolResultRef != "" {
			if r.ResultStore == nil {
				result := harness.ToolCallResult{
					Version:          harness.ProtocolVersion,
					RuntimeSessionID: frame.RuntimeSessionID,
					TurnID:           frame.TurnID,
					ToolCallID:       strings.TrimSpace(frame.ToolCallID),
					IdempotencyKey:   idempotencyKey,
					Approved:         false,
					Error:            &harness.ErrorInfo{Code: "tool_result_replay_unavailable", Message: "brokered tool result store is unavailable"},
				}
				return result, true, nil
			}
			stored, err := r.ResultStore.GetResult(ctx, task.Namespace, payload.ToolResultRef)
			if err != nil {
				result := harness.ToolCallResult{
					Version:          harness.ProtocolVersion,
					RuntimeSessionID: frame.RuntimeSessionID,
					TurnID:           frame.TurnID,
					ToolCallID:       strings.TrimSpace(frame.ToolCallID),
					IdempotencyKey:   idempotencyKey,
					Approved:         false,
					Error:            &harness.ErrorInfo{Code: "tool_result_replay_unavailable", Message: "brokered tool result is unavailable"},
				}
				return result, true, nil
			}
			payload.ToolResult = json.RawMessage(stored)
		}
		result := harness.ToolCallResult{
			Version:          harness.ProtocolVersion,
			RuntimeSessionID: frame.RuntimeSessionID,
			TurnID:           frame.TurnID,
			ToolCallID:       strings.TrimSpace(frame.ToolCallID),
			IdempotencyKey:   idempotencyKey,
			Approved:         approved,
			Output:           payload.ToolResult,
			Error:            payload.ToolError,
		}
		if len(result.Output) == 0 && result.Error == nil {
			if event.Type == events.ExecutionEventTypeToolCallCompleted {
				result.Output = json.RawMessage(`{"success":true,"replayed":true}`)
			} else {
				continue
			}
		}
		return result, true, nil
	}
	return harness.ToolCallResult{}, false, nil
}

func brokeredToolEventContent(result harness.ToolCallResult, extra map[string]any) map[string]any {
	content := map[string]any{
		"idempotencyKey": result.IdempotencyKey,
		"approved":       result.Approved,
	}
	if len(result.Output) > 0 {
		content["toolResultLength"] = len(result.Output)
		preview := events.RedactExecutionEventText(string(result.Output))
		if len(preview) > 512 {
			preview = preview[:512] + "...[truncated]"
		}
		content["toolResultPreview"] = preview
	}
	if result.Error != nil {
		content["toolError"] = result.Error
	}
	maps.Copy(content, extra)
	return content
}

func validateBrokeredToolArguments(tool *corev1alpha1.Tool, args json.RawMessage) error {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	var decoded any
	if err := json.Unmarshal(args, &decoded); err != nil {
		return fmt.Errorf("brokered tool arguments must be valid JSON: %w", err)
	}
	if _, ok := decoded.(map[string]any); !ok {
		return fmt.Errorf("brokered tool arguments must be a JSON object")
	}
	if tool == nil || tool.Spec.Parameters == nil || len(tool.Spec.Parameters.Raw) == 0 {
		return nil
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(tool.Spec.Parameters.Raw, &schema); err != nil {
		return fmt.Errorf("brokered tool schema is invalid JSON: %w", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return fmt.Errorf("brokered tool schema is invalid: %w", err)
	}
	if err := resolved.Validate(decoded); err != nil {
		return fmt.Errorf("brokered tool arguments do not match schema: %w", err)
	}
	return nil
}

func (r *TaskReconciler) continueHarnessBrokeredToolCall(
	ctx context.Context,
	client *harness.Client,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
	frame harness.HarnessEventFrame,
) error {
	if harnessWrapperPlannedToolExecutionMode(task) != harness.ToolExecutionModeBrokered {
		return nil
	}
	result, err := r.handleHarnessBrokeredToolCall(ctx, task, agent, frame)
	if err != nil {
		if errors.Is(err, errHarnessBrokeredApprovalPending) {
			return err
		}
		return fmt.Errorf("brokered tool call failed: %w", err)
	}
	continueCtx, cancel := context.WithTimeout(ctx, harnessWrapperBrokeredToolTimeout)
	defer cancel()
	_, err = client.ContinueTurn(continueCtx, harness.ContinueTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        task.Namespace,
		TaskName:         task.Name,
		SessionName:      harnessWrapperSessionName(task),
		RuntimeSessionID: frame.RuntimeSessionID,
		TurnID:           frame.TurnID,
		CorrelationID:    frame.CorrelationID,
		ToolResults:      []harness.ToolCallResult{result},
	})
	if err != nil {
		return fmt.Errorf("continue brokered tool call %q: %w", frame.ToolCallID, err)
	}
	if err := r.clearHarnessBrokeredApprovalWaiting(ctx, task, frame.ToolName); err != nil {
		return err
	}
	return nil
}

func brokeredToolError(code string, err error) *harness.ErrorInfo {
	message := "brokered tool call failed"
	if err != nil {
		message = events.RedactExecutionEventText(err.Error())
	}
	return &harness.ErrorInfo{Code: code, Message: message}
}

func brokeredToolOutput(output string) json.RawMessage {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return json.RawMessage(`{"success":true}`)
	}
	if json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	encoded, _ := json.Marshal(map[string]any{"result": output})
	return encoded
}

func (r *TaskReconciler) recordHarnessBrokeredToolEvent(
	ctx context.Context,
	task *corev1alpha1.Task,
	frame harness.HarnessEventFrame,
	eventType string,
	summary string,
	content map[string]any,
) error {
	if r == nil || r.ExecutionEventStore == nil || task == nil {
		return nil
	}
	severity := events.ExecutionEventSeverityInfo
	if eventType == events.ExecutionEventTypeToolCallFailed {
		severity = events.ExecutionEventSeverityError
	}
	if content == nil {
		content = map[string]any{}
	}
	content["toolName"] = strings.TrimSpace(frame.ToolName)
	content["toolCallID"] = strings.TrimSpace(frame.ToolCallID)
	content["runtimeSessionID"] = string(frame.RuntimeSessionID)
	content["turnID"] = string(frame.TurnID)
	content["correlationID"] = frame.CorrelationID
	content["brokered"] = true
	encoded, err := json.Marshal(content)
	if err != nil {
		return fmt.Errorf("marshal brokered tool event content: %w", err)
	}
	_, err = r.ExecutionEventStore.AppendExecutionEvent(ctx, &store.ExecutionEvent{
		Namespace:   task.Namespace,
		StreamType:  store.ExecutionEventStreamTypeTask,
		StreamID:    task.Name,
		TaskName:    task.Name,
		SessionName: r.executionEventSessionName(ctx, task),
		AgentName:   harnessWrapperTaskAgentName(task),
		Type:        eventType,
		Severity:    severity,
		ToolName:    strings.TrimSpace(frame.ToolName),
		ToolCallID:  strings.TrimSpace(frame.ToolCallID),
		Summary:     events.RedactExecutionEventText(summary),
		Content:     encoded,
	})
	return err
}
