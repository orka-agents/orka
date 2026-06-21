package harness

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestStartTurnRequestValidationAndJSONRoundTrip(t *testing.T) {
	request := StartTurnRequest{
		Version:           ProtocolVersion,
		Namespace:         "default",
		TaskName:          "task-a",
		SessionName:       "session-a",
		RuntimeSessionID:  "runtime-a",
		TurnID:            "turn-a",
		CorrelationID:     "corr-a",
		Deadline:          time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC),
		AuthIdentity:      AuthIdentity{Subject: "user:test"},
		ToolPolicyRef:     &PolicyRef{Name: "default-tools"},
		ApprovalPolicyRef: &PolicyRef{Name: "default-approvals"},
		EventCursor:       7,
		Input:             TurnInput{Prompt: "hello", ContextRefs: []ContextRef{{Kind: "event", Name: "task-a", Seq: 7}}},
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
			request.Namespace = "default"
		case "task name is required":
			request.TaskName = "task-a"
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

func TestCapabilitiesAndHealthValidateVersionedDTOs(t *testing.T) {
	capabilities := CapabilitiesResponse{
		Version:                 ProtocolVersion,
		ProtocolVersion:         ProtocolVersion,
		Transport:               HTTPTransport,
		RuntimeName:             "fake",
		ProviderKind:            ProviderKindKubernetesService,
		ToolExecutionModes:      []ToolExecutionMode{ToolExecutionModeObserved, ToolExecutionModeBrokered},
		SupportsCancel:          true,
		SupportsRuntimeSessions: true,
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

func TestToolRequestIdempotencyKey(t *testing.T) {
	got := ToolRequestIdempotencyKey(" runtime ", " turn ", " tool ")
	if got != "runtime:turn:tool" {
		t.Fatalf("ToolRequestIdempotencyKey() = %q", got)
	}
}
