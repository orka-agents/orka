// Copyright (c) 2026. MIT License - see LICENSE file for details.

package fakeentra

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	ExchangeScope     = "api://AzureADTokenExchange/.default"
	ProbePath         = "/resource/probe"
	ProbeOK           = "AGENT_IDENTITY_PROBE_OK"
	DisabledErrorCode = 7000112

	jwtBearer = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
)

type Config struct {
	TenantID          string
	BlueprintClientID string
	Assertion         string
	Identities        []string
	Resource          string
	TokenLifetime     time.Duration
}

type Request struct {
	Step      string `json:"step"`
	ClientID  string `json:"clientID"`
	Identity  string `json:"identity"`
	Scope     string `json:"scope"`
	Error     string `json:"error,omitempty"`
	ErrorCode int    `json:"errorCode,omitempty"`
}

type Probe struct {
	Authorized bool   `json:"authorized"`
	Identity   string `json:"identity,omitempty"`
	Resource   string `json:"resource,omitempty"`
}

type State struct {
	Requests []Request `json:"requests"`
	Probes   []Probe   `json:"probes"`
	Issued   []string  `json:"issued"`
}

type appGrant struct{ identity, resource string }

type Server struct {
	cfg      Config
	mu       sync.Mutex
	exchange map[string]string
	app      map[string]appGrant
	disabled map[string]bool
	state    State
}

func New(cfg Config) *Server {
	cfg.TenantID = strings.ToLower(cfg.TenantID)
	cfg.BlueprintClientID = strings.ToLower(cfg.BlueprintClientID)
	for i, id := range cfg.Identities {
		cfg.Identities[i] = strings.ToLower(id)
	}
	if cfg.TokenLifetime <= 0 {
		cfg.TokenLifetime = time.Hour
	}
	return &Server{cfg: cfg, exchange: map[string]string{}, app: map[string]appGrant{}, disabled: map[string]bool{}}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("POST /{tenant}/oauth2/v2.0/token", s.token)
	mux.HandleFunc(ProbePath, s.probe)
	mux.HandleFunc("GET /fixture/state", s.snapshot)
	mux.HandleFunc("POST /fixture/identities/{id}/disable", s.disable)
	return mux
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", 900144)
		return
	}
	form := r.PostForm
	clientID := strings.ToLower(form.Get("client_id"))
	record := Request{ClientID: clientID, Scope: form.Get("scope")}
	refuse := func(status int, code string, number int) {
		record.Error, record.ErrorCode = code, number
		s.record(record)
		writeError(w, status, code, number)
	}
	if strings.ToLower(r.PathValue("tenant")) != s.cfg.TenantID {
		record.Step = "unknown-tenant"
		refuse(http.StatusBadRequest, "invalid_request", 90002)
		return
	}
	if form.Get("grant_type") != "client_credentials" || form.Get("client_assertion_type") != jwtBearer {
		record.Step = "unsupported"
		refuse(http.StatusBadRequest, "unsupported_grant_type", 70003)
		return
	}
	assertion := form.Get("client_assertion")
	switch {
	case clientID == s.cfg.BlueprintClientID:
		record.Step = "exchange"
		record.Identity = strings.ToLower(form.Get("fmi_path"))
		switch {
		case assertion != s.cfg.Assertion:
			refuse(http.StatusUnauthorized, "invalid_client", 700213)
		case record.Scope != ExchangeScope:
			refuse(http.StatusBadRequest, "invalid_scope", 70011)
		case !slices.Contains(s.cfg.Identities, record.Identity):
			refuse(http.StatusBadRequest, "unauthorized_client", 700016)
		default:
			s.issue(w, record, func(token string) { s.exchange[token] = record.Identity })
		}
	case slices.Contains(s.cfg.Identities, clientID):
		record.Step = "app"
		record.Identity = clientID
		resource, scoped := strings.CutSuffix(record.Scope, "/.default")
		s.mu.Lock()
		owner, disabled := s.exchange[assertion], s.disabled[clientID]
		s.mu.Unlock()
		switch {
		case owner != clientID:
			refuse(http.StatusUnauthorized, "invalid_client", 700213)
		case disabled:
			refuse(http.StatusBadRequest, "unauthorized_client", DisabledErrorCode)
		case !scoped || resource == "":
			refuse(http.StatusBadRequest, "invalid_scope", 70011)
		default:
			s.issue(w, record, func(token string) { s.app[token] = appGrant{identity: clientID, resource: resource} })
		}
	default:
		record.Step = "unknown-client"
		refuse(http.StatusBadRequest, "unauthorized_client", 700016)
	}
}

func (s *Server) issue(w http.ResponseWriter, record Request, keep func(string)) {
	token := "fx-entra-" + record.Step + "-" + randomHex()
	s.mu.Lock()
	keep(token)
	s.state.Issued = append(s.state.Issued, token)
	s.state.Requests = append(s.state.Requests, record)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"token_type": "Bearer", "access_token": token, "expires_in": int(s.cfg.TokenLifetime.Seconds()),
	})
}

func (s *Server) probe(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	grant, ok := s.app[token]
	ok = ok && bearer
	probe := Probe{Authorized: ok && grant.resource == s.cfg.Resource, Identity: grant.identity, Resource: grant.resource}
	s.state.Probes = append(s.state.Probes, probe)
	s.mu.Unlock()
	if !probe.Authorized {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"result": ProbeOK, "identity": grant.identity})
}

func (s *Server) snapshot(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	state := State{
		Requests: slices.Clone(s.state.Requests),
		Probes:   slices.Clone(s.state.Probes),
		Issued:   slices.Clone(s.state.Issued),
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) disable(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(r.PathValue("id"))
	if !slices.Contains(s.cfg.Identities, id) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	s.mu.Lock()
	s.disabled[id] = true
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"disabled": true})
}

func (s *Server) record(request Request) {
	s.mu.Lock()
	s.state.Requests = append(s.state.Requests, request)
	s.mu.Unlock()
}

func writeError(w http.ResponseWriter, status int, code string, number int) {
	writeJSON(w, status, map[string]any{"error": code, "error_codes": []int{number}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func randomHex() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
