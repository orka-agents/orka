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

// MaxArtifactBytes is the largest artifact Orka stores. Every artifacts.v1
// provider accepts artifacts up to this size and rejects larger ones with
// [ErrTooLarge].
const MaxArtifactBytes = 10 << 20

// MaxResultBytes is the largest Task result Orka stores, the same limit as the
// worker result API. Every state.v1 provider accepts results, and prompt
// result receipt data, up to this size and rejects larger ones with
// [ErrTooLarge].
const MaxResultBytes = 10 << 20

// ArtifactOwner identifies the Task whose artifacts an operation addresses.
// Requests are structs so the identity can grow without another change to
// every method.
type ArtifactOwner struct {
	Namespace string
	TaskName  string
	// TaskUID is the owning Task's UID. It is required: a request without it
	// returns ErrValidation. Artifacts are bound to the UID, so an operation
	// for an older Task with the same name never reads, overwrites or deletes
	// a newer Task's artifacts (see [ArtifactStore]).
	TaskUID string
}

// Validate checks that the owner names a namespace, a Task name and a Task
// UID.
func (o ArtifactOwner) Validate() error {
	return validateTaskIdentity("artifact owner", o.Namespace, o.TaskName, o.TaskUID)
}

// ResultKey identifies the result of one Task. TaskUID is required, as for
// [ArtifactOwner]: a result is bound to the UID of the Task that produced it
// (see [ResultStore]).
type ResultKey struct {
	Namespace string
	TaskName  string
	TaskUID   string
}

// Validate checks that the key names a namespace, a Task name and a Task UID.
func (k ResultKey) Validate() error {
	return validateTaskIdentity("result key", k.Namespace, k.TaskName, k.TaskUID)
}

func validateTaskIdentity(what, namespace, taskName, taskUID string) error {
	if namespace == "" || taskName == "" {
		return ValidationErrorf("%s namespace and Task name are required", what)
	}
	return validateControlIdentifier(what+" Task UID", taskUID)
}

// ArtifactKey identifies one artifact file.
type ArtifactKey struct {
	Owner    ArtifactOwner
	Filename string
}

// SaveArtifactRequest stores one artifact file.
type SaveArtifactRequest struct {
	Owner       ArtifactOwner
	Filename    string
	ContentType string
	Data        []byte
}

// ArtifactMetadata describes a stored artifact file.
type ArtifactMetadata struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
	// CreatedAt is when the provider stored this version of the file, in UTC.
	CreatedAt time.Time `json:"createdAt"`
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
