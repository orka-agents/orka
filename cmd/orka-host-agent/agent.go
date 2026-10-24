/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	"github.com/microsoft/agent-host-protocol/clients/go/ahpws"

	"github.com/orka-agents/orka/internal/agenthost"
)

const (
	maxTokenBytes  = 4096
	maxStatusBytes = 64 << 10
	// staleAfter marks an idle status the host has not refreshed recently.
	// Hosts rewrite it at least every five minutes; twice that absorbs write
	// and clock jitter, so a host that keeps to the contract never flaps.
	staleAfter = 10 * time.Minute
)

// handshaker completes an AHP initialize and ping at a WebSocket URL.
type handshaker func(ctx context.Context, target string) error

type agent struct {
	listenPort       int
	tokenFile        string
	statusFile       string
	dialTimeout      time.Duration
	handshakeTimeout time.Duration
	handshake        handshaker
	now              func() time.Time
	logger           *log.Logger

	started atomic.Bool
	mu      sync.Mutex
}

func (a *agent) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeText(w, http.StatusOK, "ok") })
	mux.HandleFunc("GET /startupz", a.startupz)
	mux.HandleFunc("GET /readyz", a.readyz)
	mux.HandleFunc("GET /status", a.status)
	return mux
}

// startupz answers the host container's startup probe. It completes one AHP
// handshake without authenticating, so it never claims a single-user host,
// and then stops talking AHP so it cannot keep the host from being idle.
func (a *agent) startupz(w http.ResponseWriter, r *http.Request) {
	if !a.ensureStarted(r.Context()) {
		writeText(w, http.StatusServiceUnavailable, "host is not answering the Agent Host Protocol")
		return
	}
	writeText(w, http.StatusOK, "started")
}

// ensureStarted completes the startup handshake once. The host's startup
// probe usually does it, but a restarted sidecar starts over after that probe
// has passed for good, so the readiness and liveness probes do it then.
func (a *agent) ensureStarted(ctx context.Context) bool {
	if a.started.Load() {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.started.Load() {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, a.handshakeTimeout)
	defer cancel()
	if err := a.probe(ctx); err != nil {
		a.logger.Printf("startup check failed: %v", err)
		return false
	}
	a.started.Store(true)
	a.logger.Printf("host answered the Agent Host Protocol handshake")
	return true
}

// readyz answers the readiness and liveness probes after startup. A host
// that reports idle status is alive while it keeps that status fresh, which
// leaves its listener untouched; hosts may log every connection that never
// completes a handshake. Otherwise readyz checks that the loopback listener
// accepts connections. It never opens an AHP session.
func (a *agent) readyz(w http.ResponseWriter, r *http.Request) {
	if !a.ensureStarted(r.Context()) {
		writeText(w, http.StatusServiceUnavailable, "not started")
		return
	}
	if a.statusFile != "" {
		status, err := a.readStatus()
		if err != nil || status.Stale {
			writeText(w, http.StatusServiceUnavailable, "host idle status is missing or stale")
			return
		}
		writeText(w, http.StatusOK, "ready")
		return
	}
	conn, err := net.DialTimeout("tcp", a.listenAddress(), a.dialTimeout)
	if err != nil {
		writeText(w, http.StatusServiceUnavailable, "host listener is not accepting connections")
		return
	}
	_ = conn.Close()
	writeText(w, http.StatusOK, "ready")
}

// status serves the host's idle status, limited to the fields Orka reads.
func (a *agent) status(w http.ResponseWriter, _ *http.Request) {
	if a.statusFile == "" {
		writeText(w, http.StatusNotFound, "this host does not report idle status")
		return
	}
	response, err := a.readStatus()
	if err != nil {
		writeText(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(response)
}

// readStatus reads and parses the host's idle status file. The host does not
// choose staleness; the agent judges it from updatedAt, or from the file's
// modification time when the host writes none. A time further in the future
// than staleAfter is as untrustworthy as an old one.
func (a *agent) readStatus() (agenthost.IdleStatus, error) {
	file, err := os.Open(a.statusFile)
	if err != nil {
		return agenthost.IdleStatus{}, errors.New("idle status is not available")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return agenthost.IdleStatus{}, errors.New("idle status is not readable")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxStatusBytes+1))
	if err != nil {
		return agenthost.IdleStatus{}, errors.New("idle status is not readable")
	}
	parsed, err := agenthost.ParseIdleStatus(raw)
	if err != nil {
		return agenthost.IdleStatus{}, errors.New("idle status is not valid")
	}
	updated := info.ModTime()
	if parsed.UpdatedAt != nil {
		updated = *parsed.UpdatedAt
	}
	age := a.now().Sub(updated)
	parsed.Stale = age > staleAfter || age < -staleAfter
	// So is an idleSince that far ahead: idle time would count from the
	// future, and the host would not suspend until then.
	if parsed.IdleSince != nil && parsed.IdleSince.Sub(a.now()) > staleAfter {
		parsed.Stale = true
	}
	return parsed, nil
}

func (a *agent) probe(ctx context.Context) error {
	token, err := readToken(a.tokenFile)
	if err != nil {
		return err
	}
	target := (&url.URL{
		Scheme:   "ws",
		Host:     a.listenAddress(),
		Path:     "/",
		RawQuery: url.Values{"tkn": []string{token}}.Encode(),
	}).String()
	if err := a.handshake(ctx, target); err != nil {
		return redact(err, token)
	}
	return nil
}

func (a *agent) listenAddress() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(a.listenPort))
}

// ahpHandshake offers every protocol version the AHP Go client supports,
// then pings. It never sends authenticate.
func ahpHandshake(ctx context.Context, target string) error {
	transport, err := ahpws.Connect(ctx, target)
	if err != nil {
		return err
	}
	client, err := ahp.Connect(ctx, transport, ahp.DefaultConfig())
	if err != nil {
		_ = transport.Close(ctx)
		return err
	}
	defer func() { _ = client.Shutdown(context.WithoutCancel(ctx)) }()
	if _, err := client.Initialize(ctx, clientID(), ahptypes.SupportedProtocolVersions(), nil); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if err := client.Ping(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	return nil
}

func clientID() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "orka-host-agent"
	}
	return "orka-host-agent-" + hex.EncodeToString(buf)
}

func readToken(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("connection token is not available")
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, maxTokenBytes+1))
	if err != nil || len(raw) > maxTokenBytes {
		return "", errors.New("connection token is not readable")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", errors.New("connection token is empty")
	}
	return token, nil
}

// redact removes the token, raw and query-escaped, from an error message.
func redact(err error, token string) error {
	message := err.Error()
	for _, secret := range []string{token, url.QueryEscape(token)} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "REDACTED")
		}
	}
	return errors.New(message)
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body+"\n")
}
