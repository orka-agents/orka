package storagetest

import (
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// writeCaseFuture is a lease expiry that stays in the future while the suite
// runs. It is fixed, so every attempt of a write sends the same request.
var writeCaseFuture = time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)

var writeCaseCreated = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func observeSession(name string) func(t *testing.T, p storagev1.StateProvider) any {
	return func(t *testing.T, p storagev1.StateProvider) any {
		t.Helper()
		session, err := p.GetSession(t.Context(), "tenant", name)
		listed, listErr := p.ListSessions(t.Context(), "tenant")
		require.NoError(t, listErr)
		// IsLocked changes nothing while no lock has expired.
		locked, lockErr := p.IsLocked(t.Context(), "tenant", name, "observer", "observer-uid")
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
	require.NoError(t, p.AcquireLock(t.Context(), "tenant", "session", "task-a", "uid-a"))
}

func chatSession() *storagev1.SessionRecord {
	return &storagev1.SessionRecord{Namespace: "tenant", Name: "chat", SessionType: sessionsTypeChat}
}

func reservedChat(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	created, err := p.AcquireChatTurn(t.Context(), chatSession(), "turn-1", writeCaseFuture)
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
			args:        fixed("tenant", "session"),
			observe:     session,
			afterCommit: createTaskSession,
		},
		writeCase[storagev1.StateProvider]{
			method:  "AcquireLock",
			setup:   createTaskSession,
			args:    fixed("tenant", "session", "task-a", "uid-a"),
			observe: session,
		},
		writeCase[storagev1.StateProvider]{
			method:  "ReleaseLock",
			setup:   lockedTenantSession,
			args:    fixed("tenant", "session", "task-a", "uid-a"),
			observe: session,
			afterCommit: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.AcquireLock(t.Context(), "tenant", "session", "task-a", "uid-a"))
			},
		},
		writeCase[storagev1.StateProvider]{
			method:  "IsLocked",
			setup:   lockedTenantSession,
			args:    fixed("tenant", "session", "task-b", "uid-b"),
			observe: session,
		},
		writeCase[storagev1.StateProvider]{
			method:  "AcquireChatTurn",
			args:    func(storagev1.ControllerEpochFence) []any { return []any{chatSession(), "turn-1", writeCaseFuture} },
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
				return []any{chatSession(), "turn-1", 0, []storagev1.SessionMessage{
					{ID: "chat-1", Role: "user", Content: "hello"},
					{ID: "chat-2", Role: "assistant", Content: "hi"},
				}, 11, 13}
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
				return []any{"tenant", "session", "task-a", "uid-a", []storagev1.SessionMessage{{ID: "m1", Role: "user", Content: "hello"}}}
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
			args:    fixed("tenant", "session", "task-a", "uid-a", 3, 4),
			observe: session,
		},
		writeCase[storagev1.StateProvider]{
			method:  "ReleaseLockWithTokenUsage",
			setup:   lockedTenantSession,
			args:    fixed("tenant", "session", "task-a", "uid-a", 3, 4),
			observe: session,
		},
		writeCase[storagev1.StateProvider]{
			method:  "EnsureSessionSoulWithLock",
			setup:   lockedTenantSession,
			args:    fixed("tenant", "session", "task-a", "uid-a", storagev1.CanonicalBytesDigest([]byte("soul revision"))),
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
