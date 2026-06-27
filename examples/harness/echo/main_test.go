package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sozercan/orka/internal/harness"
)

func TestEchoHarnessCancelEndpointMatchesCapabilities(t *testing.T) {
	s := &server{runtimeName: "orka-example-echo-harness", turns: map[harness.HarnessTurnID]harness.StartTurnRequest{}}
	mux := http.NewServeMux()
	mux.HandleFunc(harness.HealthPath, s.health)
	mux.HandleFunc(harness.CapabilitiesPath, s.capabilities)
	mux.HandleFunc(harness.TurnsPath, s.startTurn)
	mux.HandleFunc(harness.TurnsPath+"/", s.turn)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client, err := harness.NewClient(srv.URL)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	ctx := context.Background()
	caps, err := client.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities() error = %v", err)
	}
	if !caps.SupportsCancel {
		t.Fatal("echo harness should advertise cancellation")
	}
	request := harness.StartTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        "default",
		TaskName:         "task",
		SessionName:      "session",
		RuntimeSessionID: "runtime",
		TurnID:           "turn",
		CorrelationID:    "corr",
		Deadline:         time.Now().UTC().Add(time.Minute),
		AuthIdentity:     harness.AuthIdentity{Subject: "user:test"},
	}
	if _, err := client.StartTurn(ctx, request); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	cancelled, err := client.CancelTurn(ctx, harness.CancelTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        request.Namespace,
		TaskName:         request.TaskName,
		SessionName:      request.SessionName,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		Reason:           "test",
	})
	if err != nil {
		t.Fatalf("CancelTurn() error = %v", err)
	}
	if !cancelled.Accepted ||
		cancelled.TurnID != request.TurnID ||
		cancelled.RuntimeSessionID != request.RuntimeSessionID {
		t.Fatalf("CancelTurn() = %#v", cancelled)
	}
}
