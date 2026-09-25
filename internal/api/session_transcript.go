package api

import (
	"encoding/json"
	"maps"
	"slices"

	"github.com/orka-agents/orka/internal/store"
)

// sessionTranscriptMessage adds display text without changing the stored tool
// arguments. Browsers otherwise round JSON numbers before rendering them.
func sessionTranscriptMessage(message store.SessionMessage) store.SessionMessage {
	calls, ok := message.ToolCalls.([]any)
	if !ok {
		return message
	}
	projected := slices.Clone(calls)
	for i, value := range calls {
		call, ok := value.(map[string]any)
		if !ok {
			continue
		}
		arguments, present := call["arguments"]
		if !present {
			continue
		}
		text, isString := arguments.(string)
		if !isString {
			data, err := json.MarshalIndent(arguments, "", "  ")
			if err != nil {
				continue
			}
			text = string(data)
		}
		projectedCall := maps.Clone(call)
		projectedCall["argumentsText"] = text
		projected[i] = projectedCall
	}
	message.ToolCalls = projected
	return message
}
