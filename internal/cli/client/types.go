/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package client

import "encoding/json"

// ChatRequest is the request body for POST /api/v1/chat.
type ChatRequest struct {
	Message   string `json:"message"`
	SessionID string `json:"sessionId,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Model     string `json:"model,omitempty"`
	Provider  string `json:"provider,omitempty"`
}

// ChatConfigResponse is the response from GET /api/v1/chat/config.
type ChatConfigResponse struct {
	Enabled        bool     `json:"enabled"`
	Provider       string   `json:"provider"`
	Model          string   `json:"model"`
	AvailableTools []string `json:"availableTools"`
}

// SSEEvent represents a parsed Server-Sent Event.
type SSEEvent struct {
	Event string
	Data  string
}

// SSEEventData holds the parsed data payload from an SSE event.
type SSEEventData struct {
	// status event
	SessionID string `json:"sessionId,omitempty"`

	// message event
	Content string `json:"content,omitempty"`

	// tool_call / tool_result event
	Name string `json:"name,omitempty"`
	Args string `json:"args,omitempty"`

	// tool_result event — raw JSON (may be object or string)
	Result json.RawMessage `json:"result,omitempty"`

	// error event
	Error string `json:"error,omitempty"`

	// done event
	Usage *ChatUsage `json:"usage,omitempty"`
}

// ChatUsage holds token usage statistics from a chat response.
type ChatUsage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
}
