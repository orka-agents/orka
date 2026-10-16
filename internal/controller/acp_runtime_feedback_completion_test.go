package controller

import (
	"context"
	"testing"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/runtimefeedback"
)

// This uses the same reason selection and registered completion callback as
// dispatch. The runtime-cancelled case deliberately keeps the caller live.
func TestRuntimeFeedbackCompletionRetainsRuntimeCancellation(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		terminal      harnessv2.EventType
		cancelContext bool
		want          string
	}{
		{name: "runtime cancelled with live controller", terminal: harnessv2.EventCancelled, want: runtimefeedback.Cancelled},
		{name: "normal completion", terminal: harnessv2.EventCompleted, want: runtimefeedback.Completed},
		{name: "runtime failure closes capture", terminal: harnessv2.EventFailed, want: runtimefeedback.Completed},
		{name: "no terminal received", want: runtimefeedback.Cancelled},
		{name: "controller cancelled", terminal: harnessv2.EventCompleted, cancelContext: true, want: runtimefeedback.Cancelled},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newFeedbackFixture(t)
			idle := *fixture.status.Load()
			idle.Sessions = append([]harnessv2.RuntimeSessionStatus(nil), idle.Sessions...)
			idle.Sessions[0].State = harnessv2.RuntimeSessionStateIdle
			idle.Sessions[0].ActivePromptID = ""
			idle.ActivePrompts = nil
			idle.Pressure.ActivePrompts = 0
			fixture.status.Store(&idle)

			service := &feedbackTestService{}
			dispatcher := &ACPDispatcher{Client: fixture.reader, APIReader: fixture.reader, RuntimeFeedback: service}
			policy := harnessv2.MCPPolicyConfiguration{ToolPolicy: harnessv2.MCPToolPolicy{AllowedToolNames: []string{RuntimeFeedbackToolName}}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			complete, err := dispatcher.startRuntimeFeedback(ctx, fixture.task, fixture.request.Metadata.Fence, fixture.runtimeClient, policy)
			if err != nil {
				t.Fatal(err)
			}
			var terminal *harnessv2.Event
			if scenario.terminal != "" {
				terminal = &harnessv2.Event{Type: scenario.terminal}
			}
			if scenario.cancelContext {
				cancel()
			} else if ctx.Err() != nil {
				t.Fatal("fixture cancelled the live controller context")
			}
			complete(runtimeFeedbackCompletionReason(ctx, terminal))
			if len(service.registrations) != 1 || len(service.completions) != 1 || len(service.reasons) != 1 {
				t.Fatalf("capture lifecycle counts = %d registrations, %d completions, %d reasons", len(service.registrations), len(service.completions), len(service.reasons))
			}
			if service.registrations[0] != service.completions[0] || service.reasons[0] != scenario.want {
				t.Fatalf("capture completed with reason %q, want %q and unchanged registered identity", service.reasons[0], scenario.want)
			}
		})
	}
}
