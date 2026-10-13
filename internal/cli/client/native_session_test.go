package client

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
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

func TestNativeSessionRequestsRejectRedirects(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(method+"/"+strconv.Itoa(status), func(t *testing.T) {
				var targetRequests atomic.Int64
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					targetRequests.Add(1)
					if err := json.NewEncoder(w).Encode(NativeSessionExport{Data: []byte("private-history")}); err != nil {
						t.Error(err)
					}
				}))
				defer target.Close()
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, target.URL, status)
				}))
				defer server.Close()
				c := NewWithNamespace(server.URL, "", "team")
				var err error
				if method == http.MethodPost {
					_, err = c.ImportNativeSession(t.Context(), "session", "operation", []byte("private-history"))
				} else {
					_, err = c.ExportNativeSession(t.Context(), "session")
				}
				if targetRequests.Load() != 0 {
					t.Fatal("native migration contacted the redirect target")
				}
				if err == nil || !strings.Contains(err.Error(), "HTTP "+strconv.Itoa(status)) {
					t.Fatalf("expected the original redirect status, got %v", err)
				}
			})
		}
	}
}

func TestNativeSessionRedirectPolicyPreservesSharedClient(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("native migration contacted the redirect target")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	c := NewWithNamespace(server.URL, "", "team")
	c.HTTPClient = server.Client()
	var redirects atomic.Int64
	c.HTTPClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		redirects.Add(1)
		return nil
	}
	_, err := c.ImportNativeSession(t.Context(), "session", "operation", []byte("private-history"))
	if err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("configured TLS transport was not preserved or redirect was accepted: %v", err)
	}
	if redirects.Load() != 0 {
		t.Fatal("native request used the shared client's permissive redirect policy")
	}
	if err := c.HTTPClient.CheckRedirect(nil, nil); err != nil || redirects.Load() != 1 {
		t.Fatal("native request changed the shared client's redirect policy")
	}
}
