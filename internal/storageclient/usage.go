package storageclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/usage"
)

func (c *Client) TaskTokenUsage(ctx context.Context, namespace, taskUID string) (int64, int64, error) {
	observations, err := c.ListTaskCounterObservations(ctx, namespace, taskUID)
	if err != nil {
		return 0, 0, err
	}
	return usage.TaskTokens(observations, namespace, taskUID, "", time.Now())
}

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
