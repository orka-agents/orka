package storageclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/usage"
)

// TaskTokenUsage returns the input and output tokens a Task consumed, exactly
// as the usage report attributes them. The provider returns the Task's
// counter observations; Orka applies the attribution rules.
func (c *Client) TaskTokenUsage(ctx context.Context, namespace, taskUID string) (int64, int64, error) {
	observations, err := c.ListTaskCounterObservations(ctx, namespace, taskUID)
	if err != nil {
		return 0, 0, err
	}
	return usage.TaskTokens(observations, namespace, taskUID, "", time.Now())
}

// CommitSessionTurnFinalization attributes the turn's prompt attempt usage
// before the provider commits the finalization, which adds it to the
// Session's token counters the first time the turn is finalized. Totals
// beyond the exact reporting range stay out of the counters rather than block
// finalization. Native state is sealed before the provider sees it.
func (c *Client) CommitSessionTurnFinalization(
	ctx context.Context, request store.CommitSessionTurnFinalizationRequest,
) (*store.SessionTurn, error) {
	observations, err := c.ListTaskCounterObservations(ctx, request.Namespace, request.Key.TaskUID)
	if err != nil {
		return nil, fmt.Errorf("read session turn usage: %w", err)
	}
	attemptID := fmt.Sprintf("%d/%s", request.Key.Attempt, request.Key.PromptID)
	request.InputTokens, request.OutputTokens, err = usage.TaskTokens(observations, request.Namespace, request.Key.TaskUID, attemptID, time.Now())
	if errors.Is(err, usage.ErrTotalsOutOfRange) {
		request.InputTokens, request.OutputTokens = 0, 0
	} else if err != nil {
		return nil, fmt.Errorf("attribute session turn usage: %w", err)
	}
	return c.commitSessionTurnFinalization(ctx, request)
}

// deriveExecutionEventUsage attaches the usage observation Orka derives from
// a sanitized event and the caller's attribution, so the provider records it
// atomically with the event. An observation Orka code already attached is
// kept. Either way its provider and model names are redacted.
func deriveExecutionEventUsage(ctx context.Context, event *store.ExecutionEvent) error {
	if event.Usage == nil {
		derived, err := usage.ExecutionEventObservation(*event, store.UsageAttributionFrom(ctx))
		if err != nil {
			return err
		}
		event.Usage = derived
	}
	if event.Usage != nil {
		event.Usage.Observation = protectUsageObservation(event.Usage.Observation)
	}
	return nil
}
