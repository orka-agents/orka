package v2

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCredentialBootstrapSealedResponseBindsRequestAndProcess(t *testing.T) {
	actor := SubstrateActorIdentity{Atespace: "test", Name: "direct", UID: "actor-uid"}
	receiver, err := NewCredentialBootstrapReceiver("nonce", actor)
	require.NoError(t, err)
	request, exchange, err := SealCredentialBootstrapExchange(receiver.Challenge, "nonce", actor, []byte(`{"recover":true}`))
	require.NoError(t, err)
	plaintext := []byte(`{"handoffToken":"fixture-installed-credential"}`)
	response, err := receiver.SealResponse(request, plaintext)
	require.NoError(t, err)
	require.NotContains(t, string(response), "fixture-installed-credential")
	decoded, err := exchange.OpenResponse(response)
	require.NoError(t, err)
	require.Equal(t, plaintext, decoded)

	_, anotherExchange, err := SealCredentialBootstrapExchange(receiver.Challenge, "nonce", actor, []byte(`{"recover":true}`))
	require.NoError(t, err)
	_, err = anotherExchange.OpenResponse(response)
	require.Error(t, err, "a response from another request must not confirm recovery")
	_, err = exchange.OpenResponse(request)
	require.Error(t, err, "a request must not be reflected as an authenticated response")

	var modified sealedBootstrapResponse
	require.NoError(t, json.Unmarshal(response, &modified))
	modified.Ciphertext = modified.Ciphertext[:len(modified.Ciphertext)-4] + "AAAA"
	tampered, err := json.Marshal(modified)
	require.NoError(t, err)
	_, err = exchange.OpenResponse(tampered)
	require.Error(t, err, "ciphertext modification must fail authentication")

	replacement, err := NewCredentialBootstrapReceiver("nonce", actor)
	require.NoError(t, err)
	_, err = replacement.SealResponse(request, plaintext)
	require.Error(t, err, "another process cannot answer the old process's recovery request")
	_, err = receiver.SealResponse([]byte(`{}`), plaintext)
	require.Error(t, err)
}
