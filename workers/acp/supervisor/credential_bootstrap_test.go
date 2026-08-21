/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func validBootstrapRequest() CredentialBootstrapRequest {
	return CredentialBootstrapRequest{
		ControllerToken:  strings.Repeat("c", 48),
		CapabilitySecret: strings.Repeat("s", harnessv2.MinCapabilitySecretBytes),
		ProviderToken:    "provider-token",
	}
}

func putBootstrap(t *testing.T, server *httptest.Server, nonce string, body []byte) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPut, server.URL+harnessv2.CredentialBootstrapPath, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build bootstrap request: %v", err)
	}
	if nonce != "" {
		request.Header.Set(harnessv2.CredentialBootstrapNonceHeader, nonce)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("send bootstrap request: %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func TestCredentialBootstrapConfigured(t *testing.T) {
	credentialEnvNames := []string{
		EnvControllerTokenFile, EnvControllerTokenBootstrap,
		EnvCapabilitySecretFile, EnvCapabilitySecretBootstrap,
		EnvProviderTokenFile, EnvProviderTokenBootstrap,
	}
	clearAll := func(t *testing.T) {
		t.Helper()
		t.Setenv(EnvCredentialBootstrapNonce, "")
		for _, name := range credentialEnvNames {
			t.Setenv(name, "")
		}
	}

	t.Run("nonce absent", func(t *testing.T) {
		clearAll(t)
		if CredentialBootstrapConfigured() {
			t.Fatal("configured without a nonce")
		}
	})
	t.Run("nonce alone enables", func(t *testing.T) {
		clearAll(t)
		t.Setenv(EnvCredentialBootstrapNonce, "pool-nonce")
		if !CredentialBootstrapConfigured() {
			t.Fatal("not configured with nonce and no credential sources")
		}
	})
	for _, name := range credentialEnvNames {
		t.Run("credential source wins over nonce: "+name, func(t *testing.T) {
			clearAll(t)
			t.Setenv(EnvCredentialBootstrapNonce, "pool-nonce")
			t.Setenv(name, "already-provided")
			if CredentialBootstrapConfigured() {
				t.Fatalf("bootstrap configured while %s is set; direct credential sources must win", name)
			}
		})
	}
}

func TestCredentialBootstrapHandlerSemantics(t *testing.T) {
	state := &credentialBootstrapState{nonce: "pool-nonce", received: make(chan struct{})}
	server := httptest.NewServer(state.handler())
	defer server.Close()

	seed := validBootstrapRequest()
	seedBody, err := json.Marshal(seed)
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}

	t.Run("health serves booting lifecycle without a nonce", func(t *testing.T) {
		response, err := server.Client().Get(server.URL + "/v2/health")
		if err != nil {
			t.Fatalf("health request: %v", err)
		}
		defer func() { _ = response.Body.Close() }()
		var health struct {
			Protocol  string `json:"protocol"`
			Lifecycle string `json:"lifecycle"`
		}
		if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
			t.Fatalf("decode health: %v", err)
		}
		if response.StatusCode != http.StatusOK || health.Protocol != "orka.harness.v2" || health.Lifecycle != "booting" {
			t.Fatalf("health = %d %+v, want 200 booting", response.StatusCode, health)
		}
	})
	t.Run("non-PUT method rejected", func(t *testing.T) {
		response, err := server.Client().Post(server.URL+harnessv2.CredentialBootstrapPath, "application/json", bytes.NewReader(seedBody))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("POST status = %d, want 405", response.StatusCode)
		}
	})
	t.Run("missing nonce forbidden", func(t *testing.T) {
		if status := putBootstrap(t, server, "", seedBody).StatusCode; status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", status)
		}
	})
	t.Run("wrong nonce forbidden", func(t *testing.T) {
		if status := putBootstrap(t, server, "other-nonce", seedBody).StatusCode; status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", status)
		}
	})
	t.Run("malformed body rejected", func(t *testing.T) {
		if status := putBootstrap(t, server, "pool-nonce", []byte("{not json")).StatusCode; status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", status)
		}
	})
	t.Run("unknown fields rejected", func(t *testing.T) {
		if status := putBootstrap(t, server, "pool-nonce", []byte(`{"controllerToken":"x","extra":true}`)).StatusCode; status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", status)
		}
	})
	t.Run("invalid credential shapes rejected", func(t *testing.T) {
		short, _ := json.Marshal(CredentialBootstrapRequest{ControllerToken: "short", CapabilitySecret: "short", ProviderToken: ""})
		if status := putBootstrap(t, server, "pool-nonce", short).StatusCode; status != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", status)
		}
	})
	t.Run("first authorized write wins", func(t *testing.T) {
		if status := putBootstrap(t, server, "pool-nonce", seedBody).StatusCode; status != http.StatusCreated {
			t.Fatalf("status = %d, want 201", status)
		}
		select {
		case <-state.received:
		default:
			t.Fatal("received channel not closed after first write")
		}
	})
	t.Run("identical repeat acknowledged", func(t *testing.T) {
		if status := putBootstrap(t, server, "pool-nonce", seedBody).StatusCode; status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
	})
	t.Run("different payload conflicts", func(t *testing.T) {
		other := seed
		other.ProviderToken = "rotated-provider-token"
		otherBody, _ := json.Marshal(other)
		if status := putBootstrap(t, server, "pool-nonce", otherBody).StatusCode; status != http.StatusConflict {
			t.Fatalf("status = %d, want 409", status)
		}
	})
	t.Run("seeded request preserved", func(t *testing.T) {
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.request != seed {
			t.Fatalf("stored request = %+v, want the first write", state.request)
		}
	})
}

func TestAwaitCredentialBootstrapSeedsAndReleasesListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	address := listener.Addr().String()
	_ = listener.Close()

	t.Setenv(EnvCredentialBootstrapNonce, "pool-nonce")
	t.Setenv(EnvListenAddress, address)

	type awaited struct {
		request CredentialBootstrapRequest
		err     error
	}
	result := make(chan awaited, 1)
	go func() {
		request, err := AwaitCredentialBootstrap(context.Background())
		result <- awaited{request: request, err: err}
	}()

	seed := validBootstrapRequest()
	seedBody, _ := json.Marshal(seed)
	deadline := time.Now().Add(10 * time.Second)
	for {
		request, err := http.NewRequest(http.MethodPut, fmt.Sprintf("http://%s%s", address, harnessv2.CredentialBootstrapPath), bytes.NewReader(seedBody))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		request.Header.Set(harnessv2.CredentialBootstrapNonceHeader, "pool-nonce")
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			status := response.StatusCode
			_ = response.Body.Close()
			if status != http.StatusCreated {
				t.Fatalf("seed status = %d, want 201", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("bootstrap server never became reachable: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("AwaitCredentialBootstrap: %v", got.err)
		}
		if got.request != seed {
			t.Fatalf("seeded request = %+v, want %+v", got.request, seed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("AwaitCredentialBootstrap did not return after seeding")
	}

	// The listener must be released so the configured supervisor can bind it.
	deadline = time.Now().Add(5 * time.Second)
	for {
		reclaimed, err := net.Listen("tcp", address)
		if err == nil {
			_ = reclaimed.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("listen address was not released after bootstrap: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAwaitCredentialBootstrapStopsWhenContextIsCancelled(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	address := listener.Addr().String()
	_ = listener.Close()

	t.Setenv(EnvCredentialBootstrapNonce, "pool-nonce")
	t.Setenv(EnvListenAddress, address)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := AwaitCredentialBootstrap(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("AwaitCredentialBootstrap error = %v, want context cancellation", err)
	}
}
