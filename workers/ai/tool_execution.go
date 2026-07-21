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
) (string, error) {
	var execArgs json.RawMessage
	approvalKey := ""
	alreadyFired := false
	customTool := customTools[toolName]
	var err error
	if _, ok := allowed[toolName]; !ok {
		err = fmt.Errorf("tool %q was not enabled for this task", call.Name)
		tools.RecordRejectedToolCall(ctx, toolName, call.ID, "tool_not_enabled", err.Error())
	} else {
		execArgs, approvalKey, alreadyFired, err = gate.prepareApprovedCall(
			ctx,
			toolName,
			call.Arguments,
			customTool,
		)
	}
	if err != nil {
		return "", err
	}
	if alreadyFired {
		return fmt.Sprintf("already executed approved action for idempotency key %s; skipping duplicate", approvalKey), nil
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
		return result, execErr
	}
	if approvalKey != "" {
		execArgs, err = injectIdempotencyKey(execArgs, approvalKey)
		if err != nil {
			return "", err
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
	return tools.DefaultRegistry.Execute(execCtx, toolName, execArgs)
}
