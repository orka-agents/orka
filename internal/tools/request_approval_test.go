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

func TestRequestApprovalToolActionOnlyRequestsUseDistinctApprovalIDs(t *testing.T) {
	var got []approvals.ApprovalTarget
	ctx := WithToolContext(context.Background(), &ToolContext{
		Namespace: "default",
		TaskID:    "task-1",
		TaskUID:   "task-uid-1",
		ApprovalEmitter: func(_ context.Context, target approvals.ApprovalTarget) error {
			got = append(got, target)
			return nil
		},
	})
	tool := NewRequestApprovalTool()
	if _, err := tool.Execute(ctx, json.RawMessage(`{"action":"dispatch team"}`)); err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	if _, err := tool.Execute(ctx, json.RawMessage(`{"action":"escalate incident"}`)); err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}
	if len(got) != 2 || got[0].ApprovalID == got[1].ApprovalID {
		t.Fatalf("approval IDs = %#v, want distinct IDs for different action-only requests", got)
	}
}
