/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package anthropictest lets fake Anthropic servers in tests answer the
// streaming requests the anthropic provider sends.
package anthropictest

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
)

// WriteMessage writes message, a complete Messages API response body, to w as
// the server-sent event stream the API returns for the same message. The
// anthropic provider streams every request, so a fake server that used to
// answer with message as JSON calls this instead.
func WriteMessage(w http.ResponseWriter, message string) error {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal([]byte(message), &msg); err != nil {
		return fmt.Errorf("decode message: %w", err)
	}
	var blocks []map[string]json.RawMessage
	if raw, ok := msg["content"]; ok {
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return fmt.Errorf("decode content: %w", err)
		}
	}
	var usage map[string]json.RawMessage
	if raw, ok := msg["usage"]; ok {
		if err := json.Unmarshal(raw, &usage); err != nil {
			return fmt.Errorf("decode usage: %w", err)
		}
	}

	start := maps.Clone(msg)
	start["content"] = json.RawMessage(`[]`)
	start["stop_reason"] = json.RawMessage(`null`)
	start["stop_sequence"] = json.RawMessage(`null`)

	w.Header().Set("Content-Type", "text/event-stream")
	events := []event{{"message_start", map[string]any{"message": start}}}
	for i, block := range blocks {
		var kind string
		_ = json.Unmarshal(block["type"], &kind)
		opening := maps.Clone(block)
		var delta map[string]any
		switch kind {
		case "text":
			var text string
			_ = json.Unmarshal(block["text"], &text)
			opening["text"] = json.RawMessage(`""`)
			delta = map[string]any{"type": "text_delta", "text": text}
		case "tool_use":
			input := rawOr(block["input"], `{}`)
			opening["input"] = json.RawMessage(`{}`)
			delta = map[string]any{"type": "input_json_delta", "partial_json": string(input)}
		}
		events = append(events, blockEvent("content_block_start", i, "content_block", opening))
		if delta != nil {
			events = append(events, blockEvent("content_block_delta", i, "delta", delta))
		}
		events = append(events, blockEvent("content_block_stop", i, "", nil))
	}
	stop := map[string]json.RawMessage{
		"stop_reason":   rawOr(msg["stop_reason"], `null`),
		"stop_sequence": rawOr(msg["stop_sequence"], `null`),
	}
	if raw, ok := msg["stop_details"]; ok {
		stop["stop_details"] = raw
	}
	deltaUsage := map[string]json.RawMessage{}
	if out, ok := usage["output_tokens"]; ok {
		deltaUsage["output_tokens"] = out
	}
	events = append(events,
		event{"message_delta", map[string]any{"delta": stop, "usage": deltaUsage}},
		event{"message_stop", map[string]any{}},
	)

	for _, e := range events {
		e.fields["type"] = e.name
		data, err := json.Marshal(e.fields)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.name, data); err != nil {
			return err
		}
	}
	return nil
}

type event struct {
	name   string
	fields map[string]any
}

func blockEvent(name string, index int, key string, value any) event {
	fields := map[string]any{"index": index}
	if key != "" {
		fields[key] = value
	}
	return event{name, fields}
}

func rawOr(raw json.RawMessage, fallback string) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(fallback)
	}
	return raw
}
