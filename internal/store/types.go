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

// ArtifactMetadata describes a stored artifact file.
type ArtifactMetadata struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
	CreatedAt   string `json:"createdAt"`
}

// PlanState represents the autonomous plan state for a coordinator task.
type PlanState struct {
	TaskName     string
	Namespace    string
	Iteration    int
	Summary      string // Human-readable progress summary
	ProgressPct  int    // 0-100 progress estimate
	GoalComplete bool   // LLM determined goal is met
	PlanDocument string // Freeform markdown plan managed by LLM
	CreatedAt    time.Time
	UpdatedAt    time.Time
}
