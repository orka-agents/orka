package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/llm"
	toolspkg "github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/internal/worker"
	"github.com/orka-agents/orka/workers/common"
)

func TestToolAliasIsAdvertisedAndRouted(t *testing.T) {
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "read-artifact-build-scope",
			Annotations: map[string]string{toolAliasAnnotation: "read_artifact"},
		},
		Spec: corev1alpha1.ToolSpec{Description: "read artifact"},
	}
	customTools := map[string]*corev1alpha1.Tool{tool.Name: tool}
	registerToolAliases(customTools, []*corev1alpha1.Tool{tool})
	if customTools["read_artifact"] != tool {
		t.Fatal("tool alias was not routed to the scoped Tool")
	}
	llmTools := buildLLMTools([]string{tool.Name}, customTools)
	if len(llmTools) != 1 || llmTools[0].Name != "read_artifact" {
		t.Fatalf("LLM tools = %+v", llmTools)
	}
}

func TestToolAliasConflictFallsBackToResourceName(t *testing.T) {
	first := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "first", Annotations: map[string]string{toolAliasAnnotation: "shared"}},
	}
	second := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "second", Annotations: map[string]string{toolAliasAnnotation: "shared"}},
	}
	customTools := map[string]*corev1alpha1.Tool{first.Name: first, second.Name: second}
	registerToolAliases(customTools, []*corev1alpha1.Tool{first, second})
	if customTools["shared"] != first {
		t.Fatal("the first valid alias should remain registered")
	}
	llmTools := buildLLMTools([]string{second.Name}, customTools)
	if len(llmTools) != 1 || llmTools[0].Name != second.Name {
		t.Fatalf("conflicting alias was advertised: %+v", llmTools)
	}
}

func TestToolAliasPreservesApprovalPolicy(t *testing.T) {
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "scoped-sensitive-tool",
			Annotations: map[string]string{toolAliasAnnotation: "sensitive_tool"},
		},
		Spec: corev1alpha1.ToolSpec{
			Description: "sensitive",
			HTTP:        &corev1alpha1.HTTPExecution{URL: "http://example.invalid", Method: "POST"},
		},
	}
	customTools := map[string]*corev1alpha1.Tool{tool.Name: tool, "sensitive_tool": tool}
	gate := &approvalGate{
		namespace: "default", taskName: "task", taskUID: "uid",
		required: map[string]struct{}{tool.Name: {}}, firedKeys: map[string]bool{},
	}
	_, err, _ := executeLoopTool(
		context.Background(),
		llm.ToolCall{Name: "sensitive_tool", Arguments: json.RawMessage(`{}`)},
		"sensitive_tool",
		map[string]struct{}{"sensitive_tool": {}},
		customTools,
		worker.NewToolExecutor(),
		gate,
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "approval") {
		t.Fatalf("aliased approval-gated tool error = %v", err)
	}
}

func TestToolAliasPreservesAnalysisClassification(t *testing.T) {
	submit := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "submit-analysis-task"}}
	timeline := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "verify-timeline-build"}}
	customTools := map[string]*corev1alpha1.Tool{"finish": submit, "confirm": timeline}
	guard := newAnalysisLoopGuard([]llm.Tool{{Name: "finish"}, {Name: "confirm"}}, customTools)
	if !guard.validationRequired || !guard.isValidationTool("finish") || !guard.isTimelineTool("confirm") {
		t.Fatalf("aliased analysis tools were not classified: %+v", guard)
	}
	guard.timelineVerified = true
	selected := guard.selectToolCalls([]llm.ToolCall{{Name: "confirm"}, {Name: "finish"}})
	if len(selected) != 1 || selected[0].Name != "finish" {
		t.Fatalf("selected calls after timeline verification = %+v", selected)
	}
}

func TestToolAliasApprovalPreScanUsesResourceName(t *testing.T) {
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "scoped-sensitive-tool"},
		Spec: corev1alpha1.ToolSpec{
			Description: "sensitive",
			HTTP:        &corev1alpha1.HTTPExecution{URL: "http://example.invalid", Method: "POST"},
		},
	}
	customTools := map[string]*corev1alpha1.Tool{"sensitive_tool": tool}
	gate := &approvalGate{
		namespace: "default", taskName: "task", taskUID: "uid",
		required: map[string]struct{}{tool.Name: {}}, firedKeys: map[string]bool{},
		recorder: common.NewFakeEventRecorder(),
	}
	decision, err := gate.preScan(
		context.Background(),
		[]llm.ToolCall{{ID: "call", Name: "sensitive_tool", Arguments: json.RawMessage(`{}`)}},
		map[string]struct{}{"sensitive_tool": {}},
		customTools,
	)
	if err != nil {
		t.Fatal(err)
	}
	if decision == nil || !strings.Contains(decision.result, "approval requested") {
		t.Fatalf("approval decision = %+v", decision)
	}
}

func TestToolAliasRejectsConditionallyRegisteredBuiltIn(t *testing.T) {
	tool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{
		Name:        "custom-approval-tool",
		Annotations: map[string]string{toolAliasAnnotation: "request_approval"},
	}}
	customTools := map[string]*corev1alpha1.Tool{tool.Name: tool}
	registerToolAliases(customTools, []*corev1alpha1.Tool{tool})
	if customTools["request_approval"] != nil {
		t.Fatal("reserved request_approval alias was registered")
	}
	llmTools := buildLLMTools([]string{tool.Name}, customTools)
	if len(llmTools) != 1 || llmTools[0].Name != tool.Name {
		t.Fatalf("reserved alias was advertised: %+v", llmTools)
	}
}

func TestToolAliasUsesModelFacingSubmissionName(t *testing.T) {
	submit := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "submit-analysis-task"}}
	timeline := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "verify-timeline-build"}}
	customTools := map[string]*corev1alpha1.Tool{"finish": submit, "confirm": timeline}
	guard := newAnalysisLoopGuard([]llm.Tool{{Name: "finish"}, {Name: "confirm"}}, customTools)
	req := &llm.CompletionRequest{Tools: []llm.Tool{{Name: "finish"}, {Name: "confirm"}}}
	guard.investigationToolCalls = analysisMaxInvestigationToolCalls
	guard.prepareRequest(req, nil, 1, analysisLoopMaxIterations)
	if len(req.Messages) != 1 || !strings.Contains(req.Messages[0].Content, "finish") ||
		strings.Contains(req.Messages[0].Content, "validate_analysis") {
		t.Fatalf("validation prompt = %+v", req.Messages)
	}
	guard.timelineVerified = true
	guard.investigationToolCalls = 0
	guard.prepareRequest(req, nil, 2, analysisLoopMaxIterations)
	if len(req.Tools) != 1 || req.Tools[0].Name != "finish" {
		t.Fatalf("tools after timeline verification = %+v", req.Tools)
	}
}

func TestVerifiedTimelineCallIsSkipped(t *testing.T) {
	timeline := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "verify-timeline-build"}}
	submit := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "submit-analysis-task"}}
	guard := newAnalysisLoopGuard(
		[]llm.Tool{{Name: "confirm"}, {Name: "finish"}},
		map[string]*corev1alpha1.Tool{"confirm": timeline, "finish": submit},
	)
	guard.timelineVerified = true
	result, skip := guard.completedToolCallResult("confirm")
	if !skip || !strings.Contains(result, "finish") {
		t.Fatalf("completedToolCallResult() = %q, %t", result, skip)
	}
}

func TestRequestApprovalCanonicalizesAliasedTarget(t *testing.T) {
	tool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "scoped-sensitive-tool"}}
	call := llm.ToolCall{Arguments: json.RawMessage(`{
		"action":"run",
		"riskSummary":"risk",
		"severity":"warning",
		"targetTool":"sensitive_tool",
		"targetArguments":{}
	}`)}
	canonical, err := canonicalizeRequestApprovalCall(
		call,
		map[string]*corev1alpha1.Tool{"sensitive_tool": tool},
	)
	if err != nil {
		t.Fatal(err)
	}
	var args requestApprovalCallArgs
	if err := json.Unmarshal(canonical.Arguments, &args); err != nil {
		t.Fatal(err)
	}
	if args.TargetTool != tool.Name {
		t.Fatalf("targetTool = %q, want %q", args.TargetTool, tool.Name)
	}
}

func TestExplicitApprovalTargetCanonicalizesAlias(t *testing.T) {
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "scoped-sensitive-tool"},
		Spec: corev1alpha1.ToolSpec{
			Description: "sensitive",
			HTTP:        &corev1alpha1.HTTPExecution{URL: "http://example.invalid", Method: "POST"},
		},
	}
	call := llm.ToolCall{Arguments: json.RawMessage(`{
		"action":"run",
		"riskSummary":"risk",
		"severity":"warning",
		"targetTool":"sensitive_tool",
		"targetArguments":{}
	}`)}
	target, err := explicitApprovalTargetForCall(
		context.Background(),
		call,
		map[string]*corev1alpha1.Tool{"sensitive_tool": tool},
		&toolspkg.ToolContext{Namespace: "default", TaskID: "task", TaskUID: "uid"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if target.TargetTool != tool.Name {
		t.Fatalf("target tool = %q, want %q", target.TargetTool, tool.Name)
	}
}

func TestMalformedAliasedApprovalRecordsFailure(t *testing.T) {
	recorder := common.NewFakeEventRecorder()
	_, err := executeRequestApprovalToolCall(
		context.Background(),
		llm.ToolCall{ID: "call", Name: "request_approval", Arguments: json.RawMessage(`{`)},
		nil,
		recorder,
		nil,
	)
	if err == nil {
		t.Fatal("malformed approval call succeeded")
	}
	for _, event := range recorder.Events() {
		if event.Type == "ToolCallFailed" && event.ToolCallID == "call" {
			return
		}
	}
	t.Fatalf("failure event missing: %+v", recorder.Events())
}
