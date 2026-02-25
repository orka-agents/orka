/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sozercan/orka/internal/llm"
)

func TestTruncateMessages_UnderBudget(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
	}
	result := llm.TruncateMessages(msgs, 100000)
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
}

func TestTruncateMessages_EmptyInput(t *testing.T) {
	result := llm.TruncateMessages(nil, 1000)
	if len(result) != 0 {
		t.Fatalf("expected 0 messages, got %d", len(result))
	}
}

func TestTruncateMessages_DropsMiddleKeepsFirstAndRecent(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "original request"},            // ~4 tokens
		{Role: "assistant", Content: strings.Repeat("a", 100)}, // ~25 tokens — will be dropped
		{Role: "user", Content: strings.Repeat("b", 100)},      // ~25 tokens — will be dropped
		{Role: "assistant", Content: strings.Repeat("c", 100)}, // ~25 tokens
		{Role: "user", Content: "latest question"},             // ~4 tokens
	}
	// Budget: enough for first + note + last two, but not all messages
	// Total ~83 tokens. Budget 60 forces truncation but leaves room for note + recent blocks.
	result := llm.TruncateMessages(msgs, 60)

	if result[0].Content != "original request" {
		t.Errorf("first message should be preserved, got %q", result[0].Content)
	}
	if result[1].Role != "system" { //nolint:goconst // test string, not worth a constant
		t.Errorf("second message should be truncation note, got role %q", result[1].Role)
	}
	if !strings.Contains(result[1].Content, "truncated") {
		t.Errorf("truncation note should contain 'truncated', got %q", result[1].Content)
	}
	last := result[len(result)-1]
	if last.Content != "latest question" {
		t.Errorf("last message should be 'latest question', got %q", last.Content)
	}
}

func TestTruncateMessages_ToolCallsKeptAtomic(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "do something"},
		{Role: "assistant", Content: "", ToolCalls: []llm.ToolCall{
			{ID: "tc1", Name: "list_tasks", Arguments: json.RawMessage(`{}`)},
		}},
		{Role: "tool", ToolCallID: "tc1", Name: "list_tasks", Content: `{"tasks":[]}`},
		{Role: "assistant", Content: "done"},
		{Role: "user", Content: "now do more"},
	}
	// Budget large enough to keep everything
	result := llm.TruncateMessages(msgs, 100000)
	if len(result) != 5 {
		t.Fatalf("expected 5 messages, got %d", len(result))
	}

	// Budget that forces dropping the tool call block
	// first msg (~3 tokens) + last two (~5 tokens each) = ~13 tokens
	// tool call block is ~4 tokens for assistant + ~3 for result = ~7 tokens
	result = llm.TruncateMessages(msgs, 14)

	// Verify tool call and tool result are either both present or both absent
	hasToolCall := false
	hasToolResult := false
	for _, m := range result {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			hasToolCall = true
		}
		if m.Role == "tool" {
			hasToolResult = true
		}
	}
	if hasToolCall != hasToolResult {
		t.Error("tool call and tool result should be kept or dropped together")
	}

	// Verify truncation note content when truncation occurred
	for _, m := range result {
		if m.Role == "system" {
			if !strings.Contains(m.Content, "truncated") {
				t.Errorf("truncation note should contain 'truncated', got %q", m.Content)
			}
			if !strings.Contains(m.Content, "list_tasks") {
				t.Errorf("truncation note should contain 'list_tasks', got %q", m.Content)
			}
		}
	}
}

func TestTruncateMessages_BudgetTooSmallForAnythingButFirst(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "hello world this is a long message"},
		{Role: "assistant", Content: "response"},
	}
	result := llm.TruncateMessages(msgs, 1)
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	if result[0].Content != "hello world this is a long message" {
		t.Error("should keep first message even if over budget")
	}
}

func TestEstimateMessageTokens_IncludesToolCalls(t *testing.T) {
	m := llm.Message{
		Content: "test",
		ToolCalls: []llm.ToolCall{
			{Name: "my_tool", Arguments: json.RawMessage(`{"key":"value"}`)},
		},
	}
	tokens := llm.EstimateMessageTokens(m)
	contentOnly := llm.EstimateTokens("test")
	if tokens <= contentOnly {
		t.Error("token count should include tool call content")
	}
}
