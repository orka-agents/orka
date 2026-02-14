/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package llm

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
		note := Message{
			Role:    "system",
			Content: "[Earlier messages truncated. Use list_tasks to check what has already been done.]",
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
