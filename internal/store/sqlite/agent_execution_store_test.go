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
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

var (
	_ store.AgentExecutionSnapshotStore          = (*Store)(nil)
	_ store.AgentExecutionSnapshotLifecycleStore = (*Store)(nil)
	_ store.SessionLineageStore                  = (*Store)(nil)
)

func newAgentExecutionTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "agent-execution.db")
	db, err := NewDB(dbPath)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewStore(db, dbPath)
}

func TestAgentExecutionFreshDatabaseHasNoHarnessV1Tables(t *testing.T) {
	s := newAgentExecutionTestStore(t)
	for _, table := range []string{"harness_v1_attempts", "runtime_sessions"} {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("fresh database created unsupported table %q", table)
		}
	}
	counts, err := s.CountAgentExecutionSnapshotReferences(context.Background(), store.AgentExecutionSnapshotKey{
		TaskUID: "task-uid", Digest: store.CanonicalAgentExecutionSnapshotDigest([]byte("snapshot")),
	})
	if err != nil {
		t.Fatalf("count snapshot references without v1 tables: %v", err)
	}
	if counts.Total() != 0 {
		t.Fatalf("fresh database reference counts = %#v", counts)
	}
}

func testSnapshotCipher(t *testing.T) *AgentExecutionSnapshotCipher {
	t.Helper()
	cipher, err := NewAgentExecutionSnapshotCipher(bytes.Repeat([]byte{0x42}, AgentExecutionSnapshotKeyBytes))
	if err != nil {
		t.Fatalf("NewAgentExecutionSnapshotCipher: %v", err)
	}
	return cipher
}

func persistLifecycleSnapshot(
	t *testing.T,
	ctx context.Context,
	s *Store,
	taskUID string,
	body []byte,
	createdAt time.Time,
) store.AgentExecutionSnapshotKey {
	t.Helper()
	key := store.AgentExecutionSnapshotKey{
		TaskUID: taskUID,
		Digest:  store.CanonicalAgentExecutionSnapshotDigest(body),
	}
	if err := s.PersistAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshot{
		TaskUID:       key.TaskUID,
		Digest:        key.Digest,
		SchemaVersion: store.AgentExecutionSnapshotSchemaVersion,
		Body:          body,
		CreatedAt:     createdAt,
	}); err != nil {
		t.Fatalf("persist lifecycle snapshot %s: %v", key.ID(), err)
	}
	return key
}

func TestAgentExecutionSnapshotFailsClosedWithoutCipher(t *testing.T) {
	ctx := context.Background()
	s := newAgentExecutionTestStore(t)
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
	s := newAgentExecutionTestStore(t)
	if err := s.SetAgentExecutionSnapshotCipher(testSnapshotCipher(t)); err != nil {
		t.Fatal(err)
	}

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

	if err := s.DeleteAgentExecutionSnapshots(ctx, "task-uid-1"); err != nil {
		t.Fatalf("delete snapshots: %v", err)
	}
	if _, err := s.GetAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshotKey{TaskUID: "task-uid-1", Digest: snapshot.Digest}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestAgentExecutionSnapshotCipherActivationRejectsRotationWithRetainedSnapshots(t *testing.T) {
	ctx := context.Background()
	s := newAgentExecutionTestStore(t)
	previousCipher := testSnapshotCipher(t)
	if err := s.SetAgentExecutionSnapshotCipher(previousCipher); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"prompt":"retained"}`)
	snapshot := store.AgentExecutionSnapshot{
		TaskUID:       "task-uid-rotation",
		Digest:        store.CanonicalAgentExecutionSnapshotDigest(body),
		SchemaVersion: store.AgentExecutionSnapshotSchemaVersion,
		Body:          body,
	}
	if err := s.PersistAgentExecutionSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}

	rotatedCipher, err := NewAgentExecutionSnapshotCipher(bytes.Repeat([]byte{0x43}, AgentExecutionSnapshotKeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewStore(s.db, "restart-with-rotated-key")
	if err := restarted.SetAgentExecutionSnapshotCipher(rotatedCipher); err == nil || !strings.Contains(err.Error(), "cannot authenticate retained snapshot") {
		t.Fatalf("rotated key activation error = %v", err)
	}
	if err := restarted.PersistAgentExecutionSnapshot(ctx, snapshot); !errors.Is(err, errSnapshotCipherRequired) {
		t.Fatalf("failed key activation must leave snapshot persistence closed, got %v", err)
	}

	restartedWithPreviousKey := NewStore(s.db, "restart-with-previous-key")
	if err := restartedWithPreviousKey.SetAgentExecutionSnapshotCipher(previousCipher); err != nil {
		t.Fatalf("activate previous key after restart: %v", err)
	}
	if _, err := restartedWithPreviousKey.GetAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshotKey{
		TaskUID: snapshot.TaskUID,
		Digest:  snapshot.Digest,
	}); err != nil {
		t.Fatalf("read retained snapshot with previous key: %v", err)
	}

	if err := s.DeleteAgentExecutionSnapshots(ctx, snapshot.TaskUID); err != nil {
		t.Fatal(err)
	}
	restartedAfterRetention := NewStore(s.db, "restart-after-retention")
	if err := restartedAfterRetention.SetAgentExecutionSnapshotCipher(rotatedCipher); err != nil {
		t.Fatalf("activate rotated key after retained snapshots are removed: %v", err)
	}
}

func TestAgentExecutionSnapshotLifecycleMetadataOrderingAndStrictCutoff(t *testing.T) {
	ctx := context.Background()
	s := newAgentExecutionTestStore(t)
	if err := s.SetAgentExecutionSnapshotCipher(testSnapshotCipher(t)); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 8, 6, 8, 0, 0, 0, time.UTC)
	early := persistLifecycleSnapshot(t, ctx, s, "task-z", []byte(`{"snapshot":"early"}`), base)
	tiedAt := base.Add(time.Minute)
	tied := []store.AgentExecutionSnapshotKey{
		persistLifecycleSnapshot(t, ctx, s, "task-b", []byte(`{"snapshot":"task-b"}`), tiedAt),
		persistLifecycleSnapshot(t, ctx, s, "task-a", []byte(`{"snapshot":"task-a-1"}`), tiedAt),
		persistLifecycleSnapshot(t, ctx, s, "task-a", []byte(`{"snapshot":"task-a-2"}`), tiedAt),
	}
	sort.Slice(tied, func(i, j int) bool {
		if tied[i].TaskUID != tied[j].TaskUID {
			return tied[i].TaskUID < tied[j].TaskUID
		}
		return tied[i].Digest < tied[j].Digest
	})
	cutoff := base.Add(2 * time.Minute)
	_ = persistLifecycleSnapshot(t, ctx, s, "task-boundary", []byte(`{"snapshot":"boundary"}`), cutoff)
	_ = persistLifecycleSnapshot(t, ctx, s, "task-late", []byte(`{"snapshot":"late"}`), cutoff.Add(time.Nanosecond))

	metadata, err := s.ListAgentExecutionSnapshotMetadataBefore(ctx, cutoff)
	if err != nil {
		t.Fatalf("ListAgentExecutionSnapshotMetadataBefore: %v", err)
	}
	wantKeys := append([]store.AgentExecutionSnapshotKey{early}, tied...)
	if len(metadata) != len(wantKeys) {
		t.Fatalf("metadata length = %d, want %d: %#v", len(metadata), len(wantKeys), metadata)
	}
	for index, wantKey := range wantKeys {
		if metadata[index].Key != wantKey {
			t.Fatalf("metadata[%d].Key = %#v, want %#v", index, metadata[index].Key, wantKey)
		}
		wantCreatedAt := tiedAt
		if index == 0 {
			wantCreatedAt = base
		}
		if !metadata[index].CreatedAt.Equal(wantCreatedAt) ||
			metadata[index].SchemaVersion != store.AgentExecutionSnapshotSchemaVersion {
			t.Fatalf("metadata[%d] = %#v, want createdAt=%s schemaVersion=%d",
				index, metadata[index], wantCreatedAt, store.AgentExecutionSnapshotSchemaVersion)
		}
	}
	if _, err := s.ListAgentExecutionSnapshotMetadataBefore(ctx, time.Time{}); err == nil {
		t.Fatal("zero metadata cutoff must fail validation")
	}
}

func TestAgentExecutionSnapshotLifecycleMetadataRejectsCorruptStoredIdentity(t *testing.T) {
	ctx := context.Background()
	s := newAgentExecutionTestStore(t)
	now := time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO agent_execution_snapshots
		(task_uid, digest, schema_version, nonce, ciphertext, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		"task-corrupt", "not-a-canonical-digest", 1, []byte{1}, []byte{2}, now); err != nil {
		t.Fatalf("seed corrupt snapshot metadata: %v", err)
	}

	if _, err := s.ListAgentExecutionSnapshotMetadataBefore(ctx, now.Add(time.Minute)); err == nil ||
		!strings.Contains(err.Error(), "snapshot digest") {
		t.Fatalf("corrupt stored identity error = %v, want digest integrity failure", err)
	}
}

func TestAgentExecutionSnapshotLifecycleReferenceCounts(t *testing.T) {
	ctx := context.Background()
	s := newAgentExecutionTestStore(t)
	if err := s.SetAgentExecutionSnapshotCipher(testSnapshotCipher(t)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	key := persistLifecycleSnapshot(t, ctx, s, "task-uid", []byte(`{"snapshot":"target"}`), now)
	otherDigestKey := persistLifecycleSnapshot(t, ctx, s, key.TaskUID, []byte(`{"snapshot":"other"}`), now)
	otherTaskKey := persistLifecycleSnapshot(t, ctx, s, "other-task-uid", []byte(`{"snapshot":"target"}`), now)
	bindingDigest := store.CanonicalAgentExecutionSnapshotDigest([]byte("binding"))
	requestDigest := store.CanonicalAgentExecutionSnapshotDigest([]byte("request"))

	mustExec := func(statement string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, statement, args...); err != nil {
			t.Fatalf("seed snapshot reference: %v", err)
		}
	}
	seedPromptAttempt := func(id, taskUID string, attempt int, snapshotDigest string) {
		mustExec(`INSERT INTO prompt_attempts
			(id, namespace, task_uid, attempt, prompt_id, request_digest,
			 binding_digest, snapshot_digest, execution_state, delivery_state,
			 controller_epoch_name, controller_epoch, version, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'Queued', 'NotRequested', 'epoch', 1, 1, ?, ?)`,
			id, "ns", taskUID, attempt, "prompt-"+id, requestDigest,
			bindingDigest, snapshotDigest, now, now)
	}
	seedLineage := func(id, configDigest string) {
		mustExec(`INSERT INTO session_lineages
			(namespace, session_name, namespace_uid, session_uid, contract_version,
			 lineage_generation, runtime_identity, config_digest, version, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'orka.harness.v2', 1, 'codex', ?, 1, ?, ?)`,
			"ns", "session-"+id, "namespace-uid", "session-uid-"+id,
			configDigest, now, now)
	}
	seedSessionTurn := func(id, taskUID string, attempt int) {
		sessionName := "turn-session-" + id
		mustExec(`INSERT INTO sessions(namespace, name, session_type) VALUES (?, ?, 'task')`, "ns", sessionName)
		mustExec(`INSERT INTO session_turns
			(id, namespace, session_name, session_uid, lease_generation, task_uid,
			 attempt, prompt_id, prompt_attempt_id, request_digest, user_prompt,
			 state, controller_epoch_name, controller_epoch, version, created_at, updated_at)
			VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, ?, 'prompt', 'Open', 'epoch', 1, 1, ?, ?)`,
			"turn-"+id, "ns", sessionName, "turn-session-uid-"+id, taskUID, attempt,
			"turn-prompt-"+id, "turn-attempt-"+id, requestDigest, now, now)
	}

	seedPromptAttempt("prompt-match-1", key.TaskUID, 1, key.Digest)
	seedPromptAttempt("prompt-match-2", key.TaskUID, 2, key.Digest)
	seedPromptAttempt("prompt-other-digest", key.TaskUID, 3, otherDigestKey.Digest)
	seedPromptAttempt("prompt-other-task", otherTaskKey.TaskUID, 1, key.Digest)
	seedSessionTurn("match-1", key.TaskUID, 1)
	seedSessionTurn("match-2", key.TaskUID, 2)
	seedSessionTurn("other-task", otherTaskKey.TaskUID, 1)
	// A lineage configuration digest is not an execution snapshot reference.
	seedLineage("match-1", key.Digest)
	seedLineage("match-2", key.Digest)
	seedLineage("other", otherDigestKey.Digest)

	counts, err := s.CountAgentExecutionSnapshotReferences(ctx, key)
	if err != nil {
		t.Fatalf("CountAgentExecutionSnapshotReferences: %v", err)
	}
	want := store.AgentExecutionSnapshotReferenceCounts{
		PromptAttempts: 2,
		SessionTurns:   2,
	}
	if counts != want || counts.Total() != 4 {
		t.Fatalf("reference counts = %#v (total %d), want %#v (total 4)", counts, counts.Total(), want)
	}

	otherTaskCounts, err := s.CountAgentExecutionSnapshotReferences(ctx, otherTaskKey)
	if err != nil {
		t.Fatalf("count other Task references: %v", err)
	}
	wantOtherTask := store.AgentExecutionSnapshotReferenceCounts{
		PromptAttempts: 1,
		SessionTurns:   1,
	}
	if otherTaskCounts != wantOtherTask {
		t.Fatalf("other Task reference counts = %#v, want %#v", otherTaskCounts, wantOtherTask)
	}
	if _, err := s.CountAgentExecutionSnapshotReferences(ctx, store.AgentExecutionSnapshotKey{}); err == nil {
		t.Fatal("incomplete snapshot key must fail reference-count validation")
	}
}

func TestAgentExecutionSnapshotLifecycleDeletesOnlyExactKey(t *testing.T) {
	ctx := context.Background()
	s := newAgentExecutionTestStore(t)
	if err := s.SetAgentExecutionSnapshotCipher(testSnapshotCipher(t)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 6, 11, 0, 0, 0, time.UTC)
	targetBody := []byte(`{"snapshot":"target"}`)
	target := persistLifecycleSnapshot(t, ctx, s, "task-uid", targetBody, now)
	sibling := persistLifecycleSnapshot(t, ctx, s, target.TaskUID, []byte(`{"snapshot":"sibling"}`), now)
	otherTask := persistLifecycleSnapshot(t, ctx, s, "other-task-uid", targetBody, now)

	if err := s.DeleteAgentExecutionSnapshot(ctx, target); err != nil {
		t.Fatalf("DeleteAgentExecutionSnapshot: %v", err)
	}
	if _, err := s.GetAgentExecutionSnapshot(ctx, target); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted target error = %v, want ErrNotFound", err)
	}
	for _, retained := range []store.AgentExecutionSnapshotKey{sibling, otherTask} {
		if _, err := s.GetAgentExecutionSnapshot(ctx, retained); err != nil {
			t.Fatalf("retained snapshot %s: %v", retained.ID(), err)
		}
	}
	if err := s.DeleteAgentExecutionSnapshot(ctx, target); err != nil {
		t.Fatalf("idempotent exact delete: %v", err)
	}
	if err := s.DeleteAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshotKey{}); err == nil {
		t.Fatal("incomplete exact-delete key must fail validation")
	}
}

func TestSessionLineageProjectionIsIdempotentAndRejectsDivergence(t *testing.T) {
	ctx := context.Background()
	s := newAgentExecutionTestStore(t)

	now := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	lineage := store.SessionLineage{
		Namespace:         "ns",
		SessionName:       "chat",
		NamespaceUID:      "ns-uid",
		SessionUID:        "session-uid",
		ContractVersion:   "orka.harness.v2",
		LineageGeneration: 1,
		RuntimeIdentity:   "codex",
		ConfigDigest:      store.CanonicalAgentExecutionSnapshotDigest([]byte("cfg")),
		Version:           1,
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	if _, err := s.GetSessionLineage(ctx, "ns", "chat"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound before projection, got %v", err)
	}

	projected, err := s.ProjectSessionLineage(ctx, lineage)
	if err != nil {
		t.Fatalf("project lineage: %v", err)
	}
	if projected.LineageGeneration != 1 || projected.Version != 1 {
		t.Fatalf("unexpected projected lineage: %+v", projected)
	}

	// An identical authoritative record projects idempotently.
	again, err := s.ProjectSessionLineage(ctx, lineage)
	if err != nil {
		t.Fatalf("verify projection: %v", err)
	}
	if again.SessionUID != lineage.SessionUID {
		t.Fatalf("verified lineage mismatch: %+v", again)
	}

	unsupported := lineage
	unsupported.ContractVersion = "orka.harness.v1"
	if _, err := s.ProjectSessionLineage(ctx, unsupported); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("expected ErrValidation for unsupported protocol, got %v", err)
	}

	changedRuntime := lineage
	changedRuntime.RuntimeIdentity = "opencode"
	if _, err := s.ProjectSessionLineage(ctx, changedRuntime); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict for changed runtime, got %v", err)
	}

	// A recreated same-name Session (new UID) never attaches to old state.
	recreated := lineage
	recreated.SessionUID = "different-session-uid"
	if _, err := s.ProjectSessionLineage(ctx, recreated); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict for recreated session UID, got %v", err)
	}

	// A recreated same-name namespace never attaches to old state.
	recreatedNamespace := lineage
	recreatedNamespace.NamespaceUID = "different-ns-uid"
	if _, err := s.ProjectSessionLineage(ctx, recreatedNamespace); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict for recreated namespace UID, got %v", err)
	}

	changedConfig := lineage
	changedConfig.ConfigDigest = store.CanonicalAgentExecutionSnapshotDigest([]byte("different-cfg"))
	if _, err := s.ProjectSessionLineage(ctx, changedConfig); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict for changed configuration, got %v", err)
	}
}

func TestV2ExecutionSnapshotAndSessionLineageSurviveDatabaseReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "v2-restart.db")
	db, err := NewDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := NewStore(db, dbPath)
	if err := s.SetAgentExecutionSnapshotCipher(testSnapshotCipher(t)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"contractVersion":"orka.harness.v2","prompt":"saved work"}`)
	key := persistLifecycleSnapshot(t, ctx, s, "task-v2", body, now)
	lineage := store.SessionLineage{
		Namespace: "tenant", SessionName: "chat", NamespaceUID: "namespace-uid", SessionUID: "session-uid",
		ContractVersion: "orka.harness.v2", LineageGeneration: 1, RuntimeIdentity: "codex",
		ConfigDigest: key.Digest, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := s.ProjectSessionLineage(ctx, lineage); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = NewDB(dbPath)
	if err != nil {
		t.Fatalf("reopen current v2 database: %v", err)
	}
	reopened := NewStore(db, dbPath)
	if err := reopened.SetAgentExecutionSnapshotCipher(testSnapshotCipher(t)); err != nil {
		t.Fatal(err)
	}
	snapshot, err := reopened.GetAgentExecutionSnapshot(ctx, key)
	if err != nil {
		t.Fatalf("read reopened snapshot: %v", err)
	}
	if snapshot.TaskUID != key.TaskUID || snapshot.Digest != key.Digest ||
		snapshot.SchemaVersion != store.AgentExecutionSnapshotSchemaVersion ||
		!bytes.Equal(snapshot.Body, body) || !snapshot.CreatedAt.Equal(now) {
		t.Fatal("database reopen changed the immutable v2 execution snapshot")
	}
	got, err := reopened.GetSessionLineage(ctx, lineage.Namespace, lineage.SessionName)
	if err != nil {
		t.Fatalf("read reopened Session lineage: %v", err)
	}
	if got.NamespaceUID != lineage.NamespaceUID || got.SessionUID != lineage.SessionUID ||
		got.ContractVersion != lineage.ContractVersion || got.LineageGeneration != lineage.LineageGeneration ||
		got.RuntimeIdentity != lineage.RuntimeIdentity || got.ConfigDigest != lineage.ConfigDigest ||
		got.Version != lineage.Version || !got.CreatedAt.Equal(now) || !got.UpdatedAt.Equal(now) {
		t.Fatal("database reopen changed the immutable v2 Session lineage")
	}
	if _, err := reopened.ProjectSessionLineage(ctx, lineage); err != nil {
		t.Fatalf("verify unchanged lineage after reopen: %v", err)
	}
}
