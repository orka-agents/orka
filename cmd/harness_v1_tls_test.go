package main

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHarnessV1TLSHTTPClientAuthenticatesConfiguredCA(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	caFile := filepath.Join(t.TempDir(), "ca.crt")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := newHarnessV1TLSHTTPClient(caFile)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("GET with configured harness v1 CA: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
}

func TestValidateHarnessV1TLSEndpointRejectsPlaintext(t *testing.T) {
	if err := validateHarnessV1TLSEndpoint("http://wrapper.default.svc:8080"); err == nil {
		t.Fatal("plaintext harness v1 endpoint was accepted")
	}
}

func TestValidateHarnessV1DispatchOptionsRejectsParallelWorkers(t *testing.T) {
	if err := validateHarnessV1DispatchOptions(time.Second, 1); err != nil {
		t.Fatalf("default dispatch options: %v", err)
	}
	if err := validateHarnessV1DispatchOptions(time.Second, 2); err == nil ||
		!strings.Contains(err.Error(), "harness v1 dispatch workers must be exactly 1") {
		t.Fatalf("parallel dispatch options error = %v, want exact-one rejection", err)
	}
}
