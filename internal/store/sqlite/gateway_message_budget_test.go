package sqlite

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
)

func budgetQuery(event store.GatewayEvent, id string) store.GatewayMessageBudgetQuery {
	return store.GatewayMessageBudgetQuery{Namespace: event.Namespace, NamespaceUID: event.NamespaceUID, EventID: event.ID, TaskName: event.TaskName, TaskUID: event.TaskUID, RequestID: id}
}

func TestGatewayMessageBudgetReadOnlyReplayAndIncarnation(t *testing.T) {
	s := setupTestStore(t)
	now := time.Now().UTC()
	event := admitMessageTask(t, s, now)
	for _, state := range []store.GatewayDeliveryState{store.GatewayDeliveryPending, store.GatewayDeliverySending, store.GatewayDeliveryRetryScheduled, store.GatewayDeliveryDelivered, store.GatewayDeliveryFailed, store.GatewayDeliveryExpired, store.GatewayDeliveryDeadLettered} {
		req := messageEnqueue(event, now, string(state))
		row := enqueueMessage(t, s, req)
		_, err := s.db.ExecContext(t.Context(), `UPDATE gateway_deliveries SET state = ? WHERE id = ?`, state, row.ID)
		require.NoError(t, err)
	}
	before, err := s.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: event.Namespace})
	require.NoError(t, err)
	for _, id := range []string{"Pending", "new"} {
		b, err := s.GetGatewayMessageBudget(t.Context(), budgetQuery(event, id))
		require.NoError(t, err)
		require.Equal(t, 7, b.Accepted)
		require.Equal(t, id == "Pending", b.RequestExists)
	}
	after, err := s.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: event.Namespace})
	require.NoError(t, err)
	require.Equal(t, before, after)
	req := messageEnqueue(event, now, "Pending")
	req.MaxMessages = 1
	req.Text = "changed"
	_, _, err = s.EnqueueGatewayMessage(t.Context(), req)
	require.ErrorIs(t, err, store.ErrDuplicateMismatch)
	for _, mutate := range []func(*store.GatewayMessageBudgetQuery){func(q *store.GatewayMessageBudgetQuery) { q.TaskUID = "other" }, func(q *store.GatewayMessageBudgetQuery) { q.NamespaceUID = "other" }, func(q *store.GatewayMessageBudgetQuery) { q.EventID = "other" }, func(q *store.GatewayMessageBudgetQuery) { q.TaskName = "other" }, func(q *store.GatewayMessageBudgetQuery) { q.RequestID = "" }} {
		q := budgetQuery(event, "Pending")
		mutate(&q)
		_, err := s.GetGatewayMessageBudget(t.Context(), q)
		require.Error(t, err)
	}
}

func TestGatewayMessageBudgetRestartAndConcurrentLastSlot(t *testing.T) {
	for _, limit := range []int{2, 12} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "budget.db")
			db, err := NewDB(path)
			require.NoError(t, err)
			s := NewStore(db, path)
			now := time.Now().UTC()
			event := admitMessageTask(t, s, now)
			for i := 0; i < limit-1; i++ {
				req := messageEnqueue(event, now, fmt.Sprint(i))
				req.MaxMessages = limit
				enqueueMessage(t, s, req)
			}
			require.NoError(t, db.Close())
			db, err = NewDB(path)
			require.NoError(t, err)
			defer func() { require.NoError(t, db.Close()) }()
			s = NewStore(db, path)
			const callers = 8
			var wg sync.WaitGroup
			ready := make(chan struct{}, callers)
			start := make(chan struct{})
			results := make(chan error, callers)
			for i := range callers {
				wg.Go(func() {
					id := fmt.Sprintf("last-%d", i)
					b, err := s.GetGatewayMessageBudget(t.Context(), budgetQuery(event, id))
					if err != nil {
						t.Error(err)
					} else if b.Accepted != limit-1 || b.RequestExists {
						t.Errorf("budget = %+v", b)
					}
					ready <- struct{}{}
					<-start
					req := messageEnqueue(event, now, id)
					req.MaxMessages = limit
					_, _, err = s.EnqueueGatewayMessage(t.Context(), req)
					results <- err
				})
			}
			for range callers {
				<-ready
			}
			close(start)
			wg.Wait()
			close(results)
			accepted, full := 0, 0
			for err := range results {
				if err == nil {
					accepted++
				} else if errors.Is(err, store.ErrCapacity) {
					full++
				} else {
					t.Fatal(err)
				}
			}
			require.Equal(t, 1, accepted)
			require.Equal(t, callers-1, full)
			b, err := s.GetGatewayMessageBudget(t.Context(), budgetQuery(event, "0"))
			require.NoError(t, err)
			require.Equal(t, limit, b.Accepted)
			require.True(t, b.RequestExists)
		})
	}
}
