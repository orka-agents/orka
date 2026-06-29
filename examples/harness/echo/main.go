package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sozercan/orka/internal/harness"
)

type server struct {
	runtimeName string
	authToken   string
	mu          sync.Mutex
	turns       map[harness.HarnessTurnID]harness.StartTurnRequest
}

func main() {
	addr := strings.TrimSpace(os.Getenv("ORKA_EXAMPLE_HARNESS_ADDR"))
	if addr == "" {
		addr = ":8090"
	}
	runtimeName := strings.TrimSpace(os.Getenv("ORKA_EXAMPLE_HARNESS_RUNTIME_NAME"))
	if runtimeName == "" {
		runtimeName = "orka-example-echo-harness"
	}
	s := &server{
		runtimeName: runtimeName,
		authToken:   (strings.TrimSpace(os.Getenv("ORKA_EXAMPLE_HARNESS_BEARER_TOKEN"))),
		turns:       map[harness.HarnessTurnID]harness.StartTurnRequest{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc(harness.HealthPath, s.health)
	mux.HandleFunc(harness.CapabilitiesPath, s.capabilities)
	mux.HandleFunc(harness.TurnsPath, s.startTurn)
	mux.HandleFunc(harness.TurnsPath+"/", s.turn)
	log.Printf("example echo harness listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	harness.WriteJSON(w, http.StatusOK, harness.HealthResponse{
		Version:   harness.ProtocolVersion,
		Status:    harness.HealthStatusOK,
		Ready:     true,
		CheckedAt: time.Now().UTC(),
	})
}

func (s *server) capabilities(w http.ResponseWriter, r *http.Request) {
	harness.WriteJSON(w, http.StatusOK, harness.CapabilitiesResponse{
		Version:                 harness.ProtocolVersion,
		ProtocolVersion:         harness.ProtocolVersion,
		Transport:               harness.HTTPTransport,
		RuntimeName:             s.runtimeName,
		ProviderKind:            harness.ProviderKindKubernetesService,
		ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeObserved},
		SupportsCancel:          true,
		SupportsRuntimeSessions: true,
		MaxConcurrentTurns:      1,
	})
}

func (s *server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if strings.TrimSpace(s.authToken) == "" {
		return true
	}
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.authToken)) != 1 {
		harness.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

func (s *server) startTurn(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		harness.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request harness.StartTurnRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		harness.WriteError(w, http.StatusBadRequest, "invalid JSON request")
		return
	}
	if err := request.Validate(); err != nil {
		harness.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mu.Lock()
	if _, exists := s.turns[request.TurnID]; exists {
		s.mu.Unlock()
		harness.WriteError(w, http.StatusConflict, "turn already exists")
		return
	}
	s.turns[request.TurnID] = request
	s.mu.Unlock()
	harness.WriteJSON(w, http.StatusAccepted, harness.StartTurnResponse{
		Version:          harness.ProtocolVersion,
		Accepted:         true,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		EventStreamPath:  fmt.Sprintf("/v1/turns/%s/events", request.TurnID),
	})
}

func (s *server) turn(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	trimmed := strings.TrimPrefix(r.URL.Path, harness.TurnsPath+"/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) != 2 {
		harness.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	turnID := harness.HarnessTurnID(parts[0])
	s.mu.Lock()
	request, ok := s.turns[turnID]
	s.mu.Unlock()
	if !ok {
		harness.WriteError(w, http.StatusNotFound, "turn not found")
		return
	}
	switch parts[1] {
	case "events":
		s.streamEvents(w, request)
	case "cancel":
		s.cancelTurn(w, r, request)
	default:
		harness.WriteError(w, http.StatusNotFound, "not found")
	}
}

func (s *server) streamEvents(w http.ResponseWriter, request harness.StartTurnRequest) {
	w.Header().Set("Content-Type", "text/event-stream")
	_ = harness.WriteSSEFrame(w, frame(request, 1, harness.FrameTurnStarted, "turn started", nil))
	output := frame(request, 2, harness.FrameRuntimeOutput, "echo", nil)
	output.ContentText = "echo: " + request.Input.Prompt
	output.Content = json.RawMessage(fmt.Sprintf(`{"message":%q}`, output.ContentText))
	_ = harness.WriteSSEFrame(w, output)
	completed := &harness.TurnCompleted{Result: "ok", FinalEventSeq: 3}
	_ = harness.WriteSSEFrame(w, frame(request, 3, harness.FrameTurnCompleted, "turn completed", completed))
	_ = harness.WriteSSEDone(w)
}

func (s *server) cancelTurn(w http.ResponseWriter, r *http.Request, started harness.StartTurnRequest) {
	if r.Method != http.MethodPost {
		harness.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request harness.CancelTurnRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		harness.WriteError(w, http.StatusBadRequest, "invalid JSON request")
		return
	}
	if err := request.Validate(); err != nil {
		harness.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.RuntimeSessionID != started.RuntimeSessionID || request.TurnID != started.TurnID {
		harness.WriteError(w, http.StatusBadRequest, "cancel request does not match started turn")
		return
	}
	harness.WriteJSON(w, http.StatusAccepted, harness.CancelTurnResponse{
		Version:          harness.ProtocolVersion,
		Accepted:         true,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		Message:          "cancel accepted",
	})
}

func frame(
	request harness.StartTurnRequest,
	seq int64,
	typ harness.FrameType,
	summary string,
	completed *harness.TurnCompleted,
) harness.HarnessEventFrame {
	return harness.HarnessEventFrame{
		Version:          harness.ProtocolVersion,
		Type:             typ,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		Seq:              seq,
		CreatedAt:        time.Now().UTC(),
		Summary:          summary,
		Completed:        completed,
	}
}
