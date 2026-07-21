package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/worker"
)

func TestAnalysisLoopGuardSelectsOneToolCall(t *testing.T) {
	guard := newAnalysisLoopGuard([]llm.Tool{
		{Name: "submit_analysis"},
		{Name: "verify_timeline"},
	}, nil)
	calls := []llm.ToolCall{
		{Name: "read_artifact"},
		{Name: "submit_analysis"},
		{Name: "verify_timeline"},
	}
	got := guard.selectToolCalls(calls)
	if len(got) != 1 || got[0].Name != "verify_timeline" {
		t.Fatalf("selected calls = %+v", got)
	}
	got = guard.selectToolCalls(calls[:2])
	if len(got) != 1 || got[0].Name != "submit_analysis" {
		t.Fatalf("selected calls = %+v", got)
	}
}

func TestAnalysisLoopGuardUsesToolCallBudget(t *testing.T) {
	guard := newAnalysisLoopGuard([]llm.Tool{
		{Name: "read_artifact"},
		{Name: "submit_analysis"},
	}, nil)
	for range analysisMaxInvestigationToolCalls {
		if err := guard.beginToolCall("read_artifact"); err != nil {
			t.Fatal(err)
		}
	}
	if err := guard.beginToolCall("read_artifact"); err == nil {
		t.Fatal("tool-call budget allowed an extra investigation call")
	}
	req := &llm.CompletionRequest{Tools: []llm.Tool{
		{Name: "read_artifact"},
		{Name: "submit_analysis"},
	}}
	guard.prepareRequest(req, nil, 1, analysisLoopMaxIterations)
	if len(req.Tools) != 1 || req.Tools[0].Name != "submit_analysis" {
		t.Fatalf("budgeted tools = %+v", req.Tools)
	}
}

func TestAnalysisLoopGuardCachesDuplicateToolCall(t *testing.T) {
	guard := newAnalysisLoopGuard([]llm.Tool{{Name: "submit_analysis"}}, nil)
	first := json.RawMessage(`{"path":"build-log.txt","offset":0}`)
	second := json.RawMessage(`{"offset":0,"path":"build-log.txt"}`)
	tool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{toolCacheIdenticalCallsAnnotation: "true"},
	}}
	guard.rememberToolResult("read_artifact", first, "evidence", tool)
	got, ok := guard.cachedToolResult("read_artifact", second, tool)
	if !ok || got != "evidence" {
		t.Fatalf("cached result = %q, %t", got, ok)
	}
}

func TestValidatedAnalysisResultSupportsFlatSubmission(t *testing.T) {
	args := json.RawMessage(`{
		"summary":"summary",
		"is_transient":false,
		"root_cause":"cause",
		"severity":"High",
		"suggested_fix":"fix",
		"relevant_files":[]
	}`)
	result, matched, err := validatedAnalysisResult(
		"submit_analysis",
		args,
		`{"gcs_bytes":123,"validation_token":"signed"}`,
	)
	if err != nil || !matched {
		t.Fatalf("validatedAnalysisResult() = %q, %t, %v", result, matched, err)
	}
	var got validatedAnalysis
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("unmarshal final result: %v", err)
	}
	if got.GCSBytes == nil || *got.GCSBytes != 123 || got.ValidationToken != "signed" {
		t.Fatalf("final result = %+v", got)
	}
}

func TestToolCallFingerprintDoesNotCanonicalizeTrailingData(t *testing.T) {
	valid := toolCallFingerprint("read_artifact", json.RawMessage(`{"path":"x"}`))
	malformed := toolCallFingerprint("read_artifact", json.RawMessage(`{"path":"x"}junk`))
	if valid == malformed {
		t.Fatal("malformed arguments reused the valid JSON fingerprint")
	}
}

func TestCachedToolResultDoesNotBypassRequestAllowlist(t *testing.T) {
	guard := newAnalysisLoopGuard([]llm.Tool{{Name: "submit_analysis"}}, nil)
	tool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{toolCacheIdenticalCallsAnnotation: "true"},
	}}
	args := json.RawMessage(`{"path":"build-log.txt"}`)
	guard.rememberToolResult("read_artifact", args, "cached", tool)
	_, cached, _, err := executeGuardedLoopTool(
		context.Background(),
		llm.ToolCall{Name: "read_artifact", Arguments: args},
		"read_artifact",
		map[string]struct{}{"submit_analysis": {}},
		map[string]*corev1alpha1.Tool{"read_artifact": tool},
		worker.NewToolExecutor(),
		&approvalGate{firedKeys: map[string]bool{}},
		nil,
		guard,
	)
	if cached || err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("cached=%t error=%v", cached, err)
	}
}
