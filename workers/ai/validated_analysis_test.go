package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/worker"
	"github.com/orka-agents/orka/internal/workerenv"
)

const testValidationToolName = "validate-analysis-task"

func TestValidatedAnalysisResult(t *testing.T) {
	args := validAnalysisArguments()
	result, matched, err := validatedAnalysisResult(
		testValidationToolName,
		args,
		`{"gcs_bytes":123,"validation_token":"signed"}`,
	)
	if err != nil {
		t.Fatalf("validatedAnalysisResult() error = %v", err)
	}
	if !matched {
		t.Fatal("validatedAnalysisResult() did not recognize the validation tool")
	}
	var got validatedAnalysis
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("unmarshal final result: %v", err)
	}
	if got.GCSBytes == nil || *got.GCSBytes != 123 || got.ValidationToken != "signed" {
		t.Fatalf("final result = %+v", got)
	}
	if got.RootCause != "cause" || got.Summary != "summary" {
		t.Fatalf("validated fields changed: %+v", got)
	}
}

func TestExecuteAgentLoopFinalizesValidatedAnalysis(t *testing.T) {
	server := validationToolServer(t, http.StatusOK, `{"gcs_bytes":123,"validation_token":"signed"}`)
	defer server.Close()
	provider := &sequenceProvider{responses: []*llm.CompletionResponse{{
		ToolCalls:  []llm.ToolCall{{ID: "validate", Name: testValidationToolName, Arguments: validAnalysisArguments()}},
		StopReason: "tool_calls",
	}}}
	result, err := executeAgentLoop(
		context.Background(), provider, []llm.Message{{Role: "user", Content: "analyze"}}, "", "model",
		[]llm.Tool{{Name: testValidationToolName}}, validationCustomTools(server.URL), worker.NewToolExecutor(),
	)
	if err != nil {
		t.Fatalf("executeAgentLoop() error = %v", err)
	}
	if provider.callCount != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.callCount)
	}
	if !strings.Contains(result, `"validation_token":"signed"`) || !strings.Contains(result, `"gcs_bytes":123`) {
		t.Fatalf("result = %s", result)
	}
}

func TestExecuteAgentLoopRequiresValidation(t *testing.T) {
	server := validationToolServer(t, http.StatusOK, `{"gcs_bytes":123,"validation_token":"signed"}`)
	defer server.Close()
	provider := &sequenceProvider{responses: []*llm.CompletionResponse{
		{Content: `{"summary":"unvalidated"}`, StopReason: "end_turn"},
		{
			ToolCalls: []llm.ToolCall{{
				ID: "validate", Name: testValidationToolName, Arguments: validAnalysisArguments(),
			}},
			StopReason: "tool_calls",
		},
	}}
	result, err := executeAgentLoop(
		context.Background(), provider, []llm.Message{{Role: "user", Content: "analyze"}}, "", "model",
		[]llm.Tool{{Name: testValidationToolName}}, validationCustomTools(server.URL), worker.NewToolExecutor(),
	)
	if err != nil {
		t.Fatalf("executeAgentLoop() error = %v", err)
	}
	if provider.callCount != 2 {
		t.Fatalf("provider calls = %d, want 2", provider.callCount)
	}
	if !strings.Contains(result, `"validation_token":"signed"`) {
		t.Fatalf("result = %s", result)
	}
}

func TestExecuteAgentLoopStopsRepeatedValidationFailure(t *testing.T) {
	server := validationToolServer(t, http.StatusBadRequest, "analysis.root_cause is required")
	defer server.Close()
	provider := &mockProvider{response: &llm.CompletionResponse{
		ToolCalls:  []llm.ToolCall{{ID: "validate", Name: testValidationToolName, Arguments: validAnalysisArguments()}},
		StopReason: "tool_calls",
	}}
	_, err := executeAgentLoop(
		context.Background(), provider, []llm.Message{{Role: "user", Content: "analyze"}}, "", "model",
		[]llm.Tool{{Name: testValidationToolName}}, validationCustomTools(server.URL), worker.NewToolExecutor(),
	)
	if err == nil || !strings.Contains(err.Error(), "repeated the same failure 3 times") {
		t.Fatalf("executeAgentLoop() error = %v", err)
	}
}

func validAnalysisArguments() json.RawMessage {
	return json.RawMessage(`{
		"analysis": {
			"summary": "summary",
			"is_transient": false,
			"root_cause": "cause",
			"severity": "High",
			"suggested_fix": "fix",
			"relevant_files": []
		},
		"evidence_tokens": []
	}`)
}

func validationToolServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
}

func validationCustomTools(url string) map[string]*corev1alpha1.Tool {
	return map[string]*corev1alpha1.Tool{
		testValidationToolName: {
			ObjectMeta: metav1.ObjectMeta{Name: testValidationToolName},
			Spec: corev1alpha1.ToolSpec{
				Description: "validate analysis",
				HTTP:        &corev1alpha1.HTTPExecution{URL: url, Method: http.MethodPost},
			},
		},
	}
}

func TestAnalysisTransientStateRejectsMalformedAnalysis(t *testing.T) {
	_, analysisLike, err := analysisTransientState(`{"is_transient": tru}`)
	if !analysisLike || err == nil {
		t.Fatalf("analysisTransientState() = analysisLike %t, error %v", analysisLike, err)
	}
}

func TestAnalysisLoopGuardNarrowsFinalizationTools(t *testing.T) {
	guard := newAnalysisLoopGuard([]llm.Tool{
		{Name: "read-artifact-build"},
		{Name: "verify-timeline-build"},
		{Name: testValidationToolName},
	}, nil)
	req := &llm.CompletionRequest{Tools: []llm.Tool{
		{Name: "read-artifact-build"},
		{Name: "verify-timeline-build"},
		{Name: testValidationToolName},
	}}
	guard.prepareRequest(req, nil, analysisLoopMaxIterations-analysisValidationFocusRounds, analysisLoopMaxIterations)
	allowed := advertisedToolNames(req.Tools)
	if _, ok := allowed["read-artifact-build"]; ok {
		t.Fatal("finalization request still advertises artifact investigation tools")
	}
	if _, ok := allowed[testValidationToolName]; !ok {
		t.Fatal("finalization request omitted validate_analysis")
	}
}

func TestValidatedAnalysisCapsAutonomousIterations(t *testing.T) {
	coordination := workerenv.CoordinationEnv{Enabled: true, AutonomousMode: true}
	if got := agentLoopMaxIterations(coordination, true); got != analysisLoopMaxIterations {
		t.Fatalf("validated autonomous iterations = %d", got)
	}
	if got := agentLoopMaxIterations(coordination, false); got != 100 {
		t.Fatalf("ordinary autonomous iterations = %d", got)
	}
}
