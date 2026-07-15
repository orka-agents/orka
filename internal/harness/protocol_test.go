package harness

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const (
	protocolTestNamespace = "default"
	protocolTestTaskName  = "task-a"
)

func TestStartTurnRequestValidationAndJSONRoundTrip(t *testing.T) {
	request := StartTurnRequest{
		Version:           ProtocolVersion,
		Namespace:         protocolTestNamespace,
		TaskName:          protocolTestTaskName,
		SessionName:       "session-a",
		RuntimeSessionID:  "runtime-a",
		TurnID:            "turn-a",
		CorrelationID:     "corr-a",
		Deadline:          time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC),
		AuthIdentity:      AuthIdentity{Subject: "user:test"},
		ToolPolicyRef:     &PolicyRef{Name: "default-tools"},
		ApprovalPolicyRef: &PolicyRef{Name: "default-approvals"},
		EventCursor:       7,
		Input: TurnInput{Prompt: "hello", ContextRefs: []ContextRef{{Kind: "event", Name: protocolTestTaskName, Seq: 7}}, Tools: []ToolDefinition{{
			Name:          "read_incident",
			Description:   "Read incident status",
			BrokeredClass: BrokeredToolClassRead,
			Parameters:    json.RawMessage(`{"type":"object","properties":{"incident":{"type":"string"}}}`),
		}}},
		ToolExecutionMode: ToolExecutionModeObserved,
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var decoded StartTurnRequest
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("decoded Validate() error = %v", err)
	}
	if decoded.RuntimeSessionID != request.RuntimeSessionID || decoded.TurnID != request.TurnID {
		t.Fatalf("decoded ids = %q/%q, want %q/%q", decoded.RuntimeSessionID, decoded.TurnID, request.RuntimeSessionID, request.TurnID)
	}
}

func TestStartTurnRequestValidationErrorsAreDeterministic(t *testing.T) {
	request := StartTurnRequest{}
	want := []string{
		"version is required",
		"namespace is required",
		"task name is required",
		"session name is required",
		"runtime session id is required",
		"turn id is required",
		"correlation id is required",
		"deadline is required",
		"auth identity subject or username is required",
	}
	for _, message := range want {
		if err := request.Validate(); err == nil || err.Error() != message {
			t.Fatalf("Validate() = %v, want %q", err, message)
		}
		switch message {
		case "version is required":
			request.Version = ProtocolVersion
		case "namespace is required":
			request.Namespace = protocolTestNamespace
		case "task name is required":
			request.TaskName = protocolTestTaskName
		case "session name is required":
			request.SessionName = "session-a"
		case "runtime session id is required":
			request.RuntimeSessionID = "runtime-a"
		case "turn id is required":
			request.TurnID = "turn-a"
		case "correlation id is required":
			request.CorrelationID = "corr-a"
		case "deadline is required":
			request.Deadline = time.Now().UTC().Add(time.Minute)
		case "auth identity subject or username is required":
			request.AuthIdentity.Subject = "user:test"
		}
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("Validate() after required fields error = %v", err)
	}
}

func TestStartTurnRequestRejectsInvalidToolDefinitions(t *testing.T) {
	request := StartTurnRequest{
		Version:          ProtocolVersion,
		Namespace:        protocolTestNamespace,
		TaskName:         protocolTestTaskName,
		SessionName:      "session-a",
		RuntimeSessionID: "runtime-a",
		TurnID:           "turn-a",
		CorrelationID:    "corr-a",
		Deadline:         time.Now().UTC().Add(time.Minute),
		AuthIdentity:     AuthIdentity{Subject: "user:test"},
		Input: TurnInput{Tools: []ToolDefinition{{
			Name:          "read_incident",
			BrokeredClass: BrokeredToolClass("admin"),
		}}},
	}
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "unsupported brokered class") {
		t.Fatalf("Validate() = %v, want brokered class error", err)
	}
	request.Input.Tools[0].BrokeredClass = BrokeredToolClassRead
	request.Input.Tools[0].Parameters = json.RawMessage(`{"type":`)
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "parameters must be valid JSON") {
		t.Fatalf("Validate() = %v, want parameter JSON error", err)
	}
}

func TestCapabilitiesAndHealthValidateVersionedDTOs(t *testing.T) {
	capabilities := CapabilitiesResponse{
		Version:                 ProtocolVersion,
		ProtocolVersion:         ProtocolVersion,
		Transport:               HTTPTransport,
		RuntimeName:             "fake",
		ProviderKind:            ProviderKindRemote,
		ToolExecutionModes:      []ToolExecutionMode{ToolExecutionModeObserved, ToolExecutionModeBrokered},
		BrokeredToolClasses:     []BrokeredToolClass{BrokeredToolClassRead, BrokeredToolClassWrite},
		SupportsCancel:          true,
		SupportsRuntimeSessions: true,
		SupportsContinuation:    true,
		SupportsArtifacts:       true,
		MaxTurnSeconds:          600,
		MaxOutputBytes:          1048576,
	}
	if err := capabilities.Validate(); err != nil {
		t.Fatalf("Capabilities Validate() error = %v", err)
	}
	health := HealthResponse{Version: ProtocolVersion, Status: HealthStatusOK, Ready: true, CheckedAt: time.Now().UTC()}
	if err := health.Validate(); err != nil {
		t.Fatalf("Health Validate() error = %v", err)
	}
	capabilities.Version = "orka.harness.v2"
	if err := capabilities.Validate(); err == nil || !strings.Contains(err.Error(), "unsupported version") {
		t.Fatalf("Capabilities Validate() = %v, want unsupported version", err)
	}
}

func TestCapabilitiesRejectBrokeredClassesWithoutBrokeredMode(t *testing.T) {
	capabilities := CapabilitiesResponse{
		Version:              ProtocolVersion,
		ProtocolVersion:      ProtocolVersion,
		Transport:            HTTPTransport,
		RuntimeName:          "fake",
		ProviderKind:         ProviderKindRemote,
		ToolExecutionModes:   []ToolExecutionMode{ToolExecutionModeObserved},
		BrokeredToolClasses:  []BrokeredToolClass{BrokeredToolClassRead},
		MaxConcurrentTurns:   1,
		SupportsContinuation: true,
	}
	if err := capabilities.Validate(); err == nil || !strings.Contains(err.Error(), "brokeredToolClasses require") {
		t.Fatalf("Capabilities Validate() = %v, want brokered class dependency error", err)
	}
}

func TestContinueTurnRequestValidationAndResponseIdentity(t *testing.T) {
	request := ContinueTurnRequest{
		Version:          ProtocolVersion,
		Namespace:        protocolTestNamespace,
		TaskName:         protocolTestTaskName,
		SessionName:      "session-a",
		RuntimeSessionID: "runtime-a",
		TurnID:           "turn-a",
		CorrelationID:    "corr-a",
		ToolResults: []ToolCallResult{{
			Version:          ProtocolVersion,
			RuntimeSessionID: "runtime-a",
			TurnID:           "turn-a",
			ToolCallID:       "tool-1",
			IdempotencyKey:   "runtime-a:turn-a:tool-1",
			Approved:         true,
			Output:           json.RawMessage(`{"success":true}`),
		}},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("ContinueTurnRequest Validate() error = %v", err)
	}
	response := ContinueTurnResponse{
		Version:          ProtocolVersion,
		Accepted:         true,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
	}
	if err := response.ValidateFor(request); err != nil {
		t.Fatalf("ContinueTurnResponse ValidateFor() error = %v", err)
	}
	response.TurnID = "other"
	if err := response.ValidateFor(request); err == nil || !strings.Contains(err.Error(), "want") {
		t.Fatalf("ContinueTurnResponse ValidateFor() = %v, want identity error", err)
	}
}

func TestContinueTurnRequestRequiresToolResultPayload(t *testing.T) {
	request := ContinueTurnRequest{
		Version:          ProtocolVersion,
		Namespace:        protocolTestNamespace,
		TaskName:         protocolTestTaskName,
		SessionName:      "session-a",
		RuntimeSessionID: "runtime-a",
		TurnID:           "turn-a",
		CorrelationID:    "corr-a",
	}
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "at least one tool result") {
		t.Fatalf("ContinueTurnRequest Validate() = %v, want missing tool result error", err)
	}
	request.ToolResults = []ToolCallResult{{
		Version:          ProtocolVersion,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		ToolCallID:       "tool-1",
		IdempotencyKey:   "runtime-a:turn-a:tool-1",
	}}
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "output or error") {
		t.Fatalf("ContinueTurnRequest Validate() = %v, want missing payload error", err)
	}
}

func TestToolRequestIdempotencyKey(t *testing.T) {
	got := ToolRequestIdempotencyKey(" runtime ", " turn ", " tool ")
	if got != "runtime:turn:tool" {
		t.Fatalf("ToolRequestIdempotencyKey() = %q", got)
	}
}

func TestCapabilitiesBrokeredModeRequiresContinuation(t *testing.T) {
	capabilities := CapabilitiesResponse{
		Version:             ProtocolVersion,
		ProtocolVersion:     ProtocolVersion,
		Transport:           HTTPTransport,
		RuntimeName:         "brokered-runtime",
		ProviderKind:        ProviderKindRemote,
		ToolExecutionModes:  []ToolExecutionMode{ToolExecutionModeBrokered},
		BrokeredToolClasses: []BrokeredToolClass{BrokeredToolClassRead},
	}
	if err := capabilities.Validate(); err == nil || !strings.Contains(err.Error(), "supportsContinuation") {
		t.Fatalf("Capabilities Validate() = %v, want continuation requirement", err)
	}
	capabilities.SupportsContinuation = true
	if err := capabilities.Validate(); err != nil {
		t.Fatalf("Capabilities Validate() error = %v", err)
	}
}

func TestContinueTurnRequestRejectsDuplicateAndNonCanonicalToolResults(t *testing.T) {
	base := ToolCallResult{
		Version:          ProtocolVersion,
		RuntimeSessionID: "runtime-a",
		TurnID:           "turn-a",
		ToolCallID:       "tool-a",
		IdempotencyKey:   ToolRequestIdempotencyKey("runtime-a", "turn-a", "tool-a"),
		Output:           json.RawMessage(`{"ok":true}`),
	}
	request := ContinueTurnRequest{
		Version:          ProtocolVersion,
		Namespace:        "default",
		TaskName:         "task-a",
		SessionName:      "session-a",
		RuntimeSessionID: "runtime-a",
		TurnID:           "turn-a",
		CorrelationID:    "correlation-a",
		ToolResults:      []ToolCallResult{base, base},
	}
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "duplicates tool call id") {
		t.Fatalf("ContinueTurnRequest Validate() = %v, want duplicate rejection", err)
	}
	request.ToolResults = []ToolCallResult{base}
	request.ToolResults[0].IdempotencyKey = "unrelated-key"
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "canonical key") {
		t.Fatalf("ContinueTurnRequest Validate() = %v, want canonical key rejection", err)
	}
}

func TestCapabilitiesBrokeredModeRequiresToolClass(t *testing.T) {
	capabilities := CapabilitiesResponse{
		Version:              ProtocolVersion,
		ProtocolVersion:      ProtocolVersion,
		Transport:            HTTPTransport,
		RuntimeName:          "brokered-runtime",
		ProviderKind:         ProviderKindRemote,
		ToolExecutionModes:   []ToolExecutionMode{ToolExecutionModeBrokered},
		SupportsContinuation: true,
	}
	if err := capabilities.Validate(); err == nil || !strings.Contains(err.Error(), "brokeredToolClass") {
		t.Fatalf("Capabilities Validate() = %v, want brokered class requirement", err)
	}
}
