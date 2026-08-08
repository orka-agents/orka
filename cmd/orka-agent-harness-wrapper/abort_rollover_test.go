package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/orka-agents/orka/internal/harness"
)

func TestRunAbortRolloverAuthenticatesAndRequiresExactGeneration(t *testing.T) {
	const token = "abort-rollover-controller-token-value"
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != harness.AdminAbortRolloverPath || r.Method != http.MethodPost {
			harness.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			harness.WriteError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		var request harness.DurableRolloverAbortRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.ExpectedGeneration != "generation-41" {
			harness.WriteError(w, http.StatusBadRequest, "invalid abort")
			return
		}
		harness.WriteJSON(w, http.StatusOK, harness.DurableRolloverAbortResponse{
			CurrentGeneration: request.ExpectedGeneration,
			AdmissionReopened: true,
		})
	}))
	defer server.Close()
	caFile := writeTLSServerCA(t, server)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := runAbortRollover([]string{
		"--endpoint=" + server.URL,
		"--bearer-token-file=" + tokenFile,
		"--ca-file=" + caFile,
		"--expected-generation=generation-41",
		"--timeout=2s",
	}); err != nil {
		t.Fatalf("runAbortRollover: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("abort calls = %d, want 1", calls.Load())
	}
}

func TestRunAbortRolloverRejectsInvalidResponseWithoutLeakingBearer(t *testing.T) {
	const token = "abort-rollover-secret-token-value"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		harness.WriteJSON(w, http.StatusOK, harness.DurableRolloverAbortResponse{
			CurrentGeneration: "different-generation",
			AdmissionReopened: true,
		})
	}))
	defer server.Close()
	caFile := writeTLSServerCA(t, server)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runAbortRollover([]string{
		"--endpoint=" + server.URL,
		"--bearer-token-file=" + tokenFile,
		"--ca-file=" + caFile,
		"--expected-generation=generation-41",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid rollover abort") {
		t.Fatalf("runAbortRollover error = %v, want invalid-response rejection", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("runAbortRollover error leaked bearer: %v", err)
	}
}
