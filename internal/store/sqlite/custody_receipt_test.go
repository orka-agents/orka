package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"os"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

var custodyTestNow = time.Date(2026, 9, 1, 10, 30, 0, 0, time.UTC)

func custodyTestCredential(uid, label string) storagev1.SealedConnectorCredential {
	return storagev1.SealedConnectorCredential{
		ConnectionUID: uid, Namespace: "ns", Name: "connection-" + uid, SubjectDigest: "subject", Provider: "github",
		DataKeyNonce: []byte("data key nonce " + label), DataKeyCiphertext: []byte("wrapped data key " + label),
		Nonce: []byte("nonce " + label), Ciphertext: []byte("sealed custody ciphertext " + label),
		ExpiresAt: custodyTestNow.Add(time.Hour), GrantSequence: 1,
	}
}

// custodyReceipt reads the receipt row an operation left.
func custodyReceipt(t *testing.T, s *Store, operationID string) (result []byte, scope string, cleared bool) {
	t.Helper()
	require.NoError(t, s.db.QueryRow(`SELECT result, custody_scope, result_cleared FROM operation_receipts WHERE operation_id = ?`,
		operationID).Scan(&result, &scope, &cleared))
	return result, scope, cleared
}

// receiptCopy is how a receipt result holds sealed bytes.
func receiptCopy(sealed []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(sealed))
}

// requireNoCopyInFiles checks that neither the database file nor its log
// holds any of the given byte strings.
func requireNoCopyInFiles(t *testing.T, s *Store, copies map[string][]byte) {
	t.Helper()
	for _, path := range []string{s.dbPath, s.dbPath + "-wal"} {
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		for name, sealed := range copies {
			require.Falsef(t, bytes.Contains(data, sealed), "%s still holds %s", path, name)
		}
	}
}

type putCall struct {
	ctx      context.Context
	envelope storagev1.WriteEnvelope
	request  storagev1.PutSealedConnectorCredentialRequest
	stored   *storagev1.SealedConnectorCredential
}

func putCustodyWithEnvelope(t *testing.T, s *Store, dataset, uid, label string) putCall {
	t.Helper()
	request := storagev1.PutSealedConnectorCredentialRequest{Credential: custodyTestCredential(uid, label), Now: custodyTestNow}
	ctx, envelope := testEnvelope(t, t.Context(), dataset, "PutSealedConnectorCredential", time.Now().UTC(), request)
	stored, err := s.PutSealedConnectorCredential(ctx, request)
	require.NoError(t, err)
	return putCall{ctx: ctx, envelope: envelope, request: request, stored: stored}
}

// Removing a Connection's custody clears the sealed results its receipts
// hold, in the removal's own transaction, and its log truncation removes their
// copies from the files. The receipts stay, so a replay never runs the write
// again: it returns ErrConflict. Another Connection's receipts are untouched.
func TestCustodyRemovalClearsSealedReceiptResults(t *testing.T) {
	for name, remove := range map[string]func(ctx context.Context, s *Store, put putCall) error{
		"shred": func(ctx context.Context, s *Store, put putCall) error {
			return s.ShredConnectorCredential(ctx, put.request.Credential.ConnectionUID, put.stored.Version)
		},
		"delete": func(ctx context.Context, s *Store, put putCall) error {
			return s.DeleteConnectorCredential(ctx, put.request.Credential.ConnectionUID)
		},
		"tombstone": func(ctx context.Context, s *Store, put putCall) error {
			return s.TombstoneConnectorCustody(ctx, put.request.Credential.ConnectionUID)
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			s := setupDiskStore(t)
			dataset := advanceTestEpoch(t, s, testWriteFence)
			own := putCustodyWithEnvelope(t, s, dataset, "connection-a", "own")
			other := putCustodyWithEnvelope(t, s, dataset, "connection-b", "other")

			result, scope, cleared := custodyReceipt(t, s, own.envelope.OperationID)
			require.Equal(t, "connection-a", scope)
			require.False(t, cleared)
			require.True(t, bytes.Contains(result, receiptCopy(own.request.Credential.Ciphertext)),
				"the receipt holds the sealed result before the removal")

			require.NoError(t, remove(ctx, s, own))

			result, scope, cleared = custodyReceipt(t, s, own.envelope.OperationID)
			require.Nil(t, result, "the removal clears the receipt's sealed result")
			require.Equal(t, "connection-a", scope)
			require.True(t, cleared)
			otherResult, otherScope, otherCleared := custodyReceipt(t, s, other.envelope.OperationID)
			require.Equal(t, "connection-b", otherScope)
			require.False(t, otherCleared, "another Connection's receipt is untouched")
			require.True(t, bytes.Contains(otherResult, receiptCopy(other.request.Credential.Ciphertext)))

			_, err := s.PutSealedConnectorCredential(own.ctx, own.request)
			require.ErrorIs(t, err, store.ErrConflict, "a replay after the custody went never runs the write again")
			replayed, err := s.PutSealedConnectorCredential(other.ctx, other.request)
			require.NoError(t, err)
			require.Equal(t, other.stored, replayed, "another Connection's replay returns its result")

			requireNoCopyInFiles(t, s, map[string][]byte{
				"the receipt's copy of the wrapped data key": receiptCopy(own.request.Credential.DataKeyCiphertext),
				"the receipt's copy of the ciphertext":       receiptCopy(own.request.Credential.Ciphertext),
			})
			if name != "tombstone" {
				requireNoCopyInFiles(t, s, map[string][]byte{
					"the removed wrapped data key": own.request.Credential.DataKeyCiphertext,
					"the removed ciphertext":       own.request.Credential.Ciphertext,
				})
			}
		})
	}
}

// Deleting a Connection's consents clears the sealed consent a consumed
// consent's receipt holds.
func TestConsentDeletionClearsConsumedConsentReceipts(t *testing.T) {
	ctx := t.Context()
	s := setupDiskStore(t)
	dataset := advanceTestEpoch(t, s, testWriteFence)
	consent := storagev1.SealedConnectorConsent{
		Nonce: "consent-a", ConnectionUID: "connection-a", Namespace: "ns", Name: "connection-a", SubjectDigest: "subject",
		Provider: "github", Mode: "readOnly", AuthorityDigest: "authority", RevocationDigest: "revocation",
		Scopes: []string{"repo"}, Sequence: 1, ExpiresAt: custodyTestNow.Add(10 * time.Minute),
		VerifierNonce: []byte("verifier nonce a"), VerifierCiphertext: []byte("sealed verifier ciphertext a"),
	}
	require.NoError(t, s.CreateSealedConnectorConsent(ctx, storagev1.CreateSealedConnectorConsentRequest{Consent: consent, Now: custodyTestNow}))
	consumeCtx, consume := testEnvelope(t, ctx, dataset, "ConsumeSealedConnectorConsent", time.Now().UTC(), consent.Nonce, custodyTestNow)
	consumed, err := s.ConsumeSealedConnectorConsent(consumeCtx, consent.Nonce, custodyTestNow)
	require.NoError(t, err)
	replayed, err := s.ConsumeSealedConnectorConsent(consumeCtx, consent.Nonce, custodyTestNow)
	require.NoError(t, err)
	require.Equal(t, consumed, replayed, "a lost response is recovered from the receipt")
	result, scope, _ := custodyReceipt(t, s, consume.OperationID)
	require.Equal(t, "connection-a", scope)
	require.True(t, bytes.Contains(result, receiptCopy(consent.VerifierCiphertext)))

	require.NoError(t, s.DeleteConnectorConsentsForConnection(ctx, "connection-a"))

	result, _, cleared := custodyReceipt(t, s, consume.OperationID)
	require.Nil(t, result)
	require.True(t, cleared)
	_, err = s.ConsumeSealedConnectorConsent(consumeCtx, consent.Nonce, custodyTestNow)
	require.ErrorIs(t, err, store.ErrConflict)
	requireNoCopyInFiles(t, s, map[string][]byte{"the receipt's copy of the verifier": receiptCopy(consent.VerifierCiphertext)})
}

// Writes whose results carry no custody keep no custody scope, so removing
// custody leaves their receipts alone.
func TestReceiptsOutsideCustodyHaveNoScope(t *testing.T) {
	ctx := t.Context()
	s := setupTestStore(t)
	dataset := advanceTestEpoch(t, s, testWriteFence)
	saveCtx, save := testEnvelope(t, ctx, dataset, "SaveResult", time.Now().UTC(), "ns", "task", []byte("result"))
	require.NoError(t, s.SaveResult(saveCtx, "ns", "task", []byte("result")))
	var scope string
	var cleared bool
	require.NoError(t, s.db.QueryRow(`SELECT custody_scope, result_cleared FROM operation_receipts WHERE operation_id = ?`,
		save.OperationID).Scan(&scope, &cleared))
	require.Empty(t, scope)
	require.False(t, cleared)
	cleaned, err := s.clearCustodyReceipts(ctx, s.db, "")
	require.NoError(t, err)
	require.False(t, cleaned, "receipts without a custody scope are never cleared")
	err = s.db.QueryRow(`SELECT 1 FROM operation_receipts WHERE operation_id = ? AND result_cleared = 0`, save.OperationID).Scan(new(int))
	require.NotErrorIs(t, err, sql.ErrNoRows)
}

// Clearing a custody scope searches the scope index rather than scanning
// every receipt while it holds the writer.
func TestCustodyClearingSearchesTheScopeIndex(t *testing.T) {
	s := setupTestStore(t)
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+clearCustodyReceiptsSQL, "connection-a")
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		plan = append(plan, detail)
	}
	require.NoError(t, rows.Err())
	require.Len(t, plan, 1)
	require.Contains(t, plan[0], "USING INDEX idx_operation_receipts_custody_scope")
}
