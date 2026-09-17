package supervisor

import (
	"encoding/json"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const codexMCPContentKey = "content"

func codexMCPOutputStart(envelope codexCommandOutputEnvelope) bool {
	var mcp bool
	return json.Unmarshal(envelope.Meta["is_mcp_tool_call"], &mcp) == nil && mcp
}

func codexMCPOutputInputMatches(raw json.RawMessage, name string) bool {
	if name == "" || len(raw) > harnessv2.MaxMCPArgumentsBytes+(1<<10) {
		return false
	}
	var input struct {
		Server string `json:"server"`
		Tool   string `json:"tool"`
	}
	return json.Unmarshal(raw, &input) == nil && input.Server == acpMCPServerName && input.Tool == name
}

// codexCompletedMCPText accepts the terminal shape of the pinned adapter's
// completeItemEvent(mcpToolCall), after the prompt has recorded the exact frozen
// brokered read-only identity. It neither grants permission nor authenticates a
// broker result; broker execution and permission checks retain their own fences.
// Only one complete text block enters the existing logical-text redaction path.
func codexCompletedMCPText(envelope codexCommandOutputEnvelope, name string) (string, bool) {
	if envelope.Status != harnessv2.ToolCallStatusCompleted || len(envelope.Meta) != 0 ||
		!codexMCPOutputInputMatches(envelope.RawInput, name) {
		return "", false
	}
	var output map[string]json.RawMessage
	if json.Unmarshal(envelope.RawOutput, &output) != nil || len(output) != 2 || string(output["error"]) != acpJSONNull {
		return "", false
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(output["result"], &result) != nil {
		return "", false
	}
	for key, value := range result {
		switch key {
		case codexMCPContentKey:
		case "isError":
			if string(value) != "false" {
				return "", false
			}
		default:
			return "", false
		}
	}
	var content []map[string]json.RawMessage
	if json.Unmarshal(result[codexMCPContentKey], &content) != nil || len(content) != 1 || len(content[0]) != 2 ||
		string(content[0]["type"]) != `"text"` {
		return "", false
	}
	var text string
	raw := content[0]["text"]
	if len(raw) == 0 || string(raw) == acpJSONNull || json.Unmarshal(raw, &text) != nil || len(text) > harnessv2.MaxPromptContentBytes {
		return "", false
	}
	return text, true
}
