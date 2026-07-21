package main

import (
	"context"
	"encoding/json"
	"fmt"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/internal/worker"
)

func executeLoopTool(
	ctx context.Context,
	call llm.ToolCall,
	toolName string,
	allowed map[string]struct{},
	customTools map[string]*corev1alpha1.Tool,
	executor *worker.ToolExecutor,
	gate *approvalGate,
	baseToolCtx *tools.ToolContext,
) (string, error, bool) {
	var execArgs json.RawMessage
	approvalKey := ""
	alreadyFired := false
	customTool := customTools[toolName]
	policyToolName := toolName
	if customTool != nil {
		policyToolName = customTool.Name
	}
	var err error
	if _, ok := allowed[toolName]; !ok {
		err = fmt.Errorf("tool %q was not enabled for this task", call.Name)
		tools.RecordRejectedToolCall(ctx, toolName, call.ID, "tool_not_enabled", err.Error())
	} else {
		execArgs, approvalKey, alreadyFired, err = gate.prepareApprovedCall(
			ctx,
			policyToolName,
			call.Arguments,
			customTool,
		)
	}
	if err != nil {
		return "", err, false
	}
	if alreadyFired {
		result := fmt.Sprintf(
			"already executed approved action for idempotency key %s; skipping duplicate",
			approvalKey,
		)
		return result, nil, false
	}
	if customTool != nil {
		execCtx := worker.WithToolCallID(ctx, call.ID)
		if approvalKey != "" {
			execCtx = worker.WithToolIdempotencyKey(execCtx, approvalKey)
		}
		result, execErr := executor.Execute(execCtx, customTool, execArgs)
		if execErr == nil || worker.ToolRequestWasAttempted(execErr) {
			gate.markFired(approvalKey)
		}
		return result, execErr, execErr == nil
	}
	if approvalKey != "" {
		execArgs, err = injectIdempotencyKey(execArgs, approvalKey)
		if err != nil {
			return "", err, false
		}
	}
	execCtx := ctx
	if baseToolCtx != nil {
		toolCtxCopy := *baseToolCtx
		toolCtxCopy.ToolCallID = call.ID
		if toolCtxCopy.Tenant == "" {
			toolCtxCopy.Tenant = toolCtxCopy.Namespace
		}
		execCtx = tools.WithToolContext(ctx, &toolCtxCopy)
	}
	gate.markFired(approvalKey)
	result, execErr := tools.DefaultRegistry.Execute(execCtx, toolName, execArgs)
	return result, execErr, execErr == nil
}

func executeGuardedLoopTool(
	ctx context.Context,
	call llm.ToolCall,
	toolName string,
	allowed map[string]struct{},
	customTools map[string]*corev1alpha1.Tool,
	executor *worker.ToolExecutor,
	gate *approvalGate,
	baseToolCtx *tools.ToolContext,
	guard *analysisLoopGuard,
) (result string, cached, completed bool, execErr error) {
	if _, ok := allowed[toolName]; !ok {
		result, execErr, completed = executeLoopTool(
			ctx, call, toolName, allowed, customTools, executor, gate, baseToolCtx,
		)
		return result, false, completed, execErr
	}
	if err := guard.beginToolCall(toolName); err != nil {
		return "", false, false, err
	}
	customTool := customTools[toolName]
	if result, cached = guard.cachedToolResult(toolName, call.Arguments, customTool); cached {
		return result, true, true, nil
	}
	result, execErr, completed = executeLoopTool(
		ctx,
		call,
		toolName,
		allowed,
		customTools,
		executor,
		gate,
		baseToolCtx,
	)
	return result, false, completed, execErr
}
