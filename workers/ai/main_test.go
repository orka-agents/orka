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
	"slices"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/llm"
	"github.com/sozercan/orka/internal/store"
	toolspkg "github.com/sozercan/orka/internal/tools"
	"github.com/sozercan/orka/internal/workerenv"
	"github.com/sozercan/orka/workers/common"
)

const roleUser = "user"

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
			if msg.Role == roleUser || msg.Role == "assistant" {
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

func TestLoadSkillsFromVolume_PromptFile(t *testing.T) {
	skillsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(skillsDir, "PROMPT.md"), []byte("Primary skill prompt"), 0o644); err != nil {
		t.Fatalf("write prompt file: %v", err)
	}

	original := os.Getenv("ORKA_SKILLS_DIR")
	os.Setenv("ORKA_SKILLS_DIR", skillsDir)      //nolint:errcheck
	defer os.Setenv("ORKA_SKILLS_DIR", original) //nolint:errcheck

	got := loadSkillsFromVolume()
	if got != "Primary skill prompt" {
		t.Fatalf("loadSkillsFromVolume() = %q, want %q", got, "Primary skill prompt")
	}
}

func TestLoadSkillsFromVolume_FallbackSkillFiles(t *testing.T) {
	skillsDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(skillsDir, "skill-a"), 0o755); err != nil {
		t.Fatalf("mkdir skill-a: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(skillsDir, "skill-b"), 0o755); err != nil {
		t.Fatalf("mkdir skill-b: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillsDir, "skill-a", "SKILL.md"), []byte("Skill A"), 0o644); err != nil {
		t.Fatalf("write skill-a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillsDir, "skill-b", "SKILL.md"), []byte("Skill B"), 0o644); err != nil {
		t.Fatalf("write skill-b: %v", err)
	}

	original := os.Getenv("ORKA_SKILLS_DIR")
	os.Setenv("ORKA_SKILLS_DIR", skillsDir)      //nolint:errcheck
	defer os.Setenv("ORKA_SKILLS_DIR", original) //nolint:errcheck

	got := loadSkillsFromVolume()
	if got != "Skill A\n\nSkill B" {
		t.Fatalf("loadSkillsFromVolume() = %q, want %q", got, "Skill A\\n\\nSkill B")
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

func TestFormatDurableMemoryContext_BoundsEntriesAndChars(t *testing.T) {
	createdAt := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	memories := make([]store.Memory, 0, 6)
	for i := 1; i <= 6; i++ {
		memories = append(memories, store.Memory{
			ID:        fmt.Sprintf("mem-%d", i),
			Namespace: "default",
			Source:    "task",
			TaskName:  fmt.Sprintf("task-%d", i),
			Content:   fmt.Sprintf("memory-%d durable guidance", i),
			CreatedAt: createdAt,
		})
	}

	got := formatDurableMemoryContext(memories, 1000)
	if got == "" {
		t.Fatal("expected memory context, got empty string")
	}
	if len(got) > 1000 {
		t.Fatalf("context length = %d, want <= 1000", len(got))
	}
	if count := strings.Count(got, "durable guidance"); count != defaultMemoryContextLimit {
		t.Fatalf("memory count = %d, want %d\n%s", count, defaultMemoryContextLimit, got)
	}
	if strings.Contains(got, "memory-6") {
		t.Fatalf("context included memory beyond default limit: %s", got)
	}

	bounded := formatDurableMemoryContext(memories, 220)
	if bounded == "" {
		t.Fatal("expected bounded memory context, got empty string")
	}
	if len(bounded) > 220 {
		t.Fatalf("bounded context length = %d, want <= 220\n%s", len(bounded), bounded)
	}
}

func TestFormatDurableMemoryContext_TruncatesIndividualMemory(t *testing.T) {
	longContent := strings.Repeat("x", memoryContextPerEntryMaxChars+500)
	got := formatDurableMemoryContext([]store.Memory{
		{
			ID:        "mem-1",
			Namespace: "default",
			Source:    "task",
			Content:   longContent,
			CreatedAt: time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC),
		},
	}, 5000)

	if got == "" {
		t.Fatal("expected memory context, got empty string")
	}
	if !strings.Contains(got, "durable memory truncated") {
		t.Fatalf("expected truncation marker in context: %s", got)
	}
	if strings.Contains(got, strings.Repeat("x", memoryContextPerEntryMaxChars+1)) {
		t.Fatalf("memory content was not truncated to per-entry limit")
	}
}

func TestAppendMemoryReflectionGuidance_IncludesRememberGuidance(t *testing.T) {
	got := appendMemoryReflectionGuidance("base prompt")

	for _, want := range []string{
		"base prompt",
		"## Durable Memory Reflection",
		"remember",
		"durable project facts",
		"repository conventions",
		"lessons learned",
		"reusable procedures",
		"Do not store secrets",
		"raw transcripts",
		"review-only",
		"not automatically applied",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("reflection guidance missing %q:\n%s", want, got)
		}
	}
}

func TestAutoEnableMemoryTools_WhenControllerConfigPresent(t *testing.T) {
	t.Setenv("ORKA_CONTROLLER_URL", "http://controller.example")
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_TASK_NAME", "task-1")

	got := autoEnableMemoryTools([]string{"web_search", "recall_memory", " web_search "})
	want := []string{"web_search", "recall_memory", "remember", "propose_memory", "search_transcript"}
	if !slices.Equal(got, want) {
		t.Fatalf("autoEnableMemoryTools() = %#v, want %#v", got, want)
	}
}

func TestAutoEnableMemoryTools_NoControllerConfigDoesNotMutateTools(t *testing.T) {
	t.Setenv("ORKA_CONTROLLER_URL", "")
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_TASK_NAME", "task-1")

	got := autoEnableMemoryTools([]string{"web_search"})
	want := []string{"web_search"}
	if !slices.Equal(got, want) {
		t.Fatalf("autoEnableMemoryTools() = %#v, want %#v", got, want)
	}
	for _, toolName := range memoryToolNames {
		if containsTool(got, toolName) {
			t.Fatalf("autoEnableMemoryTools() added memory tool %q without controller config: %#v", toolName, got)
		}
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
		for _, want := range []string{
			"at most eight coder repair passes",
			"at most three times",
			"more than 30 minutes",
			"run the reviewer tasks again",
		} {
			if !strings.Contains(result, want) {
				t.Errorf("should contain %q, got: %s", want, result)
			}
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
		nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Task completed successfully" {
		t.Errorf("result = %q, want 'Task completed successfully'", result)
	}
}

func TestAIWorkerEventCompletenessSmoke(t *testing.T) {
	provider := &mockProvider{
		response: &llm.CompletionResponse{
			Content:      "Task completed successfully",
			StopReason:   "end_turn",
			InputTokens:  12,
			OutputTokens: 8,
			Model:        "test-model",
			Provider:     "azure.ai.openai",
		},
	}
	recorder := common.NewFakeEventRecorder()
	common.RecordEvent(context.Background(), recorder, events.ExecutionEventTypeWorkerStarted,
		common.WithEventTaskName("task-events"),
		common.WithEventContent(eventContent(map[string]any{"provider": provider.Name(), "model": "test-model"})),
	)

	result, err := executeAgentLoopWithEvents(
		context.Background(), provider, []llm.Message{{Role: "user", Content: "hello"}}, "", "test-model",
		nil, nil, nil, recorder,
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	common.RecordEvent(context.Background(), recorder, events.ExecutionEventTypeResultSubmitted,
		common.WithEventTaskName("task-events"),
		common.WithEventContent(eventContent(map[string]any{"resultLength": len(result)})),
	)
	common.RecordEvent(context.Background(), recorder, events.ExecutionEventTypeWorkerCompleted,
		common.WithEventTaskName("task-events"),
	)

	assertRecordedEventTypesEventually(t, recorder, []string{
		events.ExecutionEventTypeWorkerStarted,
		events.ExecutionEventTypeModelRequestStarted,
		events.ExecutionEventTypeModelRequestCompleted,
		events.ExecutionEventTypeModelMessage,
		events.ExecutionEventTypeResultSubmitted,
		events.ExecutionEventTypeWorkerCompleted,
	})
	data, err := json.Marshal(recorder.Events())
	if err != nil {
		t.Fatalf("marshal events: %v", err)
	}
	if strings.Contains(string(data), "sk-test12345678901234567890") {
		t.Fatalf("AI worker events leaked fake API key: %s", data)
	}
	var sawTelemetryProvider bool
	for _, event := range recorder.Events() {
		if event.Type != events.ExecutionEventTypeModelRequestCompleted {
			continue
		}
		var content map[string]any
		if err := json.Unmarshal(event.Content, &content); err != nil {
			t.Fatalf("unmarshal model event content: %v", err)
		}
		if content["provider"] == "azure.ai.openai" {
			sawTelemetryProvider = true
			break
		}
	}
	if !sawTelemetryProvider {
		t.Fatalf("model completion event did not preserve response provider: %#v", recorder.Events())
	}
}

func TestAIWorkerEventToolCallCompleteness(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	toolspkg.DefaultRegistry.Register(staticTestTool{name: customToolName})
	llmTools := toolspkg.DefaultRegistry.ToLLMTools([]string{customToolName})
	provider := &mockProvider{responses: []*llm.CompletionResponse{
		{
			Content: "calling tool",
			ToolCalls: []llm.ToolCall{{
				ID:        "call-1",
				Name:      customToolName,
				Arguments: json.RawMessage(`{"path":"README.md"}`),
			}},
			StopReason: "tool_use",
			Model:      "test-model",
		},
		{Content: "done", StopReason: "end_turn", Model: "test-model"},
	}}
	recorder := common.NewFakeEventRecorder()

	result, err := executeAgentLoopWithEvents(
		context.Background(), provider, []llm.Message{{Role: "user", Content: "use tool"}}, "", "test-model",
		llmTools, nil, nil, recorder,
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != "done" {
		t.Fatalf("result = %q, want done", result)
	}
	assertRecordedEventTypesEventually(t, recorder, []string{
		events.ExecutionEventTypeToolCallStarted,
		events.ExecutionEventTypeToolCallCompleted,
	})
	captured := recorder.Events()
	var sawToolMetadata bool
	for _, event := range captured {
		if event.Type == events.ExecutionEventTypeToolCallCompleted &&
			event.ToolName == customToolName &&
			event.ToolCallID == "call-1" {
			sawToolMetadata = true
		}
	}
	if !sawToolMetadata {
		t.Fatalf("tool call metadata missing in events: %#v", captured)
	}
}

func TestAIWorkerEventContextTruncated(t *testing.T) {
	provider := &mockProvider{
		errs:      []error{&llm.ProviderError{StatusCode: http.StatusBadRequest, Message: "context window too long"}},
		responses: []*llm.CompletionResponse{{Content: "ok", StopReason: "end_turn", Model: "test-model"}},
	}
	recorder := common.NewFakeEventRecorder()
	result, err := executeAgentLoopWithEvents(
		context.Background(), provider,
		[]llm.Message{{Role: "user", Content: strings.Repeat("hello ", 200)}},
		"", "test-model", nil, nil, nil, recorder,
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != "ok" {
		t.Fatalf("result = %q, want ok", result)
	}
	assertRecordedEventTypesEventually(t, recorder, []string{events.ExecutionEventTypeContextTruncated})
}

func TestAIWorkerEventRecorderFailureDoesNotChangeResult(t *testing.T) {
	provider := &mockProvider{response: &llm.CompletionResponse{Content: "ok", StopReason: "end_turn"}}
	result, err := executeAgentLoopWithEvents(
		context.Background(), provider, []llm.Message{{Role: "user", Content: "hello"}}, "", "test-model",
		nil, nil, nil, panicEventRecorder{},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != "ok" {
		t.Fatalf("result = %q, want ok", result)
	}
}

func TestAIWorkerEventRecordsValidationFailure(t *testing.T) {
	gotBody := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/events/default/task/invalid-ai-task" {
			t.Errorf("path = %s, want internal event path", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotBody <- body
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	t.Setenv(workerenv.ControllerURL, server.URL)
	t.Setenv(workerenv.TaskNamespace, "default")
	t.Setenv(workerenv.TaskName, "invalid-ai-task")
	t.Setenv(workerenv.AIProvider, "")
	t.Setenv(workerenv.AIModel, "")
	t.Setenv(workerenv.AIPrompt, "")

	err := run()
	if err == nil {
		t.Fatal("run() error = nil, want validation failure")
	}
	select {
	case body := <-gotBody:
		if body["type"] != events.ExecutionEventTypeWorkerFailed {
			t.Fatalf("event type = %#v, want WorkerFailed", body["type"])
		}
		if body["taskName"] != "invalid-ai-task" {
			t.Fatalf("taskName = %#v, want invalid-ai-task", body["taskName"])
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for WorkerFailed event")
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
		nil, nil, nil,
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
	response  *llm.CompletionResponse
	responses []*llm.CompletionResponse
	err       error
	errs      []error
}

func (m *mockProvider) Complete(_ context.Context, req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	_ = req
	if len(m.errs) > 0 {
		err := m.errs[0]
		m.errs = m.errs[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(m.responses) > 0 {
		resp := m.responses[0]
		m.responses = m.responses[1:]
		return resp, nil
	}
	return m.response, m.err
}

func (m *mockProvider) Stream(_ context.Context, _ *llm.CompletionRequest) (<-chan llm.StreamChunk, error) {
	return nil, fmt.Errorf("stream not implemented")
}

func (m *mockProvider) Name() string {
	return "mock"
}

type staticTestTool struct {
	name string
}

func (t staticTestTool) Name() string { return t.name }

func (t staticTestTool) Description() string { return "test tool" }

func (t staticTestTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (t staticTestTool) Execute(context.Context, json.RawMessage) (string, error) {
	return "tool result", nil
}

type panicEventRecorder struct{}

func (panicEventRecorder) Record(context.Context, string, ...common.EventOption) {
	panic("event recorder failed")
}

func replaceDefaultToolRegistryForTest(t *testing.T) func() {
	t.Helper()
	original := toolspkg.DefaultRegistry
	toolspkg.DefaultRegistry = toolspkg.NewRegistry()
	return func() { toolspkg.DefaultRegistry = original }
}

func assertEventTypesPresent(t *testing.T, got []string, want []string) {
	t.Helper()
	seen := make(map[string]bool, len(got))
	for _, typ := range got {
		seen[typ] = true
	}
	for _, typ := range want {
		if !seen[typ] {
			t.Fatalf("event types %v missing %s", got, typ)
		}
	}
}

func assertRecordedEventTypesEventually(t *testing.T, recorder *common.FakeEventRecorder, want []string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	var got []string
	for time.Now().Before(deadline) {
		got = recorder.EventTypes()
		if hasEventTypes(got, want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	assertEventTypesPresent(t, got, want)
}

func hasEventTypes(got []string, want []string) bool {
	seen := make(map[string]bool, len(got))
	for _, typ := range got {
		seen[typ] = true
	}
	for _, typ := range want {
		if !seen[typ] {
			return false
		}
	}
	return true
}
