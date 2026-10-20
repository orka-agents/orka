package storagetest

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// concurrentCallers is how many callers race in each concurrency case. A
// provider that is not linearizable per dataset, such as one that reads and
// then writes without a lock or a serializable transaction, admits more than
// one winner.
const concurrentCallers = 8

// runConcurrently releases concurrentCallers callers together and returns
// their errors in caller order.
func runConcurrently(call func(caller int) error) []error {
	errs := make([]error, concurrentCallers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for caller := range concurrentCallers {
		wg.Go(func() {
			<-start
			errs[caller] = call(caller)
		})
	}
	close(start)
	wg.Wait()
	return errs
}

// requireOneWinner checks that exactly one caller succeeded and every other
// caller was refused with want.
func requireOneWinner(t *testing.T, errs []error, want error) int {
	t.Helper()
	winner := -1
	for caller, err := range errs {
		if err == nil {
			require.Equal(t, -1, winner, "callers %d and %d both succeeded", winner, caller)
			winner = caller
			continue
		}
		require.ErrorIs(t, err, want, "caller %d", caller)
	}
	require.NotEqual(t, -1, winner, "no caller succeeded")
	return winner
}

func runConcurrency(t *testing.T, newProvider StateFactory) {
	t.Run("parallel Session creation admits one", func(t *testing.T) {
		provider := newProvider(t)
		now := time.Now().UTC()
		errs := runConcurrently(func(caller int) error {
			return provider.CreateSession(t.Context(), &storagev1.SessionRecord{
				Namespace: "tenant-a", Name: "contended", SessionType: sessionsTypeTask,
				ActiveTask: fmt.Sprintf("task-%d", caller), CreatedAt: now, UpdatedAt: now,
			})
		})
		winner := requireOneWinner(t, errs, storagev1.ErrConflict)
		got, err := provider.GetSession(t.Context(), "tenant-a", "contended")
		require.NoError(t, err)
		require.Equal(t, fmt.Sprintf("task-%d", winner), got.ActiveTask, "the stored Session is the winner's")
	})

	t.Run("parallel chat turns admit one", func(t *testing.T) {
		provider := newProvider(t)
		expires := time.Now().UTC().Add(time.Hour)
		created := make([]bool, concurrentCallers)
		errs := runConcurrently(func(caller int) error {
			session := &storagev1.SessionRecord{Namespace: "tenant-a", Name: "chat", SessionType: sessionsTypeChat}
			var err error
			created[caller], err = provider.AcquireChatTurn(t.Context(), session, fmt.Sprintf("turn-%d", caller), expires)
			return err
		})
		winner := requireOneWinner(t, errs, storagev1.ErrConflict)
		require.True(t, created[winner], "the first reservation creates the chat Session")
	})

	t.Run("parallel deduplicated appends store one event", func(t *testing.T) {
		provider := newProvider(t)
		appended := make([]bool, concurrentCallers)
		ids := make([]string, concurrentCallers)
		errs := runConcurrently(func(caller int) error {
			event := taskEvent(eventsTenant, "task", storagev1.ExecutionEventTypeTaskStarted)
			event.Summary = fmt.Sprintf("caller %d", caller)
			stored, created, err := provider.AppendExecutionEventIfAbsent(t.Context(), event, "contended-key")
			if err == nil {
				appended[caller], ids[caller] = created, stored.ID
			}
			return err
		})
		for caller, err := range errs {
			require.NoError(t, err, "caller %d", caller)
		}
		winners := 0
		for caller := range concurrentCallers {
			if appended[caller] {
				winners++
			}
			require.Equal(t, ids[0], ids[caller], "every caller sees the one stored event")
		}
		require.Equal(t, 1, winners, "exactly one append is new")
		require.Len(t, listEvents(t, provider, storagev1.ExecutionEventFilter{Namespace: eventsTenant, StreamID: "task"}), 1)
	})

	t.Run("parallel terminal approvals accept one", func(t *testing.T) {
		provider := newProvider(t)
		errs := runConcurrently(func(caller int) error {
			eventType := storagev1.ExecutionEventTypeApprovalApproved
			if caller%2 == 1 {
				eventType = storagev1.ExecutionEventTypeApprovalDeclined
			}
			event := taskEvent(eventsTenant, "task-approval", eventType)
			event.Content = json.RawMessage(`{"approvalID":"approval-1"}`)
			_, err := provider.AppendExecutionEvent(t.Context(), event)
			return err
		})
		requireOneWinner(t, errs, storagev1.ErrConflict)
		require.Len(t, listEvents(t, provider, storagev1.ExecutionEventFilter{Namespace: eventsTenant, StreamID: "task-approval"}), 1)
	})

	t.Run("parallel gateway claims lease each event once", func(t *testing.T) {
		provider := newProvider(t)
		now := time.Now().UTC().Truncate(time.Second)
		const events = concurrentCallers / 2
		for i := range events {
			admitGateway(t, provider, gatewayEvent(now, eventsTenant, fmt.Sprintf("contended-%d", i)))
		}
		claimed := make([]string, concurrentCallers)
		errs := runConcurrently(func(caller int) error {
			event, err := provider.ClaimNextGatewayEvent(t.Context(), eventsTenant, fmt.Sprintf("owner-%d", caller), now, time.Minute)
			if err == nil {
				claimed[caller] = event.ID
			}
			return err
		})
		seen := map[string]int{}
		for caller, err := range errs {
			if err != nil {
				require.ErrorIs(t, err, storagev1.ErrNotFound, "caller %d", caller)
				continue
			}
			previous, duplicate := seen[claimed[caller]]
			require.False(t, duplicate, "callers %d and %d claimed %s", previous, caller, claimed[caller])
			seen[claimed[caller]] = caller
		}
		require.Len(t, seen, events, "every event is claimed exactly once")
	})
}
