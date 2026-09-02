/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package supervisor

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// Credential bootstrap lets a provider-hosted supervisor boot with no
// credentials at all: execution-workspace providers materialize the workload
// from an operator-visible template (and may build a per-template golden
// snapshot by booting and checkpointing an instance), so nothing secret may
// exist in the process until the controller seeds it. The awaiting-bootstrap
// phase serves only a minimal health probe and a one-time credential PUT. The
// provider-visible template carries a public per-instance nonce and Ed25519
// public key; the controller signs the nonce plus exact request body with a key
// derived from a separate controller-only bootstrap seed. A captured golden
// snapshot therefore contains only a waiting supervisor and non-secret
// verification material.

const (
	// EnvCredentialBootstrapNonce enables the awaiting-bootstrap phase. The
	// value is public per-instance entropy bound into the controller signature.
	EnvCredentialBootstrapNonce = "ORKA_ACP_CREDENTIAL_BOOTSTRAP_NONCE"
	// EnvCredentialBootstrapPublicKey carries the non-secret per-instance Ed25519
	// key that authenticates the controller's one-time bootstrap request.
	EnvCredentialBootstrapPublicKey = harnessv2.CredentialBootstrapPublicKeyEnv

	credentialBootstrapMaxBodyBytes = 64 << 10
)

// CredentialBootstrapRequest is the one-time seeding payload.
type CredentialBootstrapRequest = harnessv2.CredentialBootstrapRequest

// CredentialBootstrapConfigured reports whether the supervisor must wait for
// controller-seeded credentials before loading its configuration: the nonce is
// set and no credential file or read-once bootstrap variable is present.
func CredentialBootstrapConfigured() bool {
	if strings.TrimSpace(os.Getenv(EnvCredentialBootstrapNonce)) == "" {
		return false
	}
	for _, name := range []string{
		EnvControllerTokenFile, EnvControllerTokenBootstrap,
		EnvCapabilitySecretFile, EnvCapabilitySecretBootstrap,
		EnvProviderTokenFile, EnvProviderTokenBootstrap,
	} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return false
		}
	}
	return true
}

type credentialBootstrapState struct {
	mu        sync.Mutex
	nonce     string
	publicKey string
	seeded    bool
	request   CredentialBootstrapRequest
	received  chan struct{}
}

// handle implements the one-time, idempotent seeding endpoint: the first
// controller-signed payload wins; an identical repeat is acknowledged so a
// controller retry after an ambiguous response converges; a different payload
// conflicts so the controller can recycle the exact instance.
func (s *credentialBootstrapState) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	nonce := strings.TrimSpace(r.Header.Get(harnessv2.CredentialBootstrapNonceHeader))
	if nonce == "" || subtle.ConstantTimeCompare([]byte(nonce), []byte(s.nonce)) != 1 {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, credentialBootstrapMaxBodyBytes))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err := harnessv2.VerifyCredentialBootstrap(
		s.publicKey,
		nonce,
		body,
		r.Header.Get(harnessv2.CredentialBootstrapSignatureHeader),
	); err != nil {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	var request CredentialBootstrapRequest
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err := request.Validate(); err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seeded {
		if subtle.ConstantTimeCompare([]byte(s.request.ControllerToken), []byte(request.ControllerToken)) == 1 &&
			subtle.ConstantTimeCompare([]byte(s.request.CapabilitySecret), []byte(request.CapabilitySecret)) == 1 &&
			subtle.ConstantTimeCompare([]byte(s.request.ProviderToken), []byte(request.ProviderToken)) == 1 {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusConflict)
		return
	}
	s.seeded = true
	s.request = request
	close(s.received)
	w.WriteHeader(http.StatusCreated)
}

func (s *credentialBootstrapState) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"protocol":"orka.harness.v2","lifecycle":"booting"}`))
	})
	mux.HandleFunc(harnessv2.CredentialBootstrapPath, s.handle)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	return mux
}

// AwaitCredentialBootstrap serves the awaiting-bootstrap phase on the
// supervisor listen address until the controller seeds credentials, then
// shuts the phase server down and returns the seeded values. The caller
// exports them as the read-once bootstrap variables and proceeds with the
// normal configuration load.
func AwaitCredentialBootstrap(ctx context.Context) (CredentialBootstrapRequest, error) {
	nonce := strings.TrimSpace(os.Getenv(EnvCredentialBootstrapNonce))
	if nonce == "" {
		return CredentialBootstrapRequest{}, errors.New("credential bootstrap nonce is not configured")
	}
	publicKey := strings.TrimSpace(os.Getenv(EnvCredentialBootstrapPublicKey))
	if publicKey == "" {
		return CredentialBootstrapRequest{}, errors.New("credential bootstrap public key is not configured")
	}
	state := &credentialBootstrapState{nonce: nonce, publicKey: publicKey, received: make(chan struct{})}
	listenAddress := strings.TrimSpace(os.Getenv(EnvListenAddress))
	if listenAddress == "" {
		listenAddress = ":8080"
	}
	server := &http.Server{
		Addr:              listenAddress,
		Handler:           state.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    32 << 10,
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.ListenAndServe() }()

	select {
	case <-state.received:
	case err := <-serveResult:
		return CredentialBootstrapRequest{}, fmt.Errorf("credential bootstrap server exited before seeding: %w", err)
	case <-ctx.Done():
		_ = server.Close()
		return CredentialBootstrapRequest{}, fmt.Errorf("credential bootstrap was not seeded: %w", ctx.Err())
	}
	// Release the listener so the fully configured supervisor can bind the
	// same address; in-flight bootstrap responses have already been written.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
	}
	<-serveResult
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.request, nil
}
