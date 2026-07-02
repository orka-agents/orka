/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/orka-agents/orka/internal/llm"
)

type streamUsageProvider struct{}

func (streamUsageProvider) Name() string { return "stream-usage" }
func (streamUsageProvider) Complete(context.Context, *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	return nil, nil
}
func (streamUsageProvider) Stream(context.Context, *llm.CompletionRequest) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk, 3)
	ch <- llm.StreamChunk{Content: "hello", Model: "stream-model", Provider: "anthropic"}
	ch <- llm.StreamChunk{ToolCall: &llm.ToolCall{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{}`)}}
	ch <- llm.StreamChunk{Done: true, StopReason: oaiStopReasonToolUse, InputTokens: 12, OutputTokens: 7, Model: "stream-model", Provider: "anthropic"}
	close(ch)
	return ch, nil
}

func TestCompleteViaStreamPreservesTerminalUsageMetadata(t *testing.T) {
	resp, err := completeViaStream(context.Background(), streamUsageProvider{}, &llm.CompletionRequest{Model: "stream-model"})
	if err != nil {
		t.Fatalf("completeViaStream() error = %v", err)
	}
	if resp.Content != "hello" || len(resp.ToolCalls) != 1 || resp.StopReason != oaiStopReasonToolUse {
		t.Fatalf("unexpected response = %#v", resp)
	}
	if resp.InputTokens != 12 || resp.OutputTokens != 7 {
		t.Fatalf("usage = input:%d output:%d, want input:12 output:7", resp.InputTokens, resp.OutputTokens)
	}
	if resp.Model != "stream-model" || resp.Provider != "anthropic" {
		t.Fatalf("metadata = model:%q provider:%q", resp.Model, resp.Provider)
	}
}
