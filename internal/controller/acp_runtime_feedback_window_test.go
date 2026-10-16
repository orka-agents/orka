package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/runtimefeedback"
)

func TestRuntimeFeedbackToolRequiresCaptureOverlappingAdmittedPrompt(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		status      string
		endOffset   time.Duration
		unavailable bool
	}{
		{name: "collecting through admission", status: runtimefeedback.Collecting},
		{name: "expired before late admission", status: runtimefeedback.Expired, endOffset: -time.Second, unavailable: true},
		{name: "expired at admission", status: runtimefeedback.Expired, unavailable: true},
		{name: "finalized before unsent retry", status: runtimefeedback.Finalized, endOffset: -time.Second, unavailable: true},
		{name: "finalized at admission", status: runtimefeedback.Finalized, unavailable: true},
		{name: "expired during current prompt", status: runtimefeedback.Expired, endOffset: 30 * time.Second},
		{name: "finalized during current prompt", status: runtimefeedback.Finalized, endOffset: 30 * time.Second},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newFeedbackFixture(t)
			startedAt := time.Now().UTC().Add(-time.Minute)
			setFeedbackPromptStartedAt(f, startedAt)
			service := &feedbackTestService{report: func(_ context.Context, q runtimefeedback.Query) (runtimefeedback.Report, error) {
				r := feedbackReport(q)
				r.Status = scenario.status
				r.Capture.StartedAt = startedAt.Add(-time.Minute)
				r.Capture.ExpiresAt = r.Capture.StartedAt.Add(runtimefeedback.CaptureSeconds * time.Second)
				if scenario.status != runtimefeedback.Collecting {
					endedAt := startedAt.Add(scenario.endOffset)
					r.Capture.EndedAt = &endedAt
					r.Events[0].Timestamp = endedAt.Add(-time.Second)
					if scenario.status == runtimefeedback.Expired {
						r.Capture.ExpiresAt = endedAt
					}
				}
				return r, nil
			}}
			tool := &runtimeFeedbackTool{reader: f.reader, service: service}
			result, err := tool.Execute(f.ctx, json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			var decoded runtimeFeedbackResult
			if err := json.Unmarshal([]byte(result), &decoded); err != nil {
				t.Fatal(err)
			}
			if scenario.unavailable {
				if decoded.Status != runtimefeedback.Unavailable || decoded.Report != nil || strings.Contains(result, "203.0.113.9") {
					t.Fatal("released pre-admission capture evidence to the retried prompt")
				}
			} else if decoded.Report == nil || decoded.Report.Status != scenario.status || decoded.Report.Completeness != "Partial" ||
				decoded.Report.AttributionScope != "Container" || len(decoded.Report.Events) != 1 {
				t.Fatal("valid current-prompt evidence lost its bounded partial container semantics")
			}
			if len(service.registrations) != 0 || len(service.completions) != 0 {
				t.Fatal("diagnosis renewed or changed the frozen capture")
			}
		})
	}
}

func TestRuntimeFeedbackToolRejectsMissingOrChangedPromptStart(t *testing.T) {
	for _, scenario := range []string{"missing start", "start changed during read"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFeedbackFixture(t)
			if scenario == "missing start" {
				setFeedbackPromptStartedAt(f, time.Time{})
			}
			reports := 0
			service := &feedbackTestService{report: func(_ context.Context, q runtimefeedback.Query) (runtimefeedback.Report, error) {
				reports++
				setFeedbackPromptStartedAt(f, f.status.Load().ActivePrompts[0].StartedAt.Add(time.Nanosecond))
				return feedbackReport(q), nil
			}}
			tool := &runtimeFeedbackTool{reader: f.reader, service: service}
			result, err := tool.Execute(f.ctx, json.RawMessage(`{}`))
			if err == nil || result != "" {
				t.Fatal("released evidence without an unchanged verified prompt start")
			}
			wantReports := 1
			if scenario == "missing start" {
				wantReports = 0
			}
			if reports != wantReports {
				t.Fatalf("report calls = %d, want %d", reports, wantReports)
			}
		})
	}
}

func setFeedbackPromptStartedAt(f *feedbackFixture, startedAt time.Time) {
	status := *f.status.Load()
	status.ActivePrompts = append([]harnessv2.ActivePromptStatus(nil), status.ActivePrompts...)
	status.ActivePrompts[0].StartedAt = startedAt
	f.status.Store(&status)
}
