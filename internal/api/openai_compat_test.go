/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"

	// Register LLM providers for integration tests
	_ "github.com/sozercan/mercan/internal/llm/anthropic"
	_ "github.com/sozercan/mercan/internal/llm/openai"
)

func setupTestOpenAIHandler(objs ...runtime.Object) (*OpenAICompatHandler, *fiber.App) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	handler := NewOpenAICompatHandler(fakeClient, "default", false, DefaultChatConfig())

	app := fiber.New()
	return handler, app
}

func TestHandleChatCompletions_MissingMessages(t *testing.T) {
	handler, app := setupTestOpenAIHandler()
	app.Post("/v1/chat/completions", handler.HandleChatCompletions)

	body := `{"model":"test-model","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
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
	if oaiErr.Error.Type != "invalid_request_error" {
		t.Errorf("expected error type invalid_request_error, got %s", oaiErr.Error.Type)
	}
}

func TestHandleChatCompletions_NGreaterThanOne(t *testing.T) {
	handler, app := setupTestOpenAIHandler()
	app.Post("/v1/chat/completions", handler.HandleChatCompletions)

	body := `{"model":"test-model","messages":[{"role":"user","content":"hello"}],"n":2}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
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
	if oaiErr.Error.Type != "invalid_request_error" {
		t.Errorf("expected error type invalid_request_error, got %s", oaiErr.Error.Type)
	}
	if oaiErr.Error.Param == nil || *oaiErr.Error.Param != "n" {
		t.Errorf("expected error param 'n', got %v", oaiErr.Error.Param)
	}
}

func TestHandleChatCompletions_InvalidBody(t *testing.T) {
	handler, app := setupTestOpenAIHandler()
	app.Post("/v1/chat/completions", handler.HandleChatCompletions)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("not json"))
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
	app.Post("/v1/chat/completions", handler.HandleChatCompletions)

	body := `{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
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
	app.Get("/v1/models", handler.HandleListModels)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
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
	app.Get("/v1/models", handler.HandleListModels)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
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
		{"max_tokens", "length"},
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
	app.Post("/v1/chat/completions", handler.HandleChatCompletions)

	reqBody := OAIRequest{
		Model: "gpt-4",
		Messages: []OAIMessage{
			{Role: "user", Content: "hello"},
		},
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
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

func TestHandleChatCompletions_ProviderSlashModel(t *testing.T) {
	// Create provider and secret
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
	app.Post("/v1/chat/completions", handler.HandleChatCompletions)

	// Use "provider/model" format
	reqBody := OAIRequest{
		Model: "anthropic/claude-sonnet-4-20250514",
		Messages: []OAIMessage{
			{Role: "user", Content: "hello"},
		},
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
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
