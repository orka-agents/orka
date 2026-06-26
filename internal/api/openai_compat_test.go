/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/llm"

	// Register LLM providers for integration tests
	_ "github.com/sozercan/orka/internal/llm/anthropic"
	_ "github.com/sozercan/orka/internal/llm/openai"
)

func setupTestOpenAIHandler(objs ...runtime.Object) (*OpenAICompatHandler, *fiber.App) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	config := DefaultChatConfig()
	resolver := NewProviderResolver(fakeClient, config)
	handler := NewOpenAICompatHandler(fakeClient, "default", false, config, resolver, nil)

	app := fiber.New()
	return handler, app
}

func TestHandleChatCompletions_MissingMessages(t *testing.T) {
	handler, app := setupTestOpenAIHandler()
	app.Post("/openai/v1/chat/completions", handler.HandleChatCompletions)

	body := `{"model":"test-model","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.StatusCode != 400 {
		t.Errorf("expected status 400, got %d", resp.StatusCode)
	}

	var oaiErr OAIError
	json.NewDecoder(resp.Body).Decode(&oaiErr) //nolint:errcheck
	if oaiErr.Error.Type != "invalid_request_error" {
		t.Errorf("expected error type invalid_request_error, got %s", oaiErr.Error.Type)
	}
}

func TestHandleChatCompletions_NGreaterThanOne(t *testing.T) {
	handler, app := setupTestOpenAIHandler()
	app.Post("/openai/v1/chat/completions", handler.HandleChatCompletions)

	body := `{"model":"test-model","messages":[{"role":"user","content":"hello"}],"n":2}`
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.StatusCode != 400 {
		t.Errorf("expected status 400, got %d", resp.StatusCode)
	}

	var oaiErr OAIError
	json.NewDecoder(resp.Body).Decode(&oaiErr) //nolint:errcheck
	if oaiErr.Error.Type != "invalid_request_error" {
		t.Errorf("expected error type invalid_request_error, got %s", oaiErr.Error.Type)
	}
	if oaiErr.Error.Param == nil || *oaiErr.Error.Param != "n" {
		t.Errorf("expected error param 'n', got %v", oaiErr.Error.Param)
	}
}

func TestHandleChatCompletions_InvalidBody(t *testing.T) {
	handler, app := setupTestOpenAIHandler()
	app.Post("/openai/v1/chat/completions", handler.HandleChatCompletions)

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.StatusCode != 400 {
		t.Errorf("expected status 400, got %d", resp.StatusCode)
	}
}

func TestHandleChatCompletions_NoProvider(t *testing.T) {
	handler, app := setupTestOpenAIHandler()
	app.Post("/openai/v1/chat/completions", handler.HandleChatCompletions)

	body := `{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.StatusCode != 400 {
		t.Errorf("expected status 400, got %d", resp.StatusCode)
	}

	var oaiErr OAIError
	json.NewDecoder(resp.Body).Decode(&oaiErr) //nolint:errcheck
	if !strings.Contains(oaiErr.Error.Message, "provider") {
		t.Errorf("expected error about provider, got: %s", oaiErr.Error.Message)
	}
}

func TestHandleListModels_Empty(t *testing.T) {
	handler, app := setupTestOpenAIHandler()
	app.Get("/openai/v1/models", handler.HandleListModels)

	req := httptest.NewRequest(http.MethodGet, "/openai/v1/models", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	var modelList OAIModelList
	json.NewDecoder(resp.Body).Decode(&modelList) //nolint:errcheck
	if modelList.Object != "list" {
		t.Errorf("expected object 'list', got %s", modelList.Object)
	}
	if len(modelList.Data) != 0 {
		t.Errorf("expected 0 models, got %d", len(modelList.Data))
	}
}

func TestHandleListModels_WithProviders(t *testing.T) {
	provider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "anthropic",
			Namespace: "default",
		},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeAnthropic,
			DefaultModel: "claude-sonnet-4-20250514",
			SecretRef: corev1alpha1.ProviderSecretRef{
				Name: "anthropic-secret",
			},
		},
	}

	handler, app := setupTestOpenAIHandler(provider)
	app.Get("/openai/v1/models", handler.HandleListModels)

	req := httptest.NewRequest(http.MethodGet, "/openai/v1/models", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	var modelList OAIModelList
	json.NewDecoder(resp.Body).Decode(&modelList) //nolint:errcheck
	if len(modelList.Data) != 2 {
		t.Fatalf("expected 2 models (prefixed and plain), got %d", len(modelList.Data))
	}

	// Check we have both "anthropic/claude-sonnet-4-20250514" and "claude-sonnet-4-20250514"
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

func TestConvertOAIMessages(t *testing.T) {
	tests := []struct {
		name          string
		messages      []OAIMessage
		wantMsgCount  int
		wantSystem    string
		wantFirstRole string
		wantFirstText string
	}{
		{
			name: "simple user message",
			messages: []OAIMessage{
				{Role: "user", Content: "hello"},
			},
			wantMsgCount:  1,
			wantSystem:    "",
			wantFirstRole: "user",
			wantFirstText: "hello",
		},
		{
			name: "system + user messages",
			messages: []OAIMessage{
				{Role: "system", Content: "You are helpful."},
				{Role: "user", Content: "hi"},
			},
			wantMsgCount:  1,
			wantSystem:    "You are helpful.",
			wantFirstRole: "user",
			wantFirstText: "hi",
		},
		{
			name: "assistant with tool calls",
			messages: []OAIMessage{
				{Role: "user", Content: "use the tool"},
				{Role: "assistant", Content: "", ToolCalls: []OAIToolCall{
					{ID: "call_1", Type: "function", Function: OAIFunctionCall{Name: "read_file", Arguments: `{"path":"foo.txt"}`}},
				}},
				{Role: "tool", Content: "file contents here", ToolCallID: "call_1", Name: "read_file"},
			},
			wantMsgCount:  3,
			wantSystem:    "",
			wantFirstRole: "user",
			wantFirstText: "use the tool",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs, system := convertOAIMessages(tt.messages)
			if len(msgs) != tt.wantMsgCount {
				t.Errorf("expected %d messages, got %d", tt.wantMsgCount, len(msgs))
			}
			if system != tt.wantSystem {
				t.Errorf("expected system %q, got %q", tt.wantSystem, system)
			}
			if len(msgs) > 0 {
				if msgs[0].Role != tt.wantFirstRole {
					t.Errorf("expected first role %q, got %q", tt.wantFirstRole, msgs[0].Role)
				}
				if msgs[0].Content != tt.wantFirstText {
					t.Errorf("expected first text %q, got %q", tt.wantFirstText, msgs[0].Content)
				}
			}
		})
	}
}

func TestConvertOAITools(t *testing.T) {
	tools := []OAITool{
		{
			Type: "function",
			Function: OAIFunctionDef{
				Name:        "get_weather",
				Description: "Get the weather",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"location":{"type":"string"}}}`),
			},
		},
		{
			Type: "not_function",
			Function: OAIFunctionDef{
				Name: "should_be_skipped",
			},
		},
	}

	result := convertOAITools(tools)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result))
	}
	if result[0].Name != "get_weather" {
		t.Errorf("expected tool name 'get_weather', got %s", result[0].Name)
	}
	if result[0].Description != "Get the weather" {
		t.Errorf("expected description 'Get the weather', got %s", result[0].Description)
	}
}

func TestConvertOAITools_Empty(t *testing.T) {
	result := convertOAITools(nil)
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

func TestMapFinishReason(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"end_turn", "stop"},
		{"stop", "stop"},
		{"", "stop"},
		{"tool_use", "tool_calls"},
		{"tool_calls", "tool_calls"},
		{oaiParamMaxTokens, "length"},
		{"length", "length"},
		{"content_filter", "content_filter"},
		{"unknown", "stop"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := mapFinishReason(tt.input)
			if got != tt.want {
				t.Errorf("mapFinishReason(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestExtractContent(t *testing.T) {
	tests := []struct {
		name  string
		input any
		want  string
	}{
		{"nil", nil, ""},
		{"string", "hello", "hello"},
		{"text parts", []any{
			map[string]any{"type": "text", "text": "hello"},
			map[string]any{"type": "text", "text": " world"},
		}, "hello\n world"},
		{"image part ignored", []any{
			map[string]any{"type": "image_url", "image_url": "data:..."},
			map[string]any{"type": "text", "text": "describe this"},
		}, "describe this"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractContent(tt.input)
			if got != tt.want {
				t.Errorf("extractContent() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandleChatCompletions_NonStreamingResponse(t *testing.T) {
	// Create provider and secret
	provider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default",
			Namespace: "default",
		},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeOpenAI,
			DefaultModel: "gpt-4",
			SecretRef: corev1alpha1.ProviderSecretRef{
				Name: "openai-secret",
				Key:  "api-key",
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openai-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-key": []byte("test-key"),
		},
	}

	handler, app := setupTestOpenAIHandler(provider, secret)
	app.Post("/openai/v1/chat/completions", handler.HandleChatCompletions)

	reqBody := OAIRequest{
		Model: "gpt-4",
		Messages: []OAIMessage{
			{Role: "user", Content: "hello"},
		},
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The request will fail because the API key is fake, but it should get past
	// the provider resolution phase (status != 400). We expect a 500 from the
	// actual API call failing.
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 400 {
		t.Errorf("did not expect 400; provider resolution should have succeeded. body: %s", string(respBody))
	}
}

// --- Tests: writeStreamChunk & writeStreamDone ---

func TestWriteStreamChunk(t *testing.T) {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)

	chunk := OAIResponse{
		ID:      "chatcmpl-123",
		Object:  "chat.completion.chunk",
		Created: 1234567890,
		Model:   "gpt-4",
		Choices: []OAIChoice{{
			Index: 0,
			Delta: &OAIMessage{Role: "assistant", Content: "hello"},
		}},
	}
	_ = writeStreamChunk(w, chunk)

	got := buf.String()
	if !strings.HasPrefix(got, "data: ") {
		t.Errorf("expected SSE data prefix, got: %q", got)
	}
	if !strings.Contains(got, "chatcmpl-123") {
		t.Errorf("expected completion ID in output, got: %q", got)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Errorf("expected double newline suffix, got: %q", got)
	}
}

func TestWriteStreamDone(t *testing.T) {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)

	_ = writeStreamDone(w)

	got := buf.String()
	if got != "data: [DONE]\n\n" {
		t.Errorf("writeStreamDone() = %q, want %q", got, "data: [DONE]\n\n")
	}
}

// --- Tests: handleNonStreamingCompletion ---

type oaiMockProvider struct {
	name      string
	resp      *llm.CompletionResponse
	responses []*llm.CompletionResponse
	err       error
	streamCh  chan llm.StreamChunk
	streamErr error
	requests  []*llm.CompletionRequest
	callIdx   int
}

func (m *oaiMockProvider) Complete(_ context.Context, req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	if req != nil {
		reqCopy := *req
		reqCopy.Messages = append([]llm.Message(nil), req.Messages...)
		reqCopy.Tools = append([]llm.Tool(nil), req.Tools...)
		m.requests = append(m.requests, &reqCopy)
	}
	if m.err != nil {
		return nil, m.err
	}
	if m.responses != nil {
		if m.callIdx >= len(m.responses) {
			return nil, fmt.Errorf("unexpected call %d", m.callIdx)
		}
		resp := m.responses[m.callIdx]
		m.callIdx++
		return resp, nil
	}
	m.callIdx++
	return m.resp, nil
}

func (m *oaiMockProvider) Stream(_ context.Context, _ *llm.CompletionRequest) (<-chan llm.StreamChunk, error) {
	if m.streamErr != nil {
		return nil, m.streamErr
	}
	return m.streamCh, nil
}

func (m *oaiMockProvider) Name() string {
	if m.name != "" {
		return m.name
	}
	return "mock"
}

func TestHandleNonStreamingCompletion_Success(t *testing.T) {
	mock := &oaiMockProvider{
		resp: &llm.CompletionResponse{
			Content:      "Hello!",
			StopReason:   "end_turn",
			InputTokens:  10,
			OutputTokens: 5,
		},
	}

	handler, app := setupTestOpenAIHandler()
	app.Post("/test", func(c fiber.Ctx) error {
		return handler.handleNonStreamingCompletion(
			c, context.Background(), mock,
			&llm.CompletionRequest{Model: "gpt-4", Messages: []llm.Message{{Role: "user", Content: "hi"}}},
			"chatcmpl-123", "gpt-4", 1234567890,
		)
	})

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var oaiResp OAIResponse
	json.NewDecoder(resp.Body).Decode(&oaiResp) //nolint:errcheck
	if oaiResp.ID != "chatcmpl-123" {
		t.Errorf("ID = %q, want chatcmpl-123", oaiResp.ID)
	}
	if oaiResp.Object != "chat.completion" {
		t.Errorf("Object = %q", oaiResp.Object)
	}
	if len(oaiResp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(oaiResp.Choices))
	}
	if oaiResp.Choices[0].Message == nil || extractContent(oaiResp.Choices[0].Message.Content) != "Hello!" {
		t.Errorf("unexpected message content")
	}
	if oaiResp.Usage == nil || oaiResp.Usage.TotalTokens != 15 {
		t.Errorf("usage = %+v", oaiResp.Usage)
	}
}

func TestHandleNonStreamingCompletion_WithToolCalls(t *testing.T) {
	mock := &oaiMockProvider{
		resp: &llm.CompletionResponse{
			Content:    "",
			StopReason: "end_turn",
			ToolCalls: []llm.ToolCall{
				{ID: "call_1", Name: "get_weather", Arguments: json.RawMessage(`{"location":"NYC"}`)},
			},
		},
	}

	handler, app := setupTestOpenAIHandler()
	app.Post("/test", func(c fiber.Ctx) error {
		return handler.handleNonStreamingCompletion(
			c, context.Background(), mock,
			&llm.CompletionRequest{Model: "gpt-4"},
			"chatcmpl-tc", "gpt-4", 1234567890,
		)
	})

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var oaiResp OAIResponse
	json.NewDecoder(resp.Body).Decode(&oaiResp) //nolint:errcheck
	if len(oaiResp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(oaiResp.Choices))
	}
	if *oaiResp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", *oaiResp.Choices[0].FinishReason)
	}
	if len(oaiResp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(oaiResp.Choices[0].Message.ToolCalls))
	}
	if oaiResp.Choices[0].Message.ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("tool call name = %q", oaiResp.Choices[0].Message.ToolCalls[0].Function.Name)
	}
}

func TestHandleNonStreamingCompletion_Error(t *testing.T) {
	mock := &oaiMockProvider{
		err: fmt.Errorf("API rate limited"),
	}

	handler, app := setupTestOpenAIHandler()
	app.Post("/test", func(c fiber.Ctx) error {
		return handler.handleNonStreamingCompletion(
			c, context.Background(), mock,
			&llm.CompletionRequest{Model: "gpt-4"},
			"chatcmpl-err", "gpt-4", 1234567890,
		)
	})

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 500 {
		t.Errorf("expected 500, got %d", resp.StatusCode)
	}

	var oaiErr OAIError
	json.NewDecoder(resp.Body).Decode(&oaiErr) //nolint:errcheck
	if oaiErr.Error.Type != "server_error" {
		t.Errorf("error type = %q, want server_error", oaiErr.Error.Type)
	}
}

// --- Tests: handleStreamingCompletion ---

func TestHandleStreamingCompletion_StreamSuccess(t *testing.T) {
	ch := make(chan llm.StreamChunk, 3)
	ch <- llm.StreamChunk{Content: "Hello"}
	ch <- llm.StreamChunk{Content: " world"}
	ch <- llm.StreamChunk{Done: true, StopReason: "end_turn"}
	close(ch)

	mock := &oaiMockProvider{streamCh: ch}

	handler, app := setupTestOpenAIHandler()
	app.Post("/test", func(c fiber.Ctx) error {
		return handler.handleStreamingCompletion(
			c, context.Background(), mock,
			&llm.CompletionRequest{Model: "gpt-4", Messages: []llm.Message{{Role: "user", Content: "hi"}}},
			"chatcmpl-stream", "gpt-4", 1234567890, nil,
		)
	})

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	if !strings.Contains(bodyStr, "Hello") {
		t.Errorf("expected Hello in stream, got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "[DONE]") {
		t.Errorf("expected [DONE] in stream, got: %s", bodyStr)
	}
}

func TestHandleStreamingCompletion_FallbackToComplete(t *testing.T) {
	mock := &oaiMockProvider{
		streamErr: fmt.Errorf("streaming not supported"),
		resp: &llm.CompletionResponse{
			Content:    "Fallback response",
			StopReason: "end_turn",
		},
	}

	handler, app := setupTestOpenAIHandler()
	app.Post("/test", func(c fiber.Ctx) error {
		return handler.handleStreamingCompletion(
			c, context.Background(), mock,
			&llm.CompletionRequest{Model: "gpt-4", Messages: []llm.Message{{Role: "user", Content: "hi"}}},
			"chatcmpl-fallback", "gpt-4", 1234567890, nil,
		)
	})

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	if !strings.Contains(bodyStr, "Fallback response") {
		t.Errorf("expected fallback content, got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "[DONE]") {
		t.Errorf("expected [DONE], got: %s", bodyStr)
	}
}

func TestHandleStreamingCompletion_BothFail(t *testing.T) {
	mock := &oaiMockProvider{
		streamErr: fmt.Errorf("stream fail"),
		err:       fmt.Errorf("complete also fails"),
	}

	handler, app := setupTestOpenAIHandler()
	app.Post("/test", func(c fiber.Ctx) error {
		return handler.handleStreamingCompletion(
			c, context.Background(), mock,
			&llm.CompletionRequest{Model: "gpt-4"},
			"chatcmpl-bothfail", "gpt-4", 1234567890, nil,
		)
	})

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	if !strings.Contains(bodyStr, "Error:") {
		t.Errorf("expected error chunk, got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "[DONE]") {
		t.Errorf("expected [DONE], got: %s", bodyStr)
	}
}

func TestHandleStreamingCompletion_WithToolCalls(t *testing.T) {
	ch := make(chan llm.StreamChunk, 2)
	ch <- llm.StreamChunk{
		ToolCall: &llm.ToolCall{ID: "call_1", Name: "search", Arguments: json.RawMessage(`{"q":"test"}`)},
	}
	ch <- llm.StreamChunk{Done: true, StopReason: "tool_use"}
	close(ch)

	mock := &oaiMockProvider{streamCh: ch}

	handler, app := setupTestOpenAIHandler()
	app.Post("/test", func(c fiber.Ctx) error {
		return handler.handleStreamingCompletion(
			c, context.Background(), mock,
			&llm.CompletionRequest{Model: "gpt-4"},
			"chatcmpl-tc", "gpt-4", 1234567890, nil,
		)
	})

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	if !strings.Contains(bodyStr, "search") {
		t.Errorf("expected tool call in stream, got: %s", bodyStr)
	}
}

func TestHandleStreamingCompletion_StreamIncludesUsageChunk(t *testing.T) {
	ch := make(chan llm.StreamChunk, 2)
	ch <- llm.StreamChunk{Content: "Hello"}
	ch <- llm.StreamChunk{Done: true, StopReason: "end_turn", InputTokens: 14, OutputTokens: 6}
	close(ch)
	mock := &oaiMockProvider{streamCh: ch}

	handler, app := setupTestOpenAIHandler()
	app.Post("/test", func(c fiber.Ctx) error {
		return handler.handleStreamingCompletion(
			c, context.Background(), mock,
			&llm.CompletionRequest{Model: "gpt-4"},
			"chatcmpl-stream-usage", "gpt-4", 1234567890,
			&StreamOptions{IncludeUsage: true},
		)
	})

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	if !strings.Contains(bodyStr, `"usage":{"prompt_tokens":14,"completion_tokens":6,"total_tokens":20}`) {
		t.Fatalf("expected usage chunk in stream, got: %s", bodyStr)
	}
}

func TestHandleStreamingCompletion_StreamOmitsUsageChunkWithoutCounts(t *testing.T) {
	ch := make(chan llm.StreamChunk, 2)
	ch <- llm.StreamChunk{Content: "Hello"}
	ch <- llm.StreamChunk{Done: true, StopReason: "end_turn"}
	close(ch)
	mock := &oaiMockProvider{streamCh: ch}

	handler, app := setupTestOpenAIHandler()
	app.Post("/test", func(c fiber.Ctx) error {
		return handler.handleStreamingCompletion(
			c, context.Background(), mock,
			&llm.CompletionRequest{Model: "gpt-4"},
			"chatcmpl-stream-no-usage", "gpt-4", 1234567890,
			&StreamOptions{IncludeUsage: true},
		)
	})

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	if strings.Contains(bodyStr, "prompt_tokens") || strings.Contains(bodyStr, "completion_tokens") {
		t.Fatalf("unexpected zero usage chunk in stream: %s", bodyStr)
	}
}

func TestHandleStreamingCompletion_WithUsage(t *testing.T) {
	mock := &oaiMockProvider{
		streamErr: fmt.Errorf("stream not supported"),
		resp: &llm.CompletionResponse{
			Content:      "response",
			StopReason:   "end_turn",
			InputTokens:  10,
			OutputTokens: 5,
		},
	}

	handler, app := setupTestOpenAIHandler()
	app.Post("/test", func(c fiber.Ctx) error {
		return handler.handleStreamingCompletion(
			c, context.Background(), mock,
			&llm.CompletionRequest{Model: "gpt-4"},
			"chatcmpl-usage", "gpt-4", 1234567890,
			&StreamOptions{IncludeUsage: true},
		)
	})

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	if !strings.Contains(bodyStr, "prompt_tokens") {
		t.Errorf("expected usage in stream, got: %s", bodyStr)
	}
}

func TestHandleStreamingCompletion_FallbackWithToolCalls(t *testing.T) {
	mock := &oaiMockProvider{
		streamErr: fmt.Errorf("stream not supported"),
		resp: &llm.CompletionResponse{
			Content:    "",
			StopReason: "tool_use",
			ToolCalls: []llm.ToolCall{
				{ID: "call_1", Name: "search", Arguments: json.RawMessage(`{"q":"test"}`)},
			},
		},
	}

	handler, app := setupTestOpenAIHandler()
	app.Post("/test", func(c fiber.Ctx) error {
		return handler.handleStreamingCompletion(
			c, context.Background(), mock,
			&llm.CompletionRequest{Model: "gpt-4"},
			"chatcmpl-fbtc", "gpt-4", 1234567890, nil,
		)
	})

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	if !strings.Contains(bodyStr, "search") {
		t.Errorf("expected tool call in fallback stream, got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "tool_calls") {
		t.Errorf("expected tool_calls finish reason, got: %s", bodyStr)
	}
}

func TestHandleStreamingToolLoop_StreamsProgressAndHidesServerSideToolCalls(t *testing.T) {
	filePath := fmt.Sprintf("/tmp/orka-openai-stream-%d.txt", time.Now().UnixNano())
	if err := os.WriteFile(filePath, []byte("streaming test content"), 0o600); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(filePath) })

	mock := &oaiMockProvider{
		responses: []*llm.CompletionResponse{
			{
				Content:    "Reading file before final answer.\n" + goalStateSentinel + "\n",
				StopReason: oaiStopReasonToolUse,
				ToolCalls: []llm.ToolCall{{
					ID:        "call_file_read",
					Name:      "file_read",
					Arguments: json.RawMessage(fmt.Sprintf(`{"path":%q}`, filePath)),
				}},
			},
			{
				Content:      goalStateSentinel + "\nPR ready: https://example.test/pr/1",
				StopReason:   oaiStopReasonEndTurn,
				InputTokens:  10,
				OutputTokens: 5,
			},
		},
	}

	handler, app := setupTestOpenAIHandler()
	app.Post("/test", func(c fiber.Ctx) error {
		return handler.handleStreamingToolLoop(
			c, context.Background(), mock,
			&llm.CompletionRequest{
				Model:    "gpt-4",
				Messages: []llm.Message{{Role: "user", Content: "read then report"}},
				Tools:    []llm.Tool{{Name: "file_read"}},
			},
			"chatcmpl-tool-loop", "gpt-4", 1234567890, nil, nil,
		)
	})

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	for _, want := range []string{
		`"role":"assistant"`,
		"Reading file before final answer.",
		"[Tool file_read completed]",
		"PR ready: https://example.test/pr/1",
		"[DONE]",
	} {
		if !strings.Contains(bodyStr, want) {
			t.Fatalf("expected stream body to contain %q, got: %s", want, bodyStr)
		}
	}
	for _, forbidden := range []string{goalStateSentinel, `"tool_calls"`, "call_file_read", "streaming test content"} {
		if strings.Contains(bodyStr, forbidden) {
			t.Fatalf("stream body leaked internal marker/tool call %q: %s", forbidden, bodyStr)
		}
	}
	if mock.callIdx != 2 {
		t.Fatalf("expected 2 LLM calls, got %d", mock.callIdx)
	}
}

func TestHandleNonStreamingToolLoop_StripsGoalStateSentinel(t *testing.T) {
	mock := &oaiMockProvider{
		responses: []*llm.CompletionResponse{{
			Content:    goalStateSentinel + "\nPR ready: https://example.test/pr/2",
			StopReason: oaiStopReasonEndTurn,
		}},
	}

	handler, app := setupTestOpenAIHandler()
	app.Post("/test", func(c fiber.Ctx) error {
		return handler.handleNonStreamingToolLoop(
			c, context.Background(), mock,
			&llm.CompletionRequest{Model: "gpt-4", Messages: []llm.Message{{Role: "user", Content: "ship"}}},
			"chatcmpl-nonstream-tool-loop", "gpt-4", 1234567890, nil,
		)
	})

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var oaiResp OAIResponse
	json.NewDecoder(resp.Body).Decode(&oaiResp) //nolint:errcheck
	content := extractContent(oaiResp.Choices[0].Message.Content)
	if strings.Contains(content, goalStateSentinel) {
		t.Fatalf("non-streaming response leaked sentinel: %q", content)
	}
	if content != "PR ready: https://example.test/pr/2" {
		t.Fatalf("content = %q", content)
	}
}

func TestHandleStreamingToolLoop_DoesNotStreamPrematureCoordinatorText(t *testing.T) {
	mock := &oaiMockProvider{
		responses: []*llm.CompletionResponse{
			{Content: "premature secret progress summary", StopReason: oaiStopReasonEndTurn},
			{Content: goalStateSentinel + "\nPR ready: https://example.test/pr/5", StopReason: oaiStopReasonEndTurn},
		},
	}

	handler, app := setupTestOpenAIHandler()
	handler.config.MaxPrematureEndRetries = 1
	app.Post("/test", func(c fiber.Ctx) error {
		return handler.handleStreamingToolLoop(
			c, context.Background(), mock,
			&llm.CompletionRequest{Model: "gpt-4", Messages: []llm.Message{{Role: "user", Content: "ship"}}},
			"chatcmpl-premature", "gpt-4", 1234567890, nil, nil,
		)
	})

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	if strings.Contains(bodyStr, "premature secret progress summary") {
		t.Fatalf("stream body leaked premature coordinator text: %s", bodyStr)
	}
	for _, want := range []string{"[Continuing workflow...]", "PR ready: https://example.test/pr/5", "[DONE]"} {
		if !strings.Contains(bodyStr, want) {
			t.Fatalf("expected stream body to contain %q, got: %s", want, bodyStr)
		}
	}
	if mock.callIdx != 2 {
		t.Fatalf("expected 2 LLM calls, got %d", mock.callIdx)
	}
}

// --- Tests: extractContent edge cases ---

func TestExtractContent_NonStringNonArray(t *testing.T) {
	// Test the default branch (non-string, non-array) with a number
	got := extractContent(42)
	if got != "42" {
		t.Errorf("extractContent(42) = %q, want %q", got, "42")
	}
}

func TestExtractContent_JSONStringViaDefault(t *testing.T) {
	// A boolean should go through the default path
	got := extractContent(true)
	if got != "true" {
		t.Errorf("extractContent(true) = %q, want %q", got, "true")
	}
}

// --- Tests: resolveProviderFromModel edge cases ---

func TestResolveProviderFromModel_SecretMissingKey(t *testing.T) {
	provider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeOpenAI,
			DefaultModel: "gpt-4",
			SecretRef:    corev1alpha1.ProviderSecretRef{Name: "secret1", Key: "missing-key"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "secret1", Namespace: "default"},
		Data:       map[string][]byte{"other-key": []byte("val")},
	}
	handler, _ := setupTestOpenAIHandler(provider, secret)
	_, _, err := handler.resolver.Resolve(context.Background(), ResolveOpts{ModelStr: "gpt-4", Namespace: "default", RequireModel: true})
	if err == nil {
		t.Fatal("expected error for missing secret key")
	}
	if !strings.Contains(err.Error(), "missing-key") {
		t.Errorf("error = %v", err)
	}
}

func TestResolveProviderFromModel_NoModel(t *testing.T) {
	provider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:      corev1alpha1.ProviderTypeOpenAI,
			SecretRef: corev1alpha1.ProviderSecretRef{Name: "secret1"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "secret1", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("val")},
	}
	handler, _ := setupTestOpenAIHandler(provider, secret)
	handler.config.Model = "" // no default
	_, _, err := handler.resolver.Resolve(context.Background(), ResolveOpts{ModelStr: "", Namespace: "default", RequireModel: true})
	if err == nil {
		t.Fatal("expected error for no model")
	}
	if !strings.Contains(err.Error(), "no model") {
		t.Errorf("error = %v", err)
	}
}

func TestHandleChatCompletions_MaxTokensTooSmall(t *testing.T) {
	provider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeOpenAI,
			DefaultModel: "gpt-4",
			SecretRef:    corev1alpha1.ProviderSecretRef{Name: "secret1"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "secret1", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("key")},
	}

	handler, app := setupTestOpenAIHandler(provider, secret)
	app.Post("/openai/v1/chat/completions", handler.HandleChatCompletions)

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}],"max_tokens":5}`
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}

	var oaiErr OAIError
	json.NewDecoder(resp.Body).Decode(&oaiErr) //nolint:errcheck
	if oaiErr.Error.Param == nil || *oaiErr.Error.Param != oaiParamMaxTokens {
		t.Errorf("expected param max_tokens, got %v", oaiErr.Error.Param)
	}
}

func TestHandleChatCompletions_ProviderSlashModel(t *testing.T) {
	// Mock API server returns a quick error so the test doesn't time out
	// reaching the real Anthropic API.
	mockAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid key"}}`))
	}))
	defer mockAPI.Close()

	// Create provider and secret
	provider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "anthropic",
			Namespace: "default",
		},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeAnthropic,
			DefaultModel: "claude-sonnet-4-20250514",
			BaseURL:      mockAPI.URL,
			SecretRef: corev1alpha1.ProviderSecretRef{
				Name: "anthropic-secret",
				Key:  "api-key",
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "anthropic-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-key": []byte("test-key"),
		},
	}

	handler, app := setupTestOpenAIHandler(provider, secret)
	app.Post("/openai/v1/chat/completions", handler.HandleChatCompletions)

	// Use "provider/model" format
	reqBody := OAIRequest{
		Model: "anthropic/claude-sonnet-4-20250514",
		Messages: []OAIMessage{
			{Role: "user", Content: "hello"},
		},
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should NOT fail with a provider resolution error (400)
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 400 {
		t.Errorf("did not expect 400; provider/model split should have worked. body: %s", string(respBody))
	}
}

func TestOpenAICompat_ContextTokenAuthorizationRequiresProviderScopeForModels(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	handler, app := setupTestOpenAIHandler()
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	if err != nil {
		t.Fatalf("NewContextTokenAuthorizationConfig returned error: %v", err)
	}
	handler.contextTokenAuthorization = authz

	app.Use(NewAuthMiddleware(handler.client, AuthConfig{ContextTokens: ctxTokenConfig}))
	app.Get("/openai/v1/models", handler.HandleListModels)

	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeTaskList,
	})
	req := httptest.NewRequest(http.MethodGet, "/openai/v1/models", nil)
	req.Header.Set(KontxtHeaderName, token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

func TestOpenAICompat_ContextTokenAuthorizationFiltersListModels(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	allowedProvider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "openai", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeOpenAI,
			DefaultModel: "gpt-4o",
		},
	}
	disallowedModelProvider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "openai-mini", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeOpenAI,
			DefaultModel: "gpt-4o-mini",
		},
	}
	disallowedProvider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeAnthropic,
			DefaultModel: "claude-sonnet-4-20250514",
		},
	}
	handler, app := setupTestOpenAIHandler(allowedProvider, disallowedModelProvider, disallowedProvider)
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	if err != nil {
		t.Fatalf("NewContextTokenAuthorizationConfig returned error: %v", err)
	}
	handler.contextTokenAuthorization = authz

	app.Use(NewAuthMiddleware(handler.client, AuthConfig{ContextTokens: ctxTokenConfig}))
	app.Get("/openai/v1/models", handler.HandleListModels)

	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeProvidersUse,
		"tctx": map[string]any{
			"allowedProviders": []string{"openai", "openai-mini"},
			"allowedModels":    []string{"gpt-4o"},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/openai/v1/models", nil)
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
	for _, id := range []string{"openai/gpt-4o", "gpt-4o"} {
		if !got[id] {
			t.Fatalf("expected allowed model ID %q in response, got %#v", id, got)
		}
	}
	for _, id := range []string{"openai-mini/gpt-4o-mini", "gpt-4o-mini", "anthropic/claude-sonnet-4-20250514", "claude-sonnet-4-20250514"} {
		if got[id] {
			t.Fatalf("did not expect disallowed model ID %q in response: %#v", id, got)
		}
	}
}

func TestOpenAICompat_ContextTokenAuthorizationAuditAllowsListModels(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	allowedProvider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "openai", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeOpenAI,
			DefaultModel: "gpt-4o",
		},
	}
	disallowedModelProvider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "openai-mini", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeOpenAI,
			DefaultModel: "gpt-4o-mini",
		},
	}
	disallowedProvider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeAnthropic,
			DefaultModel: "claude-sonnet-4-20250514",
		},
	}
	handler, app := setupTestOpenAIHandler(allowedProvider, disallowedModelProvider, disallowedProvider)
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeAudit})
	if err != nil {
		t.Fatalf("NewContextTokenAuthorizationConfig returned error: %v", err)
	}
	handler.contextTokenAuthorization = authz

	app.Use(NewAuthMiddleware(handler.client, AuthConfig{ContextTokens: ctxTokenConfig}))
	app.Get("/openai/v1/models", handler.HandleListModels)

	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeProvidersUse,
		"tctx": map[string]any{
			"allowedProviders": []string{"openai", "openai-mini"},
			"allowedModels":    []string{"gpt-4o"},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/openai/v1/models", nil)
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
	for _, id := range []string{"openai/gpt-4o", "gpt-4o", "openai-mini/gpt-4o-mini", "gpt-4o-mini", "anthropic/claude-sonnet-4-20250514", "claude-sonnet-4-20250514"} {
		if !got[id] {
			t.Fatalf("expected model ID %q in audit-mode response, got %#v", id, got)
		}
	}
}
