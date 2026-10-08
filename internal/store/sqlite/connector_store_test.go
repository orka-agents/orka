/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func newConnectorTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "connectors.db")
	db, err := NewDB(dbPath)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := NewStore(db, dbPath)
	cipher, err := NewAgentExecutionSnapshotCipher(bytes.Repeat([]byte{0x42}, AgentExecutionSnapshotKeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAgentExecutionSnapshotCipher(cipher); err != nil {
		t.Fatal(err)
	}
	return s
}

func testCredentialRef() store.ConnectorCredentialRef {
	return store.ConnectorCredentialRef{
		ConnectionUID: "uid-1", Namespace: "tenant", Name: "github-abc", SubjectDigest: "digest-a", Provider: "github",
	}
}

func TestConnectorCredentialRoundTrip(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	ref := testCredentialRef()
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	credential := store.ConnectorCredential{
		AccessToken: "gho_access", RefreshToken: "ghr_refresh", TokenType: "bearer",
		ExpiresAt: expires, Scopes: []string{"repo", "read:user"},
	}
	if err := s.PutConnectorCredential(ctx, ref, credential); err != nil {
		t.Fatalf("PutConnectorCredential: %v", err)
	}
	got, err := s.GetConnectorCredential(ctx, ref)
	if err != nil {
		t.Fatalf("GetConnectorCredential: %v", err)
	}
	if got.AccessToken != credential.AccessToken || got.RefreshToken != credential.RefreshToken || got.TokenType != "bearer" {
		t.Fatalf("credential = %+v", got)
	}
	if !got.ExpiresAt.Equal(expires) || len(got.Scopes) != 2 || got.UpdatedAt.IsZero() {
		t.Fatalf("credential metadata = %+v", got)
	}

	// The row never holds plaintext.
	var ciphertext, dek []byte
	if err := s.db.QueryRow(`SELECT ciphertext, dek_ciphertext FROM connector_credentials WHERE connection_uid = ?`, ref.ConnectionUID).Scan(&ciphertext, &dek); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"gho_access", "ghr_refresh"} {
		if bytes.Contains(ciphertext, []byte(secret)) || bytes.Contains(dek, []byte(secret)) {
			t.Fatalf("row stores plaintext %q", secret)
		}
	}

	// Rebinding any identity field must fail to open.
	for name, mutate := range map[string]func(*store.ConnectorCredentialRef){
		"subject":   func(r *store.ConnectorCredentialRef) { r.SubjectDigest = "digest-b" },
		"provider":  func(r *store.ConnectorCredentialRef) { r.Provider = "gmail" },
		"namespace": func(r *store.ConnectorCredentialRef) { r.Namespace = "other" },
		"name":      func(r *store.ConnectorCredentialRef) { r.Name = "other" },
	} {
		wrong := ref
		mutate(&wrong)
		if _, err := s.GetConnectorCredential(ctx, wrong); err == nil {
			t.Fatalf("%s rebinding opened the credential", name)
		} else if strings.Contains(err.Error(), "gho_access") {
			t.Fatalf("error leaked the token: %v", err)
		}
	}

	// Replacing the material rotates the wrapped data key.
	var dekBefore []byte
	_ = s.db.QueryRow(`SELECT dek_ciphertext FROM connector_credentials WHERE connection_uid = ?`, ref.ConnectionUID).Scan(&dekBefore)
	if err := s.PutConnectorCredential(ctx, ref, store.ConnectorCredential{AccessToken: "gho_second"}); err != nil {
		t.Fatal(err)
	}
	var dekAfter []byte
	_ = s.db.QueryRow(`SELECT dek_ciphertext FROM connector_credentials WHERE connection_uid = ?`, ref.ConnectionUID).Scan(&dekAfter)
	if bytes.Equal(dekBefore, dekAfter) {
		t.Fatal("replacing the credential must mint a fresh data key")
	}
	got, err = s.GetConnectorCredential(ctx, ref)
	if err != nil || got.AccessToken != "gho_second" || got.RefreshToken != "" || !got.ExpiresAt.IsZero() {
		t.Fatalf("replaced credential = %+v, err = %v", got, err)
	}

	if err := s.DeleteConnectorCredential(ctx, ref.ConnectionUID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetConnectorCredential(ctx, ref); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteConnectorCredential(ctx, ref.ConnectionUID); err != nil {
		t.Fatalf("deleting a missing credential must succeed: %v", err)
	}
}

func TestConnectorCredentialFailsClosed(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "nocipher.db")
	db, err := NewDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := NewStore(db, dbPath)
	if err := s.PutConnectorCredential(ctx, testCredentialRef(), store.ConnectorCredential{AccessToken: "x"}); !errors.Is(err, errConnectorCipherRequired) {
		t.Fatalf("Put without cipher err = %v", err)
	}
	if _, err := s.GetConnectorCredential(ctx, testCredentialRef()); !errors.Is(err, errConnectorCipherRequired) {
		t.Fatalf("Get without cipher err = %v", err)
	}
	if err := s.CreateConnectorConsent(ctx, store.ConnectorConsent{}); !errors.Is(err, errConnectorCipherRequired) {
		t.Fatalf("CreateConsent without cipher err = %v", err)
	}

	withCipher := newConnectorTestStore(t)
	if err := withCipher.PutConnectorCredential(ctx, store.ConnectorCredentialRef{ConnectionUID: "uid"}, store.ConnectorCredential{AccessToken: "x"}); err == nil {
		t.Fatal("incomplete ref must be rejected")
	}
	if err := withCipher.PutConnectorCredential(ctx, testCredentialRef(), store.ConnectorCredential{}); err == nil {
		t.Fatal("empty access token must be rejected")
	}
	if err := withCipher.DeleteConnectorCredential(ctx, " "); err == nil {
		t.Fatal("empty UID must be rejected")
	}
}

func TestConnectorCredentialKeyRotationBlocksOpen(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	ref := testCredentialRef()
	if err := s.PutConnectorCredential(ctx, ref, store.ConnectorCredential{AccessToken: "gho"}); err != nil {
		t.Fatal(err)
	}
	rotated, err := NewAgentExecutionSnapshotCipher(bytes.Repeat([]byte{0x43}, AgentExecutionSnapshotKeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	other := NewStore(s.db, "rotated")
	other.snapshotCipher = rotated
	if _, err := other.GetConnectorCredential(ctx, ref); err == nil {
		t.Fatal("a different controller key must not unwrap the data key")
	}
}

func TestConnectorConsentSingleUseAndExpiry(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	consent := store.ConnectorConsent{
		Nonce: "nonce-1", ConnectionUID: "uid-1", Namespace: "tenant", Name: "github-abc", AuthorityDigest: "authority-1", RevocationDigest: "revocation-1", Scopes: []string{"read:user", "repo"},
		SubjectDigest: "digest-a", Provider: "github", Mode: "readOnly", CodeVerifier: "verifier-secret",
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}
	if err := s.CreateConnectorConsent(ctx, consent); err != nil {
		t.Fatalf("CreateConnectorConsent: %v", err)
	}
	var sealed []byte
	if err := s.db.QueryRow(`SELECT verifier_ciphertext FROM connector_consents WHERE nonce = ?`, consent.Nonce).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("verifier-secret")) {
		t.Fatal("consent row stores the verifier in plaintext")
	}
	got, err := s.ConsumeConnectorConsent(ctx, consent.Nonce)
	if err != nil {
		t.Fatalf("ConsumeConnectorConsent: %v", err)
	}
	if got.CodeVerifier != consent.CodeVerifier || got.ConnectionUID != consent.ConnectionUID || got.Mode != consent.Mode ||
		got.SubjectDigest != consent.SubjectDigest || got.AuthorityDigest != "authority-1" || got.RevocationDigest != "revocation-1" || strings.Join(got.Scopes, " ") != "read:user repo" {
		t.Fatalf("consent = %+v", got)
	}
	if _, err := s.ConsumeConnectorConsent(ctx, consent.Nonce); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second consume err = %v, want ErrNotFound", err)
	}
	if _, err := s.ConsumeConnectorConsent(ctx, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("empty nonce err = %v", err)
	}

	expired := consent
	expired.Nonce = "nonce-expired"
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	if err := s.CreateConnectorConsent(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeConnectorConsent(ctx, expired.Nonce); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired consume err = %v, want ErrNotFound", err)
	}

	// Creating a new consent purges stale rows, and disconnect drops the rest.
	stale := consent
	stale.Nonce = "nonce-stale"
	stale.ExpiresAt = time.Now().Add(-time.Hour)
	if err := s.CreateConnectorConsent(ctx, stale); err != nil {
		t.Fatal(err)
	}
	fresh := consent
	fresh.Nonce = "nonce-fresh"
	if err := s.CreateConnectorConsent(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	var staleCount int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM connector_consents WHERE nonce = ?`, stale.Nonce).Scan(&staleCount)
	if staleCount != 0 {
		t.Fatal("expired consents must be purged on create")
	}
	if err := s.DeleteConnectorConsentsForConnection(ctx, consent.ConnectionUID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeConnectorConsent(ctx, fresh.Nonce); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("consents must be gone after disconnect")
	}
	if err := s.CreateConnectorConsent(ctx, store.ConnectorConsent{Nonce: "x"}); err == nil {
		t.Fatal("incomplete consent must be rejected")
	}
}

// testConsentSequence numbers fixture completions as if each came from a
// newer consent than the last, so a test that parks several completions in
// order models consents started in that order.
var testConsentSequence atomic.Int64

func testConnectorCompletion() store.ConnectorCompletion {
	return store.ConnectorCompletion{
		Nonce: "completion-1", ConnectionUID: "uid-1", Namespace: "tenant", Name: "github-abc",
		SubjectDigest: "digest-a", Provider: "github", Mode: "readOnly", ConsentAuthorityDigest: "consent-authority-1",
		Credential:      store.ConnectorCredential{AccessToken: "gho_parked", RefreshToken: "ghr_parked", Scopes: []string{"repo"}, AuthorityDigest: "authority-1"},
		ExpiresAt:       time.Now().Add(10 * time.Minute),
		ConsentSequence: testConsentSequence.Add(1),
	}
}

func TestConnectorCompletionRoundTrip(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	completion := testConnectorCompletion()
	if err := s.CreateConnectorCompletion(ctx, completion); err != nil {
		t.Fatalf("CreateConnectorCompletion: %v", err)
	}
	var payload []byte
	if err := s.db.QueryRow(`SELECT payload FROM connector_completions WHERE nonce = ?`, completion.Nonce).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte("gho_parked")) {
		t.Fatal("completion row stores the token in plaintext")
	}
	got, err := s.ConsumeConnectorCompletion(ctx, completion.Nonce)
	if err != nil {
		t.Fatalf("ConsumeConnectorCompletion: %v", err)
	}
	if got.Credential.AccessToken != "gho_parked" || got.Credential.RefreshToken != "ghr_parked" || got.SubjectDigest != "digest-a" ||
		got.Mode != "readOnly" || got.Credential.AuthorityDigest != "authority-1" || got.ConsentAuthorityDigest != "consent-authority-1" {
		t.Fatalf("completion = %+v", got)
	}
	if _, err := s.ConsumeConnectorCompletion(ctx, completion.Nonce); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second consume err = %v, want ErrNotFound", err)
	}
	// Peek leaves the row for a retry; list opens every parked completion.
	peekable := completion
	peekable.Nonce = "completion-peek"
	if err := s.CreateConnectorCompletion(ctx, peekable); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		peeked, err := s.PeekConnectorCompletion(ctx, peekable.Nonce)
		if err != nil || peeked.Credential.AccessToken != "gho_parked" {
			t.Fatalf("peek = %+v err = %v", peeked, err)
		}
	}
	listed, err := s.ListConnectorCompletionsForConnection(ctx, completion.ConnectionUID)
	if err != nil || len(listed) != 1 || listed[0].Nonce != peekable.Nonce || listed[0].Credential.RefreshToken != "ghr_parked" {
		t.Fatalf("listed = %+v err = %v", listed, err)
	}
	if err := s.DeleteConnectorCompletion(ctx, peekable.Nonce); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PeekConnectorCompletion(ctx, peekable.Nonce); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("peek after delete err = %v", err)
	}
	if err := s.DeleteConnectorCompletion(ctx, peekable.Nonce); err != nil {
		t.Fatalf("delete must be idempotent: %v", err)
	}
	if _, err := s.PeekConnectorCompletion(ctx, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("peek empty nonce err = %v", err)
	}
}

// TestConnectorCompletionExpiryAndDisconnect covers expired rows, which stay
// listed for revocation, and disconnect, which drops every parked row.
func TestConnectorCompletionExpiryAndDisconnect(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	completion := testConnectorCompletion()
	expired := completion
	expired.Nonce = "completion-expired"
	expired.ExpiresAt = time.Now().Add(-time.Second)
	if err := s.CreateConnectorCompletion(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeConnectorCompletion(ctx, expired.Nonce); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired consume err = %v", err)
	}
	// Expired rows cannot be redeemed but remain listed for revocation until
	// the reconciler deletes them.
	expiredToo := expired
	expiredToo.Nonce = "completion-expired-2"
	if err := s.CreateConnectorCompletion(ctx, expiredToo); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PeekConnectorCompletion(ctx, expiredToo.Nonce); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired peek err = %v", err)
	}
	expiredListed, listErr := s.ListConnectorCompletionsForConnection(ctx, completion.ConnectionUID)
	if listErr != nil || len(expiredListed) != 1 || expiredListed[0].Nonce != expiredToo.Nonce || expiredListed[0].Credential.AccessToken != "gho_parked" {
		t.Fatalf("expired listing = %+v err = %v", expiredListed, listErr)
	}
	if err := s.DeleteConnectorCompletion(ctx, expiredToo.Nonce); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateConnectorCompletion(ctx, store.ConnectorCompletion{Nonce: "x"}); err == nil {
		t.Fatal("incomplete completion must be rejected")
	}
	// Disconnect drops parked completions too.
	parked := completion
	parked.Nonce = "completion-parked"
	if err := s.CreateConnectorCompletion(ctx, parked); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteConnectorConsentsForConnection(ctx, completion.ConnectionUID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeConnectorCompletion(ctx, parked.Nonce); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("completions must be gone after disconnect")
	}
}

func TestConnectorCredentialTombstoneBlocksRecreation(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	completion := store.ConnectorCompletion{
		Nonce: "completion-t", ConnectionUID: "uid-1", Namespace: "tenant", Name: "github-abc",
		SubjectDigest: "digest-a", Provider: "github", Mode: "readOnly",
		Credential: store.ConnectorCredential{AccessToken: "gho_parked"},
		ExpiresAt:  time.Now().Add(10 * time.Minute),
	}
	// Deleting custody tombstones the UID permanently.
	ref := testCredentialRef()
	if err := s.PutConnectorCredential(ctx, ref, store.ConnectorCredential{AccessToken: "gho"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteConnectorCredential(ctx, ref.ConnectionUID); err != nil {
		t.Fatal(err)
	}
	if err := s.PutConnectorCredential(ctx, ref, store.ConnectorCredential{AccessToken: "gho_again"}); !errors.Is(err, store.ErrConnectorCustodyTombstoned) {
		t.Fatalf("put after delete err = %v, want ErrConnectorCustodyTombstoned", err)
	}
	if _, err := s.GetConnectorCredential(ctx, ref); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("tombstoned get err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteConnectorCredential(ctx, ref.ConnectionUID); err != nil {
		t.Fatalf("repeated delete must be idempotent: %v", err)
	}
	parkedAfterDelete := completion
	parkedAfterDelete.Nonce = "completion-after-delete"
	if err := s.CreateConnectorCompletion(ctx, parkedAfterDelete); !errors.Is(err, store.ErrConnectorCustodyTombstoned) {
		t.Fatalf("completion after delete err = %v, want ErrConnectorCustodyTombstoned", err)
	}
	fresh := ref
	fresh.ConnectionUID = "uid-2"
	if err := s.PutConnectorCredential(ctx, fresh, store.ConnectorCredential{AccessToken: "gho"}); err != nil {
		t.Fatalf("a new UID must not be affected by another UID's tombstone: %v", err)
	}
}

// TestConnectorCompletionAuthenticatesPlaintextColumns covers a parked row
// whose mode column was altered: the sealed payload no longer opens, so the
// completion fence cannot be widened from readOnly to readWrite by editing
// the store.
func TestConnectorCompletionAuthenticatesPlaintextColumns(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	completion := testConnectorCompletion()
	if err := s.CreateConnectorCompletion(ctx, completion); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE connector_completions SET mode = 'readWrite' WHERE nonce = ?`, completion.Nonce); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PeekConnectorCompletion(ctx, completion.Nonce); err == nil {
		t.Fatal("a completion whose mode column changed must not open")
	}
	if _, err := s.ConsumeConnectorCompletion(ctx, completion.Nonce); err == nil {
		t.Fatal("a completion whose mode column changed must not be consumable")
	}
}

// TestConnectorConsentAuthenticatesPlaintextColumns covers a pending consent
// whose authority digest column was altered: the verifier no longer opens, so
// the callback cannot be steered to a replaced token endpoint.
func TestConnectorConsentAuthenticatesPlaintextColumns(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	consent := store.ConnectorConsent{
		Nonce: "nonce-aad", ConnectionUID: "uid-1", Namespace: "tenant", Name: "github-abc", AuthorityDigest: "authority-1",
		SubjectDigest: "digest-a", Provider: "github", Mode: "readOnly", CodeVerifier: "verifier", ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	if err := s.CreateConnectorConsent(ctx, consent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE connector_consents SET authority_digest = 'authority-2' WHERE nonce = ?`, consent.Nonce); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeConnectorConsent(ctx, consent.Nonce); err == nil {
		t.Fatal("a consent whose authority digest changed must not open")
	}
}

func TestConnectorCompletionCommitMarksRowAtomically(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	completion := testConnectorCompletion()
	if err := s.CreateConnectorCompletion(ctx, completion); err != nil {
		t.Fatal(err)
	}
	ref := store.ConnectorCredentialRef{ConnectionUID: completion.ConnectionUID, Namespace: completion.Namespace, Name: completion.Name, SubjectDigest: completion.SubjectDigest, Provider: completion.Provider}
	if _, err := s.CommitConnectorCompletion(ctx, completion.Nonce, ref, completion.Credential); err != nil {
		t.Fatalf("CommitConnectorCompletion: %v", err)
	}
	peeked, err := s.PeekConnectorCompletion(ctx, completion.Nonce)
	if err != nil || !peeked.Committed || peeked.ConsentAuthorityDigest != "consent-authority-1" {
		t.Fatalf("peek after commit = %+v err = %v, want committed with the consent authority kept", peeked, err)
	}
	held, err := s.GetConnectorCredential(ctx, ref)
	if err != nil || held.AccessToken != "gho_parked" {
		t.Fatalf("custody after commit = %+v err = %v", held, err)
	}
	if _, err := s.CommitConnectorCompletion(ctx, "missing", ref, completion.Credential); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("commit of a missing completion err = %v, want ErrNotFound", err)
	}
	// A missing completion leaves custody untouched: the transaction rolled back.
	if held, err := s.GetConnectorCredential(ctx, ref); err != nil || held.AccessToken != "gho_parked" {
		t.Fatalf("custody after rolled-back commit = %+v err = %v", held, err)
	}
	// The committed marker lives inside the sealed body: the row has no
	// plaintext column to flip, and the re-sealed payload still opens only
	// against its fence columns.
	if _, err := s.db.Exec(`UPDATE connector_completions SET mode = 'readWrite' WHERE nonce = ?`, completion.Nonce); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PeekConnectorCompletion(ctx, completion.Nonce); err == nil {
		t.Fatal("a committed completion whose fence column changed must not open")
	}
}

// TestConnectorCommitRetiresReplacedCredential covers a re-authorization:
// the credential a commit replaces stays sealed for disconnect to revoke,
// and disconnect removes it with custody.
func TestConnectorCommitRetiresReplacedCredential(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	completion := testConnectorCompletion()
	ref := store.ConnectorCredentialRef{ConnectionUID: completion.ConnectionUID, Namespace: completion.Namespace, Name: completion.Name, SubjectDigest: completion.SubjectDigest, Provider: completion.Provider}
	if err := s.PutConnectorCredential(ctx, ref, store.ConnectorCredential{AccessToken: "gho_first", RefreshToken: "ghr_first"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateConnectorCompletion(ctx, completion); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitConnectorCompletion(ctx, completion.Nonce, ref, completion.Credential); err != nil {
		t.Fatal(err)
	}
	retired, err := s.ListRetiredConnectorCredentials(ctx, ref)
	if err != nil || len(retired) != 1 || retired[0].AccessToken != "gho_first" || retired[0].RefreshToken != "ghr_first" {
		t.Fatalf("retired = %+v err = %v, want the replaced credential", retired, err)
	}
	if held, err := s.GetConnectorCredential(ctx, ref); err != nil || held.AccessToken != "gho_parked" {
		t.Fatalf("custody = %+v err = %v", held, err)
	}
	if err := s.DeleteConnectorCredential(ctx, ref.ConnectionUID); err != nil {
		t.Fatal(err)
	}
	if retired, err := s.ListRetiredConnectorCredentials(ctx, ref); err != nil || len(retired) != 0 {
		t.Fatalf("retired after disconnect = %+v err = %v, want none", retired, err)
	}
}

func TestConnectorTombstoneFencesCommitsBeforeDeletion(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	completion := testConnectorCompletion()
	ref := store.ConnectorCredentialRef{ConnectionUID: completion.ConnectionUID, Namespace: completion.Namespace, Name: completion.Name, SubjectDigest: completion.SubjectDigest, Provider: completion.Provider}
	if err := s.PutConnectorCredential(ctx, ref, store.ConnectorCredential{AccessToken: "gho_a", RefreshToken: "ghr_a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateConnectorCompletion(ctx, completion); err != nil {
		t.Fatal(err)
	}
	if err := s.TombstoneConnectorCustody(ctx, ref.ConnectionUID); err != nil {
		t.Fatal(err)
	}
	if err := s.TombstoneConnectorCustody(ctx, ref.ConnectionUID); err != nil {
		t.Fatalf("tombstoning twice must be idempotent: %v", err)
	}
	// The material stays readable for revocation while new commits fail.
	if held, err := s.GetConnectorCredential(ctx, ref); err != nil || held.AccessToken != "gho_a" {
		t.Fatalf("custody after tombstone = %+v err = %v", held, err)
	}
	if _, err := s.CommitConnectorCompletion(ctx, completion.Nonce, ref, completion.Credential); !errors.Is(err, store.ErrConnectorCustodyTombstoned) {
		t.Fatalf("commit after tombstone err = %v, want ErrConnectorCustodyTombstoned", err)
	}
	if err := s.DeleteConnectorCredential(ctx, ref.ConnectionUID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetConnectorCredential(ctx, ref); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("custody after delete err = %v, want ErrNotFound", err)
	}
}

func TestConnectorCommitDoesNotRetireIdenticalMaterial(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	completion := testConnectorCompletion()
	ref := store.ConnectorCredentialRef{ConnectionUID: completion.ConnectionUID, Namespace: completion.Namespace, Name: completion.Name, SubjectDigest: completion.SubjectDigest, Provider: completion.Provider}
	// The provider re-issues the same long-lived tokens on every re-consent.
	if err := s.PutConnectorCredential(ctx, ref, completion.Credential); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		completion.Nonce = fmt.Sprintf("nonce-%d", i)
		completion.ConsentSequence = testConsentSequence.Add(1)
		if err := s.CreateConnectorCompletion(ctx, completion); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CommitConnectorCompletion(ctx, completion.Nonce, ref, completion.Credential); err != nil {
			t.Fatal(err)
		}
	}
	if retired, err := s.ListRetiredConnectorCredentials(ctx, ref); err != nil || len(retired) != 0 {
		t.Fatalf("retired = %+v err = %v, want identical material not duplicated", retired, err)
	}
	// Genuinely different material is still kept for revocation.
	completion.Nonce = "nonce-new"
	completion.ConsentSequence = testConsentSequence.Add(1)
	completion.Credential.AccessToken = "gho_different"
	if err := s.CreateConnectorCompletion(ctx, completion); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitConnectorCompletion(ctx, completion.Nonce, ref, completion.Credential); err != nil {
		t.Fatal(err)
	}
	if retired, err := s.ListRetiredConnectorCredentials(ctx, ref); err != nil || len(retired) != 1 || retired[0].AccessToken != "gho_parked" {
		t.Fatalf("retired = %+v err = %v, want the distinct predecessor", retired, err)
	}
}

// TestConnectorCustodyAssignsMonotonicGrants covers the grant sequence: every
// committed grant takes the next number for the Connection, the counter
// outlives the credential row, and the number is read back with custody.
func TestConnectorCustodyAssignsMonotonicGrants(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	completion := testConnectorCompletion()
	ref := store.ConnectorCredentialRef{ConnectionUID: completion.ConnectionUID, Namespace: completion.Namespace, Name: completion.Name, SubjectDigest: completion.SubjectDigest, Provider: completion.Provider}
	if err := s.PutConnectorCredential(ctx, ref, store.ConnectorCredential{AccessToken: "gho_1"}); err != nil {
		t.Fatal(err)
	}
	if held, err := s.GetConnectorCredential(ctx, ref); err != nil || held.GrantSequence != 1 {
		t.Fatalf("first grant = %+v err = %v, want 1", held, err)
	}
	if err := s.CreateConnectorCompletion(ctx, completion); err != nil {
		t.Fatal(err)
	}
	committed, err := s.CommitConnectorCompletion(ctx, completion.Nonce, ref, completion.Credential)
	if err != nil || committed.GrantSequence != 2 || committed.AccessToken != "gho_parked" {
		t.Fatalf("committed = %+v err = %v, want grant 2", committed, err)
	}
	if held, err := s.GetConnectorCredential(ctx, ref); err != nil || held.GrantSequence != 2 {
		t.Fatalf("custody after commit = %+v err = %v, want grant 2", held, err)
	}
	// A row that goes away without a disconnect (a shred) does not reset
	// the counter: the next grant is still later than every earlier one.
	if _, err := s.db.Exec(`DELETE FROM connector_credentials WHERE connection_uid = ?`, ref.ConnectionUID); err != nil {
		t.Fatal(err)
	}
	if err := s.PutConnectorCredential(ctx, ref, store.ConnectorCredential{AccessToken: "gho_3"}); err != nil {
		t.Fatal(err)
	}
	if held, err := s.GetConnectorCredential(ctx, ref); err != nil || held.GrantSequence != 3 {
		t.Fatalf("grant after shred = %+v err = %v, want 3", held, err)
	}
}

// TestConnectorConsentReplacesPendingConsent covers one pending consent per
// Connection: a new authorize replaces the earlier one, so repeated calls
// never grow the table, and a committed completion carries its grant.
func TestConnectorConsentReplacesPendingConsent(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	first := testConnectorConsent()
	if err := s.CreateConnectorConsent(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := testConnectorConsent()
	second.Nonce = "nonce-second"
	if err := s.CreateConnectorConsent(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeConnectorConsent(ctx, first.Nonce); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("first consent after a second authorize err = %v, want ErrNotFound", err)
	}
	if _, err := s.ConsumeConnectorConsent(ctx, second.Nonce); err != nil {
		t.Fatalf("second consent must be usable: %v", err)
	}
	other := testConnectorConsent()
	other.Nonce, other.ConnectionUID, other.Name = "nonce-other", "uid-2", "github-def"
	if err := s.CreateConnectorConsent(ctx, other); err != nil {
		t.Fatal(err)
	}
	third := testConnectorConsent()
	third.Nonce = "nonce-third"
	if err := s.CreateConnectorConsent(ctx, third); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeConnectorConsent(ctx, other.Nonce); err != nil {
		t.Fatalf("another Connection's consent must survive: %v", err)
	}
	completion := testConnectorCompletion()
	if err := s.CreateConnectorCompletion(ctx, completion); err != nil {
		t.Fatal(err)
	}
	ref := store.ConnectorCredentialRef{ConnectionUID: completion.ConnectionUID, Namespace: completion.Namespace, Name: completion.Name, SubjectDigest: completion.SubjectDigest, Provider: completion.Provider}
	if _, err := s.CommitConnectorCompletion(ctx, completion.Nonce, ref, completion.Credential); err != nil {
		t.Fatal(err)
	}
	if peeked, err := s.PeekConnectorCompletion(ctx, completion.Nonce); err != nil || peeked.Credential.GrantSequence != 1 {
		t.Fatalf("committed completion = %+v err = %v, want its grant sealed with it", peeked, err)
	}
}

func testConnectorConsent() store.ConnectorConsent {
	return store.ConnectorConsent{
		Nonce: "nonce-1", ConnectionUID: "uid-1", Namespace: "tenant", Name: "github-abc", AuthorityDigest: "authority-1", Scopes: []string{"read:user", "repo"},
		SubjectDigest: "digest-a", Provider: "github", Mode: "readOnly", CodeVerifier: "verifier-secret",
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}
}

// TestConnectorCommitIsFencedInsideTheTransaction covers two API replicas
// committing one completion: the second commit sees the sealed committed
// marker inside its own transaction and changes nothing, so custody keeps
// one grant. Old tombstones are reaped with their grant counters.
func TestConnectorCommitIsFencedInsideTheTransaction(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	completion := testConnectorCompletion()
	if err := s.CreateConnectorCompletion(ctx, completion); err != nil {
		t.Fatal(err)
	}
	ref := store.ConnectorCredentialRef{ConnectionUID: completion.ConnectionUID, Namespace: completion.Namespace, Name: completion.Name, SubjectDigest: completion.SubjectDigest, Provider: completion.Provider}
	if _, err := s.CommitConnectorCompletion(ctx, completion.Nonce, ref, completion.Credential); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitConnectorCompletion(ctx, completion.Nonce, ref, completion.Credential); !errors.Is(err, store.ErrConnectorCompletionCommitted) {
		t.Fatalf("second commit err = %v, want ErrConnectorCompletionCommitted", err)
	}
	if held, err := s.GetConnectorCredential(ctx, ref); err != nil || held.GrantSequence != 1 {
		t.Fatalf("custody after a repeated commit = %+v err = %v, want one grant", held, err)
	}
	old := time.Now().Add(-2 * connectorTombstoneRetention).UTC()
	if _, err := s.db.Exec(`INSERT INTO connector_credential_tombstones (connection_uid, deleted_at) VALUES ('uid-old', ?)`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO connector_credential_grants (connection_uid, grant_sequence) VALUES ('uid-old', 7)`); err != nil {
		t.Fatal(err)
	}
	if err := s.TombstoneConnectorCustody(ctx, "uid-recent"); err != nil {
		t.Fatal(err)
	}
	var tombstones, grants int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM connector_credential_tombstones WHERE connection_uid = 'uid-old'`).Scan(&tombstones); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM connector_credential_grants WHERE connection_uid = 'uid-old'`).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if tombstones != 0 || grants != 0 {
		t.Fatalf("old tombstone rows = %d grant rows = %d, want both reaped", tombstones, grants)
	}
	if err := s.PutConnectorCredential(ctx, store.ConnectorCredentialRef{ConnectionUID: "uid-recent", Namespace: "tenant", Name: "n", SubjectDigest: "d", Provider: "github"}, store.ConnectorCredential{AccessToken: "x"}); !errors.Is(err, store.ErrConnectorCustodyTombstoned) {
		t.Fatalf("a fresh tombstone must still fence: err = %v", err)
	}
}

// TestConnectorKeyActivationAuthenticatesCustody covers key rotation while
// linked accounts exist: a candidate key that cannot open the retained
// connector rows is refused even when no execution snapshot is retained,
// and the current key still activates.
func TestConnectorKeyActivationAuthenticatesCustody(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	completion := testConnectorCompletion()
	ref := store.ConnectorCredentialRef{ConnectionUID: completion.ConnectionUID, Namespace: completion.Namespace, Name: completion.Name, SubjectDigest: completion.SubjectDigest, Provider: completion.Provider}
	if err := s.PutConnectorCredential(ctx, ref, store.ConnectorCredential{AccessToken: "gho_1"}); err != nil {
		t.Fatal(err)
	}
	other, err := NewAgentExecutionSnapshotCipher(bytes.Repeat([]byte{0x24}, AgentExecutionSnapshotKeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAgentExecutionSnapshotCipher(other); err == nil || !strings.Contains(err.Error(), "connector custody") {
		t.Fatalf("rotation over linked custody err = %v, want refusal", err)
	}
	same, err := NewAgentExecutionSnapshotCipher(bytes.Repeat([]byte{0x42}, AgentExecutionSnapshotKeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAgentExecutionSnapshotCipher(same); err != nil {
		t.Fatalf("the current key must activate: %v", err)
	}
	// Pending rows are sealed under the key too.
	if err := s.DeleteConnectorCredential(ctx, ref.ConnectionUID); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateConnectorConsent(ctx, testConnectorConsent()); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAgentExecutionSnapshotCipher(other); err == nil || !strings.Contains(err.Error(), "consent") {
		t.Fatalf("rotation over a pending consent err = %v, want refusal", err)
	}
}

// TestConnectorCommitJudgesExpiryAndRevocationIdentity covers a commit that
// stalled past the completion's lifetime (refused and the row dropped) and
// a re-consent that re-issued the same token strings under another
// revocation identity (the previous row is retired, not deduplicated).
func TestConnectorCommitJudgesExpiryAndRevocationIdentity(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	expired := testConnectorCompletion()
	expired.Nonce = "nonce-expired"
	expired.ExpiresAt = time.Now().Add(time.Second)
	if err := s.CreateConnectorCompletion(ctx, expired); err != nil {
		t.Fatal(err)
	}
	ref := store.ConnectorCredentialRef{ConnectionUID: expired.ConnectionUID, Namespace: expired.Namespace, Name: expired.Name, SubjectDigest: expired.SubjectDigest, Provider: expired.Provider}
	if _, err := s.db.Exec(`UPDATE connector_completions SET expires_at = ? WHERE nonce = ?`, time.Now().Add(-time.Minute).UTC(), expired.Nonce); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitConnectorCompletion(ctx, expired.Nonce, ref, expired.Credential); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("commit past expiry err = %v, want ErrNotFound", err)
	}
	if _, err := s.GetConnectorCredential(ctx, ref); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("custody after an expired commit err = %v, want nothing stored", err)
	}
	if _, err := s.PeekConnectorCompletion(ctx, expired.Nonce); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired completion after commit attempt err = %v, want dropped", err)
	}

	first := store.ConnectorCredential{AccessToken: "gho_same", RefreshToken: "ghr_same", RevocationDigest: "revocation-a"}
	if err := s.PutConnectorCredential(ctx, ref, first); err != nil {
		t.Fatal(err)
	}
	completion := testConnectorCompletion()
	completion.Credential = store.ConnectorCredential{AccessToken: "gho_same", RefreshToken: "ghr_same", RevocationDigest: "revocation-b"}
	if err := s.CreateConnectorCompletion(ctx, completion); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitConnectorCompletion(ctx, completion.Nonce, ref, completion.Credential); err != nil {
		t.Fatal(err)
	}
	retired, err := s.ListRetiredConnectorCredentials(ctx, ref)
	if err != nil || len(retired) != 1 || retired[0].RevocationDigest != "revocation-a" {
		t.Fatalf("retired = %+v err = %v, want the same tokens kept under their previous revocation identity", retired, err)
	}
}

// TestConnectorDeletionLeavesNoCiphertextInFiles covers the crypto-shred:
// after a disconnect neither the database file nor its write-ahead log
// still holds the deleted row's wrapped data key or ciphertext.
func TestConnectorDeletionLeavesNoCiphertextInFiles(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	completion := testConnectorCompletion()
	ref := store.ConnectorCredentialRef{ConnectionUID: completion.ConnectionUID, Namespace: completion.Namespace, Name: completion.Name, SubjectDigest: completion.SubjectDigest, Provider: completion.Provider}
	if err := s.PutConnectorCredential(ctx, ref, store.ConnectorCredential{AccessToken: "gho_shred_me", RefreshToken: "ghr_shred_me"}); err != nil {
		t.Fatal(err)
	}
	var dekCiphertext, ciphertext []byte
	if err := s.db.QueryRow(`SELECT dek_ciphertext, ciphertext FROM connector_credentials WHERE connection_uid = ?`, ref.ConnectionUID).Scan(&dekCiphertext, &ciphertext); err != nil {
		t.Fatal(err)
	}
	// A parked completion is sealed under the controller key itself, so its
	// payload must leave the files with the disconnect as well; the
	// finalizer deletes custody first and the parked rows after it.
	if err := s.CreateConnectorCompletion(ctx, completion); err != nil {
		t.Fatal(err)
	}
	var payload []byte
	if err := s.db.QueryRow(`SELECT payload FROM connector_completions WHERE nonce = ?`, completion.Nonce).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteConnectorCredential(ctx, ref.ConnectionUID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteConnectorConsentsForConnection(ctx, ref.ConnectionUID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{s.dbPath, s.dbPath + "-wal"} {
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		for name, sealed := range map[string][]byte{"wrapped key": dekCiphertext, "ciphertext": ciphertext, "completion payload": payload} {
			if bytes.Contains(data, sealed) {
				t.Fatalf("%s still holds the deleted %s", path, name)
			}
		}
	}
}

// TestConnectorCompletionDropsLeaveNoLogCopy covers the paths that delete
// a parked completion outside a disconnect: an expired one met at commit
// and one consumed. Its sealed payload must leave the write-ahead log too.
func TestConnectorCompletionDropsLeaveNoLogCopy(t *testing.T) {
	ctx := context.Background()
	for name, drop := range map[string]func(*Store, store.ConnectorCompletion) error{
		"expired at commit": func(s *Store, completion store.ConnectorCompletion) error {
			ref := store.ConnectorCredentialRef{ConnectionUID: completion.ConnectionUID, Namespace: completion.Namespace, Name: completion.Name, SubjectDigest: completion.SubjectDigest, Provider: completion.Provider}
			if _, err := s.CommitConnectorCompletion(ctx, completion.Nonce, ref, completion.Credential); !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("expired commit err = %v, want ErrNotFound", err)
			}
			return nil
		},
		"consumed": func(s *Store, completion store.ConnectorCompletion) error {
			_, err := s.ConsumeConnectorCompletion(ctx, completion.Nonce)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newConnectorTestStore(t)
			completion := testConnectorCompletion()
			if name == "expired at commit" {
				completion.ExpiresAt = time.Now().Add(-time.Second)
			}
			if err := s.CreateConnectorCompletion(ctx, completion); err != nil {
				t.Fatal(err)
			}
			var payload []byte
			if err := s.db.QueryRow(`SELECT payload FROM connector_completions WHERE nonce = ?`, completion.Nonce).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			if err := drop(s, completion); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{s.dbPath, s.dbPath + "-wal"} {
				data, err := os.ReadFile(path)
				if err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				if bytes.Contains(data, payload) {
					t.Fatalf("%s still holds the dropped completion payload", path)
				}
			}
		})
	}
}

// TestConnectorCommitDropsSupersededCompletions covers two parked
// completions for one Connection (an older consent was approved, then a
// newer one): once the newer one commits, the older one can never be
// committed over it, while an older committed recovery record is kept.
func TestConnectorCommitDropsSupersededCompletions(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	committedOld := testConnectorCompletion()
	committedOld.Nonce = "committed-old"
	older := testConnectorCompletion()
	older.Nonce = "older"
	older.Credential.AccessToken = "gho_older"
	newer := testConnectorCompletion()
	newer.Nonce = "newer"
	newer.Credential.AccessToken = "gho_newer"
	ref := store.ConnectorCredentialRef{ConnectionUID: newer.ConnectionUID, Namespace: newer.Namespace, Name: newer.Name, SubjectDigest: newer.SubjectDigest, Provider: newer.Provider}
	if err := s.CreateConnectorCompletion(ctx, committedOld); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitConnectorCompletion(ctx, committedOld.Nonce, ref, committedOld.Credential); err != nil {
		t.Fatal(err)
	}
	for _, completion := range []store.ConnectorCompletion{older, newer} {
		if err := s.CreateConnectorCompletion(ctx, completion); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CommitConnectorCompletion(ctx, newer.Nonce, ref, newer.Credential); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitConnectorCompletion(ctx, older.Nonce, ref, older.Credential); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("commit of a superseded completion err = %v, want ErrNotFound", err)
	}
	if held, err := s.GetConnectorCredential(ctx, ref); err != nil || held.AccessToken != "gho_newer" {
		t.Fatalf("custody = %+v err = %v, want the newer grant kept", held, err)
	}
	if peeked, err := s.PeekConnectorCompletion(ctx, committedOld.Nonce); err != nil || !peeked.Committed {
		t.Fatalf("older committed record = %+v err = %v, want it kept for status recovery", peeked, err)
	}
}

// TestConnectorConsentsAreNumberedAndFenceCompletions covers overlapping
// authorization flows whose callbacks finish out of order: consent order,
// not callback arrival, decides which tokens may become custody.
func TestConnectorConsentsAreNumberedAndFenceCompletions(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	first := testConnectorConsent()
	if err := s.CreateConnectorConsent(ctx, first); err != nil {
		t.Fatal(err)
	}
	consumedFirst, err := s.ConsumeConnectorConsent(ctx, first.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	second := testConnectorConsent()
	second.Nonce = "nonce-2"
	if err := s.CreateConnectorConsent(ctx, second); err != nil {
		t.Fatal(err)
	}
	consumedSecond, err := s.ConsumeConnectorConsent(ctx, second.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if consumedFirst.Sequence < 1 || consumedSecond.Sequence <= consumedFirst.Sequence {
		t.Fatalf("sequences = %d, %d, want increasing from 1", consumedFirst.Sequence, consumedSecond.Sequence)
	}
	park := func(nonce, token string, sequence int64) error {
		completion := testConnectorCompletion()
		completion.Nonce, completion.Credential.AccessToken, completion.ConsentSequence = nonce, token, sequence
		return s.CreateConnectorCompletion(ctx, completion)
	}
	ref := store.ConnectorCredentialRef{ConnectionUID: "uid-1", Namespace: "tenant", Name: "github-abc", SubjectDigest: "digest-a", Provider: "github"}

	// The newer consent's callback parks first; the older one, arriving
	// later, is refused instead of replacing it.
	if err := park("newer", "gho_newer", consumedSecond.Sequence); err != nil {
		t.Fatal(err)
	}
	if err := park("older", "gho_older", consumedFirst.Sequence); !errors.Is(err, store.ErrConnectorConsentSuperseded) {
		t.Fatalf("older park after newer err = %v, want ErrConnectorConsentSuperseded", err)
	}
	if _, err := s.CommitConnectorCompletion(ctx, "newer", ref, store.ConnectorCredential{AccessToken: "gho_newer"}); err != nil {
		t.Fatal(err)
	}
	// Once the newer consent is custody, no older consent can park again.
	if err := park("older-again", "gho_older", consumedFirst.Sequence); !errors.Is(err, store.ErrConnectorConsentSuperseded) {
		t.Fatalf("older park after commit err = %v, want ErrConnectorConsentSuperseded", err)
	}
	if held, err := s.GetConnectorCredential(ctx, ref); err != nil || held.AccessToken != "gho_newer" {
		t.Fatalf("custody = %+v err = %v, want the newer grant", held, err)
	}

	// A newer park replaces an older uncommitted one, so a Connection holds
	// at most one uncommitted completion.
	if err := park("third", "gho_third", consumedSecond.Sequence+1); err != nil {
		t.Fatal(err)
	}
	if err := park("fourth", "gho_fourth", consumedSecond.Sequence+2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PeekConnectorCompletion(ctx, "third"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("replaced completion peek err = %v, want ErrNotFound", err)
	}
}

// TestConnectorCommitBoundsRetainedGrants covers repeated relinking: the
// superseded grants kept for revocation are bounded, and a commit past the
// bound is refused with custody unchanged.
func TestConnectorCommitBoundsRetainedGrants(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	ref := store.ConnectorCredentialRef{ConnectionUID: "uid-1", Namespace: "tenant", Name: "github-abc", SubjectDigest: "digest-a", Provider: "github"}
	var last string
	for i := range maxRetiredConnectorCredentials + 2 {
		completion := testConnectorCompletion()
		completion.Nonce = fmt.Sprintf("grant-%d", i)
		completion.Credential.AccessToken = fmt.Sprintf("gho_%d", i)
		if err := s.CreateConnectorCompletion(ctx, completion); err != nil {
			t.Fatal(err)
		}
		_, err := s.CommitConnectorCompletion(ctx, completion.Nonce, ref, completion.Credential)
		if i <= maxRetiredConnectorCredentials {
			if err != nil {
				t.Fatalf("grant %d: %v", i, err)
			}
			last = completion.Credential.AccessToken
			continue
		}
		if !errors.Is(err, store.ErrConnectorRetiredLimit) {
			t.Fatalf("grant %d err = %v, want ErrConnectorRetiredLimit", i, err)
		}
	}
	if held, err := s.GetConnectorCredential(ctx, ref); err != nil || held.AccessToken != last {
		t.Fatalf("custody = %+v err = %v, want %s kept", held, err, last)
	}
	if retired, err := s.ListRetiredConnectorCredentials(ctx, ref); err != nil || len(retired) != maxRetiredConnectorCredentials {
		t.Fatalf("retired = %d err = %v, want %d", len(retired), err, maxRetiredConnectorCredentials)
	}
}

// TestConnectorKeyActivationSkipsExpiredConsents covers key rotation after a
// pending consent's TTL passed: it holds no token and can never be used, so
// it does not block activating a key that cannot open it.
func TestConnectorKeyActivationSkipsExpiredConsents(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	expired := testConnectorConsent()
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	if err := s.CreateConnectorConsent(ctx, expired); err != nil {
		t.Fatal(err)
	}
	other, err := NewAgentExecutionSnapshotCipher(bytes.Repeat([]byte{0x24}, AgentExecutionSnapshotKeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAgentExecutionSnapshotCipher(other); err != nil {
		t.Fatalf("an expired consent must not block key activation: %v", err)
	}
}
