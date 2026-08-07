package main

import (
	"context"
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

func TestRunDrainClosesWaitsAndPreparesExactGeneration(t *testing.T) {
	const token = "drain-controller-token-value"
	var (
		drainCalls    atomic.Int32
		rolloverCalls atomic.Int32
	)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			harness.WriteError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		switch r.URL.Path {
		case harness.AdminClosePath:
			harness.WriteJSON(w, http.StatusOK, harness.DurableAdmissionCloseResponse{AdmissionClosed: true})
		case harness.AdminDrainPath:
			completed := drainCalls.Add(1) >= 2
			harness.WriteJSON(w, http.StatusOK, harness.DurableDrainStatus{
				AdmissionClosed: true,
				Completed:       completed,
			})
		case harness.AdminRolloverPath:
			rolloverCalls.Add(1)
			var request harness.DurableRolloverPrepareRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.NextGeneration != "42" {
				harness.WriteError(w, http.StatusBadRequest, "invalid rollover")
				return
			}
			harness.WriteJSON(w, http.StatusOK, harness.DurableRolloverPrepareResponse{
				CurrentGeneration: "41",
				NextGeneration:    request.NextGeneration,
				Prepared:          true,
			})
		default:
			harness.WriteError(w, http.StatusNotFound, "not found")
		}
	}))
	defer server.Close()
	caFile := writeTLSServerCA(t, server)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := runDrain([]string{
		"--endpoint=" + server.URL,
		"--bearer-token-file=" + tokenFile,
		"--ca-file=" + caFile,
		"--next-generation=42",
		"--timeout=2s",
		"--poll-interval=1ms",
	}); err != nil {
		t.Fatalf("runDrain: %v", err)
	}
	if drainCalls.Load() < 2 || rolloverCalls.Load() != 1 {
		t.Fatalf("calls: drain=%d rollover=%d, want >=2/1", drainCalls.Load(), rolloverCalls.Load())
	}
}

func TestRunDrainRetiresControllerBeforeClosingWrapper(t *testing.T) {
	const (
		wrapperToken    = "wrapper-drain-controller-token"
		controllerToken = "projected-service-account-token"
	)
	var retirementCalls atomic.Int32
	controller := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+controllerToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		retirementCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer controller.Close()
	wrapper := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if retirementCalls.Load() != 1 {
			harness.WriteError(w, http.StatusConflict, "controller retirement was not completed first")
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+wrapperToken {
			harness.WriteError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		switch r.URL.Path {
		case harness.AdminClosePath:
			harness.WriteJSON(w, http.StatusOK, harness.DurableAdmissionCloseResponse{AdmissionClosed: true})
		case harness.AdminDrainPath:
			harness.WriteJSON(w, http.StatusOK, harness.DurableDrainStatus{AdmissionClosed: true, Completed: true})
		default:
			harness.WriteError(w, http.StatusNotFound, "not found")
		}
	}))
	defer wrapper.Close()
	tempDir := t.TempDir()
	wrapperTokenFile := filepath.Join(tempDir, "wrapper-token")
	controllerTokenFile := filepath.Join(tempDir, "controller-token")
	if err := os.WriteFile(wrapperTokenFile, []byte(wrapperToken), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(controllerTokenFile, []byte(controllerToken), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := runDrain([]string{
		"--endpoint=" + wrapper.URL,
		"--bearer-token-file=" + wrapperTokenFile,
		"--ca-file=" + writeTLSServerCA(t, wrapper),
		"--controller-endpoint=" + controller.URL + "/internal/v1/harness-v1/retirement",
		"--controller-token-file=" + controllerTokenFile,
		"--controller-ca-file=" + writeTLSServerCA(t, controller),
		"--timeout=2s",
		"--poll-interval=1ms",
	}); err != nil {
		t.Fatalf("runDrain: %v", err)
	}
	if retirementCalls.Load() != 1 {
		t.Fatalf("controller retirement calls = %d, want 1", retirementCalls.Load())
	}
}

func TestRunDrainRejectsPlaintextControllerRetirement(t *testing.T) {
	err := requestControllerRetirement(
		context.Background(), "http://controller.example/internal/v1/harness-v1/retirement", "token", "ca",
	)
	if err == nil || !strings.Contains(err.Error(), "HTTPS URL") {
		t.Fatalf("requestControllerRetirement() error = %v, want HTTPS rejection", err)
	}
}

func TestRunDrainTimesOutWithoutPreparingRolloverOrLeakingBearer(t *testing.T) {
	const token = "timeout-drain-token-value"
	var rolloverCalls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case harness.AdminClosePath:
			harness.WriteJSON(w, http.StatusOK, harness.DurableAdmissionCloseResponse{AdmissionClosed: true})
		case harness.AdminDrainPath:
			harness.WriteJSON(w, http.StatusOK, harness.DurableDrainStatus{AdmissionClosed: true})
		case harness.AdminRolloverPath:
			rolloverCalls.Add(1)
			harness.WriteError(w, http.StatusInternalServerError, "unexpected")
		}
	}))
	defer server.Close()
	caFile := writeTLSServerCA(t, server)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runDrain([]string{
		"--endpoint=" + server.URL,
		"--bearer-token-file=" + tokenFile,
		"--ca-file=" + caFile,
		"--next-generation=2",
		"--timeout=30ms",
		"--poll-interval=5ms",
	})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("runDrain timeout error = %v", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("runDrain error leaked bearer: %v", err)
	}
	if rolloverCalls.Load() != 0 {
		t.Fatalf("rollover calls = %d, want 0", rolloverCalls.Load())
	}
}

func TestRunDrainWithoutReplacementGenerationClosesAndDrainsOnly(t *testing.T) {
	const token = "shutdown-drain-token-value"
	var rolloverCalls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			harness.WriteError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		switch r.URL.Path {
		case harness.AdminClosePath:
			harness.WriteJSON(w, http.StatusOK, harness.DurableAdmissionCloseResponse{AdmissionClosed: true})
		case harness.AdminDrainPath:
			harness.WriteJSON(w, http.StatusOK, harness.DurableDrainStatus{AdmissionClosed: true, Completed: true})
		case harness.AdminRolloverPath:
			rolloverCalls.Add(1)
			harness.WriteError(w, http.StatusInternalServerError, "unexpected")
		default:
			harness.WriteError(w, http.StatusNotFound, "not found")
		}
	}))
	defer server.Close()
	caFile := writeTLSServerCA(t, server)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := runDrain([]string{
		"--endpoint=" + server.URL,
		"--bearer-token-file=" + tokenFile,
		"--ca-file=" + caFile,
		"--timeout=2s",
		"--poll-interval=1ms",
	}); err != nil {
		t.Fatalf("runDrain: %v", err)
	}
	if rolloverCalls.Load() != 0 {
		t.Fatalf("rollover calls = %d, want 0", rolloverCalls.Load())
	}
}
