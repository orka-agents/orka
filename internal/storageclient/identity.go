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
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

const callerEnvelopeStampRule = "a caller that supplies a write envelope must stamp identity and time before computing its digest"

const callerEnvelopeProtectRule = "a caller that supplies a write envelope must send an already redacted and truncated request"

func unprotected(method, field string) error {
	return store.ValidationErrorf("%s: %s would be changed before storage: %s", method, field, callerEnvelopeProtectRule)
}

func callerOwnsEnvelope(ctx context.Context) bool {
	_, ok := storagev1.WriteEnvelopeFrom(ctx)
	return ok
}

func unstamped(method, field string) error {
	return store.ValidationErrorf("%s: %s is missing: %s", method, field, callerEnvelopeStampRule)
}

func requireStampedSessionMessages(method string, messages []store.SessionMessage) error {
	for i, message := range messages {
		if message.ID == "" {
			return unstamped(method, fmt.Sprintf("the ID of transcript message %d", i))
		}
	}
	return nil
}

func (c *Client) GetMessages(ctx context.Context, namespace, taskName, parentTask string, markRead bool) ([]store.Message, error) {
	return store.ReadMessages(ctx, c.StateProvider, namespace, taskName, parentTask, markRead)
}

func (c *Client) SendMessage(ctx context.Context, msg *store.Message) error {
	if msg == nil {
		return store.ValidationErrorf("message is required")
	}
	if callerOwnsEnvelope(ctx) {
		if msg.CreatedAt.IsZero() {
			return unstamped("SendMessage", "the message's creation time")
		}
		return c.StateProvider.SendMessage(ctx, msg)
	}
	ctx, now := c.ops.withIssueTime(ctx)
	stamped := *msg
	if stamped.CreatedAt.IsZero() {
		stamped.CreatedAt = now
	}
	if err := c.StateProvider.SendMessage(ctx, &stamped); err != nil {
		return err
	}
	*msg = stamped
	return nil
}

func (c *Client) AppendMessages(ctx context.Context, namespace, name string, messages []store.SessionMessage) error {
	if callerOwnsEnvelope(ctx) {
		if err := requireStampedSessionMessages("AppendMessages", messages); err != nil {
			return err
		}
		return c.StateProvider.AppendMessages(ctx, namespace, name, messages)
	}
	ctx, now := c.ops.withIssueTime(ctx)
	identified, err := identifySessionMessages(messages, now)
	if err != nil {
		return err
	}
	return c.StateProvider.AppendMessages(ctx, namespace, name, identified)
}

func (c *Client) AppendMessagesWithLock(ctx context.Context, owner store.SessionOwnerWrite, messages []store.SessionMessage) error {
	if callerOwnsEnvelope(ctx) {
		if err := requireStampedSessionMessages("AppendMessagesWithLock", messages); err != nil {
			return err
		}
		return c.StateProvider.AppendMessagesWithLock(ctx, owner, messages)
	}
	ctx, now := c.ops.withIssueTime(ctx)
	identified, err := identifySessionMessages(messages, now)
	if err != nil {
		return err
	}
	return c.StateProvider.AppendMessagesWithLock(ctx, owner, identified)
}

func (c *Client) CommitSessionTurn(ctx context.Context, request store.CommitSessionTurnRequest) error {
	if callerOwnsEnvelope(ctx) {
		if err := requireStampedSessionMessages("CommitSessionTurn", request.Messages); err != nil {
			return err
		}
		return c.StateProvider.CommitSessionTurn(ctx, request)
	}
	ctx, now := c.ops.withIssueTime(ctx)
	identified, err := identifySessionMessages(request.Messages, now)
	if err != nil {
		return err
	}
	request.Messages = identified
	return c.StateProvider.CommitSessionTurn(ctx, request)
}

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

func requireStampedMemory(memory *store.Memory) error {
	switch {
	case memory.ID == "":
		return unstamped("CreateMemory", "the memory's ID")
	case memory.CreatedAt.IsZero():
		return unstamped("CreateMemory", "the memory's creation time")
	case memory.UpdatedAt.IsZero():
		return unstamped("CreateMemory", "the memory's update time")
	}
	return nil
}

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

func requireStampedMemoryProposal(proposal *store.MemoryProposal) error {
	switch {
	case proposal.ID == "":
		return unstamped("CreateMemoryProposal", "the proposal's ID")
	case proposal.CreatedAt.IsZero():
		return unstamped("CreateMemoryProposal", "the proposal's creation time")
	case proposal.UpdatedAt.IsZero():
		return unstamped("CreateMemoryProposal", "the proposal's update time")
	}
	return nil
}

func (c *Client) CreateDroppedFinding(ctx context.Context, dropped *store.DroppedFinding) error {
	if dropped == nil {
		return store.ValidationErrorf("dropped finding is required")
	}
	if callerOwnsEnvelope(ctx) {
		switch {
		case dropped.ID == "":
			return unstamped("CreateDroppedFinding", "the dropped finding's ID")
		case dropped.CreatedAt.IsZero():
			return unstamped("CreateDroppedFinding", "the dropped finding's creation time")
		}
		return c.StateProvider.CreateDroppedFinding(ctx, dropped)
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
