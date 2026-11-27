/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package ahphost

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const doorTimeout = 10 * time.Second

func (h *Host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !originAllowed(r.Header.Get("Origin")) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	token, ok := presentedToken(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "an AHP connection token is required", http.StatusUnauthorized)
		return
	}
	verifyCtx, cancel := context.WithTimeout(context.Background(), doorTimeout)
	principal, err := h.cfg.Tokens.Verify(verifyCtx, token)
	cancel()
	if err != nil {
		if !errors.Is(err, ErrInvalidToken) {
			h.cfg.Logger.Error(err, "AHP token verification failed")
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "invalid AHP connection token", http.StatusUnauthorized)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	c := newConn(h, principal, ws)
	c.run(context.Background())
}

func presentedToken(r *http.Request) (string, bool) {
	if header := strings.TrimSpace(r.Header.Get("Authorization")); header != "" {
		scheme, token, ok := strings.Cut(header, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
			return "", false
		}
		return strings.TrimSpace(token), true
	}
	values := r.URL.Query()["tkn"]
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
		return "", false
	}
	return values[0], true
}

func originAllowed(origin string) bool {
	origin = strings.TrimSpace(origin)
	if origin == "" || origin == "null" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		return false
	default:
		return true
	}
}
