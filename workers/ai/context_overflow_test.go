/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/llm"
)

type contextOverflowCaptureProvider struct {
	requests       []*llm.CompletionRequest
	captureErr     error
	alwaysOverflow bool
	validateRetry  func(*llm.CompletionRequest) error
}

func (p *contextOverflowCaptureProvider) Complete(
	_ context.Context,
	req *llm.CompletionRequest,
) (*llm.CompletionResponse, error) {
	serialized, err := json.Marshal(req)
	if err != nil {
		p.captureErr = err
		return nil, err
	}
	var captured llm.CompletionRequest
	if err := json.Unmarshal(serialized, &captured); err != nil {
		p.captureErr = err
		return nil, err
	}
	p.requests = append(p.requests, &captured)
	if len(p.requests) == 1 || p.alwaysOverflow {
		return nil, &llm.ProviderError{StatusCode: 400, Message: "context length exceeded"}
	}
	if p.validateRetry != nil {
		if err := p.validateRetry(&captured); err != nil {
			return nil, err
		}
	}
	return &llm.CompletionResponse{Content: "recovered", StopReason: "end_turn"}, nil
}

func (p *contextOverflowCaptureProvider) Stream(
	context.Context,
	*llm.CompletionRequest,
) (<-chan llm.StreamChunk, error) {
	return nil, fmt.Errorf("stream not implemented")
}

func (p *contextOverflowCaptureProvider) Name() string { return "context-overflow-capture" }

func TestExecuteAgentLoopContextOverflowShrinksSingleCurrentPrompt(t *testing.T) {
	provider := &contextOverflowCaptureProvider{}
	prompt := "BEGIN-CURRENT-TASK\n" + strings.Repeat("details ", 2000) + "\nEND-CURRENT-TASK"

	result, err := executeAgentLoop(
		context.Background(), provider,
		[]llm.Message{{Role: roleUser, Content: prompt}},
		"system", "model", nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("executeAgentLoop() error = %v", err)
	}
	if result != "recovered" {
		t.Fatalf("result = %q, want recovered", result)
	}
	if provider.captureErr != nil {
		t.Fatalf("capture request: %v", provider.captureErr)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(provider.requests))
	}

	before, err := llm.EstimateCompletionRequestSize(provider.requests[0])
	if err != nil {
		t.Fatalf("estimate first request: %v", err)
	}
	after, err := llm.EstimateCompletionRequestSize(provider.requests[1])
	if err != nil {
		t.Fatalf("estimate retry request: %v", err)
	}
	if after.SerializedBytes >= before.SerializedBytes {
		t.Fatalf("retry bytes = %d, first bytes = %d; retry did not shrink", after.SerializedBytes, before.SerializedBytes)
	}
	gotPrompt := provider.requests[1].Messages[0].Content
	if !strings.Contains(gotPrompt, "[Current user task truncated for context recovery.]") {
		t.Fatalf("retry prompt lacks truncation marker: %q", gotPrompt)
	}
	if !strings.Contains(gotPrompt, "BEGIN-CURRENT-TASK") || !strings.Contains(gotPrompt, "END-CURRENT-TASK") {
		t.Fatalf("retry prompt lost current task boundaries: %q", gotPrompt)
	}
}

func TestExecuteAgentLoopContextOverflowRefusesNonShrinkingRetry(t *testing.T) {
	provider := &contextOverflowCaptureProvider{alwaysOverflow: true}
	prompt := "BEGIN-CURRENT-TASK\n" + strings.Repeat("important details ", 100) + "\nEND-CURRENT-TASK"

	_, err := executeAgentLoop(
		context.Background(), provider,
		[]llm.Message{{Role: roleUser, Content: prompt}},
		strings.Repeat("fixed-system ", 2000), "model",
		[]llm.Tool{{
			Name:        "fixed_tool",
			Description: strings.Repeat("fixed-description ", 1000),
			Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		}},
		nil, nil,
	)
	if err == nil {
		t.Fatal("executeAgentLoop() error = nil, want context overflow failure")
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1 when retry request cannot shrink", len(provider.requests))
	}
}

func TestExecuteAgentLoopContextOverflowAccountsForToolSchemasAndArguments(t *testing.T) {
	const currentTask = "CURRENT-TASK-SENTINEL: keep this request"
	provider := &contextOverflowCaptureProvider{
		validateRetry: func(req *llm.CompletionRequest) error {
			for _, message := range req.Messages {
				if message.Role == roleUser && message.Content == currentTask {
					return nil
				}
			}
			return fmt.Errorf("retry dropped the current user task")
		},
	}
	largeArguments := json.RawMessage(fmt.Sprintf(`{"payload":%q}`, strings.Repeat("argument ", 2000)))
	messages := []llm.Message{
		{Role: roleUser, Content: "old history"},
		{
			Role: "assistant",
			ToolCalls: []llm.ToolCall{{
				ID:        "old-call",
				Name:      "large_tool",
				Arguments: largeArguments,
			}},
		},
		{Role: "tool", ToolCallID: "old-call", Content: "old result"},
		{Role: roleUser, Content: currentTask},
	}
	tools := []llm.Tool{{
		Name:        "large_tool",
		Description: strings.Repeat("description ", 1000),
		Parameters: json.RawMessage(fmt.Sprintf(
			`{"type":"object","description":%q,"properties":{}}`, strings.Repeat("schema ", 1200),
		)),
	}}

	result, err := executeAgentLoop(
		context.Background(), provider, messages,
		strings.Repeat("system ", 1000), "model", tools, nil, nil,
	)
	if err != nil {
		t.Fatalf("executeAgentLoop() error = %v", err)
	}
	if result != "recovered" {
		t.Fatalf("result = %q, want recovered", result)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(provider.requests))
	}
	before, err := llm.EstimateCompletionRequestSize(provider.requests[0])
	if err != nil {
		t.Fatalf("estimate first request: %v", err)
	}
	after, err := llm.EstimateCompletionRequestSize(provider.requests[1])
	if err != nil {
		t.Fatalf("estimate retry request: %v", err)
	}
	if after.SerializedBytes >= before.SerializedBytes {
		t.Fatalf("retry bytes = %d, first bytes = %d", after.SerializedBytes, before.SerializedBytes)
	}
	if len(provider.requests[1].Tools) != 1 || provider.requests[1].SystemPrompt == "" {
		t.Fatal("retry unexpectedly removed fixed system prompt or tool schemas")
	}
}
