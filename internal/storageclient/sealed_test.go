package storageclient_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/storetest"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// sealedTestKey is a snapshot key with a recognizable pattern, so the tests
// can search provider input for it.
var sealedTestKey = []byte("orka-sealed-records-test-key-32b")

func activatedClient(t *testing.T, backend storageclient.EmbeddedStore, key []byte) *storageclient.Client {
	t.Helper()
	client, err := storageclient.NewEmbedded(backend)
	require.NoError(t, err)
	require.NoError(t, client.ActivateSnapshotKey(t.Context(), key))
	return client
}

func snapshotFor(taskUID string, body []byte) store.AgentExecutionSnapshot {
	return store.AgentExecutionSnapshot{
		TaskUID: taskUID, Digest: store.CanonicalAgentExecutionSnapshotDigest(body),
		SchemaVersion: store.AgentExecutionSnapshotSchemaVersion, Body: body,
	}
}

// nativeImport returns a staged import request for a fresh bundle.
func nativeImport(t *testing.T, name, text string) store.NativeSessionImport {
	t.Helper()
	snapshot := storetest.NativeSessionSnapshot(t, text)
	snapshot.RuntimeSessionUID, snapshot.RuntimeProfileDigest, snapshot.WorkingDirectory = "", "", ""
	return store.NativeSessionImport{
		Namespace: "tenant", SessionName: name, OperationID: "import-" + name,
		RequestDigest: store.NativeSessionImportDigest("tenant", name, snapshot.DataDigest), Snapshot: snapshot,
	}
}

// finalizationFixture creates a Session with a bound owner and an open turn.
func finalizationFixture(t *testing.T, client *storageclient.Client, name, taskUID string) store.CommitSessionTurnFinalizationRequest {
	t.Helper()
	ctx := t.Context()
	uid := name + "-uid"
	if _, err := client.GetSession(ctx, "tenant", name); err != nil {
		require.NoError(t, client.CreateSession(ctx, &store.SessionRecord{Namespace: "tenant", Name: name, SessionType: "task"}))
	}
	require.NoError(t, client.BindSessionCleanupIdentity(ctx, "tenant", name, uid))
	fence := store.ControllerEpochFence{Name: store.DefaultControllerEpochName, Epoch: 1, HolderID: "controller"}
	turn, err := client.CreateSessionTurnRecord(ctx, store.CreateSessionTurnRecordRequest{
		Namespace: "tenant", SessionName: name, Fence: fence,
		Turn: store.SessionTurn{
			Key:             store.SessionTurnKey{SessionUID: uid, LeaseGeneration: 1, TaskUID: taskUID, Attempt: 1, PromptID: "prompt"},
			PromptAttemptID: taskUID + "-attempt", RequestDigest: store.CanonicalBytesDigest([]byte(taskUID)), UserPrompt: "a question",
		},
	})
	require.NoError(t, err)
	payload := []byte(`{"phase":"Succeeded"}`)
	return store.CommitSessionTurnFinalizationRequest{
		Key: turn.Key, Namespace: "tenant", SessionName: name, Fence: fence, ExpectedTurnVersion: turn.Version,
		FinalizationDigest: store.CanonicalBytesDigest([]byte("finalize " + taskUID)),
		TerminalKind:       store.SessionTurnAssistantResult, TerminalContent: "an answer",
		Projection: store.OutboxProjection{
			ID: "projection-" + turn.ID, AggregateKind: "SessionTurn", AggregateID: turn.ID,
			ProjectionKind: "TaskTerminalStatus", Payload: payload, PayloadDigest: store.CanonicalBytesDigest(payload),
		},
		FinalizedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}
}

// TestProviderReceivesOnlySealedSnapshotsAndNativeSessions drives every write
// of execution snapshots and native session state through the client, then
// proves the provider never received the plaintext bodies, the native
// bundles in any encoding, or the snapshot key.
func TestProviderReceivesOnlySealedSnapshotsAndNativeSessions(t *testing.T) {
	recorder := newRecordingStore(t)
	client := activatedClient(t, recorder, sealedTestKey)
	ctx := t.Context()
	plaintexts := make([][]byte, 0, 4)

	body := []byte(`{"prompt":"resolved prompt with a private skill: snapshot-plaintext-marker"}`)
	plaintexts = append(plaintexts, body)
	snapshot := snapshotFor("task-uid", body)
	require.NoError(t, client.PersistAgentExecutionSnapshot(ctx, snapshot))
	require.NoError(t, client.PersistAgentExecutionSnapshot(ctx, snapshot), "an identical snapshot persists idempotently")
	read, err := client.GetAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshotKey{TaskUID: snapshot.TaskUID, Digest: snapshot.Digest})
	require.NoError(t, err)
	require.Equal(t, body, read.Body)

	imported := nativeImport(t, "imported", "imported private native history")
	plaintexts = append(plaintexts, imported.Snapshot.Data)
	_, err = client.StageNativeSessionImport(ctx, imported)
	require.NoError(t, err)
	staged, err := client.GetNativeSession(ctx, "tenant", "imported", "")
	require.NoError(t, err)
	require.Equal(t, imported.Snapshot.Data, staged.Snapshot.Data)

	// A capture outside a turn, then a turn that captures new state and a
	// turn that carries it across an outcome marker.
	require.NoError(t, client.CreateSession(ctx, &store.SessionRecord{Namespace: "tenant", Name: "captured", SessionType: "task"}))
	require.NoError(t, client.BindSessionCleanupIdentity(ctx, "tenant", "captured", "captured-uid"))
	captured := store.NativeSessionRecord{
		Namespace: "tenant", SessionName: "captured", SessionUID: "captured-uid", RuntimeSessionGeneration: 1,
		SourceOperationID: "capture-1", Snapshot: storetest.NativeSessionSnapshot(t, "captured private native history"),
	}
	plaintexts = append(plaintexts, captured.Snapshot.Data)
	require.NoError(t, client.SaveNativeSession(ctx, captured))

	request := finalizationFixture(t, client, "captured", "turn-task-1")
	finalized := captured
	finalized.RuntimeSessionGeneration, finalized.SourceOperationID = 2, "capture-2"
	finalized.Snapshot = storetest.NativeSessionSnapshot(t, "finalized private native history")
	plaintexts = append(plaintexts, finalized.Snapshot.Data)
	request.NativeSession = &finalized
	turn, err := client.CommitSessionTurnFinalization(ctx, request)
	require.NoError(t, err)
	require.Equal(t, store.NativeSessionCaptureDigest(&finalized), turn.NativeSessionDigest)
	stored, err := client.GetNativeSession(ctx, "tenant", "captured", "captured-uid")
	require.NoError(t, err)
	require.Equal(t, finalized.Snapshot.Data, stored.Snapshot.Data)
	require.Equal(t, 2, stored.MessageCount)

	carry := finalizationFixture(t, client, "captured", "turn-task-2")
	carry.TerminalKind, carry.TerminalContent = store.SessionTurnOutcomeMarker, `{"kind":"Failed"}`
	carry.NativeSession, carry.NativeSessionCarried = stored, true
	_, err = client.CommitSessionTurnFinalization(ctx, carry)
	require.NoError(t, err)
	carried, err := client.GetNativeSession(ctx, "tenant", "captured", "captured-uid")
	require.NoError(t, err)
	require.Equal(t, finalized.Snapshot.Data, carried.Snapshot.Data)
	require.Equal(t, 4, carried.MessageCount)

	recorder.requireCalled(t, "CreateSealedAgentExecutionSnapshot", "StageSealedNativeSessionImport", "SaveSealedNativeSession",
		"CommitSealedSessionTurnFinalization")
	secrets := map[string]string{
		"key":        string(sealedTestKey),
		"hex key":    hex.EncodeToString(sealedTestKey),
		"base64 key": base64.StdEncoding.EncodeToString(sealedTestKey),
	}
	for i, plaintext := range plaintexts {
		secrets[fmt.Sprintf("plaintext %d", i)] = string(plaintext)
		secrets[fmt.Sprintf("base64 plaintext %d", i)] = base64.StdEncoding.EncodeToString(plaintext)
	}
	secrets["snapshot marker"] = "snapshot-plaintext-marker"
	recorder.requireNoSecret(t, secrets)
	// The provider received the sealed records' plain facts, so the check
	// above is not passing on empty records.
	recorder.requireReceived(t, "StageSealedNativeSessionImport", imported.Snapshot.DataDigest)
	recorder.requireReceived(t, "CommitSealedSessionTurnFinalization", finalized.Snapshot.DataDigest)
	recorder.requireReceived(t, "CreateSealedAgentExecutionSnapshot", snapshot.Digest)
}

// legacySeal and legacyOpen implement the stored formats independently of
// the client, as the embedded store sealed them before the client held the
// key.
func legacySeal(t *testing.T, key, additionalData, plaintext []byte) ([]byte, []byte) {
	t.Helper()
	aead := legacyAEAD(t, key)
	nonce := make([]byte, aead.NonceSize())
	_, err := rand.Read(nonce)
	require.NoError(t, err)
	return nonce, aead.Seal(nil, nonce, plaintext, additionalData)
}

func legacyOpen(t *testing.T, key, nonce, ciphertext, additionalData []byte) []byte {
	t.Helper()
	plaintext, err := legacyAEAD(t, key).Open(nil, nonce, ciphertext, additionalData)
	require.NoError(t, err)
	return plaintext
}

func legacyAEAD(t *testing.T, key []byte) cipher.AEAD {
	t.Helper()
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	aead, err := cipher.NewGCM(block)
	require.NoError(t, err)
	return aead
}

func legacySnapshotAAD(taskUID, digest string, schemaVersion int32) []byte {
	return fmt.Appendf(nil, "orka.agent-execution-snapshot\x00%s\x00%s\x00%d", taskUID, digest, schemaVersion)
}

func legacyNativeAAD(namespace, name, uid, digest string) []byte {
	return fmt.Appendf(nil, "orka.native-session.v1\x00%s\x00%s\x00%s\x00%s", namespace, name, uid, digest)
}

// TestSealedFormatsMatchStoredRecords proves the client reads records in the
// stored formats and writes records those formats can read, so existing
// datasets open unchanged.
func TestSealedFormatsMatchStoredRecords(t *testing.T) {
	backend := openSQLite(t)
	ctx := t.Context()

	// Records sealed in the stored formats open through the client.
	body := []byte(`{"prompt":"sealed in the stored format"}`)
	snapshot := snapshotFor("legacy-task", body)
	nonce, ciphertext := legacySeal(t, sealedTestKey, legacySnapshotAAD(snapshot.TaskUID, snapshot.Digest, snapshot.SchemaVersion), body)
	require.NoError(t, backend.CreateSealedAgentExecutionSnapshot(ctx, storagev1.SealedAgentExecutionSnapshot{
		Key: store.AgentExecutionSnapshotKey{TaskUID: snapshot.TaskUID, Digest: snapshot.Digest}, SchemaVersion: snapshot.SchemaVersion,
		Nonce: nonce, Ciphertext: ciphertext, CreatedAt: time.Now().UTC(),
	}))
	imported := nativeImport(t, "legacy", "native history sealed in the stored format")
	record := store.NativeSessionRecord{
		Namespace: imported.Namespace, SessionName: imported.SessionName, Snapshot: imported.Snapshot,
		SourceOperationID: imported.OperationID, CreatedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}
	encoded, err := json.Marshal(record)
	require.NoError(t, err)
	recordDigest := store.CanonicalBytesDigest(encoded)
	nonce, ciphertext = legacySeal(t, sealedTestKey, legacyNativeAAD(record.Namespace, record.SessionName, "", recordDigest), encoded)
	_, err = backend.StageSealedNativeSessionImport(ctx, storagev1.StageSealedNativeSessionImportRequest{
		Record: storagev1.SealedNativeSession{
			Namespace: record.Namespace, SessionName: record.SessionName, RecordDigest: recordDigest,
			DataDigest: record.Snapshot.DataDigest, SourceOperationID: record.SourceOperationID, Nonce: nonce, Ciphertext: ciphertext,
		},
		Receipt: store.NativeSessionImportReceipt{
			Namespace: record.Namespace, SessionName: record.SessionName, OperationID: record.SourceOperationID,
			RequestDigest: imported.RequestDigest, DataDigest: record.Snapshot.DataDigest,
			ProviderSessionID: record.Snapshot.ProviderSessionID, CreatedAt: record.CreatedAt,
		},
	})
	require.NoError(t, err)

	client := activatedClient(t, backend, sealedTestKey)
	read, err := client.GetAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshotKey{TaskUID: snapshot.TaskUID, Digest: snapshot.Digest})
	require.NoError(t, err)
	require.Equal(t, body, read.Body)
	native, err := client.GetNativeSession(ctx, "tenant", "legacy", "")
	require.NoError(t, err)
	require.Equal(t, record, *native)

	// Records the client seals open in the stored formats.
	written := snapshotFor("client-task", []byte(`{"prompt":"sealed by the client"}`))
	require.NoError(t, client.PersistAgentExecutionSnapshot(ctx, written))
	sealed, err := backend.GetSealedAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshotKey{TaskUID: written.TaskUID, Digest: written.Digest})
	require.NoError(t, err)
	require.Equal(t, written.Body, legacyOpen(t, sealedTestKey, sealed.Nonce, sealed.Ciphertext,
		legacySnapshotAAD(written.TaskUID, written.Digest, sealed.SchemaVersion)))

	clientImport := nativeImport(t, "client", "native history sealed by the client")
	_, err = client.StageNativeSessionImport(ctx, clientImport)
	require.NoError(t, err)
	sealedNative, err := backend.GetSealedNativeSession(ctx, "tenant", "client", "")
	require.NoError(t, err)
	plaintext := legacyOpen(t, sealedTestKey, sealedNative.Nonce, sealedNative.Ciphertext,
		legacyNativeAAD(sealedNative.Namespace, sealedNative.SessionName, sealedNative.SessionUID, sealedNative.RecordDigest))
	require.Equal(t, sealedNative.RecordDigest, store.CanonicalBytesDigest(plaintext))
	var opened store.NativeSessionRecord
	require.NoError(t, json.Unmarshal(plaintext, &opened))
	require.Equal(t, clientImport.Snapshot, opened.Snapshot)
	require.Equal(t, clientImport.OperationID, opened.SourceOperationID)
}

// tamperingStore changes what the provider returns for sealed records.
type tamperingStore struct {
	*recordingStore
	snapshot func(*storagev1.SealedAgentExecutionSnapshot)
	native   func(*storagev1.SealedNativeSession)
}

func (s *tamperingStore) GetSealedAgentExecutionSnapshot(ctx context.Context, key store.AgentExecutionSnapshotKey) (*storagev1.SealedAgentExecutionSnapshot, error) {
	sealed, err := s.Store.GetSealedAgentExecutionSnapshot(ctx, key)
	if err == nil && s.snapshot != nil {
		s.snapshot(sealed)
	}
	return sealed, err
}

func (s *tamperingStore) GetSealedNativeSession(ctx context.Context, namespace, name, uid string) (*storagev1.SealedNativeSession, error) {
	sealed, err := s.Store.GetSealedNativeSession(ctx, namespace, name, uid)
	if err == nil && s.native != nil {
		s.native(sealed)
	}
	return sealed, err
}

// TestClientRejectsWrongKeysAndModifiedRecords covers a missing key, a key
// that cannot open retained records, and records a provider altered or
// returned for another identity.
func TestClientRejectsWrongKeysAndModifiedRecords(t *testing.T) {
	ctx := context.Background()
	backend := &tamperingStore{recordingStore: newRecordingStore(t)}
	client := activatedClient(t, backend, sealedTestKey)
	snapshot := snapshotFor("task-uid", []byte(`{"prompt":"retained"}`))
	require.NoError(t, client.PersistAgentExecutionSnapshot(ctx, snapshot))
	other := snapshotFor("task-uid", []byte(`{"prompt":"another retained snapshot"}`))
	require.NoError(t, client.PersistAgentExecutionSnapshot(ctx, other))
	imported := nativeImport(t, "imported", "retained native history")
	_, err := client.StageNativeSessionImport(ctx, imported)
	require.NoError(t, err)
	key := store.AgentExecutionSnapshotKey{TaskUID: snapshot.TaskUID, Digest: snapshot.Digest}

	missing, err := storageclient.NewEmbedded(backend)
	require.NoError(t, err)
	_, err = missing.GetAgentExecutionSnapshot(ctx, key)
	require.ErrorIs(t, err, storageclient.ErrSnapshotKeyNotActive)
	_, err = missing.GetNativeSession(ctx, "tenant", "imported", "")
	require.ErrorIs(t, err, storageclient.ErrSnapshotKeyNotActive)
	require.ErrorIs(t, missing.PersistAgentExecutionSnapshot(ctx, snapshot), storageclient.ErrSnapshotKeyNotActive)

	wrong := bytes.Repeat([]byte{0x24}, storageclient.SnapshotKeyBytes)
	require.ErrorContains(t, missing.ActivateSnapshotKey(ctx, wrong), "cannot authenticate retained snapshot")
	require.Error(t, missing.ActivateSnapshotKey(ctx, wrong[:16]), "a short key is rejected")
	_, err = missing.GetAgentExecutionSnapshot(ctx, key)
	require.ErrorIs(t, err, storageclient.ErrSnapshotKeyNotActive, "a rejected key is not installed")
	require.NoError(t, client.DeleteAgentExecutionSnapshots(ctx, snapshot.TaskUID))
	require.ErrorContains(t, missing.ActivateSnapshotKey(ctx, wrong), "cannot authenticate retained native session")
	require.NoError(t, client.PersistAgentExecutionSnapshot(ctx, snapshot))
	require.NoError(t, client.PersistAgentExecutionSnapshot(ctx, other))

	for name, tamper := range map[string]func(*storagev1.SealedAgentExecutionSnapshot){
		"ciphertext":     func(s *storagev1.SealedAgentExecutionSnapshot) { s.Ciphertext[0] ^= 0xff },
		"nonce":          func(s *storagev1.SealedAgentExecutionSnapshot) { s.Nonce[0] ^= 0xff },
		"short nonce":    func(s *storagev1.SealedAgentExecutionSnapshot) { s.Nonce = s.Nonce[:4] },
		"schema version": func(s *storagev1.SealedAgentExecutionSnapshot) { s.SchemaVersion++ },
		"identity":       func(s *storagev1.SealedAgentExecutionSnapshot) { s.Key.Digest = other.Digest },
	} {
		backend.snapshot = tamper
		_, err := client.GetAgentExecutionSnapshot(ctx, key)
		require.Error(t, err, name)
	}
	// Another record's sealed bytes under this key fail authentication.
	backend.snapshot = func(s *storagev1.SealedAgentExecutionSnapshot) {
		swapped, err := backend.Store.GetSealedAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshotKey{TaskUID: other.TaskUID, Digest: other.Digest})
		require.NoError(t, err)
		s.Nonce, s.Ciphertext = swapped.Nonce, swapped.Ciphertext
	}
	_, err = client.GetAgentExecutionSnapshot(ctx, key)
	require.ErrorContains(t, err, "decrypt agent execution snapshot")
	backend.snapshot = nil
	read, err := client.GetAgentExecutionSnapshot(ctx, key)
	require.NoError(t, err)
	require.Equal(t, snapshot.Body, read.Body)

	for name, tamper := range map[string]func(*storagev1.SealedNativeSession){
		"ciphertext":         func(s *storagev1.SealedNativeSession) { s.Ciphertext[0] ^= 0xff },
		"record digest":      func(s *storagev1.SealedNativeSession) { s.RecordDigest = store.CanonicalBytesDigest([]byte("x")) },
		"data digest":        func(s *storagev1.SealedNativeSession) { s.DataDigest = store.CanonicalBytesDigest([]byte("x")) },
		"source operation":   func(s *storagev1.SealedNativeSession) { s.SourceOperationID = "tampered" },
		"runtime generation": func(s *storagev1.SealedNativeSession) { s.RuntimeSessionGeneration = 7 },
		"boundary":           func(s *storagev1.SealedNativeSession) { s.MessageCount, s.ThroughMessageID = 2, "message" },
		"owner":              func(s *storagev1.SealedNativeSession) { s.SessionUID = "another-owner" },
		"short nonce":        func(s *storagev1.SealedNativeSession) { s.Nonce = s.Nonce[:4] },
	} {
		backend.native = tamper
		_, err := client.GetNativeSession(ctx, "tenant", "imported", "")
		require.Error(t, err, name)
		require.False(t, strings.Contains(err.Error(), "retained native history"), "error leaks plaintext")
	}
	backend.native = nil
	native, err := client.GetNativeSession(ctx, "tenant", "imported", "")
	require.NoError(t, err)
	require.Equal(t, imported.Snapshot.Data, native.Snapshot.Data)
}
