package supervisor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestCredentialBootstrapSealedHTTP(t *testing.T) {
	actor := harnessv2.SubstrateActorIdentity{Atespace: "tenant", Name: "runtime", UID: "actor-uid"}
	receiver, err := harnessv2.NewCredentialBootstrapReceiver("nonce", actor)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := harnessv2.CredentialBootstrapPublicKey(validBootstrapSigningSecret())
	if err != nil {
		t.Fatal(err)
	}
	state := &credentialBootstrapState{receiver: receiver, nonce: "nonce", publicKey: publicKey, received: make(chan struct{})}
	server := httptest.NewServer(state.handler())
	defer server.Close()
	response, err := server.Client().Get(server.URL + harnessv2.CredentialBootstrapPath)
	if err != nil {
		t.Fatal(err)
	}
	var challenge harnessv2.SealedBootstrapChallenge
	err = json.NewDecoder(response.Body).Decode(&challenge)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("bootstrap did not expose a non-cacheable public challenge")
	}
	plaintext, err := json.Marshal(validBootstrapRequest())
	if err != nil {
		t.Fatal(err)
	}
	if status := putBootstrap(t, server, "nonce", plaintext).StatusCode; status != http.StatusBadRequest {
		t.Fatalf("signed plaintext bypassed sealed bootstrap: HTTP %d", status)
	}
	for i, want := range []int{http.StatusCreated, http.StatusOK} {
		body, err := harnessv2.SealCredentialBootstrap(challenge, "nonce", actor, plaintext)
		if err != nil {
			t.Fatal(err)
		}
		if status := putBootstrap(t, server, "nonce", body).StatusCode; status != want {
			t.Fatalf("bootstrap attempt %d: HTTP %d, want %d", i, status, want)
		}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.seeded || state.request != validBootstrapRequest() {
		t.Fatal("bootstrap did not preserve the accepted payload")
	}
}

func TestCredentialBootstrapIdentityProjection(t *testing.T) {
	root := t.TempDir()
	if identity, err := readSubstrateBootstrapIdentity(filepath.Join(root, "absent")); identity != nil || err != nil {
		t.Fatal("ordinary runtime requires a Substrate projection")
	}
	if _, err := readSubstrateBootstrapIdentity(root); err == nil {
		t.Fatal("accepted an incomplete native identity")
	}
	for name, value := range map[string]string{"atespace": "tenant", "name": "runtime", "uid": "actor-uid"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	identity, err := readSubstrateBootstrapIdentity(root)
	if err != nil || identity == nil || identity.UID != "actor-uid" {
		t.Fatal("native identity projection was not read")
	}
	if err := os.Remove(filepath.Join(root, "uid")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "name"), filepath.Join(root, "uid")); err != nil {
		t.Fatal(err)
	}
	if _, err := readSubstrateBootstrapIdentity(root); err == nil {
		t.Fatal("accepted an identity field redirected through a symlink")
	}
}
