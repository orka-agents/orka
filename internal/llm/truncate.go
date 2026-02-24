/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package llm

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// EstimateTokens returns an approximate token count (~4 chars per token).
func EstimateTokens(text string) int {
	return (len(text) + 3) / 4
}

// EstimateMessageTokens returns approximate tokens for a Message including tool call content.
func EstimateMessageTokens(m Message) int {
	tokens := EstimateTokens(m.Content)
	for _, tc := range m.ToolCalls {
		tokens += EstimateTokens(tc.Name) + EstimateTokens(string(tc.Arguments))
	}
	return tokens
}

// messageBlock is a group of messages that must be kept or dropped together.
// An assistant message with tool calls and its corresponding tool results form one block.
type messageBlock struct {
	messages []Message
	tokens   int
}

// groupMessageBlocks splits messages into atomic blocks. An assistant message
// with tool calls is grouped with all immediately following tool-result messages.
func groupMessageBlocks(messages []Message) []messageBlock {
	var blocks []messageBlock
	i := 0
	for i < len(messages) {
		m := messages[i]
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			block := messageBlock{messages: []Message{m}, tokens: EstimateMessageTokens(m)}
			i++
			for i < len(messages) && messages[i].Role == "tool" {
				block.messages = append(block.messages, messages[i])
				block.tokens += EstimateMessageTokens(messages[i])
				i++
			}
			blocks = append(blocks, block)
		} else {
			blocks = append(blocks, messageBlock{
				messages: []Message{m},
				tokens:   EstimateMessageTokens(m),
			})
			i++
		}
	}
	return blocks
}

// TruncateMessages keeps the first message and the newest messages that fit
// within the token budget. Tool-call/tool-result groups are kept or dropped
// atomically so the LLM never sees orphaned tool results.
func TruncateMessages(messages []Message, tokenBudget int) []Message {
	if len(messages) == 0 {
		return messages
	}

	totalTokens := 0
	for _, m := range messages {
		totalTokens += EstimateMessageTokens(m)
	}
	if totalTokens <= tokenBudget {
		return messages
	}

	// Always keep the first message
	first := messages[0]
	firstTokens := EstimateMessageTokens(first)
	remaining := tokenBudget - firstTokens
	if remaining <= 0 {
		return []Message{first}
	}

	// Group remaining messages into atomic blocks
	blocks := groupMessageBlocks(messages[1:])

	// From the tail, collect blocks that fit
	var kept []messageBlock
	for i := len(blocks) - 1; i >= 0; i-- {
		if remaining-blocks[i].tokens < 0 {
			break
		}
		remaining -= blocks[i].tokens
		kept = append([]messageBlock{blocks[i]}, kept...)
	}

	// Count how many blocks we dropped
	droppedBlocks := len(blocks) - len(kept)
	if droppedBlocks > 0 {
		noteContent := extractDroppedSummary(blocks[:droppedBlocks])
		noteTokens := EstimateTokens(noteContent)

		// If the note doesn't fit, drop more kept blocks to make room
		for noteTokens > remaining && len(kept) > 0 {
			remaining += kept[0].tokens
			droppedBlocks++
			kept = kept[1:]
			noteContent = extractDroppedSummary(blocks[:droppedBlocks])
			noteTokens = EstimateTokens(noteContent)
		}

		// If the note still doesn't fit (very tight budget), use a minimal note
		if noteTokens > remaining {
			noteContent = "[Earlier messages truncated.]"
		}

		note := Message{
			Role:    "system",
			Content: noteContent,
		}
		result := []Message{first, note}
		for _, b := range kept {
			result = append(result, b.messages...)
		}
		return result
	}

	result := []Message{first}
	for _, b := range kept {
		result = append(result, b.messages...)
	}
	return result
}

// maxContextPerTool is the maximum number of context items (files, URLs, queries) shown per tool.
const maxContextPerTool = 5

// maxValueLen is the maximum character length for an individual extracted value.
const maxValueLen = 80

// truncateValue caps a string at maxValueLen, appending "…" if truncated.
func truncateValue(s string) string {
	if len(s) <= maxValueLen {
		return s
	}
	return s[:maxValueLen] + "…"
}

// extractDroppedSummary builds an enriched truncation note from dropped blocks,
// including tool names, file paths, URLs, and search queries.
func extractDroppedSummary(dropped []messageBlock) string {
	type toolInfo struct {
		count   int
		files   map[string]bool
		urls    map[string]bool
		queries map[string]bool
	}

	tools := make(map[string]*toolInfo)
	totalExchanges := len(dropped)

	for _, block := range dropped {
		for _, msg := range block.messages {
			if msg.Role != "assistant" {
				continue
			}
			for _, tc := range msg.ToolCalls {
				info, ok := tools[tc.Name]
				if !ok {
					info = &toolInfo{
						files:   make(map[string]bool),
						urls:    make(map[string]bool),
						queries: make(map[string]bool),
					}
					tools[tc.Name] = info
				}
				info.count++

				var args map[string]any
				if err := json.Unmarshal(tc.Arguments, &args); err != nil {
					continue
				}
				for _, key := range []string{"path", "file", "filename", "file_path"} {
					if v, ok := args[key].(string); ok && v != "" {
						info.files[v] = true
					}
				}
				if v, ok := args["url"].(string); ok && v != "" {
					info.urls[v] = true
				}
				if v, ok := args["query"].(string); ok && v != "" {
					info.queries[v] = true
				}
			}
		}
	}

	if len(tools) == 0 {
		totalMsgs := 0
		for _, b := range dropped {
			totalMsgs += len(b.messages)
		}
		return fmt.Sprintf("[Earlier conversation truncated (%d messages dropped). Use list_tasks to check completed work.]", totalMsgs)
	}

	// Build sorted tool summaries
	toolNames := make([]string, 0, len(tools))
	for name := range tools {
		toolNames = append(toolNames, name)
	}
	sort.Strings(toolNames)

	var parts []string
	for _, name := range toolNames {
		info := tools[name]
		// Collect context items
		var context []string
		for f := range info.files {
			context = append(context, truncateValue(f))
		}
		for u := range info.urls {
			context = append(context, truncateValue(u))
		}
		for q := range info.queries {
			context = append(context, truncateValue(q))
		}
		sort.Strings(context)

		if len(context) > 0 {
			if len(context) > maxContextPerTool {
				overflow := len(context) - maxContextPerTool
				context = append(context[:maxContextPerTool], fmt.Sprintf("…+%d more", overflow))
			}
			parts = append(parts, fmt.Sprintf("%s(%s)", name, strings.Join(context, ", ")))
		} else {
			parts = append(parts, fmt.Sprintf("%s(%dx)", name, info.count))
		}
	}

	return fmt.Sprintf("[Earlier conversation truncated (%d tool-call exchanges dropped).\nTools used: %s.\nUse list_tasks to check completed work.]",
		totalExchanges, strings.Join(parts, ", "))
}
