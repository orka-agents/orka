/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/llm"

	_ "github.com/sozercan/orka/internal/llm/anthropic"
	_ "github.com/sozercan/orka/internal/llm/openai"
)

const (
	testHelloContent    = "Hello!"
	testToolNameSearch  = "search"
	testRoleUser        = "user"
	testRoleTool        = "tool"
	testRoleAssistant   = "assistant"
	testInvalidReqError = "invalid_request_error"
	testToolUseID       = "tu_1"
)

func setupTestAnthropicHandler(objs ...runtime.Object) (*AnthropicCompatHandler, *fiber.App) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	config := DefaultChatConfig()
	resolver := NewProviderResolver(fakeClient, config)
	handler := NewAnthropicCompatHandler(fakeClient, "default", false, config, resolver, nil)

	app := fiber.New()
	return handler, app
}

// --- Tests: parseAnthropicContent ---

func TestParseAnthropicContent(t *testing.T) {
	tests := []struct {
		name      string
		raw       json.RawMessage
		wantLen   int
		wantFirst string
		wantErr   bool
	}{
		{
			name:      "string content",
			raw:       json.RawMessage(`"hello world"`),
			wantLen:   1,
			wantFirst: "hello world",
		},
		{
			name:    "array of content blocks",
			raw:     json.RawMessage(`[{"type":"text","text":"first"},{"type":"text","text":"second"}]`),
			wantLen: 2,
		},
		{
			name:    "empty content",
			raw:     nil,
			wantLen: 0,
		},
		{
			name:    "empty raw message",
			raw:     json.RawMessage(``),
			wantLen: 0,
		},
		{
			name:    "invalid JSON",
			raw:     json.RawMessage(`{not valid`),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocks, err := parseAnthropicContent(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(blocks) != tt.wantLen {
				t.Fatalf("expected %d blocks, got %d", tt.wantLen, len(blocks))
			}
			if tt.wantFirst != "" && len(blocks) > 0 {
				if blocks[0].Text != tt.wantFirst {
					t.Errorf("expected first block text %q, got %q", tt.wantFirst, blocks[0].Text)
				}
				if blocks[0].Type != oaiContentTypeText {
					t.Errorf("expected first block type 'text', got %q", blocks[0].Type)
				}
			}
		})
	}
}

func TestParseAnthropicContent_ArrayDetails(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"hello"},{"type":"tool_use","id":"tu_1","name":"search","input":{"q":"test"}}]`)
	blocks, err := parseAnthropicContent(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0].Type != oaiContentTypeText || blocks[0].Text != "hello" {
		t.Errorf("block 0: type=%q text=%q", blocks[0].Type, blocks[0].Text)
	}
	if blocks[1].Type != oaiStopReasonToolUse || blocks[1].ID != testToolUseID || blocks[1].Name != testToolNameSearch {
		t.Errorf("block 1: type=%q id=%q name=%q", blocks[1].Type, blocks[1].ID, blocks[1].Name)
	}
}

// --- Tests: parseAnthropicSystem ---

func TestParseAnthropicSystem(t *testing.T) {
	tests := []struct {
		name    string
		raw     json.RawMessage
		want    string
		wantErr bool
	}{
		{
			name: "string system",
			raw:  json.RawMessage(`"You are a helpful assistant."`),
			want: "You are a helpful assistant.",
		},
		{
			name: "array of text blocks",
			raw:  json.RawMessage(`[{"type":"text","text":"First instruction."},{"type":"text","text":"Second instruction."}]`),
			want: "First instruction.\nSecond instruction.",
		},
		{
			name: "empty",
			raw:  nil,
			want: "",
		},
		{
			name: "empty raw message",
			raw:  json.RawMessage(``),
			want: "",
		},
		{
			name: "single text block array",
			raw:  json.RawMessage(`[{"type":"text","text":"Only one."}]`),
			want: "Only one.",
		},
		{
			name:    "invalid JSON",
			raw:     json.RawMessage(`{invalid`),
			wantErr: true,
		},
		{
			name: "array with non-text blocks ignored",
			raw:  json.RawMessage(`[{"type":"image","text":"ignored"},{"type":"text","text":"kept"}]`),
			want: "kept",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAnthropicSystem(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

// --- Tests: convertAnthropicMessages ---

func TestConvertAnthropicMessages(t *testing.T) { //nolint:gocyclo
	tests := []struct {
		name     string
		messages []AnthropicMessage
		wantLen  int
		check    func(t *testing.T, msgs []llm.Message)
	}{
		{
			name: "user text message",
			messages: []AnthropicMessage{
				{Role: "user", Content: json.RawMessage(`"Hello!"`)},
			},
			wantLen: 1,
			check: func(t *testing.T, msgs []llm.Message) {
				if msgs[0].Role != testRoleUser {
					t.Errorf("role = %q, want user", msgs[0].Role)
				}
				if msgs[0].Content != testHelloContent {
					t.Errorf("content = %q, want Hello!", msgs[0].Content)
				}
			},
		},
		{
			name: "user with tool_result blocks",
			messages: []AnthropicMessage{
				{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"tu_1","content":"result text"}]`)},
			},
			wantLen: 1,
			check: func(t *testing.T, msgs []llm.Message) {
				if msgs[0].Role != testRoleTool {
					t.Errorf("role = %q, want tool", msgs[0].Role)
				}
				if msgs[0].ToolCallID != testToolUseID {
					t.Errorf("tool_call_id = %q, want tu_1", msgs[0].ToolCallID)
				}
				if msgs[0].Content != "result text" {
					t.Errorf("content = %q, want 'result text'", msgs[0].Content)
				}
			},
		},
		{
			name: "user with tool_result with nested content blocks",
			messages: []AnthropicMessage{
				{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"tu_2","content":[{"type":"text","text":"line1"},{"type":"text","text":"line2"}]}]`)},
			},
			wantLen: 1,
			check: func(t *testing.T, msgs []llm.Message) {
				if msgs[0].Role != testRoleTool {
					t.Errorf("role = %q, want tool", msgs[0].Role)
				}
				if msgs[0].Content != "line1\nline2" {
					t.Errorf("content = %q, want 'line1\\nline2'", msgs[0].Content)
				}
			},
		},
		{
			name: "assistant with text blocks",
			messages: []AnthropicMessage{
				{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"I can help."}]`)},
			},
			wantLen: 1,
			check: func(t *testing.T, msgs []llm.Message) {
				if msgs[0].Role != testRoleAssistant {
					t.Errorf("role = %q, want assistant", msgs[0].Role)
				}
				if msgs[0].Content != "I can help." {
					t.Errorf("content = %q, want 'I can help.'", msgs[0].Content)
				}
			},
		},
		{
			name: "assistant with tool_use blocks",
			messages: []AnthropicMessage{
				{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"tu_1","name":"search","input":{"q":"test"}}]`)},
			},
			wantLen: 1,
			check: func(t *testing.T, msgs []llm.Message) {
				if msgs[0].Role != testRoleAssistant {
					t.Errorf("role = %q, want assistant", msgs[0].Role)
				}
				if len(msgs[0].ToolCalls) != 1 {
					t.Fatalf("expected 1 tool call, got %d", len(msgs[0].ToolCalls))
				}
				if msgs[0].ToolCalls[0].ID != testToolUseID {
					t.Errorf("tool call ID = %q, want tu_1", msgs[0].ToolCalls[0].ID)
				}
				if msgs[0].ToolCalls[0].Name != testToolNameSearch {
					t.Errorf("tool call name = %q, want search", msgs[0].ToolCalls[0].Name)
				}
			},
		},
		{
			name: "mixed assistant message text and tool_use",
			messages: []AnthropicMessage{
				{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"Let me search."},{"type":"tool_use","id":"tu_1","name":"search","input":{"q":"test"}}]`)},
			},
			wantLen: 1,
			check: func(t *testing.T, msgs []llm.Message) {
				if msgs[0].Content != "Let me search." {
					t.Errorf("content = %q, want 'Let me search.'", msgs[0].Content)
				}
				if len(msgs[0].ToolCalls) != 1 {
					t.Fatalf("expected 1 tool call, got %d", len(msgs[0].ToolCalls))
				}
				if msgs[0].ToolCalls[0].Name != testToolNameSearch {
					t.Errorf("tool call name = %q", msgs[0].ToolCalls[0].Name)
				}
			},
		},
		{
			name: "multiple messages in conversation",
			messages: []AnthropicMessage{
				{Role: "user", Content: json.RawMessage(`"Hello"`)},
				{Role: "assistant", Content: json.RawMessage(`"Hi there!"`)},
				{Role: "user", Content: json.RawMessage(`"How are you?"`)},
			},
			wantLen: 3,
			check: func(t *testing.T, msgs []llm.Message) {
				if msgs[0].Role != testRoleUser || msgs[0].Content != "Hello" {
					t.Errorf("msg 0: role=%q content=%q", msgs[0].Role, msgs[0].Content)
				}
				if msgs[1].Role != testRoleAssistant || msgs[1].Content != "Hi there!" {
					t.Errorf("msg 1: role=%q content=%q", msgs[1].Role, msgs[1].Content)
				}
				if msgs[2].Role != testRoleUser || msgs[2].Content != "How are you?" {
					t.Errorf("msg 2: role=%q content=%q", msgs[2].Role, msgs[2].Content)
				}
			},
		},
		{
			name: "user with mixed text and tool_result",
			messages: []AnthropicMessage{
				{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"Here is context."},{"type":"tool_result","tool_use_id":"tu_1","content":"tool output"}]`)},
			},
			wantLen: 2,
			check: func(t *testing.T, msgs []llm.Message) {
				// tool_result messages come first, then text
				toolMsg := msgs[0]
				textMsg := msgs[1]
				if toolMsg.Role != testRoleTool || toolMsg.ToolCallID != "tu_1" {
					t.Errorf("tool msg: role=%q id=%q", toolMsg.Role, toolMsg.ToolCallID)
				}
				if textMsg.Role != testRoleUser || textMsg.Content != "Here is context." {
					t.Errorf("text msg: role=%q content=%q", textMsg.Role, textMsg.Content)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs, err := convertAnthropicMessages(tt.messages)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(msgs) != tt.wantLen {
				t.Fatalf("expected %d messages, got %d", tt.wantLen, len(msgs))
			}
			if tt.check != nil {
				tt.check(t, msgs)
			}
		})
	}
}

func TestConvertAnthropicMessages_InvalidContent(t *testing.T) {
	msgs := []AnthropicMessage{
		{Role: "user", Content: json.RawMessage(`{invalid}`)},
	}
	_, err := convertAnthropicMessages(msgs)
	if err == nil {
		t.Fatal("expected error for invalid content")
	}
}

// --- Tests: convertAnthropicTools ---

func TestConvertAnthropicTools(t *testing.T) {
	tests := []struct {
		name    string
		tools   []AnthropicTool
		wantLen int
	}{
		{
			name: "single tool with input_schema",
			tools: []AnthropicTool{
				{
					Name:        "get_weather",
					Description: "Get weather info",
					InputSchema: json.RawMessage(`{"type":"object","properties":{"location":{"type":"string"}}}`),
				},
			},
			wantLen: 1,
		},
		{
			name: "multiple tools",
			tools: []AnthropicTool{
				{Name: "tool_a", Description: "First tool", InputSchema: json.RawMessage(`{"type":"object"}`)},
				{Name: "tool_b", Description: "Second tool", InputSchema: json.RawMessage(`{"type":"object"}`)},
			},
			wantLen: 2,
		},
		{
			name:    "empty tools",
			tools:   nil,
			wantLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := convertAnthropicTools(tt.tools)
			if tt.wantLen == 0 {
				if result != nil {
					t.Errorf("expected nil, got %v", result)
				}
				return
			}
			if len(result) != tt.wantLen {
				t.Fatalf("expected %d tools, got %d", tt.wantLen, len(result))
			}
		})
	}
}

func TestConvertAnthropicTools_FieldMapping(t *testing.T) {
	tools := []AnthropicTool{
		{
			Name:        "search",
			Description: "Search the web",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
		},
	}
	result := convertAnthropicTools(tools)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result))
	}
	if result[0].Name != testToolNameSearch {
		t.Errorf("name = %q, want search", result[0].Name)
	}
	if result[0].Description != "Search the web" {
		t.Errorf("description = %q", result[0].Description)
	}
	if string(result[0].Parameters) != string(tools[0].InputSchema) {
		t.Errorf("parameters = %s, want %s", result[0].Parameters, tools[0].InputSchema)
	}
}

// --- Tests: convertToAnthropicResponse ---

func TestConvertToAnthropicResponse(t *testing.T) {
	tests := []struct {
		name             string
		resp             *llm.CompletionResponse
		model            string
		wantContentLen   int
		wantStopReason   string
		wantInputTokens  int
		wantOutputTokens int
		checkContent     func(t *testing.T, content []AnthropicContentBlock)
	}{
		{
			name: "text-only response",
			resp: &llm.CompletionResponse{
				Content:      testHelloContent,
				StopReason:   "stop",
				InputTokens:  10,
				OutputTokens: 5,
			},
			model:            "claude-sonnet-4-20250514",
			wantContentLen:   1,
			wantStopReason:   "end_turn",
			wantInputTokens:  10,
			wantOutputTokens: 5,
			checkContent: func(t *testing.T, content []AnthropicContentBlock) {
				if content[0].Type != oaiContentTypeText {
					t.Errorf("type = %q, want text", content[0].Type)
				}
				if content[0].Text != testHelloContent {
					t.Errorf("text = %q, want Hello!", content[0].Text)
				}
			},
		},
		{
			name: "tool calls response",
			resp: &llm.CompletionResponse{
				StopReason: "tool_calls",
				ToolCalls: []llm.ToolCall{
					{ID: testToolUseID, Name: "search", Arguments: json.RawMessage(`{"q":"test"}`)},
				},
			},
			model:          "claude-sonnet-4-20250514",
			wantContentLen: 1,
			wantStopReason: oaiStopReasonToolUse,
			checkContent: func(t *testing.T, content []AnthropicContentBlock) {
				if content[0].Type != oaiStopReasonToolUse {
					t.Errorf("type = %q, want tool_use", content[0].Type)
				}
				if content[0].ID != testToolUseID {
					t.Errorf("id = %q, want tu_1", content[0].ID)
				}
				if content[0].Name != testToolNameSearch {
					t.Errorf("name = %q, want search", content[0].Name)
				}
			},
		},
		{
			name: "mixed text and tool calls",
			resp: &llm.CompletionResponse{
				Content:    "Let me search.",
				StopReason: oaiStopReasonToolUse,
				ToolCalls: []llm.ToolCall{
					{ID: testToolUseID, Name: "search", Arguments: json.RawMessage(`{"q":"test"}`)},
				},
			},
			model:          "claude-sonnet-4-20250514",
			wantContentLen: 2,
			wantStopReason: oaiStopReasonToolUse,
			checkContent: func(t *testing.T, content []AnthropicContentBlock) {
				if content[0].Type != oaiContentTypeText || content[0].Text != "Let me search." {
					t.Errorf("block 0: type=%q text=%q", content[0].Type, content[0].Text)
				}
				if content[1].Type != oaiStopReasonToolUse || content[1].Name != testToolNameSearch {
					t.Errorf("block 1: type=%q name=%q", content[1].Type, content[1].Name)
				}
			},
		},
		{
			name: "stop reason mapping - max_tokens",
			resp: &llm.CompletionResponse{
				Content:    "Truncated...",
				StopReason: "max_tokens",
			},
			model:          "claude-sonnet-4-20250514",
			wantContentLen: 1,
			wantStopReason: "max_tokens",
		},
		{
			name: "empty content with no tool calls",
			resp: &llm.CompletionResponse{
				StopReason: "stop",
			},
			model:          "claude-sonnet-4-20250514",
			wantContentLen: 0,
			wantStopReason: "end_turn",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := convertToAnthropicResponse(tt.resp, tt.model)

			if result.Type != "message" {
				t.Errorf("type = %q, want message", result.Type)
			}
			if result.Role != testRoleAssistant {
				t.Errorf("role = %q, want assistant", result.Role)
			}
			if result.Model != tt.model {
				t.Errorf("model = %q, want %q", result.Model, tt.model)
			}
			if !strings.HasPrefix(result.ID, "msg_") {
				t.Errorf("ID = %q, expected msg_ prefix", result.ID)
			}
			if len(result.Content) != tt.wantContentLen {
				t.Fatalf("content len = %d, want %d", len(result.Content), tt.wantContentLen)
			}
			if result.StopReason == nil {
				t.Fatal("stop_reason is nil")
			}
			if *result.StopReason != tt.wantStopReason {
				t.Errorf("stop_reason = %q, want %q", *result.StopReason, tt.wantStopReason)
			}
			if tt.wantInputTokens > 0 && result.Usage.InputTokens != tt.wantInputTokens {
				t.Errorf("input_tokens = %d, want %d", result.Usage.InputTokens, tt.wantInputTokens)
			}
			if tt.wantOutputTokens > 0 && result.Usage.OutputTokens != tt.wantOutputTokens {
				t.Errorf("output_tokens = %d, want %d", result.Usage.OutputTokens, tt.wantOutputTokens)
			}
			if tt.checkContent != nil {
				tt.checkContent(t, result.Content)
			}
		})
	}
}

// --- Tests: mapAnthropicStopReason ---

func TestMapAnthropicStopReason(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"stop", "end_turn"},
		{"end_turn", "end_turn"},
		{"", "end_turn"},
		{"tool_calls", oaiStopReasonToolUse},
		{oaiStopReasonToolUse, oaiStopReasonToolUse},
		{"max_tokens", "max_tokens"},
		{"length", "max_tokens"},
		{"unknown_reason", "end_turn"},
		{"STOP", "end_turn"},                 // case insensitive
		{"Tool_Calls", oaiStopReasonToolUse}, // case insensitive
		{"MAX_TOKENS", "max_tokens"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := mapAnthropicStopReason(tt.input)
			if got != tt.want {
				t.Errorf("mapAnthropicStopReason(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- Tests: anthropicError ---

func TestAnthropicError(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		errType    string
		message    string
		wantStatus int
	}{
		{
			name:       "400 invalid request",
			status:     400,
			errType:    testInvalidReqError,
			message:    "model is required",
			wantStatus: 400,
		},
		{
			name:       "500 api error",
			status:     500,
			errType:    "api_error",
			message:    "completion failed",
			wantStatus: 500,
		},
		{
			name:       "403 permission error",
			status:     403,
			errType:    "permission_error",
			message:    "namespace not allowed",
			wantStatus: 403,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := fiber.New()
			app.Get("/test", func(c fiber.Ctx) error {
				return anthropicError(c, tt.status, tt.errType, tt.message)
			})

			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}

			var errResp AnthropicError
			if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
				t.Fatalf("failed to decode response: %v", err)
			}
			if errResp.Type != "error" {
				t.Errorf("type = %q, want 'error'", errResp.Type)
			}
			if errResp.Error.Type != tt.errType {
				t.Errorf("error.type = %q, want %q", errResp.Error.Type, tt.errType)
			}
			if errResp.Error.Message != tt.message {
				t.Errorf("error.message = %q, want %q", errResp.Error.Message, tt.message)
			}
		})
	}
}

// --- Tests: HandleMessages validation ---

func TestHandleMessages_MissingModel(t *testing.T) {
	handler, app := setupTestAnthropicHandler()
	app.Post("/anthropic/v1/messages", handler.HandleMessages)

	body := `{"messages":[{"role":"user","content":"hello"}],"max_tokens":100}`
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}

	var errResp AnthropicError
	json.NewDecoder(resp.Body).Decode(&errResp) //nolint:errcheck
	if errResp.Error.Type != testInvalidReqError {
		t.Errorf("error type = %q", errResp.Error.Type)
	}
	if !strings.Contains(errResp.Error.Message, "model") {
		t.Errorf("error message = %q, expected mention of model", errResp.Error.Message)
	}
}

func TestHandleMessages_EmptyMessages(t *testing.T) {
	handler, app := setupTestAnthropicHandler()
	app.Post("/anthropic/v1/messages", handler.HandleMessages)

	body := `{"model":"claude-sonnet-4-20250514","messages":[],"max_tokens":100}`
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}

	var errResp AnthropicError
	json.NewDecoder(resp.Body).Decode(&errResp) //nolint:errcheck
	if !strings.Contains(errResp.Error.Message, "messages") {
		t.Errorf("error message = %q, expected mention of messages", errResp.Error.Message)
	}
}

func TestHandleMessages_MissingMaxTokens(t *testing.T) {
	handler, app := setupTestAnthropicHandler()
	app.Post("/anthropic/v1/messages", handler.HandleMessages)

	body := `{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}

	var errResp AnthropicError
	json.NewDecoder(resp.Body).Decode(&errResp) //nolint:errcheck
	if !strings.Contains(errResp.Error.Message, "max_tokens") {
		t.Errorf("error message = %q, expected mention of max_tokens", errResp.Error.Message)
	}
}

func TestHandleMessages_InvalidBody(t *testing.T) {
	handler, app := setupTestAnthropicHandler()
	app.Post("/anthropic/v1/messages", handler.HandleMessages)

	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestHandleMessages_NoProvider(t *testing.T) {
	handler, app := setupTestAnthropicHandler()
	app.Post("/anthropic/v1/messages", handler.HandleMessages)

	body := `{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hello"}],"max_tokens":100}`
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}

	var errResp AnthropicError
	json.NewDecoder(resp.Body).Decode(&errResp) //nolint:errcheck
	if !strings.Contains(errResp.Error.Message, "provider") {
		t.Errorf("error message = %q, expected mention of provider", errResp.Error.Message)
	}
}

func TestHandleMessages_ProviderSlashModel(t *testing.T) {
	mockAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid key"}}`))
	}))
	defer mockAPI.Close()

	provider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeAnthropic,
			DefaultModel: "claude-sonnet-4-20250514",
			BaseURL:      mockAPI.URL,
			SecretRef:    corev1alpha1.ProviderSecretRef{Name: "anthropic-secret", Key: "api-key"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic-secret", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("test-key")},
	}

	handler, app := setupTestAnthropicHandler(provider, secret)
	app.Post("/anthropic/v1/messages", handler.HandleMessages)

	body := `{"model":"anthropic/claude-sonnet-4-20250514","messages":[{"role":"user","content":"hello"}],"max_tokens":100}`
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should get past provider resolution (not 400)
	if resp.StatusCode == 400 {
		t.Errorf("did not expect 400; provider/model resolution should have succeeded")
	}
}

// --- Tests: HandleListModels ---

func TestAnthropicHandleListModels_Empty(t *testing.T) {
	handler, app := setupTestAnthropicHandler()
	app.Get("/anthropic/v1/models", handler.HandleListModels)

	req := httptest.NewRequest(http.MethodGet, "/anthropic/v1/models", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var modelList OAIModelList
	json.NewDecoder(resp.Body).Decode(&modelList) //nolint:errcheck
	if modelList.Object != "list" {
		t.Errorf("object = %q, want list", modelList.Object)
	}
	if len(modelList.Data) != 0 {
		t.Errorf("expected 0 models, got %d", len(modelList.Data))
	}
}

func TestAnthropicHandleListModels_WithProviders(t *testing.T) {
	provider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeAnthropic,
			DefaultModel: "claude-sonnet-4-20250514",
			SecretRef:    corev1alpha1.ProviderSecretRef{Name: "secret"},
		},
	}

	handler, app := setupTestAnthropicHandler(provider)
	app.Get("/anthropic/v1/models", handler.HandleListModels)

	req := httptest.NewRequest(http.MethodGet, "/anthropic/v1/models", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var modelList OAIModelList
	json.NewDecoder(resp.Body).Decode(&modelList) //nolint:errcheck
	if len(modelList.Data) != 2 {
		t.Fatalf("expected 2 models (prefixed and plain), got %d", len(modelList.Data))
	}

	ids := map[string]bool{}
	for _, m := range modelList.Data {
		ids[m.ID] = true
	}
	if !ids["anthropic/claude-sonnet-4-20250514"] {
		t.Error("expected model 'anthropic/claude-sonnet-4-20250514' not found")
	}
	if !ids["claude-sonnet-4-20250514"] {
		t.Error("expected model 'claude-sonnet-4-20250514' not found")
	}
}

func TestAnthropicCompat_ContextTokenAuthorizationFiltersListModels(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	allowedProvider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeAnthropic,
			DefaultModel: "claude-sonnet-4-20250514",
		},
	}
	disallowedModelProvider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic-haiku", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeAnthropic,
			DefaultModel: "claude-3-5-haiku-20241022",
		},
	}
	disallowedProvider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "openai", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeOpenAI,
			DefaultModel: "gpt-4o",
		},
	}
	handler, app := setupTestAnthropicHandler(allowedProvider, disallowedModelProvider, disallowedProvider)
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	if err != nil {
		t.Fatalf("NewContextTokenAuthorizationConfig returned error: %v", err)
	}
	handler.contextTokenAuthorization = authz

	app.Use(NewAuthMiddleware(handler.client, AuthConfig{ContextTokens: ctxTokenConfig}))
	app.Get("/anthropic/v1/models", handler.HandleListModels)

	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeProvidersUse,
		"tctx": map[string]any{
			"allowedProviders": []string{"anthropic", "anthropic-haiku"},
			"allowedModels":    []string{"claude-sonnet-4-20250514"},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/anthropic/v1/models", nil)
	req.Header.Set(KontxtHeaderName, token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var models OAIModelList
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	got := map[string]bool{}
	for _, model := range models.Data {
		got[model.ID] = true
	}
	for _, id := range []string{"anthropic/claude-sonnet-4-20250514", "claude-sonnet-4-20250514"} {
		if !got[id] {
			t.Fatalf("expected allowed model ID %q in response, got %#v", id, got)
		}
	}
	for _, id := range []string{"anthropic-haiku/claude-3-5-haiku-20241022", "claude-3-5-haiku-20241022", "openai/gpt-4o", "gpt-4o"} {
		if got[id] {
			t.Fatalf("did not expect disallowed model ID %q in response: %#v", id, got)
		}
	}
}

// --- Mock provider for tool loop tests ---

type mockAnthropicProvider struct {
	responses []*llm.CompletionResponse
	errors    []error
	callIdx   int
}

func (m *mockAnthropicProvider) Complete(_ context.Context, _ *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	if m.callIdx >= len(m.responses) {
		return nil, fmt.Errorf("unexpected call %d", m.callIdx)
	}
	idx := m.callIdx
	m.callIdx++
	if m.errors != nil && idx < len(m.errors) && m.errors[idx] != nil {
		return nil, m.errors[idx]
	}
	return m.responses[idx], nil
}

func (m *mockAnthropicProvider) Stream(_ context.Context, _ *llm.CompletionRequest) (<-chan llm.StreamChunk, error) {
	return nil, fmt.Errorf("streaming not supported by mock")
}

func (m *mockAnthropicProvider) Name() string {
	return "mock-anthropic"
}

// --- Tests: injectOrkaTools ---

func TestInjectOrkaTools_BuiltinTools(t *testing.T) {
	handler, _ := setupTestAnthropicHandler()
	req := &llm.CompletionRequest{}
	injectOrkaTools(context.Background(), handler.client, req, "default")

	if len(req.Tools) < len(builtinProxyTools) {
		t.Fatalf("expected at least %d tools, got %d", len(builtinProxyTools), len(req.Tools))
	}

	names := map[string]bool{}
	for _, tool := range req.Tools {
		names[tool.Name] = true
	}
	for _, expected := range builtinProxyTools {
		if !names[expected] {
			t.Errorf("expected built-in tool %q not found", expected)
		}
	}
}

func TestInjectOrkaTools_PreservesClientTools(t *testing.T) {
	handler, _ := setupTestAnthropicHandler()
	clientTool := llm.Tool{
		Name:        "my_custom_tool",
		Description: "A client-provided tool",
		Parameters:  json.RawMessage(`{"type":"object"}`),
	}
	req := &llm.CompletionRequest{Tools: []llm.Tool{clientTool}}

	injectOrkaTools(context.Background(), handler.client, req, "default")

	// Client tool should still be first
	if req.Tools[0].Name != "my_custom_tool" {
		t.Errorf("first tool = %q, want my_custom_tool", req.Tools[0].Name)
	}
	// Built-in tools should also be present
	names := map[string]bool{}
	for _, tool := range req.Tools {
		names[tool.Name] = true
	}
	for _, expected := range builtinProxyTools {
		if !names[expected] {
			t.Errorf("expected built-in tool %q not found after injection", expected)
		}
	}
	// Total should be client + builtins
	if len(req.Tools) < 1+len(builtinProxyTools) {
		t.Errorf("expected at least %d tools, got %d", 1+len(builtinProxyTools), len(req.Tools))
	}
}

func TestInjectOrkaTools_WithToolCRDs(t *testing.T) {
	toolCRD := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-tool", Namespace: "default"},
		Spec: corev1alpha1.ToolSpec{
			Description: "A custom tool",
			Parameters:  &apiextensionsv1.JSON{Raw: json.RawMessage(`{"type":"object"}`)},
			HTTP:        corev1alpha1.HTTPExecution{URL: "http://example.com/tool"},
		},
	}

	handler, _ := setupTestAnthropicHandler(toolCRD)
	req := &llm.CompletionRequest{}
	injectOrkaTools(context.Background(), handler.client, req, "default")

	names := map[string]bool{}
	for _, tool := range req.Tools {
		names[tool.Name] = true
	}
	if !names["custom-tool"] {
		t.Error("expected Tool CRD 'custom-tool' not found in injected tools")
	}
	for _, expected := range builtinProxyTools {
		if !names[expected] {
			t.Errorf("expected built-in tool %q not found", expected)
		}
	}
}

// --- Tests: runNonStreamingToolLoop ---

func TestRunNonStreamingToolLoop_NoToolCalls(t *testing.T) {
	_, _ = setupTestAnthropicHandler()
	mock := &mockAnthropicProvider{
		responses: []*llm.CompletionResponse{
			{Content: testHelloContent, StopReason: "end_turn"},
		},
	}
	req := &llm.CompletionRequest{
		Model:    "test-model",
		Messages: []llm.Message{{Role: "user", Content: "Hi"}},
	}

	resp, err := runNonStreamingToolLoop(context.Background(), mock, req, "test-model", ChatConfig{MaxIterations: 20, ToolTimeout: 30 * time.Second}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != testHelloContent {
		t.Errorf("content = %q, want Hello!", resp.Content)
	}
	if mock.callIdx != 1 {
		t.Errorf("expected 1 LLM call, got %d", mock.callIdx)
	}
}

func TestRunNonStreamingToolLoop_SingleToolCall(t *testing.T) {
	_, _ = setupTestAnthropicHandler()
	mock := &mockAnthropicProvider{
		responses: []*llm.CompletionResponse{
			{
				Content:    "Let me read the file.",
				StopReason: oaiStopReasonToolUse,
				ToolCalls: []llm.ToolCall{
					{ID: "tc_1", Name: "file_read", Arguments: json.RawMessage(`{"path":"test.txt"}`)},
				},
			},
			{Content: "The file contains: hello world", StopReason: "end_turn"},
		},
	}
	req := &llm.CompletionRequest{
		Model:    "test-model",
		Messages: []llm.Message{{Role: "user", Content: "Read test.txt"}},
	}

	resp, err := runNonStreamingToolLoop(context.Background(), mock, req, "test-model", ChatConfig{MaxIterations: 20, ToolTimeout: 30 * time.Second}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "The file contains: hello world" {
		t.Errorf("content = %q, want final response", resp.Content)
	}
	if mock.callIdx != 2 {
		t.Errorf("expected 2 LLM calls, got %d", mock.callIdx)
	}
}

func TestRunNonStreamingToolLoop_MultiStepToolLoop(t *testing.T) {
	_, _ = setupTestAnthropicHandler()
	mock := &mockAnthropicProvider{
		responses: []*llm.CompletionResponse{
			{
				StopReason: oaiStopReasonToolUse,
				ToolCalls:  []llm.ToolCall{{ID: "tc_1", Name: "web_search", Arguments: json.RawMessage(`{"query":"test"}`)}},
			},
			{
				StopReason: oaiStopReasonToolUse,
				ToolCalls:  []llm.ToolCall{{ID: "tc_2", Name: "file_read", Arguments: json.RawMessage(`{"path":"result.txt"}`)}},
			},
			{Content: "Here is the final answer.", StopReason: "end_turn"},
		},
	}
	req := &llm.CompletionRequest{
		Model:    "test-model",
		Messages: []llm.Message{{Role: "user", Content: "Search and read"}},
	}

	resp, err := runNonStreamingToolLoop(context.Background(), mock, req, "test-model", ChatConfig{MaxIterations: 20, ToolTimeout: 30 * time.Second}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "Here is the final answer." {
		t.Errorf("content = %q, want final answer", resp.Content)
	}
	if mock.callIdx != 3 {
		t.Errorf("expected 3 LLM calls, got %d", mock.callIdx)
	}
}

func TestRunNonStreamingToolLoop_IterationLimit(t *testing.T) {
	config := DefaultChatConfig()
	config.MaxIterations = 2

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	_ = NewAnthropicCompatHandler(fakeClient, "default", false, config, NewProviderResolver(fakeClient, config), nil)

	mock := &mockAnthropicProvider{
		responses: []*llm.CompletionResponse{
			// Iteration 0: tool call
			{StopReason: oaiStopReasonToolUse, ToolCalls: []llm.ToolCall{{ID: "tc_1", Name: "web_search", Arguments: json.RawMessage(`{"query":"a"}`)}}},
			// Iteration 1: tool call
			{StopReason: oaiStopReasonToolUse, ToolCalls: []llm.ToolCall{{ID: "tc_2", Name: "web_search", Arguments: json.RawMessage(`{"query":"b"}`)}}},
			// Iteration 2: hits limit, summary call
			{Content: "Summary of work done.", StopReason: "end_turn"},
		},
	}
	req := &llm.CompletionRequest{
		Model:    "test-model",
		Messages: []llm.Message{{Role: "user", Content: "Do many things"}},
	}

	resp, err := runNonStreamingToolLoop(context.Background(), mock, req, "test-model", ChatConfig{MaxIterations: 20, ToolTimeout: 30 * time.Second}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "Summary of work done." {
		t.Errorf("content = %q, want summary", resp.Content)
	}
	if mock.callIdx != 3 {
		t.Errorf("expected 3 LLM calls (2 iterations + 1 summary), got %d", mock.callIdx)
	}
}

func TestRunNonStreamingToolLoop_LLMError(t *testing.T) {
	_, _ = setupTestAnthropicHandler()
	mock := &mockAnthropicProvider{
		responses: []*llm.CompletionResponse{nil},
		errors:    []error{fmt.Errorf("provider unavailable")},
	}
	req := &llm.CompletionRequest{
		Model:    "test-model",
		Messages: []llm.Message{{Role: "user", Content: "Hi"}},
	}

	_, err := runNonStreamingToolLoop(context.Background(), mock, req, "test-model", ChatConfig{MaxIterations: 20, ToolTimeout: 30 * time.Second}, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "provider unavailable") {
		t.Errorf("error = %q, want 'provider unavailable'", err.Error())
	}
}

func TestRunNonStreamingToolLoop_ToolExecutionError(t *testing.T) {
	_, _ = setupTestAnthropicHandler()
	mock := &mockAnthropicProvider{
		responses: []*llm.CompletionResponse{
			{
				StopReason: oaiStopReasonToolUse,
				ToolCalls:  []llm.ToolCall{{ID: "tc_1", Name: "nonexistent_tool", Arguments: json.RawMessage(`{}`)}},
			},
			{Content: "I encountered an error but here is my response.", StopReason: "end_turn"},
		},
	}
	req := &llm.CompletionRequest{
		Model:    "test-model",
		Messages: []llm.Message{{Role: "user", Content: "Use a bad tool"}},
	}

	resp, err := runNonStreamingToolLoop(context.Background(), mock, req, "test-model", ChatConfig{MaxIterations: 20, ToolTimeout: 30 * time.Second}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "I encountered an error but here is my response." {
		t.Errorf("content = %q, want error recovery response", resp.Content)
	}
	if mock.callIdx != 2 {
		t.Errorf("expected 2 LLM calls, got %d", mock.callIdx)
	}
}

// --- Tests: executeToolCall ---

func TestExecuteToolCall_UnknownTool(t *testing.T) {
	result := executeToolCall(context.Background(), llm.ToolCall{
		ID:        "tc_1",
		Name:      "nonexistent_tool",
		Arguments: json.RawMessage(`{}`),
	}, 10*time.Second, nil)

	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if parsed["success"] != false {
		t.Errorf("expected success=false, got %v", parsed["success"])
	}
	errMsg, ok := parsed["error"].(string)
	if !ok || errMsg == "" {
		t.Errorf("expected non-empty error message, got %v", parsed["error"])
	}
}

func TestExecuteToolCall_Timeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	// Use an unknown tool so the registry returns an error, validating the timeout/error path
	result := executeToolCall(ctx, llm.ToolCall{
		ID:        "tc_1",
		Name:      "nonexistent_timeout_tool",
		Arguments: json.RawMessage(`{}`),
	}, 1*time.Millisecond, nil)

	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if parsed["success"] != false {
		t.Errorf("expected success=false, got %v", parsed["success"])
	}
}

func TestAnthropicCompat_ContextTokenAuthorizationRejectsDisallowedProvider(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	llmProvider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeAnthropic,
			DefaultModel: "claude-sonnet-4-20250514",
			SecretRef:    corev1alpha1.ProviderSecretRef{Name: "anthropic-secret", Key: "api-key"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic-secret", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("test-key")},
	}
	handler, app := setupTestAnthropicHandler(llmProvider, secret)
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	if err != nil {
		t.Fatalf("NewContextTokenAuthorizationConfig returned error: %v", err)
	}
	handler.contextTokenAuthorization = authz

	app.Use(NewAuthMiddleware(handler.client, AuthConfig{ContextTokens: ctxTokenConfig}))
	app.Post("/anthropic/v1/messages", handler.HandleMessages)

	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeProvidersUse + " " + ContextTokenScopeToolsUse,
		"tctx": map[string]any{
			"allowedProviders": []string{"openai"},
		},
	})
	body := `{"model":"anthropic/claude-sonnet-4-20250514","messages":[{"role":"user","content":"hello"}],"max_tokens":100}`
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set(KontxtHeaderName, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}
