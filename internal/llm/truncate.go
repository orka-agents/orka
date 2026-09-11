/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// estimateTokens returns an approximate token count (~4 chars per token).
func estimateTokens(text string) int {
	return (len(text) + 3) / 4
}

// estimateMessageTokens returns approximate tokens for a Message including tool call content.
func estimateMessageTokens(m Message) int {
	tokens := estimateTokens(m.Content)
	for _, tc := range m.ToolCalls {
		tokens += estimateTokens(tc.Name) + estimateTokens(string(tc.Arguments))
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

// ErrRequiredContextTooLarge means that normal instructions and the current
// request cannot fit without losing required content.
var ErrRequiredContextTooLarge = errors.New("required model context exceeds token budget")

// TruncateMessages preserves instructions and the latest user message, then
// keeps recent complete exchanges within the token budget. Callers that need a
// hard limit should use FitMessages. This compatibility wrapper leaves the
// context intact when it cannot be safely reduced.
func TruncateMessages(messages []Message, tokenBudget int) []Message {
	fitted, err := FitMessages(messages, tokenBudget)
	if err != nil {
		return messages
	}
	return fitted
}

// FitMessages keeps all system messages and the latest user message unchanged.
// Older exchanges are optional, and tool calls and results stay together.
func FitMessages(messages []Message, tokenBudget int) ([]Message, error) {
	currentRequestIndex := -1
	for i, message := range slices.Backward(messages) {
		if message.Role == "user" {
			currentRequestIndex = i
			break
		}
	}
	return FitMessagesKeeping(messages, tokenBudget, currentRequestIndex)
}

// FitMessagesKeeping pins the user request at currentRequestIndex even when
// newer user-role messages are control prompts. Use -1 if there is no current
// request. The returned messages retain their roles, contents, and order, except
// for an optional assistant reference note describing omitted history.
// tokenBudget covers message content only. Callers fitting a complete provider
// request should use FitRequestMessagesKeeping to include framing and reserves.
func FitMessagesKeeping(messages []Message, tokenBudget, currentRequestIndex int) ([]Message, error) {
	return fitMessagesKeeping(messages, tokenBudget, currentRequestIndex, false)
}

func fitMessagesKeeping(messages []Message, tokenBudget, currentRequestIndex int, includeFraming bool, requiredMessageIndexes ...int) ([]Message, error) {
	if currentRequestIndex < -1 || currentRequestIndex >= len(messages) {
		return nil, errors.New("current request index is outside model context")
	}
	if currentRequestIndex >= 0 && messages[currentRequestIndex].Role != "user" {
		return nil, errors.New("current request must retain its user role")
	}
	for _, index := range requiredMessageIndexes {
		if index < 0 || index >= len(messages) {
			return nil, errors.New("required message index is outside model context")
		}
	}

	blocks := groupMessageBlocks(messages)
	if includeFraming {
		for i := range blocks {
			for _, message := range blocks[i].messages {
				blocks[i].tokens += messageFramingTokens(message)
			}
		}
	}
	if err := validateToolBlocks(blocks); err != nil {
		return nil, err
	}
	kept, totalTokens, requiredTokens := requiredMessageBlocks(blocks, currentRequestIndex, requiredMessageIndexes...)
	if totalTokens <= tokenBudget {
		return messages, nil
	}
	if requiredTokens > tokenBudget {
		return nil, fmt.Errorf("%w: system instructions and current request need at least %d tokens; budget is %d",
			ErrRequiredContextTooLarge, requiredTokens, tokenBudget)
	}

	remaining := tokenBudget - requiredTokens
	for i, block := range slices.Backward(blocks) {
		if kept[i] {
			continue
		}
		if block.tokens > remaining {
			break
		}
		remaining -= block.tokens
		kept[i] = true
	}
	if includeFraming {
		remaining -= messageFramingTokens(Message{Role: contextRoleAssistant})
	}
	return messagesWithTruncationNote(blocks, kept, remaining), nil
}

func requiredMessageBlocks(blocks []messageBlock, currentRequestIndex int, requiredMessageIndexes ...int) ([]bool, int, int) {
	kept := make([]bool, len(blocks))
	totalTokens, requiredTokens, messageIndex := 0, 0, 0
	for i, block := range blocks {
		for _, message := range block.messages {
			if message.Role == "system" || messageIndex == currentRequestIndex || slices.Contains(requiredMessageIndexes, messageIndex) {
				kept[i] = true
			}
			messageIndex++
		}
		totalTokens += block.tokens
		if kept[i] {
			requiredTokens += block.tokens
		}
	}
	return kept, totalTokens, requiredTokens
}

func validateToolBlocks(blocks []messageBlock) error {
	for i, block := range blocks {
		first := block.messages[0]
		if first.Role == "tool" {
			return fmt.Errorf("invalid model context: tool result without a matching call in block %d", i)
		}
		if first.Role != "assistant" || len(first.ToolCalls) == 0 {
			continue
		}
		pending := make(map[string]bool, len(first.ToolCalls))
		for _, call := range first.ToolCalls {
			if call.ID == "" || pending[call.ID] {
				return fmt.Errorf("invalid model context: missing or duplicate tool call ID in block %d", i)
			}
			pending[call.ID] = true
		}
		for _, result := range block.messages[1:] {
			if !pending[result.ToolCallID] {
				return fmt.Errorf("invalid model context: tool result without a matching call in block %d", i)
			}
			delete(pending, result.ToolCallID)
		}
		if len(pending) > 0 {
			return fmt.Errorf("invalid model context: incomplete tool exchange in block %d", i)
		}
	}
	return nil
}

func messagesWithTruncationNote(blocks []messageBlock, kept []bool, remaining int) []Message {
	var dropped []messageBlock
	for i, block := range blocks {
		if !kept[i] {
			dropped = append(dropped, block)
		}
	}
	note := extractDroppedSummary(dropped)
	if estimateTokens(note) > remaining {
		note = "[Earlier messages truncated.]"
	}
	if estimateTokens(note) > remaining {
		note = ""
	}
	var result []Message
	for i, block := range blocks {
		if kept[i] {
			result = append(result, block.messages...)
		} else if note != "" {
			// A note is reference material. It cannot override instructions or
			// displace the current request or a complete retained exchange.
			result = append(result, Message{Role: "assistant", Content: note})
			note = ""
		}
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
