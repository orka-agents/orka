package cliwrapper

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/harness"
)

const (
	FakeBehaviorSuccess         = "success"
	FakeBehaviorFailure         = "failure"
	FakeBehaviorDelayed         = "delayed-output"
	FakeBehaviorLongRunning     = "long-running"
	FakeBehaviorCancellation    = "cancellation"
	FakeBehaviorInvalidFrame    = "invalid-frame"
	FakeBehaviorRedactionOutput = "secret-output"
)

type FakeAdapter struct {
	Behavior        string
	Delay           time.Duration
	RuntimeName     string
	RedactionOutput string
	Now             func() time.Time
}

func NewFakeAdapter(behavior string) *FakeAdapter {
	return &FakeAdapter{
		Behavior:    behavior,
		Delay:       5 * time.Millisecond,
		RuntimeName: "fake-harness-wrapper",
		Now:         time.Now,
	}
}

func (a *FakeAdapter) Name() string {
	if strings.TrimSpace(a.RuntimeName) != "" {
		return a.RuntimeName
	}
	return "fake-harness-wrapper"
}

func (a *FakeAdapter) BuildCommand(context.Context, TurnContext) (*CommandSpec, error) {
	return nil, fmt.Errorf("fake adapter emits frames directly")
}

func (a *FakeAdapter) ParseResult(context.Context, TurnContext, CommandResult) (TurnResult, error) {
	return TurnResult{}, fmt.Errorf("fake adapter emits frames directly")
}

func (a *FakeAdapter) RunTurn(
	ctx context.Context,
	turn TurnContext,
	emit func(harness.HarnessEventFrame) error,
) (TurnResult, error) {
	behavior := a.Behavior
	if behavior == "" {
		behavior = FakeBehaviorSuccess
	}
	delay := a.Delay
	if delay <= 0 {
		delay = 5 * time.Millisecond
	}
	frames := a.framesFor(turn, behavior)
	for _, frame := range frames {
		if err := emit(frame); err != nil {
			return TurnResult{}, err
		}
		if behavior == FakeBehaviorDelayed {
			if !sleepContext(ctx, delay) {
				return TurnResult{}, ctx.Err()
			}
		}
	}
	if behavior == FakeBehaviorLongRunning || behavior == FakeBehaviorCancellation {
		select {
		case <-ctx.Done():
			_ = emit(a.frame(turn, harness.FrameTurnCancelled, "cancelled", nil))
			return TurnResult{}, nil
		case <-time.After(10 * time.Second):
			_ = emit(a.frame(
				turn,
				harness.FrameTurnFailed,
				"turn timeout",
				&harness.TurnFailed{Reason: "timeout", Message: "fake long-running turn timed out"},
			))
			return TurnResult{}, nil
		}
	}
	return TurnResult{Result: "ok"}, nil
}

func (a *FakeAdapter) framesFor(turn TurnContext, behavior string) []harness.HarnessEventFrame {
	start := a.frame(turn, harness.FrameTurnStarted, "turn started", nil)
	switch behavior {
	case FakeBehaviorFailure:
		failed := a.frame(
			turn,
			harness.FrameTurnFailed,
			"turn failed",
			&harness.TurnFailed{Reason: "fake_failure", Message: "simulated failure"},
		)
		failed.Error = &harness.ErrorInfo{Code: "fake_failure", Message: "simulated failure"}
		return []harness.HarnessEventFrame{start, failed}
	case FakeBehaviorInvalidFrame:
		invalid := a.frame(turn, harness.FrameType("BogusFrame"), "bogus", nil)
		return []harness.HarnessEventFrame{start, invalid}
	case FakeBehaviorRedactionOutput:
		output := a.frame(turn, harness.FrameRuntimeOutput, "secret output", nil)
		secretOutput := a.RedactionOutput
		if secretOutput == "" {
			secretOutput = strings.Join([]string{
				"Authorization:", "Bearer", "bearer-value-for-redaction",
				"api" + "_key=" + "redaction-value-1234567890",
			}, " ")
		}
		output.ContentText = secretOutput
		output.Content = json.RawMessage(fmt.Sprintf(`{"message":%q}`, secretOutput))
		completed := a.frame(turn, harness.FrameTurnCompleted, "turn completed", &harness.TurnCompleted{Result: "ok"})
		return []harness.HarnessEventFrame{start, output, completed}
	case FakeBehaviorLongRunning, FakeBehaviorCancellation:
		return []harness.HarnessEventFrame{start}
	default:
		output := a.frame(turn, harness.FrameRuntimeOutput, "echo", nil)
		output.Content = json.RawMessage(fmt.Sprintf(`{"message":%q}`, "echo: "+turn.Prompt))
		output.ContentText = "echo: " + turn.Prompt
		tool := a.frame(turn, harness.FrameToolCallRequested, "tool requested", nil)
		tool.ToolName = "echo"
		tool.ToolCallID = "tool-1"
		tool.Content = json.RawMessage(`{"input":"hello"}`)
		result := a.frame(turn, harness.FrameToolResultReceived, "tool completed", nil)
		result.ToolName = "echo"
		result.ToolCallID = "tool-1"
		result.Content = json.RawMessage(`{"output":"hello"}`)
		completed := a.frame(turn, harness.FrameTurnCompleted, "turn completed", &harness.TurnCompleted{Result: "ok"})
		return []harness.HarnessEventFrame{start, output, tool, result, completed}
	}
}

func (a *FakeAdapter) frame(
	turn TurnContext,
	typ harness.FrameType,
	summary string,
	terminal any,
) harness.HarnessEventFrame {
	now := a.Now
	if now == nil {
		now = time.Now
	}
	frame := harness.HarnessEventFrame{
		Version:          harness.ProtocolVersion,
		Type:             typ,
		RuntimeSessionID: harness.RuntimeSessionID(turn.RuntimeSessionID),
		TurnID:           harness.HarnessTurnID(turn.TurnID),
		CorrelationID:    turn.CorrelationID,
		CreatedAt:        now().UTC(),
		Severity:         events.ExecutionEventSeverityInfo,
		Summary:          summary,
		Metadata: map[string]string{
			"fakeBehavior": a.Behavior,
		},
	}
	switch value := terminal.(type) {
	case *harness.TurnCompleted:
		frame.Completed = value
	case *harness.TurnFailed:
		frame.Failed = value
		frame.Severity = events.ExecutionEventSeverityError
	}
	return frame
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
