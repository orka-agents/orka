package v2

import (
	"strings"
	"testing"
	"time"
)

func TestOperationCapabilityRoundTripAndBinding(t *testing.T) {
	metadata := testMutationMetadata(t, true)
	metadata.RequestDigest = RequestDigest(testSHA256("capability-request"))
	metadata.ExpiresAt = time.Now().UTC().Add(time.Minute)
	secret := []byte(strings.Repeat("s", MinCapabilitySecretBytes))
	token, err := SignOperationCapability(secret, ClaimsForMutation(metadata))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyOperationCapability(secret, token, metadata, true, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	changed := metadata
	changed.OperationID = "different-operation"
	if err := VerifyOperationCapability(secret, token, changed, true, time.Now().UTC()); err == nil {
		t.Fatal("capability unexpectedly verified for different operation")
	}
	parts := strings.Split(token, ".")
	parts[1] = strings.Repeat("A", len(parts[1]))
	if err := VerifyOperationCapability(secret, strings.Join(parts, "."), metadata, true, time.Now().UTC()); err == nil {
		t.Fatal("tampered capability unexpectedly verified")
	}
}

func TestOperationCapabilityRejectsExpiryAndWeakSecret(t *testing.T) {
	metadata := testMutationMetadata(t, true)
	metadata.RequestDigest = RequestDigest(testSHA256("capability-request"))
	metadata.ExpiresAt = time.Now().UTC().Add(time.Minute)
	if _, err := SignOperationCapability([]byte("short"), ClaimsForMutation(metadata)); err == nil {
		t.Fatal("weak capability secret unexpectedly accepted")
	}
	secret := []byte(strings.Repeat("s", MinCapabilitySecretBytes))
	claims := ClaimsForMutation(metadata)
	token, err := SignOperationCapability(secret, claims)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyOperationCapability(secret, token, metadata, true, metadata.ExpiresAt.Add(time.Second)); err == nil {
		t.Fatal("expired capability unexpectedly verified")
	}
}
