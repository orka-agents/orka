package sqlite

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
	storetest "github.com/orka-agents/orka/internal/store/storetest"
	"github.com/stretchr/testify/require"
)

func nativeTestStore(t *testing.T, path string) *Store {
	t.Helper()
	db, err := NewDB(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	s := NewStore(db, path)
	cipher, err := NewAgentExecutionSnapshotCipher(bytes.Repeat([]byte{7}, 32))
	require.NoError(t, err)
	require.NoError(t, s.SetAgentExecutionSnapshotCipher(cipher))
	return s
}

func nativeImportFixture(t *testing.T, name, text string) store.NativeSessionImport {
	t.Helper()
	snapshot := storetest.NativeSessionSnapshot(t, text)
	snapshot.RuntimeSessionUID = ""
	snapshot.RuntimeProfileDigest = ""
	snapshot.WorkingDirectory = ""
	return store.NativeSessionImport{Namespace: "tenant", SessionName: name, OperationID: "import-operation", RequestDigest: store.NativeSessionImportDigest("tenant", name, snapshot.DataDigest), Snapshot: snapshot}
}

func TestNativeSessionImportOwnershipAndRetry(t *testing.T) {
	s := nativeTestStore(t, ":memory:")
	ctx := t.Context()
	request := nativeImportFixture(t, "fresh", "private original history")
	receipt, err := s.StageNativeSessionImport(ctx, request)
	require.NoError(t, err)
	withoutKey := NewStore(s.db, ":memory:")
	_, err = withoutKey.GetNativeSession(ctx, "tenant", "fresh", "")
	require.ErrorIs(t, err, errSnapshotCipherRequired)
	retry, err := s.StageNativeSessionImport(ctx, request)
	require.NoError(t, err)
	require.Equal(t, receipt, retry)
	changed := nativeImportFixture(t, "fresh", "different history")
	_, err = s.StageNativeSessionImport(ctx, changed)
	require.ErrorIs(t, err, store.ErrDuplicateMismatch)
	changed = request
	changed.OperationID = "another-operation"
	_, err = s.StageNativeSessionImport(ctx, changed)
	require.ErrorIs(t, err, store.ErrConflict)
	pending, err := s.GetNativeSession(ctx, "tenant", "fresh", "")
	require.NoError(t, err)
	require.Empty(t, pending.SessionUID)
	_, err = s.GetNativeSession(ctx, "tenant", "fresh", "provider-uuid")
	require.ErrorIs(t, err, store.ErrConflict)
	require.NoError(t, s.BindSessionCleanupIdentity(ctx, "tenant", "fresh", "canonical-session-uid"))
	bound, err := s.GetNativeSession(ctx, "tenant", "fresh", "canonical-session-uid")
	require.NoError(t, err)
	require.Equal(t, "canonical-session-uid", bound.SessionUID)
	require.Empty(t, bound.Snapshot.RuntimeSessionUID)
	_, err = s.GetNativeSession(ctx, "tenant", "fresh", "")
	require.ErrorIs(t, err, store.ErrConflict)
	require.NoError(t, s.CreateSession(ctx, &store.SessionRecord{Namespace: "tenant", Name: "empty-existing", SessionType: "task"}))
	other := nativeImportFixture(t, "empty-existing", "existing")
	_, err = s.StageNativeSessionImport(ctx, other)
	require.ErrorIs(t, err, store.ErrConflict)
	require.NoError(t, s.CreateSession(ctx, &store.SessionRecord{Namespace: "tenant", Name: "gateway", SessionType: store.SessionTypeGateway}))
	other = nativeImportFixture(t, "gateway", "private gateway")
	_, err = s.StageNativeSessionImport(ctx, other)
	require.ErrorIs(t, err, store.ErrGatewayOwnedSession)
}

func TestNativeSessionImportAndCaptureOperationIDsAreIndependent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operation-kinds.db")
	s := nativeTestStore(t, path)
	ctx := t.Context()
	request := nativeImportFixture(t, "shared-operation", "imported native history")
	request.OperationID = "capture-native-g1-prompt-a"
	receipt, err := s.StageNativeSessionImport(ctx, request)
	require.NoError(t, err)
	require.NoError(t, s.BindSessionCleanupIdentity(ctx, request.Namespace, request.SessionName, "canonical-uid"))
	require.NoError(t, s.AppendMessages(ctx, request.Namespace, request.SessionName, []store.SessionMessage{
		{ID: "new-result", Role: "assistant", Content: "continued result"},
	}))
	capture := store.NativeSessionRecord{
		Namespace: request.Namespace, SessionName: request.SessionName, SessionUID: "canonical-uid",
		Snapshot:     storetest.NativeSessionSnapshot(t, "continued native history"),
		MessageCount: 1, ThroughMessageID: "new-result", RuntimeSessionGeneration: 1,
		SourceOperationID: request.OperationID,
	}
	require.NoError(t, s.SaveNativeSession(ctx, capture))
	require.NoError(t, s.db.Close())
	s = nativeTestStore(t, path)
	require.NoError(t, s.SaveNativeSession(ctx, capture), "capture retry must use its own immutable receipt")
	retry, err := s.StageNativeSessionImport(ctx, request)
	require.NoError(t, err)
	require.Equal(t, receipt, retry, "import retry must retain its original receipt after capture")
	changedImport := nativeImportFixture(t, request.SessionName, "different imported history")
	changedImport.OperationID = request.OperationID
	_, err = s.StageNativeSessionImport(ctx, changedImport)
	require.ErrorIs(t, err, store.ErrDuplicateMismatch)
	changedCapture := capture
	changedCapture.Snapshot = storetest.NativeSessionSnapshot(t, "different continued history")
	require.ErrorIs(t, s.SaveNativeSession(ctx, changedCapture), store.ErrDuplicateMismatch)
	checkpoint, err := s.GetNativeSession(ctx, request.Namespace, request.SessionName, capture.SessionUID)
	require.NoError(t, err)
	require.Equal(t, capture.Snapshot.Data, checkpoint.Snapshot.Data)
}

func TestNativeSessionRejectsChatTurnWithoutChangingCheckpoint(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "staged import", true: "bound native Session"}[bound], func(t *testing.T) {
			s := nativeTestStore(t, ":memory:")
			request := nativeImportFixture(t, "imported", "private source history")
			_, err := s.StageNativeSessionImport(t.Context(), request)
			require.NoError(t, err)
			uid := ""
			if bound {
				uid = "canonical-uid"
				require.NoError(t, s.BindSessionCleanupIdentity(t.Context(), request.Namespace, request.SessionName, uid))
			}
			before, err := s.GetNativeSession(t.Context(), request.Namespace, request.SessionName, uid)
			require.NoError(t, err)
			created, err := s.AcquireChatTurn(t.Context(), &store.SessionRecord{
				Namespace: request.Namespace, Name: request.SessionName, SessionType: store.SessionTypeChat,
			}, "unrelated-chat", time.Now().Add(time.Minute))
			require.False(t, created)
			require.ErrorIs(t, err, store.ErrConflict)
			after, err := s.GetNativeSession(t.Context(), request.Namespace, request.SessionName, uid)
			require.NoError(t, err)
			require.Equal(t, before, after)
			session, err := s.GetSession(t.Context(), request.Namespace, request.SessionName)
			require.NoError(t, err)
			require.Zero(t, session.MessageCount)
			require.Empty(t, session.Messages)
			// A rejected chat must leave no lease blocking the first real Task.
			require.NoError(t, s.AcquireLock(t.Context(), request.Namespace, request.SessionName, "agent-task", "agent-task-uid"))
			require.NoError(t, s.ReleaseLock(t.Context(), request.Namespace, request.SessionName, "agent-task", "agent-task-uid"))
		})
	}
}

func TestNativeSessionCaptureMonotonicBoundary(t *testing.T) {
	s := nativeTestStore(t, ":memory:")
	ctx := t.Context()
	require.NoError(t, s.CreateSession(ctx, &store.SessionRecord{Namespace: "tenant", Name: "captured", SessionType: "task"}))
	require.NoError(t, s.BindSessionCleanupIdentity(ctx, "tenant", "captured", "canonical-uid"))
	require.NoError(t, s.AppendMessages(ctx, "tenant", "captured", []store.SessionMessage{{ID: "message-one", Role: "assistant", Content: "first"}}))
	snapshot := storetest.NativeSessionSnapshot(t, "first native history")
	first := store.NativeSessionRecord{Namespace: "tenant", SessionName: "captured", SessionUID: "canonical-uid", Snapshot: snapshot, MessageCount: 1, ThroughMessageID: "message-one", RuntimeSessionGeneration: 1, SourceOperationID: "capture-one"}
	require.NoError(t, s.SaveNativeSession(ctx, first))
	require.NoError(t, s.SaveNativeSession(ctx, first))
	sameOp := first
	sameOp.Snapshot = storetest.NativeSessionSnapshot(t, "changed native history")
	require.ErrorIs(t, s.SaveNativeSession(ctx, sameOp), store.ErrDuplicateMismatch)
	wrongUID := first
	wrongUID.SessionUID = "wrong-owner"
	require.ErrorIs(t, s.SaveNativeSession(ctx, wrongUID), store.ErrConflict)
	require.NoError(t, s.AppendMessages(ctx, "tenant", "captured", []store.SessionMessage{{ID: "message-two", Role: "assistant", Content: "next"}}))
	_, err := s.GetNativeSession(ctx, "tenant", "captured", "canonical-uid")
	require.ErrorIs(t, err, store.ErrConflict)
	second := first
	second.MessageCount = 2
	second.ThroughMessageID = "message-two"
	second.RuntimeSessionGeneration = 2
	second.SourceOperationID = "capture-two"
	require.NoError(t, s.SaveNativeSession(ctx, second))
	require.NoError(t, s.SaveNativeSession(ctx, first)) // Its existing receipt converges without overwriting newer state.
	got, err := s.GetNativeSession(ctx, "tenant", "captured", "canonical-uid")
	require.NoError(t, err)
	require.Equal(t, int64(2), got.RuntimeSessionGeneration)
	require.Equal(t, "message-two", got.ThroughMessageID)
	stale := second
	stale.RuntimeSessionGeneration = 1
	stale.SourceOperationID = "stale-capture"
	require.ErrorIs(t, s.SaveNativeSession(ctx, stale), store.ErrConflict)
	boundary := second
	boundary.ThroughMessageID = "message-one"
	boundary.SourceOperationID = "wrong-boundary"
	require.ErrorIs(t, s.SaveNativeSession(ctx, boundary), store.ErrConflict)
}

func TestNativeSessionRestartEncryptionAndMetadataAuthentication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "native.db")
	s := nativeTestStore(t, path)
	ctx := t.Context()
	request := nativeImportFixture(t, "restart", "sensitive native payload")
	_, err := s.StageNativeSessionImport(ctx, request)
	require.NoError(t, err)
	var ciphertext []byte
	require.NoError(t, s.db.QueryRow(`SELECT ciphertext FROM native_session_snapshots`).Scan(&ciphertext))
	require.NotContains(t, string(ciphertext), "sensitive native payload")
	require.NoError(t, s.db.Close())
	restarted := nativeTestStore(t, path)
	got, err := restarted.GetNativeSession(ctx, "tenant", "restart", "")
	require.NoError(t, err)
	require.Equal(t, request.Snapshot.Data, got.Snapshot.Data)
	different, err := NewAgentExecutionSnapshotCipher(bytes.Repeat([]byte{8}, 32))
	require.NoError(t, err)
	require.Error(t, restarted.SetAgentExecutionSnapshotCipher(different))
	got, err = restarted.GetNativeSession(ctx, "tenant", "restart", "")
	require.NoError(t, err)
	require.Equal(t, request.Snapshot.Data, got.Snapshot.Data)
	_, err = restarted.db.Exec(`UPDATE native_session_snapshots SET source_operation_id='tampered'`)
	require.NoError(t, err)
	_, err = restarted.GetNativeSession(ctx, "tenant", "restart", "")
	require.Error(t, err)
}

func TestNativeSessionCleanupErasesPrivateStateAndPreventsResurrection(t *testing.T) {
	for _, coordinated := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "coordinated"}[coordinated], func(t *testing.T) {
			s := nativeTestStore(t, ":memory:")
			ctx := t.Context()
			request := nativeImportFixture(t, "deleted", "private history")
			_, err := s.StageNativeSessionImport(ctx, request)
			require.NoError(t, err)
			if coordinated {
				intent := store.SessionCleanupIntent{Namespace: "tenant", SessionName: "deleted", OperationID: "delete", OperationDigest: store.CanonicalBytesDigest([]byte("delete")), PreparedAt: time.Now().UTC()}
				_, err = s.PrepareSessionCleanup(ctx, intent)
				require.NoError(t, err)
				_, err = s.GetNativeSession(ctx, "tenant", "deleted", "")
				require.ErrorIs(t, err, store.ErrConflict)
				require.NoError(t, s.CompleteSessionCleanup(ctx, store.CompleteSessionCleanupRequest{Namespace: "tenant", SessionName: "deleted", OperationID: intent.OperationID, OperationDigest: intent.OperationDigest}))
			} else {
				require.NoError(t, s.DeleteSession(ctx, "tenant", "deleted"))
			}
			for _, table := range []string{"native_session_snapshots", "native_session_operations"} {
				var count int
				require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&count))
				require.Zero(t, count)
			}
			_, err = s.StageNativeSessionImport(ctx, request)
			require.ErrorIs(t, err, store.ErrConflict)
			require.ErrorIs(t, s.CreateSession(ctx, &store.SessionRecord{Namespace: "tenant", Name: "deleted", SessionType: "task"}), store.ErrConflict)
			_, err = s.AcquireChatTurn(ctx, &store.SessionRecord{Namespace: "tenant", Name: "deleted", SessionType: store.SessionTypeChat}, "turn", time.Now().Add(time.Minute))
			require.ErrorIs(t, err, store.ErrConflict)
			var name string
			err = s.db.QueryRow(`SELECT name FROM sessions WHERE namespace='tenant' AND name='deleted'`).Scan(&name)
			require.ErrorIs(t, err, sql.ErrNoRows)
		})
	}
}

func TestNativeSessionStorageFailsClosedWithoutCipher(t *testing.T) {
	db, err := NewDB(":memory:")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	s := NewStore(db, ":memory:")
	_, err = s.GetNativeSession(t.Context(), "tenant", "ordinary", "not-yet-bound")
	require.ErrorIs(t, err, store.ErrNotFound)
	request := nativeImportFixture(t, "no-key", "private")
	_, err = s.StageNativeSessionImport(t.Context(), request)
	require.ErrorIs(t, err, errSnapshotCipherRequired)
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count))
	require.Zero(t, count)
}

func TestNativeSessionDeletedNameSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "native-delete.db")
	s := nativeTestStore(t, path)
	request := nativeImportFixture(t, "deleted-before-restart", "private")
	_, err := s.StageNativeSessionImport(t.Context(), request)
	require.NoError(t, err)
	require.NoError(t, s.DeleteSession(t.Context(), request.Namespace, request.SessionName))
	require.NoError(t, s.db.Close())
	restarted := nativeTestStore(t, path)
	_, err = restarted.StageNativeSessionImport(t.Context(), request)
	require.ErrorIs(t, err, store.ErrConflict)
	_, err = restarted.GetNativeSession(t.Context(), request.Namespace, request.SessionName, "")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestNativeSessionAtomicCanonicalFinalization(t *testing.T) {
	for _, persistenceOnly := range []bool{true, false} {
		t.Run(map[bool]string{true: "hybrid-persistence", false: "sqlite-control"}[persistenceOnly], func(t *testing.T) {
			s := nativeTestStore(t, ":memory:")
			ctx := t.Context()
			request := sessionTurnFinalizationOwnershipFixture(t, s, persistenceOnly, "native-task-uid")
			require.NoError(t, s.BindSessionCleanupIdentity(ctx, "ns", "session", request.Key.SessionUID))
			native := &store.NativeSessionRecord{Namespace: "ns", SessionName: "session", SessionUID: request.Key.SessionUID, Snapshot: storetest.NativeSessionSnapshot(t, "completed native task"), RuntimeSessionGeneration: 1, SourceOperationID: "native-capture-operation", MessageCount: 999, ThroughMessageID: "untrusted-guessed-boundary"}
			request.NativeSession = native
			finalize := func(request store.FinalizeSessionTurnRequest) (*store.SessionTurn, error) {
				if !persistenceOnly {
					return s.FinalizeSessionTurn(ctx, request)
				}
				return s.CommitSessionTurnFinalization(ctx, store.CommitSessionTurnFinalizationRequest{
					Key: request.Key, Namespace: "ns", SessionName: "session", Fence: request.Fence, ExpectedTurnVersion: request.ExpectedTurnVersion,
					FinalizationDigest: request.FinalizationDigest, TerminalKind: request.TerminalKind, TerminalContent: request.TerminalContent, Projection: request.Projection, FinalizedAt: request.FinalizedAt, NativeSession: request.NativeSession,
				})
			}
			invalid := request
			wrong := *native
			wrong.Snapshot.Data = []byte("invalid")
			invalid.NativeSession = &wrong
			_, err := finalize(invalid)
			require.Error(t, err)
			session, err := s.GetSession(ctx, "ns", "session")
			require.NoError(t, err)
			require.Empty(t, session.Messages)
			require.Zero(t, session.MessageCount)
			turnID, err := request.Key.CanonicalID()
			require.NoError(t, err)
			turn, err := s.GetSessionTurn(ctx, turnID)
			require.NoError(t, err)
			require.Equal(t, store.SessionTurnOpen, turn.State)
			_, err = s.GetNativeSession(ctx, "ns", "session", request.Key.SessionUID)
			require.ErrorIs(t, err, store.ErrNotFound)
			turn, err = finalize(request)
			require.NoError(t, err)
			require.Equal(t, store.NativeSessionCaptureDigest(native), turn.NativeSessionDigest)
			stored, err := s.GetNativeSession(ctx, "ns", "session", request.Key.SessionUID)
			require.NoError(t, err)
			session, err = s.GetSession(ctx, "ns", "session")
			require.NoError(t, err)
			require.Equal(t, 2, stored.MessageCount)
			require.Equal(t, session.Messages[len(session.Messages)-1].ID, stored.ThroughMessageID)
			require.NotEqual(t, native.ThroughMessageID, stored.ThroughMessageID)
			retry, err := finalize(request)
			require.NoError(t, err)
			require.Equal(t, turn.NativeSessionDigest, retry.NativeSessionDigest)
			omitted := request
			omitted.NativeSession = nil
			_, err = finalize(omitted)
			require.ErrorIs(t, err, store.ErrConflict)
			changed := request
			other := *native
			other.Snapshot = storetest.NativeSessionSnapshot(t, "changed native state")
			changed.NativeSession = &other
			_, err = finalize(changed)
			require.ErrorIs(t, err, store.ErrConflict)
			session, err = s.GetSession(ctx, "ns", "session")
			require.NoError(t, err)
			require.Equal(t, 2, session.MessageCount)
		})
	}
}

func TestNativeSessionCarriedAcrossOutcomeMarker(t *testing.T) {
	s := nativeTestStore(t, ":memory:")
	ctx := t.Context()
	request := sessionTurnFinalizationOwnershipFixture(t, s, false, "carried-task-uid")
	uid := request.Key.SessionUID
	require.NoError(t, s.BindSessionCleanupIdentity(ctx, "ns", "session", uid))
	prior := store.NativeSessionRecord{Namespace: "ns", SessionName: "session", SessionUID: uid, Snapshot: storetest.NativeSessionSnapshot(t, "checkpoint before a failed prompt"), RuntimeSessionGeneration: 1, SourceOperationID: "prior-capture"}
	require.NoError(t, s.SaveNativeSession(ctx, prior))
	stored, err := s.GetNativeSession(ctx, "ns", "session", uid)
	require.NoError(t, err)

	marker := request
	marker.TerminalKind = store.SessionTurnOutcomeMarker
	marker.TerminalContent = `{"kind":"Failed","reason":"prompt failed","assistantResultRecorded":false}`
	marker.NativeSession = stored
	marker.NativeSessionCarried = true

	fresh := marker
	fresh.NativeSessionCarried = false
	_, err = s.FinalizeSessionTurn(ctx, fresh)
	require.Error(t, err, "a marker must not accept a fresh capture")
	carriedResult := request
	carriedResult.NativeSession = stored
	carriedResult.NativeSessionCarried = true
	_, err = s.FinalizeSessionTurn(ctx, carriedResult)
	require.Error(t, err, "an assistant result must not carry a stale checkpoint")
	changed := marker
	other := *stored
	other.Snapshot = storetest.NativeSessionSnapshot(t, "different native state")
	changed.NativeSession = &other
	_, err = s.FinalizeSessionTurn(ctx, changed)
	require.ErrorIs(t, err, store.ErrConflict, "carried bytes must match the stored checkpoint")
	turn, err := s.GetSessionTurn(ctx, mustTurnID(t, request.Key))
	require.NoError(t, err)
	require.Equal(t, store.SessionTurnOpen, turn.State, "rejected finalizations must not settle the turn")
	unchanged, err := s.GetNativeSession(ctx, "ns", "session", uid)
	require.NoError(t, err)
	require.Equal(t, stored, unchanged)

	turn, err = s.FinalizeSessionTurn(ctx, marker)
	require.NoError(t, err)
	require.Equal(t, store.NativeSessionCaptureDigest(stored), turn.NativeSessionDigest)
	session, err := s.GetSession(ctx, "ns", "session")
	require.NoError(t, err)
	require.Equal(t, 2, session.MessageCount, "the user prompt and failure marker advance canonical history")
	after, err := s.GetNativeSession(ctx, "ns", "session", uid)
	require.NoError(t, err, "the carried checkpoint must cover the advanced transcript")
	require.Equal(t, stored.Snapshot, after.Snapshot)
	require.Equal(t, stored.SourceOperationID, after.SourceOperationID)
	require.Equal(t, stored.RuntimeSessionGeneration, after.RuntimeSessionGeneration)
	require.Equal(t, 2, after.MessageCount)
	require.Equal(t, session.Messages[len(session.Messages)-1].ID, after.ThroughMessageID)
	retry, err := s.FinalizeSessionTurn(ctx, marker)
	require.NoError(t, err)
	require.Equal(t, turn.NativeSessionDigest, retry.NativeSessionDigest)
}

func mustTurnID(t *testing.T, key store.SessionTurnKey) string {
	t.Helper()
	id, err := key.CanonicalID()
	require.NoError(t, err)
	return id
}
