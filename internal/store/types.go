package store

import "time"

// SessionRecord represents a full session.
type SessionRecord struct {
	Namespace    string
	Name         string
	SessionType  string // "task" or "chat"
	ActiveTask   string
	MessageCount int
	InputTokens  int
	OutputTokens int
	Cancelled    bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Messages     []SessionMessage
}

// SessionMetadata is the lightweight listing representation.
type SessionMetadata struct {
	Name         string
	SessionType  string
	MessageCount int
	InputTokens  int
	OutputTokens int
	CreatedAt    time.Time
	UpdatedAt    time.Time
	ActiveTask   string
}

// SessionMessage is a single transcript entry.
type SessionMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	Name       string         `json:"name,omitempty"`
	Input      map[string]any `json:"input,omitempty"`
	ToolCalls  any            `json:"toolCalls,omitempty"`
	ToolCallID string         `json:"toolCallID,omitempty"`
	Timestamp  time.Time      `json:"ts"`
}
