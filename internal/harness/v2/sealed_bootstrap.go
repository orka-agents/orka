package v2

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

const (
	SubstrateIdentityDirectory = "/run/orka-substrate-identity"
	SealedBootstrapSchema      = "orka.harness.v2/sealed-bootstrap/v1"
)

// SubstrateActorIdentity comes from Substrate's read-only SystemInfo volume,
// regenerated on every run/restore. It is not copied from a snapshot or trusted
// from template environment variables.
type SubstrateActorIdentity struct {
	Atespace string `json:"atespace"`
	Name     string `json:"name"`
	UID      string `json:"uid"`
}

func (i SubstrateActorIdentity) Validate() error {
	if strings.TrimSpace(i.Atespace) == "" || strings.TrimSpace(i.Name) == "" || strings.TrimSpace(i.UID) == "" || len(i.Atespace) > 63 || len(i.Name) > 63 || len(i.UID) > 128 {
		return errors.New("substrate bootstrap requires an exact Actor identity")
	}
	return nil
}

// SealedBootstrapChallenge contains only public material. The key and boot
// nonce are newly generated in each credential-free supervisor process.
type SealedBootstrapChallenge struct {
	Schema    string                 `json:"schema"`
	Nonce     string                 `json:"nonce"`
	BootNonce string                 `json:"bootNonce"`
	PublicKey string                 `json:"publicKey"`
	Actor     SubstrateActorIdentity `json:"actor"`
}

type SealedCredentialBootstrap struct {
	Challenge       SealedBootstrapChallenge `json:"challenge"`
	SenderPublicKey string                   `json:"senderPublicKey"`
	Nonce           string                   `json:"nonce"`
	Ciphertext      string                   `json:"ciphertext"`
}

// CredentialBootstrapReceiver holds a single process's private key. Full
// memory snapshots are prohibited; this key never goes into durable storage.
type CredentialBootstrapReceiver struct {
	Challenge SealedBootstrapChallenge
	key       *ecdh.PrivateKey
}

func NewCredentialBootstrapReceiver(nonce string, actor SubstrateActorIdentity) (*CredentialBootstrapReceiver, error) {
	if err := actor.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(nonce) == "" {
		return nil, errors.New("bootstrap nonce is required")
	}
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	bootNonce := make([]byte, 32)
	if _, err := rand.Read(bootNonce); err != nil {
		return nil, err
	}
	return &CredentialBootstrapReceiver{key: key, Challenge: SealedBootstrapChallenge{
		Schema: SealedBootstrapSchema, Nonce: nonce, BootNonce: base64.RawURLEncoding.EncodeToString(bootNonce),
		PublicKey: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), Actor: actor,
	}}, nil
}

func (c SealedBootstrapChallenge) Validate(nonce string, expected SubstrateActorIdentity) error {
	if err := expected.Validate(); err != nil {
		return err
	}
	if c.Schema != SealedBootstrapSchema || c.Nonce != nonce || c.Actor != expected {
		return errors.New("substrate bootstrap challenge does not match the expected Actor lifetime and nonce")
	}
	key, err := base64.RawURLEncoding.DecodeString(c.PublicKey)
	if err != nil || len(key) != 32 {
		return errors.New("bootstrap receiver key is invalid")
	}
	boot, err := base64.RawURLEncoding.DecodeString(c.BootNonce)
	if err != nil || len(boot) != 32 {
		return errors.New("bootstrap receiver boot nonce is invalid")
	}
	return nil
}

// SealCredentialBootstrap encrypts the payload for the exact observed process
// and Actor lifetime. The caller additionally signs the resulting envelope
// with the controller-only bootstrap signing seed. A rerouted request or
// replaced process cannot decrypt a prior process's payload.
func SealCredentialBootstrap(challenge SealedBootstrapChallenge, nonce string, actor SubstrateActorIdentity, plaintext []byte) ([]byte, error) {
	body, _, err := SealCredentialBootstrapExchange(challenge, nonce, actor, plaintext)
	return body, err
}

// SealCredentialBootstrapExchange also retains the ephemeral exchange key so
// the caller can authenticate and decrypt a response from this exact process.
func SealCredentialBootstrapExchange(challenge SealedBootstrapChallenge, nonce string, actor SubstrateActorIdentity, plaintext []byte) ([]byte, *CredentialBootstrapExchange, error) {
	if err := challenge.Validate(nonce, actor); err != nil {
		return nil, nil, err
	}
	receiverBytes, _ := base64.RawURLEncoding.DecodeString(challenge.PublicKey)
	receiverKey, err := ecdh.X25519().NewPublicKey(receiverBytes)
	if err != nil {
		return nil, nil, err
	}
	senderKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	shared, err := senderKey.ECDH(receiverKey)
	if err != nil {
		return nil, nil, err
	}
	aead, aad, err := bootstrapAEAD(shared, challenge)
	if err != nil {
		return nil, nil, err
	}
	iv := make([]byte, aead.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return nil, nil, err
	}
	envelope := SealedCredentialBootstrap{
		Challenge: challenge, SenderPublicKey: base64.RawURLEncoding.EncodeToString(senderKey.PublicKey().Bytes()),
		Nonce: base64.RawURLEncoding.EncodeToString(iv), Ciphertext: base64.RawURLEncoding.EncodeToString(aead.Seal(nil, iv, plaintext, aad)),
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, nil, err
	}
	return body, &CredentialBootstrapExchange{shared: shared, requestDigest: sha256.Sum256(body)}, nil
}

func (r *CredentialBootstrapReceiver) Open(envelope SealedCredentialBootstrap) ([]byte, error) {
	shared, err := r.sharedKey(envelope)
	if err != nil {
		return nil, err
	}
	aead, aad, err := bootstrapAEAD(shared, r.Challenge)
	if err != nil {
		return nil, err
	}
	iv, err := base64.RawURLEncoding.DecodeString(envelope.Nonce)
	if err != nil || len(iv) != aead.NonceSize() {
		return nil, errors.New("bootstrap encryption nonce is invalid")
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(envelope.Ciphertext)
	if err != nil {
		return nil, errors.New("bootstrap ciphertext is invalid")
	}
	plaintext, err := aead.Open(nil, iv, ciphertext, aad)
	if err != nil {
		return nil, errors.New("bootstrap ciphertext authentication failed")
	}
	return plaintext, nil
}

func (r *CredentialBootstrapReceiver) sharedKey(envelope SealedCredentialBootstrap) ([]byte, error) {
	if r == nil || r.key == nil || envelope.Challenge != r.Challenge {
		return nil, errors.New("sealed bootstrap targets another process or Actor lifetime")
	}
	senderBytes, err := base64.RawURLEncoding.DecodeString(envelope.SenderPublicKey)
	if err != nil {
		return nil, errors.New("bootstrap sender key is invalid")
	}
	senderKey, err := ecdh.X25519().NewPublicKey(senderBytes)
	if err != nil {
		return nil, errors.New("bootstrap sender key is invalid")
	}
	shared, err := r.key.ECDH(senderKey)
	if err != nil {
		return nil, errors.New("bootstrap key exchange failed")
	}
	return shared, nil
}

func bootstrapAEAD(shared []byte, challenge SealedBootstrapChallenge) (cipher.AEAD, []byte, error) {
	aad, err := json.Marshal(challenge)
	if err != nil {
		return nil, nil, err
	}
	salt := sha256.Sum256(aad)
	key, err := hkdf.Key(sha256.New, shared, salt[:], SealedBootstrapSchema, 32)
	if err != nil {
		return nil, nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	aead, err := cipher.NewGCM(block)
	return aead, aad, err
}
