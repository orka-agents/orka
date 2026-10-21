package storageclient

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
)

// TestConnectorSealerRefusesOtherFormats checks that the sealer handed to the
// embedded connector families can neither open nor forge execution snapshots
// or native session records sealed under the same key.
func TestConnectorSealerRefusesOtherFormats(t *testing.T) {
	custody, err := newKeyCustody(bytes.Repeat([]byte{0x31}, SnapshotKeyBytes))
	require.NoError(t, err)
	sealer := connectorSealer{custody: custody}

	body := []byte(`{"prompt":"snapshot"}`)
	snapshotAAD := snapshotAdditionalData("task-uid", store.CanonicalBytesDigest(body), 1)
	nonce, ciphertext, err := custody.seal(snapshotAAD, body)
	require.NoError(t, err)
	_, err = sealer.OpenConnectorCustody(nonce, ciphertext, snapshotAAD)
	require.ErrorIs(t, err, errNotConnectorCustody)

	nativeAAD := nativeSessionAAD("tenant", "session", "uid", store.CanonicalBytesDigest([]byte("record")))
	_, _, err = sealer.SealConnectorCustody(nativeAAD, []byte("forged record"))
	require.ErrorIs(t, err, errNotConnectorCustody)
	_, _, err = sealer.SealConnectorCustody(snapshotAAD, []byte("forged snapshot"))
	require.ErrorIs(t, err, errNotConnectorCustody)

	connectorAAD := []byte("orka.connector-data-key\x00connection-uid")
	nonce, ciphertext, err = sealer.SealConnectorCustody(connectorAAD, []byte("data key"))
	require.NoError(t, err)
	opened, err := sealer.OpenConnectorCustody(nonce, ciphertext, connectorAAD)
	require.NoError(t, err)
	require.Equal(t, []byte("data key"), opened)
	_, err = sealer.OpenConnectorCustody(nonce[:4], ciphertext, connectorAAD)
	require.ErrorIs(t, err, errInvalidNonce)
}
