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
	"path/filepath"
	"strings"
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
		Nonce: "nonce-1", ConnectionUID: "uid-1", Namespace: "tenant", Name: "github-abc", AuthorityDigest: "authority-1", Scopes: []string{"read:user", "repo"},
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
		got.SubjectDigest != consent.SubjectDigest || got.AuthorityDigest != "authority-1" || strings.Join(got.Scopes, " ") != "read:user repo" {
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

func testConnectorCompletion() store.ConnectorCompletion {
	return store.ConnectorCompletion{
		Nonce: "completion-1", ConnectionUID: "uid-1", Namespace: "tenant", Name: "github-abc",
		SubjectDigest: "digest-a", Provider: "github", Mode: "readOnly", ConsentAuthorityDigest: "consent-authority-1",
		Credential: store.ConnectorCredential{AccessToken: "gho_parked", RefreshToken: "ghr_parked", Scopes: []string{"repo"}, AuthorityDigest: "authority-1"},
		ExpiresAt:  time.Now().Add(10 * time.Minute),
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
	if err := s.CommitConnectorCompletion(ctx, completion.Nonce, ref, completion.Credential); err != nil {
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
	if err := s.CommitConnectorCompletion(ctx, "missing", ref, completion.Credential); !errors.Is(err, store.ErrNotFound) {
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
	if err := s.CommitConnectorCompletion(ctx, completion.Nonce, ref, completion.Credential); err != nil {
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
	if err := s.CommitConnectorCompletion(ctx, completion.Nonce, ref, completion.Credential); !errors.Is(err, store.ErrConnectorCustodyTombstoned) {
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
		if err := s.CreateConnectorCompletion(ctx, completion); err != nil {
			t.Fatal(err)
		}
		if err := s.CommitConnectorCompletion(ctx, completion.Nonce, ref, completion.Credential); err != nil {
			t.Fatal(err)
		}
	}
	if retired, err := s.ListRetiredConnectorCredentials(ctx, ref); err != nil || len(retired) != 0 {
		t.Fatalf("retired = %+v err = %v, want identical material not duplicated", retired, err)
	}
	// Genuinely different material is still kept for revocation.
	completion.Nonce = "nonce-new"
	completion.Credential.AccessToken = "gho_different"
	if err := s.CreateConnectorCompletion(ctx, completion); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitConnectorCompletion(ctx, completion.Nonce, ref, completion.Credential); err != nil {
		t.Fatal(err)
	}
	if retired, err := s.ListRetiredConnectorCredentials(ctx, ref); err != nil || len(retired) != 1 || retired[0].AccessToken != "gho_parked" {
		t.Fatalf("retired = %+v err = %v, want the distinct predecessor", retired, err)
	}
}
func TestConnectorCommitKeepsReplacedGrantWithRefreshTokenUntilDisconnect(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Minute).UTC()
	future := time.Now().Add(time.Hour).UTC()
	commit := func(completion store.ConnectorCompletion, ref store.ConnectorCredentialRef) {
		t.Helper()
		if err := s.CreateConnectorCompletion(ctx, completion); err != nil {
			t.Fatal(err)
		}
		if err := s.CommitConnectorCompletion(ctx, completion.Nonce, ref, completion.Credential); err != nil {
			t.Fatal(err)
		}
	}
	// A re-consent replaces a grant whose access token has expired but
	// whose refresh token is still a live grant: it is kept for revocation
	// at disconnect and never pruned by access-token expiry.
	completion := testConnectorCompletion()
	ref := store.ConnectorCredentialRef{ConnectionUID: completion.ConnectionUID, Namespace: completion.Namespace, Name: completion.Name, SubjectDigest: completion.SubjectDigest, Provider: completion.Provider}
	if err := s.PutConnectorCredential(ctx, ref, store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", ExpiresAt: past}); err != nil {
		t.Fatal(err)
	}
	commit(completion, ref)
	retired, err := s.ListRetiredConnectorCredentials(ctx, ref)
	if err != nil || len(retired) != 1 || retired[0].RefreshToken != "ghr_old" {
		t.Fatalf("retired = %+v err = %v, want the expired grant kept for its refresh token", retired, err)
	}
	// A later refresh prunes nothing from that grant.
	held, err := s.GetConnectorCredential(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceConnectorCredential(ctx, ref, store.ConnectorCredential{AccessToken: "gho_refreshed", RefreshToken: "ghr_parked", ExpiresAt: future}, held.Version); err != nil {
		t.Fatal(err)
	}
	if retired, err := s.ListRetiredConnectorCredentials(ctx, ref); err != nil || len(retired) != 2 || retired[0].RefreshToken != "ghr_old" {
		t.Fatalf("retired after refresh = %+v err = %v, want the old grant kept and the refreshed-away token retired", retired, err)
	}
	// A replaced grant with an expired access token and no refresh token
	// has nothing left to revoke.
	second := testConnectorCompletion()
	second.Nonce, second.ConnectionUID, second.Name = "nonce-second", "uid-2", "github-def"
	secondRef := store.ConnectorCredentialRef{ConnectionUID: second.ConnectionUID, Namespace: second.Namespace, Name: second.Name, SubjectDigest: second.SubjectDigest, Provider: second.Provider}
	if err := s.PutConnectorCredential(ctx, secondRef, store.ConnectorCredential{AccessToken: "gho_expired", ExpiresAt: past}); err != nil {
		t.Fatal(err)
	}
	commit(second, secondRef)
	if retired, err := s.ListRetiredConnectorCredentials(ctx, secondRef); err != nil || len(retired) != 0 {
		t.Fatalf("retired = %+v err = %v, want nothing kept for an expired grant without a refresh token", retired, err)
	}
}

func TestConnectorReplaceRetiresUnexpiredPreviousCredential(t *testing.T) {
	s := newConnectorTestStore(t)
	ctx := context.Background()
	completion := testConnectorCompletion()
	ref := store.ConnectorCredentialRef{ConnectionUID: completion.ConnectionUID, Namespace: completion.Namespace, Name: completion.Name, SubjectDigest: completion.SubjectDigest, Provider: completion.Provider}
	future := time.Now().Add(time.Hour).UTC()
	past := time.Now().Add(-time.Minute).UTC()
	if err := s.PutConnectorCredential(ctx, ref, store.ConnectorCredential{AccessToken: "gho_a", RefreshToken: "ghr_a", ExpiresAt: future}); err != nil {
		t.Fatal(err)
	}
	held, err := s.GetConnectorCredential(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	// A stale writer retires nothing.
	if err := s.ReplaceConnectorCredential(ctx, ref, store.ConnectorCredential{AccessToken: "gho_stale", ExpiresAt: future}, held.Version+1); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale replace err = %v, want ErrConflict", err)
	}
	if retired, err := s.ListRetiredConnectorCredentials(ctx, ref); err != nil || len(retired) != 0 {
		t.Fatalf("retired after stale replace = %+v err = %v, want none", retired, err)
	}
	// The refreshed-away access token is still valid at the provider, so it
	// is kept for revocation.
	if err := s.ReplaceConnectorCredential(ctx, ref, store.ConnectorCredential{AccessToken: "gho_b", RefreshToken: "ghr_b", ExpiresAt: past}, held.Version); err != nil {
		t.Fatal(err)
	}
	retired, err := s.ListRetiredConnectorCredentials(ctx, ref)
	if err != nil || len(retired) != 1 || retired[0].AccessToken != "gho_a" || retired[0].RefreshToken != "ghr_a" {
		t.Fatalf("retired = %+v err = %v, want the replaced credential", retired, err)
	}
	// An already expired token has nothing left to revoke and is not kept.
	held, err = s.GetConnectorCredential(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceConnectorCredential(ctx, ref, store.ConnectorCredential{AccessToken: "gho_c", ExpiresAt: future}, held.Version); err != nil {
		t.Fatal(err)
	}
	if retired, err := s.ListRetiredConnectorCredentials(ctx, ref); err != nil || len(retired) != 1 || retired[0].AccessToken != "gho_a" {
		t.Fatalf("retired = %+v err = %v, want only the unexpired credential", retired, err)
	}
	// Retired material is pruned once it expires, so the table is bounded by
	// the provider's token lifetime rather than by refresh count.
	if _, err := s.db.ExecContext(ctx, `UPDATE connector_retired_credentials SET revocable_until = ? WHERE connection_uid = ?`, past, ref.ConnectionUID); err != nil {
		t.Fatal(err)
	}
	held, err = s.GetConnectorCredential(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceConnectorCredential(ctx, ref, store.ConnectorCredential{AccessToken: "gho_d", ExpiresAt: future}, held.Version); err != nil {
		t.Fatal(err)
	}
	if retired, err := s.ListRetiredConnectorCredentials(ctx, ref); err != nil || len(retired) != 1 || retired[0].AccessToken != "gho_c" {
		t.Fatalf("retired = %+v err = %v, want only the credential replaced last", retired, err)
	}
	if held, err := s.GetConnectorCredential(ctx, ref); err != nil || held.AccessToken != "gho_d" {
		t.Fatalf("custody = %+v err = %v", held, err)
	}
}
