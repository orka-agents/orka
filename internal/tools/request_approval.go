/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sozercan/orka/internal/approvals"
)

const requestApprovalToolName = "request_approval"

// RequestApprovalTool records human approval requests for consequential worker actions.
type RequestApprovalTool struct{}

func NewRequestApprovalTool() *RequestApprovalTool { return &RequestApprovalTool{} }

func (t *RequestApprovalTool) Name() string { return requestApprovalToolName }

func (t *RequestApprovalTool) Description() string {
	return "Request explicit human approval before a consequential action. The worker enforces configured approval-required tools independently of this helper."
}

func (t *RequestApprovalTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"action":{"type":"string","description":"Human-readable action requiring approval"},
			"riskSummary":{"type":"string","description":"Why this action needs approval"},
			"severity":{"type":"string","enum":["warning","critical"],"description":"Approval severity"},
			"targetTool":{"type":"string","description":"Exact custom side-effect tool name the approval covers"},
			"targetArguments":{"type":"object","description":"Exact side-effect arguments the approval covers"}
		},
		"required":["action","targetTool","targetArguments"]
	}`)
}

type RequestApprovalValidationError struct {
	Err error
}

func (e *RequestApprovalValidationError) Error() string {
	if e == nil || e.Err == nil {
		return "invalid request_approval arguments"
	}
	return e.Err.Error()
}

func (e *RequestApprovalValidationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func requestApprovalValidationError(format string, args ...any) error {
	return &RequestApprovalValidationError{Err: fmt.Errorf(format, args...)}
}

type requestApprovalArgs struct {
	Action          string          `json:"action"`
	RiskSummary     string          `json:"riskSummary"`
	Severity        string          `json:"severity"`
	TargetTool      string          `json:"targetTool"`
	TargetArguments json.RawMessage `json:"targetArguments"`
}

func (t *RequestApprovalTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var req requestApprovalArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &req); err != nil {
			return "", requestApprovalValidationError("failed to parse request_approval arguments: %w", err)
		}
	}
	toolCtx := GetToolContext(ctx)
	if toolCtx == nil || toolCtx.ApprovalEmitter == nil {
		return "", fmt.Errorf("approval emitter is not configured")
	}
	targetTool := strings.TrimSpace(req.TargetTool)
	if targetTool == "" {
		return "", requestApprovalValidationError("targetTool is required")
	}
	if len(req.TargetArguments) == 0 {
		return "", requestApprovalValidationError("targetArguments is required")
	}
	var targetArgsObject map[string]any
	if err := json.Unmarshal(req.TargetArguments, &targetArgsObject); err != nil || targetArgsObject == nil {
		return "", requestApprovalValidationError("targetArguments must be a JSON object")
	}
	if _, ok := DefaultRegistry.Get(targetTool); ok {
		return "", requestApprovalValidationError("targetTool %q is a built-in tool and cannot be approved with request_approval", targetTool)
	}
	targetSpecDigest := ""
	if toolCtx.ApprovalTargetSpecDigest != nil {
		var err error
		targetSpecDigest, err = toolCtx.ApprovalTargetSpecDigest(ctx, targetTool)
		if err != nil {
			return "", requestApprovalValidationError("resolve target tool spec digest: %w", err)
		}
	}
	target, err := approvals.NewApprovalTarget(
		toolCtx.Namespace,
		toolCtx.TaskID,
		toolCtx.TaskUID,
		targetTool,
		req.TargetArguments,
		req.Action,
		req.RiskSummary,
		req.Severity,
		targetSpecDigest,
	)
	if err != nil {
		return "", requestApprovalValidationError("build approval target: %w", err)
	}
	if err := toolCtx.ApprovalEmitter(ctx, target); err != nil {
		return "", fmt.Errorf("emit approval request: %w", err)
	}
	return ChatToolSuccess(map[string]any{
		"approvalID":       target.ApprovalID,
		"targetTool":       target.TargetTool,
		"targetArgsDigest": target.TargetArgsDigest,
		"status":           approvals.StatusPending,
		"message":          "approval requested; wait for a human decision before executing the approved action",
	})
}
