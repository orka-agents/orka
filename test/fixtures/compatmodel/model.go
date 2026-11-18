// Package compatmodel supplies a deterministic OpenAI model for compatibility
// routing tests. Its fixture credentials identify the expected namespace; they
// are not real provider credentials. Never expose this fixture as a model service.
//
//nolint:goconst // Keep the fixed JSON wire fixtures readable.
package compatmodel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
}

type request struct {
	Model    string            `json:"model"`
	Messages []message         `json:"messages"`
	Tools    []json.RawMessage `json:"tools"`
	Stream   bool              `json:"stream"`
	Input    []struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		CallID  string          `json:"call_id"`
		Output  json.RawMessage `json:"output"`
	} `json:"input"`
}

// Handler returns text or performs a fixed create/wait/fetch tool sequence.
// Both compatibility APIs can use this one OpenAI-backed Provider.
func Handler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		w.WriteHeader(http.StatusOK)
		return
	}
	credential := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer fixture-")
	if credential == "" || credential == r.Header.Get("Authorization") {
		http.Error(w, "expected fixture model credential", http.StatusUnauthorized)
		return
	}
	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid fixture request", http.StatusBadRequest)
		return
	}
	for _, input := range req.Input {
		msg := message{Role: input.Role, Content: input.Content}
		if input.Type == "function_call_output" {
			msg.Role, msg.ToolCallID, msg.Content = "tool", input.CallID, input.Output
		}
		req.Messages = append(req.Messages, msg)
	}
	content, call := next(req, credential)
	if strings.HasSuffix(r.URL.Path, "/responses") {
		writeResponses(w, req, content, call)
		return
	}
	finish := "stop"
	responseMessage := map[string]any{"role": "assistant", "content": content}
	if call != nil {
		finish = "tool_calls"
		responseMessage["tool_calls"] = []any{call}
	}
	usage := map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "fixture-completion",
			"object":  "chat.completion",
			"created": 1,
			"model":   req.Model,
			"choices": []any{
				map[string]any{"index": 0, "message": responseMessage, "finish_reason": finish},
			},
			"usage": usage,
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
		event["id"], event["object"] = "fixture-completion", "chat.completion.chunk"
		event["model"], event["created"] = req.Model, 1
		data, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		w.(http.Flusher).Flush()
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	w.(http.Flusher).Flush()
}

func writeResponses(w http.ResponseWriter, req request, content string, call map[string]any) {
	item := map[string]any{
		"id":     "fixture-message",
		"type":   "message",
		"role":   "assistant",
		"status": "completed",
		"content": []any{
			map[string]any{"type": "output_text", "text": content, "annotations": []any{}},
		},
	}
	if call != nil {
		function := call["function"].(map[string]any)
		item = map[string]any{"id": "fixture-call", "type": "function_call", "status": "completed",
			"call_id": call["id"], "name": function["name"], "arguments": function["arguments"]}
	}
	response := map[string]any{
		"id":     "fixture-response",
		"object": "response",
		"status": "completed",
		"model":  req.Model,
		"output": []any{
			item,
		},
		"usage": map[string]int{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15},
	}
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
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

func next(req request, namespace string) (string, map[string]any) {
	finish := func(result string) (string, map[string]any) {
		return "<ORKA_GOAL_STATE_REACHED>\nOUTPUT:" + namespace + "\n" + result, nil
	}
	if len(req.Tools) == 0 {
		return finish("tools disabled")
	}
	prompt := ""
	results := map[string]string{}
	for _, msg := range req.Messages {
		var text string
		_ = json.Unmarshal(msg.Content, &text)
		if msg.Role == "user" {
			prompt = text
		}
		if msg.Role == "tool" {
			name, _, _ := strings.Cut(msg.ToolCallID, "-")
			results[name] = text
		}
	}
	parts := strings.Fields(prompt)
	if len(parts) == 0 {
		return finish("empty fixture prompt")
	}
	target := ""
	if len(parts) > 1 {
		target = parts[1]
	}
	if result := results["read"]; result != "" {
		return finish(result)
	}
	if result := results["agents"]; result != "" {
		return finish(result)
	}
	if parts[0] == "read" {
		return tool(
			"read",
			"fetch_task_output",
			map[string]any{"name": "same-task", "namespace": target},
		)
	}
	if parts[0] == "agents" {
		return tool("agents", "list_agents", map[string]any{"namespace": target})
	}
	if parts[0] != "create" {
		return finish("plain response")
	}
	created := results["create"]
	if created == "" {
		return tool("create", "create_container_task", map[string]any{
			"name": "same-task", "namespace": target,
			"command": []string{"sh", "-c"}, "args": []string{"printf 'RESULT:" + namespace + "'"},
		})
	}
	var result struct {
		Success bool `json:"success"`
		Data    struct {
			Name  string `json:"name"`
			Phase string `json:"phase"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(created), &result) != nil || !result.Success {
		return finish(created)
	}
	name := result.Data.Name
	if output := results["fetch"]; output != "" {
		return finish(output)
	}
	if waited := results["wait"]; waited != "" {
		if json.Unmarshal([]byte(waited), &result) != nil || !result.Success ||
			result.Data.Phase == "Failed" {
			return finish(waited)
		}
		if result.Data.Phase == "Succeeded" {
			return tool("fetch", "fetch_task_output", map[string]any{"name": name})
		}
	}
	return tool(
		fmt.Sprintf("wait-%d", len(req.Messages)),
		"wait_for_task",
		map[string]any{"name": name, "timeout": 30},
	)
}

func tool(id, name string, arguments map[string]any) (string, map[string]any) {
	data, _ := json.Marshal(arguments)
	return "", map[string]any{
		"id":       id,
		"type":     "function",
		"function": map[string]any{"name": name, "arguments": string(data)},
	}
}
