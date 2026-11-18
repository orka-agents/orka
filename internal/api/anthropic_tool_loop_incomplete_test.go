package api

import (
	"context"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/llm"
)

const (
	incompleteTestPartialContent = "partial"
	incompleteTestRefusalReason  = "refusal"
	incompleteTestDoneContent    = "done"
	incompleteTestToolName       = "list_tasks"
	incompleteTestModel          = "test-model"
	incompleteTestPauseTurn      = "pause_turn"
	incompleteTestResponseIncomp = "response.incomplete"
	incompleteTestBareIncomplete = "incomplete"
)

func TestValidateToolLoopCompletionAcceptsMaxTokensTextResponse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		resp    *llm.CompletionResponse
		wantErr bool
	}{
		{name: "max_tokens text only", resp: &llm.CompletionResponse{Content: incompleteTestPartialContent, StopReason: oaiParamMaxTokens}},
		{name: "length text only", resp: &llm.CompletionResponse{Content: incompleteTestPartialContent, StopReason: oaiStopReasonLength}},
		{name: "MAX_TOKENS is case-insensitive", resp: &llm.CompletionResponse{Content: incompleteTestPartialContent, StopReason: "MAX_TOKENS"}},
		{
			name:    "max_tokens with truncated tool call",
			resp:    &llm.CompletionResponse{StopReason: oaiParamMaxTokens, ToolCalls: []llm.ToolCall{{ID: acpPublicConsumerToolCallID, Name: incompleteTestToolName}}},
			wantErr: true,
		},
		{
			name:    "max_tokens with text and truncated tool call",
			resp:    &llm.CompletionResponse{Content: incompleteTestPartialContent, StopReason: oaiParamMaxTokens, ToolCalls: []llm.ToolCall{{ID: acpPublicConsumerToolCallID, Name: incompleteTestToolName}}},
			wantErr: true,
		},
		{name: "max_tokens without text", resp: &llm.CompletionResponse{StopReason: oaiParamMaxTokens}, wantErr: true},
		{name: "max_tokens with whitespace-only text", resp: &llm.CompletionResponse{Content: " \n", StopReason: oaiParamMaxTokens}, wantErr: true},
		{name: "length without text", resp: &llm.CompletionResponse{StopReason: oaiStopReasonLength}, wantErr: true},
		{name: "bare incomplete with text", resp: &llm.CompletionResponse{Content: incompleteTestPartialContent, StopReason: incompleteTestBareIncomplete}, wantErr: true},
		{name: "response.incomplete with text", resp: &llm.CompletionResponse{Content: incompleteTestPartialContent, StopReason: incompleteTestResponseIncomp}, wantErr: true},
		{name: "pause_turn with text", resp: &llm.CompletionResponse{Content: incompleteTestPartialContent, StopReason: incompleteTestPauseTurn}, wantErr: true},
		{name: "blank stop reason with text", resp: &llm.CompletionResponse{Content: incompleteTestPartialContent, StopReason: ""}, wantErr: true},
		{name: "refusal outcome", resp: &llm.CompletionResponse{StopReason: incompleteTestRefusalReason}, wantErr: true},
		{name: "end of turn", resp: &llm.CompletionResponse{Content: incompleteTestDoneContent, StopReason: oaiStopReasonEndTurn}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateToolLoopCompletion(tc.resp)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateToolLoopCompletion() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// A text-only response truncated by the output token budget must terminate
// the loop and reach the client verbatim: the premature-end safety net must
// not discard the partial text and issue more model calls.
func TestRunToolLoop_MaxTokensTextIsReturnedBeforePrematureEndRetry(t *testing.T) {
	t.Parallel()
	for _, stopReason := range []string{oaiParamMaxTokens, oaiStopReasonLength} {
		t.Run(stopReason, func(t *testing.T) {
			t.Parallel()
			mock := &mockAnthropicProvider{
				responses: []*llm.CompletionResponse{
					{Content: incompleteTestPartialContent, StopReason: stopReason},
					// A second call means the loop treated the truncation as a
					// premature end of turn and re-prompted.
					{Content: goalStateSentinel + "\nunexpected", StopReason: oaiStopReasonEndTurn},
				},
			}
			req := &llm.CompletionRequest{
				Model:    incompleteTestModel,
				Messages: []llm.Message{{Role: testRoleUser, Content: "write a long report"}},
				Tools:    []llm.Tool{{Name: incompleteTestToolName}},
			}
			var finals []string
			observer := &toolLoopObserver{
				OnFinalContent:      func(content string) { finals = append(finals, content) },
				OnPrematureEndRetry: func() { t.Fatal("premature-end retry must not run for a max_tokens truncation") },
			}

			resp, err := runToolLoopWithObserver(context.Background(), mock, req, incompleteTestModel,
				ChatConfig{MaxIterations: 20, ToolTimeout: 30 * time.Second, MaxPrematureEndRetries: 3}, nil, observer)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp.Content != incompleteTestPartialContent || resp.StopReason != stopReason {
				t.Fatalf("response = %q/%q, want %q/%q", resp.Content, resp.StopReason, incompleteTestPartialContent, stopReason)
			}
			if mock.callIdx != 1 {
				t.Fatalf("expected exactly 1 LLM call, got %d", mock.callIdx)
			}
			if len(finals) != 1 || finals[0] != incompleteTestPartialContent {
				t.Fatalf("observer final content = %v, want [%q]", finals, incompleteTestPartialContent)
			}
		})
	}
}

// Other incomplete outcomes still fail the loop instead of being returned as
// if the turn completed.
func TestRunToolLoop_NonBudgetIncompleteStillFails(t *testing.T) {
	t.Parallel()
	for _, stopReason := range []string{incompleteTestPauseTurn, incompleteTestResponseIncomp, incompleteTestBareIncomplete, ""} {
		t.Run("stop="+stopReason, func(t *testing.T) {
			t.Parallel()
			mock := &mockAnthropicProvider{
				responses: []*llm.CompletionResponse{{Content: incompleteTestPartialContent, StopReason: stopReason}},
			}
			req := &llm.CompletionRequest{
				Model:    incompleteTestModel,
				Messages: []llm.Message{{Role: testRoleUser, Content: "hi"}},
			}
			_, err := runToolLoopWithObserver(context.Background(), mock, req, incompleteTestModel,
				ChatConfig{MaxIterations: 20, ToolTimeout: 30 * time.Second, MaxPrematureEndRetries: 3}, nil, nil)
			if err == nil {
				t.Fatalf("expected an error for stop reason %q", stopReason)
			}
			if mock.callIdx != 1 {
				t.Fatalf("expected exactly 1 LLM call, got %d", mock.callIdx)
			}
		})
	}
}
