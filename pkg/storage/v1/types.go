package v1

import (
	"time"
)

type SessionRecord struct {
	Namespace     string
	Name          string
	SessionType   string
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

type SessionPageCursor struct {
	CreatedAt time.Time
	Name      string
}

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

const MaxArtifactBytes = 10 << 20

const MaxResultBytes = 10 << 20

type ArtifactOwner struct {
	Namespace string
	TaskName  string
	TaskUID   string
}

type ArtifactKey struct {
	Owner    ArtifactOwner
	Filename string
}

type SaveArtifactRequest struct {
	Owner       ArtifactOwner
	Filename    string
	ContentType string
	Data        []byte
}

type ArtifactMetadata struct {
	Filename    string    `json:"filename"`
	ContentType string    `json:"contentType"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"createdAt"`
}

type PlanState struct {
	TaskName     string
	Namespace    string
	Iteration    int
	Summary      string
	ProgressPct  int
	GoalComplete bool
	PlanDocument string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

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

type TranscriptSearchHistoryBound struct {
	SessionName      string `json:"sessionName"`
	MaxMessages      int    `json:"maxMessages"`
	ThroughMessageID string `json:"throughMessageId"`
}

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
	SessionTypeChat    = "chat"
	SessionTypeGateway = "gateway"
)
