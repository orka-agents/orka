/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/orka-agents/orka/internal/llm"
	chattools "github.com/orka-agents/orka/internal/tools"
)

type recordingToolChatProvider struct {
	chatMockProvider
	requests [][]llm.Message
}

func (p *recordingToolChatProvider) Complete(ctx context.Context, req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	messages := slices.Clone(req.Messages)
	for i := range messages {
		messages[i].ToolCalls = slices.Clone(messages[i].ToolCalls)
		for j := range messages[i].ToolCalls {
			messages[i].ToolCalls[j].Arguments = bytes.Clone(messages[i].ToolCalls[j].Arguments)
		}
	}
	p.requests = append(p.requests, messages)
	return p.chatMockProvider.Complete(ctx, req)
}

func TestHandleChatPreservesToolJSON(t *testing.T) {
	tests := []struct {
		name      string
		arguments string
		toolError bool
	}{
		{
			name:      "precise nested numbers",
			arguments: `{"big":9223372036854775807,"precise":0.1234567890123456789,"nested":[null,-0,1.2300e+42,9007199254740993]}`,
		},
		{
			name:      "nested overflow",
			arguments: `{"nested":{"overflow":1e+400}}`,
			toolError: true,
		},
		{name: "scalar overflow", arguments: `1e+400`, toolError: true},
		{name: "null", arguments: `null`},
		{
			name:      "array",
			arguments: `[null,{"negative":-0,"precise":0.1234567890123456789},9007199254740993]`,
			toolError: true,
		},
		{
			name:      "serialized string",
			arguments: `"{\n  \"id\": 9223372036854775807,\n  \"zero\": -0\n}\n"`,
			toolError: true,
		},
		{name: "boolean", arguments: `true`, toolError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const providerType = "test-chat-tool-json"
			provider := &recordingToolChatProvider{chatMockProvider: chatMockProvider{
				name: providerType,
				responses: []*llm.CompletionResponse{
					{ToolCalls: []llm.ToolCall{{ID: "precise-call", Name: "list_tasks", Arguments: json.RawMessage(tt.arguments)}}, InputTokens: 7, OutputTokens: 11},
					{Content: "done", InputTokens: 7, OutputTokens: 11},
					{Content: "continued", InputTokens: 3, OutputTokens: 5},
				},
			}}
			llm.RegisterProvider(providerType, func(llm.ProviderConfig) (llm.Provider, error) { return provider, nil })
			fakeClient := fake.NewClientBuilder().WithScheme(newTestScheme()).
				WithRuntimeObjects(providerCRD("default", "default", providerType, "test-model")...).Build()
			ss := newTestSessionStore(t)
			ch := newTestChatHandler(t, fakeClient, ss, newTestResultStore(t), DefaultChatConfig())
			app := fiber.New()
			app.Post("/api/v1/chat", ch.HandleChat)

			post := func(message string) []byte {
				t.Helper()
				body, err := json.Marshal(ChatRequest{SessionID: "precise-session", Message: message})
				require.NoError(t, err)
				req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json")
				resp, err := app.Test(req)
				require.NoError(t, err)
				defer func() { require.NoError(t, resp.Body.Close()) }()
				data, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, resp.StatusCode, "%s", data)
				return data
			}
			var response struct {
				Message   string `json:"message"`
				ToolCalls []struct {
					Args   json.RawMessage `json:"args"`
					Result json.RawMessage `json:"result"`
				} `json:"toolCalls"`
				Usage ChatUsage `json:"usage"`
			}
			require.NoError(t, json.Unmarshal(post("check arguments"), &response))
			assert.Equal(t, "done", response.Message)
			require.Len(t, response.ToolCalls, 1)
			assert.Equal(t, tt.arguments, string(response.ToolCalls[0].Args))
			assert.Equal(t, 14, response.Usage.InputTokens)
			assert.Equal(t, 22, response.Usage.OutputTokens)
			var result ToolResult
			require.NoError(t, json.Unmarshal(response.ToolCalls[0].Result, &result))
			assert.Equal(t, !tt.toolError, result.Success)
			if tt.toolError {
				assert.Equal(t, "invalid_arguments", result.ErrorType)
			}

			before, err := ss.GetSession(context.Background(), "default", "precise-session")
			require.NoError(t, err)
			require.Len(t, before.Messages, 4)
			assert.Equal(t, string(response.ToolCalls[0].Result), before.Messages[2].Content)
			post("continue")
			require.Len(t, provider.requests, 3)
			decodeNumbers := func(raw string) any {
				t.Helper()
				decoder := json.NewDecoder(strings.NewReader(raw))
				decoder.UseNumber()
				var value any
				require.NoError(t, decoder.Decode(&value))
				return value
			}
			for _, request := range provider.requests[1:] {
				require.GreaterOrEqual(t, len(request), 3)
				require.Len(t, request[1].ToolCalls, 1)
				assert.Equal(t, decodeNumbers(tt.arguments), decodeNumbers(string(request[1].ToolCalls[0].Arguments)))
				assert.Equal(t, before.Messages[2].Content, request[2].Content)
			}
			after, err := ss.GetSession(context.Background(), "default", "precise-session")
			require.NoError(t, err)
			require.Len(t, after.Messages, 6)
			assert.Equal(t, before.Messages, after.Messages[:4])
			assert.Equal(t, "continued", after.Messages[5].Content)
			assert.Equal(t, 17, after.InputTokens)
			assert.Equal(t, 27, after.OutputTokens)
		})
	}
}

type preciseChatResultTool struct {
	chattools.ListTasksTool
}

func (*preciseChatResultTool) Execute(context.Context, json.RawMessage) (string, error) {
	return `{"success":true,"data":{"big":9223372036854775807,"precise":0.1234567890123456789,"array":[null,-0,1.2300e+42,9007199254740993]}}`, nil
}

func TestExecuteToolCallsPreservesResultJSON(t *testing.T) {
	fakeClient := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
	resultStore := newTestResultStore(t)
	ch := newTestChatHandler(t, fakeClient, newTestSessionStore(t), resultStore, DefaultChatConfig())
	executor := NewToolExecutor(fakeClient, nil, "default", "result-session", "", false, 5, time.Minute, resultStore)
	tool := &preciseChatResultTool{}
	executor.registry.Register(tool)
	messages, calls, _ := ch.executeToolCalls(context.Background(), &llm.CompletionResponse{
		ToolCalls: []llm.ToolCall{{ID: "result-call", Name: tool.Name(), Arguments: json.RawMessage(`{}`)}},
	}, executor, nil, make(map[string]int))
	require.Len(t, messages, 2)
	want, err := tool.Execute(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, want, messages[1].Content)
	encoded, err := json.Marshal(calls)
	require.NoError(t, err)
	var response []struct {
		Result json.RawMessage `json:"result"`
	}
	require.NoError(t, json.Unmarshal(encoded, &response))
	require.Len(t, response, 1)
	assert.Equal(t, want, string(response[0].Result))
}
