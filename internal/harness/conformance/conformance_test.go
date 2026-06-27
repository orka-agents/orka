package conformance

import (
	"context"
	"strings"
	"testing"

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
