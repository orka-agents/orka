package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func contextBudgetTestRequest() *CompletionRequest {
	return &CompletionRequest{
		Model:        "primary-model",
		SystemPrompt: "Preserve the public API.",
		MaxTokens:    64,
		Messages: []Message{
			{ID: "current-request", Role: "user", Content: "Find the failure without changing the API."},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "read-one", Name: "file_read", Arguments: json.RawMessage(`{"path":"main.go"}`)}}},
			{Role: "tool", Name: "file_read", ToolCallID: "read-one", Content: "The failure is in the parser."},
		},
		Tools:         []Tool{{Name: "file_read", Description: "Read a file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}},
		StopSequences: []string{"DONE"},
		ResponseFormat: &ResponseFormat{
			Type: "json_schema",
			JSONSchema: &JSONSchemaFormat{
				Name: "answer", Description: "The diagnosis", Schema: map[string]any{"type": "object"},
			},
		},
	}
}

func TestEstimateRequestTokensCountsAllProviderInputs(t *testing.T) {
	large := strings.Repeat("x", 4096)
	for name, change := range map[string]func(*CompletionRequest){
		"system prompt":    func(req *CompletionRequest) { req.SystemPrompt = large },
		"message content":  func(req *CompletionRequest) { req.Messages[0].Content = large },
		"tool result name": func(req *CompletionRequest) { req.Messages[2].Name = large },
		"tool result ID":   func(req *CompletionRequest) { req.Messages[2].ToolCallID = large },
		"tool call ID":     func(req *CompletionRequest) { req.Messages[1].ToolCalls[0].ID = large },
		"tool call name":   func(req *CompletionRequest) { req.Messages[1].ToolCalls[0].Name = large },
		"tool arguments": func(req *CompletionRequest) {
			req.Messages[1].ToolCalls[0].Arguments = json.RawMessage(`{"path":"` + large + `"}`)
		},
		"tool definition name":        func(req *CompletionRequest) { req.Tools[0].Name = large },
		"tool definition description": func(req *CompletionRequest) { req.Tools[0].Description = large },
		"tool definition schema": func(req *CompletionRequest) {
			req.Tools[0].Parameters = json.RawMessage(`{"type":"object","description":"` + large + `"}`)
		},
		"stop sequence": func(req *CompletionRequest) { req.StopSequences = []string{large} },
		"response format": func(req *CompletionRequest) {
			req.ResponseFormat.JSONSchema.Schema = map[string]any{"type": "object", "description": large}
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := contextBudgetTestRequest()
			before := EstimateRequestTokens(req)
			change(req)
			require.GreaterOrEqual(t, EstimateRequestTokens(req)-before, 1000, "large provider input was omitted from the estimate")
		})
	}

	t.Run("private source IDs are excluded", func(t *testing.T) {
		req := contextBudgetTestRequest()
		before := EstimateRequestTokens(req)
		req.Messages[0].ID = large
		require.Equal(t, before, EstimateRequestTokens(req))
	})
	t.Run("message role framing", func(t *testing.T) {
		req := contextBudgetTestRequest()
		before := EstimateRequestTokens(req)
		req.Messages[0].Role = "assistant"
		require.Greater(t, EstimateRequestTokens(req), before)
	})
	emptyEstimate := EstimateRequestTokens(&CompletionRequest{})
	require.Positive(t, emptyEstimate, "protocol framing needs a reserve even for empty content")
	require.Zero(t, EstimateRequestTokens(nil))
}

func TestMessageTokenBudgetReservesNonMessageInputsAndOutput(t *testing.T) {
	large := strings.Repeat("x", 4096)
	for name, change := range map[string]func(*CompletionRequest){
		"instructions": func(req *CompletionRequest) { req.SystemPrompt = large },
		"tools":        func(req *CompletionRequest) { req.Tools[0].Description = large },
		"schema": func(req *CompletionRequest) {
			req.Tools[0].Parameters = json.RawMessage(`{"description":"` + large + `"}`)
		},
		"stop sequence": func(req *CompletionRequest) { req.StopSequences = []string{large} },
		"output schema": func(req *CompletionRequest) {
			req.ResponseFormat.JSONSchema.Description = large
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := contextBudgetTestRequest()
			before := MessageTokenBudget(req, 8192)
			change(req)
			require.LessOrEqual(t, MessageTokenBudget(req, 8192), before-1000)
		})
	}

	req := contextBudgetTestRequest()
	before := MessageTokenBudget(req, 8192)
	req.MaxTokens += 113
	require.Equal(t, before-113, MessageTokenBudget(req, 8192))
	require.Equal(t, req.MaxTokens, ResponseTokenReserve(req))
	for _, output := range []int{0, -1} {
		req.MaxTokens = output
		require.Equal(t, 4096, ResponseTokenReserve(req))
	}
}

func TestMessageTokenBudgetCoversGeneratedReferenceNote(t *testing.T) {
	req := &CompletionRequest{
		Model: "small-model", MaxTokens: 1,
		Messages: []Message{
			{Role: "user", Content: strings.Repeat("old history", 100)},
			{ID: "current", Role: "user", Content: "now"},
		},
	}
	// Leave exactly nine payload tokens for the current request and minimal
	// truncation note. The note's assistant role also needs framing space.
	req.ContextWindow = 8192 - MessageTokenBudget(req, 8192) + 9
	fitted, err := FitMessagesKeeping(req.Messages, MessageTokenBudget(req, req.ContextWindow), 1)
	require.NoError(t, err)
	require.Len(t, fitted, 2)
	require.Equal(t, "assistant", fitted[0].Role)
	require.Equal(t, "current", fitted[1].ID)
	req.Messages = fitted
	require.NoError(t, CheckContextWindow(req), "message budget omitted the generated note's role framing")
}

func TestContextWindowRejectsImpossibleRequestWithoutMutation(t *testing.T) {
	req := contextBudgetTestRequest()
	req.Messages[0].Content = strings.Repeat("preserve this exact current request\n", 100)
	req.ContextWindow = 512
	before, err := json.Marshal(req)
	require.NoError(t, err)
	budget := MessageTokenBudget(req, req.ContextWindow)
	fitted, err := FitMessagesKeeping(req.Messages, budget, 0)
	require.ErrorIs(t, err, ErrRequiredContextTooLarge)
	require.Nil(t, fitted)
	err = CheckContextWindow(req)
	require.ErrorIs(t, err, ErrContextLimit)
	var limit *ContextLimitError
	require.ErrorAs(t, err, &limit)
	require.Equal(t, "primary-model", limit.Model)
	require.Equal(t, 512, limit.Window)
	require.Equal(t, req.MaxTokens, limit.OutputTokens)
	require.Greater(t, limit.InputTokens+limit.OutputTokens, limit.Window)
	after, err := json.Marshal(req)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, "current-request", req.Messages[0].ID)
}

func TestContextWindowAdmissionBoundaryAndDisabledCompatibility(t *testing.T) {
	req := contextBudgetTestRequest()
	req.ContextWindow = EstimateRequestTokens(req) + ResponseTokenReserve(req)
	require.NoError(t, CheckContextWindow(req))
	req.ContextWindow--
	require.ErrorIs(t, CheckContextWindow(req), ErrContextLimit)
	req.ContextWindow = 0
	require.NoError(t, CheckContextWindow(req))
	require.NoError(t, CheckContextWindow(nil))
}
