/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sozercan/orka/internal/approvals"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/llm"
	toolspkg "github.com/sozercan/orka/internal/tools"
	"github.com/sozercan/orka/internal/workerenv"
	"github.com/sozercan/orka/workers/common"
)

const gatedDispatchTool = "dispatch_work_order"

func TestApprovalGatePrescansBatchBeforeExecutingGatedTool(t *testing.T) {
	for _, tt := range []struct {
		name  string
		calls []llm.ToolCall
	}{
		{
			name: "dispatch before request approval",
			calls: []llm.ToolCall{
				{
					ID:        "call-dispatch",
					Name:      gatedDispatchTool,
					Arguments: json.RawMessage(`{"incident":"inc-1"}`),
				},
				{
					ID:        "call-approval",
					Name:      "request_approval",
					Arguments: json.RawMessage(`{"action":"approve dispatch"}`),
				},
			},
		},
		{
			name: "request approval before dispatch",
			calls: []llm.ToolCall{
				{
					ID:        "call-approval",
					Name:      "request_approval",
					Arguments: json.RawMessage(`{"action":"approve dispatch"}`),
				},
				{
					ID:        "call-dispatch",
					Name:      gatedDispatchTool,
					Arguments: json.RawMessage(`{"incident":"inc-1"}`),
				},
			},
		},
		{
			name: "dispatch only",
			calls: []llm.ToolCall{{
				ID:        "call-dispatch",
				Name:      gatedDispatchTool,
				Arguments: json.RawMessage(`{"incident":"inc-1"}`),
			}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			restore := replaceDefaultToolRegistryForTest(t)
			defer restore()
			var executions atomic.Int32
			toolspkg.DefaultRegistry.Register(recordingTool{
				name: gatedDispatchTool,
				onExecute: func(json.RawMessage) {
					executions.Add(1)
				},
			})
			toolspkg.DefaultRegistry.Register(toolspkg.NewRequestApprovalTool())
			t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
			t.Setenv(workerenv.AutonomousMode, "true")
			recorder := common.NewFakeEventRecorder()

			result, err := executeAgentLoopWithEvents(
				context.Background(),
				&mockProvider{response: &llm.CompletionResponse{
					Content:    "need action",
					ToolCalls:  tt.calls,
					StopReason: "tool_use",
				}},
				[]llm.Message{{Role: "user", Content: "handle incident"}},
				"",
				"test-model",
				toolspkg.DefaultRegistry.ToLLMTools([]string{gatedDispatchTool, "request_approval"}),
				nil,
				nil,
				recorder,
				&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
			)
			if err != nil {
				t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
			}
			if executions.Load() != 0 {
				t.Fatalf("gated tool executed before approval")
			}
			if !strings.Contains(result, "approval requested") {
				t.Fatalf("result = %q, want approval requested", result)
			}
			assertRecordedEventTypesEventually(t, recorder, []string{events.ExecutionEventTypeApprovalRequested})
		})
	}
}

func TestApprovalGateExecutesApprovedMatchingTargetWithIdempotencyKey(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	var gotArgs json.RawMessage
	toolspkg.DefaultRegistry.Register(recordingTool{
		name: gatedDispatchTool,
		onExecute: func(args json.RawMessage) {
			gotArgs = append(json.RawMessage(nil), args...)
		},
	})
	args := json.RawMessage(`{"incident":"inc-1"}`)
	target := approvalTargetForTest(t, args)
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{resolvedApprovalForTarget(target)})
	t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
	t.Setenv(workerenv.AutonomousMode, "true")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "dispatching",
				ToolCalls: []llm.ToolCall{{
					ID:        "call-dispatch",
					Name:      gatedDispatchTool,
					Arguments: args,
				}},
				StopReason: "tool_use",
			},
			{Content: "done", StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		toolspkg.DefaultRegistry.ToLLMTools([]string{gatedDispatchTool}),
		nil,
		nil,
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != "done" {
		t.Fatalf("result = %q, want done", result)
	}
	var body map[string]any
	if err := json.Unmarshal(gotArgs, &body); err != nil {
		t.Fatalf("executed args JSON: %v", err)
	}
	if body["idempotencyKey"] != target.ApprovalID {
		t.Fatalf("executed args = %#v, want idempotencyKey %s", body, target.ApprovalID)
	}
}

func TestApprovalGateMismatchedResolvedApprovalRequestsNewApproval(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	var executions atomic.Int32
	toolspkg.DefaultRegistry.Register(recordingTool{
		name: gatedDispatchTool,
		onExecute: func(json.RawMessage) {
			executions.Add(1)
		},
	})
	oldTarget := approvalTargetForTest(t, json.RawMessage(`{"incident":"old"}`))
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{resolvedApprovalForTarget(oldTarget)})
	t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
	t.Setenv(workerenv.AutonomousMode, "true")
	recorder := common.NewFakeEventRecorder()

	_, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{response: &llm.CompletionResponse{
			Content: "dispatching",
			ToolCalls: []llm.ToolCall{{
				ID:        "call-dispatch",
				Name:      gatedDispatchTool,
				Arguments: json.RawMessage(`{"incident":"new"}`),
			}},
			StopReason: "tool_use",
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		toolspkg.DefaultRegistry.ToLLMTools([]string{gatedDispatchTool}),
		nil,
		nil,
		recorder,
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if executions.Load() != 0 {
		t.Fatalf("mismatched approval executed gated tool")
	}
	assertRecordedEventTypesEventually(t, recorder, []string{events.ExecutionEventTypeApprovalRequested})
}

func TestApprovalGateSkipsDuplicateApprovedFireWithinWorker(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	var executions atomic.Int32
	toolspkg.DefaultRegistry.Register(recordingTool{
		name: gatedDispatchTool,
		onExecute: func(json.RawMessage) {
			executions.Add(1)
		},
	})
	args := json.RawMessage(`{"incident":"inc-1"}`)
	target := approvalTargetForTest(t, args)
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{resolvedApprovalForTarget(target)})
	t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
	t.Setenv(workerenv.AutonomousMode, "true")

	_, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "dispatching",
				ToolCalls: []llm.ToolCall{{
					ID:        "call-1",
					Name:      gatedDispatchTool,
					Arguments: args,
				}},
				StopReason: "tool_use",
			},
			{
				Content: "dispatching again",
				ToolCalls: []llm.ToolCall{{
					ID:        "call-2",
					Name:      gatedDispatchTool,
					Arguments: args,
				}},
				StopReason: "tool_use",
			},
			{Content: "done", StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		toolspkg.DefaultRegistry.ToLLMTools([]string{gatedDispatchTool}),
		nil,
		nil,
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if executions.Load() != 1 {
		t.Fatalf("executions = %d, want 1", executions.Load())
	}
}

func TestResolvedApprovalsContextIsReadable(t *testing.T) {
	got := formatResolvedApprovalsContext([]approvals.ResolvedApproval{{
		ID:           "k-1",
		TargetTool:   "dispatch_work_order",
		Status:       approvals.StatusApproved,
		Actor:        "reviewer-a",
		DecisionTime: "2026-06-23T10:00:00Z",
		Reason:       "safe to dispatch",
	}})
	wantParts := []string{
		"## Resolved Human Approvals",
		"APPROVED k-1",
		"dispatch_work_order",
		"reviewer-a",
		"safe to dispatch",
	}
	for _, want := range wantParts {
		if !strings.Contains(got, want) {
			t.Fatalf("resolved approval context missing %q:\n%s", want, got)
		}
	}
}

func approvalTargetForTest(t *testing.T, args json.RawMessage) approvals.ApprovalTarget {
	t.Helper()
	target, err := approvals.NewApprovalTarget(
		"default",
		"incident-task",
		"task-uid-1",
		gatedDispatchTool,
		args,
		"dispatch",
		"",
		"",
	)
	if err != nil {
		t.Fatalf("NewApprovalTarget() error = %v", err)
	}
	return target
}

func resolvedApprovalForTarget(target approvals.ApprovalTarget) approvals.ResolvedApproval {
	return approvals.ResolvedApproval{
		ID:               target.ApprovalID,
		TaskUID:          target.TaskUID,
		TargetTool:       target.TargetTool,
		TargetArgsDigest: target.TargetArgsDigest,
		Status:           approvals.StatusApproved,
	}
}

func setResolvedApprovalsEnv(t *testing.T, resolved []approvals.ResolvedApproval) {
	t.Helper()
	data, err := json.Marshal(resolved)
	if err != nil {
		t.Fatalf("marshal resolved approvals: %v", err)
	}
	t.Setenv(workerenv.ResolvedApprovals, string(data))
}

type recordingTool struct {
	name      string
	onExecute func(json.RawMessage)
}

func (t recordingTool) Name() string        { return t.name }
func (t recordingTool) Description() string { return "recording tool" }
func (t recordingTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (t recordingTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	if t.onExecute != nil {
		t.onExecute(args)
	}
	return `{"ok":true}`, nil
}

func TestApprovalToolingRequiresAutonomousMode(t *testing.T) {
	t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
	_, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{response: &llm.CompletionResponse{Content: "done", StopReason: "end_turn"}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		[]llm.Tool{{Name: gatedDispatchTool}},
		nil,
		nil,
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err == nil || !strings.Contains(err.Error(), "autonomous coordination mode") {
		t.Fatalf("error = %v, want autonomous-mode configuration error", err)
	}
}

func TestRequestApprovalToolCallStopsWorkerAfterEmitting(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	toolspkg.DefaultRegistry.Register(toolspkg.NewRequestApprovalTool())
	t.Setenv(workerenv.AutonomousMode, "true")
	recorder := common.NewFakeEventRecorder()

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{response: &llm.CompletionResponse{
			Content: "requesting approval",
			ToolCalls: []llm.ToolCall{{
				ID:   "call-approval",
				Name: "request_approval",
				Arguments: json.RawMessage(
					`{"action":"dispatch team","targetTool":"dispatch_work_order","targetArguments":{"incident":"inc-1"}}`,
				),
			}},
			StopReason: "tool_use",
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		toolspkg.DefaultRegistry.ToLLMTools([]string{"request_approval"}),
		nil,
		nil,
		recorder,
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if !strings.Contains(result, "approval requested") {
		t.Fatalf("result = %q, want approval requested", result)
	}
	assertRecordedEventTypesEventually(t, recorder, []string{events.ExecutionEventTypeApprovalRequested})
}

func TestApprovalGateDeclinedDecisionDoesNotExecuteAndContinues(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	var executions atomic.Int32
	toolspkg.DefaultRegistry.Register(recordingTool{
		name: gatedDispatchTool,
		onExecute: func(json.RawMessage) {
			executions.Add(1)
		},
	})
	args := json.RawMessage(`{"incident":"inc-1"}`)
	target := approvalTargetForTest(t, args)
	declined := resolvedApprovalForTarget(target)
	declined.Status = approvals.StatusDeclined
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{declined})
	t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
	t.Setenv(workerenv.AutonomousMode, "true")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "dispatching",
				ToolCalls: []llm.ToolCall{{
					ID:        "call-1",
					Name:      gatedDispatchTool,
					Arguments: args,
				}},
				StopReason: "tool_use",
			},
			{Content: "decline handled", StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		toolspkg.DefaultRegistry.ToLLMTools([]string{gatedDispatchTool}),
		nil,
		nil,
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != "decline handled" {
		t.Fatalf("result = %q, want decline handled", result)
	}
	if executions.Load() != 0 {
		t.Fatalf("declined approval executed gated tool")
	}
}

func TestApprovalGateDeclinedExplicitApprovalTargetDoesNotExecuteWithoutRequiredTools(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	var executions atomic.Int32
	toolspkg.DefaultRegistry.Register(recordingTool{
		name: gatedDispatchTool,
		onExecute: func(json.RawMessage) {
			executions.Add(1)
		},
	})
	args := json.RawMessage(`{"incident":"inc-1"}`)
	target := approvalTargetForTest(t, args)
	declined := resolvedApprovalForTarget(target)
	declined.Status = approvals.StatusDeclined
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{declined})
	t.Setenv(workerenv.AutonomousMode, "true")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "dispatching",
				ToolCalls: []llm.ToolCall{{
					ID:        "call-1",
					Name:      gatedDispatchTool,
					Arguments: args,
				}},
				StopReason: "tool_use",
			},
			{Content: "decline handled", StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		toolspkg.DefaultRegistry.ToLLMTools([]string{gatedDispatchTool}),
		nil,
		nil,
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != "decline handled" {
		t.Fatalf("result = %q, want decline handled", result)
	}
	if executions.Load() != 0 {
		t.Fatalf("declined explicit approval target executed gated tool")
	}
}
