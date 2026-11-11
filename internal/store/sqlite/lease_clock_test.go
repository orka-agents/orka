/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// leaseClockBase is far from the wall clock, so a lease judged on the store's
// own clock gives a different answer than one judged at the request time.
var leaseClockBase = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

// A consuming read keeps the consumed IDs with its receipt, and a replay reads
// those messages again: it never returns a message sent later, and it marks
// nothing more.
func TestConsumeMessagesReplayReadsTheReceiptIDs(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	datasetID := advanceTestEpoch(t, s, testWriteFence)
	for _, content := range []string{"one", "two"} {
		require.NoError(t, s.SendMessage(ctx, &store.Message{
			Namespace: "tenant", FromTask: "writer", ToTask: "reader", ParentTask: "parent", Content: content,
		}))
	}
	request := store.ConsumeMessagesRequest{Namespace: "tenant", TaskName: "reader", ParentTask: "parent", Limit: 1}
	envelopeCtx, envelope := testEnvelope(t, ctx, datasetID, "ConsumeMessages", time.Now().UTC(), request)
	first, err := s.ConsumeMessages(envelopeCtx, request)
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.Equal(t, "one", first[0].Content)

	var receipt []byte
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT result FROM operation_receipts WHERE operation_id = ?`,
		envelope.OperationID).Scan(&receipt))
	ids, err := decodeReceiptResult[[]int64]("ConsumeMessages", receipt)
	require.NoError(t, err)
	require.Equal(t, []int64{first[0].ID}, ids, "the receipt keeps the consumed IDs, not the messages")

	replay, err := s.ConsumeMessages(envelopeCtx, request)
	require.NoError(t, err)
	require.Equal(t, first, replay)
	unread, err := s.ListUnreadMessages(ctx, "tenant", "reader", "parent", 0)
	require.NoError(t, err)
	require.Len(t, unread, 1, "the replay marked nothing more")
	require.Equal(t, "two", unread[0].Content)
}

// A consuming replay reads its messages again only from the reader's own
// inbox, so a row stored under a deleted message's ID for another Task is
// never returned to the reader and stays unread for its recipient.
func TestConsumeMessagesReplayRereadsOnlyItsInbox(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	datasetID := advanceTestEpoch(t, s, testWriteFence)
	for _, from := range []string{"writer", "deleted-writer"} {
		require.NoError(t, s.SendMessage(ctx, &store.Message{
			Namespace: "tenant", FromTask: from, ToTask: "reader", ParentTask: "parent", Content: from,
		}))
	}
	request := store.ConsumeMessagesRequest{Namespace: "tenant", TaskName: "reader", ParentTask: "parent"}
	envelopeCtx, _ := testEnvelope(t, ctx, datasetID, "ConsumeMessages", time.Now().UTC(), request)
	first, err := s.ConsumeMessages(envelopeCtx, request)
	require.NoError(t, err)
	require.Equal(t, []string{"writer", "deleted-writer"}, messageContentsOf(first))
	require.NoError(t, s.DeleteTaskMessages(ctx, "tenant", "deleted-writer"))
	// SQLite never reuses an ID; store a row under the deleted one directly to
	// stand in for a backend that would.
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO messages (id, namespace, from_task, to_task, parent_task, content) VALUES (?, ?, ?, ?, ?, ?)`,
		first[1].ID, "tenant", "x", "victim", "other", "for the victim")
	require.NoError(t, err)

	replay, err := s.ConsumeMessages(envelopeCtx, request)
	require.NoError(t, err)
	require.Equal(t, first[:1], replay, "the replay returns only what is left of the reader's batch")
	victim, err := s.ListUnreadMessages(ctx, "tenant", "victim", "other", 0)
	require.NoError(t, err)
	require.Equal(t, []string{"for the victim"}, messageContentsOf(victim))
}

func messageContentsOf(messages []store.Message) []string {
	contents := make([]string, len(messages))
	for i, message := range messages {
		contents[i] = message.Content
	}
	return contents
}

// The SendMessage receipt keeps the assigned identity, not the content, so
// the content leaves the store with the message when cleanup deletes it, and
// a replay still writes the identity back.
func TestSendMessageReceiptKeepsOnlyTheIdentity(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	datasetID := advanceTestEpoch(t, s, testWriteFence)
	sent := &store.Message{
		Namespace: "tenant", FromTask: "writer", ToTask: "reader", ParentTask: "parent", Content: "secret plan",
		CreatedAt: leaseClockBase,
	}
	envelopeCtx, envelope := testEnvelope(t, ctx, datasetID, "SendMessage", time.Now().UTC(), sent)
	require.NoError(t, s.SendMessage(envelopeCtx, sent))
	require.NotZero(t, sent.ID)
	require.Equal(t, leaseClockBase, sent.CreatedAt)

	var receipt []byte
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT result FROM operation_receipts WHERE operation_id = ?`,
		envelope.OperationID).Scan(&receipt))
	require.NotContains(t, string(receipt), "secret plan", "the receipt does not keep the content")
	identity, err := decodeReceiptResult[sentMessage]("SendMessage", receipt)
	require.NoError(t, err)
	require.Equal(t, sentMessage{ID: sent.ID, CreatedAt: leaseClockBase}, identity)

	replayed := &store.Message{
		Namespace: "tenant", FromTask: "writer", ToTask: "reader", ParentTask: "parent", Content: "secret plan",
		CreatedAt: leaseClockBase,
	}
	require.NoError(t, s.SendMessage(envelopeCtx, replayed))
	require.Equal(t, sent.ID, replayed.ID, "a replay writes the original identity back")
	unread, err := s.ListUnreadMessages(ctx, "tenant", "reader", "parent", 0)
	require.NoError(t, err)
	require.Len(t, unread, 1, "the replay stored no second message")
}

// A send that is refused leaves the caller's message as it was: the store
// writes the identity back only once the write has committed.
func TestRefusedSendLeavesTheMessageUnchanged(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	fence, _, err := s.RegisterTaskDataFence(ctx, "tenant", "writer")
	require.NoError(t, err)
	require.NoError(t, s.DeleteTaskMessages(ctx, "tenant", "writer"))

	msg := &store.Message{Namespace: "tenant", FromTask: "writer", ToTask: "reader", ParentTask: "parent", Content: "late"}
	require.ErrorIs(t, s.SendMessage(store.WithTaskDataFence(ctx, fence), msg), store.ErrTaskDataCleanupChanged)
	require.Zero(t, msg.ID, "a refused send assigns no ID")
	require.True(t, msg.CreatedAt.IsZero(), "a refused send fills no creation time")
	unread, err := s.ListUnreadMessages(ctx, "tenant", "reader", "parent", 0)
	require.NoError(t, err)
	require.Empty(t, unread)
}

// A transient lock is judged at the time of each request, not on the store
// clock: an owner write before the expiry passes and one at or after it is
// fenced, and another owner reclaims the lock once it has expired.
func TestTransientLockExpiryIsJudgedAtTheRequestTime(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.CreateSession(ctx, &store.SessionRecord{
		Namespace: "tenant", Name: "session", SessionType: "task",
		CreatedAt: leaseClockBase, UpdatedAt: leaseClockBase,
	}))
	expiry := leaseClockBase.Add(time.Minute)
	require.ErrorIs(t, s.AcquireLockUntil(ctx, "tenant", "session", "owner-a", "uid-a", expiry, expiry), store.ErrValidation,
		"a transient lock must expire after its request time")
	require.NoError(t, s.AcquireLockUntil(ctx, "tenant", "session", "owner-a", "uid-a", leaseClockBase, expiry))

	owner := store.SessionOwnerWrite{Namespace: "tenant", Name: "session", OwnerName: "owner-a", OwnerUID: "uid-a",
		Now: expiry.Add(-time.Nanosecond)}
	require.NoError(t, s.UpdateTokenCountsWithLock(ctx, owner, 1, 1), "the lock holds until its expiry")
	require.NoError(t, s.AppendMessagesWithLock(ctx, owner, []store.SessionMessage{{ID: "m1", Role: "user", Content: "in time"}}))

	late := owner
	late.Now = expiry
	require.ErrorIs(t, s.UpdateTokenCountsWithLock(ctx, late, 1, 1), store.ErrFenced, "the lock has expired at its expiry")
	require.ErrorIs(t, s.AppendMessagesWithLock(ctx, late, []store.SessionMessage{{ID: "m2", Role: "user", Content: "late"}}),
		store.ErrFenced)

	locked, err := s.IsLocked(ctx, "tenant", "session", "owner-b", "uid-b", owner.Now)
	require.NoError(t, err)
	require.True(t, locked)
	require.ErrorIs(t, s.AcquireLock(ctx, "tenant", "session", "owner-b", "uid-b", owner.Now), store.ErrConflict)
	require.NoError(t, s.AcquireLock(ctx, "tenant", "session", "owner-b", "uid-b", expiry),
		"another owner reclaims the lock once it has expired")

	session, err := s.GetSession(ctx, "tenant", "session")
	require.NoError(t, err)
	require.Equal(t, 1, session.InputTokens)
	require.Equal(t, 1, session.MessageCount)
	require.Equal(t, "owner-b", session.ActiveTask)
}

// Session cleanup judges a chat turn reservation at the plan's preparation
// time.
func TestSessionCleanupJudgesLocksAtThePreparationTime(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	session := &store.SessionRecord{Namespace: "tenant", Name: "chat", SessionType: store.SessionTypeChat}
	expiry := leaseClockBase.Add(time.Minute)
	_, err := s.AcquireChatTurn(ctx, store.AcquireChatTurnRequest{
		Session: session, TurnID: "turn", Now: leaseClockBase, ExpiresAt: expiry,
	})
	require.NoError(t, err)
	intent := store.SessionCleanupIntent{
		Namespace: "tenant", SessionName: "chat", OperationID: "delete-chat",
		OperationDigest: storagev1.CanonicalBytesDigest([]byte("delete-chat")), PreparedAt: expiry.Add(-time.Nanosecond),
	}
	_, err = s.PrepareSessionCleanup(ctx, intent)
	require.ErrorIs(t, err, store.ErrConflict, "the reservation is live at the preparation time")
	intent.PreparedAt = expiry
	_, err = s.PrepareSessionCleanup(ctx, intent)
	require.NoError(t, err, "the reservation has expired at the preparation time")
}
