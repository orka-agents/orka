// Copyright (c) 2026. MIT License - see LICENSE file for details.

// Package fakeentra is a Microsoft Entra stand-in for the Agent identity and
// keyless Provider E2Es. It plays the agent identity blueprint flow the
// controller uses (a blueprint exchange token for one agent identity, then
// that identity's own token for a resource) and serves a resource that accepts
// only the tokens it issued. It also plays a workload identity's
// client-credentials flow and serves an Azure OpenAI chat completions endpoint
// that accepts only that identity's tokens. Everything is synthetic; no real
// tenant is involved.
package fakeentra

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// ExchangeScope is the scope the blueprint requests an exchange token for.
	ExchangeScope = "api://AzureADTokenExchange/.default"
	// ProbePath is the resource endpoint that checks an Agent identity token.
	ProbePath = "/resource/probe"
	// ProbeOK starts the probe's answer to a token it issued for its resource.
	ProbeOK = "AGENT_IDENTITY_PROBE_OK"
	// ModelReplyPrefix starts every answer of the fake Azure OpenAI endpoint.
	ModelReplyPrefix = "KEYLESS_PROVIDER_OK"
	// DisabledErrorCode is the AADSTS code Entra returns for a disabled
	// application, which a disabled agent identity is.
	DisabledErrorCode = 7000112

	jwtBearer = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
)

// Config fixes the tenant the fake serves and what it trusts.
type Config struct {
	TenantID          string
	BlueprintClientID string
	// Assertion is the controller's federated credential the blueprint must
	// present.
	Assertion string
	// Identities are the agent identities that are children of the blueprint.
	Identities []string
	// Resource is the resource the probe accepts tokens for.
	Resource string
	// TokenLifetime is the expires_in of every issued token.
	TokenLifetime time.Duration
	// Workloads are workload identity client IDs whose federated credential
	// is Assertion.
	Workloads []string
	// ModelResource is the resource the Azure OpenAI endpoint accepts tokens
	// for, such as https://ai.azure.com.
	ModelResource string
}

// Request records one token request without its credentials.
type Request struct {
	Step     string `json:"step"`
	ClientID string `json:"clientID"`
	Identity string `json:"identity"`
	Scope    string `json:"scope"`
	// Error is the refusal code, empty when a token was issued.
	Error     string `json:"error,omitempty"`
	ErrorCode int    `json:"errorCode,omitempty"`
}

// Probe records one call to the resource.
type Probe struct {
	Authorized bool   `json:"authorized"`
	Identity   string `json:"identity,omitempty"`
	Resource   string `json:"resource,omitempty"`
}

// ModelCall records one call to the Azure OpenAI endpoint.
type ModelCall struct {
	Path       string `json:"path"`
	Authorized bool   `json:"authorized"`
	Identity   string `json:"identity,omitempty"`
	// APIKey reports that the call carried an api-key header.
	APIKey bool `json:"apiKey,omitempty"`
	Stream bool `json:"stream,omitempty"`
}

// State is what the fake has seen. Issued holds every token value it handed
// out, so a test can prove none of them reached a model.
type State struct {
	Requests []Request   `json:"requests"`
	Probes   []Probe     `json:"probes"`
	Models   []ModelCall `json:"models"`
	Issued   []string    `json:"issued"`
}

type appGrant struct{ identity, resource string }

// Server is the fake. Create it with New.
type Server struct {
	cfg      Config
	mu       sync.Mutex
	exchange map[string]string
	app      map[string]appGrant
	disabled map[string]bool
	state    State
}

// New returns a fake for cfg.
func New(cfg Config) *Server {
	cfg.TenantID = strings.ToLower(cfg.TenantID)
	cfg.BlueprintClientID = strings.ToLower(cfg.BlueprintClientID)
	for i, id := range cfg.Identities {
		cfg.Identities[i] = strings.ToLower(id)
	}
	for i, id := range cfg.Workloads {
		cfg.Workloads[i] = strings.ToLower(id)
	}
	if cfg.TokenLifetime <= 0 {
		cfg.TokenLifetime = time.Hour
	}
	return &Server{cfg: cfg, exchange: map[string]string{}, app: map[string]appGrant{}, disabled: map[string]bool{}}
}

// Handler serves the token endpoint, the probe, and the fixture controls.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("POST /{tenant}/oauth2/v2.0/token", s.token)
	mux.HandleFunc(ProbePath, s.probe)
	mux.HandleFunc("POST /openai/deployments/{deployment}/chat/completions", s.chatCompletions)
	mux.HandleFunc("POST /openai/responses", s.responses)
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
	case slices.Contains(s.cfg.Workloads, clientID):
		record.Step = "workload"
		record.Identity = clientID
		switch {
		case assertion != s.cfg.Assertion:
			refuse(http.StatusUnauthorized, "invalid_client", 700213)
		case s.cfg.ModelResource == "" || record.Scope != s.cfg.ModelResource+"/.default":
			refuse(http.StatusBadRequest, "invalid_scope", 70011)
		default:
			s.issue(w, record, func(token string) { s.app[token] = appGrant{identity: clientID, resource: s.cfg.ModelResource} })
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

// probe answers HEAD for the Tool health check and GET for a call. A GET is
// served only with an app token this fake issued for its resource.
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

// modelCall checks a call to the Azure OpenAI endpoint and records it. It is
// authorized only with a workload token this fake issued for ModelResource;
// an agent identity's token for the same resource is refused.
func (s *Server) modelCall(r *http.Request, stream bool) ModelCall {
	token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	defer s.mu.Unlock()
	grant, ok := s.app[token]
	call := ModelCall{
		Path: r.URL.Path,
		Authorized: ok && bearer && s.cfg.ModelResource != "" && grant.resource == s.cfg.ModelResource &&
			slices.Contains(s.cfg.Workloads, grant.identity),
		Identity: grant.identity,
		APIKey:   r.Header.Get("Api-Key") != "",
		Stream:   stream,
	}
	s.state.Models = append(s.state.Models, call)
	return call
}

// modelError is an Azure OpenAI error body.
type modelError struct {
	Error struct {
		Code    string `json:"code"`
		Type    string `json:"type,omitempty"`
		Message string `json:"message,omitempty"`
	} `json:"error"`
}

func writeModelError(w http.ResponseWriter, status int, code, kind, message string) {
	var body modelError
	body.Error.Code, body.Error.Type, body.Error.Message = code, kind, message
	writeJSON(w, status, body)
}

type chatMessage struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type chatChoice struct {
	Index        int          `json:"index"`
	Message      *chatMessage `json:"message,omitempty"`
	Delta        *chatMessage `json:"delta,omitempty"`
	FinishReason string       `json:"finish_reason,omitempty"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatCompletion struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage,omitempty"`
}

// chatCompletions answers with ModelReplyPrefix and the last user message,
// as one JSON response or as a stream.
func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Stream   bool `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&request); err != nil {
		writeModelError(w, http.StatusBadRequest, "invalid_request", "", "")
		return
	}
	if call := s.modelCall(r, request.Stream); !call.Authorized || call.APIKey {
		writeModelError(w, http.StatusUnauthorized, "PermissionDenied", "", "")
		return
	}
	reply := ModelReplyPrefix
	for _, message := range request.Messages {
		if text, ok := message.Content.(string); ok && message.Role == "user" {
			reply = ModelReplyPrefix + " " + text
		}
	}
	usage := &chatUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}
	completion := func(object string, choices []chatChoice, usage *chatUsage) chatCompletion {
		return chatCompletion{ID: "chatcmpl-fake", Object: object, Created: 1, Model: "fake-model",
			Choices: choices, Usage: usage}
	}
	answer := &chatMessage{Role: "assistant", Content: reply}
	if !request.Stream {
		writeJSON(w, http.StatusOK, completion("chat.completion",
			[]chatChoice{{Message: answer, FinishReason: "stop"}}, usage))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, chunk := range []chatCompletion{
		completion("chat.completion.chunk", []chatChoice{{Delta: answer}}, nil),
		completion("chat.completion.chunk", []chatChoice{{Delta: &chatMessage{}, FinishReason: "stop"}}, nil),
		completion("chat.completion.chunk", []chatChoice{}, usage),
	} {
		encoded, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

// responses answers as an Azure deployment without the Responses API does,
// so clients fall back to chat completions.
func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	if call := s.modelCall(r, false); !call.Authorized || call.APIKey {
		writeModelError(w, http.StatusUnauthorized, "PermissionDenied", "", "")
		return
	}
	writeModelError(w, http.StatusNotFound, "invalid_url", "invalid_request_error", "Not Found")
}

func (s *Server) snapshot(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	state := State{
		Requests: slices.Clone(s.state.Requests),
		Probes:   slices.Clone(s.state.Probes),
		Models:   slices.Clone(s.state.Models),
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
