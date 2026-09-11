package v2

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestSealedBootstrapBindsProcessAndActor(t *testing.T) {
	actor := SubstrateActorIdentity{Atespace: "tenant", Name: "runtime", UID: "actor-uid"}
	receiver, err := NewCredentialBootstrapReceiver("template-nonce", actor)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("test-only bootstrap payload")
	body, err := SealCredentialBootstrap(receiver.Challenge, "template-nonce", actor, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, plaintext) {
		t.Fatal("sealed payload contains plaintext")
	}
	var envelope SealedCredentialBootstrap
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	opened, err := receiver.Open(envelope)
	if err != nil || !bytes.Equal(opened, plaintext) {
		t.Fatal("receiver could not authenticate and open its payload")
	}

	// A new process in the same Actor must not receive the previous boot's
	// credentials, even when it still has the same public template nonce.
	replacement, err := NewCredentialBootstrapReceiver("template-nonce", actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replacement.Open(envelope); err == nil {
		t.Fatal("replacement process accepted the old process's payload")
	}

	for _, field := range []string{"uid", "atespace", "name", "nonce"} {
		t.Run(field, func(t *testing.T) {
			expected, nonce := actor, "template-nonce"
			switch field {
			case "uid":
				expected.UID = "replacement-uid"
			case "atespace":
				expected.Atespace = "other-tenant"
			case "name":
				expected.Name = "other-runtime"
			case "nonce":
				nonce = "other-template"
			}
			if _, err := SealCredentialBootstrap(receiver.Challenge, nonce, expected, plaintext); err == nil {
				t.Fatal("controller sealed a payload for a mismatched identity")
			}
		})
	}

	for _, field := range []string{"ciphertext", "sender", "iv", "challenge"} {
		t.Run("tamper-"+field, func(t *testing.T) {
			tampered := envelope
			switch field {
			case "ciphertext":
				data, _ := base64.RawURLEncoding.DecodeString(tampered.Ciphertext)
				data[0] ^= 1
				tampered.Ciphertext = base64.RawURLEncoding.EncodeToString(data)
			case "sender":
				tampered.SenderPublicKey = replacement.Challenge.PublicKey
			case "iv":
				tampered.Nonce = base64.RawURLEncoding.EncodeToString(make([]byte, 12))
			case "challenge":
				tampered.Challenge.Actor.UID = "replacement-uid"
			}
			if _, err := receiver.Open(tampered); err == nil {
				t.Fatal("receiver accepted a modified envelope")
			}
		})
	}
}

func TestSealedBootstrapRejectsInvalidPublicKey(t *testing.T) {
	actor := SubstrateActorIdentity{Atespace: "tenant", Name: "runtime", UID: "actor-uid"}
	receiver, err := NewCredentialBootstrapReceiver("nonce", actor)
	if err != nil {
		t.Fatal(err)
	}
	challenge := receiver.Challenge
	challenge.PublicKey = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if _, err := SealCredentialBootstrap(challenge, "nonce", actor, []byte("test-only payload")); err == nil {
		t.Fatal("accepted an all-zero X25519 public key")
	}
}
