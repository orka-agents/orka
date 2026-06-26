/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/approvals"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/llm"
	toolspkg "github.com/sozercan/orka/internal/tools"
	"github.com/sozercan/orka/internal/worker"
	"github.com/sozercan/orka/internal/workerenv"
	"github.com/sozercan/orka/workers/common"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	gatedDispatchTool    = "dispatch_work_order"
	declineHandledResult = "decline handled"
	correctedResult      = "corrected"
)

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

func TestApprovalGatePreScanRejectsRequiredToolThatIsNotEnabled(t *testing.T) {
	t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
	t.Setenv(workerenv.AutonomousMode, "true")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "hallucinated tool",
				ToolCalls: []llm.ToolCall{{
					ID:        "call-dispatch",
					Name:      gatedDispatchTool,
					Arguments: json.RawMessage(`{"incident":"inc-1"}`),
				}},
				StopReason: "tool_use",
			},
			{Content: correctedResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		[]llm.Tool{},
		nil,
		nil,
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != correctedResult {
		t.Fatalf("result = %q, want %s", result, correctedResult)
	}
}

func TestApprovalGatePreScanReturnsMalformedGatedArgsToModel(t *testing.T) {
	t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
	t.Setenv(workerenv.AutonomousMode, "true")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "bad gated args",
				ToolCalls: []llm.ToolCall{
					{
						ID:        "call-dispatch",
						Name:      gatedDispatchTool,
						Arguments: json.RawMessage(`{"unterminated"`),
					},
					{
						ID:        "call-other",
						Name:      "other_tool",
						Arguments: json.RawMessage(`{"ok":true}`),
					},
				},
				StopReason: "tool_use",
			},
			{Content: correctedResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		[]llm.Tool{{Name: gatedDispatchTool}, {Name: "other_tool"}},
		nil,
		nil,
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != correctedResult {
		t.Fatalf("result = %q, want corrected", result)
	}
}

func TestApprovalGatePreScanReturnsNonObjectGatedArgsToModel(t *testing.T) {
	t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
	t.Setenv(workerenv.AutonomousMode, "true")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "bad gated args",
				ToolCalls: []llm.ToolCall{{
					ID:        "call-dispatch",
					Name:      gatedDispatchTool,
					Arguments: json.RawMessage(`[]`),
				}},
				StopReason: "tool_use",
			},
			{Content: correctedResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		[]llm.Tool{{Name: gatedDispatchTool}},
		nil,
		nil,
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != correctedResult {
		t.Fatalf("result = %q, want corrected", result)
	}
}

func TestApprovalGatePrepareApprovedCallSkipsUngatedMalformedArgs(t *testing.T) {
	args := json.RawMessage(`{"unterminated"`)
	for _, tt := range []struct {
		name     string
		resolved []approvals.ResolvedApproval
	}{
		{name: "no resolved approvals"},
		{
			name:     "unrelated resolved approval",
			resolved: []approvals.ResolvedApproval{{ID: "other", Status: approvals.StatusApproved}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			gate := &approvalGate{resolved: tt.resolved}
			gotArgs, approvalKey, alreadyFired, err := gate.prepareApprovedCall("ungated_tool", args, nil)
			if err != nil {
				t.Fatalf("prepareApprovedCall() error = %v", err)
			}
			if string(gotArgs) != string(args) || approvalKey != "" || alreadyFired {
				t.Fatalf("prepareApprovedCall() = args %q key %q fired %t", gotArgs, approvalKey, alreadyFired)
			}
		})
	}
}

func TestApprovalGateMismatchedCustomToolSpecRequestsNewApproval(t *testing.T) {
	var executions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		executions.Add(1)
		fmt.Fprint(w, `{"ok":true}`) //nolint:errcheck
	}))
	defer toolServer.Close()

	args := json.RawMessage(`{"incident":"inc-1"}`)
	oldTool := approvalTestCustomTool(toolServer.URL + "/old")
	oldSpecDigest, err := approvalTargetSpecDigest(oldTool)
	if err != nil {
		t.Fatalf("old spec digest: %v", err)
	}
	oldTarget, err := approvals.NewApprovalTarget(
		"default",
		"incident-task",
		"task-uid-1",
		gatedDispatchTool,
		args,
		"dispatch",
		"",
		"",
		oldSpecDigest,
	)
	if err != nil {
		t.Fatalf("old approval target: %v", err)
	}
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{resolvedApprovalForTarget(oldTarget)})
	t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
	t.Setenv(workerenv.AutonomousMode, "true")
	recorder := common.NewFakeEventRecorder()
	currentTool := approvalTestCustomTool(toolServer.URL + "/new")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{response: &llm.CompletionResponse{
			Content: "dispatching",
			ToolCalls: []llm.ToolCall{{
				ID:        "call-dispatch",
				Name:      gatedDispatchTool,
				Arguments: args,
			}},
			StopReason: "tool_use",
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		[]llm.Tool{{Name: gatedDispatchTool}},
		map[string]*corev1alpha1.Tool{gatedDispatchTool: currentTool},
		worker.NewToolExecutor(),
		recorder,
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if executions.Load() != 0 {
		t.Fatalf("mismatched custom tool spec executed gated tool")
	}
	if !strings.Contains(result, "approval requested") {
		t.Fatalf("result = %q, want new approval requested", result)
	}
	assertRecordedEventTypesEventually(t, recorder, []string{events.ExecutionEventTypeApprovalRequested})
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
		ID:                "k-1",
		TargetTool:        "dispatch_work_order",
		Status:            approvals.StatusApproved,
		Actor:             "reviewer-a",
		DecisionTime:      "2026-06-23T10:00:00Z",
		Reason:            "safe to dispatch",
		TargetArgsPreview: json.RawMessage(`{"incident":"inc-1"}`),
	}})
	wantParts := []string{
		"## Resolved Human Approvals",
		"APPROVED k-1",
		"dispatch_work_order",
		"reviewer-a",
		`args={"incident":"inc-1"}`,
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

func TestApprovalTargetSpecDigestKeepsHTTPToolSpecDigest(t *testing.T) {
	tool := approvalTestCustomTool("https://tools.example.test/dispatch")
	got, err := approvalTargetSpecDigest(tool)
	if err != nil {
		t.Fatalf("approvalTargetSpecDigest() error = %v", err)
	}
	want, err := approvals.TargetSpecDigest(tool.Spec)
	if err != nil {
		t.Fatalf("TargetSpecDigest() error = %v", err)
	}
	if got != want {
		t.Fatalf("HTTP tool digest = %q, want raw spec digest %q", got, want)
	}
}

func TestApprovalTargetSpecDigestIncludesMCPStatusEndpoint(t *testing.T) {
	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			Description: "mcp tool",
			MCP: &corev1alpha1.MCPToolServer{
				SubstrateActor: &corev1alpha1.SubstrateMCPActor{
					TemplateRef: corev1alpha1.WorkspaceTemplateReference{Name: "template-a"},
				},
			},
		},
		Status: corev1alpha1.ToolStatus{
			Endpoint: "http://actor-a/mcp",
			Actor:    &corev1alpha1.ToolActorStatus{RouteHost: "actor-a.test"},
		},
	}
	first, err := approvalTargetSpecDigest(tool)
	if err != nil {
		t.Fatalf("approvalTargetSpecDigest() error = %v", err)
	}
	tool.Status.Endpoint = "http://actor-b/mcp"
	second, err := approvalTargetSpecDigest(tool)
	if err != nil {
		t.Fatalf("approvalTargetSpecDigest() second error = %v", err)
	}
	if first == second {
		t.Fatalf("MCP status endpoint change did not affect approval target digest")
	}
}

func approvalTestCustomTool(url string) *corev1alpha1.Tool {
	return &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: gatedDispatchTool},
		Spec: corev1alpha1.ToolSpec{
			Description: "approval-gated custom tool",
			HTTP: &corev1alpha1.HTTPExecution{
				URL:    url,
				Method: http.MethodPost,
			},
		},
	}
}

func resolvedApprovalForTarget(target approvals.ApprovalTarget) approvals.ResolvedApproval {
	return approvals.ResolvedApproval{
		ID:               target.ApprovalID,
		TaskUID:          target.TaskUID,
		TargetTool:       target.TargetTool,
		TargetArgsDigest: target.TargetArgsDigest,
		TargetSpecDigest: target.TargetSpecDigest,
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

func TestRequestApprovalToolValidationErrorReturnsWholeBatchToModel(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	toolspkg.DefaultRegistry.Register(toolspkg.NewRequestApprovalTool())
	t.Setenv(workerenv.AutonomousMode, "true")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "bad approval",
				ToolCalls: []llm.ToolCall{
					{
						ID:        "call-approval",
						Name:      "request_approval",
						Arguments: json.RawMessage(`{"action":"missing target"}`),
					},
					{
						ID:        "call-other",
						Name:      "other_tool",
						Arguments: json.RawMessage(`{"ok":true}`),
					},
				},
				StopReason: "tool_use",
			},
			{Content: correctedResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		toolspkg.DefaultRegistry.ToLLMTools([]string{"request_approval"}),
		nil,
		nil,
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != correctedResult {
		t.Fatalf("result = %q, want corrected", result)
	}
}

func TestRequestApprovalToolDuplicateTerminalDecisionReturnsWholeBatchToModel(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	toolspkg.DefaultRegistry.Register(toolspkg.NewRequestApprovalTool())
	t.Setenv(workerenv.AutonomousMode, "true")
	customTool := approvalTestCustomTool("https://tools.example.test/dispatch")
	args := json.RawMessage(`{"incident":"inc-1"}`)
	specDigest, err := approvalTargetSpecDigest(customTool)
	if err != nil {
		t.Fatalf("target spec digest: %v", err)
	}
	target, err := approvals.NewApprovalTarget(
		"default",
		"incident-task",
		"task-uid-1",
		gatedDispatchTool,
		args,
		"dispatch",
		"",
		"",
		specDigest,
	)
	if err != nil {
		t.Fatalf("approval target: %v", err)
	}
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{resolvedApprovalForTarget(target)})

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "duplicate approval",
				ToolCalls: []llm.ToolCall{
					{
						ID:   "call-approval",
						Name: "request_approval",
						Arguments: json.RawMessage(
							`{"action":"dispatch","targetTool":"dispatch_work_order","targetArguments":{"incident":"inc-1"}}`,
						),
					},
					{ID: "call-other", Name: "other_tool", Arguments: json.RawMessage(`{"ok":true}`)},
				},
				StopReason: "tool_use",
			},
			{Content: correctedResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		toolspkg.DefaultRegistry.ToLLMTools([]string{"request_approval"}),
		map[string]*corev1alpha1.Tool{gatedDispatchTool: customTool},
		nil,
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != correctedResult {
		t.Fatalf("result = %q, want %s", result, correctedResult)
	}
}

func TestRequestApprovalToolCallEmitsCustomToolSpecDigest(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	toolspkg.DefaultRegistry.Register(toolspkg.NewRequestApprovalTool())
	t.Setenv(workerenv.AutonomousMode, "true")
	recorder := common.NewFakeEventRecorder()
	customTool := approvalTestCustomTool("https://tools.example.test/dispatch")
	wantDigest, err := approvalTargetSpecDigest(customTool)
	if err != nil {
		t.Fatalf("target spec digest: %v", err)
	}

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
		map[string]*corev1alpha1.Tool{gatedDispatchTool: customTool},
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
	var target approvals.ApprovalTarget
	for _, event := range recorder.Events() {
		if event.Type != events.ExecutionEventTypeApprovalRequested {
			continue
		}
		if err := json.Unmarshal(event.Content, &target); err != nil {
			t.Fatalf("approval content JSON: %v", err)
		}
	}
	if target.TargetSpecDigest != wantDigest {
		t.Fatalf("TargetSpecDigest = %q, want %q", target.TargetSpecDigest, wantDigest)
	}
}

func TestRequestApprovalToolCallStopsWorkerAfterEmitting(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	toolspkg.DefaultRegistry.Register(toolspkg.NewRequestApprovalTool())
	t.Setenv(workerenv.AutonomousMode, "true")
	recorder := common.NewFakeEventRecorder()
	customTool := approvalTestCustomTool("https://tools.example.test/dispatch")

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
		map[string]*corev1alpha1.Tool{gatedDispatchTool: customTool},
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
			{Content: declineHandledResult, StopReason: "end_turn"},
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
	if result != declineHandledResult {
		t.Fatalf("result = %q, want decline handled", result)
	}
	if executions.Load() != 0 {
		t.Fatalf("declined approval executed gated tool")
	}
}

func TestApprovalGatePreScanStaleExplicitCustomToolSpecBlocksWholeBatch(t *testing.T) {
	var otherExecutions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/other" {
			otherExecutions.Add(1)
		}
		fmt.Fprint(w, `{"ok":true}`) //nolint:errcheck
	}))
	defer toolServer.Close()
	oldTool := approvalTestCustomTool(toolServer.URL + "/old")
	oldSpecDigest, err := approvalTargetSpecDigest(oldTool)
	if err != nil {
		t.Fatalf("old target spec digest: %v", err)
	}
	args := json.RawMessage(`{"incident":"inc-1"}`)
	target, err := approvals.NewApprovalTarget(
		"default",
		"incident-task",
		"task-uid-1",
		gatedDispatchTool,
		args,
		"dispatch",
		"",
		"",
		oldSpecDigest,
	)
	if err != nil {
		t.Fatalf("approval target: %v", err)
	}
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{resolvedApprovalForTarget(target)})
	t.Setenv(workerenv.AutonomousMode, "true")
	currentTool := approvalTestCustomTool(toolServer.URL + "/new")
	otherTool := approvalTestCustomTool(toolServer.URL + "/other")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "dispatching",
				ToolCalls: []llm.ToolCall{
					{ID: "call-other", Name: "other_tool", Arguments: json.RawMessage(`{"ok":true}`)},
					{ID: "call-1", Name: gatedDispatchTool, Arguments: args},
				},
				StopReason: "tool_use",
			},
			{Content: "spec change handled", StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		[]llm.Tool{{Name: "other_tool"}, {Name: gatedDispatchTool}},
		map[string]*corev1alpha1.Tool{gatedDispatchTool: currentTool, "other_tool": otherTool},
		worker.NewToolExecutor(),
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != "spec change handled" {
		t.Fatalf("result = %q, want spec change handled", result)
	}
	if otherExecutions.Load() != 0 {
		t.Fatalf("earlier batch tool executed before stale explicit approval was handled")
	}
}

func TestApprovalGateStaleExplicitCustomToolSpecDoesNotExecuteWithoutRequiredTools(t *testing.T) {
	var executions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		executions.Add(1)
		fmt.Fprint(w, `{"ok":true}`) //nolint:errcheck
	}))
	defer toolServer.Close()
	oldTool := approvalTestCustomTool(toolServer.URL + "/old")
	oldSpecDigest, err := approvalTargetSpecDigest(oldTool)
	if err != nil {
		t.Fatalf("old target spec digest: %v", err)
	}
	args := json.RawMessage(`{"incident":"inc-1"}`)
	target, err := approvals.NewApprovalTarget(
		"default",
		"incident-task",
		"task-uid-1",
		gatedDispatchTool,
		args,
		"dispatch",
		"",
		"",
		oldSpecDigest,
	)
	if err != nil {
		t.Fatalf("approval target: %v", err)
	}
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{resolvedApprovalForTarget(target)})
	t.Setenv(workerenv.AutonomousMode, "true")
	currentTool := approvalTestCustomTool(toolServer.URL + "/new")

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
			{Content: "spec change handled", StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		[]llm.Tool{{Name: gatedDispatchTool}},
		map[string]*corev1alpha1.Tool{gatedDispatchTool: currentTool},
		worker.NewToolExecutor(),
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != "spec change handled" {
		t.Fatalf("result = %q, want spec change handled", result)
	}
	if executions.Load() != 0 {
		t.Fatalf("stale explicit custom approval target executed custom tool")
	}
}

func TestApprovalGateDeclinedExplicitCustomToolTargetDoesNotExecuteWithoutRequiredTools(t *testing.T) {
	var executions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		executions.Add(1)
		fmt.Fprint(w, `{"ok":true}`) //nolint:errcheck
	}))
	defer toolServer.Close()
	customTool := approvalTestCustomTool(toolServer.URL)
	specDigest, err := approvalTargetSpecDigest(customTool)
	if err != nil {
		t.Fatalf("target spec digest: %v", err)
	}
	args := json.RawMessage(`{"incident":"inc-1"}`)
	target, err := approvals.NewApprovalTarget(
		"default",
		"incident-task",
		"task-uid-1",
		gatedDispatchTool,
		args,
		"dispatch",
		"",
		"",
		specDigest,
	)
	if err != nil {
		t.Fatalf("approval target: %v", err)
	}
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
			{Content: declineHandledResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		[]llm.Tool{{Name: gatedDispatchTool}},
		map[string]*corev1alpha1.Tool{gatedDispatchTool: customTool},
		worker.NewToolExecutor(),
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != declineHandledResult {
		t.Fatalf("result = %q, want decline handled", result)
	}
	if executions.Load() != 0 {
		t.Fatalf("declined explicit custom approval target executed custom tool")
	}
}

func TestApprovalGateDeclinedExplicitCustomToolBodyAuthTargetDoesNotExecuteWithoutRequiredTools(t *testing.T) {
	var executions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		executions.Add(1)
		fmt.Fprint(w, `{"ok":true}`) //nolint:errcheck
	}))
	defer toolServer.Close()
	customTool := approvalTestCustomTool(toolServer.URL)
	customTool.Spec.HTTP.AuthInject = "body"
	customTool.Spec.HTTP.AuthBodyKey = "api_key"
	specDigest, err := approvalTargetSpecDigest(customTool)
	if err != nil {
		t.Fatalf("target spec digest: %v", err)
	}
	approvedArgs := json.RawMessage(`{"incident":"inc-1"}`)
	retryArgs := json.RawMessage(`{"incident":"inc-1","api_key":"model-supplied"}`)
	target, err := approvals.NewApprovalTarget(
		"default",
		"incident-task",
		"task-uid-1",
		gatedDispatchTool,
		approvedArgs,
		"dispatch",
		"",
		"",
		specDigest,
	)
	if err != nil {
		t.Fatalf("approval target: %v", err)
	}
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
					Arguments: retryArgs,
				}},
				StopReason: "tool_use",
			},
			{Content: declineHandledResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		[]llm.Tool{{Name: gatedDispatchTool}},
		map[string]*corev1alpha1.Tool{gatedDispatchTool: customTool},
		worker.NewToolExecutor(),
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != declineHandledResult {
		t.Fatalf("result = %q, want decline handled", result)
	}
	if executions.Load() != 0 {
		t.Fatalf("declined body-auth approval target executed custom tool")
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
			{Content: declineHandledResult, StopReason: "end_turn"},
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
	if result != declineHandledResult {
		t.Fatalf("result = %q, want decline handled", result)
	}
	if executions.Load() != 0 {
		t.Fatalf("declined explicit approval target executed gated tool")
	}
}
