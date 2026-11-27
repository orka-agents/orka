/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package client

import "encoding/json"

type ChatRequest struct {
	Message   string `json:"message"`
	SessionID string `json:"sessionId,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Agent     string `json:"agentRef,omitempty"`
	Model     string `json:"model,omitempty"`
	Provider  string `json:"provider,omitempty"`
}

type ChatConfigResponse struct {
	Enabled        bool     `json:"enabled"`
	Provider       string   `json:"provider"`
	Model          string   `json:"model"`
	AvailableTools []string `json:"availableTools"`
}

type SSEEvent struct {
	Event string
	Data  string
}

type SSEEventData struct {
	SessionID string `json:"sessionId,omitempty"`

	Content string `json:"content,omitempty"`

	ID   string          `json:"id,omitempty"`
	Name string          `json:"name,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`

	Result json.RawMessage `json:"result,omitempty"`

	Error string `json:"error,omitempty"`

	Usage *ChatUsage `json:"usage,omitempty"`
}

type ChatUsage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
}
