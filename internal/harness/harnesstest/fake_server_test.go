package harnesstest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/orka/internal/harness"
)

func TestFakeHarnessPassesConformance(t *testing.T) {
	RunHarnessConformance(t, func(t *testing.T, behavior FakeBehavior) (string, func()) {
		t.Helper()
		server := NewFakeHarnessServer(FakeHarnessConfig{Behavior: behavior})
		return server.URL(), server.Close
	})
}

func TestFakeHarnessRejectsUnsafeTurnID(t *testing.T) {
	server := NewFakeHarnessServer(FakeHarnessConfig{Behavior: BehaviorSuccess})
	defer server.Close()
	client, err := harness.NewClient(server.URL())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.StartTurn(context.Background(), harness.StartTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        "default",
		TaskName:         "task-a",
		SessionName:      "session-a",
		RuntimeSessionID: "runtime-a",
		TurnID:           "../bad",
		CorrelationID:    "corr-a",
		Deadline:         time.Now().UTC().Add(time.Minute),
		AuthIdentity:     harness.AuthIdentity{Subject: "user:test"},
	})
	if err == nil || !strings.Contains(err.Error(), "single safe path segment") {
		t.Fatalf("StartTurn() error = %v, want unsafe segment rejection", err)
	}
}
