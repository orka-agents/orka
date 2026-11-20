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

// The methods in this file give a write the identities and record times a
// provider would otherwise assign, once per operation and before its request
// digest is computed, so every retry of the operation sends the same request
// and no provider clock or ID generator decides what Orka stores. Record
// times are the operation's IssuedAt. A provider still fills a value a caller
// that bypasses the client leaves empty.
//
// A caller that supplies its own write envelope in the context owns the
// operation, and the envelope's digest already covers its request, so the
// client sends that request unchanged and assigns nothing. Such a caller
// stamps the identities and times itself before computing the digest; a
// request that lacks one is refused with ErrValidation rather than stored
// without it. The same holds for what the client would redact, bound or
// otherwise prepare (protect.go, gateway.go): a caller-owned request that
// preparation would change is refused, and one it would not change is sent
// as it is.

// callerEnvelopeStampRule is why a caller-owned request that lacks a stamp is
// refused.
const callerEnvelopeStampRule = "a caller that supplies a write envelope must stamp identity and time before computing its digest"

// callerEnvelopeProtectRule is why a caller-owned request that the client
// would redact, bound or otherwise prepare is refused. The client sends a
// caller-owned request unchanged, so it must already be in the form the
// client would send.
const callerEnvelopeProtectRule = "a caller that supplies a write envelope must send an already redacted and truncated request"

// unprotected refuses a caller-owned request whose field the client would
// change before storing it.
func unprotected(method, field string) error {
	return store.ValidationErrorf("%s: %s would be changed before storage: %s", method, field, callerEnvelopeProtectRule)
}

// callerOwnsEnvelope reports whether ctx already carries a write envelope, so
// the client must not change the request it covers.
func callerOwnsEnvelope(ctx context.Context) bool {
	_, ok := storagev1.WriteEnvelopeFrom(ctx)
	return ok
}

// unstamped refuses a caller-owned request that lacks field.
func unstamped(method, field string) error {
	return store.ValidationErrorf("%s: %s is missing: %s", method, field, callerEnvelopeStampRule)
}

// requireStampedSessionMessages refuses caller-owned transcript messages
// without an ID. A message with an ID may keep a zero timestamp, as the
// client leaves it.
func requireStampedSessionMessages(method string, messages []store.SessionMessage) error {
	for i, message := range messages {
		if message.ID == "" {
			return unstamped(method, fmt.Sprintf("the ID of transcript message %d", i))
		}
	}
	return nil
}

// GetMessages implements Orka's message store over the provider contract: a
// consuming read is ConsumeMessages and a plain one ListUnreadMessages, each
// bounded to MaxMessageBatch messages.
func (c *Client) GetMessages(ctx context.Context, namespace, taskName, parentTask string, markRead bool) ([]store.Message, error) {
	return store.ReadMessages(ctx, c.StateProvider, namespace, taskName, parentTask, markRead)
}

// SendMessage stamps the message's creation time before storing it. The
// stamp goes on a copy, and the stored ID and creation time reach msg only
// once the write succeeds, so a write whose outcome stays unknown leaves msg
// as the caller passed it.
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

// AppendMessages gives every transcript message without an ID or timestamp
// one before appending them.
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

// AppendMessagesWithLock gives every transcript message without an ID or
// timestamp one before appending them under the owner's lock.
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

// CommitSessionTurn gives every turn message without an ID or timestamp one
// before committing the turn.
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

// requireStampedMemory refuses a caller-owned memory that lacks what
// identifyMemory would assign.
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

// requireStampedMemoryProposal refuses a caller-owned proposal that lacks
// what identifyMemoryProposal would assign.
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

// CreateDroppedFinding gives a diagnostic without an ID or creation time one
// before recording it. The ID hashes the diagnostic and its creation time, in
// the drop_ format the provider would use.
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
