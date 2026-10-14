/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package aimockcapture is a test-only, in-memory capture proxy for aimock.
// It retains raw model request bodies, never headers or response bodies. Do not
// expose this fixture outside an isolated test environment.
package aimockcapture

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// MaxBodyBytes bounds each captured request body.
	MaxBodyBytes int64 = 4 << 20
	// MaxTotalBodyBytes bounds retained bodies, including in-flight reservations.
	MaxTotalBodyBytes int64 = 64 << 20
	// MaxEntries bounds retained entries, including in-flight reservations.
	MaxEntries = 4096

	healthTimeout = 2 * time.Second
	capturePath   = "/__orka_eval/capture"
	healthPath    = "/__orka_eval/health"
)

// Entry contains exactly the request body and safe response lifecycle metadata.
// Status is zero until a final response status is available.
type Entry struct {
	RequestID string `json:"requestId"`
	Path      string `json:"path"`
	Method    string `json:"method"`
	Body      string `json:"body"`
	Status    int    `json:"status"`
	Complete  bool   `json:"complete"`
	Error     string `json:"error"`
}

// Capture is a consistent snapshot; entries are never evicted or truncated.
// InstanceID is nonempty and stable for this handler's lifetime, including empty
// snapshots. A recreated handler has a new identity.
type Capture struct {
	InstanceID string  `json:"instanceId"`
	Entries    []Entry `json:"entries"`
	Rejected   uint64  `json:"rejected"`
}

type limits struct {
	body, total int64
	entries     int
}

// Handler owns an isolated capture store and reverse proxy.
type Handler struct {
	proxy        *httputil.ReverseProxy
	healthClient *http.Client
	healthURL    string
	limits       limits
	idPrefix     string
	captureReads chan struct{}

	mu        sync.Mutex
	entries   []Entry
	bodyBytes int64
	reserved  int64
	pending   int
	rejected  uint64
	sequence  uint64
}

type requestKey struct{}

type relay struct {
	handler *Handler
	index   int
	id      string

	mu      sync.Mutex
	eof     bool
	failure string
}

// New constructs a proxy with fixed capture bounds and a bounded readiness probe.
func New(upstream *url.URL) (*Handler, error) {
	return newHandler(upstream, limits{MaxBodyBytes, MaxTotalBodyBytes, MaxEntries})
}

func newHandler(upstream *url.URL, bounds limits) (*Handler, error) {
	if upstream == nil || upstream.Host == "" || upstream.User != nil ||
		(upstream.Scheme != "http" && upstream.Scheme != "https") {
		return nil, errors.New("invalid upstream URL")
	}
	var seed [16]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return nil, errors.New("request ID initialization failed")
	}
	target := *upstream
	health := target
	health.Path, health.RawPath, health.RawQuery, health.Fragment = "/__aimock/health", "", "", ""
	health.ForceQuery = false
	h := &Handler{
		limits: bounds, idPrefix: "orka-eval-" + hex.EncodeToString(seed[:]) + "-",
		healthURL: health.String(), captureReads: make(chan struct{}, 1),
		healthClient: &http.Client{
			Timeout:       healthTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	h.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&target)
			pr.Out.Host = pr.In.Host
			// Preserve the query verbatim rather than sanitizing it as a side effect of Rewrite.
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			if state, ok := pr.In.Context().Value(requestKey{}).(*relay); ok {
				// Set after hop-by-hop header removal so the minted ID always reaches aimock.
				pr.Out.Header.Set("X-Request-Id", state.id)
			}
		},
		ErrorLog: log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
			if state, ok := r.Context().Value(requestKey{}).(*relay); ok {
				state.fail("upstream request failed")
			}
			http.Error(w, "upstream request failed", http.StatusBadGateway)
		},
		ModifyResponse: func(response *http.Response) error {
			if state, ok := response.Request.Context().Value(requestKey{}).(*relay); ok &&
				response.StatusCode != http.StatusSwitchingProtocols {
				response.Body = &responseBody{ReadCloser: response.Body, state: state}
			}
			return nil
		},
	}
	return h, nil
}

// ServeHTTP captures all POSTs except the aimock and evaluation control namespaces.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case capturePath, healthPath:
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == healthPath {
			h.health(w, r)
			return
		}
		h.writeCapture(w, r)
		return
	}
	if r.Method != http.MethodPost || inNamespace(r.URL.Path, "/__aimock") ||
		inNamespace(r.URL.Path, "/__orka_eval") {
		h.proxy.ServeHTTP(w, r)
		return
	}

	state, status, message := h.capture(r)
	if state == nil {
		http.Error(w, message, status)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), requestKey{}, state))
	writer := &responseWriter{ResponseWriter: w, state: state}
	returned := false
	defer func() { state.finish(r.Context(), returned) }()
	h.proxy.ServeHTTP(writer, r)
	// Flush server-side buffering before declaring a successfully relayed response.
	_ = writer.FlushError()
	returned = true
}

// Only one export may allocate a snapshot or encoding buffer at a time. Waiting
// readers retain neither, and cancellation does not have to wait for that export.
func (h *Handler) writeCapture(w http.ResponseWriter, r *http.Request) {
	select {
	case h.captureReads <- struct{}{}:
		defer func() { <-h.captureReads }()
	case <-r.Context().Done():
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = writeSnapshot(w, h.snapshot())
}

// Encode one bounded entry at a time rather than buffering the entire capture.
// The snapshot copies metadata only: immutable body strings are shared with the
// store. Insignificant newlines from Encode preserve the existing JSON schema.
func writeSnapshot(w io.Writer, snapshot Capture) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if _, err := io.WriteString(w, `{"instanceId":`); err != nil {
		return err
	}
	if err := encoder.Encode(snapshot.InstanceID); err != nil {
		return err
	}
	if _, err := io.WriteString(w, `,"entries":[`); err != nil {
		return err
	}
	for i, entry := range snapshot.Entries {
		if i != 0 {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		if err := encoder.Encode(entry); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, `],"rejected":`+strconv.FormatUint(snapshot.Rejected, 10)+"}\n")
	return err
}

func inNamespace(path, namespace string) bool {
	return path == namespace || strings.HasPrefix(path, namespace+"/")
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	probe, err := http.NewRequestWithContext(r.Context(), http.MethodGet, h.healthURL, nil)
	if err != nil {
		http.Error(w, "upstream not ready", http.StatusServiceUnavailable)
		return
	}
	response, err := h.healthClient.Do(probe)
	if err != nil {
		http.Error(w, "upstream not ready", http.StatusServiceUnavailable)
		return
	}
	_ = response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		http.Error(w, "upstream not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) capture(r *http.Request) (*relay, int, string) {
	h.mu.Lock()
	available := h.limits.total - h.bodyBytes - h.reserved
	var status int
	var message string
	switch {
	case r.ContentLength > h.limits.body:
		status, message = http.StatusRequestEntityTooLarge, "capture body limit exceeded"
	case len(h.entries)+h.pending >= h.limits.entries:
		status, message = http.StatusServiceUnavailable, "capture entry limit exceeded"
	case r.ContentLength > available:
		status, message = http.StatusServiceUnavailable, "capture total body limit exceeded"
	}
	if status != 0 {
		h.rejected++
		h.mu.Unlock()
		return nil, status, message
	}
	allowance := min(h.limits.body, available)
	if r.ContentLength >= 0 {
		allowance = min(allowance, r.ContentLength)
	}
	// Reserve both a slot and bytes before reading, including unknown-length bodies.
	h.pending++
	h.reserved += allowance
	h.mu.Unlock()

	body, err := io.ReadAll(io.LimitReader(r.Body, allowance+1))
	_ = r.Body.Close()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pending--
	h.reserved -= allowance
	switch {
	case err != nil || r.Context().Err() != nil:
		status, message = http.StatusBadRequest, "capture body read failed"
	case int64(len(body)) > allowance:
		switch allowance {
		case h.limits.body:
			status, message = http.StatusRequestEntityTooLarge, "capture body limit exceeded"
		case available:
			status, message = http.StatusServiceUnavailable, "capture total body limit exceeded"
		default:
			status, message = http.StatusBadRequest, "capture body length mismatch"
		}
	}
	if status == 0 && !utf8.Valid(body) {
		// JSON string fields cannot preserve invalid UTF-8; fail closed before forwarding.
		status, message = http.StatusBadRequest, "capture body encoding invalid"
	}
	if status != 0 {
		h.rejected++
		return nil, status, message
	}
	h.sequence++
	state := &relay{handler: h, index: len(h.entries), id: h.idPrefix + strconv.FormatUint(h.sequence, 10)}
	entry := Entry{RequestID: state.id, Path: strings.Clone(r.URL.Path), Method: r.Method, Body: string(body)}
	h.entries = append(h.entries, entry)
	h.bodyBytes += int64(len(body))
	r.Body = io.NopCloser(strings.NewReader(entry.Body))
	return state, 0, ""
}

func (h *Handler) snapshot() Capture {
	h.mu.Lock()
	defer h.mu.Unlock()
	entries := make([]Entry, len(h.entries))
	copy(entries, h.entries)
	return Capture{InstanceID: h.idPrefix, Entries: entries, Rejected: h.rejected}
}

func (s *relay) fail(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure == "" {
		s.failure = message
	}
}

func (s *relay) finish(ctx context.Context, returned bool) {
	s.mu.Lock()
	failure := s.failure
	success := returned && s.eof && failure == ""
	s.mu.Unlock()
	if ctx.Err() != nil {
		failure, success = "request canceled", false
	} else if !success && failure == "" {
		failure = "upstream response incomplete"
	}
	s.handler.mu.Lock()
	defer s.handler.mu.Unlock()
	s.handler.entries[s.index].Complete = success
	s.handler.entries[s.index].Error = failure
}

type responseBody struct {
	io.ReadCloser
	state *relay
}

func (b *responseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		b.state.mu.Lock()
		b.state.eof = true
		b.state.mu.Unlock()
	} else if err != nil {
		b.state.fail("upstream response failed")
	}
	return n, err
}

type responseWriter struct {
	http.ResponseWriter
	state *relay
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseWriter) WriteHeader(status int) {
	if status >= http.StatusOK {
		w.state.handler.mu.Lock()
		w.state.handler.entries[w.state.index].Status = status
		w.state.handler.mu.Unlock()
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if err != nil || n != len(p) {
		w.state.fail("downstream response failed")
	}
	return n, err
}

func (w *responseWriter) FlushError() error {
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if err != nil {
		w.state.fail("downstream response failed")
	}
	return err
}

func (w *responseWriter) Flush() { _ = w.FlushError() }
