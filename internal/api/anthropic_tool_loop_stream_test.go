/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/orka-agents/orka/internal/llm"
)

type streamUsageProvider struct{}

func (streamUsageProvider) Name() string { return "stream-usage" }
func (streamUsageProvider) Complete(context.Context, *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	return nil, nil
}

type streamChunksProvider struct {
	chunks []llm.StreamChunk
	err    error
}

func (streamChunksProvider) Name() string { return "stream-chunks" }
func (streamChunksProvider) Complete(context.Context, *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	return nil, nil
}
func (p streamChunksProvider) Stream(context.Context, *llm.CompletionRequest) (<-chan llm.StreamChunk, error) {
	if p.err != nil {
		return nil, p.err
	}
	ch := make(chan llm.StreamChunk, len(p.chunks))
	for _, chunk := range p.chunks {
		ch <- chunk
	}
	close(ch)
	return ch, nil
}

func TestCompleteViaStreamPreservesOpenErrorChain(t *testing.T) {
	providerErr := &llm.ProviderError{StatusCode: 400, Message: "context length exceeded"}
	_, err := completeViaStream(context.Background(), streamChunksProvider{err: providerErr}, &llm.CompletionRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, errStreamUnavailable) {
		t.Fatalf("error = %v, want stream-unavailable sentinel", err)
	}
	if !llm.IsContextTooLongErr(err) {
		t.Fatalf("error = %v, want context-too-long provider cause", err)
	}
}

func TestCompleteViaStreamMarksInitialChunkErrorFallbackEligible(t *testing.T) {
	providerErr := &llm.ProviderError{StatusCode: 503, Message: "stream unavailable"}
	_, err := completeViaStream(context.Background(), streamChunksProvider{
		chunks: []llm.StreamChunk{{Error: providerErr, Done: true}},
	}, &llm.CompletionRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, errStreamUnavailable) {
		t.Fatalf("error = %v, want stream-unavailable sentinel", err)
	}
	if !errors.Is(err, providerErr) {
		t.Fatalf("error = %v, want provider cause", err)
	}
}

func TestCompleteViaStreamRejectsChunkErrorAfterOutput(t *testing.T) {
	providerErr := &llm.ProviderError{StatusCode: 503, Message: "stream interrupted"}
	_, err := completeViaStream(context.Background(), streamChunksProvider{
		chunks: []llm.StreamChunk{{Content: "partial"}, {Error: providerErr, Done: true}},
	}, &llm.CompletionRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, errStreamUnavailable) {
		t.Fatalf("error = %v, must not allow fallback after partial output", err)
	}
	if !errors.Is(err, providerErr) {
		t.Fatalf("error = %v, want provider cause", err)
	}
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

func TestCompleteViaStreamRejectsMissingTerminalOutcome(t *testing.T) {
	tests := []struct {
		name   string
		chunks []llm.StreamChunk
	}{
		{name: "channel closes without terminal", chunks: []llm.StreamChunk{{Content: "partial"}}},
		{name: "terminal missing reason", chunks: []llm.StreamChunk{{Done: true}}},
		{name: "failed terminal", chunks: []llm.StreamChunk{{Done: true, StopReason: "failed"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := completeViaStream(context.Background(), streamChunksProvider{chunks: tt.chunks}, &llm.CompletionRequest{})
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
