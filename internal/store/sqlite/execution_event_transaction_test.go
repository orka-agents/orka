package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

func TestListExecutionEventsUsesTaskDataTransaction(t *testing.T) {
	s := newCoexistenceTestStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	rollback := errors.New("rollback the uncommitted history")
	filter := store.ExecutionEventFilter{Namespace: "ns", StreamType: "task", StreamID: "task"}
	err := s.WithTaskDataTransaction(ctx, func(txCtx context.Context) error {
		_, err := s.AppendExecutionEvent(txCtx, &store.ExecutionEvent{
			Namespace: "ns", StreamType: "task", StreamID: "task", TaskName: "task",
			Type: events.ExecutionEventTypeApprovalRequested, ToolCallID: "approval-1", Summary: "Review operation",
		})
		if err != nil {
			return err
		}
		listed, err := s.ListExecutionEvents(txCtx, filter)
		if err != nil {
			return err
		}
		require.Len(t, listed, 1, "the read must see the same transaction's uncommitted request")
		require.Equal(t, "approval-1", listed[0].ToolCallID)
		return rollback
	})
	require.ErrorIs(t, err, rollback)
	listed, err := s.ListExecutionEvents(t.Context(), filter)
	require.NoError(t, err)
	require.Empty(t, listed)
}
