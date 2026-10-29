package storagetest

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
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

// readDuringWrites calls write writes times while concurrentCallers-1 readers
// call read in a loop, and fails on the first error either returns. Each write
// waits for a read to start after the previous one, so reads overlap the whole
// workload even when a provider could finish every write before a reader runs.
func readDuringWrites(t *testing.T, writes int, write func(i int) error, read func() error) {
	t.Helper()
	var started, readers atomic.Int64
	readers.Store(concurrentCallers - 1)
	failures := make(chan error, concurrentCallers)
	written := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		defer close(written)
		for i := range writes {
			if err := write(i); err != nil {
				failures <- err
				return
			}
			for seen := started.Load(); started.Load() == seen && readers.Load() > 0; {
				runtime.Gosched()
			}
		}
	})
	for range concurrentCallers - 1 {
		wg.Go(func() {
			defer readers.Add(-1)
			for {
				select {
				case <-written:
					return
				default:
				}
				started.Add(1)
				if err := read(); err != nil {
					failures <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
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

	t.Run("Session reads see one snapshot during appends", func(t *testing.T) {
		provider := newProvider(t)
		createSessionFixture(t, provider, "tenant-a", "busy", sessionsTypeTask, time.Now().UTC())
		readDuringWrites(t, 50, func(i int) error {
			return provider.AppendMessages(t.Context(), "tenant-a", "busy", []storagev1.SessionMessage{
				{ID: fmt.Sprintf("message-%d", i), Role: "user", Content: "hello"},
			})
		}, func() error {
			session, err := provider.GetSession(t.Context(), "tenant-a", "busy")
			if err != nil {
				return err
			}
			if session.MessageCount != len(session.Messages) {
				return fmt.Errorf("session reports %d messages but returned %d", session.MessageCount, len(session.Messages))
			}
			return nil
		})
	})

	t.Run("stream heads are read from one snapshot during appends", func(t *testing.T) {
		provider := newProvider(t)
		// Enough streams that a provider reading heads in bounded batches reads
		// the first and last streams in different queries.
		streams := make([]string, 1024)
		for i := range streams {
			streams[i] = fmt.Sprintf("task-%d", i)
		}
		first, last := streams[0], streams[len(streams)-1]
		readDuringWrites(t, 25, func(int) error {
			for _, stream := range []string{first, last} {
				if _, err := provider.AppendExecutionEvent(t.Context(),
					taskEvent(eventsTenant, stream, storagev1.ExecutionEventTypeModelMessage)); err != nil {
					return err
				}
			}
			return nil
		}, func() error {
			heads, err := provider.GetLatestExecutionEventSeqs(t.Context(), eventsTenant, storagev1.ExecutionEventStreamTypeTask, streams)
			if err != nil {
				return err
			}
			// Each write appends to first before last, so no snapshot has last ahead.
			if heads[last] > heads[first] {
				return fmt.Errorf("stream %s is at %d but %s, appended first, is at %d", last, heads[last], first, heads[first])
			}
			return nil
		})
	})

	t.Run("Session event pages are read from one snapshot during appends and deletes", func(t *testing.T) {
		sessionEventPagesSnapshot(t, newProvider(t))
	})

	t.Run("a Session UID stays fenced while its cleanup completes", func(t *testing.T) {
		cleanupFenceSnapshot(t, newProvider(t))
	})

	t.Run("a renewed gateway claim returns the claim it renewed", func(t *testing.T) {
		gatewayClaimRenewalSnapshot(t, newProvider(t))
	})

	t.Run("binding a cleanup identity to a new Session never conflicts", func(t *testing.T) {
		cleanupIdentityBinding(t, newProvider(t))
	})

	t.Run("a lock acquired while its Session is deleted is held or not found", func(t *testing.T) {
		lockDuringDelete(t, newProvider(t))
	})

	t.Run("a usage observation replayed during retention is recorded", func(t *testing.T) {
		usageReplayDuringRetention(t, newProvider(t))
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

// sessionEventPagesSnapshot checks that a Session events page and its latest
// sequence come from one snapshot while streams are appended to and deleted.
func sessionEventPagesSnapshot(t *testing.T, provider storagev1.StateProvider) {
	const session = "snapshot-session"
	filter := storagev1.SessionExecutionEventFilter{Namespace: eventsTenant, SessionName: session, Limit: 50}
	// firstEventSeqs holds the Session sequence of each round's first event.
	// A round appends two events to a new stream and then deletes the stream,
	// so in any snapshot with an empty page the latest sequence is zero or a
	// round's second event, never a first event that was still stored.
	var firstEventSeqs sync.Map
	appendToSession := func(stream, eventType string) error {
		event := taskEvent(eventsTenant, stream, eventType)
		event.SessionName = session
		_, err := provider.AppendExecutionEvent(t.Context(), event)
		return err
	}
	readDuringWrites(t, 25, func(i int) error {
		stream := fmt.Sprintf("session-task-%d", i)
		if err := appendToSession(stream, storagev1.ExecutionEventTypeTaskStarted); err != nil {
			return err
		}
		_, latest, err := provider.ListSessionExecutionEvents(t.Context(), filter)
		if err != nil {
			return err
		}
		firstEventSeqs.Store(latest, struct{}{})
		if err := appendToSession(stream, storagev1.ExecutionEventTypeTaskSucceeded); err != nil {
			return err
		}
		return provider.DeleteExecutionEvents(t.Context(), eventsTenant, storagev1.ExecutionEventStreamTypeTask, stream)
	}, func() error {
		page, latest, err := provider.ListSessionExecutionEvents(t.Context(), filter)
		if err != nil {
			return err
		}
		if _, first := firstEventSeqs.Load(latest); first && len(page) == 0 {
			return fmt.Errorf("empty page reports latest sequence %d, which belongs to an event that was still stored", latest)
		}
		return nil
	})
}

// cleanupFenceSnapshot checks that a Session UID stays fenced while its
// cleanup intent is replaced by a completion.
func cleanupFenceSnapshot(t *testing.T, provider storagev1.StateProvider) {
	// prepared holds one more than the last round whose cleanup intent was
	// stored. From then on its Session UID is fenced, first by the intent and
	// then by the completion that replaces it.
	var prepared atomic.Int64
	readDuringWrites(t, 25, func(i int) error {
		name, uid := fmt.Sprintf("fenced-%d", i), fmt.Sprintf("fenced-uid-%d", i)
		if err := provider.CreateSession(t.Context(), &storagev1.SessionRecord{
			Namespace: "tenant", Name: name, SessionType: "task", CreatedAt: controlBaseTime, UpdatedAt: controlBaseTime,
		}); err != nil {
			return err
		}
		if err := provider.BindSessionCleanupIdentity(t.Context(), "tenant", name, uid); err != nil {
			return err
		}
		intent := storagev1.SessionCleanupIntent{
			Namespace: "tenant", SessionName: name, SessionUID: uid, OperationID: "delete-" + name,
			OperationDigest: controlDigest("delete-" + name), PreparedAt: controlBaseTime,
		}
		if _, err := provider.PrepareSessionCleanup(t.Context(), intent); err != nil {
			return err
		}
		prepared.Store(int64(i) + 1)
		return provider.CompleteSessionCleanup(t.Context(), storagev1.CompleteSessionCleanupRequest{
			Namespace: "tenant", SessionName: name, OperationID: intent.OperationID, OperationDigest: intent.OperationDigest,
		})
	}, func() error {
		round := prepared.Load()
		if round == 0 {
			return nil
		}
		uid := fmt.Sprintf("fenced-uid-%d", round-1)
		fenced, err := provider.HasSessionCleanupFenceForUID(t.Context(), uid)
		if err != nil {
			return err
		}
		if !fenced {
			return fmt.Errorf("session UID %s lost its cleanup fence", uid)
		}
		return nil
	})
}

// gatewayClaimRenewalSnapshot checks that a successful claim renewal returns
// the event still dispatching under its owner while claims are retried.
func gatewayClaimRenewalSnapshot(t *testing.T, provider storagev1.StateProvider) {
	base := time.Date(2026, time.May, 6, 7, 8, 9, 0, time.UTC)
	const owner = "dispatcher"
	// claimed holds one more than the last round whose event this owner holds.
	// Each round claims a new event and then retries it past base, so a
	// renewal at base either fails or returns the event still dispatching
	// under this owner.
	var claimed atomic.Int64
	readDuringWrites(t, 25, func(i int) error {
		event := gatewayEvent(base, eventsTenant, fmt.Sprintf("renew-%d", i))
		if _, _, err := provider.AdmitGatewayEvent(t.Context(), storagev1.GatewayEventAdmission{
			Event: event, AppendUserMessage: true, PendingLimit: 100,
		}); err != nil {
			return err
		}
		got, err := provider.ClaimNextGatewayEvent(t.Context(), eventsTenant, owner, base, time.Minute)
		if err != nil {
			return err
		}
		if got.ID != event.ID {
			return fmt.Errorf("claimed %s, want %s", got.ID, event.ID)
		}
		claimed.Store(int64(i) + 1)
		return provider.RetryGatewayEvent(t.Context(), eventsTenant, event.ID, owner, "retry later", base.Add(time.Hour))
	}, func() error {
		round := claimed.Load()
		if round == 0 {
			return nil
		}
		id := gatewayEvent(base, eventsTenant, fmt.Sprintf("renew-%d", round-1)).ID
		renewed, err := provider.RenewGatewayEventClaim(t.Context(), eventsTenant, id, owner, base, time.Minute)
		if errors.Is(err, storagev1.ErrConflict) {
			return nil
		}
		if err != nil {
			return err
		}
		if renewed.State != storagev1.GatewayEventDispatching || renewed.ClaimOwner != owner {
			return fmt.Errorf("renewal of %s returned state %s owned by %q", id, renewed.State, renewed.ClaimOwner)
		}
		return nil
	})
}

// cleanupIdentityBinding checks that binding a cleanup identity to a Session
// created during the call succeeds or reports the Session missing. A conflict
// matches no serial order, because nothing else binds or fences these
// Sessions.
func cleanupIdentityBinding(t *testing.T, provider storagev1.StateProvider) {
	// created holds the number of Sessions stored so far. Readers bind the next
	// one, which the writer may create while the binding runs.
	var created atomic.Int64
	readDuringWrites(t, 50, func(i int) error {
		if err := provider.CreateSession(t.Context(), &storagev1.SessionRecord{
			Namespace: "tenant", Name: fmt.Sprintf("binding-%d", i), SessionType: "task",
			CreatedAt: controlBaseTime, UpdatedAt: controlBaseTime,
		}); err != nil {
			return err
		}
		created.Store(int64(i) + 1)
		return nil
	}, func() error {
		next := created.Load()
		err := provider.BindSessionCleanupIdentity(t.Context(), "tenant",
			fmt.Sprintf("binding-%d", next), fmt.Sprintf("binding-uid-%d", next))
		if err == nil || errors.Is(err, storagev1.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("binding Session binding-%d: %w", next, err)
	})
}

// lockDuringDelete races lock acquisition against deletion of the same
// Session. Readers lock the newest Session, all as the same owner, while the
// writer creates a Session and then deletes it. Acquisition must succeed or
// report the Session missing, and a deletion must not succeed once a lock on
// the Session was granted. Any other result matches no serial order.
func lockDuringDelete(t *testing.T, provider storagev1.StateProvider) {
	const rounds = 50
	// current holds one more than the newest round whose Session was created.
	var current atomic.Int64
	var granted [rounds]atomic.Int64
	var deleted [rounds]bool
	readDuringWrites(t, rounds, func(i int) error {
		name := fmt.Sprintf("locked-%d", i)
		if err := provider.CreateSession(t.Context(), &storagev1.SessionRecord{
			Namespace: "tenant", Name: name, SessionType: "task", CreatedAt: controlBaseTime, UpdatedAt: controlBaseTime,
		}); err != nil {
			return err
		}
		current.Store(int64(i) + 1)
		err := provider.DeleteSession(t.Context(), "tenant", name)
		switch {
		case err == nil:
			deleted[i] = true
		case errors.Is(err, storagev1.ErrConflict):
			// A reader locked the Session first.
		default:
			return err
		}
		return nil
	}, func() error {
		round := current.Load()
		if round == 0 {
			return nil
		}
		name := fmt.Sprintf("locked-%d", round-1)
		err := provider.AcquireLock(t.Context(), "tenant", name, "task-"+name, "uid-"+name)
		switch {
		case err == nil:
			granted[round-1].Add(1)
			return nil
		case errors.Is(err, storagev1.ErrNotFound):
			return nil
		default:
			return fmt.Errorf("locking Session %s during its deletion: %w", name, err)
		}
	})
	for i := range rounds {
		if deleted[i] && granted[i].Load() > 0 {
			t.Fatalf("Session locked-%d was deleted although a lock on it was granted", i)
		}
	}
}

// usageReplayDuringRetention replays usage observations while retention
// removes them. The writer records an observation that is already past the
// retention cutoff and then prunes; readers replay the newest one unchanged.
// Before or after the prune, an identical replay is recorded, so any error
// matches no serial order.
func usageReplayDuringRetention(t *testing.T, provider storagev1.StateProvider) {
	input := int64(5)
	observation := func(round int64) storagev1.UsageObservation {
		return storagev1.UsageObservation{ID: fmt.Sprintf("expired-%d", round), Namespace: "tenant",
			CounterID: fmt.Sprintf("counter-%d", round), Scope: "call", Source: "agent", Model: "model",
			InputTokens: &input, Status: "running", ObservedAt: controlBaseTime}
	}
	// current holds one more than the newest round whose observation was recorded.
	var current atomic.Int64
	readDuringWrites(t, 50, func(i int) error {
		if err := provider.RecordUsage(t.Context(), observation(int64(i))); err != nil {
			return err
		}
		current.Store(int64(i) + 1)
		return provider.PruneUsage(t.Context(), controlBaseTime.Add(time.Hour))
	}, func() error {
		round := current.Load()
		if round == 0 {
			return nil
		}
		if err := provider.RecordUsage(t.Context(), observation(round-1)); err != nil {
			return fmt.Errorf("replaying usage observation expired-%d during retention: %w", round-1, err)
		}
		return nil
	})
}
