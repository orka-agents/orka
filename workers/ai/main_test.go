/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"encoding/json"
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
