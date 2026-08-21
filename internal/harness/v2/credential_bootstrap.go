package v2

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// Credential bootstrap is the one-time seeding step for provider-hosted
// supervisors that boot credential-free: execution-workspace providers
// materialize the workload from a provider-visible template (and may build a
// golden snapshot by booting and checkpointing an instance), so no credential
// may exist in the process or its template until the controller seeds it. A
// public per-pool nonce binds the target workload, while an Ed25519 signature
// derived from the controller-only pool capability secret authenticates the
// exact payload against a non-secret public key embedded in the template.

const (
	// CredentialBootstrapPath is the one-time, idempotent seeding endpoint.
	CredentialBootstrapPath = "/v2/credential-bootstrap"
	// CredentialBootstrapNonceHeader carries the pool bootstrap nonce.
	CredentialBootstrapNonceHeader = "X-Orka-Credential-Bootstrap-Nonce"
	// CredentialBootstrapSignatureHeader carries the controller's Ed25519
	// signature over the nonce and exact request body.
	CredentialBootstrapSignatureHeader = "X-Orka-Credential-Bootstrap-Signature"
	// CredentialBootstrapPublicKeyEnv carries the non-secret per-pool Ed25519
	// public key used by a credential-free provider-hosted supervisor.
	CredentialBootstrapPublicKeyEnv = "ORKA_ACP_CREDENTIAL_BOOTSTRAP_PUBLIC_KEY"

	credentialBootstrapSigningSeedDomain = "orka.harness.v2/credential-bootstrap-signing-seed/v1"
	credentialBootstrapSigningDomain     = "orka.harness.v2/credential-bootstrap-signature/v1"
)

// CredentialBootstrapRequest is the seeding payload. First write wins; an
// identical repeat is acknowledged; a different payload conflicts.
type CredentialBootstrapRequest struct {
	ControllerToken  string `json:"controllerToken"`
	CapabilitySecret string `json:"capabilitySecret"`
	ProviderToken    string `json:"providerToken"`
}

// Validate enforces the same minimum credential shapes as supervisor boot.
func (r CredentialBootstrapRequest) Validate() error {
	if len(strings.TrimSpace(r.ControllerToken)) < 32 {
		return errors.New("controllerToken must be at least 32 bytes")
	}
	if len(strings.TrimSpace(r.CapabilitySecret)) < MinCapabilitySecretBytes {
		return errors.New("capabilitySecret is too short")
	}
	if strings.TrimSpace(r.ProviderToken) == "" {
		return errors.New("providerToken is required")
	}
	return nil
}

// CredentialBootstrapPublicKey derives and encodes the non-secret Ed25519
// verification key for one pool capability secret.
func CredentialBootstrapPublicKey(capabilitySecret []byte) (string, error) {
	privateKey, err := credentialBootstrapSigningKey(capabilitySecret)
	if err != nil {
		return "", err
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return base64.RawURLEncoding.EncodeToString(publicKey), nil
}

// SignCredentialBootstrap signs the public nonce and exact request body with
// the controller-only key derived from the pool capability secret.
func SignCredentialBootstrap(capabilitySecret []byte, nonce string, body []byte) (string, error) {
	privateKey, err := credentialBootstrapSigningKey(capabilitySecret)
	if err != nil {
		return "", err
	}
	message, err := credentialBootstrapSigningMessage(nonce, body)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, message)), nil
}

// VerifyCredentialBootstrap verifies a controller signature from the public
// key embedded in the provider-visible template.
func VerifyCredentialBootstrap(publicKeyEncoded, nonce string, body []byte, signatureEncoded string) error {
	publicKey, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(publicKeyEncoded))
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return errors.New("credential bootstrap public key is invalid")
	}
	signature, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(signatureEncoded))
	if err != nil || len(signature) != ed25519.SignatureSize {
		return errors.New("credential bootstrap signature is invalid")
	}
	message, err := credentialBootstrapSigningMessage(nonce, body)
	if err != nil {
		return err
	}
	if !ed25519.Verify(ed25519.PublicKey(publicKey), message, signature) {
		return errors.New("credential bootstrap signature verification failed")
	}
	return nil
}

func credentialBootstrapSigningKey(capabilitySecret []byte) (ed25519.PrivateKey, error) {
	if len(capabilitySecret) < MinCapabilitySecretBytes {
		return nil, errors.New("credential bootstrap capability secret is too short")
	}
	mac := hmac.New(sha256.New, capabilitySecret)
	_, _ = mac.Write([]byte(credentialBootstrapSigningSeedDomain))
	return ed25519.NewKeyFromSeed(mac.Sum(nil)), nil
}

func credentialBootstrapSigningMessage(nonce string, body []byte) ([]byte, error) {
	nonce = strings.TrimSpace(nonce)
	if nonce == "" {
		return nil, errors.New("credential bootstrap nonce is required")
	}
	if len(body) == 0 {
		return nil, errors.New("credential bootstrap request body is required")
	}
	var message bytes.Buffer
	message.WriteString(credentialBootstrapSigningDomain)
	if err := binary.Write(&message, binary.BigEndian, uint32(len(nonce))); err != nil {
		return nil, fmt.Errorf("encode credential bootstrap nonce length: %w", err)
	}
	message.WriteString(nonce)
	if err := binary.Write(&message, binary.BigEndian, uint64(len(body))); err != nil {
		return nil, fmt.Errorf("encode credential bootstrap body length: %w", err)
	}
	message.Write(body)
	return message.Bytes(), nil
}
