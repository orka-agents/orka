package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func (s *workspaceAgentServer) configureSubstrateBootstrap() {
	key := strings.TrimSpace(os.Getenv(harnessv2.WorkspaceBootstrapPublicKeyEnv))
	if key == "" {
		return
	}
	if s.bootstrapAuth != "" || s.controlAuthConfigured {
		s.startupErr = errors.New("native workspace bootstrap must start without private bootstrap or control credentials")
		return
	}
	identity, err := harnessv2.ReadSubstrateActorIdentity(harnessv2.SubstrateIdentityDirectory)
	if err != nil || identity == nil {
		s.startupErr = errors.New("native workspace bootstrap requires its provider identity projection")
		return
	}
	s.bootstrapPublicKey = key
	s.bootstrapReceiver, err = harnessv2.NewCredentialBootstrapReceiver(harnessv2.WorkspaceBootstrapNonce(key), *identity)
	if err != nil {
		s.startupErr = err
	}
}

func (s *workspaceAgentServer) handleSubstrateBootstrap(w http.ResponseWriter, r *http.Request) {
	if s.bootstrapReceiver == nil || s.startupErr != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.bootstrapReceiver.Challenge)
		return
	}
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	nonce := s.bootstrapReceiver.Challenge.Nonce
	if r.Header.Get(harnessv2.CredentialBootstrapNonceHeader) != nonce ||
		harnessv2.VerifyCredentialBootstrap(s.bootstrapPublicKey, nonce, body,
			r.Header.Get(harnessv2.CredentialBootstrapSignatureHeader)) != nil {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	var envelope harnessv2.SealedCredentialBootstrap
	if json.Unmarshal(body, &envelope) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	sealedBody := body
	body, err = s.bootstrapReceiver.Open(envelope)
	if err != nil {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	var request harnessv2.WorkspaceBootstrapRequest
	if json.Unmarshal(body, &request) != nil || len(request.HandoffToken) > 32768 ||
		request.Recover != (r.Method == http.MethodPost) ||
		(request.Recover && request.HandoffToken != "") ||
		(!request.Recover && strings.TrimSpace(request.HandoffToken) == "") {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cleanupInProgress || s.activeAttachment != nil {
		w.WriteHeader(http.StatusConflict)
		return
	}
	if request.Recover {
		s.recoverSubstrateBootstrap(w, sealedBody)
		return
	}
	current, err := handoffToken()
	if err == nil && subtle.ConstantTimeCompare([]byte(current), []byte(request.HandoffToken)) != 1 {
		w.WriteHeader(http.StatusConflict)
		return
	}
	if err != nil && !handoffBootstrapAllowedForTokenError(err) {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if _, err := secureWriteFile(
		handoffTokenFilePath(), []byte(request.HandoffToken), 0o600, false, 0, 0, time.Time{},
	); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// The caller holds s.mu and has verified the request signature and sealed
// process challenge. Reading the existing credential never rotates it or
// replays any workspace operation.
func (s *workspaceAgentServer) recoverSubstrateBootstrap(w http.ResponseWriter, request []byte) {
	token, err := handoffToken()
	if err != nil {
		if !handoffBootstrapAllowedForTokenError(err) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		token = ""
	}
	plaintext, err := json.Marshal(harnessv2.WorkspaceBootstrapRequest{HandoffToken: token})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	body, err := s.bootstrapReceiver.SealResponse(request, plaintext)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}
