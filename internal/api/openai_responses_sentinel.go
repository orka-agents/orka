/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

package api

import (
	"strings"

	"github.com/orka-agents/orka/internal/llm"
)

// Strip across the concatenated text while retaining each source item's
// boundary and status, even if removing the marker empties an item. Leading
// whitespace is trimmed once, matching the coordinator's existing policy.
func stripResponsesGoalStateSentinel(resp *llm.CompletionResponse) *llm.CompletionResponse {
	if resp == nil || resp.OutputItems == nil {
		return stripGoalStateSentinelFromResponse(resp)
	}
	var joined strings.Builder
	for _, item := range resp.OutputItems {
		if item.ToolCall == nil {
			joined.WriteString(item.Content)
		}
	}
	source := joined.String()
	if !strings.Contains(source, goalStateSentinel) {
		return resp
	}
	result := *resp
	result.OutputItems = append([]llm.AssistantOutputItem(nil), resp.OutputItems...)
	var content strings.Builder
	offset, skipUntil := 0, 0
	leading := true
	for i := range result.OutputItems {
		item := &result.OutputItems[i]
		if item.ToolCall != nil {
			continue
		}
		original := item.Content
		var text strings.Builder
		for j := 0; j < len(original); j++ {
			position := offset + j
			if position < skipUntil {
				continue
			}
			if strings.HasPrefix(source[position:], goalStateSentinel) {
				skipUntil = position + len(goalStateSentinel)
				continue
			}
			ch := original[j]
			if leading && strings.ContainsRune(" \t\r\n", rune(ch)) {
				continue
			}
			leading = false
			text.WriteByte(ch)
		}
		offset += len(original)
		item.Content = text.String()
		content.WriteString(item.Content)
	}
	result.Content = content.String()
	return &result
}
