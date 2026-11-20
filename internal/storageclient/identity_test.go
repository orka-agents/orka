package storageclient

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// recordingIdentityStore records the transcript messages each attempt of a
// write reached the store with, and the records the create methods received.
type recordingIdentityStore struct {
	*lossyStore
	appended  [][]store.SessionMessage
	memories  []store.Memory
	proposals []store.MemoryProposal
	dropped   []store.DroppedFinding
}

func (s *recordingIdentityStore) CreateMemory(ctx context.Context, memory *store.Memory) error {
	return s.intercept(ctx, "CreateMemory", func(ctx context.Context) error {
		s.mu.Lock()
		s.memories = append(s.memories, *memory)
		s.mu.Unlock()
		return s.Store.CreateMemory(ctx, memory)
	})
}

func (s *recordingIdentityStore) CreateMemoryProposal(ctx context.Context, proposal *store.MemoryProposal) error {
	return s.intercept(ctx, "CreateMemoryProposal", func(ctx context.Context) error {
		s.mu.Lock()
		s.proposals = append(s.proposals, *proposal)
		s.mu.Unlock()
		return s.Store.CreateMemoryProposal(ctx, proposal)
	})
}

func (s *recordingIdentityStore) CreateDroppedFinding(ctx context.Context, dropped *store.DroppedFinding) error {
	return s.intercept(ctx, "CreateDroppedFinding", func(ctx context.Context) error {
		s.mu.Lock()
		s.dropped = append(s.dropped, *dropped)
		s.mu.Unlock()
		return s.Store.CreateDroppedFinding(ctx, dropped)
	})
}

func (s *recordingIdentityStore) AppendMessages(ctx context.Context, namespace, name string, messages []store.SessionMessage) error {
	return s.intercept(ctx, "AppendMessages", func(ctx context.Context) error {
		s.mu.Lock()
		s.appended = append(s.appended, append([]store.SessionMessage(nil), messages...))
		s.mu.Unlock()
		return s.Store.AppendMessages(ctx, namespace, name, messages)
	})
}

func newIdentityFixture(t *testing.T) (*lossyFixture, *recordingIdentityStore) {
	t.Helper()
	f := newLossyFixture(t)
	recording := &recordingIdentityStore{lossyStore: f.lossy}
	client, err := New(Options{
		State: recording, Artifacts: recording, Embedded: recording,
		Fences: staticFences{datasetID: f.datasetID, fence: testFence},
	})
	if err != nil {
		t.Fatal(err)
	}
	client.ops.retryHorizon = f.client.ops.retryHorizon
	f.client = client
	return f, recording
}

// The client gives a transcript message without an ID a UUIDv7 and the
// operation's issue time before the request digest, so every attempt sends
// the same messages. A message with an ID keeps its zero timestamp, and the
// caller's slice is left as it was.
func TestClientAssignsTranscriptMessageIdentity(t *testing.T) {
	f, recording := newIdentityFixture(t)
	ctx := t.Context()
	if err := f.client.CreateSession(ctx, &store.SessionRecord{Namespace: "tenant", Name: "session", SessionType: "task"}); err != nil {
		t.Fatal(err)
	}
	messages := []store.SessionMessage{
		{Role: "user", Content: "unnamed"},
		{ID: "caller-id", Role: "assistant", Content: "named"},
	}
	f.lossy.plan("AppendMessages", stepLose)
	if err := f.client.AppendMessages(ctx, "tenant", "session", messages); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	if messages[0].ID != "" || !messages[0].Timestamp.IsZero() {
		t.Fatalf("the caller's messages changed: %#v", messages)
	}
	requireRetriedWithOneEnvelope(t, f, "AppendMessages", 2)
	if len(recording.appended) != 2 {
		t.Fatalf("attempts = %d, want 2", len(recording.appended))
	}
	first, retry := recording.appended[0], recording.appended[1]
	id, err := uuid.Parse(first[0].ID)
	if err != nil || id.Version() != 7 {
		t.Fatalf("assigned ID %q is not a UUIDv7 (%v)", first[0].ID, err)
	}
	issuedAt := f.lossy.seen("AppendMessages")[0].IssuedAt
	if !first[0].Timestamp.Equal(issuedAt) {
		t.Fatalf("assigned timestamp %s, want the operation's issue time %s", first[0].Timestamp, issuedAt)
	}
	if first[1].ID != "caller-id" || !first[1].Timestamp.IsZero() {
		t.Fatalf("a message with an ID = %#v, want it unchanged", first[1])
	}
	if first[0].ID != retry[0].ID || !first[0].Timestamp.Equal(retry[0].Timestamp) {
		t.Fatalf("the retry sent %#v, the first attempt %#v", retry[0], first[0])
	}
	session := f.session(t)
	if session.MessageCount != 2 || len(session.Messages) != 2 || session.Messages[0].ID != first[0].ID {
		t.Fatalf("session = %d messages %#v, want the two messages once", session.MessageCount, session.Messages)
	}
}

// A chat turn's messages get identities in the same way.
func TestClientAssignsChatTurnMessageIdentity(t *testing.T) {
	f := newLossyFixture(t)
	ctx := t.Context()
	now := time.Now().UTC()
	session := &store.SessionRecord{Namespace: "tenant", Name: "chat", SessionType: store.SessionTypeChat}
	if _, err := f.client.AcquireChatTurn(ctx, store.AcquireChatTurnRequest{
		Session: session, TurnID: "turn", Now: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.client.CommitSessionTurn(ctx, store.CommitSessionTurnRequest{
		Session: session, TurnID: "turn", Messages: []store.SessionMessage{{Role: "user", Content: "hello"}},
		Now: now,
	}); err != nil {
		t.Fatalf("CommitSessionTurn: %v", err)
	}
	stored, err := f.client.GetSession(ctx, "tenant", "chat")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Messages) != 1 {
		t.Fatalf("messages = %#v", stored.Messages)
	}
	if id, err := uuid.Parse(stored.Messages[0].ID); err != nil || id.Version() != 7 {
		t.Fatalf("stored message ID %q is not a client-assigned UUIDv7 (%v)", stored.Messages[0].ID, err)
	}
}

// SendMessage stamps the creation time with the operation's issue time, and
// the caller receives the assigned ID.
func TestClientStampsSentMessages(t *testing.T) {
	f := newLossyFixture(t)
	sent := message("hello")
	if err := f.client.SendMessage(t.Context(), sent); err != nil {
		t.Fatal(err)
	}
	issuedAt := f.lossy.seen("SendMessage")[0].IssuedAt
	if !sent.CreatedAt.Equal(issuedAt) || sent.ID == 0 {
		t.Fatalf("sent message = %#v, want the issue time %s and an assigned ID", sent, issuedAt)
	}
	inbox := f.inbox(t)
	if len(inbox) != 1 || inbox[0].ID != sent.ID || !inbox[0].CreatedAt.Equal(issuedAt) {
		t.Fatalf("inbox = %#v", inbox)
	}
	if err := f.client.SendMessage(t.Context(), nil); !errors.Is(err, storagev1.ErrValidation) {
		t.Fatalf("SendMessage(nil) = %v, want ErrValidation", err)
	}
}

// Memories, memory proposals and dropped findings get their IDs and times
// from the client, in the formats a provider would use: the provider already
// receives them, with the operation's issue time, so it assigns nothing.
func TestClientAssignsRecordIdentities(t *testing.T) {
	f, recording := newIdentityFixture(t)
	ctx := t.Context()

	memory := &store.Memory{Namespace: "tenant", Content: "remember this"}
	if err := f.client.CreateMemory(ctx, memory); err != nil {
		t.Fatal(err)
	}
	issuedAt := f.lossy.seen("CreateMemory")[0].IssuedAt
	received := recording.memories[0]
	if len(received.ID) <= len("mem-") || received.ID[:4] != "mem-" || received.ID != memory.ID ||
		!received.CreatedAt.Equal(issuedAt) || !received.UpdatedAt.Equal(issuedAt) {
		t.Fatalf("the provider received memory %#v, want a mem- ID and the issue time %s", received, issuedAt)
	}
	stored, err := f.client.GetMemory(ctx, "tenant", memory.ID)
	if err != nil || !stored.CreatedAt.Equal(issuedAt) {
		t.Fatalf("stored memory = %#v, %v", stored, err)
	}

	proposal := &store.MemoryProposal{Namespace: "tenant", Title: "a skill", Content: "do it"}
	if err := f.client.CreateMemoryProposal(ctx, proposal); err != nil {
		t.Fatal(err)
	}
	issuedAt = f.lossy.seen("CreateMemoryProposal")[0].IssuedAt
	receivedProposal := recording.proposals[0]
	if len(receivedProposal.ID) <= len("mprop-") || receivedProposal.ID[:6] != "mprop-" || receivedProposal.ID != proposal.ID ||
		!receivedProposal.CreatedAt.Equal(issuedAt) || !receivedProposal.UpdatedAt.Equal(issuedAt) {
		t.Fatalf("the provider received proposal %#v, want an mprop- ID and the issue time %s", receivedProposal, issuedAt)
	}

	dropped := &store.DroppedFinding{Namespace: "tenant", RepositoryScan: "scan", ScanRunID: "run", Reason: "invalid"}
	if err := f.client.CreateDroppedFinding(ctx, dropped); err != nil {
		t.Fatal(err)
	}
	issuedAt = f.lossy.seen("CreateDroppedFinding")[0].IssuedAt
	receivedDropped := recording.dropped[0]
	if len(receivedDropped.ID) != len("drop_")+12 || receivedDropped.ID[:5] != "drop_" || receivedDropped.ID != dropped.ID ||
		!receivedDropped.CreatedAt.Equal(issuedAt) {
		t.Fatalf("the provider received dropped finding %#v, want a drop_ ID and the issue time %s", receivedDropped, issuedAt)
	}
	kept := &store.DroppedFinding{ID: "drop_caller", Namespace: "tenant", RepositoryScan: "scan", ScanRunID: "run", Reason: "invalid"}
	if err := f.client.CreateDroppedFinding(ctx, kept); err != nil || kept.ID != "drop_caller" || recording.dropped[1].ID != "drop_caller" {
		t.Fatalf("a caller's ID = %q, %v; want it kept", kept.ID, err)
	}
}

// A plain GetMessages lists the inbox without a write, and both forms are
// bounded to MaxMessageBatch messages.
func TestGetMessagesMapsOntoBoundedProviderReads(t *testing.T) {
	f := newLossyFixture(t)
	ctx := t.Context()
	for range store.MaxMessageBatch + 1 {
		if err := f.client.SendMessage(ctx, message("queued")); err != nil {
			t.Fatal(err)
		}
	}
	peek, err := f.client.GetMessages(ctx, "tenant", "reader", "parent", false)
	if err != nil || len(peek) != store.MaxMessageBatch {
		t.Fatalf("peek = %d messages, %v; want the batch cap", len(peek), err)
	}
	if consumed := f.lossy.seen("ConsumeMessages"); len(consumed) != 0 {
		t.Fatalf("a plain read ran %d consuming writes", len(consumed))
	}
	first, err := f.client.GetMessages(ctx, "tenant", "reader", "parent", true)
	if err != nil || len(first) != store.MaxMessageBatch {
		t.Fatalf("first consuming read = %d messages, %v", len(first), err)
	}
	second, err := f.client.GetMessages(ctx, "tenant", "reader", "parent", true)
	if err != nil || len(second) != 1 {
		t.Fatalf("second consuming read = %d messages, %v; want the rest", len(second), err)
	}
	if consumed := f.lossy.seen("ConsumeMessages"); len(consumed) != 2 || consumed[0].OperationID == consumed[1].OperationID {
		t.Fatalf("consuming reads = %#v, want one operation each", consumed)
	}
}
