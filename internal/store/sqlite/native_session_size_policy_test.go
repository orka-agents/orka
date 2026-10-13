package sqlite

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/orka-agents/orka/internal/codexstate"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/storetest"
	"github.com/stretchr/testify/require"
)

func nativePolicySnapshot(t *testing.T, size int) harnessv2.NativeSessionSnapshot {
	t.Helper()
	snapshot := storetest.NativeSessionSnapshot(t, "private stored policy fixture")
	require.LessOrEqual(t, len(snapshot.Data), size)
	snapshot.Data = append(bytes.Clone(snapshot.Data), bytes.Repeat([]byte{' '}, size-len(snapshot.Data))...)
	summary, err := codexstate.Inspect(t.Context(), snapshot.Data, harnessv2.MaxNativeSessionBytes)
	require.NoError(t, err)
	snapshot.DataDigest = summary.DataDigest
	return snapshot
}

func nativePolicyImport(snapshot harnessv2.NativeSessionSnapshot, name string) store.NativeSessionImport {
	snapshot.RuntimeSessionUID = ""
	snapshot.RuntimeProfileDigest = ""
	snapshot.WorkingDirectory = ""
	return store.NativeSessionImport{
		Namespace: "tenant", SessionName: name, OperationID: "import-" + name,
		RequestDigest: store.NativeSessionImportDigest("tenant", name, snapshot.DataDigest), Snapshot: snapshot,
	}
}

func TestNativeSessionSizePolicyStoreConfiguration(t *testing.T) {
	s := nativeTestStore(t, ":memory:")
	require.Equal(t, harnessv2.DefaultMaxNativeSessionBytes, s.nativeSessionMaxBytes)
	for _, limit := range []int{640 << 10, harnessv2.MaxNativeSessionBytes, 0} {
		require.NoError(t, s.SetNativeSessionMaxBytes(limit))
		want := limit
		if want == 0 {
			want = harnessv2.DefaultMaxNativeSessionBytes
		}
		require.Equal(t, want, s.nativeSessionMaxBytes)
	}
	require.NoError(t, s.SetNativeSessionMaxBytes(640<<10))
	for _, limit := range []int{-1, harnessv2.MaxNativeSessionBytes + 1} {
		require.Error(t, s.SetNativeSessionMaxBytes(limit))
		require.Equal(t, 640<<10, s.nativeSessionMaxBytes, "rejected configuration must not change the previous policy")
	}
	locked, err := OpenLockedStore(filepath.Join(t.TempDir(), "native-policy.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, locked.close()) })
	require.Equal(t, harnessv2.DefaultMaxNativeSessionBytes, locked.nativeSessionMaxBytes)
}

func TestNativeSessionSizePolicyStoreImportLimitAndRetainedRead(t *testing.T) {
	const limit = 640 << 10
	s := nativeTestStore(t, ":memory:")
	require.NoError(t, s.SetNativeSessionMaxBytes(limit))
	snapshot := nativePolicySnapshot(t, limit)
	request := nativePolicyImport(snapshot, "exact")
	_, err := s.StageNativeSessionImport(t.Context(), request)
	require.NoError(t, err)
	before, err := s.GetNativeSession(t.Context(), "tenant", "exact", "")
	require.NoError(t, err)
	require.Equal(t, snapshot.Data, before.Snapshot.Data)

	over := nativePolicyImport(nativePolicySnapshot(t, limit+1), "over")
	_, err = s.StageNativeSessionImport(t.Context(), over)
	require.ErrorIs(t, err, store.ErrValidation)
	_, err = s.GetSession(t.Context(), "tenant", "over")
	require.ErrorIs(t, err, store.ErrNotFound)

	require.NoError(t, s.SetNativeSessionMaxBytes(limit-1))
	after, err := s.GetNativeSession(t.Context(), "tenant", "exact", "")
	require.NoError(t, err, "lowering the write policy must preserve stored imports")
	require.Equal(t, before, after)
	_, err = s.StageNativeSessionImport(t.Context(), nativePolicyImport(snapshot, "lowered-over"))
	require.ErrorIs(t, err, store.ErrValidation)
}

func TestNativeSessionSizePolicyStoreDefaultAndRaisedLimit(t *testing.T) {
	s := nativeTestStore(t, ":memory:")
	const size = harnessv2.DefaultMaxNativeSessionBytes + 1
	request := nativePolicyImport(nativePolicySnapshot(t, size), "raised")
	_, err := s.StageNativeSessionImport(t.Context(), request)
	require.ErrorIs(t, err, store.ErrValidation, "an omitted policy must use the 8 MiB default, not the hard ceiling")
	require.NoError(t, s.SetNativeSessionMaxBytes(size))
	_, err = s.StageNativeSessionImport(t.Context(), request)
	require.NoError(t, err, "the configured write policy must be passed to bundle inspection")
	require.NoError(t, s.SetNativeSessionMaxBytes(0))
	stored, err := s.GetNativeSession(t.Context(), "tenant", "raised", "")
	require.NoError(t, err)
	require.Equal(t, request.Snapshot.Data, stored.Snapshot.Data)
}

func TestNativeSessionSizePolicyStoreCaptureAndReplacement(t *testing.T) {
	const limit = 640 << 10
	s := nativeTestStore(t, ":memory:")
	require.NoError(t, s.SetNativeSessionMaxBytes(limit))
	require.NoError(t, s.CreateSession(t.Context(), &store.SessionRecord{Namespace: "tenant", Name: "captured", SessionType: "task"}))
	require.NoError(t, s.BindSessionCleanupIdentity(t.Context(), "tenant", "captured", "canonical-uid"))
	record := store.NativeSessionRecord{
		Namespace: "tenant", SessionName: "captured", SessionUID: "canonical-uid", Snapshot: nativePolicySnapshot(t, limit+1),
		RuntimeSessionGeneration: 1, SourceOperationID: "oversized-capture",
	}
	require.ErrorIs(t, s.SaveNativeSession(t.Context(), record), store.ErrValidation)
	record.Snapshot = nativePolicySnapshot(t, limit)
	record.SourceOperationID = "exact-capture"
	require.NoError(t, s.SaveNativeSession(t.Context(), record))
	before, err := s.GetNativeSession(t.Context(), "tenant", "captured", "canonical-uid")
	require.NoError(t, err)

	require.NoError(t, s.SetNativeSessionMaxBytes(limit-1))
	attempt := record
	attempt.SourceOperationID = "lowered-over-capture"
	attempt.RuntimeSessionGeneration++
	require.ErrorIs(t, s.SaveNativeSession(t.Context(), attempt), store.ErrValidation)
	after, err := s.GetNativeSession(t.Context(), "tenant", "captured", "canonical-uid")
	require.NoError(t, err)
	require.Equal(t, before, after)

	attempt.Snapshot = storetest.NativeSessionSnapshot(t, "valid smaller replacement")
	attempt.SourceOperationID = "smaller-replacement"
	require.NoError(t, s.SaveNativeSession(t.Context(), attempt), "the previous record is read under the hard ceiling, not the lowered policy")
	after, err = s.GetNativeSession(t.Context(), "tenant", "captured", "canonical-uid")
	require.NoError(t, err)
	require.Equal(t, attempt.Snapshot.Data, after.Snapshot.Data)
}

func TestNativeSessionSizePolicyStoreImportedFirstCapture(t *testing.T) {
	const limit = 640 << 10
	s := nativeTestStore(t, ":memory:")
	require.NoError(t, s.SetNativeSessionMaxBytes(limit))
	request := nativeImportFixture(t, "imported-policy", "initial import")
	_, err := s.StageNativeSessionImport(t.Context(), request)
	require.NoError(t, err)
	require.NoError(t, s.BindSessionCleanupIdentity(t.Context(), "tenant", "imported-policy", "canonical-uid"))
	record := store.NativeSessionRecord{
		Namespace: "tenant", SessionName: "imported-policy", SessionUID: "canonical-uid", Snapshot: nativePolicySnapshot(t, limit+1),
		RuntimeSessionGeneration: 1, SourceOperationID: "first-capture",
	}
	require.ErrorIs(t, s.SaveNativeSession(t.Context(), record), store.ErrValidation)
	stored, err := s.GetNativeSession(t.Context(), "tenant", "imported-policy", "canonical-uid")
	require.NoError(t, err)
	require.Equal(t, request.Snapshot.Data, stored.Snapshot.Data)
	require.Zero(t, stored.RuntimeSessionGeneration)
	record.Snapshot = nativePolicySnapshot(t, limit)
	require.NoError(t, s.SaveNativeSession(t.Context(), record))
}

func TestNativeSessionSizePolicyStoreFinalizationAtomicLimit(t *testing.T) {
	const limit = 640 << 10
	for _, persistenceOnly := range []bool{false, true} {
		t.Run(fmt.Sprint(persistenceOnly), func(t *testing.T) {
			s := nativeTestStore(t, ":memory:")
			require.NoError(t, s.SetNativeSessionMaxBytes(limit))
			request := sessionTurnFinalizationOwnershipFixture(t, s, persistenceOnly, "native-policy-task")
			require.NoError(t, s.BindSessionCleanupIdentity(t.Context(), "ns", "session", request.Key.SessionUID))
			request.NativeSession = &store.NativeSessionRecord{
				Namespace: "ns", SessionName: "session", SessionUID: request.Key.SessionUID, Snapshot: nativePolicySnapshot(t, limit+1),
				RuntimeSessionGeneration: 1, SourceOperationID: "finalized-capture",
			}
			finalize := func() error {
				if !persistenceOnly {
					_, err := s.FinalizeSessionTurn(t.Context(), request)
					return err
				}
				_, err := s.CommitSessionTurnFinalization(t.Context(), store.CommitSessionTurnFinalizationRequest{
					Key: request.Key, Namespace: "ns", SessionName: "session", Fence: request.Fence, ExpectedTurnVersion: request.ExpectedTurnVersion,
					FinalizationDigest: request.FinalizationDigest, TerminalKind: request.TerminalKind, TerminalContent: request.TerminalContent,
					Projection: request.Projection, FinalizedAt: request.FinalizedAt, NativeSession: request.NativeSession,
				})
				return err
			}
			require.ErrorIs(t, finalize(), store.ErrValidation)
			session, err := s.GetSession(t.Context(), "ns", "session")
			require.NoError(t, err)
			require.Zero(t, session.MessageCount)
			require.Empty(t, session.Messages)
			turn, err := s.GetSessionTurn(t.Context(), mustTurnID(t, request.Key))
			require.NoError(t, err)
			require.Equal(t, store.SessionTurnOpen, turn.State)
			request.NativeSession.Snapshot = nativePolicySnapshot(t, limit)
			require.NoError(t, finalize())
			stored, err := s.GetNativeSession(t.Context(), "ns", "session", request.Key.SessionUID)
			require.NoError(t, err)
			require.Equal(t, request.NativeSession.Snapshot.Data, stored.Snapshot.Data)
		})
	}
}

func TestNativeSessionSizePolicyStoreCarryAfterLoweringLimit(t *testing.T) {
	const limit = 640 << 10
	s := nativeTestStore(t, ":memory:")
	require.NoError(t, s.SetNativeSessionMaxBytes(limit))
	request := sessionTurnFinalizationOwnershipFixture(t, s, false, "native-policy-carry-task")
	uid := request.Key.SessionUID
	require.NoError(t, s.BindSessionCleanupIdentity(t.Context(), "ns", "session", uid))
	record := store.NativeSessionRecord{
		Namespace: "ns", SessionName: "session", SessionUID: uid, Snapshot: nativePolicySnapshot(t, limit),
		RuntimeSessionGeneration: 1, SourceOperationID: "stored-checkpoint",
	}
	require.NoError(t, s.SaveNativeSession(t.Context(), record))
	stored, err := s.GetNativeSession(t.Context(), "ns", "session", uid)
	require.NoError(t, err)
	require.NoError(t, s.SetNativeSessionMaxBytes(limit-1))
	request.TerminalKind = store.SessionTurnOutcomeMarker
	request.TerminalContent = `{"kind":"Failed","reason":"prompt failed","assistantResultRecorded":false}`
	request.NativeSession = stored
	request.NativeSessionCarried = true
	_, err = s.FinalizeSessionTurn(t.Context(), request)
	require.NoError(t, err, "carrying exact existing bytes is not a new capture")
	after, err := s.GetNativeSession(t.Context(), "ns", "session", uid)
	require.NoError(t, err)
	require.Equal(t, stored.Snapshot, after.Snapshot)
	require.Equal(t, 2, after.MessageCount)
}
