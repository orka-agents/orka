/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/store"
)

var (
	_ store.AgentExecutionSnapshotStore = (*Store)(nil)
	_ store.SessionLineageStore         = (*Store)(nil)
	_ store.HarnessV1AttemptStore       = (*Store)(nil)
)

func newCoexistenceTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "coexistence.db")
	db, err := NewDB(dbPath)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewStore(db, dbPath)
}

func testSnapshotCipher(t *testing.T) *AgentExecutionSnapshotCipher {
	t.Helper()
	cipher, err := NewAgentExecutionSnapshotCipher(bytes.Repeat([]byte{0x42}, AgentExecutionSnapshotKeyBytes))
	if err != nil {
		t.Fatalf("NewAgentExecutionSnapshotCipher: %v", err)
	}
	return cipher
}

func TestAgentExecutionSnapshotFailsClosedWithoutCipher(t *testing.T) {
	ctx := context.Background()
	s := newCoexistenceTestStore(t)
	body := []byte(`{"prompt":"resolved"}`)
	snapshot := store.AgentExecutionSnapshot{
		TaskUID:       "task-uid-1",
		Digest:        store.CanonicalAgentExecutionSnapshotDigest(body),
		SchemaVersion: store.AgentExecutionSnapshotSchemaVersion,
		Body:          body,
	}
	if err := s.PersistAgentExecutionSnapshot(ctx, snapshot); err == nil {
		t.Fatal("persist without a cipher must fail closed")
	}
	if _, err := s.GetAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshotKey{TaskUID: "task-uid-1", Digest: snapshot.Digest}); err == nil {
		t.Fatal("get without a cipher must fail closed")
	}
}

func TestAgentExecutionSnapshotRoundTripEncryptedAtRest(t *testing.T) {
	ctx := context.Background()
	s := newCoexistenceTestStore(t)
	s.SetAgentExecutionSnapshotCipher(testSnapshotCipher(t))

	body := []byte(`{"prompt":"SENSITIVE-RESOLVED-PROMPT","model":"provider/model"}`)
	snapshot := store.AgentExecutionSnapshot{
		TaskUID:       "task-uid-1",
		Digest:        store.CanonicalAgentExecutionSnapshotDigest(body),
		SchemaVersion: store.AgentExecutionSnapshotSchemaVersion,
		Body:          body,
	}
	if err := s.PersistAgentExecutionSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("persist snapshot: %v", err)
	}
	// Idempotent identical persist.
	if err := s.PersistAgentExecutionSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("idempotent persist: %v", err)
	}
	// Same key with different content is rejected.
	different := snapshot
	different.SchemaVersion = snapshot.SchemaVersion + 1
	if err := s.PersistAgentExecutionSnapshot(ctx, different); !errors.Is(err, store.ErrDuplicateMismatch) {
		t.Fatalf("expected ErrDuplicateMismatch, got %v", err)
	}

	got, err := s.GetAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshotKey{TaskUID: "task-uid-1", Digest: snapshot.Digest})
	if err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if !bytes.Equal(got.Body, body) || got.SchemaVersion != snapshot.SchemaVersion {
		t.Fatalf("snapshot round trip mismatch: %+v", got)
	}

	// The stored bytes must not contain the plaintext.
	var ciphertext []byte
	if err := s.db.QueryRow(`SELECT ciphertext FROM agent_execution_snapshots WHERE task_uid = ?`, "task-uid-1").Scan(&ciphertext); err != nil {
		t.Fatalf("read raw ciphertext: %v", err)
	}
	if bytes.Contains(ciphertext, []byte("SENSITIVE-RESOLVED-PROMPT")) {
		t.Fatal("snapshot body is stored in plaintext")
	}

	// Digest mismatch on persist is rejected.
	bad := snapshot
	bad.Digest = store.CanonicalAgentExecutionSnapshotDigest([]byte("other"))
	if err := s.PersistAgentExecutionSnapshot(ctx, bad); err == nil {
		t.Fatal("digest/body mismatch must be rejected")
	}

	keys, err := s.ListAgentExecutionSnapshotKeys(ctx, "task-uid-1")
	if err != nil || len(keys) != 1 || keys[0].Digest != snapshot.Digest {
		t.Fatalf("list snapshot keys = %v, %v", keys, err)
	}
	if err := s.DeleteAgentExecutionSnapshots(ctx, "task-uid-1"); err != nil {
		t.Fatalf("delete snapshots: %v", err)
	}
	if _, err := s.GetAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshotKey{TaskUID: "task-uid-1", Digest: snapshot.Digest}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestSessionLineageClaimEstablishVerifyAndConflict(t *testing.T) {
	ctx := context.Background()
	s := newCoexistenceTestStore(t)

	claim := store.ClaimSessionLineageRequest{
		Namespace:         "ns",
		SessionName:       "chat",
		NamespaceUID:      "ns-uid",
		SessionUID:        "session-uid",
		ContractVersion:   "orka.harness.v2",
		RuntimeIdentity:   "codex",
		ConfigDigest:      store.CanonicalAgentExecutionSnapshotDigest([]byte("cfg")),
		Provenance:        store.SessionLineageFirstUse,
		EstablishIfAbsent: true,
	}

	// A pre-existing session without lineage is never silently claimed.
	verifyOnly := claim
	verifyOnly.EstablishIfAbsent = false
	if _, err := s.ClaimSessionLineage(ctx, verifyOnly); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for verify-only claim, got %v", err)
	}

	lineage, err := s.ClaimSessionLineage(ctx, claim)
	if err != nil {
		t.Fatalf("establish lineage: %v", err)
	}
	if lineage.LineageGeneration != 1 || lineage.Version != 1 {
		t.Fatalf("unexpected new lineage: %+v", lineage)
	}

	// An identical claim verifies idempotently.
	again, err := s.ClaimSessionLineage(ctx, claim)
	if err != nil {
		t.Fatalf("verify lineage: %v", err)
	}
	if again.SessionUID != claim.SessionUID {
		t.Fatalf("verified lineage mismatch: %+v", again)
	}

	// Cross-protocol continuation is rejected.
	crossProtocol := claim
	crossProtocol.ContractVersion = "orka.harness.v1"
	crossProtocol.RuntimeIdentity = "opencode"
	if _, err := s.ClaimSessionLineage(ctx, crossProtocol); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict for cross-protocol claim, got %v", err)
	}

	// A recreated same-name Session (new UID) never attaches to old state.
	recreated := claim
	recreated.SessionUID = "different-session-uid"
	if _, err := s.ClaimSessionLineage(ctx, recreated); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict for recreated session UID, got %v", err)
	}

	// A recreated same-name namespace never attaches to old state.
	recreatedNamespace := claim
	recreatedNamespace.NamespaceUID = "different-ns-uid"
	if _, err := s.ClaimSessionLineage(ctx, recreatedNamespace); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict for recreated namespace UID, got %v", err)
	}

	if err := s.DeleteSessionLineage(ctx, "ns", "chat"); err != nil {
		t.Fatalf("delete lineage: %v", err)
	}
	if _, err := s.GetSessionLineage(ctx, "ns", "chat"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func testHarnessV1Attempt() *store.HarnessV1Attempt {
	digest := store.CanonicalAgentExecutionSnapshotDigest
	return &store.HarnessV1Attempt{
		Namespace:      "ns",
		TaskName:       "task-1",
		TaskUID:        "task-uid-1",
		Attempt:        1,
		BindingDigest:  digest([]byte("binding")),
		SnapshotDigest: digest([]byte("snapshot")),
		RequestDigest:  digest([]byte("request")),
		TurnID:         "turn-1",
		Backend:        "harness-wrapper",
		State:          store.HarnessV1AttemptPrepared,
		RetryClass:     store.HarnessV1RetryClassNone,
	}
}

func harnessV1Transition(key store.HarnessV1AttemptKey, version int64, from, to store.HarnessV1AttemptState, op string) store.HarnessV1AttemptTransition {
	return store.HarnessV1AttemptTransition{
		Key:             key,
		ExpectedVersion: version,
		ExpectedState:   from,
		TargetState:     to,
		OperationID:     op,
		OperationDigest: store.CanonicalAgentExecutionSnapshotDigest([]byte(op)),
	}
}

func TestHarnessV1AttemptLifecycleAndCAS(t *testing.T) {
	ctx := context.Background()
	s := newCoexistenceTestStore(t)
	attempt := testHarnessV1Attempt()
	key := store.HarnessV1AttemptKey{Namespace: attempt.Namespace, TaskUID: attempt.TaskUID, Attempt: attempt.Attempt}

	if err := s.CreateHarnessV1Attempt(ctx, attempt); err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	if err := s.CreateHarnessV1Attempt(ctx, attempt); err != nil {
		t.Fatalf("idempotent create: %v", err)
	}
	different := *attempt
	different.RequestDigest = store.CanonicalAgentExecutionSnapshotDigest([]byte("other-request"))
	if err := s.CreateHarnessV1Attempt(ctx, &different); !errors.Is(err, store.ErrDuplicateMismatch) {
		t.Fatalf("expected ErrDuplicateMismatch, got %v", err)
	}

	// Persist Submitting before writing StartTurn.
	current, err := s.TransitionHarnessV1Attempt(ctx, harnessV1Transition(key, 1, store.HarnessV1AttemptPrepared, store.HarnessV1AttemptSubmitting, "submit"))
	if err != nil {
		t.Fatalf("Prepared->Submitting: %v", err)
	}
	// Replay of the same operation is idempotent.
	replay, err := s.TransitionHarnessV1Attempt(ctx, harnessV1Transition(key, 1, store.HarnessV1AttemptPrepared, store.HarnessV1AttemptSubmitting, "submit"))
	if err != nil || replay.Version != current.Version {
		t.Fatalf("replay transition = %+v, %v", replay, err)
	}
	// Same operation ID with a different digest is rejected.
	conflicting := harnessV1Transition(key, 1, store.HarnessV1AttemptPrepared, store.HarnessV1AttemptSubmitting, "submit")
	conflicting.OperationDigest = store.CanonicalAgentExecutionSnapshotDigest([]byte("tampered"))
	if _, err := s.TransitionHarnessV1Attempt(ctx, conflicting); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict for tampered replay, got %v", err)
	}

	// Stale version CAS fails.
	if _, err := s.TransitionHarnessV1Attempt(ctx, harnessV1Transition(key, 1, store.HarnessV1AttemptSubmitting, store.HarnessV1AttemptAccepted, "accept-stale")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict for stale version, got %v", err)
	}

	// Illegal transition fails.
	if _, err := s.TransitionHarnessV1Attempt(ctx, harnessV1Transition(key, current.Version, store.HarnessV1AttemptSubmitting, store.HarnessV1AttemptSucceeded, "skip")); err == nil {
		t.Fatal("Submitting->Succeeded must be rejected")
	}

	sessionID := "runtime-session-1"
	acceptTransition := harnessV1Transition(key, current.Version, store.HarnessV1AttemptSubmitting, store.HarnessV1AttemptAccepted, "accept")
	acceptTransition.Updates.RuntimeSessionID = &sessionID
	current, err = s.TransitionHarnessV1Attempt(ctx, acceptTransition)
	if err != nil || current.RuntimeSessionID != sessionID {
		t.Fatalf("Submitting->Accepted = %+v, %v", current, err)
	}

	active, err := s.ListActiveHarnessV1Attempts(ctx)
	if err != nil || len(active) != 1 {
		t.Fatalf("active attempts = %v, %v", active, err)
	}

	// OutcomeUnknown requires a terminal reason.
	if _, err := s.TransitionHarnessV1Attempt(ctx, harnessV1Transition(key, current.Version, store.HarnessV1AttemptAccepted, store.HarnessV1AttemptOutcomeUnknown, "unknown-no-reason")); err == nil {
		t.Fatal("OutcomeUnknown without a reason must be rejected")
	}

	reason := "RuntimeLost"
	unknownTransition := harnessV1Transition(key, current.Version, store.HarnessV1AttemptAccepted, store.HarnessV1AttemptOutcomeUnknown, "unknown")
	unknownTransition.Updates.TerminalReason = &reason
	current, err = s.TransitionHarnessV1Attempt(ctx, unknownTransition)
	if err != nil || current.State != store.HarnessV1AttemptOutcomeUnknown {
		t.Fatalf("Accepted->OutcomeUnknown = %+v, %v", current, err)
	}

	// Terminal states admit no further transitions.
	if _, err := s.TransitionHarnessV1Attempt(ctx, harnessV1Transition(key, current.Version, store.HarnessV1AttemptOutcomeUnknown, store.HarnessV1AttemptSucceeded, "resurrect")); err == nil {
		t.Fatal("OutcomeUnknown is terminal and must not transition")
	}

	active, err = s.ListActiveHarnessV1Attempts(ctx)
	if err != nil || len(active) != 0 {
		t.Fatalf("active attempts after terminal = %v, %v", active, err)
	}

	attempts, err := s.ListHarnessV1AttemptsByTask(ctx, "ns", "task-uid-1")
	if err != nil || len(attempts) != 1 || !strings.Contains(string(attempts[0].State), "OutcomeUnknown") {
		t.Fatalf("attempts by task = %v, %v", attempts, err)
	}
}

func TestHarnessV1AttemptSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "coexistence-restart.db")
	db, err := NewDB(dbPath)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	s := NewStore(db, dbPath)
	attempt := testHarnessV1Attempt()
	if err := s.CreateHarnessV1Attempt(ctx, attempt); err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	key := store.HarnessV1AttemptKey{Namespace: attempt.Namespace, TaskUID: attempt.TaskUID, Attempt: attempt.Attempt}
	if _, err := s.TransitionHarnessV1Attempt(ctx, harnessV1Transition(key, 1, store.HarnessV1AttemptPrepared, store.HarnessV1AttemptSubmitting, "submit")); err != nil {
		t.Fatalf("transition: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	reopened, err := NewDB(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restarted := NewStore(reopened, dbPath)
	got, err := restarted.GetHarnessV1Attempt(ctx, key)
	if err != nil || got.State != store.HarnessV1AttemptSubmitting || got.Version != 2 {
		t.Fatalf("attempt after restart = %+v, %v", got, err)
	}
}
