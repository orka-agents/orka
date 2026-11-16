/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/llm"
	toolspkg "github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
)

func TestInjectIdempotencyKeyPreservesArguments(t *testing.T) {
	for _, tt := range []struct {
		name string
		args string
	}{
		{"empty arguments", ""},
		{"whitespace arguments", " \n\t "},
		{"empty object", `{}`},
		{"large integer", `{"count":9007199254740993}`},
		{"int64 limits", `{"min":-9223372036854775808,"max":9223372036854775807}`},
		{"exact number literals", `{"fraction":9007199254740992.5,"ratio":1.0000000000000001,"exp":1.00e+03}`},
		{"extreme exponents", `{"tiny":1e-400,"large":1e400}`},
		{"nested numbers", `{"config":{"count":9007199254740993},"items":[9007199254740993,{"ratio":1.00e+03}]}`},
		{"valid Unicode", `{"text":"café \ud83d\ude00 �","\\ud800":"literal escape"}`},
		{"matching reserved key", `{"idempotencyKey":" approval-key ","count":9007199254740993}`},
		{"empty reserved key", `{"idempotencyKey":"","count":9007199254740993}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := json.RawMessage(tt.args)
			got, err := injectIdempotencyKey(args, " approval-key ")
			if err != nil {
				t.Fatalf("injectIdempotencyKey() error = %v", err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(got, &fields); err != nil {
				t.Fatalf("injected arguments: %v", err)
			}
			if string(fields["idempotencyKey"]) != `"approval-key"` {
				t.Fatalf("idempotencyKey = %s", fields["idempotencyKey"])
			}
			delete(fields, "idempotencyKey")
			withoutKey, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			var original map[string]json.RawMessage
			if len(strings.TrimSpace(tt.args)) > 0 {
				if err := json.Unmarshal(args, &original); err != nil {
					t.Fatal(err)
				}
			} else {
				original = map[string]json.RawMessage{}
			}
			delete(original, "idempotencyKey")
			want, err := json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			if gotDigest, err := approvals.TargetArgsDigest(withoutKey); err != nil {
				t.Fatal(err)
			} else if wantDigest, err := approvals.TargetArgsDigest(want); err != nil || gotDigest != wantDigest {
				t.Fatalf("arguments changed: got %s, want %s (digest error %v)", withoutKey, want, err)
			}
			// Comparing digests alone would miss changes to number spellings.
			for _, number := range []string{
				"9007199254740993", "-9223372036854775808", "9223372036854775807",
				"9007199254740992.5", "1.0000000000000001", "1.00e+03", "1e-400", "1e400",
			} {
				if strings.Contains(tt.args, number) && !strings.Contains(string(got), number) {
					t.Errorf("number literal %s changed in %s", number, got)
				}
			}
			if string(args) != tt.args {
				t.Fatal("injection mutated the input")
			}
		})
	}
}

func TestInjectIdempotencyKeyRejectsMalformedArguments(t *testing.T) {
	for _, tt := range []struct {
		name    string
		args    string
		wantErr string
	}{
		{"top duplicate invalid first", `{"text":{},"text":"ok"}`, "text must be sent once"},
		{"top duplicate invalid last", `{"text":"ok","text":{}}`, "text must be sent once"},
		{"nested duplicate invalid first", `{"config":{"count":{},"count":1}}`, "config.count must be sent once"},
		{"nested duplicate invalid last", `{"config":{"count":1,"count":{}}}`, "config.count must be sent once"},
		{"array duplicate", `{"items":[{"count":1,"count":2}]}`, "items[0].count must be sent once"},
		{"escaped duplicate", `{"text":"ok","\u0074ext":"other"}`, "text must be sent once"},
		{"reserved duplicate", `{"idempotencyKey":"wrong","idempotencyKey":"approval-key"}`,
			"idempotencyKey must be sent once"},
		{"invalid UTF8 value", "{\"text\":\"\xff\"}", "invalid Unicode"},
		{"invalid UTF8 key", "{\"\xff\":1}", "invalid Unicode"},
		{"unpaired high surrogate", `{"text":"\ud800"}`, "invalid Unicode"},
		{"unpaired low surrogate", `{"text":"\udc00"}`, "invalid Unicode"},
		{"nested unpaired surrogate", `{"config":{"text":"\ud800"}}`, "invalid Unicode"},
		{"unpaired surrogate key", `{"\ud800":1}`, "invalid Unicode"},
		{"trailing JSON", `{"text":"ok"}{}`, "parse tool arguments"},
		{"truncated JSON", `{"text":`, "parse tool arguments"},
		{"null", `null`, "parse tool arguments"},
		{"array", `[]`, "parse tool arguments"},
		{"scalar", `"text"`, "parse tool arguments"},
		{"conflicting reserved key", `{"idempotencyKey":"wrong"}`, "conflicting reserved idempotencyKey"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := injectIdempotencyKey(json.RawMessage(tt.args), "approval-key")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || got != nil {
				t.Fatalf("injectIdempotencyKey() = %s, %v; want %q and no arguments", got, err, tt.wantErr)
			}
			if strings.Contains(tt.wantErr, "sent once") || strings.Contains(tt.wantErr, "Unicode") {
				if _, ok := errors.AsType[*toolspkg.ToolArgumentError](err); !ok {
					t.Fatalf("error = %v, want wrapped ToolArgumentError", err)
				}
			}
		})
	}
}

func TestInjectIdempotencyKeyWithoutKeyLeavesArgumentsUntouched(t *testing.T) {
	args := json.RawMessage(`{"text":"\ud800","text":9007199254740993}`)
	got, err := injectIdempotencyKey(args, " \t ")
	if err != nil || string(got) != string(args) {
		t.Fatalf("injectIdempotencyKey() = %s, %v; want unchanged arguments", got, err)
	}
}

// injectionRegistryTool uses declared types so the test exercises both injection
// and the actual Registry.Execute validation, not only a permissive test tool.
type injectionRegistryTool struct{ recordingTool }

func (injectionRegistryTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
		"count":{"type":"integer"},"ratio":{"type":"number"},"text":{"type":"string"},
		"config":{"type":"object","properties":{"count":{"type":"integer"},"text":{"type":"string"}}},
		"items":{"type":"array","items":{"type":"object","properties":{"count":{"type":"integer"}}}}
	}}`)
}

func TestApprovedBuiltinRegistryExecutionPreservesAndValidatesArguments(t *testing.T) {
	for _, tt := range []struct {
		name    string
		args    string
		want    string
		wantErr string
	}{
		{"large integer", `{"count":9007199254740993}`, `{"count":9007199254740993}`, ""},
		{"negative large integer", `{"count":-9007199254740993}`, `{"count":-9007199254740993}`, ""},
		{"max int64", `{"count":9223372036854775807}`, `{"count":9223372036854775807}`, ""},
		{"nested exact numbers", `{"config":{"count":9007199254740993},"ratio":1.0000000000000001}`,
			`{"config":{"count":9007199254740993},"ratio":1.0000000000000001}`, ""},
		{"integral decimal", `{"count":9007199254740993.0}`, `{"count":9007199254740993}`, ""},
		{"fraction beyond float precision", `{"count":9007199254740992.5}`, "",
			"count must be a whole number, got a fraction"},
		{"small fraction", `{"count":1e-400}`, "", "count must be a whole number, got a fraction"},
		{"out of range integer", `{"count":9223372036854775808}`, "",
			"count must be a whole number, got a number out of range"},
		{"out of range number", `{"ratio":1e400}`, "", "ratio must be a number, got a number out of range"},
		{"top duplicate invalid first", `{"text":{},"text":"ok"}`, "", "text must be sent once"},
		{"top duplicate invalid last", `{"text":"ok","text":{}}`, "", "text must be sent once"},
		{"nested duplicate invalid first", `{"config":{"count":{},"count":1}}`, "", "config.count must be sent once"},
		{"nested duplicate invalid last", `{"config":{"count":1,"count":{}}}`, "", "config.count must be sent once"},
		{"array duplicate", `{"items":[{"count":1,"count":2}]}`, "", "items[0].count must be sent once"},
		{"invalid UTF8 value", "{\"text\":\"\xff\"}", "", "invalid Unicode"},
		{"invalid UTF8 key", "{\"\xff\":1}", "", "invalid Unicode"},
		{"unpaired surrogate", `{"text":"\ud800"}`, "", "invalid Unicode"},
		{"nested unpaired surrogate", `{"config":{"text":"\udc00"}}`, "", "invalid Unicode"},
		{"unpaired surrogate key", `{"\ud800":1}`, "", "invalid Unicode"},
		{"valid Unicode", `{"text":"café \ud83d\ude00 �"}`, `{"text":"café 😀 �"}`, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			restore := replaceDefaultToolRegistryForTest(t)
			defer restore()
			var executions []json.RawMessage
			toolspkg.DefaultRegistry.Register(injectionRegistryTool{recordingTool{
				name: gatedDispatchTool,
				onExecute: func(args json.RawMessage) {
					executions = append(executions, append(json.RawMessage(nil), args...))
				},
			}})
			args := json.RawMessage(tt.args)
			target := approvalTargetForTest(t, args)
			setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{resolvedApprovalForTarget(target)})
			t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
			t.Setenv(workerenv.AutonomousMode, "true")
			provider := &mockProvider{responses: []*llm.CompletionResponse{
				{ToolCalls: []llm.ToolCall{{ID: "approved-call", Name: gatedDispatchTool, Arguments: args}},
					StopReason: "tool_use"},
				{Content: doneResult, StopReason: "end_turn"},
			}}
			recorder := common.NewFakeEventRecorder()
			result, err := runInjectionAgentLoop(provider, recorder)
			if err != nil || result != doneResult {
				t.Fatalf("agent loop = %q, %v; want successful continuation", result, err)
			}
			if len(provider.requests) != 2 {
				t.Fatalf("model requests = %d, want tool result continuation", len(provider.requests))
			}
			messages := provider.requests[1].Messages
			toolResult := messages[len(messages)-1]
			if toolResult.Role != "tool" || toolResult.ToolCallID != "approved-call" {
				t.Fatalf("continuation = %+v, want matching tool result", toolResult)
			}
			if tt.wantErr != "" {
				if len(executions) != 0 || !strings.Contains(toolResult.Content, tt.wantErr) {
					t.Fatalf("executions = %d, result = %q; want rejection %q", len(executions), toolResult.Content, tt.wantErr)
				}
				assertRecordedEventTypesEventually(t, recorder, []string{events.ExecutionEventTypeToolCallFailed})
			} else {
				if len(executions) != 1 || strings.Contains(toolResult.Content, "Error") {
					t.Fatalf("executions = %d, result = %q; want one successful execution", len(executions), toolResult.Content)
				}
				assertInjectedExecutionArguments(t, executions[0], target.ApprovalID, tt.want)
				assertRecordedEventTypesEventually(t, recorder, []string{events.ExecutionEventTypeToolCallCompleted})
			}
			assertRecordedEventTypesEventually(t, recorder, []string{events.ExecutionEventTypeToolCallStarted})
			if slices.Contains(recorder.EventTypes(), events.ExecutionEventTypeApprovalRequested) {
				t.Fatal("call did not use the existing approved target")
			}
		})
	}
}

func TestApprovedBuiltinInjectionFailureDoesNotConsumeApproval(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	var executions []json.RawMessage
	toolspkg.DefaultRegistry.Register(injectionRegistryTool{recordingTool{
		name: gatedDispatchTool,
		onExecute: func(args json.RawMessage) {
			executions = append(executions, append(json.RawMessage(nil), args...))
		},
	}})
	valid := json.RawMessage(`{"count":9007199254740993}`)
	target := approvalTargetForTest(t, valid)
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{resolvedApprovalForTarget(target)})
	t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
	t.Setenv(workerenv.AutonomousMode, "true")
	provider := &mockProvider{responses: []*llm.CompletionResponse{
		{ToolCalls: []llm.ToolCall{{ID: "malformed", Name: gatedDispatchTool,
			Arguments: json.RawMessage(`{"count":{},"count":9007199254740993}`)}}, StopReason: "tool_use"},
		{ToolCalls: []llm.ToolCall{{ID: "corrected", Name: gatedDispatchTool, Arguments: valid}}, StopReason: "tool_use"},
		{ToolCalls: []llm.ToolCall{{ID: "repeated", Name: gatedDispatchTool, Arguments: valid}}, StopReason: "tool_use"},
		{Content: doneResult, StopReason: "end_turn"},
	}}
	recorder := common.NewFakeEventRecorder()
	result, err := runInjectionAgentLoop(provider, recorder)
	if err != nil || result != doneResult || len(executions) != 1 {
		t.Fatalf("agent loop = %q, %v, executions = %d; want one corrected execution", result, err, len(executions))
	}
	assertInjectedExecutionArguments(t, executions[0], target.ApprovalID, string(valid))
	if len(provider.requests) != 4 {
		t.Fatalf("model requests = %d, want four", len(provider.requests))
	}
	for i, want := range []string{"count must be sent once", `{"ok":true}`, "skipping duplicate"} {
		messages := provider.requests[i+1].Messages
		if content := messages[len(messages)-1].Content; !strings.Contains(content, want) {
			t.Errorf("continuation %d = %q, want %q", i, content, want)
		}
	}
	assertRecordedEventTypesEventually(t, recorder, []string{
		events.ExecutionEventTypeToolCallFailed, events.ExecutionEventTypeToolCallCompleted,
	})
}

func TestApprovedBuiltinPreflightFailureDoesNotConsumeApproval(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	var executions []json.RawMessage
	toolspkg.DefaultRegistry.Register(injectionRegistryTool{recordingTool{
		name: gatedDispatchTool,
		onExecute: func(args json.RawMessage) {
			executions = append(executions, append(json.RawMessage(nil), args...))
		},
	}})
	invalid := json.RawMessage(`{"count":{}}`)
	valid := json.RawMessage(`{"count":1}`)
	invalidTarget := approvalTargetForTest(t, invalid)
	validTarget := approvalTargetForTest(t, valid)
	// Corrected arguments have a distinct approval identity; do not loosen
	// exact argument binding just to permit a retry.
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{
		resolvedApprovalForTarget(invalidTarget), resolvedApprovalForTarget(validTarget),
	})
	t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
	t.Setenv(workerenv.AutonomousMode, "true")
	provider := &mockProvider{}
	for i, args := range []json.RawMessage{invalid, invalid, valid, valid} {
		provider.responses = append(provider.responses, &llm.CompletionResponse{
			ToolCalls: []llm.ToolCall{{ID: fmt.Sprint(i), Name: gatedDispatchTool, Arguments: args}}, StopReason: "tool_use",
		})
	}
	provider.responses = append(provider.responses, &llm.CompletionResponse{Content: doneResult, StopReason: "end_turn"})
	result, err := runInjectionAgentLoop(provider, common.NewFakeEventRecorder())
	if err != nil || result != doneResult || len(executions) != 1 || len(provider.requests) != 5 {
		t.Fatalf("agent loop = %q, %v, executions = %d, requests = %d; want one corrected execution",
			result, err, len(executions), len(provider.requests))
	}
	assertInjectedExecutionArguments(t, executions[0], validTarget.ApprovalID, string(valid))
	for i, want := range []string{
		"count must be a whole number", "count must be a whole number", `{"ok":true}`, "skipping duplicate",
	} {
		messages := provider.requests[i+1].Messages
		if content := messages[len(messages)-1].Content; !strings.Contains(content, want) {
			t.Errorf("continuation %d = %q, want %q", i, content, want)
		}
	}
}

type failingInjectionRegistryTool struct {
	injectionRegistryTool
	err error
}

func (t failingInjectionRegistryTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	_, _ = t.injectionRegistryTool.Execute(ctx, args)
	return "", t.err
}

func TestApprovedBuiltinExecutionErrorStillConsumesApproval(t *testing.T) {
	for _, execErr := range []error{
		errors.New("execution failed"),
		&toolspkg.ToolArgumentError{Field: "count", Want: "valid", Got: "invalid"},
	} {
		t.Run(execErr.Error(), func(t *testing.T) {
			restore := replaceDefaultToolRegistryForTest(t)
			defer restore()
			executions := 0
			toolspkg.DefaultRegistry.Register(failingInjectionRegistryTool{
				injectionRegistryTool: injectionRegistryTool{recordingTool{
					name: gatedDispatchTool, onExecute: func(json.RawMessage) { executions++ },
				}},
				err: execErr,
			})
			args := json.RawMessage(`{"count":1}`)
			target := approvalTargetForTest(t, args)
			setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{resolvedApprovalForTarget(target)})
			t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
			t.Setenv(workerenv.AutonomousMode, "true")
			provider := &mockProvider{responses: []*llm.CompletionResponse{
				{ToolCalls: []llm.ToolCall{{ID: "first", Name: gatedDispatchTool, Arguments: args}}, StopReason: "tool_use"},
				{ToolCalls: []llm.ToolCall{{ID: "repeat", Name: gatedDispatchTool, Arguments: args}}, StopReason: "tool_use"},
				{Content: doneResult, StopReason: "end_turn"},
			}}
			result, err := runInjectionAgentLoop(provider, common.NewFakeEventRecorder())
			if err != nil || result != doneResult || executions != 1 || len(provider.requests) != 3 {
				t.Fatalf("agent loop = %q, %v, executions = %d; want one attempted execution", result, err, executions)
			}
			messages := provider.requests[2].Messages
			if content := messages[len(messages)-1].Content; !strings.Contains(content, "skipping duplicate") {
				t.Fatalf("retry result = %q, want duplicate suppression", content)
			}
		})
	}
}

func runInjectionAgentLoop(provider llm.Provider, recorder common.EventRecorder) (string, error) {
	return executeAgentLoopWithEvents(
		context.Background(), provider, []llm.Message{{Role: "user", Content: "execute approved action"}}, "", "test-model",
		modelSettings{maxTokens: 4096}, toolspkg.DefaultRegistry.ToLLMTools([]string{gatedDispatchTool}), nil, nil, recorder,
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
}

func assertInjectedExecutionArguments(t *testing.T, args json.RawMessage, approvalID, want string) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil {
		t.Fatal(err)
	}
	var key string
	if err := json.Unmarshal(fields["idempotencyKey"], &key); err != nil || key != approvalID {
		t.Fatalf("idempotencyKey = %q, %v; want %q", key, err, approvalID)
	}
	delete(fields, "idempotencyKey")
	got, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("executed arguments = %s, want %s", got, want)
	}
}
