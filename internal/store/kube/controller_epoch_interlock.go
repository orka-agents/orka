package kube

import (
	"context"
	"time"

	"github.com/orka-agents/orka/internal/store"
	coordinationv1 "k8s.io/api/coordination/v1"
)

// Controller-epoch interlock hold (ADR 0058).
//
// Every fenced control-store write runs under the controller-epoch mutation
// interlock: a mutation token written with a compare-and-swap onto the
// authoritative epoch Lease. While a live token is on the Lease no epoch
// change can commit, because CompareAndSwapControllerEpoch refuses a live
// token and any token write changes the Lease's resourceVersion.
//
// Without a hold, each write writes a token and then removes it: two Lease
// updates around every write. With a hold, a store keeps its token on the
// Lease between consecutive writes, for at most interlockMaxHold after the
// token was written and interlockLinger after the last write. The next write
// still reads the Lease and the ControllerEpoch object and runs every check
// a token write runs, and reuses the token only when the Lease is exactly
// the state this store's token write produced: same UID, same
// resourceVersion, same token, same epoch and holder. Otherwise it writes a
// token as before. A held token is removed by the idle timer, by the next
// write once it is older than interlockMaxHold, and before this store
// changes the epoch itself.

const (
	// DefaultControllerEpochInterlockLinger is how long a store keeps its
	// interlock token after a write for the next write to reuse.
	DefaultControllerEpochInterlockLinger = 500 * time.Millisecond
	// DefaultControllerEpochInterlockMaxHold bounds how long one token is
	// reused after it was written.
	DefaultControllerEpochInterlockMaxHold = 5 * time.Second
	// maxControllerEpochInterlockHold keeps every reused token far inside the
	// two-minute token expiry, with the 20-second mutation bound and clock
	// skew to spare.
	maxControllerEpochInterlockHold = 30 * time.Second
	// controllerEpochMutationTokenTTL is the expiry written with a token.
	controllerEpochMutationTokenTTL = 2 * time.Minute
)

// WithControllerEpochInterlockHold lets consecutive fenced writes of this
// store reuse its controller-epoch interlock token (ADR 0058): a token is
// kept for linger after a write and reused for at most maxHold after it was
// written. A zero linger disables the hold.
func WithControllerEpochInterlockHold(linger, maxHold time.Duration) Option {
	return func(s *Store) error {
		if linger < 0 || maxHold < 0 {
			return store.ValidationErrorf("controller epoch interlock hold durations must not be negative")
		}
		if linger > 0 && (maxHold <= 0 || maxHold > maxControllerEpochInterlockHold) {
			return store.ValidationErrorf("controller epoch interlock max hold must be between 1ns and %s", maxControllerEpochInterlockHold)
		}
		s.interlockLinger = linger
		s.interlockMaxHold = maxHold
		return nil
	}
}

// heldEpochInterlock is a token this store left on the epoch Lease after a
// write. generation identifies the parking that armed timer.
type heldEpochInterlock struct {
	snapshot   epochSnapshot
	generation uint64
	timer      *time.Timer
}

// interlockHoldEnabled reports whether this store holds tokens between writes.
func (s *Store) interlockHoldEnabled() bool {
	return s.interlockLinger > 0 && s.epochMutations != nil
}

// interlockReusable reports whether a token written at writtenAt may still
// cover a new write.
func (s *Store) interlockReusable(snapshot epochSnapshot) bool {
	return snapshot.MutationToken != "" && snapshot.MutationLease != nil && !snapshot.TokenWrittenAt.IsZero() &&
		time.Since(snapshot.TokenWrittenAt) < s.interlockMaxHold
}

// heldInterlockFor takes the parked token for a write under fence. A token
// for another fence, or too old to reuse, is removed first, as an immediate
// release would have removed it. The caller holds the local mutation slot.
func (s *Store) heldInterlockFor(fence store.ControllerEpochFence) *epochSnapshot {
	held := s.takeHeldInterlock()
	if held != nil && (held.Name != fence.Name || held.Epoch != fence.Epoch ||
		held.HolderID != fence.HolderID || !s.interlockReusable(*held)) {
		s.removeMutationToken(*held)
		return nil
	}
	return held
}

// heldInterlockState is what a freshly read, fully checked Lease says about
// a held token.
type heldInterlockState int

const (
	// heldInterlockAbsent: no held token, or the Lease carries another one.
	heldInterlockAbsent heldInterlockState = iota
	// heldInterlockReusable: the Lease is exactly the state this store's
	// token write produced, so no epoch change and no other token write has
	// committed since, and the token stays live for the next write.
	heldInterlockReusable
	// heldInterlockRemove: the Lease carries this store's token but it
	// cannot be reused: it reached the max hold (possibly during the reads)
	// or the Lease changed since it was written. It is removed before a new
	// token is written, never overwritten, so every token's age counts from
	// a gap and a successor always gets the gap between tokens it gets
	// between unheld writes.
	heldInterlockRemove
)

func (s *Store) heldInterlockOn(held *epochSnapshot, lease *coordinationv1.Lease) heldInterlockState {
	switch {
	case held == nil || lease.Annotations[annotationMutationToken] != held.MutationToken:
		return heldInterlockAbsent
	case s.interlockReusable(*held) && lease.UID == held.MutationLease.UID && lease.ResourceVersion == held.LeaseResourceVersion:
		return heldInterlockReusable
	default:
		return heldInterlockRemove
	}
}

// takeHeldInterlock removes and returns the parked token, if any, and stops
// its idle timer. The caller holds the local mutation slot.
func (s *Store) takeHeldInterlock() *epochSnapshot {
	s.interlockMu.Lock()
	defer s.interlockMu.Unlock()
	if s.heldInterlock == nil {
		return nil
	}
	held := s.heldInterlock
	s.heldInterlock = nil
	if held.timer != nil {
		held.timer.Stop()
	}
	snapshot := held.snapshot
	return &snapshot
}

// parkInterlock keeps snapshot's token on the Lease for the next write and
// arms the idle timer that removes it. The caller holds the local slot.
func (s *Store) parkInterlock(snapshot epochSnapshot) {
	s.interlockMu.Lock()
	defer s.interlockMu.Unlock()
	if s.heldInterlock != nil && s.heldInterlock.timer != nil {
		s.heldInterlock.timer.Stop()
	}
	s.interlockGeneration++
	generation := s.interlockGeneration
	snapshot.LocalMutationSlot = false
	s.heldInterlock = &heldEpochInterlock{snapshot: snapshot, generation: generation}
	s.heldInterlock.timer = time.AfterFunc(s.interlockLinger, func() { s.releaseIdleInterlock(generation) })
}

// releaseIdleInterlock removes the parked token once no write has reused it
// within the linger. A write in progress takes or re-parks the token itself.
func (s *Store) releaseIdleInterlock(generation uint64) {
	if !s.epochMutations.TryAcquire(1) {
		// The slot is busy: a write will take the token, or the write that
		// parked it has not yet freed the slot. Check again shortly.
		s.interlockMu.Lock()
		if s.heldInterlock != nil && s.heldInterlock.generation == generation {
			s.heldInterlock.timer.Reset(min(s.interlockLinger, 50*time.Millisecond))
		}
		s.interlockMu.Unlock()
		return
	}
	defer s.epochMutations.Release(1)
	s.interlockMu.Lock()
	if s.heldInterlock == nil || s.heldInterlock.generation != generation {
		s.interlockMu.Unlock()
		return
	}
	snapshot := s.heldInterlock.snapshot
	s.heldInterlock = nil
	s.interlockMu.Unlock()
	s.removeMutationToken(snapshot)
}

// ReleaseHeldControllerEpochInterlock removes a token this store is holding
// between writes, waiting for a write in progress. Call it before the
// process stops so a successor does not wait for the token to expire.
func (s *Store) ReleaseHeldControllerEpochInterlock(ctx context.Context) error {
	if s == nil || !s.interlockHoldEnabled() {
		return nil
	}
	if err := s.epochMutations.Acquire(ctx, 1); err != nil {
		return err
	}
	defer s.epochMutations.Release(1)
	if held := s.takeHeldInterlock(); held != nil {
		s.removeMutationToken(*held)
	}
	return nil
}
