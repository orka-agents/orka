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
	server := newAgentKitOrkaFixture(t)
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

func TestCheckFailsWhenProbeTurnFails(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{Behavior: harnesstest.BehaviorFailure})
	defer server.Close()

	request := defaultStartTurnRequest("turn-fails")
	result := Check(context.Background(), Target{BaseURL: server.URL(), ProbeTurn: true, StartTurnRequest: &request})
	if result.Passed {
		t.Fatal("Passed = true, want false")
	}
	if !strings.Contains(result.Message, "completed terminal") {
		t.Fatalf("Message = %q, want completed terminal failure", result.Message)
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

func TestCheckFailsWhenDuplicateStartAccepted(t *testing.T) {
	server := newAgentKitOrkaFixture(t)
	server.allowDuplicateStart = true
	defer server.Close()

	result := CheckReadiness(context.Background(), Target{BaseURL: server.URL, BearerToken: "x"})
	if result.Passed {
		t.Fatal("Passed = true, want false")
	}
	if !strings.Contains(result.Message, "duplicate start turn was accepted") {
		t.Fatalf("Message = %q, want duplicate start rejection", result.Message)
	}
	server.mu.Lock()
	cancelCount := server.cancelCount
	server.mu.Unlock()
	if cancelCount == 0 {
		t.Fatal("cancel count = 0, want duplicate-accepted turn cleanup")
	}
}

func TestCheckFailsWhenProbeFrameLimitExceeded(t *testing.T) {
	server := newAgentKitOrkaFixture(t)
	server.outputFrames = maxProbeFrames + 1
	defer server.Close()

	result := CheckReadiness(context.Background(), Target{BaseURL: server.URL, BearerToken: "x"})
	if result.Passed {
		t.Fatal("Passed = true, want false")
	}
	if !strings.Contains(result.Message, "frame count exceeded") {
		t.Fatalf("Message = %q, want frame count limit", result.Message)
	}
}

func TestCheckFailsWhenProbeFrameByteLimitExceeded(t *testing.T) {
	server := newAgentKitOrkaFixture(t)
	server.outputFrames = maxProbeFrames / 2
	server.outputText = strings.Repeat("x", maxProbeFrameBytes/(maxProbeFrames/2)+1024)
	defer server.Close()

	result := CheckReadiness(context.Background(), Target{BaseURL: server.URL, BearerToken: "x"})
	if result.Passed {
		t.Fatal("Passed = true, want false")
	}
	if !strings.Contains(result.Message, "frame bytes exceeded") {
		t.Fatalf("Message = %q, want frame byte limit", result.Message)
	}
}

func TestCheckFailsWhenStartTurnResponseOmitsEventStreamPath(t *testing.T) {
	server := newAgentKitOrkaFixture(t)
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
	server := newAgentKitOrkaFixture(t)
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

func TestCheckPassesBrokeredReadProfile(t *testing.T) {
	server := newAgentKitOrkaFixture(t)
	server.brokeredClass = harness.BrokeredToolClassRead
	defer server.Close()

	result := Check(context.Background(), Target{BaseURL: server.URL, BearerToken: "x", RequireAuth: true, ProbeBrokeredRead: true})
	if !result.Passed {
		t.Fatalf("Passed = false, failures=%v", result.Failures)
	}
}

func TestCheckPassesBrokeredWriteProfile(t *testing.T) {
	server := newAgentKitOrkaFixture(t)
	server.brokeredClass = harness.BrokeredToolClassWrite
	defer server.Close()

	result := Check(context.Background(), Target{BaseURL: server.URL, BearerToken: "x", RequireAuth: true, ProbeBrokeredWrite: true})
	if !result.Passed {
		t.Fatalf("Passed = false, failures=%v", result.Failures)
	}
}

func TestCheckPassesBrokeredCoordinationProfile(t *testing.T) {
	server := newAgentKitOrkaFixture(t)
	server.brokeredClass = harness.BrokeredToolClassCoordination
	defer server.Close()

	result := Check(context.Background(), Target{BaseURL: server.URL, BearerToken: "x", RequireAuth: true, ProbeBrokeredCoordination: true})
	if !result.Passed {
		t.Fatalf("Passed = false, failures=%v", result.Failures)
	}
}

func TestCheckBrokeredReadFailsWhenClassNotAdvertised(t *testing.T) {
	server := newAgentKitOrkaFixture(t)
	defer server.Close()

	result := Check(context.Background(), Target{BaseURL: server.URL, BearerToken: "x", RequireAuth: true, ProbeBrokeredRead: true})
	if result.Passed {
		t.Fatal("Passed = true, want false")
	}
	if !strings.Contains(result.Message, "toolExecutionMode") {
		t.Fatalf("Message = %q, want brokered toolExecutionMode failure", result.Message)
	}
}

func TestCheckBrokeredReadFailsWhenRuntimeDoesNotWaitForContinue(t *testing.T) {
	server := newAgentKitOrkaFixture(t)
	server.brokeredClass = harness.BrokeredToolClassRead
	server.brokeredEagerResult = true
	defer server.Close()

	result := Check(context.Background(), Target{BaseURL: server.URL, BearerToken: "x", RequireAuth: true, ProbeBrokeredRead: true})
	if result.Passed {
		t.Fatal("Passed = true, want false")
	}
	if !strings.Contains(result.Message, "before continue") {
		t.Fatalf("Message = %q, want before continue failure", result.Message)
	}
}

func TestCheckBrokeredReadFailsWhenRuntimeRequestsWrongToolClass(t *testing.T) {
	server := newAgentKitOrkaFixture(t)
	server.brokeredClass = harness.BrokeredToolClassRead
	server.brokeredToolNameOverride = "conformance_write"
	defer server.Close()

	result := Check(context.Background(), Target{BaseURL: server.URL, BearerToken: "x", RequireAuth: true, ProbeBrokeredRead: true})
	if result.Passed {
		t.Fatal("Passed = true, want false")
	}
	if !strings.Contains(result.Message, "want \"conformance_read\"") {
		t.Fatalf("Message = %q, want expected tool failure", result.Message)
	}
}

type agentKitOrkaFixture struct {
	*httptest.Server
	runtimeName              string
	authValue                string
	omitEventStreamPath      bool
	frameType                harness.FrameType
	allowDuplicateStart      bool
	outputFrames             int
	outputText               string
	cancelCount              int
	brokeredClass            harness.BrokeredToolClass
	brokeredEagerResult      bool
	brokeredToolNameOverride string
	mu                       sync.Mutex
	turns                    map[harness.HarnessTurnID]harness.StartTurnRequest
	continued                map[harness.HarnessTurnID]harness.ToolCallResult
	continueCh               map[harness.HarnessTurnID]chan struct{}
}

func newAgentKitOrkaFixture(t *testing.T) *agentKitOrkaFixture {
	t.Helper()
	fixture := &agentKitOrkaFixture{
		runtimeName:  "fibey-agentkit",
		authValue:    "x",
		frameType:    harness.FrameRuntimeOutput,
		outputFrames: 1,
		turns:        map[harness.HarnessTurnID]harness.StartTurnRequest{},
		continued:    map[harness.HarnessTurnID]harness.ToolCallResult{},
		continueCh:   map[harness.HarnessTurnID]chan struct{}{},
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
	modes := []harness.ToolExecutionMode{harness.ToolExecutionModeObserved}
	classes := []harness.BrokeredToolClass(nil)
	if f.brokeredClass != "" {
		modes = append(modes, harness.ToolExecutionModeBrokered)
		classes = append(classes, f.brokeredClass)
	}
	harness.WriteJSON(w, http.StatusOK, harness.CapabilitiesResponse{
		Version:                 harness.ProtocolVersion,
		ProtocolVersion:         harness.ProtocolVersion,
		Transport:               harness.HTTPTransport,
		RuntimeName:             f.runtimeName,
		RuntimeVersion:          "agentkit-fixture",
		ProviderKind:            harness.ProviderKindKubernetesService,
		ToolExecutionModes:      modes,
		BrokeredToolClasses:     classes,
		SupportsCancel:          true,
		SupportsRuntimeSessions: true,
		SupportsContinuation:    f.brokeredClass != "",
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
	if _, exists := f.turns[request.TurnID]; exists && !f.allowDuplicateStart {
		f.mu.Unlock()
		harness.WriteError(w, http.StatusConflict, "turn already exists")
		return
	}
	f.turns[request.TurnID] = request
	f.continueCh[request.TurnID] = make(chan struct{})
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
	case "continue":
		f.continueTurn(w, r, request)
	case "cancel":
		f.mu.Lock()
		f.cancelCount++
		f.mu.Unlock()
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

func (f *agentKitOrkaFixture) continueTurn(w http.ResponseWriter, r *http.Request, request harness.StartTurnRequest) {
	var body harness.ContinueTurnRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		harness.WriteError(w, http.StatusBadRequest, "invalid JSON request")
		return
	}
	if err := body.Validate(); err != nil {
		harness.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.RuntimeSessionID != request.RuntimeSessionID || body.TurnID != request.TurnID || body.CorrelationID != request.CorrelationID {
		harness.WriteError(w, http.StatusBadRequest, "continue request does not match turn")
		return
	}
	f.mu.Lock()
	f.continued[request.TurnID] = body.ToolResults[0]
	ch := f.continueCh[request.TurnID]
	f.mu.Unlock()
	if ch != nil {
		close(ch)
	}
	harness.WriteJSON(w, http.StatusAccepted, harness.ContinueTurnResponse{
		Version:          harness.ProtocolVersion,
		Accepted:         true,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
	})
}

func (f *agentKitOrkaFixture) events(w http.ResponseWriter, request harness.StartTurnRequest) {
	w.Header().Set("Content-Type", "text/event-stream")
	_ = harness.WriteSSEFrame(w, agentKitFrame(request, 1, harness.FrameTurnStarted, "turn started", nil))
	if f.brokeredClass != "" && request.ToolExecutionMode == harness.ToolExecutionModeBrokered {
		f.brokeredEvents(w, request)
		return
	}
	outputFrames := f.outputFrames
	if outputFrames <= 0 {
		outputFrames = 1
	}
	for i := range outputFrames {
		seq := int64(i + 2)
		output := agentKitFrame(request, seq, f.frameType, "runtime output", nil)
		outputText := f.outputText
		if outputText == "" {
			outputText = "AgentKit Orka fixture output"
		}
		output.ContentText = outputText
		output.Content = json.RawMessage(fmt.Sprintf(`{"message":%q}`, outputText))
		_ = harness.WriteSSEFrame(w, output)
	}
	completedSeq := int64(outputFrames + 2)
	completed := &harness.TurnCompleted{Result: "ok", FinalEventSeq: completedSeq}
	_ = harness.WriteSSEFrame(w, agentKitFrame(request, completedSeq, harness.FrameTurnCompleted, "turn completed", completed))
	_ = harness.WriteSSEDone(w)
}

func (f *agentKitOrkaFixture) brokeredEvents(w http.ResponseWriter, request harness.StartTurnRequest) {
	tool := agentKitFrame(request, 2, harness.FrameToolCallRequested, "brokered tool requested", nil)
	tool.ToolName = "conformance_" + string(f.brokeredClass)
	if f.brokeredToolNameOverride != "" {
		tool.ToolName = f.brokeredToolNameOverride
	}
	tool.ToolCallID = "tool-call-1"
	tool.Content = json.RawMessage(`{"probe":true}`)
	_ = harness.WriteSSEFrame(w, tool)
	if f.brokeredEagerResult {
		result := agentKitFrame(request, 3, harness.FrameToolResultReceived, "tool result received", nil)
		result.ToolName = tool.ToolName
		result.ToolCallID = tool.ToolCallID
		result.Content = json.RawMessage(`{"success":true}`)
		_ = harness.WriteSSEFrame(w, result)
		completed := &harness.TurnCompleted{Result: "ok", FinalEventSeq: 4}
		_ = harness.WriteSSEFrame(w, agentKitFrame(request, 4, harness.FrameTurnCompleted, "turn completed", completed))
		_ = harness.WriteSSEDone(w)
		return
	}
	f.mu.Lock()
	ch := f.continueCh[request.TurnID]
	f.mu.Unlock()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		failed := agentKitFrame(request, 3, harness.FrameTurnFailed, "continue timeout", nil)
		failed.Failed = &harness.TurnFailed{Reason: "continue_timeout", Message: "continue not received"}
		_ = harness.WriteSSEFrame(w, failed)
		_ = harness.WriteSSEDone(w)
		return
	}
	f.mu.Lock()
	continued := f.continued[request.TurnID]
	f.mu.Unlock()
	result := agentKitFrame(request, 3, harness.FrameToolResultReceived, "tool result received", nil)
	result.ToolName = tool.ToolName
	result.ToolCallID = tool.ToolCallID
	result.Content = continued.Output
	result.Error = continued.Error
	_ = harness.WriteSSEFrame(w, result)
	completed := &harness.TurnCompleted{Result: "ok", FinalEventSeq: 4}
	_ = harness.WriteSSEFrame(w, agentKitFrame(request, 4, harness.FrameTurnCompleted, "turn completed", completed))
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
