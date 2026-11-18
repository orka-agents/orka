/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/orka-agents/orka/internal/agenthost"
)

const testToken = "connection-token-0123456789abcdef"

// fakeHost serves a minimal Agent Host Protocol endpoint gated by a token.
type fakeHost struct {
	server        *httptest.Server
	methods       chan string
	authenticated atomic.Bool
}

func newFakeHost(t *testing.T, token string) *fakeHost {
	t.Helper()
	host := &fakeHost{methods: make(chan string, 16)}
	host.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("tkn") != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		for {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var request struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(raw, &request) != nil {
				return
			}
			host.methods <- request.Method
			if request.Method == "authenticate" {
				host.authenticated.Store(true)
			}
			var result any
			if request.Method == "initialize" {
				result = map[string]any{"protocolVersion": "0.9.0", "serverSeq": 0, "snapshots": []any{}}
			}
			response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
			if err := conn.Write(r.Context(), websocket.MessageText, response); err != nil {
				return
			}
		}
	}))
	t.Cleanup(host.server.Close)
	return host
}

func (h *fakeHost) port(t *testing.T) int {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(h.server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	value, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newAgent(port int, tokenFile, statusFile string, logs io.Writer) *agent {
	return &agent{
		listenPort:       port,
		tokenFile:        tokenFile,
		statusFile:       statusFile,
		dialTimeout:      time.Second,
		handshakeTimeout: 5 * time.Second,
		handshake:        ahpHandshake,
		now:              time.Now,
		logger:           log.New(logs, "", 0),
	}
}

func get(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

func TestStartupCompletesAHPHandshakeWithoutAuthenticating(t *testing.T) {
	host := newFakeHost(t, testToken)
	agent := newAgent(host.port(t), writeFile(t, "token", testToken+"\n"), "", io.Discard)
	routes := agent.routes()

	if got := get(t, routes, "/startupz"); got.Code != http.StatusOK {
		t.Fatalf("startupz = %d %q", got.Code, got.Body.String())
	}
	seen := []string{<-host.methods, <-host.methods}
	if seen[0] != "initialize" || seen[1] != "ping" {
		t.Fatalf("host saw %v, want initialize then ping", seen)
	}
	if host.authenticated.Load() {
		t.Fatal("the startup check must never authenticate")
	}
	if got := get(t, routes, "/readyz"); got.Code != http.StatusOK {
		t.Fatalf("readyz after startup = %d", got.Code)
	}
	// Later startup probes do not open another AHP session.
	if got := get(t, routes, "/startupz"); got.Code != http.StatusOK {
		t.Fatalf("second startupz = %d", got.Code)
	}
	select {
	case method := <-host.methods:
		t.Fatalf("second startup probe reached the host with %s", method)
	case <-time.After(100 * time.Millisecond):
	}
}

// A restarted sidecar starts over after the host's startup probe has passed
// for good, so its readiness probe completes the handshake instead.
func TestARestartedSidecarStartsFromItsReadinessProbe(t *testing.T) {
	host := newFakeHost(t, testToken)
	agent := newAgent(host.port(t), writeFile(t, "token", testToken), "", io.Discard)
	routes := agent.routes()

	if got := get(t, routes, "/readyz"); got.Code != http.StatusOK {
		t.Fatalf("readyz of a restarted sidecar = %d %q", got.Code, got.Body.String())
	}
	if seen := []string{<-host.methods, <-host.methods}; seen[0] != "initialize" || seen[1] != "ping" {
		t.Fatalf("host saw %v, want initialize then ping", seen)
	}
	if host.authenticated.Load() {
		t.Fatal("the startup check must never authenticate")
	}
	if got := get(t, routes, "/readyz"); got.Code != http.StatusOK {
		t.Fatalf("second readyz = %d", got.Code)
	}
	select {
	case method := <-host.methods:
		t.Fatalf("a later readiness probe reached the host with %s", method)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestReadinessFailsWhileTheHostDoesNotAnswer(t *testing.T) {
	host := newFakeHost(t, testToken)
	agent := newAgent(host.port(t), writeFile(t, "token", testToken), "", io.Discard)
	host.server.Close()
	if got := get(t, agent.routes(), "/readyz"); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz before the host answers = %d", got.Code)
	}
}

func TestStatusStalenessUsesTheFileTimeAndRejectsTheFuture(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	// A host that writes no updatedAt is judged by when it last wrote the file.
	status := writeFile(t, "idle.json", `{"idle":false,"idleSince":null}`)
	agent := newAgent(1, "unused", status, io.Discard)
	agent.now = func() time.Time { return now }
	var body agenthost.IdleStatus
	if err := json.Unmarshal(get(t, agent.routes(), "/status").Body.Bytes(), &body); err != nil || body.Stale {
		t.Fatalf("a freshly written status without updatedAt must be fresh: %+v %v", body, err)
	}
	if err := os.Chtimes(status, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(get(t, agent.routes(), "/status").Body.Bytes(), &body); err != nil || !body.Stale {
		t.Fatalf("a status file untouched for an hour must be stale: %+v %v", body, err)
	}

	future := writeFile(t, "idle.json", `{"idle":true,"updatedAt":"`+now.Add(time.Hour).Format(time.RFC3339)+`"}`)
	agent = newAgent(1, "unused", future, io.Discard)
	agent.now = func() time.Time { return now }
	if err := json.Unmarshal(get(t, agent.routes(), "/status").Body.Bytes(), &body); err != nil || !body.Stale {
		t.Fatalf("an updatedAt an hour ahead must be stale: %+v %v", body, err)
	}

	// A fresh document whose idleSince is far ahead, as after a clock that
	// jumped and came back, cannot say how long the host has been idle.
	for since, stale := range map[time.Duration]bool{time.Minute: false, time.Hour: true} {
		ahead := writeFile(t, "idle.json", `{"idle":true,"idleSince":"`+now.Add(since).Format(time.RFC3339)+
			`","updatedAt":"`+now.Format(time.RFC3339)+`"}`)
		agent = newAgent(1, "unused", ahead, io.Discard)
		agent.now = func() time.Time { return now }
		var got agenthost.IdleStatus
		if err := json.Unmarshal(get(t, agent.routes(), "/status").Body.Bytes(), &got); err != nil || got.Stale != stale {
			t.Fatalf("idleSince %s ahead: stale = %v, want %v (%v)", since, got.Stale, stale, err)
		}
	}
}

func TestStartupFailsWithWrongTokenAndNeverLogsIt(t *testing.T) {
	host := newFakeHost(t, "a-different-token-value-000000000")
	logs := &bytes.Buffer{}
	agent := newAgent(host.port(t), writeFile(t, "token", testToken), "", logs)

	got := get(t, agent.routes(), "/startupz")
	if got.Code != http.StatusServiceUnavailable {
		t.Fatalf("startupz with a wrong token = %d", got.Code)
	}
	if !strings.Contains(logs.String(), "startup check failed") {
		t.Fatalf("expected a failure log, got %q", logs.String())
	}
	if strings.Contains(logs.String(), testToken) || strings.Contains(got.Body.String(), testToken) {
		t.Fatal("the connection token leaked into a log or response")
	}
}

func TestStartupFailsWithoutAToken(t *testing.T) {
	host := newFakeHost(t, testToken)
	agent := newAgent(host.port(t), filepath.Join(t.TempDir(), "missing"), "", io.Discard)
	if got := get(t, agent.routes(), "/startupz"); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("startupz without a token = %d", got.Code)
	}
	empty := newAgent(host.port(t), writeFile(t, "token", "  \n"), "", io.Discard)
	if got := get(t, empty.routes(), "/startupz"); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("startupz with an empty token = %d", got.Code)
	}
}

func TestReadinessFollowsTheListener(t *testing.T) {
	host := newFakeHost(t, testToken)
	agent := newAgent(host.port(t), writeFile(t, "token", testToken), "", io.Discard)
	routes := agent.routes()
	if got := get(t, routes, "/startupz"); got.Code != http.StatusOK {
		t.Fatalf("startupz = %d", got.Code)
	}
	host.server.Close()
	if got := get(t, routes, "/readyz"); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with the listener gone = %d", got.Code)
	}
}

func TestStatusServesOnlyKnownFields(t *testing.T) {
	now := time.Date(2026, 10, 17, 21, 30, 0, 0, time.UTC)
	fresh := writeFile(t, "idle.json", `{"idle":true,"idleSince":"2026-10-17T21:29:40Z","activeSessions":0,`+
		`"busySessions":0,"backgroundWork":0,"connectedClients":0,"updatedAt":"2026-10-17T21:29:55Z","secret":"x"}`)
	agent := newAgent(1, "unused", fresh, io.Discard)
	agent.now = func() time.Time { return now }

	got := get(t, agent.routes(), "/status")
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d", got.Code)
	}
	if strings.Contains(got.Body.String(), "secret") {
		t.Fatalf("status passed through an unknown field: %s", got.Body.String())
	}
	var body agenthost.IdleStatus
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Idle || body.Stale || body.IdleSince == nil || body.ConnectedClients == nil || *body.ConnectedClients != 0 {
		t.Fatalf("status = %+v", body)
	}

	agent.now = func() time.Time { return now.Add(time.Hour) }
	if err := json.Unmarshal(get(t, agent.routes(), "/status").Body.Bytes(), &body); err != nil || !body.Stale {
		t.Fatalf("an old status must be stale: %+v %v", body, err)
	}
}

func TestStatusUnavailable(t *testing.T) {
	none := newAgent(1, "unused", "", io.Discard)
	if got := get(t, none.routes(), "/status"); got.Code != http.StatusNotFound {
		t.Fatalf("status without a status file = %d", got.Code)
	}
	missing := newAgent(1, "unused", filepath.Join(t.TempDir(), "idle.json"), io.Discard)
	if got := get(t, missing.routes(), "/status"); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("status with a missing file = %d", got.Code)
	}
	invalid := newAgent(1, "unused", writeFile(t, "idle.json", "not json"), io.Discard)
	if got := get(t, invalid.routes(), "/status"); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("status with an invalid file = %d", got.Code)
	}
}

func TestRedactRemovesRawAndEscapedTokens(t *testing.T) {
	token := "tok+en/with=chars"
	err := redact(io.ErrUnexpectedEOF, token)
	if err.Error() != io.ErrUnexpectedEOF.Error() {
		t.Fatalf("redact changed an unrelated message: %v", err)
	}
	dialed := "ws://127.0.0.1/?tkn=" + "tok%2Ben%2Fwith%3Dchars " + token
	wrapped := redact(&net.OpError{Op: "dial", Err: io.EOF, Net: dialed}, token)
	if strings.Contains(wrapped.Error(), "tok") {
		t.Fatalf("token survived redaction: %v", wrapped)
	}
}

func TestReadinessUsesAFreshStatusFileInsteadOfTheListener(t *testing.T) {
	host := newFakeHost(t, testToken)
	now := time.Date(2026, 10, 18, 3, 0, 0, 0, time.UTC)
	status := writeFile(t, "idle.json", `{"idle":true,"updatedAt":"2026-10-18T02:59:30Z"}`)
	agent := newAgent(host.port(t), writeFile(t, "token", testToken), status, io.Discard)
	agent.now = func() time.Time { return now }
	routes := agent.routes()
	if got := get(t, routes, "/startupz"); got.Code != http.StatusOK {
		t.Fatalf("startupz = %d", got.Code)
	}
	<-host.methods
	<-host.methods
	// The listener is gone, but a fresh status file means the host is alive,
	// and the check never touches the listener.
	host.server.Close()
	if got := get(t, routes, "/readyz"); got.Code != http.StatusOK {
		t.Fatalf("readyz with a fresh status file = %d", got.Code)
	}
	agent.now = func() time.Time { return now.Add(time.Hour) }
	if got := get(t, routes, "/readyz"); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with a stale status file = %d", got.Code)
	}
}
