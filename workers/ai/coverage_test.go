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
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/worker"
)

const testSessionDir = "/session"

// sequenceProvider returns different responses on successive calls.
type sequenceProvider struct {
	responses []*llm.CompletionResponse
	errors    []error
	callCount int
}

func (s *sequenceProvider) Complete(_ context.Context, _ *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	idx := s.callCount
	s.callCount++
	if idx < len(s.errors) && s.errors[idx] != nil {
		return nil, s.errors[idx]
	}
	if idx < len(s.responses) {
		return s.responses[idx], nil
	}
	return &llm.CompletionResponse{Content: "fallback", StopReason: "stop"}, nil
}

func (s *sequenceProvider) Stream(_ context.Context, _ *llm.CompletionRequest) (<-chan llm.StreamChunk, error) {
	return nil, fmt.Errorf("not implemented")
}

func (s *sequenceProvider) Name() string { return "sequence-mock" }

// --- run() tests ---

func TestRun_InvalidProviderType(t *testing.T) {
	t.Setenv("ORKA_AI_PROVIDER", "nonexistent-provider")
	t.Setenv("ORKA_AI_MODEL", "test-model")
	t.Setenv("ORKA_AI_PROMPT", "hello")
	// Set a generic API key env var that won't match any provider switch case
	// but will be found via secret file. Since no secret files exist, API key check
	// will fail first for an unknown provider.

	err := run()
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
	if !strings.Contains(err.Error(), "API key") {
		t.Errorf("error = %q, want mention of API key", err)
	}
}

func TestRun_ValidProviderHitsK8sError(t *testing.T) {
	t.Setenv("ORKA_AI_PROVIDER", "openai")
	t.Setenv("ORKA_AI_MODEL", "gpt-4")
	t.Setenv("ORKA_AI_PROMPT", "hello")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("ORKA_AI_BASE_URL", "http://localhost:9999")
	// Clear fallback vars
	t.Setenv("ORKA_AI_FALLBACK_COUNT", "")

	err := run()
	if err == nil {
		t.Fatal("expected error for k8s client creation")
	}
	if !strings.Contains(err.Error(), "k8s client") {
		t.Errorf("error = %q, want mention of k8s client", err)
	}
}

func TestRun_WithFallbackCountZero(t *testing.T) {
	t.Setenv("ORKA_AI_PROVIDER", "openai")
	t.Setenv("ORKA_AI_MODEL", "gpt-4")
	t.Setenv("ORKA_AI_PROMPT", "hello")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("ORKA_AI_FALLBACK_COUNT", "0")

	err := run()
	if err == nil {
		t.Fatal("expected error (no k8s)")
	}
	// Should get past fallback setup and fail at k8s client
	if !strings.Contains(err.Error(), "k8s client") {
		t.Errorf("error = %q, want mention of k8s client", err)
	}
}

func TestRun_WithFallbackMissingProviderKey(t *testing.T) {
	t.Setenv("ORKA_AI_PROVIDER", "openai")
	t.Setenv("ORKA_AI_MODEL", "gpt-4")
	t.Setenv("ORKA_AI_PROMPT", "hello")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("ORKA_AI_FALLBACK_COUNT", "1")
	// Fallback 0 missing provider/api-key
	t.Setenv("ORKA_AI_FALLBACK_0_PROVIDER", "")
	t.Setenv("ORKA_AI_FALLBACK_0_API_KEY", "")

	err := run()
	if err == nil {
		t.Fatal("expected error")
	}
	// Should skip the fallback and continue to k8s client error
	if !strings.Contains(err.Error(), "k8s client") {
		t.Errorf("error = %q, want mention of k8s client", err)
	}
}

func TestRun_WithFallbackInvalidProviderType(t *testing.T) {
	t.Setenv("ORKA_AI_PROVIDER", "openai")
	t.Setenv("ORKA_AI_MODEL", "gpt-4")
	t.Setenv("ORKA_AI_PROMPT", "hello")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("ORKA_AI_FALLBACK_COUNT", "1")
	t.Setenv("ORKA_AI_FALLBACK_0_PROVIDER", "unknown-fb-provider")
	t.Setenv("ORKA_AI_FALLBACK_0_API_KEY", "fb-key")

	err := run()
	if err == nil {
		t.Fatal("expected error")
	}
	// Invalid fallback provider is skipped with warning, then hits k8s client error
	if !strings.Contains(err.Error(), "k8s client") {
		t.Errorf("error = %q, want mention of k8s client", err)
	}
}

func TestRun_WithValidFallback(t *testing.T) {
	t.Setenv("ORKA_AI_PROVIDER", "openai")
	t.Setenv("ORKA_AI_MODEL", "gpt-4")
	t.Setenv("ORKA_AI_PROMPT", "hello")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("ORKA_AI_BASE_URL", "http://localhost:9999")
	t.Setenv("ORKA_AI_FALLBACK_COUNT", "1")
	t.Setenv("ORKA_AI_FALLBACK_0_PROVIDER", "openai")
	t.Setenv("ORKA_AI_FALLBACK_0_API_KEY", "fb-key")
	t.Setenv("ORKA_AI_FALLBACK_0_MODEL", "gpt-3.5")
	t.Setenv("ORKA_AI_FALLBACK_0_BASE_URL", "http://localhost:9998")
	t.Setenv("ORKA_AI_FALLBACK_0_AZURE_API_VERSION", "")

	err := run()
	if err == nil {
		t.Fatal("expected error")
	}
	// Valid fallback is created, then hits k8s client error
	if !strings.Contains(err.Error(), "k8s client") {
		t.Errorf("error = %q, want mention of k8s client", err)
	}
}

func TestRun_WithToolsString(t *testing.T) {
	t.Setenv("ORKA_AI_PROVIDER", "openai")
	t.Setenv("ORKA_AI_MODEL", "gpt-4")
	t.Setenv("ORKA_AI_PROMPT", "hello")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("ORKA_AI_TOOLS", "web_search,code_exec")
	t.Setenv("ORKA_AI_FALLBACK_COUNT", "")

	err := run()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "k8s client") {
		t.Errorf("error = %q, want k8s client error", err)
	}
}

func TestRun_AzureOpenAIProvider(t *testing.T) {
	t.Setenv("ORKA_AI_PROVIDER", "azure-openai")
	t.Setenv("ORKA_AI_MODEL", "gpt-4")
	t.Setenv("ORKA_AI_PROMPT", "hello")
	t.Setenv("OPENAI_API_KEY", "test-azure-key")
	t.Setenv("ORKA_AI_BASE_URL", "https://myresource.openai.azure.com")
	t.Setenv("ORKA_AI_AZURE_API_VERSION", "2024-02-01")
	t.Setenv("ORKA_AI_FALLBACK_COUNT", "")

	err := run()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "k8s client") {
		t.Errorf("error = %q, want k8s client error", err)
	}
}

func TestRun_FallbackCountNonNumeric(t *testing.T) {
	t.Setenv("ORKA_AI_PROVIDER", "openai")
	t.Setenv("ORKA_AI_MODEL", "gpt-4")
	t.Setenv("ORKA_AI_PROMPT", "hello")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("ORKA_AI_FALLBACK_COUNT", "abc")

	err := run()
	if err == nil {
		t.Fatal("expected error")
	}
	// Non-numeric fallback count parses to 0, so no fallbacks configured, hits k8s error
	if !strings.Contains(err.Error(), "k8s client") {
		t.Errorf("error = %q, want k8s client error", err)
	}
}

// --- getAPIKey tests ---

func TestGetAPIKey_AzureOpenAI(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "azure-key-123")
	key := getAPIKey("azure-openai")
	if key != "azure-key-123" {
		t.Errorf("getAPIKey(azure-openai) = %q, want %q", key, "azure-key-123")
	}
}

func TestGetAPIKey_SecretFile(t *testing.T) {
	// Clear env vars so it falls through to file-based lookup
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	// Create temp secret dirs (can't write to /secrets/ without root,
	// so this verifies the fallback returns empty when no files exist)
	key := getAPIKey("anthropic")
	if key != "" {
		t.Errorf("expected empty key when no env or secret files, got %q", key)
	}
}

// --- loadCustomTools tests ---

func newTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	return scheme
}

func TestLoadCustomTools_WithFakeClient(t *testing.T) {
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-custom-tool",
			Namespace: "default",
		},
		Spec: corev1alpha1.ToolSpec{
			Description: "A test tool",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithObjects(tool).
		Build()

	result := loadCustomTools(context.Background(), fakeClient, "default", []string{"my-custom-tool"})
	if len(result) != 1 {
		t.Fatalf("expected 1 custom tool, got %d", len(result))
	}
	if result["my-custom-tool"] == nil {
		t.Fatal("expected my-custom-tool in result")
	}
	if result["my-custom-tool"].Spec.Description != "A test tool" {
		t.Errorf("description = %q, want %q", result["my-custom-tool"].Spec.Description, "A test tool")
	}
}

func TestLoadCustomTools_ToolNotFound(t *testing.T) {
	fakeClient := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		Build()

	result := loadCustomTools(context.Background(), fakeClient, "default", []string{"nonexistent-tool"})
	if len(result) != 0 {
		t.Errorf("expected 0 tools for missing CRD, got %d", len(result))
	}
}

func TestLoadCustomTools_MixedBuiltinAndCustom(t *testing.T) {
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-tool",
			Namespace: "test-ns",
		},
		Spec: corev1alpha1.ToolSpec{
			Description: "custom",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithObjects(tool).
		Build()

	// web_search is built-in and should be skipped; my-tool is custom
	result := loadCustomTools(context.Background(), fakeClient, "test-ns", []string{"web_search", "my-tool"})
	if len(result) != 1 {
		t.Fatalf("expected 1 custom tool (builtin skipped), got %d", len(result))
	}
	if _, ok := result["my-tool"]; !ok {
		t.Error("expected my-tool in result")
	}
}

// --- buildLLMTools tests ---

func TestBuildLLMTools_CustomToolWithParameters(t *testing.T) {
	params := apiextensionsv1.JSON{Raw: []byte(`{"type":"object","properties":{"query":{"type":"string"}}}`)}
	customTools := map[string]*corev1alpha1.Tool{
		"parameterized": {
			Spec: corev1alpha1.ToolSpec{
				Description: "Tool with params",
				Parameters:  &params,
			},
		},
	}
	customTools["parameterized"].Name = "parameterized"

	llmTools := buildLLMTools([]string{"parameterized"}, customTools)
	if len(llmTools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(llmTools))
	}
	if !strings.Contains(string(llmTools[0].Parameters), "query") {
		t.Errorf("parameters should contain 'query', got %s", string(llmTools[0].Parameters))
	}
}

// --- executeAgentLoop tests ---

func TestExecuteAgentLoop_WithToolCalls(t *testing.T) {
	// First call returns tool calls, second call returns final response
	provider := &sequenceProvider{
		responses: []*llm.CompletionResponse{
			{
				Content: "",
				ToolCalls: []llm.ToolCall{
					{ID: "tc1", Name: "web_search", Arguments: json.RawMessage(`{"query":"test"}`)},
				},
				StopReason: "tool_use",
			},
			{
				Content:    "Here is the result",
				StopReason: "end_turn",
			},
		},
	}

	messages := []llm.Message{
		{Role: "user", Content: "search for test"},
	}

	result, err := executeAgentLoop(
		context.Background(), provider, messages, "system", "model",
		[]llm.Tool{{Name: "web_search"}}, nil, worker.NewToolExecutor(),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Here is the result" {
		t.Errorf("result = %q, want %q", result, "Here is the result")
	}
	if provider.callCount != 2 {
		t.Errorf("callCount = %d, want 2", provider.callCount)
	}
}

func TestExecuteAgentLoop_MaxIterationsReached(t *testing.T) {
	// Provider always returns tool calls - should hit max iterations
	provider := &mockProvider{
		response: &llm.CompletionResponse{
			Content: "",
			ToolCalls: []llm.ToolCall{
				{ID: "tc1", Name: "web_search", Arguments: json.RawMessage(`{"query":"test"}`)},
			},
			StopReason: "tool_use",
		},
	}

	messages := []llm.Message{
		{Role: "user", Content: "search forever"},
	}

	_, err := executeAgentLoop(
		context.Background(), provider, messages, "", "model",
		[]llm.Tool{{Name: "web_search"}}, nil, worker.NewToolExecutor(),
	)
	if err == nil {
		t.Fatal("expected error for max iterations")
	}
	if !strings.Contains(err.Error(), "max iterations") {
		t.Errorf("error = %q, want mention of max iterations", err)
	}
}

func TestExecuteAgentLoop_ContextTooLongRetry(t *testing.T) {
	// First call returns context-too-long error, retry succeeds
	provider := &sequenceProvider{
		responses: []*llm.CompletionResponse{
			nil,
			{Content: "truncated result", StopReason: "end_turn"},
		},
		errors: []error{
			&llm.ProviderError{StatusCode: 400, Message: "context too long"},
			nil,
		},
	}

	// Create messages with enough content to be truncatable
	messages := []llm.Message{
		{Role: "user", Content: strings.Repeat("hello world ", 100)},
		{Role: "assistant", Content: strings.Repeat("response ", 100)},
		{Role: "user", Content: "final question"},
	}

	result, err := executeAgentLoop(
		context.Background(), provider, messages, "", "model",
		nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "truncated result" {
		t.Errorf("result = %q, want %q", result, "truncated result")
	}
}

func TestExecuteAgentLoop_ContextTooLongRetryStillFails(t *testing.T) {
	// Both calls return context-too-long error
	ctxErr := &llm.ProviderError{StatusCode: 400, Message: "maximum token limit"}
	provider := &sequenceProvider{
		errors: []error{ctxErr, ctxErr},
	}

	messages := []llm.Message{
		{Role: "user", Content: strings.Repeat("x", 1000)},
	}

	_, err := executeAgentLoop(
		context.Background(), provider, messages, "", "model",
		nil, nil, nil,
	)
	if err == nil {
		t.Fatal("expected error when retry also fails")
	}
	if !strings.Contains(err.Error(), "completion failed") {
		t.Errorf("error = %q, want mention of completion failed", err)
	}
}

func TestExecuteAgentLoop_CoordinationEnabled(t *testing.T) {
	t.Setenv("ORKA_COORDINATION_ENABLED", "true")
	t.Setenv("ORKA_AUTONOMOUS_MODE", "")

	provider := &mockProvider{
		response: &llm.CompletionResponse{
			Content:    "done with coordination",
			StopReason: "end_turn",
		},
	}

	result, err := executeAgentLoop(
		context.Background(), provider,
		[]llm.Message{{Role: "user", Content: "hello"}},
		"", "model", nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "done with coordination" {
		t.Errorf("result = %q", result)
	}
}

func TestExecuteAgentLoop_AutonomousMode(t *testing.T) {
	t.Setenv("ORKA_COORDINATION_ENABLED", "")
	t.Setenv("ORKA_AUTONOMOUS_MODE", "true")

	provider := &mockProvider{
		response: &llm.CompletionResponse{
			Content:    "autonomous result",
			StopReason: "end_turn",
		},
	}

	result, err := executeAgentLoop(
		context.Background(), provider,
		[]llm.Message{{Role: "user", Content: "plan"}},
		"", "model", nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "autonomous result" {
		t.Errorf("result = %q", result)
	}
}

func TestExecuteAgentLoop_StopReasonStop(t *testing.T) {
	// Test "stop" stop reason (vs "end_turn")
	provider := &sequenceProvider{
		responses: []*llm.CompletionResponse{
			{
				Content: "",
				ToolCalls: []llm.ToolCall{
					{ID: "tc1", Name: "web_search", Arguments: json.RawMessage(`{"query":"test"}`)},
				},
				StopReason: "stop",
			},
		},
	}

	result, err := executeAgentLoop(
		context.Background(), provider,
		[]llm.Message{{Role: "user", Content: "search"}},
		"", "model",
		[]llm.Tool{{Name: "web_search"}}, nil, worker.NewToolExecutor(),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// When stop reason is "stop" and there are tool calls, the loop executes tools then returns
	_ = result
}

func TestExecuteAgentLoop_CustomToolExecution(t *testing.T) {
	// Set up a mock HTTP server for the custom tool
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"result": "custom tool output"}`) //nolint:errcheck
	}))
	defer toolServer.Close()

	customTool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "my-http-tool"},
		Spec: corev1alpha1.ToolSpec{
			Description: "A custom HTTP tool",
			HTTP: &corev1alpha1.HTTPExecution{
				URL:    toolServer.URL,
				Method: "POST",
			},
		},
	}

	customTools := map[string]*corev1alpha1.Tool{
		"my-http-tool": customTool,
	}

	provider := &sequenceProvider{
		responses: []*llm.CompletionResponse{
			{
				Content: "",
				ToolCalls: []llm.ToolCall{
					{ID: "tc1", Name: "my-http-tool", Arguments: json.RawMessage(`{"input":"test"}`)},
				},
				StopReason: "tool_use",
			},
			{
				Content:    "custom tool result processed",
				StopReason: "end_turn",
			},
		},
	}

	result, err := executeAgentLoop(
		context.Background(), provider,
		[]llm.Message{{Role: "user", Content: "use custom tool"}},
		"", "model",
		[]llm.Tool{{Name: "my-http-tool"}},
		customTools, worker.NewToolExecutor(),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "custom tool result processed" {
		t.Errorf("result = %q, want %q", result, "custom tool result processed")
	}
}

func TestExecuteAgentLoop_ToolExecutionError(t *testing.T) {
	// Test with a built-in tool that returns an error (unknown tool name)
	provider := &sequenceProvider{
		responses: []*llm.CompletionResponse{
			{
				Content: "",
				ToolCalls: []llm.ToolCall{
					{ID: "tc1", Name: "nonexistent_tool", Arguments: json.RawMessage(`{}`)},
				},
				StopReason: "tool_use",
			},
			{
				Content:    "handled error",
				StopReason: "end_turn",
			},
		},
	}

	result, err := executeAgentLoop(
		context.Background(), provider,
		[]llm.Message{{Role: "user", Content: "test"}},
		"", "model",
		[]llm.Tool{{Name: "nonexistent_tool"}},
		nil, worker.NewToolExecutor(),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "handled error" {
		t.Errorf("result = %q, want %q", result, "handled error")
	}
}

// --- loadSessionContext tests ---

func TestLoadSessionContext_CreatedFile(t *testing.T) {
	// The function reads from /session/transcript.jsonl (hardcoded path).
	// If we can create it, test full parsing; otherwise verify graceful handling.
	sessionDir := testSessionDir
	transcriptPath := filepath.Join(sessionDir, "transcript.jsonl")

	// Try to create the directory (may fail without root)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Skip("cannot create /session directory, skipping file-based test")
	}
	defer os.RemoveAll(sessionDir) //nolint:errcheck

	content := `{"role":"user","content":"Hello"}
{"role":"assistant","content":"Hi there"}
{"role":"system","content":"should be skipped"}
not valid json
{"role":"user","content":"Second question"}
`
	if err := os.WriteFile(transcriptPath, []byte(content), 0o644); err != nil {
		t.Skip("cannot write transcript file, skipping")
	}
	defer os.Remove(transcriptPath) //nolint:errcheck

	messages := loadSessionContext()
	if len(messages) != 3 {
		t.Errorf("expected 3 messages (user, assistant, user), got %d", len(messages))
	}
	if len(messages) > 0 && messages[0].Role != "user" {
		t.Errorf("first message role = %q, want %q", messages[0].Role, "user")
	}
	if len(messages) > 0 && messages[0].Content != "Hello" {
		t.Errorf("first message content = %q, want %q", messages[0].Content, "Hello")
	}
}

func TestLoadSessionContext_EmptyFile(t *testing.T) {
	sessionDir := testSessionDir
	transcriptPath := filepath.Join(sessionDir, "transcript.jsonl")

	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Skip("cannot create /session directory")
	}
	defer os.RemoveAll(sessionDir) //nolint:errcheck

	if err := os.WriteFile(transcriptPath, []byte(""), 0o644); err != nil {
		t.Skip("cannot write transcript file")
	}
	defer os.Remove(transcriptPath) //nolint:errcheck

	messages := loadSessionContext()
	if len(messages) != 0 {
		t.Errorf("expected 0 messages for empty file, got %d", len(messages))
	}
}

func TestLoadSessionContext_OnlyInvalidJSON(t *testing.T) {
	sessionDir := testSessionDir
	transcriptPath := filepath.Join(sessionDir, "transcript.jsonl")

	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Skip("cannot create /session directory")
	}
	defer os.RemoveAll(sessionDir) //nolint:errcheck

	if err := os.WriteFile(transcriptPath, []byte("bad json\nalso bad\n"), 0o644); err != nil {
		t.Skip("cannot write transcript file")
	}
	defer os.Remove(transcriptPath) //nolint:errcheck

	messages := loadSessionContext()
	if len(messages) != 0 {
		t.Errorf("expected 0 messages for invalid JSON, got %d", len(messages))
	}
}

// --- loadPlanContext tests ---

func TestLoadPlanContext_WithAuthHeader(t *testing.T) {
	var receivedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Summary":      "test",
			"ProgressPct":  50,
			"GoalComplete": false,
			"PlanDocument": "plan doc",
			"Iteration":    2,
		})
	}))
	defer server.Close()

	t.Setenv("ORKA_CONTROLLER_URL", server.URL)
	t.Setenv("ORKA_TASK_NAME", "t1")
	t.Setenv("ORKA_TASK_NAMESPACE", "ns1")

	// Create SA token file if possible
	saDir := "/var/run/secrets/kubernetes.io/serviceaccount"
	tokenPath := filepath.Join(saDir, "token")
	if err := os.MkdirAll(saDir, 0o755); err == nil {
		if err := os.WriteFile(tokenPath, []byte("test-sa-token"), 0o644); err == nil {
			defer os.Remove(tokenPath)                           //nolint:errcheck
			defer os.RemoveAll("/var/run/secrets/kubernetes.io") //nolint:errcheck
		}
	}

	result := loadPlanContext()
	if result == "" {
		t.Fatal("expected non-empty plan context")
	}
	if strings.Contains(result, "50%") && !strings.Contains(result, "plan doc") {
		t.Errorf("result missing plan document: %s", result)
	}

	// If SA token was written, verify auth header
	if _, err := os.Stat(tokenPath); err == nil {
		if receivedAuth != "Bearer test-sa-token" {
			t.Errorf("auth header = %q, want %q", receivedAuth, "Bearer test-sa-token")
		}
	}
}

func TestLoadPlanContext_MissingTaskName(t *testing.T) {
	t.Setenv("ORKA_CONTROLLER_URL", "http://localhost:9999")
	t.Setenv("ORKA_TASK_NAME", "")
	t.Setenv("ORKA_TASK_NAMESPACE", "default")

	result := loadPlanContext()
	if result != "" {
		t.Errorf("expected empty result when task name missing, got: %s", result)
	}
}

func TestLoadPlanContext_MissingTaskNamespace(t *testing.T) {
	t.Setenv("ORKA_CONTROLLER_URL", "http://localhost:9999")
	t.Setenv("ORKA_TASK_NAME", "task1")
	t.Setenv("ORKA_TASK_NAMESPACE", "")

	result := loadPlanContext()
	if result != "" {
		t.Errorf("expected empty result when namespace missing, got: %s", result)
	}
}

func TestLoadPlanContext_ConnectionRefused(t *testing.T) {
	t.Setenv("ORKA_CONTROLLER_URL", "http://127.0.0.1:1")
	t.Setenv("ORKA_TASK_NAME", "task1")
	t.Setenv("ORKA_TASK_NAMESPACE", "default")

	result := loadPlanContext()
	if result != "" {
		t.Errorf("expected empty result for connection refused, got: %s", result)
	}
}

func TestLoadPlanContext_RequestPathFormat(t *testing.T) {
	var capturedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	t.Setenv("ORKA_CONTROLLER_URL", server.URL)
	t.Setenv("ORKA_TASK_NAME", "my-task")
	t.Setenv("ORKA_TASK_NAMESPACE", "my-ns")

	_ = loadPlanContext()
	expected := "/internal/v1/plans/my-ns/my-task"
	if capturedPath != expected {
		t.Errorf("path = %q, want %q", capturedPath, expected)
	}
}

// --- writeResult tests ---

func TestWriteResult_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	t.Setenv("ORKA_RESULT_ENDPOINT", server.URL)

	err := writeResult("test result")
	if err == nil {
		t.Fatal("expected error for server error response")
	}
}

// --- Autonomous mode in run() ---

func TestRun_AutonomousModeEnvParsing(t *testing.T) {
	t.Setenv("ORKA_AI_PROVIDER", "openai")
	t.Setenv("ORKA_AI_MODEL", "gpt-4")
	t.Setenv("ORKA_AI_PROMPT", "hello")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("ORKA_AI_FALLBACK_COUNT", "")
	t.Setenv("ORKA_AUTONOMOUS_MODE", "true")
	t.Setenv("ORKA_AUTONOMOUS_ITERATION", "3")
	t.Setenv("ORKA_AUTONOMOUS_MAX_ITERATIONS", "10")
	t.Setenv("ORKA_COORDINATION_ENABLED", "")
	t.Setenv("ORKA_CONTROLLER_URL", "")

	err := run()
	if err == nil {
		t.Fatal("expected error")
	}
	// Should get through autonomous mode setup and fail at k8s client
	if !strings.Contains(err.Error(), "k8s client") {
		t.Errorf("error = %q, want k8s client error", err)
	}
}

func TestRun_AutonomousModeInvalidIteration(t *testing.T) {
	t.Setenv("ORKA_AI_PROVIDER", "openai")
	t.Setenv("ORKA_AI_MODEL", "gpt-4")
	t.Setenv("ORKA_AI_PROMPT", "hello")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("ORKA_AI_FALLBACK_COUNT", "")
	t.Setenv("ORKA_AUTONOMOUS_MODE", "true")
	t.Setenv("ORKA_AUTONOMOUS_ITERATION", "not-a-number")
	t.Setenv("ORKA_AUTONOMOUS_MAX_ITERATIONS", "also-not-a-number")
	t.Setenv("ORKA_CONTROLLER_URL", "")

	err := run()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "k8s client") {
		t.Errorf("error = %q, want k8s client error", err)
	}
}

func TestRun_CoordinationEnabled(t *testing.T) {
	t.Setenv("ORKA_AI_PROVIDER", "openai")
	t.Setenv("ORKA_AI_MODEL", "gpt-4")
	t.Setenv("ORKA_AI_PROMPT", "hello")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("ORKA_AI_FALLBACK_COUNT", "")
	t.Setenv("ORKA_COORDINATION_ENABLED", "true")
	t.Setenv("ORKA_AUTONOMOUS_MODE", "")

	err := run()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "k8s client") {
		t.Errorf("error = %q, want k8s client error", err)
	}
}

func TestRun_AutonomousModeWithPlanContext(t *testing.T) {
	planServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Summary":      "In progress",
			"ProgressPct":  50,
			"GoalComplete": false,
			"PlanDocument": "Step 1 done",
			"Iteration":    1,
		})
	}))
	defer planServer.Close()

	t.Setenv("ORKA_AI_PROVIDER", "openai")
	t.Setenv("ORKA_AI_MODEL", "gpt-4")
	t.Setenv("ORKA_AI_PROMPT", "continue work")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("ORKA_AI_FALLBACK_COUNT", "")
	t.Setenv("ORKA_AUTONOMOUS_MODE", "true")
	t.Setenv("ORKA_AUTONOMOUS_ITERATION", "2")
	t.Setenv("ORKA_AUTONOMOUS_MAX_ITERATIONS", "5")
	t.Setenv("ORKA_CONTROLLER_URL", planServer.URL)
	t.Setenv("ORKA_TASK_NAME", "t1")
	t.Setenv("ORKA_TASK_NAMESPACE", "ns1")

	err := run()
	if err == nil {
		t.Fatal("expected error")
	}
	// Exercises the planContext != "" branch and prompt augmentation
	if !strings.Contains(err.Error(), "k8s client") {
		t.Errorf("error = %q, want k8s client error", err)
	}
}

func TestLoadCustomToolsSkipsToolWhenOutboundApprovalBindingFails(t *testing.T) {
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "secured", Namespace: "default"},
		Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
			URL:                     "https://tools.example.test",
			OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "missing-policy"},
		}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(tool).Build()
	got := loadCustomTools(context.Background(), fakeClient, "default", []string{tool.Name})
	if len(got) != 0 {
		t.Fatalf("loaded tools = %#v, want fail-closed omission", got)
	}
}
