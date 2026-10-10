//nolint:goconst,lll // Keep the fixed model wire fixtures readable.
package connectorsfixture

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// The model script the fixture plays, keyed by what the conversation already
// holds. Tool call ids carry a prefix so results can be recognised.
const (
	ReadToolName  = "itemsread"
	WriteToolName = "itemswrite"
	WriteTitle    = "hello from orka"
	readOneID     = "read1"
	readTwoID     = "read2"
	writeID       = "write"
	planID        = "plan"
)

type modelMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
}

type modelRequest struct {
	Model    string            `json:"model"`
	Messages []modelMessage    `json:"messages"`
	Tools    []json.RawMessage `json:"tools"`
	Stream   bool              `json:"stream"`
	Input    []struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		CallID  string          `json:"call_id"`
		Output  json.RawMessage `json:"output"`
	} `json:"input"`
	Instructions string `json:"instructions"`
}

// model plays a fixed script through either the Chat Completions or the
// Responses shape:
//
//  1. itemsread, then itemswrite. The worker parks itemswrite for approval, so
//     the first run ends there.
//  2. After a person approves, the run resumes with the resolved approval
//     in the prompt: itemswrite again with identical arguments, then itemsread
//     again (by now the first access token has expired, so this read is
//     served by a refreshed token), then update_plan with the goal complete
//     and a final answer.
func (f *Fixture) model(w http.ResponseWriter, r *http.Request) {
	// The scheme is part of the contract: a bare credential is refused.
	authorization := r.Header.Get("Authorization")
	credential, bearer := strings.CutPrefix(authorization, "Bearer ")
	if !bearer || credential == "" || credential != f.cfg.ModelCredential {
		http.Error(w, "expected fixture model credential", http.StatusUnauthorized)
		return
	}
	var req modelRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid fixture request", http.StatusBadRequest)
		return
	}
	for _, input := range req.Input {
		msg := modelMessage{Role: input.Role, Content: input.Content}
		if input.Type == "function_call_output" {
			msg.Role, msg.ToolCallID, msg.Content = "tool", input.CallID, input.Output
		}
		req.Messages = append(req.Messages, msg)
	}
	f.mu.Lock()
	f.counters.ModelTurns++
	f.mu.Unlock()
	content, call := f.next(req)
	if strings.HasSuffix(r.URL.Path, "/responses") {
		writeResponsesShape(w, req, content, call)
		return
	}
	writeChatShape(w, req, content, call)
}

func (f *Fixture) next(req modelRequest) (string, map[string]any) {
	results := map[string]bool{}
	approved := strings.Contains(req.Instructions, "APPROVED")
	for _, msg := range req.Messages {
		var text string
		if json.Unmarshal(msg.Content, &text) != nil {
			text = string(msg.Content)
		}
		if msg.Role == "tool" {
			id, _, _ := strings.Cut(msg.ToolCallID, "-")
			results[id] = true
		}
		if (msg.Role == "system" || msg.Role == "user") && strings.Contains(text, "Resolved Human Approvals") && strings.Contains(text, "APPROVED") {
			approved = true
		}
	}
	if len(req.Tools) == 0 {
		return "connectors fixture: tools disabled", nil
	}
	switch {
	case !approved && !results[readOneID]:
		return toolCall(readOneID, ReadToolName, map[string]any{"q": "connectors"})
	case !approved:
		// Parks for approval; the worker does not call the model again in
		// this run.
		return toolCall(writeID, WriteToolName, map[string]any{"title": WriteTitle})
	case !results[writeID]:
		return toolCall(writeID, WriteToolName, map[string]any{"title": WriteTitle})
	case !results[readTwoID]:
		return toolCall(readTwoID, ReadToolName, map[string]any{"q": "connectors"})
	case !results[planID]:
		return toolCall(planID, "update_plan", map[string]any{
			"summary": "linked-account read, approved write, refreshed read all done", "progress_pct": 100, "goal_complete": true,
			"plan_document": "# Goal\nExercise the linked account\n# Completed\n- [x] read\n- [x] approved write\n- [x] refreshed read\n",
		})
	default:
		return "CONNECTORS_E2E_DONE: read, approved write, refreshed read", nil
	}
}

func toolCall(id, name string, arguments map[string]any) (string, map[string]any) {
	data, _ := json.Marshal(arguments)
	return "", map[string]any{"id": id + "-" + randomToken(4), "type": "function", "function": map[string]any{"name": name, "arguments": string(data)}}
}

func writeChatShape(w http.ResponseWriter, req modelRequest, content string, call map[string]any) {
	finish := "stop"
	message := map[string]any{"role": "assistant", "content": content}
	if call != nil {
		finish = "tool_calls"
		message["tool_calls"] = []any{call}
	}
	usage := map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	if !req.Stream {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "fixture-completion", "object": "chat.completion", "created": 1, "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": usage,
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	delta := map[string]any{"role": "assistant", "content": content}
	if call != nil {
		call["index"] = 0
		delta["tool_calls"] = []any{call}
	}
	for _, event := range []map[string]any{
		{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}},
		{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}},
		{"choices": []any{}, "usage": usage},
	} {
		event["id"], event["object"], event["model"], event["created"] = "fixture-completion", "chat.completion.chunk", req.Model, 1
		data, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		w.(http.Flusher).Flush()
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	w.(http.Flusher).Flush()
}

func writeResponsesShape(w http.ResponseWriter, req modelRequest, content string, call map[string]any) {
	item := map[string]any{"id": "fixture-message", "type": "message", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": content, "annotations": []any{}}}}
	if call != nil {
		function := call["function"].(map[string]any)
		item = map[string]any{"id": "fixture-call", "type": "function_call", "status": "completed",
			"call_id": call["id"], "name": function["name"], "arguments": function["arguments"]}
	}
	response := map[string]any{"id": "fixture-response", "object": "response", "status": "completed", "model": req.Model,
		"output": []any{item}, "usage": map[string]int{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}}
	if !req.Stream {
		writeJSON(w, http.StatusOK, response)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range []map[string]any{
		{"type": "response.output_text.delta", "delta": content, "output_index": 0},
		{"type": "response.completed", "response": response},
	} {
		data, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
		w.(http.Flusher).Flush()
	}
}
