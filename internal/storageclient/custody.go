package storageclient

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// SnapshotKeyBytes is the length of the snapshot key, an AES-256 key.
const SnapshotKeyBytes = 32

// ErrSnapshotKeyNotActive is returned by every operation on sealed records
// until ActivateSnapshotKey has succeeded.
var ErrSnapshotKeyNotActive = errors.New("agent execution snapshot encryption is not configured; snapshot persistence fails closed")

// Page sizes for authenticating retained records. Snapshot bodies are usually
// small but have no fixed size limit, and native session records can
// hold large bundles, so native records are read one at a time.
const (
	snapshotVerificationPageSize      = 8
	nativeSessionVerificationPageSize = 1
)

// keyCustody holds the snapshot key as an AES-256-GCM AEAD. Every sealed
// record format uses a fresh random nonce and binds the record's identity
// through the associated data. The key never leaves this type.
type keyCustody struct {
	aead cipher.AEAD
}

func newKeyCustody(key []byte) (*keyCustody, error) {
	if len(key) != SnapshotKeyBytes {
		return nil, fmt.Errorf("agent execution snapshot key must be exactly %d bytes", SnapshotKeyBytes)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("initialize snapshot cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize snapshot AEAD: %w", err)
	}
	return &keyCustody{aead: aead}, nil
}

var errInvalidNonce = errors.New("invalid encryption nonce")

func (k *keyCustody) seal(additionalData, plaintext []byte) (nonce, ciphertext []byte, err error) {
	nonce = make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("generate nonce: %w", err)
	}
	return nonce, k.aead.Seal(nil, nonce, plaintext, additionalData), nil
}

// open authenticates and decrypts. It rejects a nonce of the wrong length,
// which the AEAD would otherwise panic on.
func (k *keyCustody) open(nonce, ciphertext, additionalData []byte) ([]byte, error) {
	if len(nonce) != k.aead.NonceSize() {
		return nil, errInvalidNonce
	}
	return k.aead.Open(nil, nonce, ciphertext, additionalData)
}

// ActivateSnapshotKey makes key the client's snapshot key. It first
// authenticates every retained sealed snapshot, native session and connector
// custody record through the provider, and installs the key only if all of
// them open. A key that cannot open retained data is rejected, so one dataset
// never accumulates records sealed under different keys and linked accounts
// remain usable and revocable. Startup activates the key before any consumer
// uses the client; until then, sealed records fail closed with
// ErrSnapshotKeyNotActive.
func (c *Client) ActivateSnapshotKey(ctx context.Context, key []byte) error {
	custody, err := newKeyCustody(key)
	if err != nil {
		return err
	}
	if err := c.verifyRetainedSnapshots(ctx, custody); err != nil {
		return err
	}
	if err := c.verifyRetainedNativeSessions(ctx, custody); err != nil {
		return err
	}
	if err := c.verifyRetainedConnectorCustody(ctx, custody); err != nil {
		return err
	}
	c.custody.Store(custody)
	return nil
}

func (c *Client) activeCustody() (*keyCustody, error) {
	custody := c.custody.Load()
	if custody == nil {
		return nil, ErrSnapshotKeyNotActive
	}
	return custody, nil
}

func (c *Client) verifyRetainedSnapshots(ctx context.Context, custody *keyCustody) error {
	cursor := ""
	for {
		page, err := c.ListSealedAgentExecutionSnapshots(ctx, cursor, snapshotVerificationPageSize)
		if err != nil {
			return fmt.Errorf("verify agent execution snapshot key: %w", err)
		}
		for _, sealed := range page.Snapshots {
			body, err := openSnapshotBody(custody, sealed)
			if err != nil {
				return fmt.Errorf(
					"candidate agent execution snapshot key cannot authenticate retained snapshot %s/%s; restore the previous key or wait for reference-aware retention before rotating: %w",
					sealed.Key.TaskUID, sealed.Key.Digest, err,
				)
			}
			if computed := store.CanonicalAgentExecutionSnapshotDigest(body); computed != sealed.Key.Digest {
				return fmt.Errorf("retained agent execution snapshot %s/%s failed integrity verification while activating key", sealed.Key.TaskUID, sealed.Key.Digest)
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		cursor = page.NextCursor
	}
}

func (c *Client) verifyRetainedNativeSessions(ctx context.Context, custody *keyCustody) error {
	cursor := ""
	for {
		page, err := c.ListSealedNativeSessions(ctx, cursor, nativeSessionVerificationPageSize)
		if err != nil {
			return fmt.Errorf("verify native session key: %w", err)
		}
		for _, sealed := range page.Sessions {
			if err := verifyNativeSessionSeal(custody, sealed); err != nil {
				return err
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		cursor = page.NextCursor
	}
}

// verifyNativeSessionSeal authenticates a retained native record and its
// digest, without the record checks a read applies, so key activation
// accepts every record an earlier release could store.
func verifyNativeSessionSeal(custody *keyCustody, sealed storagev1.SealedNativeSession) error {
	if len(sealed.Nonce) != custody.aead.NonceSize() {
		return errors.New("retained native session has invalid nonce")
	}
	body, err := custody.open(sealed.Nonce, sealed.Ciphertext, nativeSessionAAD(sealed.Namespace, sealed.SessionName, sealed.SessionUID, sealed.RecordDigest))
	if err != nil || store.CanonicalBytesDigest(body) != sealed.RecordDigest {
		return errors.New("candidate snapshot key cannot authenticate retained native session; restore the previous key")
	}
	return nil
}
