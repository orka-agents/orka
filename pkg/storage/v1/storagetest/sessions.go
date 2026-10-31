package storagetest

import (
	"fmt"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// Session types used by the contract. Gateway sessions are owned by gateway
// admission and are covered with the gateway families.
const (
	sessionsTypeTask = "task"
	sessionsTypeChat = storagev1.SessionTypeChat
)

func runResults(t *testing.T, newProvider StateFactory) {
	t.Run("save get replace and isolate", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		_, err := provider.GetResult(ctx, "tenant-a", "task")
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		require.NoError(t, provider.SaveResult(ctx, "tenant-a", "task", []byte("first")))
		require.NoError(t, provider.SaveResult(ctx, "tenant-a", "task", []byte("second")))
		got, err := provider.GetResult(ctx, "tenant-a", "task")
		require.NoError(t, err)
		require.Equal(t, []byte("second"), got)

		_, err = provider.GetResult(ctx, "tenant-b", "task")
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		require.NoError(t, provider.DeleteResult(ctx, "tenant-a", "task"))
		_, err = provider.GetResult(ctx, "tenant-a", "task")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
	})

	t.Run("prompt result receipts are immutable", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		receipt := storagev1.PromptResultReceipt{
			AttemptID: "attempt-1", Namespace: "tenant-a", TaskName: "task",
			OperationID: "operation-1", OperationDigest: storagev1.CanonicalBytesDigest([]byte("operation-1")),
			Data: []byte("result bytes"),
		}
		_, err := provider.GetPromptResultReceipt(ctx, receipt.AttemptID)
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		require.NoError(t, provider.SavePromptResultReceipt(ctx, receipt))
		require.NoError(t, provider.SavePromptResultReceipt(ctx, receipt), "an identical replay is accepted")
		got, err := provider.GetPromptResultReceipt(ctx, receipt.AttemptID)
		require.NoError(t, err)
		require.Equal(t, receipt, *got)

		changed := receipt
		changed.Data = []byte("other bytes")
		require.ErrorIs(t, provider.SavePromptResultReceipt(ctx, changed), storagev1.ErrDuplicateMismatch)
		got, err = provider.GetPromptResultReceipt(ctx, receipt.AttemptID)
		require.NoError(t, err)
		require.Equal(t, receipt.Data, got.Data, "a mismatched replay leaves the original receipt")

		invalid := receipt
		invalid.AttemptID = "attempt-2"
		invalid.OperationDigest = "not-a-digest"
		require.ErrorIs(t, provider.SavePromptResultReceipt(ctx, invalid), storagev1.ErrValidation)

		require.NoError(t, provider.DeleteResult(ctx, receipt.Namespace, receipt.TaskName))
		_, err = provider.GetPromptResultReceipt(ctx, receipt.AttemptID)
		require.ErrorIs(t, err, storagev1.ErrNotFound, "Task result cleanup removes its receipts")
	})
}

func runPlans(t *testing.T, newProvider StateFactory) {
	provider := newProvider(t)
	ctx := t.Context()
	_, err := provider.GetPlan(ctx, "tenant-a", "coordinator")
	require.ErrorIs(t, err, storagev1.ErrNotFound)

	plan := &storagev1.PlanState{Iteration: 1, Summary: "started", ProgressPct: 10, PlanDocument: "# plan"}
	require.NoError(t, provider.SavePlan(ctx, "tenant-a", "coordinator", plan))
	plan = &storagev1.PlanState{Iteration: 2, Summary: "done", ProgressPct: 100, GoalComplete: true, PlanDocument: "# done"}
	require.NoError(t, provider.SavePlan(ctx, "tenant-a", "coordinator", plan))

	got, err := provider.GetPlan(ctx, "tenant-a", "coordinator")
	require.NoError(t, err)
	require.Equal(t, "tenant-a", got.Namespace)
	require.Equal(t, "coordinator", got.TaskName)
	require.Equal(t, 2, got.Iteration)
	require.Equal(t, "done", got.Summary)
	require.Equal(t, 100, got.ProgressPct)
	require.True(t, got.GoalComplete)
	require.Equal(t, "# done", got.PlanDocument)
	require.False(t, got.CreatedAt.IsZero())
	require.False(t, got.UpdatedAt.Before(got.CreatedAt))

	_, err = provider.GetPlan(ctx, "tenant-b", "coordinator")
	require.ErrorIs(t, err, storagev1.ErrNotFound)

	require.NoError(t, provider.DeletePlan(ctx, "tenant-a", "coordinator"))
	_, err = provider.GetPlan(ctx, "tenant-a", "coordinator")
	require.ErrorIs(t, err, storagev1.ErrNotFound)
}

func runMessages(t *testing.T, newProvider StateFactory) {
	t.Run("direct and broadcast delivery within one parent", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		send := func(from, to, parent, content string) {
			t.Helper()
			require.NoError(t, provider.SendMessage(ctx, &storagev1.Message{
				Namespace: "tenant-a", FromTask: from, ToTask: to, ParentTask: parent, Content: content,
			}))
		}
		send("worker-a", "worker-b", "parent", "direct")
		send("worker-a", "*", "parent", "broadcast")
		send("worker-a", "worker-b", "other-parent", "other family")
		require.NoError(t, provider.SendMessage(ctx, &storagev1.Message{
			Namespace: "tenant-b", FromTask: "worker-a", ToTask: "worker-b", ParentTask: "parent", Content: "other namespace",
		}))

		peek, err := provider.ListUnreadMessages(ctx, "tenant-a", "worker-b", "parent", 0)
		require.NoError(t, err)
		require.Equal(t, []string{"direct", "broadcast"}, messageContents(peek))
		for _, message := range peek {
			require.Equal(t, "tenant-a", message.Namespace)
			require.Equal(t, "worker-a", message.FromTask)
			require.Equal(t, "parent", message.ParentTask)
			require.False(t, message.CreatedAt.IsZero(), "the provider assigns a creation time")
			require.Equal(t, time.UTC, message.CreatedAt.Location(), "creation times are UTC")
		}

		own, err := provider.ListUnreadMessages(ctx, "tenant-a", "worker-a", "parent", 0)
		require.NoError(t, err)
		require.Empty(t, own, "a sender does not receive its own broadcast")

		read, err := provider.ConsumeMessages(ctx, storagev1.ConsumeMessagesRequest{Namespace: "tenant-a", TaskName: "worker-b", ParentTask: "parent"})
		require.NoError(t, err)
		require.Equal(t, []string{"direct", "broadcast"}, messageContents(read))
		again, err := provider.ConsumeMessages(ctx, storagev1.ConsumeMessagesRequest{Namespace: "tenant-a", TaskName: "worker-b", ParentTask: "parent"})
		require.NoError(t, err)
		require.Empty(t, again, "marked messages are not returned again")

		other, err := provider.ListUnreadMessages(ctx, "tenant-a", "worker-b", "other-parent", 0)
		require.NoError(t, err)
		require.Equal(t, []string{"other family"}, messageContents(other))
		otherNamespace, err := provider.ListUnreadMessages(ctx, "tenant-b", "worker-b", "parent", 0)
		require.NoError(t, err)
		require.Equal(t, []string{"other namespace"}, messageContents(otherNamespace))
	})

	t.Run("task and parent cleanup", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		for _, message := range []storagev1.Message{
			{Namespace: "tenant-a", FromTask: "worker-a", ToTask: "worker-b", ParentTask: "parent", Content: "to b"},
			{Namespace: "tenant-a", FromTask: "worker-c", ToTask: "worker-a", ParentTask: "parent", Content: "to a"},
			{Namespace: "tenant-a", FromTask: "worker-c", ToTask: "worker-b", ParentTask: "parent", Content: "c to b"},
			{Namespace: "tenant-a", FromTask: "worker-x", ToTask: "worker-y", ParentTask: "second", Content: "to y"},
		} {
			require.NoError(t, provider.SendMessage(ctx, &message))
		}

		require.NoError(t, provider.DeleteTaskMessages(ctx, "tenant-a", "worker-a"))
		toB, err := provider.ListUnreadMessages(ctx, "tenant-a", "worker-b", "parent", 0)
		require.NoError(t, err)
		require.Equal(t, []string{"c to b"}, messageContents(toB), "messages sent by the Task are removed")
		toA, err := provider.ListUnreadMessages(ctx, "tenant-a", "worker-a", "parent", 0)
		require.NoError(t, err)
		require.Empty(t, toA, "messages received by the Task are removed")

		require.NoError(t, provider.DeleteParentMessages(ctx, "tenant-a", "parent"))
		toB, err = provider.ListUnreadMessages(ctx, "tenant-a", "worker-b", "parent", 0)
		require.NoError(t, err)
		require.Empty(t, toB)
		toY, err := provider.ListUnreadMessages(ctx, "tenant-a", "worker-y", "second", 0)
		require.NoError(t, err)
		require.Equal(t, []string{"to y"}, messageContents(toY), "other parents keep their messages")
	})
}

func runSessions(t *testing.T, newProvider StateFactory) {
	t.Run("create get list and delete", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		_, err := provider.GetSession(ctx, "tenant-a", "missing")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		_, err = provider.GetSessionType(ctx, "tenant-a", "missing")
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		createSessionFixture(t, provider, "tenant-a", "alpha", sessionsTypeTask, time.Now().UTC())
		createSessionFixture(t, provider, "tenant-b", "alpha", sessionsTypeChat, time.Now().UTC())
		require.ErrorIs(t, provider.CreateSession(ctx, &storagev1.SessionRecord{
			Namespace: "tenant-a", Name: "alpha", SessionType: sessionsTypeTask,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}), storagev1.ErrConflict, "a session name is unique within its namespace")

		got, err := provider.GetSession(ctx, "tenant-a", "alpha")
		require.NoError(t, err)
		require.Equal(t, "tenant-a", got.Namespace)
		require.Equal(t, "alpha", got.Name)
		require.Equal(t, sessionsTypeTask, got.SessionType)
		require.Zero(t, got.MessageCount)
		require.Empty(t, got.Messages)
		sessionType, err := provider.GetSessionType(ctx, "tenant-b", "alpha")
		require.NoError(t, err)
		require.Equal(t, sessionsTypeChat, sessionType)

		listed, err := provider.ListSessions(ctx, "tenant-a")
		require.NoError(t, err)
		require.Len(t, listed, 1)
		require.Equal(t, "alpha", listed[0].Name)

		require.NoError(t, provider.UpdateTokenCounts(ctx, "tenant-a", "alpha", 3, 4))
		require.NoError(t, provider.UpdateTokenCounts(ctx, "tenant-a", "alpha", 1, 2))
		got, err = provider.GetSession(ctx, "tenant-a", "alpha")
		require.NoError(t, err)
		require.Equal(t, 4, got.InputTokens)
		require.Equal(t, 6, got.OutputTokens)

		require.NoError(t, provider.DeleteSession(ctx, "tenant-a", "alpha", leaseNow))
		_, err = provider.GetSession(ctx, "tenant-a", "alpha")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		require.NoError(t, provider.DeleteSession(ctx, "tenant-a", "alpha", leaseNow), "deletion is idempotent")
		_, err = provider.GetSession(ctx, "tenant-b", "alpha")
		require.NoError(t, err, "other namespaces keep their session")
	})

	t.Run("pages newest first with a cursor and type filter", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		createSessionFixture(t, provider, "tenant-a", "oldest", sessionsTypeTask, base)
		createSessionFixture(t, provider, "tenant-a", "middle", sessionsTypeChat, base.Add(time.Minute))
		createSessionFixture(t, provider, "tenant-a", "newest-b", sessionsTypeTask, base.Add(2*time.Minute))
		createSessionFixture(t, provider, "tenant-a", "newest-a", sessionsTypeTask, base.Add(2*time.Minute))
		createSessionFixture(t, provider, "tenant-b", "elsewhere", sessionsTypeTask, base.Add(3*time.Minute))

		first, more, err := provider.ListSessionsPage(ctx, "tenant-a", nil, 2, "")
		require.NoError(t, err)
		require.True(t, more)
		require.Equal(t, []string{"newest-a", "newest-b"}, sessionNames(first))

		last := first[len(first)-1]
		second, more, err := provider.ListSessionsPage(ctx, "tenant-a",
			&storagev1.SessionPageCursor{CreatedAt: last.CreatedAt, Name: last.Name}, 2, "")
		require.NoError(t, err)
		require.False(t, more)
		require.Equal(t, []string{"middle", "oldest"}, sessionNames(second))

		filtered, more, err := provider.ListSessionsPage(ctx, "tenant-a", nil, 10, sessionsTypeChat)
		require.NoError(t, err)
		require.False(t, more)
		require.Equal(t, []string{"newest-a", "newest-b", "oldest"}, sessionNames(filtered))
	})

	t.Run("transcripts keep logical order and replay stable IDs", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		require.ErrorIs(t, provider.AppendMessages(ctx, "tenant-a", "missing", []storagev1.SessionMessage{
			{ID: "m1", Role: "user", Content: "lost"},
		}), storagev1.ErrNotFound)

		createSessionFixture(t, provider, "tenant-a", "transcript", sessionsTypeTask, time.Now().UTC())
		messages := []storagev1.SessionMessage{
			{ID: "m1", Role: "user", Content: "first question about rivers"},
			{ID: "m2", Role: "assistant", Content: "first answer", Metadata: map[string]string{"k": "v"}},
			{ID: "m3", Role: "user", Content: "second question about mountains"},
		}
		require.NoError(t, provider.AppendMessages(ctx, "tenant-a", "transcript", messages))
		require.NoError(t, provider.AppendMessages(ctx, "tenant-a", "transcript", messages[:2]), "stable IDs replay")
		require.ErrorIs(t, provider.AppendMessages(ctx, "tenant-a", "transcript", []storagev1.SessionMessage{
			{ID: "m1", Role: "user", Content: "rewritten"},
		}), storagev1.ErrDuplicateMismatch)

		session, err := provider.GetSession(ctx, "tenant-a", "transcript")
		require.NoError(t, err)
		require.Equal(t, 3, session.MessageCount, "a replay does not count twice")
		require.Equal(t, []string{"m1", "m2", "m3"}, sessionMessageIDs(session.Messages))
		require.Equal(t, map[string]string{"k": "v"}, session.Messages[1].Metadata)

		latest, err := provider.LoadTranscript(ctx, "tenant-a", "transcript", 2)
		require.NoError(t, err)
		require.Equal(t, []string{"m2", "m3"}, sessionMessageIDs(latest))
		through, err := provider.LoadTranscriptThrough(ctx, "tenant-a", "transcript", "m2", 0)
		require.NoError(t, err)
		require.Equal(t, []string{"m1", "m2"}, sessionMessageIDs(through))
		_, err = provider.LoadTranscriptThrough(ctx, "tenant-a", "transcript", "missing", 0)
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		createSessionFixture(t, provider, "tenant-b", "transcript", sessionsTypeTask, time.Now().UTC())
		require.NoError(t, provider.AppendMessages(ctx, "tenant-b", "transcript", []storagev1.SessionMessage{
			{ID: "m1", Role: "user", Content: "rivers in another namespace"},
		}))

		hits, err := provider.SearchTranscript(ctx, storagev1.TranscriptSearchFilter{Namespace: "tenant-a", Query: "rivers"})
		require.NoError(t, err)
		require.Len(t, hits, 1)
		require.Equal(t, "transcript", hits[0].SessionName)
		require.Equal(t, "m1", hits[0].StableMessageID)
		require.Contains(t, hits[0].Snippet, "rivers")

		byRole, err := provider.SearchTranscript(ctx, storagev1.TranscriptSearchFilter{
			Namespace: "tenant-a", Query: "question", Roles: []string{"assistant"},
		})
		require.NoError(t, err)
		require.Empty(t, byRole)
		bounded, err := provider.SearchTranscript(ctx, storagev1.TranscriptSearchFilter{
			Namespace: "tenant-a", Query: "question",
			HistoryBounds: []storagev1.TranscriptSearchHistoryBound{{SessionName: "transcript", ThroughMessageID: "m2"}},
		})
		require.NoError(t, err)
		require.Len(t, bounded, 1)
		require.Equal(t, "m1", bounded[0].StableMessageID)

		_, err = provider.SearchTranscript(ctx, storagev1.TranscriptSearchFilter{Query: "rivers"})
		require.ErrorIs(t, err, storagev1.ErrValidation)
	})
}

func runSessionLocks(t *testing.T, newProvider StateFactory) {
	t.Run("exact Task incarnations own locks", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		require.ErrorIs(t, provider.AcquireLock(ctx, "tenant-a", "missing", "task-a", "uid-a", leaseNow), storagev1.ErrNotFound)
		createSessionFixture(t, provider, "tenant-a", "locked", sessionsTypeTask, time.Now().UTC())

		require.NoError(t, provider.AcquireLock(ctx, "tenant-a", "locked", "task-a", "uid-a", leaseNow))
		require.NoError(t, provider.AcquireLock(ctx, "tenant-a", "locked", "task-a", "uid-a", leaseNow), "the owner may reacquire")
		require.ErrorIs(t, provider.AcquireLock(ctx, "tenant-a", "locked", "task-b", "uid-b", leaseNow), storagev1.ErrConflict)
		require.ErrorIs(t, provider.AcquireLock(ctx, "tenant-a", "locked", "task-a", "uid-other", leaseNow), storagev1.ErrConflict,
			"a recreated Task with the same name is a different owner")

		locked, err := provider.IsLocked(ctx, "tenant-a", "locked", "task-a", "uid-a", leaseNow)
		require.NoError(t, err)
		require.False(t, locked, "the owner is not locked out")
		locked, err = provider.IsLocked(ctx, "tenant-a", "locked", "task-b", "uid-b", leaseNow)
		require.NoError(t, err)
		require.True(t, locked)
		_, err = provider.IsLocked(ctx, "tenant-a", "missing", "task-a", "uid-a", leaseNow)
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		require.ErrorIs(t, provider.DeleteSession(ctx, "tenant-a", "locked", leaseNow), storagev1.ErrConflict,
			"a locked session is not deleted")

		require.NoError(t, provider.ReleaseLock(ctx, "tenant-a", "locked", "task-a", "uid-other"))
		locked, err = provider.IsLocked(ctx, "tenant-a", "locked", "task-b", "uid-b", leaseNow)
		require.NoError(t, err)
		require.True(t, locked, "release by another incarnation is ignored")

		require.NoError(t, provider.ReleaseLock(ctx, "tenant-a", "locked", "task-a", "uid-a"))
		locked, err = provider.IsLocked(ctx, "tenant-a", "locked", "task-b", "uid-b", leaseNow)
		require.NoError(t, err)
		require.False(t, locked)
		require.NoError(t, provider.AcquireLock(ctx, "tenant-a", "locked", "task-b", "uid-b", leaseNow))
	})

	t.Run("fenced writes require the current owner", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		createSessionFixture(t, provider, "tenant-a", "fenced", sessionsTypeTask, time.Now().UTC())
		require.NoError(t, provider.AcquireLock(ctx, "tenant-a", "fenced", "task-a", "uid-a", leaseNow))

		require.NoError(t, provider.AppendMessagesWithLock(ctx, lockOwner("tenant-a", "fenced", "task-a", "uid-a"),
			[]storagev1.SessionMessage{{ID: "owned", Role: "user", Content: "from owner"}}))
		requireFenced(t, provider.AppendMessagesWithLock(ctx, lockOwner("tenant-a", "fenced", "task-a", "uid-stale"),
			[]storagev1.SessionMessage{{ID: "stale", Role: "user", Content: "from stale owner"}}))
		require.NoError(t, provider.UpdateTokenCountsWithLock(ctx, lockOwner("tenant-a", "fenced", "task-a", "uid-a"), 2, 3))
		requireFenced(t, provider.UpdateTokenCountsWithLock(ctx, lockOwner("tenant-a", "fenced", "task-b", "uid-b"), 50, 50))

		require.NoError(t, provider.ReleaseLockWithTokenUsage(ctx, lockOwner("tenant-a", "fenced", "task-a", "uid-a"), 5, 7))
		require.NoError(t, provider.ReleaseLockWithTokenUsage(ctx, lockOwner("tenant-a", "fenced", "task-a", "uid-a"), 5, 7),
			"a replayed completion succeeds without counting again")

		session, err := provider.GetSession(ctx, "tenant-a", "fenced")
		require.NoError(t, err)
		require.Equal(t, []string{"owned"}, sessionMessageIDs(session.Messages))
		require.Equal(t, 7, session.InputTokens)
		require.Equal(t, 10, session.OutputTokens)
		require.Empty(t, session.ActiveTask)
		require.Empty(t, session.ActiveTaskUID)
	})
}

func runChatTurns(t *testing.T, newProvider StateFactory) {
	t.Run("one reservation commits against the observed revision", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		session := &storagev1.SessionRecord{Namespace: "tenant-a", Name: "chat", SessionType: sessionsTypeChat}
		reserve := func(turnID string) storagev1.AcquireChatTurnRequest {
			return storagev1.AcquireChatTurnRequest{Session: session, TurnID: turnID, Now: leaseNow, ExpiresAt: leaseExpiry}
		}

		past := reserve("turn-0")
		past.ExpiresAt = leaseNow
		_, err := provider.AcquireChatTurn(ctx, past)
		require.ErrorIs(t, err, storagev1.ErrValidation, "a reservation must expire after its request time")

		created, err := provider.AcquireChatTurn(ctx, reserve("turn-1"))
		require.NoError(t, err)
		require.True(t, created)
		_, err = provider.AcquireChatTurn(ctx, reserve("turn-2"))
		require.ErrorIs(t, err, storagev1.ErrConflict, "one live turn at a time")
		require.ErrorIs(t, provider.AcquireLock(ctx, "tenant-a", "chat", "task-a", "uid-a", leaseNow), storagev1.ErrConflict,
			"a live chat turn excludes Task locks")
		locked, err := provider.IsLocked(ctx, "tenant-a", "chat", "task-a", "uid-a", leaseNow)
		require.NoError(t, err)
		require.True(t, locked)

		turn := []storagev1.SessionMessage{
			{ID: "chat-1", Role: "user", Content: "hello"},
			{ID: "chat-2", Role: "assistant", Content: "hi"},
		}
		require.ErrorIs(t, provider.CommitSessionTurn(ctx, turnCommit(session, "turn-2", 0, turn, 1, 1)), storagev1.ErrConflict,
			"only the reservation owner commits")
		require.ErrorIs(t, provider.CommitSessionTurn(ctx, turnCommit(session, "turn-1", 5, turn, 1, 1)), storagev1.ErrConflict,
			"the expected message count fences the commit")
		require.NoError(t, provider.CommitSessionTurn(ctx, turnCommit(session, "turn-1", 0, turn, 11, 13)))

		got, err := provider.GetSession(ctx, "tenant-a", "chat")
		require.NoError(t, err)
		require.Equal(t, 2, got.MessageCount)
		require.Equal(t, 11, got.InputTokens)
		require.Equal(t, 13, got.OutputTokens)
		require.Equal(t, []string{"chat-1", "chat-2"}, sessionMessageIDs(got.Messages))

		require.NoError(t, provider.ReleaseChatTurn(ctx, "tenant-a", "chat", "turn-1", true))
		_, err = provider.GetSession(ctx, "tenant-a", "chat")
		require.NoError(t, err, "a session with history is not removed on release")
		created, err = provider.AcquireChatTurn(ctx, reserve("turn-3"))
		require.NoError(t, err)
		require.False(t, created)
		require.ErrorIs(t, provider.CommitSessionTurn(ctx, turnCommit(session, "turn-3", 0,
			[]storagev1.SessionMessage{{ID: "chat-3", Role: "user", Content: "stale"}}, 0, 0)), storagev1.ErrConflict,
			"a stale revision cannot commit")
		require.NoError(t, provider.CommitSessionTurn(ctx, turnCommit(session, "turn-3", 2,
			[]storagev1.SessionMessage{{ID: "chat-3", Role: "user", Content: "again"}}, 0, 0)))
	})

	t.Run("an empty created session is removed when its turn fails", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		session := &storagev1.SessionRecord{Namespace: "tenant-a", Name: "abandoned", SessionType: sessionsTypeChat}
		created, err := provider.AcquireChatTurn(ctx, storagev1.AcquireChatTurnRequest{
			Session: session, TurnID: "turn-1", Now: leaseNow, ExpiresAt: leaseExpiry,
		})
		require.NoError(t, err)
		require.True(t, created)
		require.NoError(t, provider.ReleaseChatTurn(ctx, "tenant-a", "abandoned", "turn-other", true),
			"another turn cannot release or delete the reservation")
		_, err = provider.GetSession(ctx, "tenant-a", "abandoned")
		require.NoError(t, err)

		require.NoError(t, provider.ReleaseChatTurn(ctx, "tenant-a", "abandoned", "turn-1", true))
		_, err = provider.GetSession(ctx, "tenant-a", "abandoned")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
	})
}

// runLeaseClocks checks that a provider decides chat turn reservations and
// Session locks on the time each request carries, never on its own clock. The
// request times are months from the wall clock, so a provider that consults
// its own clock fails these cases.
func runLeaseClocks(t *testing.T, newProvider StateFactory) {
	t.Run("a chat turn reservation lapses at its expiry on the request clock", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		session := &storagev1.SessionRecord{Namespace: "tenant-a", Name: "chat", SessionType: sessionsTypeChat}
		created, err := provider.AcquireChatTurn(ctx, storagev1.AcquireChatTurnRequest{
			Session: session, TurnID: "turn-1", Now: leaseNow, ExpiresAt: leaseExpiry,
		})
		require.NoError(t, err)
		require.True(t, created)

		justBefore := leaseExpiry.Add(-time.Nanosecond)
		_, err = provider.AcquireChatTurn(ctx, storagev1.AcquireChatTurnRequest{
			Session: session, TurnID: "turn-2", Now: justBefore, ExpiresAt: justBefore.Add(time.Hour),
		})
		require.ErrorIs(t, err, storagev1.ErrConflict, "the reservation holds until its expiry")
		locked, err := provider.IsLocked(ctx, "tenant-a", "chat", "task-a", "uid-a", justBefore)
		require.NoError(t, err)
		require.True(t, locked)
		require.ErrorIs(t, provider.AcquireLock(ctx, "tenant-a", "chat", "task-a", "uid-a", justBefore), storagev1.ErrConflict)
		require.ErrorIs(t, provider.DeleteSession(ctx, "tenant-a", "chat", justBefore), storagev1.ErrConflict)

		turn := []storagev1.SessionMessage{{ID: "late", Role: "user", Content: "too late"}}
		late := turnCommit(session, "turn-1", 0, turn, 1, 1)
		late.Now = leaseExpiry
		require.ErrorIs(t, provider.CommitSessionTurn(ctx, late), storagev1.ErrConflict,
			"a commit at or after the expiry is refused")
		inTime := turnCommit(session, "turn-1", 0, turn, 1, 1)
		inTime.Now = justBefore
		require.NoError(t, provider.CommitSessionTurn(ctx, inTime), "a commit before the expiry succeeds")

		locked, err = provider.IsLocked(ctx, "tenant-a", "chat", "task-a", "uid-a", leaseExpiry)
		require.NoError(t, err)
		require.False(t, locked, "an expired reservation does not lock the Session")
		created, err = provider.AcquireChatTurn(ctx, storagev1.AcquireChatTurnRequest{
			Session: session, TurnID: "turn-2", Now: leaseExpiry, ExpiresAt: leaseExpiry.Add(time.Hour),
		})
		require.NoError(t, err, "an expired reservation is reclaimable")
		require.False(t, created)

		require.NoError(t, provider.AcquireLock(ctx, "tenant-a", "chat", "task-a", "uid-a", leaseExpiry.Add(time.Hour)),
			"a Task lock reclaims the second reservation once it has expired")
		locked, err = provider.IsLocked(ctx, "tenant-a", "chat", "task-b", "uid-b", leaseExpiry.Add(time.Hour))
		require.NoError(t, err)
		require.True(t, locked)
	})

	t.Run("an expired reservation does not keep a Session from deletion", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		session := &storagev1.SessionRecord{Namespace: "tenant-a", Name: "chat", SessionType: sessionsTypeChat}
		_, err := provider.AcquireChatTurn(ctx, storagev1.AcquireChatTurnRequest{
			Session: session, TurnID: "turn-1", Now: leaseNow, ExpiresAt: leaseExpiry,
		})
		require.NoError(t, err)
		require.NoError(t, provider.CommitSessionTurn(ctx, turnCommit(session, "turn-1", 0,
			[]storagev1.SessionMessage{{ID: "kept", Role: "user", Content: "hello"}}, 0, 0)))
		require.ErrorIs(t, provider.DeleteSession(ctx, "tenant-a", "chat", leaseNow), storagev1.ErrConflict)
		require.NoError(t, provider.DeleteSession(ctx, "tenant-a", "chat", leaseExpiry))
		_, err = provider.GetSession(ctx, "tenant-a", "chat")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
	})

	t.Run("lease decisions require the request time", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		createSessionFixture(t, provider, "tenant-a", "session", sessionsTypeTask, writeCaseCreated)
		var zero time.Time
		owner := storagev1.SessionOwnerWrite{Namespace: "tenant-a", Name: "session", OwnerName: "task-a", OwnerUID: "uid-a"}

		require.ErrorIs(t, provider.AcquireLock(ctx, "tenant-a", "session", "task-a", "uid-a", zero), storagev1.ErrValidation)
		_, err := provider.IsLocked(ctx, "tenant-a", "session", "task-a", "uid-a", zero)
		require.ErrorIs(t, err, storagev1.ErrValidation)
		require.NoError(t, provider.AcquireLock(ctx, "tenant-a", "session", "task-a", "uid-a", leaseNow))
		require.ErrorIs(t, provider.AppendMessagesWithLock(ctx, owner,
			[]storagev1.SessionMessage{{ID: "m1", Role: "user", Content: "hello"}}), storagev1.ErrValidation)
		require.ErrorIs(t, provider.UpdateTokenCountsWithLock(ctx, owner, 1, 1), storagev1.ErrValidation)
		require.ErrorIs(t, provider.ReleaseLockWithTokenUsage(ctx, owner, 1, 1), storagev1.ErrValidation)
		require.ErrorIs(t, provider.EnsureSessionSoulWithLock(ctx, owner, ""), storagev1.ErrValidation)
		require.ErrorIs(t, provider.DeleteSession(ctx, "tenant-a", "session", zero), storagev1.ErrValidation)
		_, err = provider.AcquireChatTurn(ctx, storagev1.AcquireChatTurnRequest{
			Session:   &storagev1.SessionRecord{Namespace: "tenant-a", Name: "chat", SessionType: sessionsTypeChat},
			TurnID:    "turn-1",
			ExpiresAt: leaseExpiry,
		})
		require.ErrorIs(t, err, storagev1.ErrValidation)
		require.ErrorIs(t, provider.CommitSessionTurn(ctx, storagev1.CommitSessionTurnRequest{
			Session: &storagev1.SessionRecord{Namespace: "tenant-a", Name: "chat"}, TurnID: "turn-1",
		}), storagev1.ErrValidation)

		session, err := provider.GetSession(ctx, "tenant-a", "session")
		require.NoError(t, err)
		require.Equal(t, "task-a", session.ActiveTask, "the refused writes changed nothing")
		require.Zero(t, session.MessageCount)
		require.Zero(t, session.InputTokens)
	})
}

func runSessionSoul(t *testing.T, newProvider StateFactory) {
	provider := newProvider(t)
	ctx := t.Context()
	digest := storagev1.CanonicalBytesDigest([]byte("soul revision"))
	createSessionFixture(t, provider, "tenant-a", "soul", sessionsTypeTask, time.Now().UTC())

	require.ErrorIs(t, provider.EnsureSessionSoulWithLock(ctx, lockOwner("tenant-a", "soul", "task-a", "uid-a"), digest),
		storagev1.ErrConflict, "pinning requires the Task lock")
	require.NoError(t, provider.AcquireLock(ctx, "tenant-a", "soul", "task-a", "uid-a", leaseNow))

	state, err := provider.ReadSessionSoul(ctx, "tenant-a", "soul", "task-a", "uid-a")
	require.NoError(t, err)
	require.False(t, state.Established)

	require.NoError(t, provider.EnsureSessionSoulWithLock(ctx, lockOwner("tenant-a", "soul", "task-a", "uid-a"), digest))
	require.NoError(t, provider.EnsureSessionSoulWithLock(ctx, lockOwner("tenant-a", "soul", "task-a", "uid-a"), digest),
		"pinning the same revision is idempotent")
	require.ErrorIs(t, provider.EnsureSessionSoulWithLock(ctx, lockOwner("tenant-a", "soul", "task-a", "uid-a"),
		storagev1.CanonicalBytesDigest([]byte("other revision"))), storagev1.ErrSessionConfigurationMismatch)

	state, err = provider.ReadSessionSoul(ctx, "tenant-a", "soul", "task-a", "uid-a")
	require.NoError(t, err)
	require.True(t, state.Established)
	require.Equal(t, digest, state.Digest)
	require.Equal(t, sessionsTypeTask, state.SessionType)
	require.Zero(t, state.MessageCount, "the pin is not a visible message")

	_, err = provider.ReadSessionSoul(ctx, "tenant-a", "soul", "task-b", "uid-b")
	require.ErrorIs(t, err, storagev1.ErrConflict, "only the lock owner reads the prompt context")
	_, err = provider.ReadSessionSoul(ctx, "tenant-a", "missing", "task-a", "uid-a")
	require.ErrorIs(t, err, storagev1.ErrNotFound)

	transcript, err := provider.LoadTranscript(ctx, "tenant-a", "soul", 0)
	require.NoError(t, err)
	require.Empty(t, transcript, "the pin is hidden from transcript readers")
}

func createSessionFixture(
	t *testing.T,
	provider storagev1.StateProvider,
	namespace, name, sessionType string,
	createdAt time.Time,
) {
	t.Helper()
	require.NoError(t, provider.CreateSession(t.Context(), &storagev1.SessionRecord{
		Namespace: namespace, Name: name, SessionType: sessionType, CreatedAt: createdAt, UpdatedAt: createdAt,
	}), fmt.Sprintf("create session %s/%s", namespace, name))
}

func messageContents(messages []storagev1.Message) []string {
	contents := make([]string, 0, len(messages))
	for _, message := range messages {
		contents = append(contents, message.Content)
	}
	return contents
}

func sessionNames(sessions []storagev1.SessionMetadata) []string {
	names := make([]string, 0, len(sessions))
	for _, session := range sessions {
		names = append(names, session.Name)
	}
	return names
}

func sessionMessageIDs(messages []storagev1.SessionMessage) []string {
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ID)
	}
	return ids
}
