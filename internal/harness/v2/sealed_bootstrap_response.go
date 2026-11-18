package v2

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
)

const sealedBootstrapResponseSchema = "orka.harness.v2/sealed-bootstrap-response/v1"

// CredentialBootstrapExchange is private to one request. The response key is
// distinct from the request key and binds the exact signed request bytes.
type CredentialBootstrapExchange struct {
	shared        []byte
	requestDigest [sha256.Size]byte
}

type sealedBootstrapResponse struct {
	Schema     string `json:"schema"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// SealResponse encrypts for the request's ephemeral sender. The caller must
// verify the bootstrap signature before requesting a response.
func (r *CredentialBootstrapReceiver) SealResponse(request, plaintext []byte) ([]byte, error) {
	var envelope SealedCredentialBootstrap
	if json.Unmarshal(request, &envelope) != nil {
		return nil, errors.New("bootstrap request is invalid")
	}
	if _, err := r.Open(envelope); err != nil {
		return nil, err
	}
	shared, err := r.sharedKey(envelope)
	if err != nil {
		return nil, err
	}
	exchange := &CredentialBootstrapExchange{shared: shared, requestDigest: sha256.Sum256(request)}
	aead, aad, err := exchange.responseAEAD()
	if err != nil {
		return nil, err
	}
	iv := make([]byte, aead.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	return json.Marshal(sealedBootstrapResponse{
		Schema: sealedBootstrapResponseSchema, Nonce: base64.RawURLEncoding.EncodeToString(iv),
		Ciphertext: base64.RawURLEncoding.EncodeToString(aead.Seal(nil, iv, plaintext, aad)),
	})
}

// OpenResponse accepts only a reply from the challenged process to the exact
// request that created this exchange. Replies cannot be reflected as requests.
func (e *CredentialBootstrapExchange) OpenResponse(body []byte) ([]byte, error) {
	var response sealedBootstrapResponse
	if json.Unmarshal(body, &response) != nil || response.Schema != sealedBootstrapResponseSchema {
		return nil, errors.New("bootstrap response is invalid")
	}
	aead, aad, err := e.responseAEAD()
	if err != nil {
		return nil, err
	}
	iv, err := base64.RawURLEncoding.DecodeString(response.Nonce)
	if err != nil || len(iv) != aead.NonceSize() {
		return nil, errors.New("bootstrap response nonce is invalid")
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(response.Ciphertext)
	if err != nil {
		return nil, errors.New("bootstrap response ciphertext is invalid")
	}
	plaintext, err := aead.Open(nil, iv, ciphertext, aad)
	if err != nil {
		return nil, errors.New("bootstrap response authentication failed")
	}
	return plaintext, nil
}

func (e *CredentialBootstrapExchange) responseAEAD() (cipher.AEAD, []byte, error) {
	if e == nil || len(e.shared) != 32 {
		return nil, nil, errors.New("bootstrap exchange is unavailable")
	}
	key, err := hkdf.Key(sha256.New, e.shared, e.requestDigest[:], sealedBootstrapResponseSchema, 32)
	if err != nil {
		return nil, nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	aead, err := cipher.NewGCM(block)
	aad := append([]byte(sealedBootstrapResponseSchema+"\x00"), e.requestDigest[:]...)
	return aead, aad, err
}
