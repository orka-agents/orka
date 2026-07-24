/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package referenceadapter provides a deterministic, provider-neutral
// orka.gateway.v1 implementation for conformance and end-to-end tests.
package referenceadapter

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/orka-agents/orka/internal/gateway/protocol"
)

// Server is an in-memory deterministic reference adapter.
type Server struct {
	authValue string

	mu            sync.Mutex
	deliveries    map[string]protocol.DeliveryRequest
	deliveryOrder []string
	responses     map[string]protocol.DeliveryResponse
	attempts      map[string]int
}

// New creates a reference adapter protected by one outbound bearer token.
func New(token string) *Server {
	return &Server{
		authValue:  strings.TrimSpace(token),
		deliveries: map[string]protocol.DeliveryRequest{},
		responses:  map[string]protocol.DeliveryResponse{},
		attempts:   map[string]int{},
	}
}

// Handler returns the HTTP adapter contract; callers choose the TLS serving layer.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.auth(s.handleHealth))
	mux.HandleFunc("GET /v1/capabilities", s.auth(s.handleCapabilities))
	mux.HandleFunc("POST /v1/deliveries", s.auth(s.handleDelivery))
	return mux
}

// Deliveries returns a snapshot of successfully recorded provider sends.
func (s *Server) Deliveries() []protocol.DeliveryRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]protocol.DeliveryRequest, 0, len(s.deliveryOrder))
	for _, id := range s.deliveryOrder {
		result = append(result, s.deliveries[id])
	}
	return result
}

// Attempts returns how many times one stable delivery ID reached the adapter.
func (s *Server) Attempts(deliveryID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[deliveryID]
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := protocol.BearerToken(r.Header.Get("Authorization"))
		if !protocol.ConstantTimeBearerEqual(got, s.authValue) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, protocol.HealthResponse{Status: "ok"})
}

func (s *Server) handleCapabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, protocol.CapabilitiesResponse{
		ProtocolVersion: protocol.Version,
		AdapterName:     "orka-reference-adapter",
		AdapterVersion:  "v1",
		Capabilities: protocol.Capabilities{
			InboundText: true, OutboundText: true, Threads: true, SenderIdentity: true,
			ExplicitSessions: true, IdempotentDelivery: true,
		},
	})
}

func (s *Server) handleDelivery(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, protocol.MaxHTTPBodyBytes+1))
	if err != nil || len(body) > protocol.MaxHTTPBodyBytes {
		writeJSON(w, http.StatusBadRequest, protocol.DeliveryResponse{Status: protocol.DeliveryStatusNonRetryableError, Message: "invalid request body"})
		return
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	var delivery protocol.DeliveryRequest
	if err := decoder.Decode(&delivery); err != nil || protocol.ValidateDeliveryRequest(&delivery) != nil {
		writeJSON(w, http.StatusBadRequest, protocol.DeliveryResponse{Status: protocol.DeliveryStatusNonRetryableError, Message: "invalid delivery"})
		return
	}

	s.mu.Lock()
	s.attempts[delivery.DeliveryID]++
	attempt := s.attempts[delivery.DeliveryID]
	if response, ok := s.responses[delivery.DeliveryID]; ok {
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, response)
		return
	}
	s.mu.Unlock()

	fixture := strings.TrimSpace(delivery.Metadata["fixture"])
	switch fixture {
	case "retryable":
		writeJSON(w, http.StatusOK, protocol.DeliveryResponse{Status: protocol.DeliveryStatusRetryableError, Message: "fixture requested retry"})
		return
	case "retryable-once":
		if attempt == 1 {
			writeJSON(w, http.StatusOK, protocol.DeliveryResponse{Status: protocol.DeliveryStatusRetryableError, Message: "fixture requested one retry"})
			return
		}
	case "permanent":
		writeJSON(w, http.StatusOK, protocol.DeliveryResponse{Status: protocol.DeliveryStatusNonRetryableError, Message: "fixture rejected delivery"})
		return
	case "delay":
		delay := 100 * time.Millisecond
		if raw := strings.TrimSpace(delivery.Metadata["fixtureDelayMs"]); raw != "" {
			if milliseconds, err := strconv.Atoi(raw); err == nil && milliseconds >= 0 && milliseconds <= 30_000 {
				delay = time.Duration(milliseconds) * time.Millisecond
			}
		}
		time.Sleep(delay)
	}

	response := protocol.DeliveryResponse{
		Status:            protocol.DeliveryStatusDelivered,
		ProviderMessageID: "reference:" + delivery.DeliveryID,
	}
	s.mu.Lock()
	if existing, ok := s.responses[delivery.DeliveryID]; ok {
		response = existing
	} else {
		s.deliveries[delivery.DeliveryID] = delivery
		s.deliveryOrder = append(s.deliveryOrder, delivery.DeliveryID)
		s.responses[delivery.DeliveryID] = response
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, response)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
