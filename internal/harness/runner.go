package harness

import (
	"context"
	"fmt"
	"time"

	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/store"
)

// TurnRunner is the provider-neutral Orka-side integration core for a harness
// turn. Controllers/providers supply endpoint discovery and task status changes;
// the runner owns protocol calls, frame mapping, event append, and terminal turn
// classification.
type TurnRunner struct {
	Client      *Client
	EventStore  store.ExecutionEventStore
	MapContext  EventMapContext
	TurnTimeout time.Duration
}

type TurnRunResult struct {
	Accepted  *StartTurnResponse
	Frames    []HarnessEventFrame
	Events    []store.ExecutionEvent
	Completed *TurnCompleted
	Failed    *TurnFailed
	Cancelled bool
}

func (r TurnRunner) Run(ctx context.Context, request StartTurnRequest) (TurnRunResult, error) {
	if r.Client == nil {
		return TurnRunResult{}, fmt.Errorf("harness client is required")
	}
	if r.EventStore == nil {
		return TurnRunResult{}, fmt.Errorf("execution event store is required")
	}
	if err := request.Validate(); err != nil {
		return TurnRunResult{}, err
	}
	if err := r.MapContext.validate(); err != nil {
		return TurnRunResult{}, err
	}
	if r.TurnTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.TurnTimeout)
		defer cancel()
	}

	accepted, err := r.Client.StartTurn(ctx, request)
	if err != nil {
		return TurnRunResult{}, err
	}
	if err := validateAcceptedTurn(request, *accepted); err != nil {
		return TurnRunResult{}, err
	}
	result := TurnRunResult{Accepted: accepted}
	// EventCursor is Orka's persisted task-event cursor. Harness frame cursors
	// are turn-local, so a newly started turn must stream from frame 0.
	var lastFrameSeq int64
	err = r.Client.StreamFrames(ctx, request.TurnID, 0, func(frame HarnessEventFrame) error {
		if err := validateFrameForTurn(request, frame); err != nil {
			return err
		}
		if frame.Seq <= lastFrameSeq {
			return fmt.Errorf("harness frame seq %d must be greater than previous seq %d", frame.Seq, lastFrameSeq)
		}
		lastFrameSeq = frame.Seq
		mapped, err := MapFrameToExecutionEvent(frame, r.MapContext)
		if err != nil {
			return err
		}
		appended, err := r.EventStore.AppendExecutionEvent(ctx, mapped)
		if err != nil {
			return fmt.Errorf("append mapped harness event: %w", err)
		}
		result.Frames = append(result.Frames, frame)
		result.Events = append(result.Events, *appended)
		switch frame.Type {
		case FrameTurnCompleted:
			result.Completed = frame.Completed
		case FrameTurnFailed:
			result.Failed = frame.Failed
		case FrameTurnCancelled:
			result.Cancelled = true
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	if result.Failed != nil {
		return result, fmt.Errorf("harness turn failed: %s", events.RedactExecutionEventText(result.Failed.Reason))
	}
	if result.Completed == nil && !result.Cancelled {
		return result, fmt.Errorf("harness turn ended without terminal frame")
	}
	return result, nil
}

func validateAcceptedTurn(request StartTurnRequest, accepted StartTurnResponse) error {
	if accepted.RuntimeSessionID != request.RuntimeSessionID {
		return fmt.Errorf("harness accepted runtime session %q, want %q", accepted.RuntimeSessionID, request.RuntimeSessionID)
	}
	if accepted.TurnID != request.TurnID {
		return fmt.Errorf("harness accepted turn %q, want %q", accepted.TurnID, request.TurnID)
	}
	if accepted.CorrelationID != "" && accepted.CorrelationID != request.CorrelationID {
		return fmt.Errorf("harness accepted correlation id %q, want %q", accepted.CorrelationID, request.CorrelationID)
	}
	return nil
}

func validateFrameForTurn(request StartTurnRequest, frame HarnessEventFrame) error {
	if frame.RuntimeSessionID != request.RuntimeSessionID {
		return fmt.Errorf("harness frame runtime session %q, want %q", frame.RuntimeSessionID, request.RuntimeSessionID)
	}
	if frame.TurnID != request.TurnID {
		return fmt.Errorf("harness frame turn %q, want %q", frame.TurnID, request.TurnID)
	}
	if frame.CorrelationID != request.CorrelationID {
		return fmt.Errorf("harness frame correlation id %q, want %q", frame.CorrelationID, request.CorrelationID)
	}
	return nil
}

func (r TurnRunner) Cancel(ctx context.Context, request CancelTurnRequest) (*CancelTurnResponse, error) {
	if r.Client == nil {
		return nil, fmt.Errorf("harness client is required")
	}
	return r.Client.CancelTurn(ctx, request)
}
