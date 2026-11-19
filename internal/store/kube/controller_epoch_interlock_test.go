package kube

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func epochLeaseKey(fence store.ControllerEpochFence) client.ObjectKey {
	return client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(fence.Name)}
}

func readEpochLease(t *testing.T, reader client.Reader, fence store.ControllerEpochFence) *coordinationv1.Lease {
	t.Helper()
	lease := &coordinationv1.Lease{}
	require.NoError(t, reader.Get(context.Background(), epochLeaseKey(fence), lease))
	return lease
}

// holdingStore is a second Store on the same API, as another controller
// process (or a restarted one) would be.
func holdingStore(t *testing.T, raw client.Client, linger, maxHold time.Duration) *Store {
	t.Helper()
	s, err := New(raw, testControlNamespace, WithAPIReader(raw), WithControllerEpochInterlockHold(linger, maxHold))
	require.NoError(t, err)
	return s
}

func successorCAS(epoch int64) store.ControllerEpochCAS {
	return store.ControllerEpochCAS{ExpectedVersion: epoch, ExpectedEpoch: epoch, NewEpoch: epoch + 1,
		HolderID: "successor", RequestDigest: testDigest("successor"), UpdatedAt: time.Now().UTC()}
}

// expireMutationToken makes the token on the Lease expired, as if the time
// a crashed or stalled holder had has passed.
func expireMutationToken(t *testing.T, raw client.Client, fence store.ControllerEpochFence) {
	t.Helper()
	lease := readEpochLease(t, raw, fence)
	require.NotEmpty(t, lease.Annotations[annotationMutationToken])
	lease.Annotations[annotationMutationExpiresAt] = formatTime(time.Now().UTC().Add(-time.Second))
	require.NoError(t, raw.Update(context.Background(), lease))
}

func waitForNoMutationToken(t *testing.T, raw client.Client, fence store.ControllerEpochFence) {
	t.Helper()
	require.Eventually(t, func() bool {
		return readEpochLease(t, raw, fence).Annotations[annotationMutationToken] == ""
	}, 5*time.Second, 5*time.Millisecond, "the held token was not removed")
}

// Consecutive writes reuse one token: only the first writes it, each write
// runs while that live token for its epoch is on the Lease, each records
// the Lease version it ran under, and the idle timer removes the token.
func TestInterlockHoldReusesOneTokenAcrossConsecutiveWrites(t *testing.T) {
	_, raw, fence := newTestStoreWithEpoch(t)
	var leaseUpdates atomic.Int32
	counted := interceptor.NewClient(raw.(client.WithWatch), interceptor.Funcs{
		Update: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			if _, ok := object.(*coordinationv1.Lease); ok {
				leaseUpdates.Add(1)
			}
			return delegate.Update(ctx, object, options...)
		},
	})
	writer, err := New(counted, testControlNamespace, WithAPIReader(raw), WithControllerEpochInterlockHold(200*time.Millisecond, 5*time.Second))
	require.NoError(t, err)
	ctx := t.Context()
	var token string
	for write := range 3 {
		_, snapshot, err := writer.requireControllerEpoch(ctx, fence)
		require.NoError(t, err)
		lease := readEpochLease(t, raw, fence)
		require.Equal(t, snapshot.MutationToken, lease.Annotations[annotationMutationToken], "write %d ran without its token on the Lease", write)
		expiresAt, err := parseTime("expiry", lease.Annotations[annotationMutationExpiresAt])
		require.NoError(t, err)
		require.True(t, expiresAt.After(time.Now().Add(time.Minute)), "write %d ran under a token near expiry", write)
		require.Equal(t, lease.ResourceVersion, snapshot.LeaseResourceVersion, "write %d does not record the Lease version it ran under", write)
		current, err := controllerEpochFromLease(fence.Name, lease)
		require.NoError(t, err)
		require.Equal(t, fence.Epoch, current.Epoch)
		if write == 0 {
			token = snapshot.MutationToken
		}
		require.Equal(t, token, snapshot.MutationToken, "write %d wrote a new token", write)
		writer.releaseControllerEpochMutation(snapshot)
	}
	require.EqualValues(t, 1, leaseUpdates.Load(), "three consecutive writes wrote the Lease more than once")
	waitForNoMutationToken(t, raw, fence)
	require.EqualValues(t, 2, leaseUpdates.Load(), "the idle token was not removed with one write")
}

// A successor cannot advance the epoch while a token is held between
// writes. Once the idle timer removes it the successor advances, and the
// next write of the batch fails closed instead of running under the old
// epoch.
func TestInterlockHoldTakeoverMidBatch(t *testing.T) {
	_, raw, fence := newTestStoreWithEpoch(t)
	owner := holdingStore(t, raw, 100*time.Millisecond, 5*time.Second)
	successor := holdingStore(t, raw, 100*time.Millisecond, 5*time.Second)
	ctx := t.Context()
	require.NoError(t, owner.WithControllerEpochMutation(ctx, fence, func(context.Context) error { return nil }))
	_, err := successor.CompareAndSwapControllerEpoch(ctx, successorCAS(fence.Epoch))
	require.ErrorIs(t, err, store.ErrConflict, "the epoch advanced past a held token")

	waitForNoMutationToken(t, raw, fence)
	_, err = successor.CompareAndSwapControllerEpoch(ctx, successorCAS(fence.Epoch))
	require.NoError(t, err)
	called := false
	err = owner.WithControllerEpochMutation(ctx, fence, func(context.Context) error { called = true; return nil })
	require.ErrorIs(t, err, store.ErrConflict)
	require.False(t, called, "a write of the old epoch ran after the takeover")
}

// The epoch changes between two writes of a batch while the first write's
// token is still held by its store, after the token expired (the holder
// stalled past its expiry). The second write sees a changed Lease, does not
// reuse the token, and fails closed.
func TestInterlockHoldEpochBumpBetweenWrites(t *testing.T) {
	_, raw, fence := newTestStoreWithEpoch(t)
	owner := holdingStore(t, raw, time.Hour, 30*time.Second)
	ctx := t.Context()
	require.NoError(t, owner.WithControllerEpochMutation(ctx, fence, func(context.Context) error { return nil }))
	expireMutationToken(t, raw, fence)
	successor := holdingStore(t, raw, time.Hour, 30*time.Second)
	_, err := successor.CompareAndSwapControllerEpoch(ctx, successorCAS(fence.Epoch))
	require.NoError(t, err)

	called := false
	err = owner.WithControllerEpochMutation(ctx, fence, func(context.Context) error { called = true; return nil })
	require.ErrorIs(t, err, store.ErrConflict)
	require.False(t, called, "the second write ran under the replaced epoch")
	lease := readEpochLease(t, raw, fence)
	current, err := controllerEpochFromLease(fence.Name, lease)
	require.NoError(t, err)
	require.Equal(t, fence.Epoch+1, current.Epoch)
	require.Empty(t, lease.Annotations[annotationMutationToken], "the failed write left a token on the new epoch")
}

// A Lease that changed while the token was held, without an epoch change,
// is not reused: the next write removes its own token, without waiting for
// it to expire, and then writes a fresh one. It never writes over its own
// token, so out-of-band Lease updates cannot extend a hold past the max hold.
func TestInterlockHoldReplacesItsTokenOnAChangedLeaseWithAGap(t *testing.T) {
	_, raw, fence := newTestStoreWithEpoch(t)
	var mu sync.Mutex
	var tokens []string
	recording := interceptor.NewClient(raw.(client.WithWatch), interceptor.Funcs{
		Update: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			err := delegate.Update(ctx, object, options...)
			if lease, ok := object.(*coordinationv1.Lease); ok && err == nil {
				mu.Lock()
				tokens = append(tokens, lease.Annotations[annotationMutationToken])
				mu.Unlock()
			}
			return err
		},
	})
	owner, err := New(recording, testControlNamespace, WithAPIReader(raw), WithControllerEpochInterlockHold(time.Hour, 30*time.Second))
	require.NoError(t, err)
	ctx := t.Context()
	_, first, err := owner.requireControllerEpoch(ctx, fence)
	require.NoError(t, err)
	owner.releaseControllerEpochMutation(first)
	lease := readEpochLease(t, raw, fence)
	lease.Annotations["orka.ai/test-unrelated"] = "changed"
	require.NoError(t, raw.Update(ctx, lease))

	_, second, err := owner.requireControllerEpoch(ctx, fence)
	require.NoError(t, err)
	require.NotEqual(t, first.MutationToken, second.MutationToken)
	require.Equal(t, second.MutationToken, readEpochLease(t, raw, fence).Annotations[annotationMutationToken])
	mu.Lock()
	require.Equal(t, []string{first.MutationToken, "", second.MutationToken}, tokens, "the token was written over instead of removed first")
	mu.Unlock()
	owner.releaseControllerEpochMutation(second)
	require.NoError(t, owner.ReleaseHeldControllerEpochInterlock(ctx))
	require.Empty(t, readEpochLease(t, raw, fence).Annotations[annotationMutationToken])
}

// A token older than the max hold is not reused: the next write removes it
// and writes a fresh one, giving a successor the same window between
// tokens as without a hold.
func TestInterlockHoldMaxHoldWritesAFreshToken(t *testing.T) {
	_, raw, fence := newTestStoreWithEpoch(t)
	owner := holdingStore(t, raw, time.Hour, 50*time.Millisecond)
	ctx := t.Context()
	_, first, err := owner.requireControllerEpoch(ctx, fence)
	require.NoError(t, err)
	owner.releaseControllerEpochMutation(first)
	time.Sleep(80 * time.Millisecond)
	_, second, err := owner.requireControllerEpoch(ctx, fence)
	require.NoError(t, err)
	require.NotEqual(t, first.MutationToken, second.MutationToken, "a token older than the max hold was reused")
	owner.releaseControllerEpochMutation(second)
	require.NoError(t, owner.ReleaseHeldControllerEpochInterlock(ctx))
}

// A process that stops right after a write, its token held, leaves the
// token to expire as a crash during a write does without a hold: the
// successor waits for the expiry and then advances, and the restarted
// process cannot write under the old epoch.
func TestInterlockHoldCrashAfterTokenHandoff(t *testing.T) {
	_, raw, fence := newTestStoreWithEpoch(t)
	crashed := holdingStore(t, raw, 50*time.Millisecond, 5*time.Second)
	ctx := t.Context()
	require.NoError(t, crashed.WithControllerEpochMutation(ctx, fence, func(context.Context) error { return nil }))
	// The process dies: its idle timer never runs.
	crashed.interlockMu.Lock()
	crashed.heldInterlock.timer.Stop()
	crashed.interlockMu.Unlock()
	time.Sleep(100 * time.Millisecond)
	require.NotEmpty(t, readEpochLease(t, raw, fence).Annotations[annotationMutationToken])

	successor := holdingStore(t, raw, 50*time.Millisecond, 5*time.Second)
	_, err := successor.CompareAndSwapControllerEpoch(ctx, successorCAS(fence.Epoch))
	require.ErrorIs(t, err, store.ErrConflict, "the epoch advanced past an unexpired token of a crashed holder")
	expireMutationToken(t, raw, fence)
	_, err = successor.CompareAndSwapControllerEpoch(ctx, successorCAS(fence.Epoch))
	require.NoError(t, err)

	restarted := holdingStore(t, raw, 50*time.Millisecond, 5*time.Second)
	called := false
	err = restarted.WithControllerEpochMutation(ctx, fence, func(context.Context) error { called = true; return nil })
	require.ErrorIs(t, err, store.ErrConflict)
	require.False(t, called)
}

// Writers in one store and in another store on the same epoch, all with a
// hold, never run at the same time, and both stores make progress.
func TestInterlockHoldConcurrentWritersAreMutuallyExclusive(t *testing.T) {
	_, raw, fence := newTestStoreWithEpoch(t)
	stores := []*Store{
		holdingStore(t, raw, 20*time.Millisecond, 30*time.Millisecond),
		holdingStore(t, raw, 20*time.Millisecond, 30*time.Millisecond),
	}
	var inside, overlaps atomic.Int32
	succeeded := make([]atomic.Int32, len(stores))
	var wg sync.WaitGroup
	for index, writer := range stores {
		for range 6 {
			wg.Go(func() {
				for range 4 {
					err := writer.WithControllerEpochMutation(t.Context(), fence, func(context.Context) error {
						if inside.Add(1) != 1 {
							overlaps.Add(1)
						}
						time.Sleep(time.Millisecond)
						inside.Add(-1)
						return nil
					})
					if err == nil {
						succeeded[index].Add(1)
					} else if !errors.Is(err, store.ErrControllerEpochMutationContention) {
						t.Errorf("store %d write: %v", index, err)
					}
				}
			})
		}
	}
	wg.Wait()
	require.Zero(t, overlaps.Load(), "two fenced writes ran at the same time")
	for index := range stores {
		require.Positive(t, succeeded[index].Load(), "store %d made no progress", index)
		require.NoError(t, stores[index].ReleaseHeldControllerEpochInterlock(t.Context()))
	}
	require.Empty(t, readEpochLease(t, raw, fence).Annotations[annotationMutationToken])
}

// The idle timer cannot strand a token: firing while the slot is busy, it
// checks again instead of giving up.
func TestInterlockHoldIdleTimerRetriesWhileTheSlotIsBusy(t *testing.T) {
	_, raw, fence := newTestStoreWithEpoch(t)
	owner := holdingStore(t, raw, 20*time.Millisecond, 5*time.Second)
	ctx := t.Context()
	_, snapshot, err := owner.requireControllerEpoch(ctx, fence)
	require.NoError(t, err)
	// Park the token but keep the slot, as between parking and freeing it.
	owner.parkInterlock(snapshot)
	time.Sleep(60 * time.Millisecond)
	require.NotEmpty(t, readEpochLease(t, raw, fence).Annotations[annotationMutationToken])
	owner.epochMutations.Release(1)
	waitForNoMutationToken(t, raw, fence)
}

// The cost of the interlock on a slow API server, before (no hold) and after
// (hold): three consecutive fenced writes, as a Session plan makes. Each GET
// is delayed 30ms and each UPDATE 110ms, the kind/gVisor medians.
func TestControllerEpochInterlockWriteCost(t *testing.T) {
	if testing.Short() {
		t.Skip("timing measurement")
	}
	const (
		getLatency    = 30 * time.Millisecond
		updateLatency = 110 * time.Millisecond
		writes        = 3
	)
	for _, mode := range []struct {
		name   string
		linger time.Duration
	}{{name: "before (no hold)"}, {name: "after (hold)", linger: DefaultControllerEpochInterlockLinger}} {
		_, raw, fence := newTestStoreWithEpoch(t)
		var gets, updates atomic.Int32
		slow := interceptor.NewClient(raw.(client.WithWatch), interceptor.Funcs{
			Get: func(ctx context.Context, delegate client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
				gets.Add(1)
				time.Sleep(getLatency)
				return delegate.Get(ctx, key, object, options...)
			},
			Update: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.UpdateOption) error {
				updates.Add(1)
				time.Sleep(updateLatency)
				return delegate.Update(ctx, object, options...)
			},
		})
		options := []Option{WithAPIReader(slow)}
		if mode.linger > 0 {
			options = append(options, WithControllerEpochInterlockHold(mode.linger, DefaultControllerEpochInterlockMaxHold))
		} else {
			options = append(options, WithControllerEpochInterlockHold(0, 0))
		}
		writer, err := New(slow, testControlNamespace, options...)
		require.NoError(t, err)
		started := time.Now()
		perWrite := make([]time.Duration, writes)
		for write := range writes {
			begin := time.Now()
			require.NoError(t, writer.WithControllerEpochMutation(t.Context(), fence, func(context.Context) error { return nil }))
			perWrite[write] = time.Since(begin)
		}
		total := time.Since(started)
		criticalGets, criticalUpdates := gets.Load(), updates.Load()
		require.NoError(t, writer.ReleaseHeldControllerEpochInterlock(t.Context()))
		t.Logf("controller epoch interlock %s: %d writes in %s (%s/write; per write %v); on the write path %d GETs + %d Lease UPDATEs (%.1f round trips/write); %d deferred UPDATE(s) after",
			mode.name, writes, total.Round(time.Millisecond), (total / writes).Round(time.Millisecond), roundDurations(perWrite),
			criticalGets, criticalUpdates, float64(criticalGets+criticalUpdates)/writes, updates.Load()-criticalUpdates)
	}
}

func roundDurations(values []time.Duration) []time.Duration {
	out := make([]time.Duration, len(values))
	for i, value := range values {
		out[i] = value.Round(time.Millisecond)
	}
	return out
}

// A token that reaches the max hold while the next write reads the Lease is
// removed before a new token is written, so continuous writes still leave a
// successor a gap between tokens.
func TestInterlockHoldTokenAgingDuringTheReadsIsRemovedFirst(t *testing.T) {
	_, raw, fence := newTestStoreWithEpoch(t)
	var slowReads atomic.Bool
	var mu sync.Mutex
	var tokens []string // the token each Lease update leaves, "" for a removal
	recording := interceptor.NewClient(raw.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, delegate client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			if slowReads.Load() {
				time.Sleep(40 * time.Millisecond)
			}
			return delegate.Get(ctx, key, object, options...)
		},
		Update: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			if lease, ok := object.(*coordinationv1.Lease); ok {
				mu.Lock()
				tokens = append(tokens, lease.Annotations[annotationMutationToken])
				mu.Unlock()
			}
			return delegate.Update(ctx, object, options...)
		},
	})
	owner, err := New(recording, testControlNamespace, WithAPIReader(recording), WithControllerEpochInterlockHold(time.Hour, 60*time.Millisecond))
	require.NoError(t, err)
	ctx := t.Context()
	_, first, err := owner.requireControllerEpoch(ctx, fence)
	require.NoError(t, err)
	owner.releaseControllerEpochMutation(first)
	// Young at the start of the next write, past the max hold after its reads.
	time.Sleep(30 * time.Millisecond)
	slowReads.Store(true)
	_, second, err := owner.requireControllerEpoch(ctx, fence)
	slowReads.Store(false)
	require.NoError(t, err)
	require.NotEqual(t, first.MutationToken, second.MutationToken)
	mu.Lock()
	recorded := append([]string(nil), tokens...)
	mu.Unlock()
	require.Equal(t, []string{first.MutationToken, "", second.MutationToken}, recorded,
		"the aged token was overwritten without being removed first")
	owner.releaseControllerEpochMutation(second)
	require.NoError(t, owner.ReleaseHeldControllerEpochInterlock(ctx))
}
