package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/sessioncontext"
	"github.com/orka-agents/orka/internal/store"
	toolspkg "github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
)

const (
	comparisonConstraint = "Keep the public API unchanged."
	comparisonFinding    = "The cache was ruled out; inspect the parser next."
	comparisonCurrent    = "Continue the parser investigation. Do not publish."
)

type contextComparison struct {
	Checkpoints         bool          `json:"checkpoints"`
	RetainedConstraints int           `json:"retainedConstraints"`
	RepeatedWork        int           `json:"repeatedWork"`
	InputTokens         int           `json:"estimatedAcceptedInputTokens"`
	RejectedInputTokens int           `json:"estimatedRejectedInputTokens"`
	OutputTokens        int           `json:"estimatedOutputTokens"`
	ModelCalls          int           `json:"modelCalls"`
	CheckpointCalls     int           `json:"checkpointCalls"`
	ContextRejections   int           `json:"contextRejections"`
	Elapsed             time.Duration `json:"elapsedNanoseconds"`
}

// This deterministic comparison exercises the real worker, truncation, SQLite,
// and checkpoint HTTP client. Its scripted model is deliberately sensitive to
// missing evidence. It measures plumbing and overhead, not provider quality.
func TestSessionCheckpointLongTaskComparison(t *testing.T) {
	var results []contextComparison
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("checkpoints=%t", enabled), func(t *testing.T) {
			results = append(results, runContextComparison(t, enabled))
		})
	}
	require.Len(t, results, 2)
	require.Equal(t, 1, results[0].RetainedConstraints)
	require.Equal(t, 2, results[1].RetainedConstraints)
	require.Greater(t, results[0].RepeatedWork, results[1].RepeatedWork)
	require.Zero(t, results[1].RepeatedWork)
	require.GreaterOrEqual(t, results[1].CheckpointCalls, 2)
	for _, result := range results {
		data, err := json.Marshal(result)
		require.NoError(t, err)
		t.Log(string(data))
	}
}

func runContextComparison(t *testing.T, enabled bool) contextComparison {
	t.Helper()
	const window = 6000
	f := newWorkerContextFixture(t, window)
	t.Setenv(workerenv.SessionCheckpointsEnabled, fmt.Sprint(enabled))
	defer replaceDefaultToolRegistryForTest(t)()
	history := make([]store.SessionMessage, 0, 6)
	history = append(history, []store.SessionMessage{
		{ID: "historical-request", Role: "user", Content: comparisonConstraint},
		{ID: "historical-call", Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "historical-read", Name: "context_fixture_read", Arguments: json.RawMessage(`{}`)},
		}},
		{ID: "historical-finding", Role: "tool", ToolCallID: "historical-read", Name: "context_fixture_read",
			Content: comparisonFinding},
	}...)
	for i := range 3 {
		history = append(history, store.SessionMessage{
			ID: fmt.Sprintf("historical-detail-%d", i), Role: "assistant", Content: strings.Repeat("parser evidence ", 500),
		})
	}
	require.NoError(t, f.store.AppendMessages(context.Background(), f.write.Namespace, f.write.SessionName, history))
	messages := make([]llm.Message, 0, len(history)+1)
	for _, source := range history {
		message, err := sessioncontext.ModelMessage(source)
		require.NoError(t, err)
		messages = append(messages, message)
	}
	messages = append(messages, llm.Message{Role: "user", Content: comparisonCurrent})
	metrics := contextComparison{Checkpoints: enabled}
	completedSteps := 0
	repeatPending := false
	toolspkg.DefaultRegistry.Register(contextFixtureTool{run: func(context.Context) (string, error) {
		if repeatPending {
			metrics.RepeatedWork++
			return comparisonFinding, nil
		}
		completedSteps++
		return strings.Repeat("saved parser evidence ", 3000), nil
	}})
	provider := contextFixtureProvider{complete: func(_ context.Context,
		req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
		metrics.ModelCalls++
		inputTokens := llm.EstimateRequestTokens(req)
		if inputTokens+llm.ResponseTokenReserve(req) > window {
			metrics.ContextRejections++
			metrics.RejectedInputTokens += inputTokens
			return nil, &llm.ProviderError{StatusCode: 400, Message: "context window too long"}
		}
		metrics.InputTokens += inputTokens
		var response *llm.CompletionResponse
		if req.SystemPrompt == checkpointInstructions {
			metrics.CheckpointCalls++
			response = comparisonCheckpoint(t, req)
		} else {
			require.True(t, slices.ContainsFunc(req.Messages, func(message llm.Message) bool {
				return message.Role == "user" && message.Content == comparisonCurrent
			}), "the current request must survive every reduction")
			var active strings.Builder
			for _, message := range req.Messages {
				active.WriteString(message.Content)
			}
			if completedSteps == 3 {
				metrics.RetainedConstraints = 1
				if strings.Contains(active.String(), comparisonConstraint) {
					metrics.RetainedConstraints++
				}
				response = &llm.CompletionResponse{Content: "The parser investigation is complete.", StopReason: "stop"}
			} else {
				repeatPending = !strings.Contains(active.String(), comparisonFinding)
				response = &llm.CompletionResponse{StopReason: "tool_calls", ToolCalls: []llm.ToolCall{
					{ID: fmt.Sprintf("comparison-%d", metrics.ModelCalls), Name: "context_fixture_read",
						Arguments: json.RawMessage(`{}`)},
				}}
			}
		}
		response.InputTokens = inputTokens
		response.OutputTokens = llm.EstimateRequestTokens(&llm.CompletionRequest{Messages: []llm.Message{
			{Role: "assistant", Content: response.Content, ToolCalls: response.ToolCalls},
		}}) - 256
		metrics.OutputTokens += response.OutputTokens
		return response, nil
	}}
	started := time.Now()
	_, err := executeAgentLoopWithEvents(context.Background(), provider, messages,
		"Investigate the parser failure using the available evidence.", "fixture",
		modelSettings{maxTokens: 512}, []llm.Tool{{Name: "context_fixture_read"}}, nil, nil, common.NoopEventRecorder{})
	metrics.Elapsed = time.Since(started)
	require.NoError(t, err)
	require.Equal(t, 3, completedSteps)
	return metrics
}

func comparisonCheckpoint(t *testing.T, req *llm.CompletionRequest) *llm.CompletionResponse {
	t.Helper()
	// Only retain facts actually present in the current source excerpts or the
	// previous note. The storage path validates their original committed IDs.
	payload := req.Messages[0].Content
	draft := checkpointDraft{
		Goal: checkpointClaim{
			Text: "Continue the parser investigation.", Sources: []string{sessioncontext.PromptMessageID(contextFixtureUID)},
		},
		Remaining: []string{"Complete the parser evidence reads."},
	}
	if strings.Contains(payload, comparisonConstraint) {
		draft.Constraints = []checkpointClaim{{Text: comparisonConstraint, Sources: []string{"historical-request"}}}
	}
	if strings.Contains(payload, comparisonFinding) {
		draft.Findings = []checkpointClaim{{Text: comparisonFinding, Sources: []string{"historical-finding"}}}
	}
	data, err := json.Marshal(draft)
	require.NoError(t, err)
	return &llm.CompletionResponse{Content: string(data), StopReason: "stop"}
}
