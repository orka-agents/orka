package harness_test

import (
	"testing"
	"time"

	"github.com/orka-agents/orka/pkg/harness"
)

func TestExternalAdapterProtocolSurface(t *testing.T) {
	request := harness.StartTurnRequest{
		Version:           harness.ProtocolVersion,
		Namespace:         "default",
		TaskName:          "adapter-test",
		SessionName:       "adapter-test",
		RuntimeSessionID:  "runtime-session",
		TurnID:            "turn-1",
		CorrelationID:     "correlation-1",
		Deadline:          time.Now().UTC().Add(time.Minute),
		AuthIdentity:      harness.AuthIdentity{Subject: "task:default/adapter-test"},
		ToolExecutionMode: harness.ToolExecutionModeObserved,
		Input:             harness.TurnInput{Prompt: "test"},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("public StartTurnRequest.Validate() error = %v", err)
	}
	got := harness.ToolRequestIdempotencyKey(request.RuntimeSessionID, request.TurnID, "tool-1")
	if got != "runtime-session:turn-1:tool-1" {
		t.Fatalf("ToolRequestIdempotencyKey() = %q", got)
	}
}
