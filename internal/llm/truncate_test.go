package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name string
		text string
		want int
	}{
		{"empty string", "", 0},
		{"1 char", "a", 1},
		{"4 chars", "abcd", 1},
		{"5 chars", "abcde", 2},
		{"8 chars", "abcdefgh", 2},
		{"12 chars", "abcdefghijkl", 3},
		{"long text", strings.Repeat("x", 100), 25},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EstimateTokens(tt.text); got != tt.want {
				t.Errorf("EstimateTokens(%q) = %d, want %d", tt.text, got, tt.want)
			}
		})
	}
}

func TestEstimateMessageTokens(t *testing.T) {
	tests := []struct {
		name string
		msg  Message
		want int
	}{
		{
			name: "content only",
			msg:  Message{Content: "hello world!"},
			want: EstimateTokens("hello world!"),
		},
		{
			name: "empty message",
			msg:  Message{},
			want: 0,
		},
		{
			name: "with tool calls",
			msg: Message{
				Content: "text",
				ToolCalls: []ToolCall{
					{Name: "search", Arguments: json.RawMessage(`{"q":"hi"}`)},
				},
			},
			want: EstimateTokens("text") + EstimateTokens("search") + EstimateTokens(`{"q":"hi"}`),
		},
		{
			name: "multiple tool calls",
			msg: Message{
				ToolCalls: []ToolCall{
					{Name: "a", Arguments: json.RawMessage(`{}`)},
					{Name: "bb", Arguments: json.RawMessage(`{"x":1}`)},
				},
			},
			want: EstimateTokens("a") + EstimateTokens("{}") + EstimateTokens("bb") + EstimateTokens(`{"x":1}`),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EstimateMessageTokens(tt.msg); got != tt.want {
				t.Errorf("EstimateMessageTokens() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestGroupMessageBlocks(t *testing.T) {
	t.Run("no messages", func(t *testing.T) {
		blocks := groupMessageBlocks(nil)
		if len(blocks) != 0 {
			t.Errorf("expected 0 blocks, got %d", len(blocks))
		}
	})

	t.Run("simple messages no tool calls", func(t *testing.T) {
		msgs := []Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello"},
			{Role: "user", Content: "bye"},
		}
		blocks := groupMessageBlocks(msgs)
		if len(blocks) != 3 {
			t.Fatalf("expected 3 blocks, got %d", len(blocks))
		}
		for i, b := range blocks {
			if len(b.messages) != 1 {
				t.Errorf("block %d: expected 1 message, got %d", i, len(b.messages))
			}
		}
	})

	t.Run("assistant with tool calls groups with tool results", func(t *testing.T) {
		msgs := []Message{
			{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "1", Name: "search", Arguments: json.RawMessage(`{}`)}}},
			{Role: "tool", Content: "result1", ToolCallID: "1"},
			{Role: "tool", Content: "result2", ToolCallID: "2"},
			{Role: "user", Content: "next"},
		}
		blocks := groupMessageBlocks(msgs)
		if len(blocks) != 2 {
			t.Fatalf("expected 2 blocks, got %d", len(blocks))
		}
		// First block: assistant + 2 tool results
		if len(blocks[0].messages) != 3 {
			t.Errorf("first block: expected 3 messages, got %d", len(blocks[0].messages))
		}
		// Second block: user message
		if len(blocks[1].messages) != 1 {
			t.Errorf("second block: expected 1 message, got %d", len(blocks[1].messages))
		}
	})

	t.Run("assistant without tool calls not grouped", func(t *testing.T) {
		msgs := []Message{
			{Role: "assistant", Content: "no tools"},
			{Role: "tool", Content: "orphan", ToolCallID: "x"},
		}
		blocks := groupMessageBlocks(msgs)
		if len(blocks) != 2 {
			t.Fatalf("expected 2 blocks, got %d", len(blocks))
		}
	})
}

func TestTruncateMessages(t *testing.T) {
	t.Run("empty messages", func(t *testing.T) {
		result := TruncateMessages(nil, 100)
		if len(result) != 0 {
			t.Errorf("expected 0 messages, got %d", len(result))
		}
	})

	t.Run("within budget returns all", func(t *testing.T) {
		msgs := []Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello"},
		}
		result := TruncateMessages(msgs, 10000)
		if len(result) != 2 {
			t.Errorf("expected 2 messages, got %d", len(result))
		}
	})

	t.Run("budget too small keeps only first", func(t *testing.T) {
		msgs := []Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello there how are you doing today"},
		}
		// First message "hi" ≈ 1 token. Set budget to 1.
		result := TruncateMessages(msgs, 1)
		if len(result) != 1 {
			t.Fatalf("expected 1 message, got %d", len(result))
		}
		if result[0].Content != "hi" {
			t.Errorf("expected first message preserved, got %q", result[0].Content)
		}
	})

	t.Run("truncation adds system note", func(t *testing.T) {
		// Create messages where total exceeds budget
		msgs := []Message{
			{Role: "user", Content: "system prompt"},
			{Role: "assistant", Content: strings.Repeat("a", 100)}, // ~25 tokens
			{Role: "user", Content: strings.Repeat("b", 100)},      // ~25 tokens
			{Role: "assistant", Content: strings.Repeat("c", 100)}, // ~25 tokens
			{Role: "user", Content: "last"},                        // ~1 token
		}
		// Budget: first msg (~4 tokens) + last msg (~1 token) + system note
		// Set budget so only last message(s) fit after first
		result := TruncateMessages(msgs, 15)
		if len(result) < 2 {
			t.Fatalf("expected at least 2 messages, got %d", len(result))
		}
		if result[0].Content != "system prompt" {
			t.Error("first message should be preserved")
		}
		// Should have a truncation note
		if result[1].Role != "system" || !strings.Contains(result[1].Content, "truncated") {
			t.Error("expected truncation system note")
		}
	})

	t.Run("tool call groups kept atomically", func(t *testing.T) {
		msgs := []Message{
			{Role: "user", Content: "go"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "1", Name: "fn", Arguments: json.RawMessage(`{}`)}}},
			{Role: "tool", Content: "result", ToolCallID: "1"},
			{Role: "user", Content: "ok"},
		}
		// Budget large enough for everything
		result := TruncateMessages(msgs, 10000)
		if len(result) != 4 {
			t.Errorf("expected 4 messages, got %d", len(result))
		}
	})

	t.Run("single message within budget", func(t *testing.T) {
		msgs := []Message{
			{Role: "user", Content: "hello"},
		}
		result := TruncateMessages(msgs, 10000)
		if len(result) != 1 {
			t.Errorf("expected 1 message, got %d", len(result))
		}
	})
}
