package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/harness"
	"github.com/sozercan/orka/internal/harness/harnesstest"
)

func TestCheckReadinessPassesForFakeHarness(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{RuntimeName: "fake-runtime", AuthToken: "x"})
	defer server.Close()

	result := CheckReadiness(context.Background(), Target{BaseURL: server.URL(), BearerToken: "x"})
	if !result.Passed {
		t.Fatalf("Passed = false, failures=%v", result.Failures)
	}
	if result.ObservedCapabilities == nil || result.ObservedCapabilities.RuntimeName != "fake-runtime" {
		t.Fatalf("ObservedCapabilities = %#v, want fake-runtime", result.ObservedCapabilities)
	}
}

func TestCheckReadinessPassesForAgentKitOrkaFixture(t *testing.T) {
	server := newAgentKitOrkaFixture(t, "fibey-agentkit", "x")
	defer server.Close()

	result := CheckReadiness(context.Background(), Target{BaseURL: server.URL, BearerToken: "x"})
	if !result.Passed {
		t.Fatalf("Passed = false, failures=%v", result.Failures)
	}
	if result.ObservedCapabilities == nil {
		t.Fatal("ObservedCapabilities = nil")
	}
	if result.ObservedCapabilities.ProtocolVersion != harness.ProtocolVersion {
		t.Fatalf("ProtocolVersion = %q, want %q", result.ObservedCapabilities.ProtocolVersion, harness.ProtocolVersion)
	}
	if result.ObservedCapabilities.RuntimeName != "fibey-agentkit" {
		t.Fatalf("RuntimeName = %q, want fibey-agentkit", result.ObservedCapabilities.RuntimeName)
	}
}

func TestCheckReadinessFailsUnsupportedProtocolVersion(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{ProtocolVersion: "orka.harness.v0", AuthToken: "x"})
	defer server.Close()

	result := CheckReadiness(context.Background(), Target{BaseURL: server.URL(), BearerToken: "x"})
	if result.Passed {
		t.Fatalf("Passed = true, want false")
	}
	if !strings.Contains(result.Message, "unsupported protocol version") {
		t.Fatalf("Message = %q, want unsupported protocol version", result.Message)
	}
}

func TestCheckFailsWhenTerminalFrameOmitted(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{Behavior: harnesstest.BehaviorMissingTerminal})
	defer server.Close()

	request := harness.StartTurnRequest{
		Version:           harness.ProtocolVersion,
		Namespace:         "default",
		TaskName:          "task-a",
		SessionName:       "session-a",
		RuntimeSessionID:  "runtime-a",
		TurnID:            "turn-a",
		CorrelationID:     "corr-a",
		Deadline:          defaultStartTurnRequest("x").Deadline,
		AuthIdentity:      harness.AuthIdentity{Subject: "user:test"},
		Input:             harness.TurnInput{Prompt: "hello"},
		ToolExecutionMode: harness.ToolExecutionModeObserved,
	}
	result := Check(context.Background(), Target{BaseURL: server.URL(), ProbeTurn: true, StartTurnRequest: &request})
	if result.Passed {
		t.Fatalf("Passed = true, want false")
	}
	if !strings.Contains(result.Message, "terminal frame count = 0") {
		t.Fatalf("Message = %q, want terminal frame omission", result.Message)
	}
}

func TestCheckFailsWhenStartTurnResponseOmitsEventStreamPath(t *testing.T) {
	server := newAgentKitOrkaFixture(t, "fibey-agentkit", "x")
	server.omitEventStreamPath = true
	defer server.Close()

	result := CheckReadiness(context.Background(), Target{BaseURL: server.URL, BearerToken: "x"})
	if result.Passed {
		t.Fatal("Passed = true, want false")
	}
	if !strings.Contains(result.Message, "eventStreamPath") {
		t.Fatalf("Message = %q, want eventStreamPath failure", result.Message)
	}
}

func TestCheckFailsWhenFrameTypeUnknown(t *testing.T) {
	server := newAgentKitOrkaFixture(t, "fibey-agentkit", "x")
	server.frameType = harness.FrameType("AgentKitProgress")
	defer server.Close()

	result := CheckReadiness(context.Background(), Target{BaseURL: server.URL, BearerToken: "x"})
	if result.Passed {
		t.Fatal("Passed = true, want false")
	}
	if !strings.Contains(result.Message, "unknown") {
		t.Fatalf("Message = %q, want unknown frame failure", result.Message)
	}
}

type agentKitOrkaFixture struct {
	*httptest.Server
	runtimeName         string
	authValue           string
	omitEventStreamPath bool
	frameType           harness.FrameType
	mu                  sync.Mutex
	turns               map[harness.HarnessTurnID]harness.StartTurnRequest
}

func newAgentKitOrkaFixture(t *testing.T, runtimeName, authValue string) *agentKitOrkaFixture {
	t.Helper()
	fixture := &agentKitOrkaFixture{
		runtimeName: runtimeName,
		authValue:   authValue,
		frameType:   harness.FrameRuntimeOutput,
		turns:       map[harness.HarnessTurnID]harness.StartTurnRequest{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc(harness.HealthPath, fixture.health)
	mux.HandleFunc(harness.CapabilitiesPath, fixture.capabilities)
	mux.HandleFunc(harness.TurnsPath, fixture.startTurn)
	mux.HandleFunc(harness.TurnsPath+"/", fixture.turnResource)
	fixture.Server = httptest.NewServer(mux)
	return fixture
}

func (f *agentKitOrkaFixture) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		harness.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	harness.WriteJSON(w, http.StatusOK, harness.HealthResponse{
		Version:   harness.ProtocolVersion,
		Status:    harness.HealthStatusOK,
		Ready:     true,
		CheckedAt: time.Now().UTC(),
	})
}

func (f *agentKitOrkaFixture) capabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		harness.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	harness.WriteJSON(w, http.StatusOK, harness.CapabilitiesResponse{
		Version:                 harness.ProtocolVersion,
		ProtocolVersion:         harness.ProtocolVersion,
		Transport:               harness.HTTPTransport,
		RuntimeName:             f.runtimeName,
		RuntimeVersion:          "agentkit-fixture",
		ProviderKind:            harness.ProviderKindKubernetesService,
		ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeObserved},
		SupportsCancel:          true,
		SupportsRuntimeSessions: true,
		MaxConcurrentTurns:      1,
	})
}

func (f *agentKitOrkaFixture) startTurn(w http.ResponseWriter, r *http.Request) {
	if !f.authorized(w, r) {
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
	if got := strings.TrimSpace(request.Metadata["runtime"]); got != f.runtimeName {
		harness.WriteError(w, http.StatusBadRequest, fmt.Sprintf("runtime %q not supported", got))
		return
	}
	eventStreamPath, err := harness.EventStreamPath(request.TurnID)
	if err != nil {
		harness.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	f.mu.Lock()
	f.turns[request.TurnID] = request
	f.mu.Unlock()
	response := harness.StartTurnResponse{
		Version:          harness.ProtocolVersion,
		Accepted:         true,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		EventStreamPath:  eventStreamPath,
	}
	if f.omitEventStreamPath {
		response.EventStreamPath = ""
	}
	harness.WriteJSON(w, http.StatusAccepted, response)
}

func (f *agentKitOrkaFixture) turnResource(w http.ResponseWriter, r *http.Request) {
	if !f.authorized(w, r) {
		return
	}
	turnID, resource, err := harness.ParseTurnResourcePath(r.URL.EscapedPath())
	if err != nil {
		harness.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	f.mu.Lock()
	request, ok := f.turns[turnID]
	f.mu.Unlock()
	if !ok {
		harness.WriteError(w, http.StatusNotFound, "turn not found")
		return
	}
	switch resource {
	case "events":
		f.events(w, request)
	case "cancel":
		harness.WriteJSON(w, http.StatusAccepted, harness.CancelTurnResponse{
			Version:          harness.ProtocolVersion,
			Accepted:         true,
			RuntimeSessionID: request.RuntimeSessionID,
			TurnID:           request.TurnID,
			CorrelationID:    request.CorrelationID,
		})
	default:
		harness.WriteError(w, http.StatusNotFound, "not found")
	}
}

func (f *agentKitOrkaFixture) events(w http.ResponseWriter, request harness.StartTurnRequest) {
	w.Header().Set("Content-Type", "text/event-stream")
	_ = harness.WriteSSEFrame(w, agentKitFrame(request, 1, harness.FrameTurnStarted, "turn started", nil))
	output := agentKitFrame(request, 2, f.frameType, "runtime output", nil)
	output.ContentText = "AgentKit Orka fixture output"
	output.Content = json.RawMessage(`{"message":"AgentKit Orka fixture output"}`)
	_ = harness.WriteSSEFrame(w, output)
	completed := &harness.TurnCompleted{Result: "ok", FinalEventSeq: 3}
	_ = harness.WriteSSEFrame(w, agentKitFrame(request, 3, harness.FrameTurnCompleted, "turn completed", completed))
	_ = harness.WriteSSEDone(w)
}

func agentKitFrame(
	request harness.StartTurnRequest,
	seq int64,
	frameType harness.FrameType,
	summary string,
	completed *harness.TurnCompleted,
) harness.HarnessEventFrame {
	return harness.HarnessEventFrame{
		Version:          harness.ProtocolVersion,
		Type:             frameType,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		Seq:              seq,
		CreatedAt:        time.Now().UTC(),
		Severity:         events.ExecutionEventSeverityInfo,
		Summary:          summary,
		Completed:        completed,
	}
}

func (f *agentKitOrkaFixture) authorized(w http.ResponseWriter, r *http.Request) bool {
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if got != f.authValue {
		harness.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}
