/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestConnectStartsConsentOpensBrowserAndWaits(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	previous := connectionReadyPollInterval
	connectionReadyPollInterval = time.Millisecond
	t.Cleanup(func() { connectionReadyPollInterval = previous })
	var gotBody map[string]string
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/connections":
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Fatalf("decode: %v", err)
			}
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"connection":   map[string]any{"name": "github-abc", "provider": "github", "mode": "readWrite", "state": "Pending"},
				"authorizeURL": "https://github.com/login/oauth/authorize?state=signed",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/connections/github-abc":
			polls++
			ready := polls >= 2
			state := "Pending"
			if ready {
				state = "Ready"
			}
			json.NewEncoder(w).Encode(map[string]any{"name": "github-abc", "provider": "github", "mode": "readWrite", "state": state, "ready": ready}) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	var opened string
	origBrowser := openBrowserFunc
	openBrowserFunc = func(url string) error { opened = url; return nil }
	t.Cleanup(func() { openBrowserFunc = origBrowser })

	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"connect", "github", "--server", srv.URL, "--token", "person-token", "--mode", "readWrite", "--timeout", "5s"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if gotBody["provider"] != "github" || gotBody["mode"] != "readWrite" {
		t.Fatalf("body = %v", gotBody)
	}
	if opened != "https://github.com/login/oauth/authorize?state=signed" {
		t.Fatalf("opened = %q", opened)
	}
	if !strings.Contains(out.String(), "Linked github (readWrite)") || polls < 2 {
		t.Fatalf("output = %q polls = %d", out.String(), polls)
	}
}

func TestConnectRejectsUnknownModeAndExplainsForbidden(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := newRootCmd()
	root.SetArgs([]string{"connect", "github", "--server", "http://127.0.0.1:1", "--token", "x", "--mode", "admin"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--mode must be") {
		t.Fatalf("err = %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"connector endpoints require a verified person"}`)) //nolint:errcheck
	}))
	defer srv.Close()
	root = newRootCmd()
	root.SetArgs([]string{"connection", "list", "--server", srv.URL, "--token", "sa-token"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "signed-in person") {
		t.Fatalf("forbidden err = %v", err)
	}
}

func TestConnectionListGetDeleteAndProviders(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var deleted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/connections":
			json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{ //nolint:errcheck
				{"name": "github-abc", "provider": "github", "mode": "readOnly", "state": "Ready", "ready": true, "linkedAt": "2026-09-28T10:00:00Z"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/connections/github-abc":
			json.NewEncoder(w).Encode(map[string]any{"name": "github-abc", "provider": "github", "mode": "readOnly", "state": "Ready", "ready": true, "message": "linked"}) //nolint:errcheck
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/connections/github-abc":
			deleted = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/connectors":
			json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{ //nolint:errcheck
				{"name": "github", "displayName": "GitHub", "ready": true, "tools": []map[string]any{{"name": "list_pull_requests", "class": "read"}}},
			}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	run := func(args ...string) string {
		root := newRootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs(append(args, "--server", srv.URL, "--token", "person-token"))
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out.String()
	}
	if out := run("connection", "list"); !strings.Contains(out, "github-abc") || !strings.Contains(out, "readOnly") || !strings.Contains(out, "PROVIDER") {
		t.Fatalf("list = %q", out)
	}
	if out := run("connection", "list", "-o", "json"); !strings.Contains(out, `"provider": "github"`) {
		t.Fatalf("list json = %q", out)
	}
	if out := run("connection", "get", "github-abc"); !strings.Contains(out, "Provider:") || !strings.Contains(out, "linked") {
		t.Fatalf("get = %q", out)
	}
	if out := run("connection", "delete", "github-abc"); deleted != "/api/v1/connections/github-abc" || !strings.Contains(out, "Connection deleted") {
		t.Fatalf("delete = %q path = %q", out, deleted)
	}
	if out := run("connection", "providers"); !strings.Contains(out, "GitHub") || !strings.Contains(out, "list_pull_requests (read)") {
		t.Fatalf("providers = %q", out)
	}
}
