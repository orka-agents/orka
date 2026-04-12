package llm

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func Test_estimateTokens(t *testing.T) {
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
			if got := estimateTokens(tt.text); got != tt.want {
				t.Errorf("estimateTokens(%q) = %d, want %d", tt.text, got, tt.want)
			}
		})
	}
}

func Test_estimateMessageTokens(t *testing.T) {
	tests := []struct {
		name string
		msg  Message
		want int
	}{
		{
			name: "content only",
			msg:  Message{Content: "hello world!"},
			want: estimateTokens("hello world!"),
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
			want: estimateTokens("text") + estimateTokens("search") + estimateTokens(`{"q":"hi"}`),
		},
		{
			name: "multiple tool calls",
			msg: Message{
				ToolCalls: []ToolCall{
					{Name: "a", Arguments: json.RawMessage(`{}`)},
					{Name: "bb", Arguments: json.RawMessage(`{"x":1}`)},
				},
			},
			want: estimateTokens("a") + estimateTokens("{}") + estimateTokens("bb") + estimateTokens(`{"x":1}`),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := estimateMessageTokens(tt.msg); got != tt.want {
				t.Errorf("estimateMessageTokens() = %d, want %d", got, tt.want)
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
		// Budget large enough for first + enriched note + last message, but not all
		result := TruncateMessages(msgs, 40)
		if len(result) < 2 {
			t.Fatalf("expected at least 2 messages, got %d", len(result))
		}
		if result[0].Content != "system prompt" {
			t.Error("first message should be preserved")
		}
		// Should have a truncation note
		if result[1].Role != "system" {
			t.Error("expected truncation note to have system role")
		}
		if !strings.Contains(result[1].Content, "truncated") {
			t.Error("expected truncation note to contain 'truncated'")
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

	t.Run("truncation with tool calls produces enriched note", func(t *testing.T) {
		msgs := []Message{
			{Role: "user", Content: "system prompt"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "1", Name: "file_read", Arguments: json.RawMessage(`{"path":"main.go"}`)}}},
			{Role: "tool", Content: "package main", ToolCallID: "1"},
			{Role: "user", Content: strings.Repeat("x", 400)}, // ~100 tokens
			{Role: "user", Content: "last"},
		}
		// Budget large enough for first + enriched note + last, but not the big middle block
		result := TruncateMessages(msgs, 60)
		if len(result) < 2 {
			t.Fatalf("expected at least 2 messages, got %d", len(result))
		}
		if result[1].Role != "system" {
			t.Error("expected system role for truncation note")
		}
		if !strings.Contains(result[1].Content, "truncated") {
			t.Error("expected truncation note to contain 'truncated'")
		}
		if !strings.Contains(result[1].Content, "file_read") {
			t.Error("expected truncation note to contain 'file_read'")
		}
		if !strings.Contains(result[1].Content, "main.go") {
			t.Error("expected truncation note to contain 'main.go'")
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

func TestExtractDroppedSummary(t *testing.T) {
	t.Run("empty drops", func(t *testing.T) {
		result := extractDroppedSummary(nil)
		if !strings.Contains(result, "0 messages dropped") {
			t.Errorf("expected '0 messages dropped', got %q", result)
		}
	})

	t.Run("user messages only no tool calls", func(t *testing.T) {
		blocks := []messageBlock{
			{messages: []Message{{Role: "user", Content: "hello"}}, tokens: 2},
			{messages: []Message{{Role: "assistant", Content: "hi there"}}, tokens: 3},
		}
		result := extractDroppedSummary(blocks)
		if !strings.Contains(result, "messages dropped") {
			t.Errorf("expected 'messages dropped', got %q", result)
		}
		if strings.Contains(result, "tool-call exchanges") {
			t.Errorf("should not mention tool-call exchanges for non-tool messages, got %q", result)
		}
	})

	t.Run("single tool call with file path", func(t *testing.T) {
		blocks := []messageBlock{
			{
				messages: []Message{
					{Role: "assistant", ToolCalls: []ToolCall{{ID: "1", Name: "file_read", Arguments: json.RawMessage(`{"path":"main.go"}`)}}},
					{Role: "tool", Content: "package main", ToolCallID: "1"},
				},
				tokens: 10,
			},
		}
		result := extractDroppedSummary(blocks)
		if !strings.Contains(result, "file_read(main.go)") {
			t.Errorf("expected 'file_read(main.go)', got %q", result)
		}
		if !strings.Contains(result, "tool-call exchanges") {
			t.Errorf("expected 'tool-call exchanges', got %q", result)
		}
	})

	t.Run("multiple tool calls with deduplication", func(t *testing.T) {
		blocks := []messageBlock{
			{
				messages: []Message{
					{Role: "assistant", ToolCalls: []ToolCall{{ID: "1", Name: "file_read", Arguments: json.RawMessage(`{"path":"main.go"}`)}}},
					{Role: "tool", Content: "content1", ToolCallID: "1"},
				},
				tokens: 10,
			},
			{
				messages: []Message{
					{Role: "assistant", ToolCalls: []ToolCall{{ID: "2", Name: "file_read", Arguments: json.RawMessage(`{"path":"utils.go"}`)}}},
					{Role: "tool", Content: "content2", ToolCallID: "2"},
				},
				tokens: 10,
			},
		}
		result := extractDroppedSummary(blocks)
		if !strings.Contains(result, "file_read(") {
			t.Errorf("expected 'file_read(' in result, got %q", result)
		}
		if !strings.Contains(result, "main.go") {
			t.Errorf("expected 'main.go' in result, got %q", result)
		}
		if !strings.Contains(result, "utils.go") {
			t.Errorf("expected 'utils.go' in result, got %q", result)
		}
	})

	t.Run("tool calls without extractable context", func(t *testing.T) {
		blocks := []messageBlock{
			{
				messages: []Message{
					{Role: "assistant", ToolCalls: []ToolCall{{ID: "1", Name: "code_exec", Arguments: json.RawMessage(`{"language":"bash","code":"echo hi"}`)}}},
					{Role: "tool", Content: "hi", ToolCallID: "1"},
				},
				tokens: 10,
			},
		}
		result := extractDroppedSummary(blocks)
		if !strings.Contains(result, "code_exec(1x)") {
			t.Errorf("expected 'code_exec(1x)', got %q", result)
		}
	})

	t.Run("mixed tool calls", func(t *testing.T) {
		blocks := []messageBlock{
			{
				messages: []Message{
					{Role: "assistant", ToolCalls: []ToolCall{{ID: "1", Name: "file_read", Arguments: json.RawMessage(`{"path":"main.go"}`)}}},
					{Role: "tool", Content: "pkg", ToolCallID: "1"},
				},
				tokens: 10,
			},
			{
				messages: []Message{
					{Role: "assistant", ToolCalls: []ToolCall{{ID: "2", Name: "web_search", Arguments: json.RawMessage(`{"query":"kubernetes pods"}`)}}},
					{Role: "tool", Content: "results", ToolCallID: "2"},
				},
				tokens: 10,
			},
			{
				messages: []Message{
					{Role: "assistant", ToolCalls: []ToolCall{{ID: "3", Name: "web_fetch", Arguments: json.RawMessage(`{"url":"https://example.com"}`)}}},
					{Role: "tool", Content: "page", ToolCallID: "3"},
				},
				tokens: 10,
			},
			{
				messages: []Message{
					{Role: "assistant", ToolCalls: []ToolCall{{ID: "4", Name: "code_exec", Arguments: json.RawMessage(`{"language":"python","code":"print(1)"}`)}}},
					{Role: "tool", Content: "1", ToolCallID: "4"},
				},
				tokens: 10,
			},
		}
		result := extractDroppedSummary(blocks)
		if !strings.Contains(result, "file_read(main.go)") {
			t.Errorf("expected 'file_read(main.go)', got %q", result)
		}
		if !strings.Contains(result, "web_search(kubernetes pods)") {
			t.Errorf("expected 'web_search(kubernetes pods)', got %q", result)
		}
		if !strings.Contains(result, "web_fetch(https://example.com)") {
			t.Errorf("expected 'web_fetch(https://example.com)', got %q", result)
		}
		if !strings.Contains(result, "code_exec(1x)") {
			t.Errorf("expected 'code_exec(1x)', got %q", result)
		}
		if !strings.Contains(result, "Tools used:") {
			t.Errorf("expected 'Tools used:', got %q", result)
		}
	})

	t.Run("unparseable JSON args", func(t *testing.T) {
		blocks := []messageBlock{
			{
				messages: []Message{
					{Role: "assistant", ToolCalls: []ToolCall{{ID: "1", Name: "broken_tool", Arguments: json.RawMessage(`{invalid json`)}}},
					{Role: "tool", Content: "error", ToolCallID: "1"},
				},
				tokens: 10,
			},
		}
		// Should not panic
		result := extractDroppedSummary(blocks)
		if !strings.Contains(result, "broken_tool") {
			t.Errorf("expected 'broken_tool' in result, got %q", result)
		}
		if !strings.Contains(result, "1x") {
			t.Errorf("expected count fallback '1x', got %q", result)
		}
	})

	t.Run("url extraction", func(t *testing.T) {
		blocks := []messageBlock{
			{
				messages: []Message{
					{Role: "assistant", ToolCalls: []ToolCall{{ID: "1", Name: "web_fetch", Arguments: json.RawMessage(`{"url":"https://example.com"}`)}}},
					{Role: "tool", Content: "page content", ToolCallID: "1"},
				},
				tokens: 10,
			},
		}
		result := extractDroppedSummary(blocks)
		if !strings.Contains(result, "web_fetch(https://example.com)") {
			t.Errorf("expected 'web_fetch(https://example.com)', got %q", result)
		}
	})

	t.Run("query extraction", func(t *testing.T) {
		blocks := []messageBlock{
			{
				messages: []Message{
					{Role: "assistant", ToolCalls: []ToolCall{{ID: "1", Name: "web_search", Arguments: json.RawMessage(`{"query":"kubernetes pods"}`)}}},
					{Role: "tool", Content: "search results", ToolCallID: "1"},
				},
				tokens: 10,
			},
		}
		result := extractDroppedSummary(blocks)
		if !strings.Contains(result, "web_search(kubernetes pods)") {
			t.Errorf("expected 'web_search(kubernetes pods)', got %q", result)
		}
	})

	t.Run("tool call counts", func(t *testing.T) {
		blocks := []messageBlock{
			{
				messages: []Message{
					{Role: "assistant", ToolCalls: []ToolCall{{ID: "1", Name: "sometool", Arguments: json.RawMessage(`{"x":1}`)}}},
					{Role: "tool", Content: "r1", ToolCallID: "1"},
				},
				tokens: 5,
			},
			{
				messages: []Message{
					{Role: "assistant", ToolCalls: []ToolCall{{ID: "2", Name: "sometool", Arguments: json.RawMessage(`{"x":2}`)}}},
					{Role: "tool", Content: "r2", ToolCallID: "2"},
				},
				tokens: 5,
			},
			{
				messages: []Message{
					{Role: "assistant", ToolCalls: []ToolCall{{ID: "3", Name: "sometool", Arguments: json.RawMessage(`{"x":3}`)}}},
					{Role: "tool", Content: "r3", ToolCallID: "3"},
				},
				tokens: 5,
			},
		}
		result := extractDroppedSummary(blocks)
		if !strings.Contains(result, "sometool(3x)") {
			t.Errorf("expected 'sometool(3x)', got %q", result)
		}
	})

	t.Run("context items capped per tool", func(t *testing.T) {
		// Create a tool with more files than maxContextPerTool (5)
		args := make([]ToolCall, 8)
		blockMsgs := make([]Message, 0, len(args)*2)
		for i := range 8 {
			args[i] = ToolCall{
				ID:        fmt.Sprintf("c%d", i),
				Name:      "file_read",
				Arguments: json.RawMessage(fmt.Sprintf(`{"path":"file%d.go"}`, i)),
			}
			blockMsgs = append(blockMsgs,
				Message{Role: "assistant", ToolCalls: []ToolCall{args[i]}},
				Message{Role: "tool", Content: "ok", ToolCallID: args[i].ID},
			)
		}
		blocks := []messageBlock{{messages: blockMsgs, tokens: 50}}
		result := extractDroppedSummary(blocks)
		if !strings.Contains(result, "…+3 more") {
			t.Errorf("expected overflow indicator '…+3 more', got %q", result)
		}
		if !strings.Contains(result, "file_read(") {
			t.Errorf("expected 'file_read(' prefix, got %q", result)
		}
	})

	t.Run("long values truncated", func(t *testing.T) {
		longPath := strings.Repeat("a", 120) + ".go"
		blocks := []messageBlock{
			{
				messages: []Message{
					{Role: "assistant", ToolCalls: []ToolCall{{ID: "1", Name: "file_read", Arguments: json.RawMessage(fmt.Sprintf(`{"path":%q}`, longPath))}}},
					{Role: "tool", Content: "ok", ToolCallID: "1"},
				},
				tokens: 10,
			},
		}
		result := extractDroppedSummary(blocks)
		if strings.Contains(result, longPath) {
			t.Error("expected long path to be truncated")
		}
		if !strings.Contains(result, "…") {
			t.Errorf("expected truncation marker '…', got %q", result)
		}
	})
}
