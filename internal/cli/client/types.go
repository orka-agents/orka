/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package client

import "encoding/json"

// CreateTaskRequest represents a task creation request.
type CreateTaskRequest struct {
	Name       string            `json:"name"`
	Namespace  string            `json:"namespace"`
	Type       string            `json:"type"`
	Image      string            `json:"image,omitempty"`
	Command    []string          `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Env        []EnvVar          `json:"env,omitempty"`
	Timeout    string            `json:"timeout,omitempty"`
	Priority   *int32            `json:"priority,omitempty"`
	WebhookURL string            `json:"webhookURL,omitempty"`
	Prompt     string            `json:"prompt,omitempty"`
	AgentRef   *AgentReference   `json:"agentRef,omitempty"`
	Schedule   string            `json:"schedule,omitempty"`
	SessionRef *SessionReference `json:"sessionRef,omitempty"`
}

// EnvVar represents a name-value pair for environment variables.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// AgentReference identifies an agent by name and optional namespace.
type AgentReference struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// SessionReference identifies a session by name.
type SessionReference struct {
	Name string `json:"name"`
}

// ListResponse is the generic paginated list response.
type ListResponse struct {
	Items    json.RawMessage `json:"items"`
	Metadata ListMeta        `json:"metadata"`
}

// ListMeta contains pagination metadata.
type ListMeta struct {
	Continue           string `json:"continue,omitempty"`
	RemainingItemCount *int64 `json:"remainingItemCount,omitempty"`
}

// ChatRequest represents a chat request.
type ChatRequest struct {
	Message      string   `json:"message"`
	SessionID    string   `json:"sessionId,omitempty"`
	Provider     string   `json:"provider,omitempty"`
	Model        string   `json:"model,omitempty"`
	Namespace    string   `json:"namespace,omitempty"`
	Temperature  *float64 `json:"temperature,omitempty"`
	MaxTokens    *int32   `json:"maxTokens,omitempty"`
	SystemPrompt string   `json:"systemPrompt,omitempty"`
	AgentRef     string   `json:"agentRef,omitempty"`
}

// ChatUsage contains token and call usage statistics from a chat session.
type ChatUsage struct {
	InputTokens  int    `json:"inputTokens"`
	OutputTokens int    `json:"outputTokens"`
	LLMCalls     int    `json:"llmCalls"`
	ToolCalls    int    `json:"toolCalls"`
	TasksCreated int    `json:"tasksCreated"`
	Duration     string `json:"duration"`
}

// CreateAgentRequest represents an agent creation request.
type CreateAgentRequest struct {
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Spec      json.RawMessage `json:"spec"`
}

// UpdateAgentRequest represents an agent update request.
type UpdateAgentRequest struct {
	Spec json.RawMessage `json:"spec"`
}
