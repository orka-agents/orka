package storagetest

import (
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// leaseNow is the request time of the suite's lease decisions. It is far from
// the wall clock, so a provider that judges a lease on its own clock instead
// of the request's fails the lease cases.
var leaseNow = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

// leaseExpiry is a lease expiry an hour after leaseNow.
var leaseExpiry = leaseNow.Add(time.Hour)

var writeCaseCreated = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

// lockOwner names a Session lock owner at leaseNow.
func lockOwner(namespace, name, ownerName, ownerUID string) storagev1.SessionOwnerWrite {
	return storagev1.SessionOwnerWrite{Namespace: namespace, Name: name, OwnerName: ownerName, OwnerUID: ownerUID, Now: leaseNow}
}

// chatTurn reserves the case chat Session for turnID at leaseNow until
// leaseExpiry.
func chatTurn(turnID string) storagev1.AcquireChatTurnRequest {
	return storagev1.AcquireChatTurnRequest{Session: chatSession(), TurnID: turnID, Now: leaseNow, ExpiresAt: leaseExpiry}
}

// turnCommit commits messages and tokens for turnID at leaseNow.
func turnCommit(
	session *storagev1.SessionRecord, turnID string, expected int, messages []storagev1.SessionMessage, input, output int,
) storagev1.CommitSessionTurnRequest {
	return storagev1.CommitSessionTurnRequest{
		Session: session, TurnID: turnID, ExpectedMessageCount: expected, Messages: messages,
		InputTokens: input, OutputTokens: output, Now: leaseNow,
	}
}

func observeSession(name string) func(t *testing.T, p storagev1.StateProvider) any {
	return func(t *testing.T, p storagev1.StateProvider) any {
		t.Helper()
		session, err := p.GetSession(t.Context(), "tenant", name)
		listed, listErr := p.ListSessions(t.Context(), "tenant")
		require.NoError(t, listErr)
		// IsLocked changes nothing while no lock has expired.
		locked, lockErr := p.IsLocked(t.Context(), "tenant", name, "observer", "observer-uid", leaseNow)
		return []any{observed(t, session, err), listed, observed(t, locked, lockErr)}
	}
}

func createTaskSession(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	createSessionFixture(t, p, "tenant", "session", sessionsTypeTask, writeCaseCreated)
}

func lockedTenantSession(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	createSessionFixture(t, p, "tenant", "session", sessionsTypeTask, writeCaseCreated)
	require.NoError(t, p.AcquireLock(t.Context(), "tenant", "session", "task-a", "uid-a", leaseNow))
}

func chatSession() *storagev1.SessionRecord {
	return &storagev1.SessionRecord{Namespace: "tenant", Name: "chat", SessionType: sessionsTypeChat}
}

func reservedChat(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	created, err := p.AcquireChatTurn(t.Context(), chatTurn("turn-1"))
	require.NoError(t, err)
	require.True(t, created)
}

func observeSoul(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	soul, err := p.ReadSessionSoul(t.Context(), "tenant", "session", "task-a", "uid-a")
	return observed(t, soul, err)
}

func observeLineage(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	lineage, err := p.GetSessionLineage(t.Context(), "tenant", "lineage")
	return observed(t, lineage, err)
}

func init() {
	session := observeSession("session")
	stateWriteCases = append(stateWriteCases,
		writeCase[storagev1.StateProvider]{
			method: "CreateSession",
			args: func(storagev1.ControllerEpochFence) []any {
				return []any{&storagev1.SessionRecord{
					Namespace: "tenant", Name: "session", SessionType: sessionsTypeTask,
					CreatedAt: writeCaseCreated, UpdatedAt: writeCaseCreated,
				}}
			},
			observe: session,
		},
		writeCase[storagev1.StateProvider]{
			method:      "DeleteSession",
			setup:       createTaskSession,
			args:        fixed("tenant", "session", leaseNow),
			observe:     session,
			afterCommit: createTaskSession,
		},
		writeCase[storagev1.StateProvider]{
			method:  "AcquireLock",
			setup:   createTaskSession,
			args:    fixed("tenant", "session", "task-a", "uid-a", leaseNow),
			observe: session,
		},
		writeCase[storagev1.StateProvider]{
			method:  "ReleaseLock",
			setup:   lockedTenantSession,
			args:    fixed("tenant", "session", "task-a", "uid-a"),
			observe: session,
			afterCommit: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.AcquireLock(t.Context(), "tenant", "session", "task-a", "uid-a", leaseNow))
			},
		},
		writeCase[storagev1.StateProvider]{
			method:  "IsLocked",
			setup:   lockedTenantSession,
			args:    fixed("tenant", "session", "task-b", "uid-b", leaseNow),
			observe: session,
		},
		writeCase[storagev1.StateProvider]{
			method:  "AcquireChatTurn",
			args:    func(storagev1.ControllerEpochFence) []any { return []any{chatTurn("turn-1")} },
			observe: observeSession("chat"),
		},
		writeCase[storagev1.StateProvider]{
			method:  "ReleaseChatTurn",
			setup:   reservedChat,
			args:    fixed("tenant", "chat", "turn-1", true),
			observe: observeSession("chat"),
		},
		writeCase[storagev1.StateProvider]{
			method: "CommitSessionTurn",
			setup:  reservedChat,
			args: func(storagev1.ControllerEpochFence) []any {
				return []any{turnCommit(chatSession(), "turn-1", 0, []storagev1.SessionMessage{
					{ID: "chat-1", Role: "user", Content: "hello"},
					{ID: "chat-2", Role: "assistant", Content: "hi"},
				}, 11, 13)}
			},
			observe: observeSession("chat"),
		},
		writeCase[storagev1.StateProvider]{
			method: "AppendMessages",
			setup:  createTaskSession,
			args: func(storagev1.ControllerEpochFence) []any {
				return []any{"tenant", "session", []storagev1.SessionMessage{{ID: "m1", Role: "user", Content: "hello"}}}
			},
			observe: session,
		},
		writeCase[storagev1.StateProvider]{
			method: "AppendMessagesWithLock",
			setup:  lockedTenantSession,
			args: func(storagev1.ControllerEpochFence) []any {
				return []any{lockOwner("tenant", "session", "task-a", "uid-a"), []storagev1.SessionMessage{{ID: "m1", Role: "user", Content: "hello"}}}
			},
			observe: session,
		},
		writeCase[storagev1.StateProvider]{
			method:  "UpdateTokenCounts",
			setup:   createTaskSession,
			args:    fixed("tenant", "session", 3, 4),
			observe: session,
		},
		writeCase[storagev1.StateProvider]{
			method:  "UpdateTokenCountsWithLock",
			setup:   lockedTenantSession,
			args:    fixed(lockOwner("tenant", "session", "task-a", "uid-a"), 3, 4),
			observe: session,
		},
		writeCase[storagev1.StateProvider]{
			method:  "ReleaseLockWithTokenUsage",
			setup:   lockedTenantSession,
			args:    fixed(lockOwner("tenant", "session", "task-a", "uid-a"), 3, 4),
			observe: session,
		},
		writeCase[storagev1.StateProvider]{
			method:  "EnsureSessionSoulWithLock",
			setup:   lockedTenantSession,
			args:    fixed(lockOwner("tenant", "session", "task-a", "uid-a"), storagev1.CanonicalBytesDigest([]byte("soul revision"))),
			observe: observeSoul,
		},
		writeCase[storagev1.StateProvider]{
			method: "ProjectSessionLineage",
			args: func(storagev1.ControllerEpochFence) []any {
				return []any{sessionLineage("tenant", "lineage", "lineage-uid")}
			},
			observe: observeLineage,
		},
	)
}
