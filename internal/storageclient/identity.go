package storageclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/orka-agents/orka/internal/store"
)

// The methods in this file give a write the identities and record times a
// provider would otherwise assign, once per operation and before its request
// digest is computed, so every retry of the operation sends the same request
// and no provider clock or ID generator decides what Orka stores. Record
// times are the operation's IssuedAt. A provider still fills a value a caller
// that bypasses the client leaves empty.

// GetMessages implements Orka's message store over the provider contract: a
// consuming read is ConsumeMessages and a plain one ListUnreadMessages, each
// bounded to MaxMessageBatch messages.
func (c *Client) GetMessages(ctx context.Context, namespace, taskName, parentTask string, markRead bool) ([]store.Message, error) {
	return store.ReadMessages(ctx, c.StateProvider, namespace, taskName, parentTask, markRead)
}

// SendMessage stamps the message's creation time before storing it. The
// provider writes the assigned ID back into msg.
func (c *Client) SendMessage(ctx context.Context, msg *store.Message) error {
	if msg == nil {
		return store.ValidationErrorf("message is required")
	}
	ctx, now := c.ops.withIssueTime(ctx)
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = now
	}
	return c.StateProvider.SendMessage(ctx, msg)
}

// AppendMessages gives every transcript message without an ID or timestamp
// one before appending them.
func (c *Client) AppendMessages(ctx context.Context, namespace, name string, messages []store.SessionMessage) error {
	ctx, now := c.ops.withIssueTime(ctx)
	identified, err := identifySessionMessages(messages, now)
	if err != nil {
		return err
	}
	return c.StateProvider.AppendMessages(ctx, namespace, name, identified)
}

// AppendMessagesWithLock gives every transcript message without an ID or
// timestamp one before appending them under the owner's lock.
func (c *Client) AppendMessagesWithLock(ctx context.Context, owner store.SessionOwnerWrite, messages []store.SessionMessage) error {
	ctx, now := c.ops.withIssueTime(ctx)
	identified, err := identifySessionMessages(messages, now)
	if err != nil {
		return err
	}
	return c.StateProvider.AppendMessagesWithLock(ctx, owner, identified)
}

// CommitSessionTurn gives every turn message without an ID or timestamp one
// before committing the turn.
func (c *Client) CommitSessionTurn(ctx context.Context, request store.CommitSessionTurnRequest) error {
	ctx, now := c.ops.withIssueTime(ctx)
	identified, err := identifySessionMessages(request.Messages, now)
	if err != nil {
		return err
	}
	request.Messages = identified
	return c.StateProvider.CommitSessionTurn(ctx, request)
}

// identifySessionMessages returns messages with a new UUIDv7 ID, and now as
// the timestamp when it is zero, for each message that has no ID. A message
// that already has an ID is the caller's natural key and is left as it is: a
// provider matches a repeat of it without comparing a zero timestamp, so
// filling one would turn a later repeat into a mismatch. It copies the slice
// when it changes anything, so the caller's messages stay as they were.
func identifySessionMessages(messages []store.SessionMessage, now time.Time) ([]store.SessionMessage, error) {
	var identified []store.SessionMessage
	for i, message := range messages {
		if message.ID != "" {
			continue
		}
		if identified == nil {
			identified = append([]store.SessionMessage(nil), messages...)
		}
		id, err := uuid.NewV7()
		if err != nil {
			return nil, fmt.Errorf("generate a transcript message ID: %w", err)
		}
		identified[i].ID = id.String()
		if message.Timestamp.IsZero() {
			identified[i].Timestamp = now
		}
	}
	if identified == nil {
		return messages, nil
	}
	return identified, nil
}

// identifyMemory gives a memory without an ID one in the mem- format, and
// zero creation and update times the operation's issue time.
func identifyMemory(memory *store.Memory, now time.Time) {
	if memory.ID == "" {
		memory.ID = "mem-" + uuid.NewString()
	}
	if memory.CreatedAt.IsZero() {
		memory.CreatedAt = now
	}
	if memory.UpdatedAt.IsZero() {
		memory.UpdatedAt = now
	}
}

// identifyMemoryProposal gives a proposal without an ID one in the mprop-
// format, and zero creation and update times the operation's issue time.
func identifyMemoryProposal(proposal *store.MemoryProposal, now time.Time) {
	if proposal.ID == "" {
		proposal.ID = "mprop-" + uuid.NewString()
	}
	if proposal.CreatedAt.IsZero() {
		proposal.CreatedAt = now
	}
	if proposal.UpdatedAt.IsZero() {
		proposal.UpdatedAt = now
	}
}

// CreateDroppedFinding gives a diagnostic without an ID or creation time one
// before recording it. The ID hashes the diagnostic and its creation time, in
// the drop_ format the provider would use.
func (c *Client) CreateDroppedFinding(ctx context.Context, dropped *store.DroppedFinding) error {
	if dropped == nil {
		return store.ValidationErrorf("dropped finding is required")
	}
	ctx, now := c.ops.withIssueTime(ctx)
	if dropped.CreatedAt.IsZero() {
		dropped.CreatedAt = now
	}
	if dropped.ID == "" {
		sum := sha256.Sum256([]byte(strings.Join([]string{
			dropped.Namespace, dropped.RepositoryScan, dropped.ScanRunID, dropped.TaskName, dropped.SliceID,
			dropped.Reason, dropped.SampleJSON, dropped.CreatedAt.UTC().Format(time.RFC3339Nano),
		}, "|")))
		dropped.ID = "drop_" + hex.EncodeToString(sum[:])[:12]
	}
	return c.StateProvider.CreateDroppedFinding(ctx, dropped)
}
