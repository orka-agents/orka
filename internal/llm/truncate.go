/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package llm

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

func estimateTokens(text string) int {
	return (len(text) + 3) / 4
}

func estimateMessageTokens(m Message) int {
	tokens := estimateTokens(m.Content)
	for _, tc := range m.ToolCalls {
		tokens += estimateTokens(tc.Name) + estimateTokens(string(tc.Arguments))
	}
	return tokens
}

type messageBlock struct {
	messages []Message
	tokens   int
}

func groupMessageBlocks(messages []Message) []messageBlock {
	var blocks []messageBlock
	i := 0
	for i < len(messages) {
		m := messages[i]
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			block := messageBlock{messages: []Message{m}, tokens: estimateMessageTokens(m)}
			i++
			for i < len(messages) && messages[i].Role == "tool" {
				block.messages = append(block.messages, messages[i])
				block.tokens += estimateMessageTokens(messages[i])
				i++
			}
			blocks = append(blocks, block)
		} else {
			blocks = append(blocks, messageBlock{
				messages: []Message{m},
				tokens:   estimateMessageTokens(m),
			})
			i++
		}
	}
	return blocks
}

const cutsPerBudget = 4

func TruncateMessages(messages []Message, tokenBudget int) []Message {
	if len(messages) == 0 {
		return messages
	}

	totalTokens := 0
	for _, m := range messages {
		totalTokens += estimateMessageTokens(m)
	}
	if totalTokens <= tokenBudget {
		return messages
	}

	first := messages[0]
	firstTokens := estimateMessageTokens(first)
	remaining := tokenBudget - firstTokens
	if remaining <= 0 {
		return []Message{first}
	}

	blocks := groupMessageBlocks(messages[1:])

	if cut, ok := stableCut(blocks, remaining, tokenBudget); ok {
		result := []Message{first, {Role: "system", Content: extractDroppedSummary(blocks[:cut])}}
		for _, b := range blocks[cut:] {
			result = append(result, b.messages...)
		}
		return result
	}

	var kept []messageBlock
	for _, block := range slices.Backward(blocks) {
		if remaining-block.tokens < 0 {
			break
		}
		remaining -= block.tokens
		kept = append([]messageBlock{block}, kept...)
	}

	droppedBlocks := len(blocks) - len(kept)
	if droppedBlocks > 0 {
		noteContent := extractDroppedSummary(blocks[:droppedBlocks])
		noteTokens := estimateTokens(noteContent)

		for noteTokens > remaining && len(kept) > 0 {
			remaining += kept[0].tokens
			droppedBlocks++
			kept = kept[1:]
			noteContent = extractDroppedSummary(blocks[:droppedBlocks])
			noteTokens = estimateTokens(noteContent)
		}

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

func stableCut(blocks []messageBlock, remaining, tokenBudget int) (int, bool) {
	quarter := max(tokenBudget/cutsPerBudget, 1)
	total := 0
	for _, b := range blocks {
		total += b.tokens
	}
	before := 0
	for i := 1; i < len(blocks); i++ {
		prev := before
		before += blocks[i-1].tokens
		if before/quarter == prev/quarter || total-before > remaining {
			continue
		}
		if total-before+estimateTokens(extractDroppedSummary(blocks[:i])) <= remaining {
			return i, true
		}
	}
	return 0, false
}

const maxContextPerTool = 5

const maxValueLen = 80

func truncateValue(s string) string {
	if len(s) <= maxValueLen {
		return s
	}
	return s[:maxValueLen] + "…"
}

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

	toolNames := make([]string, 0, len(tools))
	for name := range tools {
		toolNames = append(toolNames, name)
	}
	sort.Strings(toolNames)

	var parts []string
	for _, name := range toolNames {
		info := tools[name]
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
