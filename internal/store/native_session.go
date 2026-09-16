package store

import (
	"context"
	"encoding/json"
	"time"
)

const (
	// NativeSessionSnapshotMaxBytes bounds each private provider snapshot.
	NativeSessionSnapshotMaxBytes = 512 * 1024
	// NativeSessionSnapshotMaxTTL bounds how long a snapshot may be retained.
	NativeSessionSnapshotMaxTTL = 7 * 24 * time.Hour
)

// NativeSessionSnapshot is private provider state for one finalized Session
// turn. It is never part of a Session DTO, artifact, execution event, or Task
// status. The transcript remains the authority for whether it can be restored.
type NativeSessionSnapshot struct {
	ID                     string         `json:"id"`
	Namespace              string         `json:"namespace"`
	SessionName            string         `json:"sessionName"`
	SessionUID             string         `json:"sessionUid"`
	NamespaceUID           string         `json:"namespaceUid"`
	Key                    SessionTurnKey `json:"key"`
	ProviderSessionID      string         `json:"providerSessionId"`
	ProviderVersion        string         `json:"providerVersion"`
	ProfileDigest          string         `json:"profileDigest"`
	ConfigurationDigest    string         `json:"configurationDigest"`
	WorkspaceBindingDigest string         `json:"workspaceBindingDigest"`
	WorkspaceDigest        string         `json:"workspaceDigest"`
	WorkspaceStateDigest   string         `json:"workspaceStateDigest"`
	WorkingDirectory       string         `json:"workingDirectory"`
	Data                   []byte         `json:"-"`
	DataDigest             string         `json:"dataDigest"`
	HistoryDigest          string         `json:"historyDigest,omitempty"`
	HistoryMessageCount    int            `json:"historyMessageCount,omitempty"`
	ThroughMessageID       string         `json:"throughMessageId,omitempty"`
	CreatedAt              time.Time      `json:"createdAt"`
	ExpiresAt              time.Time      `json:"expiresAt"`
}

// NativeSessionSnapshotStore persists at most one snapshot per Session.
// Staging requires the exact latest open SessionTurn and complete pre-prompt
// transcript digest. A staged record is invisible until transcript finalization
// and the coordinating control store's projection activation both commit.
// Callers must also validate current Kubernetes mutation authority and every
// runtime, workspace, and history binding before staging or restoring state.
type NativeSessionSnapshotStore interface {
	StageNativeSessionSnapshot(context.Context, NativeSessionSnapshot, string) error
	GetNativeSessionSnapshot(context.Context, string, string, string) (*NativeSessionSnapshot, error)
}

// NativeSessionHistoryDigest binds ordered canonical transcript content and
// message identities without timestamps or database-local sort numbers.
func NativeSessionHistoryDigest(messages []SessionMessage) (string, error) {
	type historyMessage struct {
		ID         string            `json:"id"`
		Role       string            `json:"role"`
		Content    string            `json:"content"`
		Name       string            `json:"name,omitempty"`
		Input      map[string]any    `json:"input,omitempty"`
		ToolCalls  any               `json:"toolCalls,omitempty"`
		ToolCallID string            `json:"toolCallId,omitempty"`
		SourceType string            `json:"sourceType,omitempty"`
		SourceRef  string            `json:"sourceRef,omitempty"`
		Metadata   map[string]string `json:"metadata,omitempty"`
	}
	canonical := make([]historyMessage, len(messages))
	for i, message := range messages {
		canonical[i] = historyMessage{
			ID: message.ID, Role: message.Role, Content: message.Content, Name: message.Name,
			Input: message.Input, ToolCalls: message.ToolCalls, ToolCallID: message.ToolCallID,
			SourceType: message.SourceType, SourceRef: message.SourceRef, Metadata: message.Metadata,
		}
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	return CanonicalBytesDigest(data), nil
}
