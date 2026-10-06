/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
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
				"connection":   map[string]any{"name": "github-abc", "namespace": "team-a", "provider": "github", "mode": "readWrite", "state": "Revoked"},
				"authorizeURL": "https://github.com/login/oauth/authorize?state=signed",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/connections/github-abc":
			polls++
			if polls == 1 {
				// The cache has not seen the Connection the POST created yet.
				w.WriteHeader(http.StatusNotFound)
				return
			}
			ready := polls >= 3
			// The link being repaired stays Revoked until the new consent
			// completes; the command keeps waiting through it.
			state := "Revoked"
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
	if !strings.Contains(out.String(), "Linked github (readWrite)") || polls < 3 || !strings.Contains(out.String(), "orka connection complete github-abc --namespace team-a --completion") {
		t.Fatalf("output = %q polls = %d", out.String(), polls)
	}
}

func TestConnectWaitsForTheNewConsentWhenAlreadyLinked(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/connections":
			// Re-consenting an already Ready link: the API starts a new
			// consent but the current view is still the old, Ready grant.
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"connection":   map[string]any{"name": "github-abc", "provider": "github", "mode": "readOnly", "state": "Ready", "ready": true, "linkedAt": "2026-09-01T00:00:00Z", "grantSequence": 1},
				"authorizeURL": "https://github.com/login/oauth/authorize?state=signed",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/connections/github-abc":
			polls++
			// Same second, same linkedAt: only the grant sequence tells the
			// new grant apart.
			sequence := 1
			if polls >= 3 {
				sequence = 2
			}
			json.NewEncoder(w).Encode(map[string]any{"name": "github-abc", "provider": "github", "mode": "readOnly", "state": "Ready", "ready": true, "linkedAt": "2026-09-01T00:00:00Z", "grantSequence": sequence}) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	origBrowser := openBrowserFunc
	openBrowserFunc = func(string) error { return nil }
	t.Cleanup(func() { openBrowserFunc = origBrowser })

	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"connect", "github", "--server", srv.URL, "--token", "person-token", "--timeout", "5s"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if polls < 3 || !strings.Contains(out.String(), "Linked github (readOnly)") {
		t.Fatalf("the stale Ready view must not end the wait: polls = %d output = %q", polls, out.String())
	}
}

func TestConnectTimeoutBoundsAStalledPoll(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/connections":
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"connection":   map[string]any{"name": "github-abc", "provider": "github", "mode": "readOnly", "state": "Pending"},
				"authorizeURL": "https://github.com/login/oauth/authorize?state=signed",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/connections/github-abc":
			// A stalled status read must not outlive --timeout.
			select {
			case <-release:
			case <-r.Context().Done():
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	origBrowser := openBrowserFunc
	openBrowserFunc = func(string) error { return nil }
	t.Cleanup(func() { openBrowserFunc = origBrowser })

	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"connect", "github", "--server", srv.URL, "--token", "person-token", "--timeout", "300ms"})
	start := time.Now()
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "did not become ready within 300ms") {
		t.Fatalf("Execute() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the poll outlived the timeout: %s", elapsed)
	}
}

func TestConnectionCompleteSpendsTheCompletionValue(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/connections/github-abc/complete" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"name": "github-abc", "provider": "github", "mode": "readWrite", "state": "Ready", "ready": true}) //nolint:errcheck
	}))
	defer srv.Close()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"connection", "complete", "github-abc", "--completion", "one-time", "--server", srv.URL, "--token", "person-token"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if gotBody["completion"] != "one-time" || !strings.Contains(out.String(), "Linked github (readWrite): ready") {
		t.Fatalf("body = %v output = %q", gotBody, out.String())
	}
	root = newRootCmd()
	root.SetArgs([]string{"connection", "complete", "github-abc", "--server", srv.URL, "--token", "person-token"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--completion is required") {
		t.Fatalf("missing completion err = %v", err)
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

// connectionListFixture serves the connector API for the list/get/delete
// test; its fields switch the scenario the next CLI call sees.
type connectionListFixture struct {
	providerReady bool
	deleting      bool
	duplicated    bool
	// revalidated models the API judging a stored-Ready link unusable.
	revalidated bool
	deleted     string
}

func (f *connectionListFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/connections":
		items := []map[string]any{
			{"name": "github-abc", "provider": "github", "mode": "readOnly", "state": "Ready", "ready": !f.revalidated, "linkedAt": "2026-09-28T10:00:00Z", "deleting": f.deleting},
		}
		if f.duplicated {
			items = append(items, map[string]any{"name": "my-other-github", "provider": "github", "mode": "readOnly", "state": "Ready", "ready": true})
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items}) //nolint:errcheck
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/connections/github-abc":
		message := "linked"
		if f.revalidated {
			message = "the provider changed since you consented; reconnect this link before its tools can run"
		}
		json.NewEncoder(w).Encode(map[string]any{"name": "github-abc", "provider": "github", "mode": "readOnly", "state": "Ready", "ready": !f.revalidated, "message": message, "deleting": f.deleting}) //nolint:errcheck
	case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/connections/github-abc":
		f.deleted = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/connectors":
		json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{ //nolint:errcheck
			{"name": "github", "displayName": "GitHub", "ready": f.providerReady, "tools": []map[string]any{{"name": "list_pull_requests", "class": "read"}}},
		}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// requireOutput fails unless out contains every want.
func requireOutput(t *testing.T, label, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Fatalf("%s = %q, want it to contain %q", label, out, w)
		}
	}
}

// requireMatch fails unless out matches pattern exactly when match is true.
func requireMatch(t *testing.T, label, out, pattern string, match bool) {
	t.Helper()
	if regexp.MustCompile(pattern).MatchString(out) != match {
		t.Fatalf("%s = %q, want match %s = %t", label, out, pattern, match)
	}
}

func TestConnectionListGetDeleteAndProviders(t *testing.T) {
	fixture := &connectionListFixture{providerReady: true}
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(fixture)
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
	const anyTrue, readyFalse = `\s+true\s+`, `Ready:\s+false`
	requireOutput(t, "list", run("connection", "list"), "github-abc", "readOnly", "PROVIDER")
	requireOutput(t, "list json", run("connection", "list", "-o", "json"), `"provider": "github"`)
	// A link whose provider is not accepted right now is not called ready.
	fixture.providerReady = false
	out := run("connection", "list")
	requireOutput(t, "list with an unaccepted provider", out, "Ready (provider unavailable)")
	requireMatch(t, "list with an unaccepted provider", out, anyTrue, false)
	out = run("connection", "get", "github-abc")
	requireOutput(t, "get with an unaccepted provider", out, "provider unavailable")
	requireMatch(t, "get with an unaccepted provider", out, readyFalse, true)
	// A disconnect still finishing is shown as such, in every format.
	fixture.deleting = true
	out = run("connection", "get", "github-abc")
	requireOutput(t, "get while deleting", out, "Disconnecting")
	requireMatch(t, "get while deleting", out, readyFalse, true)
	requireOutput(t, "list -o json while deleting", run("connection", "list", "-o", "json"), `"state": "Disconnecting"`, `"ready": false`)
	fixture.deleting = false
	// A second link to the same provider makes both unusable, in every format.
	fixture.duplicated = true
	out = run("connection", "list")
	requireOutput(t, "list with duplicate links", out, "(duplicate link)")
	requireMatch(t, "list with duplicate links", out, anyTrue, false)
	requireOutput(t, "get -o json with duplicate links", run("connection", "get", "github-abc", "-o", "json"), `"ready": false`, "duplicate link")
	fixture.duplicated = false
	// Structured output carries the same joined readiness.
	requireOutput(t, "list -o json with an unaccepted provider", run("connection", "list", "-o", "json"), `"ready": false`, "provider unavailable", `"linkedAt"`)
	requireOutput(t, "get -o json with an unaccepted provider", run("connection", "get", "github-abc", "-o", "json"), `"ready": false`, `"message": "linked"`)
	fixture.providerReady = true
	// A stored Ready state the API judged unusable never reads as usable,
	// and get shows why.
	fixture.revalidated = true
	out = run("connection", "list")
	requireOutput(t, "list with a revalidated link", out, "Ready (not usable)")
	requireMatch(t, "list with a revalidated link", out, anyTrue, false)
	requireOutput(t, "get with a revalidated link", run("connection", "get", "github-abc"), "Ready (not usable)", "changed since you consented")
	requireOutput(t, "list -o json with a revalidated link", run("connection", "list", "-o", "json"), `"state": "Ready (not usable)"`)
	fixture.revalidated = false
	requireOutput(t, "get", run("connection", "get", "github-abc"), "Provider:", "linked")
	requireOutput(t, "delete", run("connection", "delete", "github-abc"), "Disconnect requested for github-abc")
	if fixture.deleted != "/api/v1/connections/github-abc" {
		t.Fatalf("delete path = %q", fixture.deleted)
	}
	requireOutput(t, "providers", run("connection", "providers"), "GitHub", "list_pull_requests (read)")
}
