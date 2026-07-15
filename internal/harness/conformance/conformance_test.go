package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/harness/harnesstest"
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
	server.duplicateStartMismatch = true
	defer server.Close()

	result := CheckReadiness(context.Background(), Target{BaseURL: server.URL, BearerToken: "x"})
	if result.Passed {
		t.Fatal("Passed = true, want false")
	}
	if !strings.Contains(result.Message, "accepted correlation id") {
		t.Fatalf("Message = %q, want duplicate start identity failure", result.Message)
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

func TestCheckBrokeredProbeUsesFoundryCompatibleObjectSchema(t *testing.T) {
	server := newAgentKitOrkaFixture(t)
	server.brokeredClass = harness.BrokeredToolClassRead
	defer server.Close()

	result := Check(context.Background(), Target{BaseURL: server.URL, BearerToken: "x", RequireAuth: true, ProbeBrokeredRead: true})
	if !result.Passed {
		t.Fatalf("Passed = false, failures=%v", result.Failures)
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	for _, request := range server.turns {
		if request.ToolExecutionMode != harness.ToolExecutionModeBrokered {
			continue
		}
		if len(request.Input.Tools) != 1 {
			t.Fatalf("brokered tool count = %d, want 1", len(request.Input.Tools))
		}
		var schema map[string]any
		if err := json.Unmarshal(request.Input.Tools[0].Parameters, &schema); err != nil {
			t.Fatalf("unmarshal tool schema: %v", err)
		}
		if schema["type"] != "object" {
			t.Fatalf("schema type = %#v, want object", schema["type"])
		}
		if _, ok := schema["properties"].(map[string]any); !ok {
			t.Fatalf("schema properties = %#v, want object", schema["properties"])
		}
		if schema["additionalProperties"] != true {
			t.Fatalf("schema additionalProperties = %#v, want true", schema["additionalProperties"])
		}
		return
	}
	t.Fatal("did not observe brokered start request")
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
	runtimeName                      string
	authValue                        string
	omitEventStreamPath              bool
	startResponseVersion             string
	frameType                        harness.FrameType
	allowDuplicateStart              bool
	duplicateStartMismatch           bool
	outputFrames                     int
	outputText                       string
	cancelCount                      int
	brokeredClass                    harness.BrokeredToolClass
	brokeredClasses                  []harness.BrokeredToolClass
	brokeredOnly                     bool
	brokeredEagerResult              bool
	brokeredToolNameOverride         string
	brokeredTerminalIdentityMismatch bool
	brokeredPostTerminalFrame        bool
	mu                               sync.Mutex
	turns                            map[harness.HarnessTurnID]harness.StartTurnRequest
	continued                        map[harness.HarnessTurnID]harness.ToolCallResult
	continueCh                       map[harness.HarnessTurnID]chan struct{}
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
	if f.brokeredOnly {
		modes = nil
	}
	classes := slices.Clone(f.brokeredClasses)
	if f.brokeredClass != "" && !slices.Contains(classes, f.brokeredClass) {
		classes = append(classes, f.brokeredClass)
	}
	if len(classes) > 0 {
		modes = append(modes, harness.ToolExecutionModeBrokered)
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
		SupportsContinuation:    len(classes) > 0,
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
	_, duplicate := f.turns[request.TurnID]
	if duplicate && !f.allowDuplicateStart {
		f.mu.Unlock()
		harness.WriteError(w, http.StatusConflict, "turn already exists")
		return
	}
	f.turns[request.TurnID] = request
	f.continueCh[request.TurnID] = make(chan struct{})
	f.mu.Unlock()
	version := harness.ProtocolVersion
	if f.startResponseVersion != "" {
		version = f.startResponseVersion
	}
	response := harness.StartTurnResponse{
		Version:          version,
		Accepted:         true,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		EventStreamPath:  eventStreamPath,
	}
	if duplicate && f.duplicateStartMismatch {
		response.CorrelationID = "duplicate-mismatch"
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
	if len(request.Input.Tools) > 0 && request.ToolExecutionMode == harness.ToolExecutionModeBrokered {
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
	class := request.Input.Tools[0].BrokeredClass
	tool.ToolName = "conformance_" + string(class)
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
	terminal := agentKitFrame(request, 4, harness.FrameTurnCompleted, "turn completed", completed)
	if f.brokeredTerminalIdentityMismatch {
		terminal.CorrelationID = "wrong-correlation"
	}
	_ = harness.WriteSSEFrame(w, terminal)
	if f.brokeredPostTerminalFrame {
		_ = harness.WriteSSEFrame(w, agentKitFrame(request, 5, harness.FrameRuntimeOutput, "late output", nil))
	}
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

func TestValidateProbeFramesRejectsNonIncreasingSequences(t *testing.T) {
	request := defaultStartTurnRequest("sequence")
	frames := []harness.HarnessEventFrame{
		sequenceProbeFrame(request, harness.FrameTurnStarted, 1),
		sequenceProbeFrame(request, harness.FrameRuntimeOutput, 1),
		sequenceProbeFrame(request, harness.FrameTurnCompleted, 2),
	}
	frames[2].Completed = &harness.TurnCompleted{Result: "ok", FinalEventSeq: 2}
	result := Result{Passed: true}
	validateProbeFrames(&result, request, frames)
	if !strings.Contains(strings.Join(result.Failures, "\n"), "not strictly greater") {
		t.Fatalf("failures = %#v", result.Failures)
	}
}

func TestStreamBrokeredContinuationFramesEnforcesFrameBudget(t *testing.T) {
	request := defaultStartTurnRequest("continuation-budget")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for seq := int64(1); seq <= maxProbeFrames+1; seq++ {
			if err := harness.WriteSSEFrame(w, sequenceProbeFrame(request, harness.FrameRuntimeOutput, seq)); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	client, err := harness.NewClient(server.URL, harness.WithControlTimeout(time.Second))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	frames, _, _, err := streamBrokeredContinuationFrames(
		ctx,
		client,
		request,
		sequenceProbeFrame(request, harness.FrameToolCallRequested, 1),
		0,
		0,
		0,
	)
	if err == nil || !strings.Contains(err.Error(), "frame count exceeded") {
		t.Fatalf("streamBrokeredContinuationFrames() error = %v, frames=%d", err, len(frames))
	}
}

func sequenceProbeFrame(
	request harness.StartTurnRequest,
	typ harness.FrameType,
	seq int64,
) harness.HarnessEventFrame {
	return harness.HarnessEventFrame{
		Version:          harness.ProtocolVersion,
		Type:             typ,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		Seq:              seq,
		CreatedAt:        time.Now().UTC(),
	}
}

func TestIsAuthRequiredErrorRequiresAuthenticationStatus(t *testing.T) {
	if !isAuthRequiredError(harness.ClientError{StatusCode: http.StatusUnauthorized, Message: "denied"}) {
		t.Fatal("isAuthRequiredError(401) = false")
	}
	if !isAuthRequiredError(harness.ClientError{StatusCode: http.StatusForbidden, Message: "denied"}) {
		t.Fatal("isAuthRequiredError(403) = false")
	}
	if isAuthRequiredError(harness.ClientError{StatusCode: http.StatusInternalServerError, Message: "upstream unauthorized"}) {
		t.Fatal("isAuthRequiredError(500 unauthorized body) = true")
	}
}

func TestCloneStartTurnRequestOwnsMutableFields(t *testing.T) {
	const (
		originalValue = "original"
		changedValue  = "changed"
	)
	original := defaultStartTurnRequest("clone")
	original.Metadata["caller"] = originalValue
	original.Input.Env = []harness.TurnEnvVar{{Name: "SAFE", Value: originalValue}}
	original.Input.Tools = []harness.ToolDefinition{{Name: originalValue}}
	cloned := cloneStartTurnRequest(original)
	cloned.Metadata["caller"] = changedValue
	cloned.Input.Env[0].Value = changedValue
	cloned.Input.Tools[0].Name = changedValue
	if original.Metadata["caller"] != originalValue || original.Input.Env[0].Value != originalValue ||
		original.Input.Tools[0].Name != originalValue {
		t.Fatalf("clone mutated original request: %#v", original)
	}
}

func TestValidateProbeFramesRejectsPostTerminalFrames(t *testing.T) {
	request := defaultStartTurnRequest("post-terminal")
	frames := []harness.HarnessEventFrame{
		sequenceProbeFrame(request, harness.FrameTurnStarted, 1),
		sequenceProbeFrame(request, harness.FrameTurnCompleted, 2),
		sequenceProbeFrame(request, harness.FrameRuntimeOutput, 3),
	}
	frames[1].Completed = &harness.TurnCompleted{Result: "ok", FinalEventSeq: 2}
	result := Result{Passed: true}
	validateProbeFrames(&result, request, frames)
	if !strings.Contains(strings.Join(result.Failures, "\n"), "appears after terminal frame") {
		t.Fatalf("failures = %#v", result.Failures)
	}
}

func TestCheckReadinessSelectsBrokeredProbeForBrokeredOnlyRuntime(t *testing.T) {
	server := newAgentKitOrkaFixture(t)
	server.brokeredClass = harness.BrokeredToolClassRead
	server.brokeredOnly = true
	defer server.Close()
	result := CheckReadiness(context.Background(), Target{BaseURL: server.URL, BearerToken: "x"})
	if !result.Passed {
		t.Fatalf("Passed = false, failures=%v", result.Failures)
	}
}

func TestCheckRejectsMultipleTurnProbes(t *testing.T) {
	result := Check(context.Background(), Target{
		BaseURL:           "http://127.0.0.1:1",
		ProbeTurn:         true,
		ProbeBrokeredRead: true,
	})
	if result.Passed || !strings.Contains(result.Message, "only one turn conformance probe") {
		t.Fatalf("result = %#v, want multiple probe rejection", result)
	}
}

func TestBrokeredProbeCancelsAfterInvalidTerminalIdentity(t *testing.T) {
	server := newAgentKitOrkaFixture(t)
	server.brokeredClass = harness.BrokeredToolClassRead
	server.brokeredTerminalIdentityMismatch = true
	defer server.Close()
	result := Check(context.Background(), Target{
		BaseURL:           server.URL,
		BearerToken:       "x",
		RequireAuth:       true,
		ProbeBrokeredRead: true,
	})
	if result.Passed {
		t.Fatal("Passed = true, want invalid terminal identity failure")
	}
	server.mu.Lock()
	cancelCount := server.cancelCount
	server.mu.Unlock()
	if cancelCount != 1 {
		t.Fatalf("cancel count = %d, want 1", cancelCount)
	}
}

func TestBrokeredProbeRejectsPostTerminalFrames(t *testing.T) {
	server := newAgentKitOrkaFixture(t)
	server.brokeredClass = harness.BrokeredToolClassRead
	server.brokeredPostTerminalFrame = true
	defer server.Close()
	result := Check(context.Background(), Target{
		BaseURL:           server.URL,
		BearerToken:       "x",
		RequireAuth:       true,
		ProbeBrokeredRead: true,
	})
	if result.Passed || !strings.Contains(result.Message, "appears after terminal frame") {
		t.Fatalf("result = %#v, want post-terminal frame failure", result)
	}
}

func TestCheckReadinessProbesEveryBrokeredOnlyClass(t *testing.T) {
	server := newAgentKitOrkaFixture(t)
	server.brokeredOnly = true
	server.brokeredClasses = []harness.BrokeredToolClass{
		harness.BrokeredToolClassRead,
		harness.BrokeredToolClassWrite,
	}
	defer server.Close()
	result := CheckReadiness(context.Background(), Target{BaseURL: server.URL, BearerToken: "x"})
	if !result.Passed {
		t.Fatalf("Passed = false, failures=%v", result.Failures)
	}
	server.mu.Lock()
	continued := len(server.continued)
	server.mu.Unlock()
	if continued != 2 {
		t.Fatalf("continued turns = %d, want 2 brokered profiles", continued)
	}
}

func TestCheckReadinessProbesObservedAndEveryBrokeredClass(t *testing.T) {
	server := newAgentKitOrkaFixture(t)
	server.brokeredClasses = []harness.BrokeredToolClass{
		harness.BrokeredToolClassRead,
		harness.BrokeredToolClassWrite,
	}
	defer server.Close()
	result := CheckReadiness(context.Background(), Target{BaseURL: server.URL, BearerToken: "x"})
	if !result.Passed {
		t.Fatalf("Passed = false, failures=%v", result.Failures)
	}
	server.mu.Lock()
	turns := len(server.turns)
	continued := len(server.continued)
	server.mu.Unlock()
	if turns != 3 {
		t.Fatalf("turns = %d, want 1 observed and 2 brokered probes", turns)
	}
	if continued != 2 {
		t.Fatalf("continued turns = %d, want 2 brokered profiles", continued)
	}
}

func TestBrokeredProbeCancelsAfterInvalidStartResponse(t *testing.T) {
	server := newAgentKitOrkaFixture(t)
	server.brokeredClass = harness.BrokeredToolClassRead
	server.startResponseVersion = "orka.harness.invalid"
	defer server.Close()
	result := Check(context.Background(), Target{
		BaseURL:           server.URL,
		BearerToken:       "x",
		RequireAuth:       true,
		ProbeBrokeredRead: true,
	})
	if result.Passed {
		t.Fatal("Passed = true, want invalid start response failure")
	}
	server.mu.Lock()
	cancelCount := server.cancelCount
	server.mu.Unlock()
	if cancelCount != 1 {
		t.Fatalf("cancel count = %d, want 1", cancelCount)
	}
}

func TestBrokeredReadinessPreservesCustomProbeTemplate(t *testing.T) {
	server := newAgentKitOrkaFixture(t)
	server.brokeredOnly = true
	server.brokeredClasses = []harness.BrokeredToolClass{
		harness.BrokeredToolClassRead,
		harness.BrokeredToolClassWrite,
	}
	defer server.Close()
	request := defaultStartTurnRequest("custom-template")
	request.Namespace = "custom-namespace"
	request.AuthIdentity = harness.AuthIdentity{Subject: "custom:subject"}
	result := CheckReadiness(context.Background(), Target{
		BaseURL:          server.URL,
		BearerToken:      "x",
		StartTurnRequest: &request,
	})
	if !result.Passed {
		t.Fatalf("Passed = false, failures=%v", result.Failures)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.turns) != 2 {
		t.Fatalf("turn count = %d, want 2", len(server.turns))
	}
	seenIDs := map[harness.HarnessTurnID]struct{}{}
	for turnID, started := range server.turns {
		if started.Namespace != request.Namespace || started.AuthIdentity.Subject != request.AuthIdentity.Subject {
			t.Fatalf("started request = %#v, want custom template fields", started)
		}
		seenIDs[turnID] = struct{}{}
	}
	if len(seenIDs) != 2 {
		t.Fatalf("unique turn IDs = %d, want 2", len(seenIDs))
	}
}

func TestProbeStreamContextExtendsEarlierParentDeadlineForDrain(t *testing.T) {
	parentDeadline := time.Now().Add(20 * time.Millisecond)
	parent, cancelParent := context.WithDeadline(context.Background(), parentDeadline)
	defer cancelParent()
	stream, cancelStream := probeStreamContext(parent, time.Second)
	defer cancelStream()
	deadline, ok := stream.Deadline()
	want := parentDeadline.Add(postTerminalDrainTimeout)
	if !ok || !deadline.Equal(want) {
		t.Fatalf("stream deadline = %v, want %v", deadline, want)
	}
}

func TestBrokeredProbeStreamPreservesDecodedTerminalDuringCancellation(t *testing.T) {
	framesCh := make(chan harness.HarnessEventFrame)
	errCh := make(chan error, 1)
	decoded := make(chan struct{})
	producerDone := make(chan struct{})
	emitFrame := newBrokeredProbeFrameEmitter(framesCh)
	go func() {
		defer close(producerDone)
		close(decoded)
		if err := emitFrame(harness.HarnessEventFrame{Type: harness.FrameTurnCompleted}); err != nil {
			errCh <- err
			return
		}
		errCh <- context.Canceled
	}()
	<-decoded
	cancelled := false
	terminalSeen := false
	streamErr, drained := stopProbeStreamAndDrainFrames(
		func() { cancelled = true },
		framesCh,
		errCh,
		func(frame harness.HarnessEventFrame) bool {
			terminalSeen = isProbeTerminalFrame(frame.Type)
			return true
		},
	)
	if !cancelled {
		t.Fatal("stream cancel was not called")
	}
	if !drained {
		t.Fatal("in-flight frames were not drained")
	}
	if !errors.Is(streamErr, context.Canceled) {
		t.Fatalf("stream error = %v, want context canceled", streamErr)
	}
	if !terminalSeen {
		t.Fatal("decoded terminal frame was not recorded")
	}
	select {
	case <-producerDone:
	case <-time.After(time.Second):
		t.Fatal("stream producer did not exit")
	}
}

func TestStopProbeStreamAndDrainFramesTimesOutWhenProducerDoesNotExit(t *testing.T) {
	started := time.Now()
	streamErr, drained := stopProbeStreamAndDrainFrames(
		func() {},
		make(chan harness.HarnessEventFrame),
		make(chan error),
		func(harness.HarnessEventFrame) bool { return true },
	)
	if !drained {
		t.Fatal("drain unexpectedly failed")
	}
	if !errors.Is(streamErr, errProbeStreamShutdownTimeout) {
		t.Fatalf("stream error = %v, want shutdown timeout", streamErr)
	}
	if elapsed := time.Since(started); elapsed < postTerminalDrainTimeout || elapsed > time.Second {
		t.Fatalf("shutdown wait = %v, want bounded drain interval", elapsed)
	}
}

func TestStartTurnMayHaveBeenAccepted(t *testing.T) {
	if startTurnMayHaveBeenAccepted(harness.ClientError{StatusCode: http.StatusConflict}) {
		t.Fatal("409 response was treated as accepted")
	}
	if !startTurnMayHaveBeenAccepted(harness.ClientError{StatusCode: http.StatusAccepted}) {
		t.Fatal("2xx decode failure was not treated as potentially accepted")
	}
	if !startTurnMayHaveBeenAccepted(harness.ClientError{RemoteAccepted: true}) {
		t.Fatal("validated accepted-response error was not treated as accepted")
	}
}

func TestProbeStreamContextPropagatesExplicitCancellation(t *testing.T) {
	parent, cancelParent := context.WithTimeout(context.Background(), time.Minute)
	stream, cancelStream := probeStreamContext(parent, time.Minute)
	defer cancelStream()
	cancelParent()
	select {
	case <-stream.Done():
		if !errors.Is(stream.Err(), context.Canceled) {
			t.Fatalf("stream error = %v, want context canceled", stream.Err())
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("explicit parent cancellation was not propagated")
	}
}
