/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

func TestRequestApprovalToolIncludesTargetSpecDigest(t *testing.T) {
	var got approvals.ApprovalTarget
	ctx := WithToolContext(context.Background(), &ToolContext{
		Namespace: "default",
		TaskID:    "task-1",
		TaskUID:   "task-uid-1",
		ApprovalEmitter: func(_ context.Context, target approvals.ApprovalTarget) error {
			got = target
			return nil
		},
		ApprovalTargetSpecDigest: func(_ context.Context, targetTool string) (string, error) {
			if targetTool != "dispatch_work_order" {
				t.Fatalf("targetTool = %q, want dispatch_work_order", targetTool)
			}
			return "spec-digest", nil
		},
	})

	_, err := NewRequestApprovalTool().Execute(ctx, json.RawMessage(`{
		"action":"Dispatch a technician",
		"targetTool":"dispatch_work_order",
		"targetArguments":{"incident":"inc-1"}
	}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got.TargetSpecDigest != "spec-digest" {
		t.Fatalf("TargetSpecDigest = %q, want spec-digest", got.TargetSpecDigest)
	}
}

func TestRequestApprovalToolPropagatesTargetSpecDigestError(t *testing.T) {
	wantErr := errors.New("missing custom tool")
	ctx := WithToolContext(context.Background(), &ToolContext{
		Namespace: "default",
		TaskID:    "task-1",
		TaskUID:   "task-uid-1",
		ApprovalEmitter: func(context.Context, approvals.ApprovalTarget) error {
			t.Fatal("approval should not be emitted when target spec digest fails")
			return nil
		},
		ApprovalTargetSpecDigest: func(context.Context, string) (string, error) {
			return "", wantErr
		},
	})

	_, err := NewRequestApprovalTool().Execute(ctx, json.RawMessage(`{
		"action":"Dispatch a technician",
		"targetTool":"dispatch_work_order",
		"targetArguments":{"incident":"inc-1"}
	}`))
	if !errors.Is(err, wantErr) {
		t.Fatalf("Execute() error = %v, want %v", err, wantErr)
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

func TestRequestApprovalToolRequiresObjectTargetArguments(t *testing.T) {
	ctx := WithToolContext(context.Background(), &ToolContext{
		Namespace: "default",
		TaskID:    "task-1",
		TaskUID:   "task-uid-1",
		ApprovalEmitter: func(context.Context, approvals.ApprovalTarget) error {
			t.Fatal("approval should not be emitted for non-object targetArguments")
			return nil
		},
	})
	_, err := NewRequestApprovalTool().Execute(ctx, json.RawMessage(`{
		"action":"approve dispatch",
		"targetTool":"dispatch_work_order",
		"targetArguments":"{\"incident\":\"inc-1\"}"
	}`))
	if err == nil || !strings.Contains(err.Error(), "targetArguments must be a JSON object") {
		t.Fatalf("Execute() error = %v, want targetArguments object error", err)
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
