package storagetest

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// Sealed records carry bytes the provider cannot read. These cases use
// arbitrary opaque bytes: a provider must store and return them exactly, and
// decide every conditional commit from the plain fields beside them.

var sealedBaseTime = time.Date(2026, 9, 1, 10, 30, 0, 123456789, time.FixedZone("UTC+3", 3*60*60))

func sealedBytes(label string) []byte {
	return []byte("opaque sealed bytes: " + label)
}

func sealedSnapshot(taskUID, label string) storagev1.SealedAgentExecutionSnapshot {
	return storagev1.SealedAgentExecutionSnapshot{
		Key:           storagev1.AgentExecutionSnapshotKey{TaskUID: taskUID, Digest: controlDigest(taskUID + "/" + label)},
		SchemaVersion: 1,
		Nonce:         sealedBytes("nonce " + label),
		Ciphertext:    sealedBytes("ciphertext " + label),
		CreatedAt:     sealedBaseTime,
	}
}

func requireSameSealedSnapshot(t *testing.T, want storagev1.SealedAgentExecutionSnapshot, got *storagev1.SealedAgentExecutionSnapshot) {
	t.Helper()
	require.NotNil(t, got)
	require.Equal(t, want.Key, got.Key)
	require.Equal(t, want.SchemaVersion, got.SchemaVersion)
	require.True(t, bytes.Equal(want.Nonce, got.Nonce), "nonce changed")
	require.True(t, bytes.Equal(want.Ciphertext, got.Ciphertext), "ciphertext changed")
	require.True(t, want.CreatedAt.Equal(got.CreatedAt), "creation time %s, want %s", got.CreatedAt, want.CreatedAt)
	require.Equal(t, time.UTC, got.CreatedAt.Location())
}

func runSealedSnapshots(t *testing.T, newProvider StateFactory) {
	t.Run("create and read opaque bytes", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		snapshot := sealedSnapshot("task-a", "first")
		require.NoError(t, provider.CreateSealedAgentExecutionSnapshot(ctx, snapshot))
		got, err := provider.GetSealedAgentExecutionSnapshot(ctx, snapshot.Key)
		require.NoError(t, err)
		requireSameSealedSnapshot(t, snapshot, got)

		replacement := snapshot
		replacement.Ciphertext = sealedBytes("another seal of the same key")
		require.ErrorIs(t, provider.CreateSealedAgentExecutionSnapshot(ctx, replacement), storagev1.ErrConflict,
			"an existing key is never replaced; Orka compares the stored record")
		got, err = provider.GetSealedAgentExecutionSnapshot(ctx, snapshot.Key)
		require.NoError(t, err)
		requireSameSealedSnapshot(t, snapshot, got)

		_, err = provider.GetSealedAgentExecutionSnapshot(ctx, sealedSnapshot("task-a", "absent").Key)
		require.ErrorIs(t, err, storagev1.ErrNotFound)
	})

	t.Run("validation", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		for name, mutate := range map[string]func(*storagev1.SealedAgentExecutionSnapshot){
			"task UID":       func(s *storagev1.SealedAgentExecutionSnapshot) { s.Key.TaskUID = "" },
			"digest":         func(s *storagev1.SealedAgentExecutionSnapshot) { s.Key.Digest = "not-a-digest" },
			"schema version": func(s *storagev1.SealedAgentExecutionSnapshot) { s.SchemaVersion = 0 },
			"nonce":          func(s *storagev1.SealedAgentExecutionSnapshot) { s.Nonce = nil },
			"ciphertext":     func(s *storagev1.SealedAgentExecutionSnapshot) { s.Ciphertext = nil },
			"creation time":  func(s *storagev1.SealedAgentExecutionSnapshot) { s.CreatedAt = time.Time{} },
		} {
			snapshot := sealedSnapshot("task-invalid", name)
			mutate(&snapshot)
			require.ErrorIs(t, provider.CreateSealedAgentExecutionSnapshot(ctx, snapshot), storagev1.ErrValidation, name)
		}
		_, err := provider.GetSealedAgentExecutionSnapshot(ctx, storagev1.AgentExecutionSnapshotKey{TaskUID: "task"})
		require.ErrorIs(t, err, storagev1.ErrValidation)
	})

	t.Run("list pages every record once in key order", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		want := make([]storagev1.SealedAgentExecutionSnapshot, 0, 5)
		for _, item := range [][2]string{{"task-b", "1"}, {"task-a", "2"}, {"task-c", "3"}, {"task-a", "4"}, {"task-b", "5"}} {
			snapshot := sealedSnapshot(item[0], item[1])
			require.NoError(t, provider.CreateSealedAgentExecutionSnapshot(ctx, snapshot))
			want = append(want, snapshot)
		}
		var listed []storagev1.SealedAgentExecutionSnapshot
		cursor := ""
		for pages := 0; ; pages++ {
			require.Less(t, pages, 4, "listing did not finish")
			page, err := provider.ListSealedAgentExecutionSnapshots(ctx, cursor, 2)
			require.NoError(t, err)
			require.LessOrEqual(t, len(page.Snapshots), 2)
			listed = append(listed, page.Snapshots...)
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
		require.Len(t, listed, len(want))
		for i := 1; i < len(listed); i++ {
			previous, current := listed[i-1].Key, listed[i].Key
			require.True(t, previous.TaskUID < current.TaskUID || (previous.TaskUID == current.TaskUID && previous.Digest < current.Digest),
				"records out of order: %s before %s", previous.ID(), current.ID())
		}
		for _, snapshot := range want {
			found := false
			for i := range listed {
				if listed[i].Key == snapshot.Key {
					requireSameSealedSnapshot(t, snapshot, &listed[i])
					found = true
				}
			}
			require.True(t, found, "listing missed %s", snapshot.Key.ID())
		}

		_, err := provider.ListSealedAgentExecutionSnapshots(ctx, "", 0)
		require.ErrorIs(t, err, storagev1.ErrValidation)
		_, err = provider.ListSealedAgentExecutionSnapshots(ctx, "", storagev1.MaxSealedRecordPageSize+1)
		require.ErrorIs(t, err, storagev1.ErrValidation)
		_, err = provider.ListSealedAgentExecutionSnapshots(ctx, "not a provider cursor", 2)
		require.ErrorIs(t, err, storagev1.ErrValidation)
	})

	t.Run("retention metadata and deletion", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		early := sealedSnapshot("task-a", "early")
		late := sealedSnapshot("task-a", "late")
		late.CreatedAt = early.CreatedAt.Add(time.Minute)
		other := sealedSnapshot("task-b", "other")
		for _, snapshot := range []storagev1.SealedAgentExecutionSnapshot{early, late, other} {
			require.NoError(t, provider.CreateSealedAgentExecutionSnapshot(ctx, snapshot))
		}
		metadata, err := provider.ListAgentExecutionSnapshotMetadataBefore(ctx, late.CreatedAt)
		require.NoError(t, err)
		keys := make([]storagev1.AgentExecutionSnapshotKey, 0, len(metadata))
		for _, item := range metadata {
			keys = append(keys, item.Key)
		}
		require.ElementsMatch(t, []storagev1.AgentExecutionSnapshotKey{early.Key, other.Key}, keys, "the cutoff is strict")

		counts, err := provider.CountAgentExecutionSnapshotReferences(ctx, early.Key)
		require.NoError(t, err)
		require.Zero(t, counts.Total())

		require.NoError(t, provider.DeleteAgentExecutionSnapshot(ctx, early.Key))
		require.NoError(t, provider.DeleteAgentExecutionSnapshot(ctx, early.Key), "deletion is idempotent")
		_, err = provider.GetSealedAgentExecutionSnapshot(ctx, early.Key)
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		_, err = provider.GetSealedAgentExecutionSnapshot(ctx, late.Key)
		require.NoError(t, err)

		require.NoError(t, provider.DeleteAgentExecutionSnapshots(ctx, "task-a"))
		_, err = provider.GetSealedAgentExecutionSnapshot(ctx, late.Key)
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		_, err = provider.GetSealedAgentExecutionSnapshot(ctx, other.Key)
		require.NoError(t, err, "deleting one Task's snapshots keeps another's")
	})
}

func sealedNativeSession(name, uid, label string, generation int64) storagev1.SealedNativeSession {
	return storagev1.SealedNativeSession{
		Namespace: "tenant", SessionName: name, SessionUID: uid,
		RecordDigest:             controlDigest("record " + label),
		DataDigest:               controlDigest("data " + label),
		SourceOperationID:        "operation-" + label,
		RuntimeSessionGeneration: generation,
		Nonce:                    sealedBytes("nonce " + label),
		Ciphertext:               sealedBytes("ciphertext " + label),
	}
}

func nativeImportRequest(name, label string) storagev1.StageSealedNativeSessionImportRequest {
	record := sealedNativeSession(name, "", label, 0)
	return storagev1.StageSealedNativeSessionImportRequest{
		Record: record,
		Receipt: storagev1.NativeSessionImportReceipt{
			Namespace: "tenant", SessionName: name, OperationID: record.SourceOperationID,
			RequestDigest: controlDigest("import request " + label), DataDigest: record.DataDigest,
			ProviderSessionID: "provider-" + label, CreatedAt: sealedBaseTime,
		},
	}
}

func requireSameSealedNativeSession(t *testing.T, want storagev1.SealedNativeSession, got *storagev1.SealedNativeSession) {
	t.Helper()
	require.NotNil(t, got)
	nonce, ciphertext := got.Nonce, got.Ciphertext
	require.True(t, bytes.Equal(want.Nonce, nonce), "nonce changed")
	require.True(t, bytes.Equal(want.Ciphertext, ciphertext), "ciphertext changed")
	gotFields, wantFields := *got, want
	gotFields.Nonce, gotFields.Ciphertext, wantFields.Nonce, wantFields.Ciphertext = nil, nil, nil, nil
	require.Equal(t, wantFields, gotFields)
}

func runSealedNativeSessions(t *testing.T, newProvider StateFactory) {
	t.Run("import reserves a new Session once", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		request := nativeImportRequest("imported", "import")
		receipt, err := provider.StageSealedNativeSessionImport(ctx, request)
		require.NoError(t, err)
		require.Equal(t, request.Receipt.RequestDigest, receipt.RequestDigest)
		require.True(t, request.Receipt.CreatedAt.Equal(receipt.CreatedAt))
		replay, err := provider.StageSealedNativeSessionImport(ctx, request)
		require.NoError(t, err, "a repeated import returns its receipt")
		require.Equal(t, receipt.OperationID, replay.OperationID)
		require.True(t, receipt.CreatedAt.Equal(replay.CreatedAt))

		changed := request
		changed.Receipt.RequestDigest = controlDigest("another bundle")
		_, err = provider.StageSealedNativeSessionImport(ctx, changed)
		require.ErrorIs(t, err, storagev1.ErrDuplicateMismatch)
		another := nativeImportRequest("imported", "second operation")
		_, err = provider.StageSealedNativeSessionImport(ctx, another)
		require.ErrorIs(t, err, storagev1.ErrConflict, "an import never replaces an existing Session")

		require.NoError(t, provider.CreateSession(ctx, &storagev1.SessionRecord{Namespace: "tenant", Name: "existing", SessionType: "task"}))
		_, err = provider.StageSealedNativeSessionImport(ctx, nativeImportRequest("existing", "onto existing"))
		require.ErrorIs(t, err, storagev1.ErrConflict)

		got, err := provider.GetSealedNativeSession(ctx, "tenant", "imported", "")
		require.NoError(t, err)
		requireSameSealedNativeSession(t, request.Record, got)
	})

	t.Run("reads only for the canonical owner at the current boundary", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		_, err := provider.GetSealedNativeSession(ctx, "tenant", "absent", "uid")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		request := nativeImportRequest("owned", "owned")
		_, err = provider.StageSealedNativeSessionImport(ctx, request)
		require.NoError(t, err)
		_, err = provider.GetSealedNativeSession(ctx, "tenant", "owned", "someone-else")
		require.ErrorIs(t, err, storagev1.ErrConflict)
		require.NoError(t, provider.BindSessionCleanupIdentity(ctx, "tenant", "owned", "canonical-uid"))
		got, err := provider.GetSealedNativeSession(ctx, "tenant", "owned", "canonical-uid")
		require.NoError(t, err, "an import is read by the owner that binds it")
		requireSameSealedNativeSession(t, request.Record, got)
		_, err = provider.GetSealedNativeSession(ctx, "tenant", "owned", "")
		require.ErrorIs(t, err, storagev1.ErrConflict)
	})

	t.Run("captures advance monotonically", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		require.NoError(t, provider.CreateSession(ctx, &storagev1.SessionRecord{Namespace: "tenant", Name: "captured", SessionType: "task"}))
		require.NoError(t, provider.BindSessionCleanupIdentity(ctx, "tenant", "captured", "owner-uid"))
		capture := func(label string, generation int64) storagev1.SaveSealedNativeSessionRequest {
			return storagev1.SaveSealedNativeSessionRequest{
				Record:        sealedNativeSession("captured", "owner-uid", label, generation),
				RequestDigest: controlDigest("capture request " + label),
			}
		}
		first := capture("first", 1)
		require.NoError(t, provider.SaveSealedNativeSession(ctx, first))
		require.NoError(t, provider.SaveSealedNativeSession(ctx, first), "a repeated capture is idempotent")
		changed := first
		changed.RequestDigest = controlDigest("another capture under the same operation")
		require.ErrorIs(t, provider.SaveSealedNativeSession(ctx, changed), storagev1.ErrDuplicateMismatch)

		same := capture("same position", 1)
		require.ErrorIs(t, provider.SaveSealedNativeSession(ctx, same), storagev1.ErrConflict, "a capture must advance the checkpoint")
		foreign := capture("foreign", 2)
		foreign.Record.SessionUID = "another-owner"
		require.ErrorIs(t, provider.SaveSealedNativeSession(ctx, foreign), storagev1.ErrConflict)
		stale := capture("stale boundary", 2)
		stale.Record.MessageCount, stale.Record.ThroughMessageID = 1, "message-that-does-not-exist"
		require.ErrorIs(t, provider.SaveSealedNativeSession(ctx, stale), storagev1.ErrConflict, "a capture must cover the current transcript")

		second := capture("second", 2)
		require.NoError(t, provider.SaveSealedNativeSession(ctx, second))
		older := capture("older", 1)
		require.ErrorIs(t, provider.SaveSealedNativeSession(ctx, older), storagev1.ErrConflict, "an older capture never replaces newer history")
		got, err := provider.GetSealedNativeSession(ctx, "tenant", "captured", "owner-uid")
		require.NoError(t, err)
		requireSameSealedNativeSession(t, second.Record, got)

		invalid := capture("staged", 3)
		invalid.Record.SessionUID, invalid.Record.RuntimeSessionGeneration = "", 0
		require.ErrorIs(t, provider.SaveSealedNativeSession(ctx, invalid), storagev1.ErrValidation)
	})

	t.Run("list pages every record once", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		want := map[string]storagev1.SealedNativeSession{}
		for i, name := range []string{"b", "a", "c"} {
			request := nativeImportRequest("listed-"+name, fmt.Sprintf("listed %d", i))
			_, err := provider.StageSealedNativeSessionImport(ctx, request)
			require.NoError(t, err)
			want[request.Record.SessionName] = request.Record
		}
		seen := map[string]bool{}
		cursor := ""
		for pages := 0; ; pages++ {
			require.Less(t, pages, 4, "listing did not finish")
			page, err := provider.ListSealedNativeSessions(ctx, cursor, 1)
			require.NoError(t, err)
			require.LessOrEqual(t, len(page.Sessions), 1)
			for i := range page.Sessions {
				record := page.Sessions[i]
				require.False(t, seen[record.SessionName], "listed %s twice", record.SessionName)
				seen[record.SessionName] = true
				requireSameSealedNativeSession(t, want[record.SessionName], &record)
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
		require.Len(t, seen, len(want))
		_, err := provider.ListSealedNativeSessions(ctx, "not a provider cursor", 1)
		require.ErrorIs(t, err, storagev1.ErrValidation)
	})
}

// turnFixture is an open SessionTurn of a Session whose canonical owner is
// bound, ready to finalize.
type turnFixture struct {
	provider storagev1.StateProvider
	name     string
	turn     *storagev1.SessionTurn
	fence    storagev1.ControllerEpochFence
}

func newTurnFixture(t *testing.T, provider storagev1.StateProvider, name string) turnFixture {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, provider.CreateSession(ctx, &storagev1.SessionRecord{Namespace: "tenant", Name: name, SessionType: "task"}))
	uid := name + "-uid"
	require.NoError(t, provider.BindSessionCleanupIdentity(ctx, "tenant", name, uid))
	fence := controlFence(1, "controller")
	request := storagev1.CreateSessionTurnRecordRequest{
		Namespace: "tenant", SessionName: name, Fence: fence,
		Turn: storagev1.SessionTurn{
			Key:             storagev1.SessionTurnKey{SessionUID: uid, LeaseGeneration: 1, TaskUID: name + "-task", Attempt: 1, PromptID: "prompt"},
			PromptAttemptID: name + "-attempt", RequestDigest: controlDigest(name + " turn"), UserPrompt: "the question",
			CreatedAt: sealedBaseTime, UpdatedAt: sealedBaseTime,
		},
	}
	turn, err := provider.CreateSessionTurnRecord(ctx, request)
	require.NoError(t, err)
	replay, err := provider.CreateSessionTurnRecord(ctx, request)
	require.NoError(t, err, "a repeated create returns the stored turn")
	require.Equal(t, turn.ID, replay.ID)
	stored, err := provider.GetSessionTurn(ctx, turn.ID)
	require.NoError(t, err)
	require.Equal(t, storagev1.SessionTurnOpen, stored.State)
	return turnFixture{provider: provider, name: name, turn: turn, fence: fence}
}

func (f turnFixture) finalization(label string) storagev1.SealedSessionTurnFinalization {
	payload := []byte(`{"phase":"Succeeded"}`)
	return storagev1.SealedSessionTurnFinalization{
		Key: f.turn.Key, Namespace: "tenant", SessionName: f.name, Fence: f.fence,
		ExpectedTurnVersion: f.turn.Version, FinalizationDigest: controlDigest("finalization " + label),
		TerminalKind: storagev1.SessionTurnAssistantResult, TerminalContent: "the answer",
		Projection: storagev1.OutboxProjection{
			ID: "projection-" + f.turn.ID, AggregateKind: "SessionTurn", AggregateID: f.turn.ID,
			ProjectionKind: "TaskTerminalStatus", Payload: payload, PayloadDigest: storagev1.CanonicalBytesDigest(payload),
		},
		FinalizedAt:       sealedBaseTime.Add(time.Minute),
		UserMessageID:     "user-" + label,
		TerminalMessageID: "terminal-" + label,
	}
}

func (f turnFixture) basis(t *testing.T) *storagev1.SessionTurnFinalizationBasis {
	t.Helper()
	basis, err := f.provider.ReadSessionTurnFinalizationBasis(t.Context(), f.turn.ID, "tenant", f.name)
	require.NoError(t, err)
	return basis
}

// nativeFinalization seals a capture against the boundary after the
// finalization appends its two messages.
func (f turnFixture) nativeFinalization(t *testing.T, finalization storagev1.SealedSessionTurnFinalization) *storagev1.SealedNativeSessionFinalization {
	t.Helper()
	basis := f.basis(t)
	record := sealedNativeSession(f.name, f.turn.Key.SessionUID, finalization.TerminalMessageID, 1)
	record.MessageCount, record.ThroughMessageID = basis.MessageCount+2, finalization.TerminalMessageID
	return &storagev1.SealedNativeSessionFinalization{
		Record: record, RequestDigest: controlDigest("capture " + finalization.TerminalMessageID),
		ExpectedMessageCount: basis.MessageCount, ExpectedLastMessageID: basis.LastMessageID,
	}
}

func runSessionTurnRecords(t *testing.T, newProvider StateFactory) {
	t.Run("finalization appends messages with Orka's IDs", func(t *testing.T) {
		fixture := newTurnFixture(t, newProvider(t), "plain")
		ctx := t.Context()
		basis := fixture.basis(t)
		require.Equal(t, fixture.turn.ID, basis.Turn.ID)
		require.Zero(t, basis.MessageCount)
		require.Empty(t, basis.LastMessageID)
		require.Nil(t, basis.NativeSession)

		finalization := fixture.finalization("plain")
		missing := finalization
		missing.TerminalMessageID = ""
		_, err := fixture.provider.CommitSealedSessionTurnFinalization(ctx, missing)
		require.ErrorIs(t, err, storagev1.ErrValidation)

		finalized, err := fixture.provider.CommitSealedSessionTurnFinalization(ctx, finalization)
		require.NoError(t, err)
		require.Equal(t, storagev1.SessionTurnFinalized, finalized.State)
		require.Empty(t, finalized.NativeSessionDigest)
		replay, err := fixture.provider.CommitSealedSessionTurnFinalization(ctx, finalization)
		require.NoError(t, err, "a repeated finalization returns the finalized turn")
		require.Equal(t, finalized.Version, replay.Version)
		changed := finalization
		changed.TerminalContent = "another answer"
		_, err = fixture.provider.CommitSealedSessionTurnFinalization(ctx, changed)
		require.ErrorIs(t, err, storagev1.ErrConflict)

		after := fixture.basis(t)
		require.Equal(t, 2, after.MessageCount)
		require.Equal(t, finalization.TerminalMessageID, after.LastMessageID)
		session, err := fixture.provider.GetSession(ctx, "tenant", "plain")
		require.NoError(t, err)
		require.Len(t, session.Messages, 2)
		require.Equal(t, finalization.UserMessageID, session.Messages[0].ID)
		require.Equal(t, finalization.TerminalMessageID, session.Messages[1].ID)
	})

	t.Run("sealed native capture commits at the expected boundary", func(t *testing.T) {
		fixture := newTurnFixture(t, newProvider(t), "captured")
		ctx := t.Context()
		finalization := fixture.finalization("captured")
		finalization.NativeSessionDigest = controlDigest("captured native input")
		finalization.NativeSession = fixture.nativeFinalization(t, finalization)
		finalized, err := fixture.provider.CommitSealedSessionTurnFinalization(ctx, finalization)
		require.NoError(t, err)
		require.Equal(t, finalization.NativeSessionDigest, finalized.NativeSessionDigest)
		stored, err := fixture.provider.GetSessionTurn(ctx, finalized.ID)
		require.NoError(t, err)
		require.Equal(t, finalization.NativeSessionDigest, stored.NativeSessionDigest)
		native, err := fixture.provider.GetSealedNativeSession(ctx, "tenant", "captured", fixture.turn.Key.SessionUID)
		require.NoError(t, err)
		requireSameSealedNativeSession(t, finalization.NativeSession.Record, native)
		basis := fixture.basis(t)
		requireSameSealedNativeSession(t, finalization.NativeSession.Record, basis.NativeSession)

		replay := finalization
		replay.NativeSession = nil
		_, err = fixture.provider.CommitSealedSessionTurnFinalization(ctx, replay)
		require.NoError(t, err, "a repeated finalization only compares the recorded native digest")
	})

	t.Run("a changed transcript rejects sealed native state", func(t *testing.T) {
		fixture := newTurnFixture(t, newProvider(t), "moved")
		ctx := t.Context()
		finalization := fixture.finalization("moved")
		finalization.NativeSessionDigest = controlDigest("moved native input")
		finalization.NativeSession = fixture.nativeFinalization(t, finalization)
		finalization.NativeSession.ExpectedMessageCount, finalization.NativeSession.ExpectedLastMessageID = 1, "a-message-the-transcript-lacks"
		_, err := fixture.provider.CommitSealedSessionTurnFinalization(ctx, finalization)
		require.ErrorIs(t, err, storagev1.ErrConflict)
		turn, err := fixture.provider.GetSessionTurn(ctx, fixture.turn.ID)
		require.NoError(t, err)
		require.Equal(t, storagev1.SessionTurnOpen, turn.State, "a rejected finalization changes nothing")
		basis := fixture.basis(t)
		require.Zero(t, basis.MessageCount)
		require.Nil(t, basis.NativeSession)

		boundary := fixture.finalization("wrong boundary")
		boundary.NativeSessionDigest = controlDigest("wrong boundary native input")
		boundary.NativeSession = fixture.nativeFinalization(t, boundary)
		boundary.NativeSession.Record.ThroughMessageID = "not-the-terminal-message"
		_, err = fixture.provider.CommitSealedSessionTurnFinalization(ctx, boundary)
		require.ErrorIs(t, err, storagev1.ErrConflict, "the record must cover the finalized transcript")
	})

	t.Run("a carried checkpoint must replace the record Orka read", func(t *testing.T) {
		fixture := newTurnFixture(t, newProvider(t), "carried")
		ctx := t.Context()
		stored := sealedNativeSession("carried", fixture.turn.Key.SessionUID, "checkpoint", 1)
		require.NoError(t, fixture.provider.SaveSealedNativeSession(ctx, storagev1.SaveSealedNativeSessionRequest{
			Record: stored, RequestDigest: controlDigest("checkpoint request"),
		}))
		finalization := fixture.finalization("carried")
		finalization.TerminalKind = storagev1.SessionTurnOutcomeMarker
		finalization.TerminalContent = `{"kind":"Failed"}`
		finalization.NativeSessionDigest = controlDigest("carried native input")
		carried := stored
		carried.RecordDigest = controlDigest("record at the advanced boundary")
		carried.Ciphertext = sealedBytes("the same checkpoint resealed at the advanced boundary")
		carried.MessageCount, carried.ThroughMessageID = 2, finalization.TerminalMessageID
		finalization.NativeSession = &storagev1.SealedNativeSessionFinalization{
			Record: carried, Carried: true, ExpectedRecordDigest: controlDigest("a record Orka never read"),
		}
		_, err := fixture.provider.CommitSealedSessionTurnFinalization(ctx, finalization)
		require.ErrorIs(t, err, storagev1.ErrConflict)

		finalization.NativeSession.ExpectedRecordDigest = stored.RecordDigest
		_, err = fixture.provider.CommitSealedSessionTurnFinalization(ctx, finalization)
		require.NoError(t, err)
		native, err := fixture.provider.GetSealedNativeSession(ctx, "tenant", "carried", fixture.turn.Key.SessionUID)
		require.NoError(t, err)
		requireSameSealedNativeSession(t, carried, native)
	})
}
