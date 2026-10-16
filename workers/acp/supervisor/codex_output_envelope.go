package supervisor

import (
	"bytes"
	"encoding/json"
	"strings"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// Validate the entire pinned envelope before interpreting any authority. Decode
// its original bytes: canonical number normalization must not turn 0e0 into an
// accepted integer exit code. Case/Unicode aliases are rejected where the generic
// ACP mapper would otherwise interpret a different envelope from this decoder.
func decodeCodexOutputEnvelope(raw json.RawMessage) (codexCommandOutputEnvelope, bool) {
	var envelope codexCommandOutputEnvelope
	if _, err := harnessv2.CanonicalJSON(raw); err != nil {
		return envelope, false
	}
	fields, ok := codexExactFields(raw, "sessionUpdate", "toolCallId", "kind", "status", "content", "rawInput", "rawOutput", "_meta", "title", "name")
	if !ok {
		return envelope, false
	}
	for key, target := range map[string]*string{
		"sessionUpdate": &envelope.SessionUpdate, "toolCallId": &envelope.ToolCallID, "kind": &envelope.Kind,
	} {
		if value, present := fields[key]; present && (string(value) == acpJSONNull || json.Unmarshal(value, target) != nil) {
			return envelope, false
		}
	}
	if value, present := fields["status"]; present && (string(value) == acpJSONNull || json.Unmarshal(value, &envelope.Status) != nil) {
		return envelope, false
	}
	envelope.Content, envelope.RawInput, envelope.RawOutput = fields["content"], fields["rawInput"], fields["rawOutput"]
	if value, present := fields["_meta"]; present && json.Unmarshal(value, &envelope.Meta) != nil {
		return envelope, false
	}
	return envelope, true
}

// The caller has already rejected duplicates throughout the envelope. Exact map
// lookups prevent struct decoding from accepting aliases in terminal metadata.
func codexExactFields(raw json.RawMessage, keys ...string) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, false
	}
	for key := range fields {
		for _, exact := range keys {
			if key != exact && strings.EqualFold(key, exact) {
				return nil, false
			}
		}
	}
	return fields, true
}

// An invalid envelope can contain multiple IDs that the generic decoder would
// collapse or case-fold. Retain a bounded tombstone for every such ID, including
// when a conflicting sessionUpdate made the generic mapper suppress the frame.
// This recovery path never creates identity or output authority.
func (n *codexCompletedOutputNormalizer) invalidateEnvelopeIDs(raw json.RawMessage) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return
		}
		if name, ok := key.(string); !ok || !strings.EqualFold(name, "toolCallId") {
			continue
		}
		var rawID string
		if json.Unmarshal(value, &rawID) != nil {
			continue
		}
		if id, err := canonicalACPToolCallID(rawID); err == nil {
			n.invalidate(id)
		}
	}
}
