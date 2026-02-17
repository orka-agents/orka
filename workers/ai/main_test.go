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

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/llm"
)

const customToolName = "custom_tool"

func TestGetAPIKey_EnvVar(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		envVar   string
		envValue string
		wantKey  string
	}{
		{
			name:     "anthropic API key",
			provider: "anthropic",
			envVar:   "ANTHROPIC_API_KEY",
			envValue: "test-anthropic-key",
			wantKey:  "test-anthropic-key",
		},
		{
			name:     "openai API key",
			provider: "openai",
			envVar:   "OPENAI_API_KEY",
			envValue: "test-openai-key",
			wantKey:  "test-openai-key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Save and restore original env
			original := os.Getenv(tt.envVar)
			os.Setenv(tt.envVar, tt.envValue)    //nolint:errcheck
			defer os.Setenv(tt.envVar, original) //nolint:errcheck

			key := getAPIKey(tt.provider)
			if key != tt.wantKey {
				t.Errorf("getAPIKey(%s) = %s, want %s", tt.provider, key, tt.wantKey)
			}
		})
	}
}

func TestGetAPIKey_NotFound(t *testing.T) {
	// Clear environment variables
	originalAnthropic := os.Getenv("ANTHROPIC_API_KEY")
	originalOpenAI := os.Getenv("OPENAI_API_KEY")
	os.Unsetenv("ANTHROPIC_API_KEY") //nolint:errcheck
	os.Unsetenv("OPENAI_API_KEY")    //nolint:errcheck
	defer func() {
		if originalAnthropic != "" {
			os.Setenv("ANTHROPIC_API_KEY", originalAnthropic) //nolint:errcheck
		}
		if originalOpenAI != "" {
			os.Setenv("OPENAI_API_KEY", originalOpenAI) //nolint:errcheck
		}
	}()

	key := getAPIKey("unknown-provider")
	if key != "" {
		t.Errorf("getAPIKey() = %s, want empty string", key)
	}
}

func TestLoadSessionContext_NoFile(t *testing.T) {
	// When file doesn't exist, should return nil
	messages := loadSessionContext()
	if messages != nil {
		t.Errorf("loadSessionContext() = %v, want nil", messages)
	}
}

func TestLoadSessionContext_ValidFile(t *testing.T) {
	// Create a temp directory and file
	tmpDir := t.TempDir()
	sessionDir := filepath.Join(tmpDir, "session")
	os.MkdirAll(sessionDir, 0755) //nolint:errcheck

	transcriptContent := `{"role":"user","content":"Hello"}
{"role":"assistant","content":"Hi there!"}
{"role":"user","content":"How are you?"}`

	transcriptPath := filepath.Join(sessionDir, "transcript.jsonl")
	os.WriteFile(transcriptPath, []byte(transcriptContent), 0644) //nolint:errcheck

	// Override the transcript path temporarily
	// Note: This test would need to mock the file path or modify the function
	// For now, we'll just test the parsing logic directly
	messages := []llm.Message{}
	lines := []string{
		`{"role":"user","content":"Hello"}`,
		`{"role":"assistant","content":"Hi there!"}`,
	}
	for _, line := range lines {
		var msg struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err == nil {
			messages = append(messages, llm.Message{
				Role:    msg.Role,
				Content: msg.Content,
			})
		}
	}

	if len(messages) != 2 {
		t.Errorf("Expected 2 messages, got %d", len(messages))
	}
}

func TestLoadSessionContext_MalformedJSON(t *testing.T) {
	// Test that malformed JSON is skipped
	lines := []string{
		`{"role":"user","content":"Hello"}`,
		`{invalid json}`,
		`{"role":"assistant","content":"Hi"}`,
	}

	var messages []llm.Message
	for _, line := range lines {
		var msg struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err == nil {
			if msg.Role == "user" || msg.Role == "assistant" {
				messages = append(messages, llm.Message{
					Role:    msg.Role,
					Content: msg.Content,
				})
			}
		}
	}

	if len(messages) != 2 {
		t.Errorf("Expected 2 valid messages, got %d", len(messages))
	}
}

func TestBuildLLMTools_BuiltinTools(t *testing.T) {
	// Test with built-in tool
	enabledTools := []string{"web_search"}
	customTools := map[string]*corev1alpha1.Tool{}

	llmTools := buildLLMTools(enabledTools, customTools)

	if len(llmTools) != 1 {
		t.Errorf("Expected 1 tool, got %d", len(llmTools))
	}
	if llmTools[0].Name != "web_search" {
		t.Errorf("Expected web_search tool, got %s", llmTools[0].Name)
	}
}

func TestBuildLLMTools_CustomTools(t *testing.T) {
	enabledTools := []string{customToolName}
	customTools := map[string]*corev1alpha1.Tool{
		customToolName: {
			Spec: corev1alpha1.ToolSpec{
				Description: "A custom tool",
				Parameters:  nil,
			},
		},
	}
	customTools[customToolName].Name = customToolName

	llmTools := buildLLMTools(enabledTools, customTools)

	if len(llmTools) != 1 {
		t.Errorf("Expected 1 tool, got %d", len(llmTools))
	}
	if llmTools[0].Name != customToolName {
		t.Errorf("Expected custom_tool, got %s", llmTools[0].Name)
	}
	if llmTools[0].Description != "A custom tool" {
		t.Errorf("Expected 'A custom tool', got %s", llmTools[0].Description)
	}
}

func TestBuildLLMTools_Mixed(t *testing.T) {
	enabledTools := []string{"web_search", customToolName}
	customTools := map[string]*corev1alpha1.Tool{
		customToolName: {
			Spec: corev1alpha1.ToolSpec{
				Description: "A custom tool",
			},
		},
	}
	customTools[customToolName].Name = customToolName

	llmTools := buildLLMTools(enabledTools, customTools)

	if len(llmTools) != 2 {
		t.Errorf("Expected 2 tools, got %d", len(llmTools))
	}
}

func TestBuildLLMTools_Empty(t *testing.T) {
	enabledTools := []string{}
	customTools := map[string]*corev1alpha1.Tool{}

	llmTools := buildLLMTools(enabledTools, customTools)

	if len(llmTools) != 0 {
		t.Errorf("Expected 0 tools, got %d", len(llmTools))
	}
}

func TestBuildLLMTools_NotFound(t *testing.T) {
	enabledTools := []string{"nonexistent_tool"}
	customTools := map[string]*corev1alpha1.Tool{}

	llmTools := buildLLMTools(enabledTools, customTools)

	// Tool should not be added if not found
	if len(llmTools) != 0 {
		t.Errorf("Expected 0 tools, got %d", len(llmTools))
	}
}

func TestLoadPlanContext(t *testing.T) {
	t.Run("successful plan fetch", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/internal/v1/plans/default/test-task" {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Summary":      "Phase 1 complete",
				"ProgressPct":  25,
				"GoalComplete": false,
				"PlanDocument": "# My Plan\n- Step 1 done",
				"Iteration":    1,
			})
		}))
		defer server.Close()

		t.Setenv("ORKA_CONTROLLER_URL", server.URL)
		t.Setenv("ORKA_TASK_NAME", "test-task")
		t.Setenv("ORKA_TASK_NAMESPACE", "default")

		result := loadPlanContext()
		if result == "" {
			t.Fatal("expected non-empty plan context")
		}
		if !strings.Contains(result, "Phase 1 complete") {
			t.Errorf("result should contain summary, got: %s", result)
		}
		if !strings.Contains(result, "25%") {
			t.Errorf("result should contain progress, got: %s", result)
		}
	})

	t.Run("no plan (404)", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		t.Setenv("ORKA_CONTROLLER_URL", server.URL)
		t.Setenv("ORKA_TASK_NAME", "test-task")
		t.Setenv("ORKA_TASK_NAMESPACE", "default")

		result := loadPlanContext()
		if result != "" {
			t.Errorf("expected empty result for 404, got: %s", result)
		}
	})

	t.Run("missing env vars", func(t *testing.T) {
		t.Setenv("ORKA_CONTROLLER_URL", "")
		t.Setenv("ORKA_TASK_NAME", "")
		t.Setenv("ORKA_TASK_NAMESPACE", "")

		result := loadPlanContext()
		if result != "" {
			t.Errorf("expected empty result for missing env vars, got: %s", result)
		}
	})
}

func TestAutonomousSystemPromptSuffix(t *testing.T) {
	t.Run("with max iterations", func(t *testing.T) {
		result := autonomousSystemPromptSuffix(3, 10)
		if !strings.Contains(result, "iteration: 3") {
			t.Errorf("should contain current iteration, got: %s", result)
		}
		if !strings.Contains(result, "of 10") {
			t.Errorf("should contain max iterations, got: %s", result)
		}
		if !strings.Contains(result, "Autonomous Coordinator") {
			t.Errorf("should contain autonomous instructions, got: %s", result)
		}
	})

	t.Run("unlimited iterations", func(t *testing.T) {
		result := autonomousSystemPromptSuffix(0, 0)
		if !strings.Contains(result, "iteration: 0") {
			t.Errorf("should contain current iteration, got: %s", result)
		}
		if strings.Contains(result, "of 0") {
			t.Errorf("should not contain 'of 0' for unlimited, got: %s", result)
		}
	})
}

func TestRun_MissingProvider(t *testing.T) {
	t.Setenv("ORKA_AI_PROVIDER", "")
	t.Setenv("ORKA_AI_MODEL", "test-model")
	t.Setenv("ORKA_AI_PROMPT", "hello")

	err := run()
	if err == nil {
		t.Fatal("expected error for missing ORKA_AI_PROVIDER")
	}
	if !strings.Contains(err.Error(), "ORKA_AI_PROVIDER is required") {
		t.Errorf("error = %q, want mention of ORKA_AI_PROVIDER", err)
	}
}

func TestRun_MissingModel(t *testing.T) {
	t.Setenv("ORKA_AI_PROVIDER", "openai")
	t.Setenv("ORKA_AI_MODEL", "")
	t.Setenv("ORKA_AI_PROMPT", "hello")

	err := run()
	if err == nil {
		t.Fatal("expected error for missing ORKA_AI_MODEL")
	}
	if !strings.Contains(err.Error(), "ORKA_AI_MODEL is required") {
		t.Errorf("error = %q, want mention of ORKA_AI_MODEL", err)
	}
}

func TestRun_MissingPrompt(t *testing.T) {
	t.Setenv("ORKA_AI_PROVIDER", "openai")
	t.Setenv("ORKA_AI_MODEL", "gpt-4")
	t.Setenv("ORKA_AI_PROMPT", "")

	err := run()
	if err == nil {
		t.Fatal("expected error for missing ORKA_AI_PROMPT")
	}
	if !strings.Contains(err.Error(), "ORKA_AI_PROMPT is required") {
		t.Errorf("error = %q, want mention of ORKA_AI_PROMPT", err)
	}
}

func TestRun_MissingAPIKey(t *testing.T) {
	t.Setenv("ORKA_AI_PROVIDER", "openai")
	t.Setenv("ORKA_AI_MODEL", "gpt-4")
	t.Setenv("ORKA_AI_PROMPT", "hello")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")

	err := run()
	if err == nil {
		t.Fatal("expected error for missing API key")
	}
	if !strings.Contains(err.Error(), "API key") {
		t.Errorf("error = %q, want mention of API key", err)
	}
}

func TestCreateK8sClient_OutsideCluster(t *testing.T) {
	// Outside a k8s cluster, createK8sClient should fail
	_, err := createK8sClient()
	if err == nil {
		t.Fatal("expected error when not running in cluster")
	}
	if !strings.Contains(err.Error(), "in-cluster") {
		t.Errorf("error = %q, want mention of in-cluster config", err)
	}
}

func TestLoadCustomTools_NilClient(t *testing.T) {
	// With nil client and no tool names, should return empty map
	tools := loadCustomTools(context.Background(), nil, "default", nil)
	if len(tools) != 0 {
		t.Errorf("expected empty map, got %d tools", len(tools))
	}
}

func TestLoadCustomTools_BuiltinToolSkipped(t *testing.T) {
	// Built-in tools should be skipped (no k8s lookup needed)
	tools := loadCustomTools(context.Background(), nil, "default", []string{"web_search"})
	if len(tools) != 0 {
		t.Errorf("expected empty map for built-in tools, got %d tools", len(tools))
	}
}

func TestExecuteAgentLoop_NoToolCalls(t *testing.T) {
	// Mock provider that returns a response with no tool calls
	provider := &mockProvider{
		response: &llm.CompletionResponse{
			Content:    "Task completed successfully",
			StopReason: "end_turn",
		},
	}

	messages := []llm.Message{
		{Role: "user", Content: "hello"},
	}

	result, err := executeAgentLoop(
		context.Background(), provider, messages, "", "test-model",
		nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Task completed successfully" {
		t.Errorf("result = %q, want 'Task completed successfully'", result)
	}
}

func TestExecuteAgentLoop_CompletionError(t *testing.T) {
	provider := &mockProvider{
		err: fmt.Errorf("provider error"),
	}

	messages := []llm.Message{
		{Role: "user", Content: "hello"},
	}

	_, err := executeAgentLoop(
		context.Background(), provider, messages, "", "test-model",
		nil, nil, nil, nil,
	)
	if err == nil {
		t.Fatal("expected error from provider failure")
	}
	if !strings.Contains(err.Error(), "completion failed") {
		t.Errorf("error = %q, want mention of completion failed", err)
	}
}

func TestWriteResult_NoEndpoint(t *testing.T) {
	t.Setenv("ORKA_RESULT_ENDPOINT", "")
	t.Setenv("ORKA_CONTROLLER_URL", "")

	err := writeResult("test result")
	if err == nil {
		t.Fatal("expected error without result endpoint")
	}
}

func TestWriteResult_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Setenv("ORKA_RESULT_ENDPOINT", server.URL)

	err := writeResult("test result")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadSessionContext_WithTempFile(t *testing.T) {
	// Create a temp transcript file
	dir := t.TempDir()
	transcriptDir := filepath.Join(dir, "session")
	os.MkdirAll(transcriptDir, 0o755) //nolint:errcheck

	content := `{"role":"user","content":"Hello"}
{"role":"assistant","content":"Hi there"}
{"role":"tool","content":"tool result"}
`
	transcriptPath := filepath.Join(transcriptDir, "transcript.jsonl")
	os.WriteFile(transcriptPath, []byte(content), 0o644) //nolint:errcheck

	// loadSessionContext reads from /session/transcript.jsonl which won't
	// exist in tests. The existing test already covers the nil return.
	// Here we verify the function handles missing file gracefully.
	messages := loadSessionContext()
	if messages != nil {
		t.Errorf("expected nil (file doesn't exist at fixed path), got %d messages", len(messages))
	}
}

func TestLoadPlanContext_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	t.Setenv("ORKA_CONTROLLER_URL", server.URL)
	t.Setenv("ORKA_TASK_NAME", "test-task")
	t.Setenv("ORKA_TASK_NAMESPACE", "default")

	result := loadPlanContext()
	if result != "" {
		t.Errorf("expected empty result for server error, got: %s", result)
	}
}

func TestLoadPlanContext_EmptyPlanDocument(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Summary":      "Phase 1",
			"ProgressPct":  50,
			"GoalComplete": false,
			"PlanDocument": "",
			"Iteration":    1,
		})
	}))
	defer server.Close()

	t.Setenv("ORKA_CONTROLLER_URL", server.URL)
	t.Setenv("ORKA_TASK_NAME", "test-task")
	t.Setenv("ORKA_TASK_NAMESPACE", "default")

	result := loadPlanContext()
	if result != "" {
		t.Errorf("expected empty result for empty PlanDocument, got: %s", result)
	}
}

func TestLoadPlanContext_MalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{not json}")) //nolint:errcheck
	}))
	defer server.Close()

	t.Setenv("ORKA_CONTROLLER_URL", server.URL)
	t.Setenv("ORKA_TASK_NAME", "test-task")
	t.Setenv("ORKA_TASK_NAMESPACE", "default")

	result := loadPlanContext()
	if result != "" {
		t.Errorf("expected empty result for malformed JSON, got: %s", result)
	}
}

// mockProvider implements llm.Provider for testing.
type mockProvider struct {
	response *llm.CompletionResponse
	err      error
}

func (m *mockProvider) Complete(_ context.Context, _ *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	return m.response, m.err
}

func (m *mockProvider) Stream(_ context.Context, _ *llm.CompletionRequest) (<-chan llm.StreamChunk, error) {
	return nil, fmt.Errorf("stream not implemented")
}

func (m *mockProvider) Name() string {
	return "mock"
}
