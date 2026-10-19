package v1

import (
	"time"
)

// SessionRecord represents a full session.
type SessionRecord struct {
	Namespace     string
	Name          string
	SessionType   string // "task", "chat", or "gateway"
	ActiveTask    string
	ActiveTaskUID string
	MessageCount  int
	InputTokens   int
	OutputTokens  int
	Cancelled     bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Messages      []SessionMessage
}

// SessionMetadata is the lightweight listing representation.
type SessionMetadata struct {
	Name          string
	SessionType   string
	MessageCount  int
	InputTokens   int
	OutputTokens  int
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ActiveTask    string
	ActiveTaskUID string
}

// SessionPageCursor is the position a ListSessionsPage page ended at: the
// creation time and name of its last session.
type SessionPageCursor struct {
	CreatedAt time.Time
	Name      string
}

// SessionMessage is a single transcript entry.
type SessionMessage struct {
	ID         string            `json:"id"`
	Order      int64             `json:"order,omitempty"`
	Role       string            `json:"role"`
	Content    string            `json:"content"`
	Name       string            `json:"name,omitempty"`
	Input      map[string]any    `json:"input,omitempty"`
	ToolCalls  any               `json:"toolCalls,omitempty"`
	ToolCallID string            `json:"toolCallID,omitempty"`
	SourceType string            `json:"sourceType,omitempty"`
	SourceRef  string            `json:"sourceRef,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Timestamp  time.Time         `json:"ts"`
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

// TranscriptSearchFilter constrains transcript-backed recall/search.
type TranscriptSearchFilter struct {
	Namespace          string
	Query              string
	SessionName        string
	SessionNames       []string
	HistoryBounds      []TranscriptSearchHistoryBound
	ExcludeSessionName string
	Roles              []string
	Limit              int
	MaxSnippetLength   int
}

// TranscriptSearchHistoryBound limits a session to its latest MaxMessages at
// or before ThroughMessageID, using the same logical order as LoadTranscript.
// Search intersects all bounds for a session before applying search filters.
type TranscriptSearchHistoryBound struct {
	SessionName      string `json:"sessionName"`
	MaxMessages      int    `json:"maxMessages"`
	ThroughMessageID string `json:"throughMessageId"`
}

// TranscriptSearchResult is a compact prior transcript hit.
type TranscriptSearchResult struct {
	SessionName     string    `json:"sessionName"`
	MessageID       int64     `json:"messageId"`
	StableMessageID string    `json:"stableMessageId,omitempty"`
	Role            string    `json:"role"`
	Name            string    `json:"name,omitempty"`
	Snippet         string    `json:"snippet"`
	CreatedAt       time.Time `json:"createdAt"`
}

const (
	// SessionTypeChat identifies Sessions owned by the interactive chat API.
	SessionTypeChat = "chat"
	// SessionTypeGateway identifies canonical Sessions exclusively owned by gateway admission/projection.
	SessionTypeGateway = "gateway"
)
