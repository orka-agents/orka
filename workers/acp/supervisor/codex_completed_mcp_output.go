package supervisor

import (
	"bytes"
	"encoding/json"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const codexMCPContentKey = "content"

func codexMCPOutputStart(envelope codexCommandOutputEnvelope) bool {
	var mcp bool
	return json.Unmarshal(envelope.Meta["is_mcp_tool_call"], &mcp) == nil && mcp
}

func codexMCPOutputInputMatches(raw json.RawMessage, name string) bool {
	return name != "" && codexMCPInputToolName(raw) == name
}

// Read exact keys after duplicate rejection: struct decoding also accepts case
// and Unicode aliases, which must not override the pinned server/tool fields.
func codexMCPInputToolName(raw json.RawMessage) string {
	if len(raw) > harnessv2.MaxMCPArgumentsBytes+(1<<10) {
		return ""
	}
	raw, err := harnessv2.CanonicalJSON(raw)
	if err != nil {
		return ""
	}
	var input map[string]json.RawMessage
	var server, tool string
	if json.Unmarshal(raw, &input) != nil || json.Unmarshal(input["server"], &server) != nil || server != acpMCPServerName ||
		json.Unmarshal(input["tool"], &tool) != nil || len(tool) > 253 {
		return ""
	}
	return tool
}

// Generic ACP identity decoding can fold marker keys or retain true across a
// later null. Reject duplicate envelope/metadata keys before freezing a name.
func codexMCPStartMarkerMatches(raw json.RawMessage) bool {
	raw, err := harnessv2.CanonicalJSON(raw)
	if err != nil {
		return false
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(envelope["_meta"], &fields) != nil {
		return false
	}
	return codexMCPOutputStart(codexCommandOutputEnvelope{Meta: fields})
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
	// Reject duplicate keys throughout the result before interpreting any field.
	// The canonical form also lets structuredContent be compared without losing
	// JSON number precision or adding a second public content source.
	rawOutput, err := harnessv2.CanonicalJSON(envelope.RawOutput)
	if err != nil {
		return "", false
	}
	var output map[string]json.RawMessage
	if json.Unmarshal(rawOutput, &output) != nil || len(output) != 2 || string(output["error"]) != acpJSONNull {
		return "", false
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(output["result"], &result) != nil {
		return "", false
	}
	for key, value := range result {
		switch key {
		case codexMCPContentKey, "structuredContent":
		case "_meta":
			// The pinned producer emits only this replay marker. Metadata is
			// discarded; unknown fields could indicate incomplete content.
			if string(value) != acpJSONNull && string(value) != "{}" && string(value) != `{"orka.replayed":true}` {
				return "", false
			}
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
	if !codexMCPStructuredContentMatches(result["structuredContent"], text) {
		return "", false
	}
	return text, true
}

// structured is already canonical from the complete rawOutput above. Only a
// duplicate of the text JSON object is supported; it is never projected.
func codexMCPStructuredContentMatches(structured json.RawMessage, text string) bool {
	if len(structured) == 0 || string(structured) == acpJSONNull {
		return true
	}
	if structured[0] != '{' {
		return false
	}
	canonicalText, err := harnessv2.CanonicalJSON([]byte(text))
	return err == nil && bytes.Equal(structured, canonicalText)
}
