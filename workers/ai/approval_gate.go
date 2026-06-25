/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/sozercan/orka/internal/approvals"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/llm"
	"github.com/sozercan/orka/internal/tools"
	"github.com/sozercan/orka/internal/workerenv"
	"github.com/sozercan/orka/workers/common"
)

type approvalGate struct {
	namespace string
	taskName  string
	taskUID   string
	required  map[string]struct{}
	resolved  []approvals.ResolvedApproval
	firedKeys map[string]bool
	recorder  common.EventRecorder
}

type approvalBatchDecision struct {
	result      string
	toolResults []llm.Message
	continueLLM bool
}

func newApprovalGateFromEnv(recorder common.EventRecorder, baseToolCtx *tools.ToolContext) (*approvalGate, error) {
	coordinationEnv := workerenv.ParseCoordinationEnv(os.Getenv)
	required := toolNameSet(coordinationEnv.ApprovalRequiredTools)
	resolved, err := parseResolvedApprovals(os.Getenv(workerenv.ResolvedApprovals))
	if err != nil {
		return nil, err
	}
	namespace, taskName, taskUID := approvalScope(baseToolCtx)
	if len(required) > 0 && strings.TrimSpace(taskUID) == "" {
		return nil, fmt.Errorf("%s is required for approval-required tools", workerenv.TaskUID)
	}
	return &approvalGate{
		namespace: namespace,
		taskName:  taskName,
		taskUID:   taskUID,
		required:  required,
		resolved:  resolved,
		firedKeys: map[string]bool{},
		recorder:  recorder,
	}, nil
}

func approvalScope(baseToolCtx *tools.ToolContext) (string, string, string) {
	namespace := strings.TrimSpace(os.Getenv(workerenv.TaskNamespace))
	taskName := strings.TrimSpace(os.Getenv(workerenv.TaskName))
	taskUID := strings.TrimSpace(os.Getenv(workerenv.TaskUID))
	if baseToolCtx != nil {
		if strings.TrimSpace(baseToolCtx.Namespace) != "" {
			namespace = strings.TrimSpace(baseToolCtx.Namespace)
		}
		if strings.TrimSpace(baseToolCtx.TaskID) != "" {
			taskName = strings.TrimSpace(baseToolCtx.TaskID)
		}
		if strings.TrimSpace(baseToolCtx.TaskUID) != "" {
			taskUID = strings.TrimSpace(baseToolCtx.TaskUID)
		}
	}
	return namespace, taskName, taskUID
}

func toolNameSet(names []string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name != "" {
			out[name] = struct{}{}
		}
	}
	return out
}

func parseResolvedApprovals(raw string) ([]approvals.ResolvedApproval, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var resolved []approvals.ResolvedApproval
	if err := json.Unmarshal([]byte(raw), &resolved); err != nil {
		return nil, fmt.Errorf("parse %s: %w", workerenv.ResolvedApprovals, err)
	}
	return resolved, nil
}

func (g *approvalGate) enabled() bool {
	return g != nil && len(g.required) > 0
}

func (g *approvalGate) requiresApproval(toolName string) bool {
	if !g.enabled() {
		return false
	}
	_, ok := g.required[strings.TrimSpace(toolName)]
	return ok
}

func (g *approvalGate) preScan(ctx context.Context, calls []llm.ToolCall) (*approvalBatchDecision, error) {
	if g == nil {
		return nil, nil
	}
	for _, call := range calls {
		toolName := strings.TrimSpace(call.Name)
		requiresApproval := g.requiresApproval(toolName)
		target, err := g.targetForCall(toolName, call.Arguments)
		if err != nil {
			return nil, err
		}
		decision, found := g.resolvedDecision(target)
		if found && decision.Status != approvals.StatusApproved {
			return &approvalBatchDecision{
				continueLLM: true,
				toolResults: deniedBatchToolResults(calls, call.ID, decision),
			}, nil
		}
		if !requiresApproval || found {
			continue
		}
		if err := g.emitApprovalRequest(ctx, target, call.ID); err != nil {
			return nil, err
		}
		return &approvalBatchDecision{
			result: fmt.Sprintf(
				"approval requested for %s (approvalID %s); parked until a human decides",
				target.TargetTool,
				target.ApprovalID,
			),
		}, nil
	}
	return nil, nil
}

func (g *approvalGate) targetForCall(toolName string, args json.RawMessage) (approvals.ApprovalTarget, error) {
	return approvals.NewApprovalTarget(
		g.namespace,
		g.taskName,
		g.taskUID,
		toolName,
		args,
		fmt.Sprintf("Execute %s", toolName),
		fmt.Sprintf("Human approval is required before executing %s", toolName),
		"warning",
	)
}

func (g *approvalGate) resolvedDecision(target approvals.ApprovalTarget) (approvals.ResolvedApproval, bool) {
	for _, decision := range g.resolved {
		if decision.ID != target.ApprovalID {
			continue
		}
		if decision.TaskUID != "" && decision.TaskUID != target.TaskUID {
			continue
		}
		if decision.TargetTool != target.TargetTool ||
			decision.TargetArgsDigest != target.TargetArgsDigest {
			continue
		}
		return decision, true
	}
	return approvals.ResolvedApproval{}, false
}

func deniedBatchToolResults(
	calls []llm.ToolCall,
	deniedToolCallID string,
	decision approvals.ResolvedApproval,
) []llm.Message {
	results := make([]llm.Message, 0, len(calls))
	for _, call := range calls {
		content := fmt.Sprintf(
			"Not executed because approval %s for %s is %s",
			decision.ID,
			decision.TargetTool,
			decision.Status,
		)
		if call.ID != deniedToolCallID {
			content = fmt.Sprintf(
				"Not executed because the same tool-call batch contained denied approval %s for %s",
				decision.ID,
				decision.TargetTool,
			)
		}
		results = append(results, llm.Message{Role: "tool", Content: content, ToolCallID: call.ID, Name: call.Name})
	}
	return results
}

func (g *approvalGate) emitApprovalRequest(
	ctx context.Context,
	target approvals.ApprovalTarget,
	modelToolCallID string,
) error {
	return emitApprovalRequest(ctx, g.recorder, target, modelToolCallID)
}

func emitApprovalRequest(
	ctx context.Context,
	recorder common.EventRecorder,
	target approvals.ApprovalTarget,
	modelToolCallID string,
) error {
	content, err := json.Marshal(target)
	if err != nil {
		return fmt.Errorf("marshal approval request: %w", err)
	}
	return common.RecordEventStrict(ctx, recorder, events.ExecutionEventTypeApprovalRequested,
		common.WithEventSeverity(events.ExecutionEventSeverityWarning),
		common.WithEventToolName(target.TargetTool),
		common.WithEventToolCallID(target.ApprovalID),
		common.WithEventSummary(target.Action),
		common.WithEventContent(json.RawMessage(content)),
		common.WithEventContentText(fmt.Sprintf(
			"approval requested for model tool call %s",
			strings.TrimSpace(modelToolCallID),
		)),
	)
}

func approvalEmitterFromRecorder(recorder common.EventRecorder) func(context.Context, approvals.ApprovalTarget) error {
	return func(ctx context.Context, target approvals.ApprovalTarget) error {
		return emitApprovalRequest(ctx, recorder, target, "")
	}
}

func (g *approvalGate) prepareApprovedCall(
	toolName string,
	args json.RawMessage,
) (json.RawMessage, string, bool, error) {
	requiresApproval := g.requiresApproval(toolName)
	target, err := g.targetForCall(toolName, args)
	if err != nil {
		return nil, "", false, err
	}
	decision, found := g.resolvedDecision(target)
	if !requiresApproval && !found {
		return args, "", false, nil
	}
	if !found {
		return nil, "", false, fmt.Errorf("approval %s for %s is not resolved", target.ApprovalID, target.TargetTool)
	}
	if decision.Status != approvals.StatusApproved {
		return nil, "", false, fmt.Errorf("approval %s for %s is %s", decision.ID, decision.TargetTool, decision.Status)
	}
	if g.firedKeys[target.ApprovalID] {
		return nil, target.ApprovalID, true, nil
	}
	return args, target.ApprovalID, false, nil
}

func (g *approvalGate) markFired(key string) {
	if g == nil || strings.TrimSpace(key) == "" {
		return
	}
	g.firedKeys[key] = true
}

func injectIdempotencyKey(args json.RawMessage, key string) (json.RawMessage, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return args, nil
	}
	params := map[string]any{}
	if len(strings.TrimSpace(string(args))) > 0 {
		if err := json.Unmarshal(args, &params); err != nil {
			return nil, fmt.Errorf("parse tool arguments for idempotency key injection: %w", err)
		}
	}
	if existing, ok := params["idempotencyKey"]; ok {
		existingString := strings.TrimSpace(fmt.Sprint(existing))
		if existingString != "" && existingString != key {
			return nil, fmt.Errorf("tool arguments contain conflicting reserved idempotencyKey")
		}
	}
	params["idempotencyKey"] = key
	out, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal tool arguments with idempotency key: %w", err)
	}
	return json.RawMessage(out), nil
}

func formatResolvedApprovalsContext(resolved []approvals.ResolvedApproval) string {
	if len(resolved) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## Resolved Human Approvals\n\n")
	for _, approval := range resolved {
		status := strings.ToUpper(strings.TrimSpace(approval.Status))
		if status == "" {
			status = "RESOLVED"
		}
		sb.WriteString("- ")
		sb.WriteString(status)
		sb.WriteString(" ")
		sb.WriteString(approval.ID)
		if approval.TargetTool != "" {
			sb.WriteString(" for ")
			sb.WriteString(approval.TargetTool)
		}
		if approval.Actor != "" {
			sb.WriteString(" by ")
			sb.WriteString(approval.Actor)
		}
		if approval.DecisionTime != "" {
			sb.WriteString(" at ")
			sb.WriteString(approval.DecisionTime)
		}
		if approval.Reason != "" {
			sb.WriteString(": ")
			sb.WriteString(approval.Reason)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func prependResolvedApprovalsContext(prompt string, resolved []approvals.ResolvedApproval) string {
	section := strings.TrimSpace(formatResolvedApprovalsContext(resolved))
	if section == "" {
		return prompt
	}
	return section + "\n\n## Task\n\n" + prompt
}

func prepareApprovalToolContext(baseToolCtx *tools.ToolContext, recorder common.EventRecorder) *tools.ToolContext {
	if baseToolCtx == nil {
		return nil
	}
	if baseToolCtx.ApprovalEmitter != nil && baseToolCtx.TaskUID != "" {
		return baseToolCtx
	}
	baseToolCtxCopy := *baseToolCtx
	if baseToolCtxCopy.ApprovalEmitter == nil {
		baseToolCtxCopy.ApprovalEmitter = approvalEmitterFromRecorder(recorder)
	}
	if baseToolCtxCopy.TaskUID == "" {
		baseToolCtxCopy.TaskUID = os.Getenv(workerenv.TaskUID)
	}
	return &baseToolCtxCopy
}

func handleExplicitRequestApprovalBatch(
	ctx context.Context,
	calls []llm.ToolCall,
	allowedToolCalls map[string]struct{},
	eventRecorder common.EventRecorder,
	baseToolCtx *tools.ToolContext,
) (*approvalBatchDecision, error) {
	for _, call := range calls {
		if strings.TrimSpace(call.Name) != "request_approval" {
			continue
		}
		if _, ok := allowedToolCalls["request_approval"]; !ok {
			return nil, nil
		}
		result, err := executeRequestApprovalToolCall(ctx, call, eventRecorder, baseToolCtx)
		if err != nil {
			return nil, err
		}
		return &approvalBatchDecision{result: result}, nil
	}
	return nil, nil
}

func executeRequestApprovalToolCall(
	ctx context.Context,
	call llm.ToolCall,
	eventRecorder common.EventRecorder,
	baseToolCtx *tools.ToolContext,
) (string, error) {
	toolName := strings.TrimSpace(call.Name)
	common.RecordEventWithTimeout(eventRecorder, events.ExecutionEventTypeToolCallStarted, modelLoopEventTimeout,
		common.WithEventToolName(toolName),
		common.WithEventToolCallID(call.ID),
		common.WithEventSummary("tool call started"),
		common.WithEventContent(eventContent(map[string]any{
			"toolName":      toolName,
			"toolCallID":    call.ID,
			"argumentBytes": len(call.Arguments),
		})),
	)

	execCtx := ctx
	baseToolCtx = prepareApprovalToolContext(baseToolCtx, eventRecorder)
	if baseToolCtx != nil {
		toolCtxCopy := *baseToolCtx
		toolCtxCopy.ToolCallID = call.ID
		if toolCtxCopy.Tenant == "" {
			toolCtxCopy.Tenant = toolCtxCopy.Namespace
		}
		execCtx = tools.WithToolContext(ctx, &toolCtxCopy)
	}
	result, err := tools.DefaultRegistry.Execute(execCtx, toolName, call.Arguments)
	if err != nil {
		common.RecordEventWithTimeout(eventRecorder, events.ExecutionEventTypeToolCallFailed, modelLoopEventTimeout,
			common.WithEventSeverity(events.ExecutionEventSeverityError),
			common.WithEventToolName(toolName),
			common.WithEventToolCallID(call.ID),
			common.WithEventSummary(err.Error()),
		)
		return "", err
	}
	common.RecordEventWithTimeout(eventRecorder, events.ExecutionEventTypeToolCallCompleted, modelLoopEventTimeout,
		common.WithEventToolName(toolName),
		common.WithEventToolCallID(call.ID),
		common.WithEventSummary("tool call completed"),
		common.WithEventContent(eventContent(map[string]any{
			"toolName":     toolName,
			"toolCallID":   call.ID,
			"resultLength": len(result),
		})),
	)
	return result, nil
}

func processApprovalBatch(
	ctx context.Context,
	messages []llm.Message,
	calls []llm.ToolCall,
	gate *approvalGate,
	allowedToolCalls map[string]struct{},
	eventRecorder common.EventRecorder,
	baseToolCtx *tools.ToolContext,
) ([]llm.Message, string, bool, bool, error) {
	if decision, err := gate.preScan(ctx, calls); err != nil {
		return messages, "", false, false, err
	} else if decision != nil {
		return applyApprovalBatchDecision(messages, decision)
	}
	if decision, err := handleExplicitRequestApprovalBatch(
		ctx,
		calls,
		allowedToolCalls,
		eventRecorder,
		baseToolCtx,
	); err != nil {
		return messages, "", false, false, err
	} else if decision != nil {
		return applyApprovalBatchDecision(messages, decision)
	}
	return messages, "", false, false, nil
}

func applyApprovalBatchDecision(
	messages []llm.Message,
	decision *approvalBatchDecision,
) ([]llm.Message, string, bool, bool, error) {
	if decision.result != "" {
		return messages, decision.result, true, false, nil
	}
	if len(decision.toolResults) > 0 {
		messages = append(messages, decision.toolResults...)
	}
	return messages, "", false, decision.continueLLM, nil
}
