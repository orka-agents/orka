/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sozercan/orka/internal/approvals"
)

func TestRequestApprovalToolEmitsTargetApproval(t *testing.T) {
	var got approvals.ApprovalTarget
	ctx := WithToolContext(context.Background(), &ToolContext{
		Namespace: "default",
		TaskID:    "task-1",
		TaskUID:   "task-uid-1",
		ApprovalEmitter: func(_ context.Context, target approvals.ApprovalTarget) error {
			got = target
			return nil
		},
	})
	tool := NewRequestApprovalTool()
	result, err := tool.Execute(ctx, json.RawMessage(`{
		"action":"Dispatch a technician",
		"riskSummary":"Mutates an external work-order system",
		"severity":"critical",
		"targetTool":"dispatch_work_order",
		"targetArguments":{"incident":"inc-1","zone":"az-a"}
	}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got.ApprovalID == "" || got.TargetTool != "dispatch_work_order" || got.TargetArgsDigest == "" {
		t.Fatalf("emitted target = %#v", got)
	}
	if got.Action != "Dispatch a technician" || got.RiskSummary == "" || got.Severity != "critical" {
		t.Fatalf("emitted target did not preserve safe summary fields: %#v", got)
	}
	var parsed ChatToolResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("result JSON: %v", err)
	}
	if !parsed.Success {
		t.Fatalf("result = %#v", parsed)
	}
}

func TestRequestApprovalToolRequiresStrictEmitter(t *testing.T) {
	_, err := NewRequestApprovalTool().Execute(context.Background(), json.RawMessage(`{"action":"approve"}`))
	if err == nil {
		t.Fatal("expected missing emitter error")
	}
}

func TestRequestApprovalToolRequiresConcreteTarget(t *testing.T) {
	ctx := WithToolContext(context.Background(), &ToolContext{
		Namespace: "default",
		TaskID:    "task-1",
		TaskUID:   "task-uid-1",
		ApprovalEmitter: func(context.Context, approvals.ApprovalTarget) error {
			return nil
		},
	})
	_, err := NewRequestApprovalTool().Execute(ctx, json.RawMessage(`{"action":"dispatch team"}`))
	if err == nil || err.Error() != "targetTool is required" {
		t.Fatalf("Execute() error = %v, want targetTool required", err)
	}
}

func TestRequestApprovalToolRejectsBuiltInTarget(t *testing.T) {
	ctx := WithToolContext(context.Background(), &ToolContext{
		Namespace: "default",
		TaskID:    "task-1",
		TaskUID:   "task-uid-1",
		ApprovalEmitter: func(context.Context, approvals.ApprovalTarget) error {
			return nil
		},
	})
	_, err := NewRequestApprovalTool().Execute(ctx, json.RawMessage(`{
		"action":"approve web search",
		"targetTool":"web_search",
		"targetArguments":{"query":"incident"}
	}`))
	if err == nil {
		t.Fatal("expected built-in target rejection")
	}
}
