package client

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNativeSessionErrorDoesNotExposePrivateResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-only" || r.URL.Query().Get("namespace") != "team" {
			t.Error("missing current caller authorization or namespace")
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, "private-conversation-marker")
	}))
	defer server.Close()
	c := NewWithNamespace(server.URL, "test-only", "team")
	_, err := c.ExportNativeSession(t.Context(), "session")
	if err == nil || strings.Contains(err.Error(), "private-conversation-marker") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestNativeSessionResponseIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", (1<<20)+1))
	}))
	defer server.Close()
	if _, err := NewWithNamespace(server.URL, "", "team").ExportNativeSession(t.Context(), "session"); err == nil {
		t.Fatal("oversized response accepted")
	}
}
