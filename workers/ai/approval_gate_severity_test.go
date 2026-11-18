/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/llm"
	toolspkg "github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/internal/worker"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
)

func TestRequestApprovalToolInvalidSeverityFailsClosed(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	toolspkg.DefaultRegistry.Register(toolspkg.NewRequestApprovalTool())
	t.Setenv(workerenv.AutonomousMode, "true")
	t.Setenv(workerenv.ApprovalRequiredTools, "")

	var executions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		executions.Add(1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	customTool := approvalTestCustomTool(server.URL)
	customTools := map[string]*corev1alpha1.Tool{gatedDispatchTool: customTool}
	baseToolCtx := &toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"}
	target, err := explicitApprovalTargetForCall(context.Background(), llm.ToolCall{
		Arguments: json.RawMessage(`{
			"action":"dispatch","severity":"warning","targetTool":"dispatch_work_order",
			"targetArguments":{"incident":"inc-1"}
		}`),
	}, customTools, baseToolCtx)
	if err != nil {
		t.Fatalf("approval target: %v", err)
	}

	for _, status := range []string{"", approvals.StatusApproved, approvals.StatusDeclined} {
		name := status
		if name == "" {
			name = "unresolved"
		}
		t.Run(name, func(t *testing.T) {
			var resolved []approvals.ResolvedApproval
			if status != "" {
				decision := resolvedApprovalForTarget(target)
				decision.Status = status
				resolved = append(resolved, decision)
			}
			setResolvedApprovalsEnv(t, resolved)
			for _, severity := range []string{"unsupported", "Warning", " warning "} {
				t.Run(severity, func(t *testing.T) {
					args, err := json.Marshal(map[string]any{
						"action": "dispatch", "severity": severity,
						"targetTool": gatedDispatchTool, "targetArguments": map[string]any{"incident": "inc-1"},
					})
					if err != nil {
						t.Fatalf("marshal approval call: %v", err)
					}
					var emitted bool
					directCtx := *baseToolCtx
					directCtx.ApprovalEmitter = func(context.Context, approvals.ApprovalTarget) error {
						emitted = true
						return nil
					}
					directResult, directErr := toolspkg.DefaultRegistry.Execute(
						toolspkg.WithToolContext(context.Background(), &directCtx), "request_approval", args,
					)
					_, validationError := errors.AsType[*toolspkg.RequestApprovalValidationError](directErr)
					if !validationError || !strings.Contains(directErr.Error(), "invalid severity") {
						t.Fatalf("direct error = %v, want severity validation error", directErr)
					}
					if directResult != "" || emitted {
						t.Fatalf("direct invalid call returned %q, emitted = %v", directResult, emitted)
					}

					provider := &mockProvider{responses: []*llm.CompletionResponse{
						{
							ToolCalls: []llm.ToolCall{
								{ID: "call-approval", Name: "request_approval", Arguments: args},
								// Different arguments keep the terminal target's denial out of preScan;
								// this side effect must still be skipped by the invalid explicit request.
								{ID: "call-dispatch", Name: gatedDispatchTool, Arguments: json.RawMessage(`{"incident":"inc-2"}`)},
							},
							StopReason: "tool_use",
						},
						{Content: correctedResult, StopReason: "end_turn"},
					}}
					recorder := common.NewFakeEventRecorder()
					llmTools := append(
						toolspkg.DefaultRegistry.ToLLMTools([]string{"request_approval"}),
						llm.Tool{Name: gatedDispatchTool},
					)
					result, err := executeAgentLoopWithEvents(
						context.Background(), provider, []llm.Message{{Role: "user", Content: "handle incident"}},
						"", "test-model", modelSettings{maxTokens: 4096}, llmTools, customTools,
						worker.NewToolExecutor(), recorder, baseToolCtx,
					)
					if err != nil || result != correctedResult || len(provider.requests) != 2 {
						t.Fatalf("loop result = %q, error = %v, requests = %d; want validation continuation",
							result, err, len(provider.requests))
					}
					messages := provider.requests[1].Messages
					results := messages[len(messages)-2:]
					if results[0].ToolCallID != "call-approval" || results[0].Content != directErr.Error() {
						t.Fatalf("approval result = %+v, want direct validation error %q", results[0], directErr.Error())
					}
					if results[1].ToolCallID != "call-dispatch" || !strings.Contains(results[1].Content, "Not executed") {
						t.Fatalf("dispatch result = %+v, want batch skip", results[1])
					}
					if executions.Load() != 0 {
						t.Fatalf("invalid severity executed target tool")
					}
					for _, event := range recorder.Events() {
						if event.Type == events.ExecutionEventTypeApprovalRequested {
							t.Fatalf("invalid severity emitted an approval request")
						}
					}
				})
			}
		})
	}
}

func TestRequestApprovalToolResolvedSeverityPreservesContinuation(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	toolspkg.DefaultRegistry.Register(toolspkg.NewRequestApprovalTool())
	t.Setenv(workerenv.AutonomousMode, "true")
	t.Setenv(workerenv.ApprovalRequiredTools, "")

	for _, status := range []string{approvals.StatusApproved, approvals.StatusDeclined} {
		t.Run(status, func(t *testing.T) {
			for _, tt := range []struct {
				name     string
				severity json.RawMessage
			}{
				{name: "omitted"},
				{name: "empty", severity: json.RawMessage(`""`)},
				{name: "null", severity: json.RawMessage(`null`)},
				{name: "warning", severity: json.RawMessage(`"warning"`)},
				{name: "critical", severity: json.RawMessage(`"critical"`)},
			} {
				t.Run(tt.name, func(t *testing.T) {
					var executions atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						executions.Add(1)
						_, _ = w.Write([]byte(`{"ok":true}`))
					}))
					defer server.Close()
					customTool := approvalTestCustomTool(server.URL)
					customTools := map[string]*corev1alpha1.Tool{gatedDispatchTool: customTool}
					baseToolCtx := &toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"}
					args := map[string]json.RawMessage{
						"action": json.RawMessage(`"dispatch"`), "targetTool": json.RawMessage(`"dispatch_work_order"`),
						"targetArguments": json.RawMessage(`{"incident":"inc-1"}`),
					}
					if tt.severity != nil {
						args["severity"] = tt.severity
					}
					callArgs, err := json.Marshal(args)
					if err != nil {
						t.Fatalf("marshal approval call: %v", err)
					}
					approvalCall := llm.ToolCall{ID: "call-approval", Name: "request_approval", Arguments: callArgs}
					target, err := explicitApprovalTargetForCall(context.Background(), approvalCall, customTools, baseToolCtx)
					if err != nil {
						t.Fatalf("approval target: %v", err)
					}
					decision := resolvedApprovalForTarget(target)
					decision.Status = status
					decision.Severity = "warning"
					setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{decision})
					provider := &mockProvider{responses: []*llm.CompletionResponse{
						{
							ToolCalls: []llm.ToolCall{
								approvalCall,
								{ID: "call-skipped", Name: gatedDispatchTool, Arguments: json.RawMessage(`{"incident":"inc-2"}`)},
							},
							StopReason: "tool_use",
						},
						{
							ToolCalls: []llm.ToolCall{
								{ID: "call-dispatch", Name: gatedDispatchTool, Arguments: args["targetArguments"]},
							},
							StopReason: "tool_use",
						},
						{Content: doneResult, StopReason: "end_turn"},
					}}
					recorder := common.NewFakeEventRecorder()
					llmTools := append(
						toolspkg.DefaultRegistry.ToLLMTools([]string{"request_approval"}),
						llm.Tool{Name: gatedDispatchTool},
					)
					result, err := executeAgentLoopWithEvents(
						context.Background(), provider, []llm.Message{{Role: "user", Content: "handle incident"}},
						"", "test-model", modelSettings{maxTokens: 4096}, llmTools, customTools,
						worker.NewToolExecutor(), recorder, baseToolCtx,
					)
					if err != nil || result != doneResult || len(provider.requests) != 3 {
						t.Fatalf("loop result = %q, error = %v, requests = %d; want resolved continuation",
							result, err, len(provider.requests))
					}
					messages := provider.requests[1].Messages
					results := messages[len(messages)-2:]
					if results[0].ToolCallID != "call-approval" || !strings.Contains(results[0].Content, status) {
						t.Fatalf("approval result = %+v, want terminal %s decision", results[0], status)
					}
					if results[1].ToolCallID != "call-skipped" || !strings.Contains(results[1].Content, "Not executed") {
						t.Fatalf("other result = %+v, want batch skip", results[1])
					}
					var wantExecutions int32
					if status == approvals.StatusApproved {
						wantExecutions = 1
					}
					if executions.Load() != wantExecutions {
						t.Fatalf("target executions = %d, want %d", executions.Load(), wantExecutions)
					}
					for _, event := range recorder.Events() {
						if event.Type == events.ExecutionEventTypeApprovalRequested {
							t.Fatalf("terminal request emitted a new approval request")
						}
					}
				})
			}
		})
	}
}
