package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func TestNativeSessionSnapshotRequiresFinalizationAndActivationAcrossRestart(t *testing.T) {
	dbPath := t.TempDir() + "/native-session.db"
	open := func() *Store {
		db, err := NewDB(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return NewStore(db, dbPath)
	}
	s := open()
	ctx := context.Background()
	snapshot, request, historyBefore := nativeSessionFixture(t, s)
	for range 2 {
		if err := s.StageNativeSessionSnapshot(ctx, snapshot, historyBefore); err != nil {
			t.Fatal(err)
		}
	}
	requireNoNativeSessionSnapshot(t, s, snapshot)
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	s = open()
	requireNoNativeSessionSnapshot(t, s, snapshot)
	for range 2 {
		if _, err := s.CommitSessionTurnFinalization(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	requireNoNativeSessionSnapshot(t, s, snapshot)
	projection, err := s.GetOutboxProjection(ctx, request.Projection.ID)
	if err != nil || !projection.AvailableAt.Equal(deferredSessionTurnProjectionTime) {
		t.Fatalf("terminal projection was not deferred: %v", err)
	}
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	s = open()
	requireNoNativeSessionSnapshot(t, s, snapshot)
	activate := nativeSessionActivation(request)
	wrongActivation := activate
	wrongActivation.FinalizationDigest = controlTestDigest("wrong-finalization")
	if _, err := s.ActivateSessionTurnProjection(ctx, wrongActivation); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("mismatched activation error = %v", err)
	}
	requireNoNativeSessionSnapshot(t, s, snapshot)
	if _, err := s.ActivateSessionTurnProjection(ctx, activate); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	s = open()
	ready, err := s.GetNativeSessionSnapshot(ctx, snapshot.Namespace, snapshot.SessionName, snapshot.SessionUID)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := s.LoadTranscript(ctx, snapshot.Namespace, snapshot.SessionName, 0)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := store.NativeSessionHistoryDigest(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript) != 3 || ready.HistoryMessageCount != len(transcript) || ready.HistoryDigest != digest ||
		ready.ThroughMessageID != transcript[len(transcript)-1].ID || !bytes.Equal(ready.Data, snapshot.Data) ||
		ready.DataDigest != store.CanonicalBytesDigest(snapshot.Data) {
		t.Fatal("ready snapshot does not bind the committed transcript and provider bytes")
	}
	requireNativeSessionSnapshotPrivate(t, s, ready)
	// Selection happens after the next lease and turn are acquired. Retrying
	// the previous projection must leave its already-ready source available.
	nativeSessionNextTurn(t, s, snapshot, request)
	if _, err := s.ActivateSessionTurnProjection(ctx, activate); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetNativeSessionSnapshot(ctx, snapshot.Namespace, snapshot.SessionName, snapshot.SessionUID); err != nil {
		t.Fatalf("next open turn hid an activated source: %v", err)
	}
}

func TestNativeSessionSnapshotRejectsChangedStage(t *testing.T) {
	for _, change := range []struct {
		name   string
		mutate func(*store.NativeSessionSnapshot)
	}{
		{name: "bytes", mutate: func(s *store.NativeSessionSnapshot) { s.Data = []byte("changed provider state") }},
		{name: "provider", mutate: func(s *store.NativeSessionSnapshot) { s.ProviderSessionID = "different-session" }},
		{name: "profile", mutate: func(s *store.NativeSessionSnapshot) { s.ProfileDigest = controlTestDigest("different-profile") }},
		{name: "configuration", mutate: func(s *store.NativeSessionSnapshot) { s.ConfigurationDigest = controlTestDigest("different-config") }},
		{name: "workspace", mutate: func(s *store.NativeSessionSnapshot) {
			s.WorkspaceStateDigest = controlTestDigest("different-workspace")
		}},
		{name: "expiration", mutate: func(s *store.NativeSessionSnapshot) { s.ExpiresAt = s.ExpiresAt.Add(time.Minute) }},
	} {
		t.Run(change.name, func(t *testing.T) {
			s := setupTestStore(t)
			snapshot, _, history := nativeSessionFixture(t, s)
			if err := s.StageNativeSessionSnapshot(t.Context(), snapshot, history); err != nil {
				t.Fatal(err)
			}
			change.mutate(&snapshot)
			if err := s.StageNativeSessionSnapshot(t.Context(), snapshot, history); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("changed stage error = %v, want conflict", err)
			}
			requireNativeSessionRows(t, s, 1)
		})
	}
}

func TestNativeSessionSnapshotTransactionFailuresDoNotReleaseState(t *testing.T) {
	for _, boundary := range []string{"finalization", "activation"} {
		t.Run(boundary, func(t *testing.T) {
			s := setupTestStore(t)
			snapshot, request, history := nativeSessionFixture(t, s)
			if err := s.StageNativeSessionSnapshot(t.Context(), snapshot, history); err != nil {
				t.Fatal(err)
			}
			trigger := `CREATE TRIGGER fail_native_transaction BEFORE INSERT ON outbox_projections
				BEGIN SELECT RAISE(ABORT, 'injected finalization failure'); END`
			if boundary == "activation" {
				if _, err := s.CommitSessionTurnFinalization(t.Context(), request); err != nil {
					t.Fatal(err)
				}
				trigger = `CREATE TRIGGER fail_native_transaction BEFORE UPDATE OF state ON native_session_snapshots
					WHEN NEW.state = 'Ready' BEGIN SELECT RAISE(ABORT, 'injected activation failure'); END`
			}
			if _, err := s.db.ExecContext(t.Context(), trigger); err != nil {
				t.Fatal(err)
			}
			if boundary == "finalization" {
				if _, err := s.CommitSessionTurnFinalization(t.Context(), request); err == nil {
					t.Fatal("injected finalization failure did not abort")
				}
				transcript, err := s.LoadTranscript(t.Context(), snapshot.Namespace, snapshot.SessionName, 0)
				if err != nil || len(transcript) != 1 {
					t.Fatalf("failed finalization committed transcript messages: %v", err)
				}
			} else {
				if _, err := s.ActivateSessionTurnProjection(t.Context(), nativeSessionActivation(request)); err == nil {
					t.Fatal("injected activation failure did not abort")
				}
				projection, err := s.GetOutboxProjection(t.Context(), request.Projection.ID)
				if err != nil || !projection.AvailableAt.Equal(deferredSessionTurnProjectionTime) {
					t.Fatalf("failed activation released the terminal projection: %v", err)
				}
			}
			requireNoNativeSessionSnapshot(t, s, snapshot)
			if _, err := s.db.ExecContext(t.Context(), `DROP TRIGGER fail_native_transaction`); err != nil {
				t.Fatal(err)
			}
			finalizeAndActivateNativeSession(t, s, request)
			if _, err := s.GetNativeSessionSnapshot(t.Context(), snapshot.Namespace, snapshot.SessionName, snapshot.SessionUID); err != nil {
				t.Fatalf("transaction retry did not release finalized state: %v", err)
			}
		})
	}
}

func TestNativeSessionSnapshotBoundsAndDefaultedRetries(t *testing.T) {
	for _, change := range []struct {
		name   string
		mutate func(*store.NativeSessionSnapshot)
	}{
		{name: "empty", mutate: func(s *store.NativeSessionSnapshot) { s.Data = nil }},
		{name: "oversized", mutate: func(s *store.NativeSessionSnapshot) { s.Data = make([]byte, store.NativeSessionSnapshotMaxBytes+1) }},
		{name: "digest", mutate: func(s *store.NativeSessionSnapshot) { s.DataDigest = controlTestDigest("wrong-data") }},
		{name: "expired", mutate: func(s *store.NativeSessionSnapshot) { s.ExpiresAt = time.Now().Add(-time.Second) }},
		{name: "future creation", mutate: func(s *store.NativeSessionSnapshot) { s.CreatedAt = time.Now().Add(time.Minute) }},
		{name: "excessive TTL", mutate: func(s *store.NativeSessionSnapshot) {
			s.ExpiresAt = s.CreatedAt.Add(store.NativeSessionSnapshotMaxTTL + time.Second)
		}},
		{name: "invalid binding", mutate: func(s *store.NativeSessionSnapshot) { s.WorkspaceBindingDigest = "unknown" }},
		{name: "forged history", mutate: func(s *store.NativeSessionSnapshot) { s.HistoryDigest = controlTestDigest("uncommitted") }},
	} {
		t.Run(change.name, func(t *testing.T) {
			s := setupTestStore(t)
			snapshot, _, history := nativeSessionFixture(t, s)
			change.mutate(&snapshot)
			if err := s.StageNativeSessionSnapshot(t.Context(), snapshot, history); !errors.Is(err, store.ErrValidation) {
				t.Fatalf("invalid snapshot error = %v, want validation", err)
			}
			requireNativeSessionRows(t, s, 0)
		})
	}
	t.Run("maximum bytes and default timestamps", func(t *testing.T) {
		s := setupTestStore(t)
		snapshot, _, history := nativeSessionFixture(t, s)
		snapshot.CreatedAt, snapshot.ExpiresAt = time.Time{}, time.Time{}
		snapshot.Data = make([]byte, store.NativeSessionSnapshotMaxBytes)
		for range 2 {
			if err := s.StageNativeSessionSnapshot(t.Context(), snapshot, history); err != nil {
				t.Fatal(err)
			}
		}
		requireNativeSessionRows(t, s, 1)
	})
}

func TestNativeSessionSnapshotStageRequiresCurrentSessionAndTurn(t *testing.T) {
	for _, mismatch := range []string{"unbound UID", "replacement UID", "namespace UID", "turn namespace", "newer turn", "same generation", "history", "cleanup intent", "cleanup completion", "finalized turn"} {
		t.Run(mismatch, func(t *testing.T) {
			s := setupTestStore(t)
			snapshot, request, history := nativeSessionFixture(t, s)
			ctx := t.Context()
			var err error
			switch mismatch {
			case "unbound UID":
				_, err = s.db.ExecContext(ctx, `UPDATE sessions SET control_session_uid = ''`)
			case "replacement UID":
				_, err = s.db.ExecContext(ctx, `UPDATE sessions SET control_session_uid = 'replacement'`)
			case "namespace UID":
				snapshot.NamespaceUID = "replacement-namespace"
			case "turn namespace":
				snapshot.Namespace = "other"
				err = s.CreateSession(ctx, &store.SessionRecord{Namespace: snapshot.Namespace, Name: snapshot.SessionName, SessionType: "task"})
				if err == nil {
					err = s.BindSessionCleanupIdentity(ctx, snapshot.Namespace, snapshot.SessionName, snapshot.SessionUID)
				}
			case "newer turn":
				nativeSessionNextTurn(t, s, snapshot, request)
			case "same generation":
				competingKey := snapshot.Key
				competingKey.TaskUID = "competing-task"
				_, err = s.CreateSessionTurnRecord(ctx, store.CreateSessionTurnRecordRequest{
					Namespace: snapshot.Namespace, SessionName: snapshot.SessionName, Fence: request.Fence,
					Turn: store.SessionTurn{Key: competingKey, PromptAttemptID: "competing-attempt", RequestDigest: controlTestDigest("competing"), UserPrompt: "competing prompt"},
				})
			case "history":
				history = controlTestDigest("wrong transcript")
			case "cleanup intent":
				_, err = s.db.ExecContext(ctx, `INSERT INTO session_cleanup_intents
					(namespace, session_name, operation_id, operation_digest, plan, created_at) VALUES (?, ?, 'delete', ?, '{}', ?)`,
					snapshot.Namespace, snapshot.SessionName, controlTestDigest("delete"), time.Now())
			case "cleanup completion":
				_, err = s.db.ExecContext(ctx, `INSERT INTO session_cleanup_completions
					(namespace, session_name, session_uid, operation_id, operation_digest, completed_at) VALUES (?, ?, ?, 'delete', ?, ?)`,
					snapshot.Namespace, snapshot.SessionName, snapshot.SessionUID, controlTestDigest("delete"), time.Now())
			case "finalized turn":
				_, err = s.CommitSessionTurnFinalization(ctx, request)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := s.StageNativeSessionSnapshot(ctx, snapshot, history); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("stale or mismatched stage error = %v, want conflict", err)
			}
			requireNativeSessionRows(t, s, 0)
		})
	}
}

func TestNativeSessionSnapshotInvalidFinalizationDiscardsCandidate(t *testing.T) {
	for _, mismatch := range []string{"append disabled", "outcome marker", "publication", "changed prefix", "newer turn", "changed UID"} {
		t.Run(mismatch, func(t *testing.T) {
			s := setupTestStore(t)
			snapshot, request, history := nativeSessionFixture(t, s)
			if err := s.StageNativeSessionSnapshot(t.Context(), snapshot, history); err != nil {
				t.Fatal(err)
			}
			switch mismatch {
			case "append disabled":
				request.SkipTranscriptAppend = true
			case "outcome marker":
				request.TerminalKind, request.TerminalContent = store.SessionTurnOutcomeMarker, `{"kind":"Failed"}`
			case "publication":
				request.PublicationID = "publication"
				request.PublicationReceipt = &store.PublicationReceipt{PublicationID: request.PublicationID}
			case "changed prefix":
				appendNativeSessionRacingMessage(t, s, snapshot)
			case "newer turn":
				nativeSessionNextTurn(t, s, snapshot, request)
			case "changed UID":
				if _, err := s.db.ExecContext(t.Context(), `UPDATE sessions SET control_session_uid = 'replacement'`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.CommitSessionTurnFinalization(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ActivateSessionTurnProjection(t.Context(), nativeSessionActivation(request)); err != nil {
				t.Fatal(err)
			}
			requireNoNativeSessionSnapshot(t, s, snapshot)
			requireNativeSessionRows(t, s, 0)
		})
	}
}

func TestNativeSessionSnapshotActivationRejectsNewerTurnAndHistory(t *testing.T) {
	for _, mismatch := range []string{"newer open turn", "newer finalized turn", "changed history", "namespace replacement"} {
		t.Run(mismatch, func(t *testing.T) {
			s := setupTestStore(t)
			snapshot, request, history := nativeSessionFixture(t, s)
			if err := s.StageNativeSessionSnapshot(t.Context(), snapshot, history); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CommitSessionTurnFinalization(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			switch mismatch {
			case "newer open turn", "newer finalized turn":
				_, next := nativeSessionNextTurn(t, s, snapshot, request)
				if mismatch == "newer finalized turn" {
					if _, err := s.CommitSessionTurnFinalization(t.Context(), next); err != nil {
						t.Fatal(err)
					}
				}
			case "changed history":
				appendNativeSessionRacingMessage(t, s, snapshot)
			case "namespace replacement":
				if _, err := s.db.ExecContext(t.Context(), `UPDATE session_lineages SET namespace_uid = 'replacement'`); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if _, err := s.ActivateSessionTurnProjection(t.Context(), nativeSessionActivation(request)); err != nil {
					t.Fatal(err)
				}
			}
			requireNoNativeSessionSnapshot(t, s, snapshot)
			requireNativeSessionRows(t, s, 0)
		})
	}
}

func TestNativeSessionSnapshotReplacementKeepsOnePrivateRecord(t *testing.T) {
	s := setupTestStore(t)
	snapshot, request, history := nativeSessionFixture(t, s)
	for generation := 1; generation <= 3; generation++ {
		if err := s.StageNativeSessionSnapshot(t.Context(), snapshot, history); err != nil {
			t.Fatal(err)
		}
		requireNoNativeSessionSnapshot(t, s, snapshot)
		requireNativeSessionRows(t, s, 1)
		finalizeAndActivateNativeSession(t, s, request)
		ready, err := s.GetNativeSessionSnapshot(t.Context(), snapshot.Namespace, snapshot.SessionName, snapshot.SessionUID)
		if err != nil || ready.Key.LeaseGeneration != int64(generation) || !bytes.Equal(ready.Data, snapshot.Data) {
			t.Fatalf("replacement snapshot did not advance: %v", err)
		}
		if generation != 3 {
			snapshot, request = nativeSessionNextTurn(t, s, snapshot, request)
			history = ready.HistoryDigest
		}
	}
	for _, identity := range []struct{ namespace, name, uid string }{
		{namespace: "other", name: snapshot.SessionName, uid: snapshot.SessionUID},
		{namespace: snapshot.Namespace, name: "other", uid: snapshot.SessionUID},
		{namespace: snapshot.Namespace, name: snapshot.SessionName, uid: "replacement"},
	} {
		if _, err := s.GetNativeSessionSnapshot(t.Context(), identity.namespace, identity.name, identity.uid); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("different identity snapshot error = %v", err)
		}
	}
	requireNativeSessionRows(t, s, 1)
}

func TestNativeSessionSnapshotExpiredReadDeletesPrivateBytes(t *testing.T) {
	s := setupTestStore(t)
	snapshot, request, history := nativeSessionFixture(t, s)
	if err := s.StageNativeSessionSnapshot(t.Context(), snapshot, history); err != nil {
		t.Fatal(err)
	}
	finalizeAndActivateNativeSession(t, s, request)
	if _, err := s.db.ExecContext(t.Context(), `UPDATE native_session_snapshots SET expires_at = ?`, time.Now().UTC().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	requireNoNativeSessionSnapshot(t, s, snapshot)
	requireNativeSessionRows(t, s, 0)
}

func TestNativeSessionSnapshotMaintenanceDeletesIdleExpiredCopies(t *testing.T) {
	for _, test := range []struct {
		name               string
		ready              bool
		expiredBeforeStart bool
	}{
		{name: "staged copy expires while idle"},
		{name: "ready copy expires while idle", ready: true},
		{name: "expired copy after downtime", ready: true, expiredBeforeStart: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := setupTestStore(t)
				snapshot, request, history := nativeSessionFixture(t, s)
				snapshot.ExpiresAt = time.Now().UTC().Add(90 * time.Second)
				if err := s.StageNativeSessionSnapshot(t.Context(), snapshot, history); err != nil {
					t.Fatal(err)
				}
				if test.ready {
					finalizeAndActivateNativeSession(t, s, request)
				}
				if test.expiredBeforeStart {
					time.Sleep(2 * time.Minute)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- s.Start(ctx) }()
				synctest.Wait()
				if !test.expiredBeforeStart {
					requireNativeSessionRows(t, s, 1)
					time.Sleep(time.Minute)
					synctest.Wait()
					requireNativeSessionRows(t, s, 1)
					time.Sleep(time.Minute)
					synctest.Wait()
				}
				// Query the private table directly: a snapshot read would perform
				// its own expiry cleanup and hide a missing maintenance tick.
				requireNativeSessionRows(t, s, 0)
				if _, err := s.GetSession(t.Context(), snapshot.Namespace, snapshot.SessionName); err != nil {
					t.Fatalf("snapshot expiry removed its Session: %v", err)
				}
				cancel()
				if err := <-done; err != nil {
					t.Fatalf("maintenance shutdown: %v", err)
				}
			})
		})
	}
}

func TestNativeSessionSnapshotMaintenanceDoesNotWriteAfterCancellation(t *testing.T) {
	dbPath := t.TempDir() + "/native-session.db"
	open := func() *Store {
		db, err := NewDB(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return NewStore(db, dbPath)
	}
	s := open()
	snapshot, _, history := nativeSessionFixture(t, s)
	if err := s.StageNativeSessionSnapshot(t.Context(), snapshot, history); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), `UPDATE native_session_snapshots SET expires_at = ?`, time.Now().UTC().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("cancelled maintenance startup: %v", err)
	}
	requireNativeSessionRows(t, open(), 1)
}

func TestNativeSessionSnapshotSessionDeletionRejectsLateStage(t *testing.T) {
	for _, coordinated := range []bool{false, true} {
		t.Run(fmt.Sprintf("coordinated=%v", coordinated), func(t *testing.T) {
			s := setupTestStore(t)
			snapshot, request, history := nativeSessionFixture(t, s)
			if err := s.StageNativeSessionSnapshot(t.Context(), snapshot, history); err != nil {
				t.Fatal(err)
			}
			finalizeAndActivateNativeSession(t, s, request)
			deliverNativeSessionProjection(t, s, request)
			if coordinated {
				intent := store.SessionCleanupIntent{
					Namespace: snapshot.Namespace, SessionName: snapshot.SessionName, SessionUID: snapshot.SessionUID,
					OperationID: "delete-native-session", OperationDigest: controlTestDigest("delete-native-session"), PreparedAt: time.Now().UTC(),
				}
				if _, err := s.PrepareSessionCleanup(t.Context(), intent); err != nil {
					t.Fatal(err)
				}
				if err := s.StageNativeSessionSnapshot(t.Context(), snapshot, history); !errors.Is(err, store.ErrConflict) {
					t.Fatalf("stage during deletion error = %v", err)
				}
				if err := s.CompleteSessionCleanup(t.Context(), store.CompleteSessionCleanupRequest{
					Namespace: intent.Namespace, SessionName: intent.SessionName, OperationID: intent.OperationID, OperationDigest: intent.OperationDigest,
				}); err != nil {
					t.Fatal(err)
				}
			} else if err := s.DeleteSession(t.Context(), snapshot.Namespace, snapshot.SessionName); err != nil {
				t.Fatal(err)
			}
			requireNativeSessionRows(t, s, 0)
			if err := s.StageNativeSessionSnapshot(t.Context(), snapshot, history); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("late stage after deletion error = %v", err)
			}
			requireNoNativeSessionSnapshot(t, s, snapshot)
			requireNativeSessionRows(t, s, 0)
		})
	}
}

func nativeSessionFixture(t *testing.T, s *Store) (store.NativeSessionSnapshot, store.CommitSessionTurnFinalizationRequest, string) {
	t.Helper()
	base := sessionTurnFinalizationOwnershipFixture(t, s, true, "native-task")
	ctx := t.Context()
	if err := s.BindSessionCleanupIdentity(ctx, "ns", "session", base.Key.SessionUID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	if _, err := s.ProjectSessionLineage(ctx, store.SessionLineage{
		Namespace: "ns", SessionName: "session", SessionUID: base.Key.SessionUID, NamespaceUID: "namespace-uid",
		ContractVersion: "orka.harness.v2", LineageGeneration: 1, RuntimeIdentity: "opencode", ConfigDigest: controlTestDigest("config"),
		Version: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessages(ctx, "ns", "session", []store.SessionMessage{{ID: "previous-message", Role: "assistant", Content: "Previous response"}}); err != nil {
		t.Fatal(err)
	}
	history, err := s.LoadTranscript(ctx, "ns", "session", 0)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := store.NativeSessionHistoryDigest(history)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := store.NativeSessionSnapshot{
		Namespace: "ns", SessionName: "session", SessionUID: base.Key.SessionUID, NamespaceUID: "namespace-uid", Key: base.Key,
		ProviderSessionID: "provider-session-private", ProviderVersion: "1.18.9", ProfileDigest: controlTestDigest("profile"),
		ConfigurationDigest: controlTestDigest("config"), WorkspaceBindingDigest: controlTestDigest("workspace-binding"),
		WorkspaceDigest: controlTestDigest("workspace"), WorkspaceStateDigest: controlTestDigest("workspace-state"),
		WorkingDirectory: "/workspace", Data: []byte("private-native-snapshot-bytes"), CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	request := store.CommitSessionTurnFinalizationRequest{
		Key: base.Key, Namespace: snapshot.Namespace, SessionName: snapshot.SessionName, Fence: base.Fence,
		ExpectedTurnVersion: base.ExpectedTurnVersion, FinalizationDigest: base.FinalizationDigest,
		TerminalKind: base.TerminalKind, TerminalContent: base.TerminalContent, Projection: base.Projection, FinalizedAt: base.FinalizedAt,
	}
	request.Projection.ProjectionKind = "TaskTerminalStatus"
	request.Projection.ID = store.CanonicalControlID("outbox", request.Projection.AggregateID, request.Projection.ProjectionKind)
	return snapshot, request, digest
}

func nativeSessionActivation(request store.CommitSessionTurnFinalizationRequest) store.ActivateSessionTurnProjectionRequest {
	return store.ActivateSessionTurnProjectionRequest{
		TurnID: request.Projection.AggregateID, ProjectionID: request.Projection.ID, Fence: request.Fence,
		FinalizationDigest: request.FinalizationDigest, ExpectedAggregateKind: request.Projection.AggregateKind,
		ExpectedProjectionKind: request.Projection.ProjectionKind, ExpectedPayloadDigest: request.Projection.PayloadDigest,
		AvailableAt: request.FinalizedAt, UpdatedAt: request.FinalizedAt,
	}
}

func finalizeAndActivateNativeSession(t *testing.T, s *Store, request store.CommitSessionTurnFinalizationRequest) {
	t.Helper()
	if _, err := s.CommitSessionTurnFinalization(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateSessionTurnProjection(t.Context(), nativeSessionActivation(request)); err != nil {
		t.Fatal(err)
	}
}

func nativeSessionNextTurn(
	t *testing.T, s *Store, snapshot store.NativeSessionSnapshot, previous store.CommitSessionTurnFinalizationRequest,
) (store.NativeSessionSnapshot, store.CommitSessionTurnFinalizationRequest) {
	t.Helper()
	snapshot.Key.LeaseGeneration++
	snapshot.Key.TaskUID += "-next"
	snapshot.Key.PromptID += "-next"
	snapshot.ID = ""
	snapshot.Data = []byte(fmt.Sprintf("private-provider-state-%d", snapshot.Key.LeaseGeneration))
	snapshot.DataDigest = ""
	turn, err := s.CreateSessionTurnRecord(t.Context(), store.CreateSessionTurnRecordRequest{
		Namespace: snapshot.Namespace, SessionName: snapshot.SessionName, Fence: previous.Fence,
		Turn: store.SessionTurn{Key: snapshot.Key, PromptAttemptID: "next-attempt", RequestDigest: controlTestDigest(snapshot.Key.TaskUID), UserPrompt: "next prompt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot.ID = turn.ID
	request := previous
	request.Key, request.ExpectedTurnVersion = turn.Key, turn.Version
	request.Projection.AggregateID = turn.ID
	request.Projection.ID = store.CanonicalControlID("outbox", turn.ID, request.Projection.ProjectionKind)
	request.FinalizationDigest = controlTestDigest("finalization-" + snapshot.Key.TaskUID)
	return snapshot, request
}

func appendNativeSessionRacingMessage(t *testing.T, s *Store, snapshot store.NativeSessionSnapshot) {
	t.Helper()
	if err := s.AppendMessages(t.Context(), snapshot.Namespace, snapshot.SessionName, []store.SessionMessage{{
		ID: "racing-message", Role: "user", Content: "Concurrent transcript append",
	}}); err != nil {
		t.Fatal(err)
	}
}

func requireNoNativeSessionSnapshot(t *testing.T, s *Store, snapshot store.NativeSessionSnapshot) {
	t.Helper()
	if _, err := s.GetNativeSessionSnapshot(t.Context(), snapshot.Namespace, snapshot.SessionName, snapshot.SessionUID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetNativeSessionSnapshot() error = %v, want not found", err)
	}
}

func requireNativeSessionSnapshotPrivate(t *testing.T, s *Store, snapshot *store.NativeSessionSnapshot) {
	t.Helper()
	encoded, err := json.Marshal(snapshot)
	if err != nil || bytes.Contains(encoded, snapshot.Data) || bytes.Contains(encoded, []byte(`"data":`)) {
		t.Fatalf("snapshot JSON must omit provider bytes: %v", err)
	}
	session, err := s.GetSession(t.Context(), snapshot.Namespace, snapshot.SessionName)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(session)
	if err != nil || bytes.Contains(encoded, snapshot.Data) || bytes.Contains(encoded, []byte(snapshot.ProviderSessionID)) {
		t.Fatalf("public Session record exposed private native state: %v", err)
	}
}

func requireNativeSessionRows(t *testing.T, s *Store, want int) {
	t.Helper()
	var count int
	if err := s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM native_session_snapshots`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("native snapshot row count = %d, want %d", count, want)
	}
}

func deliverNativeSessionProjection(t *testing.T, s *Store, request store.CommitSessionTurnFinalizationRequest) {
	t.Helper()
	projections, err := s.ClaimOutboxProjectionRecords(t.Context(), store.ClaimOutboxProjectionsRequest{
		Fence: request.Fence, WorkerID: "native-test-worker", LeaseDuration: time.Minute, Now: time.Now().UTC(),
	})
	if err != nil || len(projections) != 1 {
		t.Fatalf("claim terminal projection: count=%d error=%v", len(projections), err)
	}
	if _, err := s.CompleteOutboxProjectionRecord(t.Context(), store.CompleteOutboxProjectionRequest{
		ID: projections[0].ID, Fence: request.Fence, ExpectedVersion: projections[0].Version, LeaseOwner: "native-test-worker",
		OperationID: "deliver-native-test", OperationDigest: controlTestDigest("deliver-native-test"),
		NewState: store.OutboxProjectionDelivered, DeliveryDigest: controlTestDigest("delivery"), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}
