package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func runHostCLI(t *testing.T, handler http.HandlerFunc, args ...string) (string, error) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append(args, "--server", srv.URL, "--token", "person-token"))
	err := root.Execute()
	return out.String(), err
}

func TestHostStatusWakeAndDelete(t *testing.T) {
	var deleted, woken bool
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		host := map[string]any{"name": "gh-alice", "environment": "orka-gh-alice", "phase": "Suspended", "githubLogin": "alice", "suspended": true}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == agentHostAPIPath:
		case r.Method == http.MethodPost && r.URL.Path == agentHostAPIPath+"/wake":
			woken = true
		case r.Method == http.MethodDelete && r.URL.Path == agentHostAPIPath:
			deleted = true
			host["deleting"] = true
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(host) //nolint:errcheck
	}
	out, err := runHostCLI(t, handler, "host", "status")
	if err != nil || !strings.Contains(out, "orka-gh-alice: Suspended") || !strings.Contains(out, "orka host wake") {
		t.Fatalf("status: %v %q", err, out)
	}
	if out, err := runHostCLI(t, handler, "host", "wake"); err != nil || !woken || !strings.Contains(out, "Waking orka-gh-alice") {
		t.Fatalf("wake: %v %q", err, out)
	}
	// Deleting the volume needs an explicit yes.
	if _, err := runHostCLI(t, handler, "host", "delete"); err == nil || deleted {
		t.Fatal("delete without --yes must refuse and send nothing")
	}
	if out, err := runHostCLI(t, handler, "host", "delete", "--yes"); err != nil || !deleted || !strings.Contains(out, "Deleting orka-gh-alice") {
		t.Fatalf("delete: %v %q", err, out)
	}
}

// Linking a provider that serves agent hosts waits for the host and names
// the environment to pick in the Copilot app.
func TestConnectWaitsForTheAgentHost(t *testing.T) {
	previous, previousHost := connectionReadyPollInterval, agentHostReadyPollInterval
	connectionReadyPollInterval, agentHostReadyPollInterval = time.Millisecond, time.Millisecond
	t.Cleanup(func() { connectionReadyPollInterval, agentHostReadyPollInterval = previous, previousHost })
	origBrowser := openBrowserFunc
	openBrowserFunc = func(string) error { return nil }
	t.Cleanup(func() { openBrowserFunc = origBrowser })

	hostPolls := 0
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/connections":
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"connection":   map[string]any{"name": "github-abc", "namespace": "team-a", "provider": "github", "mode": "readOnly", "state": "Pending"},
				"authorizeURL": "https://github.com/login/oauth/authorize?state=signed",
				"agentHost":    true,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/connections/github-abc":
			json.NewEncoder(w).Encode(map[string]any{"name": "github-abc", "provider": "github", "mode": "readOnly", "state": "Ready", "ready": true, "grantSequence": 1}) //nolint:errcheck
		case r.Method == http.MethodGet && r.URL.Path == agentHostAPIPath:
			hostPolls++
			switch hostPolls {
			case 1:
				// The host follows the link by a moment.
				w.WriteHeader(http.StatusNotFound)
				return
			case 2:
				// Until then, a ready host kept from another link is not
				// the one to use.
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
					"name": "gh-alice-old", "environment": "orka-gh-alice-old", "phase": "Ready", "ready": true, "connection": "github-old-abc",
				})
				return
			case 3:
				// Neither is an earlier host still being deleted.
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
					"name": "gh-alice", "environment": "orka-gh-alice", "phase": "Deleting", "ready": true, "deleting": true, "connection": "github-abc",
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"name": "gh-alice", "environment": "orka-gh-alice", "phase": "Ready", "ready": hostPolls >= 5, "connection": "github-abc",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
	out, err := runHostCLI(t, handler, "connect", "github", "--timeout", "5s")
	if err != nil {
		t.Fatalf("connect: %v %q", err, out)
	}
	if !strings.Contains(out, "Linked github") || !strings.Contains(out, "Host ready: orka-gh-alice.") || hostPolls < 5 {
		t.Fatalf("output = %q, host polls = %d", out, hostPolls)
	}
}

// A relink finds the host its earlier link left Failed with
// ConnectionUnavailable, and waits for it; a transaction token read from
// stdin authenticates every request of the command, not only the first.
func TestConnectWaitsThroughARelinkWithAStdinToken(t *testing.T) {
	previous, previousHost := connectionReadyPollInterval, agentHostReadyPollInterval
	connectionReadyPollInterval, agentHostReadyPollInterval = time.Millisecond, time.Millisecond
	t.Cleanup(func() { connectionReadyPollInterval, agentHostReadyPollInterval = previous, previousHost })
	origBrowser := openBrowserFunc
	openBrowserFunc = func(string) error { return nil }
	t.Cleanup(func() { openBrowserFunc = origBrowser })
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteString("txn-from-stdin"); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	origStdin := os.Stdin
	os.Stdin = reader
	t.Cleanup(func() { os.Stdin = origStdin })

	hostPolls, unauthenticated := 0, 0
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Txn-Token") != "txn-from-stdin" {
			unauthenticated++
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/connections":
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"connection":   map[string]any{"name": "github-abc", "namespace": "team-a", "provider": "github", "mode": "readOnly", "state": "Revoked"},
				"authorizeURL": "https://github.com/login/oauth/authorize?state=signed",
				"agentHost":    true,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/connections/github-abc":
			json.NewEncoder(w).Encode(map[string]any{"name": "github-abc", "provider": "github", "mode": "readOnly", "state": "Ready", "ready": true, "grantSequence": 1}) //nolint:errcheck
		case r.Method == http.MethodGet && r.URL.Path == agentHostAPIPath:
			hostPolls++
			host := map[string]any{"name": "gh-alice", "environment": "orka-gh-alice", "phase": "Failed", "reason": "ConnectionUnavailable", "connection": "github-abc"}
			if hostPolls >= 3 {
				host = map[string]any{"name": "gh-alice", "environment": "orka-gh-alice", "phase": "Ready", "ready": true, "connection": "github-abc"}
			}
			json.NewEncoder(w).Encode(host) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
	out, err := runHostCLI(t, handler, "connect", "github", "--timeout", "5s", "--txn-token-file", "-")
	if err != nil {
		t.Fatalf("connect: %v %q", err, out)
	}
	if !strings.Contains(out, "Host ready: orka-gh-alice") || hostPolls < 3 {
		t.Fatalf("output = %q, host polls = %d", out, hostPolls)
	}
	if unauthenticated != 0 {
		t.Fatalf("%d requests went without the stdin transaction token", unauthenticated)
	}
}

// A relink finds the host still showing a failure the new link clears, such
// as the other GitHub account the earlier link gave, and waits for it. A
// failure that outlasts the wait is reported, and so is a wait the deadline
// cuts short mid-request.
func TestConnectWaitsOutAFailureTheLinkMayClear(t *testing.T) {
	previous, previousHost := connectionReadyPollInterval, agentHostReadyPollInterval
	connectionReadyPollInterval, agentHostReadyPollInterval = time.Millisecond, time.Millisecond
	t.Cleanup(func() { connectionReadyPollInterval, agentHostReadyPollInterval = previous, previousHost })
	origBrowser := openBrowserFunc
	openBrowserFunc = func(string) error { return nil }
	t.Cleanup(func() { openBrowserFunc = origBrowser })
	failed := map[string]any{
		"name": "gh-alice", "environment": "orka-gh-alice", "phase": "Failed", "reason": "GitHubIdentityChanged",
		"message": "reconnect with alice", "connection": "github-abc",
	}
	ready := map[string]any{"name": "gh-alice", "environment": "orka-gh-alice", "phase": "Ready", "ready": true, "connection": "github-abc"}
	connect := func(t *testing.T, host func(w http.ResponseWriter, r *http.Request, poll int)) (string, error) {
		t.Helper()
		polls := 0
		return runHostCLI(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/connections":
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
					"connection":   map[string]any{"name": "github-abc", "namespace": "team-a", "provider": "github", "mode": "readOnly", "state": "Ready", "ready": true, "grantSequence": 1},
					"authorizeURL": "https://github.com/login/oauth/authorize?state=signed",
					"agentHost":    true,
				})
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/connections/github-abc":
				json.NewEncoder(w).Encode(map[string]any{"name": "github-abc", "provider": "github", "mode": "readOnly", "state": "Ready", "ready": true, "grantSequence": 2}) //nolint:errcheck
			case r.Method == http.MethodGet && r.URL.Path == agentHostAPIPath:
				polls++
				host(w, r, polls)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}, "connect", "github", "--timeout", "500ms")
	}

	t.Run("cleared", func(t *testing.T) {
		out, err := connect(t, func(w http.ResponseWriter, _ *http.Request, poll int) {
			if poll < 3 {
				json.NewEncoder(w).Encode(failed) //nolint:errcheck
				return
			}
			json.NewEncoder(w).Encode(ready) //nolint:errcheck
		})
		if err != nil || !strings.Contains(out, "Host ready: orka-gh-alice") {
			t.Fatalf("connect: %v %q", err, out)
		}
	})
	t.Run("outlasts the wait", func(t *testing.T) {
		_, err := connect(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
			json.NewEncoder(w).Encode(failed) //nolint:errcheck
		})
		if err == nil || !strings.Contains(err.Error(), "GitHubIdentityChanged: reconnect with alice") || !strings.Contains(err.Error(), "orka host status") {
			t.Fatalf("err = %v, want the failure and the status hint", err)
		}
	})
	t.Run("deadline mid-request", func(t *testing.T) {
		_, err := connect(t, func(_ http.ResponseWriter, r *http.Request, _ int) {
			<-r.Context().Done()
		})
		if err == nil || err.Error() != "your agent host is not ready yet; check with 'orka host status'" {
			t.Fatalf("err = %v, want the status hint", err)
		}
	})
}
