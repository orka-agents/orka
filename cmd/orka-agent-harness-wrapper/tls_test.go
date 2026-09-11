package main

import (
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func writeTLSServerCA(t *testing.T, server *httptest.Server) string {
	t.Helper()
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return caFile
}

func TestNewWrapperTLSHTTPClientRejectsPlaintextEndpoint(t *testing.T) {
	if _, err := newWrapperTLSHTTPClient("http://wrapper.default.svc:8080", "unused"); err == nil {
		t.Fatal("plaintext wrapper endpoint was accepted")
	}
}
