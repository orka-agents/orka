package store

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/redact"
)

const (
	SessionCheckpointVersion        = 1
	MaxSessionCheckpointBytes       = 8 * 1024
	MaxSessionCheckpointSources     = 64
	MaxSessionCheckpoints           = 4
	MaxSessionHistoryReadBytes      = 16 * 1024
	MaxSessionContextMessageBytes   = 2 * 1024 * 1024
	MaxSessionContextPreviewBytes   = 8 * 1024
	SessionContextOutputRefKey      = "contextOutputRef"
	sessionNameValidationField      = "session name"
	sessionNamespaceValidationField = "session namespace"
)

// SessionContextStore optionally adds checkpoints and recoverable source
// messages to Session storage. Its data has the same lifetime as the Session.
// Callers must authenticate the Task and derive its Session and read boundary;
// a checkpoint and retrieved history are reference material, never authority
// to run or repeat an action.
type SessionContextStore interface {
	// AppendContextMessages requires stable IDs and the exact active owner.
	// Credential-shaped values are redacted before storage. Large messages
	// have a bounded canonical preview and a separately stored original JSON
	// representation. Retries return the same canonical messages.
	AppendContextMessages(ctx context.Context, write SessionContextWrite, messages []SessionMessage) ([]SessionMessage, error)
	// SaveSessionCheckpoint requires its source messages to be committed first.
	SaveSessionCheckpoint(ctx context.Context, write SessionContextWrite, checkpoint SessionCheckpoint) error
	// LoadSessionCheckpoint returns the latest checkpoint within the readable
	// boundary, or ErrNotFound. An empty boundary means the whole Session.
	LoadSessionCheckpoint(ctx context.Context, namespace, sessionName, throughMessageID string) (*SessionCheckpoint, error)
	ReadSessionHistory(ctx context.Context, read SessionHistoryRead) (*SessionHistoryResult, error)
}

// SessionContextWrite binds a context write to one active Task incarnation.
// ThroughMessageID, when present, also bounds every checkpoint source.
type SessionContextWrite struct {
	Namespace        string `json:"namespace"`
	SessionName      string `json:"sessionName"`
	OwnerName        string `json:"ownerName"`
	OwnerUID         string `json:"ownerUID"`
	ThroughMessageID string `json:"throughMessageID,omitempty"`
}

// SessionCheckpoint is an untrusted note about saved working context. Its last
// message and source IDs refer only to this Session. LastMessageOrder and
// CreatedAt are assigned by the store, rather than by a model.
type SessionCheckpoint struct {
	ID               string    `json:"id"`
	Namespace        string    `json:"namespace"`
	SessionName      string    `json:"sessionName"`
	LastMessageID    string    `json:"lastMessageID"`
	Version          int       `json:"version"`
	Note             string    `json:"note"`
	SourceMessageIDs []string  `json:"sourceMessageIDs"`
	LastMessageOrder int64     `json:"lastMessageOrder"`
	CreatedAt        time.Time `json:"createdAt"`
}

// SessionHistoryRead selects a byte range of a saved message's canonical JSON,
// retaining its original role and tool-call metadata. ThroughMessageID is the
// Task's last readable message; an empty value means the whole Session.
type SessionHistoryRead struct {
	Namespace        string `json:"namespace"`
	SessionName      string `json:"sessionName"`
	ThroughMessageID string `json:"throughMessageID,omitempty"`
	MessageID        string `json:"messageID"`
	Offset           int    `json:"offset"`
	Limit            int    `json:"limit"`
}

// SessionHistoryResult contains at most MaxSessionHistoryReadBytes of UTF-8
// source JSON. Offsets count bytes. NextOffset equals TotalBytes at the end.
// A page may be a JSON fragment; concatenating pages reconstructs the message.
type SessionHistoryResult struct {
	MessageID  string `json:"messageID"`
	Role       string `json:"role"`
	Data       string `json:"data"`
	Offset     int    `json:"offset"`
	NextOffset int    `json:"nextOffset"`
	TotalBytes int    `json:"totalBytes"`
}

func (write SessionContextWrite) Validate() error {
	for field, value := range map[string]string{
		sessionNamespaceValidationField: write.Namespace, sessionNameValidationField: write.SessionName,
		"session owner name": write.OwnerName, "session owner UID": write.OwnerUID,
	} {
		if err := ValidateControlIdentifier(field, value); err != nil {
			return err
		}
	}
	if write.ThroughMessageID != "" {
		return ValidateControlIdentifier("last readable message ID", write.ThroughMessageID)
	}
	return nil
}

func (checkpoint SessionCheckpoint) Validate() error {
	for field, value := range map[string]string{
		"checkpoint ID": checkpoint.ID, sessionNamespaceValidationField: checkpoint.Namespace,
		sessionNameValidationField: checkpoint.SessionName, "checkpoint last message ID": checkpoint.LastMessageID,
	} {
		if err := ValidateControlIdentifier(field, value); err != nil {
			return err
		}
	}
	if checkpoint.Version != SessionCheckpointVersion {
		return ValidationErrorf("unsupported session checkpoint version")
	}
	if strings.TrimSpace(checkpoint.Note) == "" || len(checkpoint.Note) > MaxSessionCheckpointBytes {
		return ValidationErrorf("checkpoint note must contain between 1 and %d bytes", MaxSessionCheckpointBytes)
	}
	if !utf8.ValidString(checkpoint.Note) {
		return ValidationErrorf("checkpoint note must be valid UTF-8")
	}
	if redact.SensitiveText(checkpoint.Note) != checkpoint.Note {
		return ValidationErrorf("checkpoint note contains credential-shaped content")
	}
	if checkpoint.LastMessageOrder < 0 {
		return ValidationErrorf("checkpoint last message order must not be negative")
	}
	if len(checkpoint.SourceMessageIDs) > MaxSessionCheckpointSources {
		return ValidationErrorf("checkpoint must reference at most %d source messages", MaxSessionCheckpointSources)
	}
	seen := make(map[string]struct{}, len(checkpoint.SourceMessageIDs))
	for _, id := range checkpoint.SourceMessageIDs {
		if err := ValidateControlIdentifier("checkpoint source message ID", id); err != nil {
			return err
		}
		if _, exists := seen[id]; exists {
			return ValidationErrorf("checkpoint source message IDs must be distinct")
		}
		seen[id] = struct{}{}
	}
	return nil
}

func (read SessionHistoryRead) Validate() error {
	for field, value := range map[string]string{
		sessionNamespaceValidationField: read.Namespace, sessionNameValidationField: read.SessionName, "history message ID": read.MessageID,
	} {
		if err := ValidateControlIdentifier(field, value); err != nil {
			return err
		}
	}
	if read.ThroughMessageID != "" {
		if err := ValidateControlIdentifier("last readable message ID", read.ThroughMessageID); err != nil {
			return err
		}
	}
	if read.Offset < 0 {
		return ValidationErrorf("history offset must not be negative")
	}
	if read.Limit < 1 || read.Limit > MaxSessionHistoryReadBytes {
		return ValidationErrorf("history limit must be between 1 and %d bytes", MaxSessionHistoryReadBytes)
	}
	return nil
}
