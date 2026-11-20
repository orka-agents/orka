package storageclient

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// The digest-checking store also records, for the writes the client stamps,
// whether each request the provider receives matches its envelope's digest.

func (s *digestCheckingStore) AppendMessages(ctx context.Context, namespace, name string, messages []store.SessionMessage) error {
	s.check(ctx, "AppendMessages", namespace, name, messages)
	return s.Store.AppendMessages(ctx, namespace, name, messages)
}

func (s *digestCheckingStore) AppendMessagesWithLock(ctx context.Context, owner store.SessionOwnerWrite, messages []store.SessionMessage) error {
	s.check(ctx, "AppendMessagesWithLock", owner, messages)
	return s.Store.AppendMessagesWithLock(ctx, owner, messages)
}

func (s *digestCheckingStore) CommitSessionTurn(ctx context.Context, request store.CommitSessionTurnRequest) error {
	s.check(ctx, "CommitSessionTurn", request)
	return s.Store.CommitSessionTurn(ctx, request)
}

func (s *digestCheckingStore) CreateMemory(ctx context.Context, memory *store.Memory) error {
	s.check(ctx, "CreateMemory", memory)
	return s.Store.CreateMemory(ctx, memory)
}

func (s *digestCheckingStore) CreateMemoryProposal(ctx context.Context, proposal *store.MemoryProposal) error {
	s.check(ctx, "CreateMemoryProposal", proposal)
	return s.Store.CreateMemoryProposal(ctx, proposal)
}

func (s *digestCheckingStore) CreateDroppedFinding(ctx context.Context, dropped *store.DroppedFinding) error {
	s.check(ctx, "CreateDroppedFinding", dropped)
	return s.Store.CreateDroppedFinding(ctx, dropped)
}

func (s *digestCheckingStore) attempts(method string) []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.matched[method]...)
}

// callerEnvelope returns ctx carrying an envelope its caller built for method
// and args, as a caller that owns the operation does.
func callerEnvelope(t *testing.T, ctx context.Context, f *lossyFixture, method string, args ...any) context.Context {
	t.Helper()
	digest, err := storagev1.RequestDigest(method, args...)
	if err != nil {
		t.Fatal(err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	return storagev1.WithWriteEnvelope(ctx, storagev1.WriteEnvelope{
		DatasetID: f.datasetID, Fence: testFence, OperationID: id.String(), RequestDigest: digest,
		IssuedAt: time.Now().UTC(), Sequence: 1,
	})
}

var callerStamp = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

// A caller that supplies its own write envelope and stamps its request sends
// that request to the provider unchanged: the digest recomputed from what the
// provider receives matches the caller's envelope. The client assigns no
// identity or time, not even a transcript message's zero timestamp.
func TestCallerEnvelopeRequestReachesTheProviderUnchanged(t *testing.T) {
	f, checking := newDigestFixture(t)
	ctx := t.Context()

	sent := &store.Message{
		Namespace: "tenant", FromTask: "writer", ToTask: "reader", ParentTask: "parent", Content: "stamped", CreatedAt: callerStamp,
	}
	if err := f.client.SendMessage(callerEnvelope(t, ctx, f, "SendMessage", sent), sent); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	requireEveryAttemptMatched(t, checking, "SendMessage", 1)
	if inbox := f.inbox(t); len(inbox) != 1 || !inbox[0].CreatedAt.Equal(callerStamp) || inbox[0].ID != sent.ID {
		t.Fatalf("inbox = %#v, want the caller's message with its own time", inbox)
	}

	if err := f.client.CreateSession(ctx, &store.SessionRecord{Namespace: "tenant", Name: "session", SessionType: "task"}); err != nil {
		t.Fatal(err)
	}
	transcript := []store.SessionMessage{
		{ID: "caller-1", Role: "user", Content: "hello", Timestamp: callerStamp},
		{ID: "caller-2", Role: "assistant", Content: "hi"},
	}
	if err := f.client.AppendMessages(callerEnvelope(t, ctx, f, "AppendMessages", "tenant", "session", transcript),
		"tenant", "session", transcript); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	requireEveryAttemptMatched(t, checking, "AppendMessages", 1)
	if !transcript[1].Timestamp.IsZero() {
		t.Fatalf("the caller's transcript changed: %#v", transcript)
	}

	memory := &store.Memory{ID: "mem-caller", Namespace: "tenant", Content: "remember this", CreatedAt: callerStamp, UpdatedAt: callerStamp}
	if err := f.client.CreateMemory(callerEnvelope(t, ctx, f, "CreateMemory", memory), memory); err != nil {
		t.Fatalf("CreateMemory: %v", err)
	}
	requireEveryAttemptMatched(t, checking, "CreateMemory", 1)
}

// A caller that supplies its own write envelope but leaves out an identity or
// time the client would assign is refused with ErrValidation before the
// provider is called, so nothing is written without it.
func TestCallerEnvelopeRefusesAnUnstampedRequest(t *testing.T) {
	owner := store.SessionOwnerWrite{Namespace: "tenant", Name: "session", OwnerName: "task", OwnerUID: "uid", Now: callerStamp}
	unnamed := []store.SessionMessage{{Role: "user", Content: "no ID"}}
	cases := map[string]func(*testing.T, *lossyFixture) error{
		"SendMessage": func(t *testing.T, f *lossyFixture) error {
			msg := &store.Message{Namespace: "tenant", FromTask: "writer", ToTask: "reader", ParentTask: "parent", Content: "untimed"}
			return f.client.SendMessage(callerEnvelope(t, t.Context(), f, "SendMessage", msg), msg)
		},
		"AppendMessages": func(t *testing.T, f *lossyFixture) error {
			return f.client.AppendMessages(callerEnvelope(t, t.Context(), f, "AppendMessages", "tenant", "session", unnamed),
				"tenant", "session", unnamed)
		},
		"AppendMessagesWithLock": func(t *testing.T, f *lossyFixture) error {
			return f.client.AppendMessagesWithLock(callerEnvelope(t, t.Context(), f, "AppendMessagesWithLock", owner, unnamed), owner, unnamed)
		},
		"CommitSessionTurn": func(t *testing.T, f *lossyFixture) error {
			request := store.CommitSessionTurnRequest{
				Session: &store.SessionRecord{Namespace: "tenant", Name: "chat"}, TurnID: "turn", Messages: unnamed, Now: callerStamp,
			}
			return f.client.CommitSessionTurn(callerEnvelope(t, t.Context(), f, "CommitSessionTurn", request), request)
		},
		"CreateMemory": func(t *testing.T, f *lossyFixture) error {
			memory := &store.Memory{Namespace: "tenant", Content: "no ID", CreatedAt: callerStamp, UpdatedAt: callerStamp}
			return f.client.CreateMemory(callerEnvelope(t, t.Context(), f, "CreateMemory", memory), memory)
		},
		"CreateMemoryProposal": func(t *testing.T, f *lossyFixture) error {
			proposal := &store.MemoryProposal{ID: "mprop-caller", Namespace: "tenant", Title: "untimed", Content: "do it"}
			return f.client.CreateMemoryProposal(callerEnvelope(t, t.Context(), f, "CreateMemoryProposal", proposal), proposal)
		},
		"CreateDroppedFinding": func(t *testing.T, f *lossyFixture) error {
			dropped := &store.DroppedFinding{Namespace: "tenant", RepositoryScan: "scan", ScanRunID: "run", Reason: "no ID", CreatedAt: callerStamp}
			return f.client.CreateDroppedFinding(callerEnvelope(t, t.Context(), f, "CreateDroppedFinding", dropped), dropped)
		},
	}
	for method, write := range cases {
		t.Run(method, func(t *testing.T) {
			f, checking := newDigestFixture(t)
			err := write(t, f)
			if !errors.Is(err, store.ErrValidation) || !strings.Contains(err.Error(), callerEnvelopeStampRule) {
				t.Fatalf("%s = %v, want ErrValidation naming the stamping rule", method, err)
			}
			if attempts := checking.attempts(method); len(attempts) != 0 {
				t.Fatalf("%s reached the provider %d times; want it refused before", method, len(attempts))
			}
		})
	}

	t.Run("nothing is stored", func(t *testing.T) {
		f, _ := newDigestFixture(t)
		msg := &store.Message{Namespace: "tenant", FromTask: "writer", ToTask: "reader", ParentTask: "parent", Content: "untimed"}
		if err := f.client.SendMessage(callerEnvelope(t, t.Context(), f, "SendMessage", msg), msg); !errors.Is(err, store.ErrValidation) {
			t.Fatalf("SendMessage = %v, want ErrValidation", err)
		}
		if inbox := f.inbox(t); len(inbox) != 0 || msg.ID != 0 || !msg.CreatedAt.IsZero() {
			t.Fatalf("inbox = %#v, message = %#v; want nothing stored and the caller's message unchanged", inbox, msg)
		}
	})
}
