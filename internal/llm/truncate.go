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

// estimateTokens returns an approximate token count (~4 chars per token).
func estimateTokens(text string) int {
	return (len(text) + 3) / 4
}

// CompletionRequestSize describes the serialized size and approximate token
// count of a complete provider request.
type CompletionRequestSize struct {
	SerializedBytes int
	EstimatedTokens int
}

const (
	currentUserTaskTruncationMarker = "\n\n[Current user task truncated for context recovery.]\n\n"
	minimalRecoveryTruncationNote   = "[Earlier messages truncated for context recovery.]"
	recoveryRoleAssistant           = "assistant"
	recoveryRoleTool                = "tool"
)

// EstimateCompletionRequestSize estimates the complete serialized request,
// including system prompts, tool schemas, messages, and tool-call arguments.
func EstimateCompletionRequestSize(req *CompletionRequest) (CompletionRequestSize, error) {
	serialized, err := json.Marshal(req)
	if err != nil {
		return CompletionRequestSize{}, fmt.Errorf("marshal completion request: %w", err)
	}
	return CompletionRequestSize{
		SerializedBytes: len(serialized),
		EstimatedTokens: (len(serialized) + 3) / 4,
	}, nil
}

// TruncateCompletionRequest returns a copy of req reduced toward tokenBudget.
// The newest user message and newest complete post-task tool exchange are
// recovery anchors. If fixed request overhead makes the target impossible, the
// user task is still reduced with an explicit marker while preserving both ends.
func TruncateCompletionRequest(req *CompletionRequest, tokenBudget int) (*CompletionRequest, error) {
	if req == nil {
		return nil, fmt.Errorf("completion request is nil")
	}

	size, err := EstimateCompletionRequestSize(req)
	if err != nil {
		return nil, err
	}
	truncated, err := cloneCompletionRequest(req)
	if err != nil {
		return nil, err
	}
	if size.EstimatedTokens <= tokenBudget || len(truncated.Messages) == 0 {
		return truncated, nil
	}

	currentUserIndex := newestUserMessageIndex(truncated.Messages)
	if currentUserIndex < 0 {
		return truncated, nil
	}

	blocks, mandatoryBlock, newestEvidenceBlock := groupRecoveryMessageBlocks(
		truncated.Messages, currentUserIndex,
	)
	kept := make([]bool, len(blocks))
	for i := range kept {
		kept[i] = true
	}
	for size.EstimatedTokens > tokenBudget {
		drop := -1
		for i := range blocks {
			if kept[i] && i != mandatoryBlock && i != newestEvidenceBlock {
				drop = i
				break
			}
		}
		if drop < 0 {
			break
		}
		kept[drop] = false
		truncated.Messages, size, err = recoveryMessagesWithinBudget(truncated, blocks, kept, tokenBudget)
		if err != nil {
			return nil, err
		}
	}
	if size.EstimatedTokens <= tokenBudget {
		return truncated, nil
	}

	currentUserIndex = newestUserMessageIndex(truncated.Messages)
	if err := truncateCurrentUserPrompt(
		truncated, currentUserIndex, tokenBudget, newestEvidenceBlock >= 0,
	); err != nil {
		return nil, err
	}
	if newestEvidenceBlock < 0 {
		return truncated, nil
	}

	anchoredSize, err := EstimateCompletionRequestSize(truncated)
	if err != nil || anchoredSize.EstimatedTokens <= tokenBudget {
		return truncated, err
	}
	anchoredMessages := append([]Message(nil), truncated.Messages...)

	// The newest evidence block is protected through task truncation. Drop it only
	// when doing so makes the requested budget attainable; if fixed overhead still
	// prevents fitting, restore the evidence and return the smaller best-effort request.
	kept[newestEvidenceBlock] = false
	truncated.Messages, size, err = recoveryMessagesWithinBudget(
		truncated, blocks, kept, tokenBudget,
	)
	if err != nil {
		return nil, err
	}
	if size.EstimatedTokens > tokenBudget {
		currentUserIndex = newestUserMessageIndex(truncated.Messages)
		if err := truncateCurrentUserPrompt(
			truncated, currentUserIndex, tokenBudget, false,
		); err != nil {
			return nil, err
		}
		size, err = EstimateCompletionRequestSize(truncated)
		if err != nil {
			return nil, err
		}
	}
	if size.EstimatedTokens > tokenBudget {
		truncated.Messages = anchoredMessages
	}
	return truncated, nil
}

func recoveryMessagesWithinBudget(
	req *CompletionRequest,
	blocks []messageBlock,
	kept []bool,
	tokenBudget int,
) ([]Message, CompletionRequestSize, error) {
	keptMessages := messagesFromKeptBlocks(blocks, kept)
	dropped := make([]messageBlock, 0)
	for i, block := range blocks {
		if !kept[i] {
			dropped = append(dropped, block)
		}
	}
	if len(dropped) == 0 {
		req.Messages = keptMessages
		size, err := EstimateCompletionRequestSize(req)
		return keptMessages, size, err
	}

	withNote := func(content string) ([]Message, CompletionRequestSize, error) {
		messages := make([]Message, 0, len(keptMessages)+1)
		messages = append(messages, Message{Role: "system", Content: content})
		messages = append(messages, keptMessages...)
		req.Messages = messages
		size, err := EstimateCompletionRequestSize(req)
		return messages, size, err
	}

	messages, size, err := withNote(extractDroppedSummary(dropped))
	if err != nil || size.EstimatedTokens <= tokenBudget {
		return messages, size, err
	}
	return withNote(minimalRecoveryTruncationNote)
}

func messagesFromKeptBlocks(blocks []messageBlock, kept []bool) []Message {
	messages := make([]Message, 0)
	for i, block := range blocks {
		if kept[i] {
			messages = append(messages, block.messages...)
		}
	}
	return messages
}

// groupRecoveryMessageBlocks keeps the current user task and the newest complete
// post-task tool exchange as recovery anchors. Older and incomplete blocks remain
// eligible for atomic removal.
func groupRecoveryMessageBlocks(messages []Message, currentUserIndex int) ([]messageBlock, int, int) {
	blocks := make([]messageBlock, 0)
	for i := 0; i < currentUserIndex; {
		start := i
		i++
		for i < currentUserIndex && messages[i].Role != "user" {
			i++
		}
		blocks = append(blocks, messageBlock{messages: messages[start:i]})
	}

	mandatoryBlock := len(blocks)
	blocks = append(blocks, messageBlock{messages: []Message{messages[currentUserIndex]}})
	postTaskBlocks := groupMessageBlocks(messages[currentUserIndex+1:])
	newestEvidenceBlock := -1
	for i := len(postTaskBlocks) - 1; i >= 0; i-- {
		if completeAssistantToolBlock(postTaskBlocks[i]) {
			newestEvidenceBlock = len(blocks) + i
			break
		}
	}
	blocks = append(blocks, postTaskBlocks...)
	return blocks, mandatoryBlock, newestEvidenceBlock
}

func completeAssistantToolBlock(block messageBlock) bool {
	if len(block.messages) < 2 {
		return false
	}
	assistant := block.messages[0]
	if assistant.Role != recoveryRoleAssistant || len(assistant.ToolCalls) == 0 {
		return false
	}

	pending := make(map[string]int, len(assistant.ToolCalls))
	for _, call := range assistant.ToolCalls {
		if call.ID == "" {
			return false
		}
		pending[call.ID]++
	}
	for _, result := range block.messages[1:] {
		if result.Role != recoveryRoleTool || pending[result.ToolCallID] == 0 {
			return false
		}
		pending[result.ToolCallID]--
	}
	for _, remaining := range pending {
		if remaining != 0 {
			return false
		}
	}
	return true
}

func cloneCompletionRequest(req *CompletionRequest) (*CompletionRequest, error) {
	serialized, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal completion request copy: %w", err)
	}
	var cloned CompletionRequest
	if err := json.Unmarshal(serialized, &cloned); err != nil {
		return nil, fmt.Errorf("unmarshal completion request copy: %w", err)
	}
	return &cloned, nil
}

func newestUserMessageIndex(messages []Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return i
		}
	}
	return -1
}

func truncateCurrentUserPrompt(
	req *CompletionRequest,
	messageIndex int,
	tokenBudget int,
	bestEffort bool,
) error {
	original := req.Messages[messageIndex].Content
	originalRunes := []rune(original)
	if len(originalRunes) < 3 {
		return nil
	}
	originalSize, err := EstimateCompletionRequestSize(req)
	if err != nil {
		return err
	}

	setContent := func(content string) (CompletionRequestSize, error) {
		req.Messages[messageIndex].Content = content
		return EstimateCompletionRequestSize(req)
	}

	const minimumRetainedRunes = 2
	bestContent := balancedTruncatedCurrentTask(originalRunes, minimumRetainedRunes)
	bestSize, err := setContent(bestContent)
	if err != nil {
		return err
	}
	if bestSize.EstimatedTokens > tokenBudget {
		// Fixed request overhead can make the target impossible. When recent
		// evidence is anchored, keep the smallest bounded task if it still shrinks
		// the request rather than discarding that evidence.
		if bestEffort && bestSize.SerializedBytes < originalSize.SerializedBytes {
			return nil
		}
		req.Messages[messageIndex].Content = original
		return nil
	}

	for low, high := minimumRetainedRunes, len(originalRunes)-1; low <= high; {
		mid := low + (high-low)/2
		content := balancedTruncatedCurrentTask(originalRunes, mid)
		size, sizeErr := setContent(content)
		if sizeErr != nil {
			return sizeErr
		}
		if size.EstimatedTokens <= tokenBudget {
			bestContent = content
			low = mid + 1
		} else {
			high = mid - 1
		}
	}

	req.Messages[messageIndex].Content = bestContent
	return nil
}

func balancedTruncatedCurrentTask(original []rune, retained int) string {
	prefixRunes := (retained + 1) / 2
	suffixRunes := retained / 2
	return string(original[:prefixRunes]) + currentUserTaskTruncationMarker +
		string(original[len(original)-suffixRunes:])
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

// TruncateMessages keeps the first message and the newest messages that fit
// within the token budget. Tool-call/tool-result groups are kept or dropped
// atomically so the LLM never sees orphaned tool results.
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

	// Always keep the first message
	first := messages[0]
	firstTokens := estimateMessageTokens(first)
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
		noteTokens := estimateTokens(noteContent)

		// If the note doesn't fit, drop more kept blocks to make room
		for noteTokens > remaining && len(kept) > 0 {
			remaining += kept[0].tokens
			droppedBlocks++
			kept = kept[1:]
			noteContent = extractDroppedSummary(blocks[:droppedBlocks])
			noteTokens = estimateTokens(noteContent)
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
