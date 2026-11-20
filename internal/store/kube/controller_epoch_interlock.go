package kube

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/store"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	// controllerEpochFencedWriteBound ends every fenced write's context this
	// long after the token covering it was written, whatever context its
	// caller passed: a write never outlives its token, and other processes
	// judging the expiry by their own clocks have the other half of the
	// token's life as margin for clock skew.
	controllerEpochFencedWriteBound = controllerEpochMutationTokenTTL / 2
	// controllerEpochTakeoverRequestTTL is how long a successor's takeover
	// request stops the holder writing new tokens; a waiting successor
	// refreshes it after controllerEpochTakeoverRequestRefresh.
	controllerEpochTakeoverRequestTTL     = 30 * time.Second
	controllerEpochTakeoverRequestRefresh = 10 * time.Second
	// controllerEpochTokenRemovalAttempts bounds an explicit release's
	// attempts to remove a held token; the idle timer keeps retrying after.
	controllerEpochTokenRemovalAttempts = 5
	// controllerEpochTokenRemovalTimeout caps one removal attempt.
	controllerEpochTokenRemovalTimeout  = 5 * time.Second
	maxControllerEpochTokenRemovalDelay = 5 * time.Second
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
// write, or failed to remove. generation identifies the parking that armed
// timer; retryDelay is the wait before the next removal attempt after one
// failed.
type heldEpochInterlock struct {
	snapshot   epochSnapshot
	generation uint64
	timer      *time.Timer
	retryDelay time.Duration
}

// fencedWriteBound is how long after its token was written a fenced write's
// context ends.
func (s *Store) fencedWriteBound() time.Duration {
	if s.interlockWriteBound > 0 {
		return s.interlockWriteBound
	}
	return controllerEpochFencedWriteBound
}

// interlockHoldEnabled reports whether this store holds tokens between writes.
func (s *Store) interlockHoldEnabled() bool {
	return s.interlockLinger > 0 && s.epochMutations != nil
}

// interlockReusable reports whether a token written at writtenAt may still
// cover a new write.
func (s *Store) interlockReusable(snapshot epochSnapshot) bool {
	return snapshot.MutationToken != "" && snapshot.MutationLease != nil && !snapshot.TokenWrittenAt.IsZero() && !snapshot.removalPending &&
		time.Since(snapshot.TokenWrittenAt) < s.interlockMaxHold
}

// heldInterlockFor takes the parked token for a write under fence. A token
// held for another fence, possibly on another epoch Lease, is removed first;
// a store holds one token at a time, so if that fails the write fails and
// the token stays parked for retry. The caller holds the local slot.
func (s *Store) heldInterlockFor(ctx context.Context, fence store.ControllerEpochFence) (*epochSnapshot, error) {
	held := s.takeHeldInterlock()
	if held == nil || (held.Name == fence.Name && held.Epoch == fence.Epoch && held.HolderID == fence.HolderID) {
		return held, nil
	}
	if err := s.removeOrRetainInterlock(ctx, *held); err != nil {
		return nil, err
	}
	return nil, nil
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

func (s *Store) heldInterlockOn(held *epochSnapshot, fence store.ControllerEpochFence, lease *coordinationv1.Lease) heldInterlockState {
	switch {
	case held == nil || lease.Annotations[annotationMutationToken] != held.MutationToken:
		return heldInterlockAbsent
	case held.Name == fence.Name && held.Epoch == fence.Epoch && held.HolderID == fence.HolderID &&
		s.interlockReusable(*held) && lease.UID == held.MutationLease.UID && lease.ResourceVersion == held.LeaseResourceVersion:
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
	s.parkInterlockFor(snapshot, s.interlockLinger, 0)
}

// parkInterlockFor parks snapshot with its idle timer due after delay;
// retryDelay is nonzero for a token whose removal failed.
func (s *Store) parkInterlockFor(snapshot epochSnapshot, delay, retryDelay time.Duration) {
	s.interlockMu.Lock()
	defer s.interlockMu.Unlock()
	if s.heldInterlock != nil && s.heldInterlock.timer != nil {
		s.heldInterlock.timer.Stop()
	}
	s.interlockGeneration++
	generation := s.interlockGeneration
	snapshot.LocalMutationSlot = false
	snapshot.cancelWrite = nil
	s.heldInterlock = &heldEpochInterlock{snapshot: snapshot, generation: generation, retryDelay: retryDelay}
	s.heldInterlock.timer = time.AfterFunc(delay, func() { s.releaseIdleInterlock(generation) })
}

// removeOrRetainInterlock removes snapshot's token. If the removal fails,
// the store keeps the token parked and its idle timer retries the removal
// with backoff until the token would have expired anyway, so a transient
// API failure does not leave it for a successor to wait out. The caller
// holds the local slot.
func (s *Store) removeOrRetainInterlock(ctx context.Context, snapshot epochSnapshot) error {
	err := s.removeMutationToken(ctx, snapshot)
	if err != nil {
		s.retainForRemoval(snapshot, 0)
	}
	return err
}

// retainForRemoval parks a token whose removal failed after previous, the
// last retry delay, unless it has expired by now.
func (s *Store) retainForRemoval(snapshot epochSnapshot, previous time.Duration) {
	if !s.interlockHoldEnabled() || snapshot.TokenWrittenAt.IsZero() ||
		time.Since(snapshot.TokenWrittenAt) >= controllerEpochMutationTokenTTL {
		return
	}
	delay := max(min(2*previous, maxControllerEpochTokenRemovalDelay), 100*time.Millisecond)
	snapshot.removalPending = true
	s.parkInterlockFor(snapshot, delay, delay)
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
	snapshot, retryDelay := s.heldInterlock.snapshot, s.heldInterlock.retryDelay
	s.heldInterlock = nil
	s.interlockMu.Unlock()
	// The idle timer has no caller: each retry is bounded on its own.
	if s.removeMutationToken(context.Background(), snapshot) != nil {
		s.retainForRemoval(snapshot, retryDelay)
	}
}

// ReleaseHeldControllerEpochInterlock removes a token this store is holding
// between writes, waiting for a write in progress. Call it before the
// process stops so a successor does not wait for the token to expire. A
// removal that keeps failing is retried a bounded number of times and then
// reported; the token stays parked so the idle timer keeps retrying.
func (s *Store) ReleaseHeldControllerEpochInterlock(ctx context.Context) error {
	if s == nil || !s.interlockHoldEnabled() {
		return nil
	}
	if err := s.epochMutations.Acquire(ctx, 1); err != nil {
		return err
	}
	defer s.epochMutations.Release(1)
	held := s.takeHeldInterlock()
	if held == nil {
		return nil
	}
	delay := 100 * time.Millisecond
	var err error
	for attempt := range controllerEpochTokenRemovalAttempts {
		if err = s.removeMutationToken(ctx, *held); err == nil {
			return nil
		}
		if attempt == controllerEpochTokenRemovalAttempts-1 {
			break
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			s.retainForRemoval(*held, delay)
			return errors.Join(err, ctx.Err())
		case <-timer.C:
		}
		delay = min(2*delay, maxControllerEpochTokenRemovalDelay)
	}
	s.retainForRemoval(*held, delay)
	return err
}

// controllerEpochTakeoverLeaseName names the Lease on which a successor
// waiting to change the epoch asks the token holder to stop writing new
// tokens. Only successors write it, so the request never competes with the
// holder's writes to the epoch Lease (ADR 0058).
func controllerEpochTakeoverLeaseName(name string) string {
	return objectName(controllerEpochTakeoverLeasePrefix, name)
}

// ControllerEpochTakeoverLeaseName is the name of the Lease holding takeover
// requests for the controller epoch name.
func ControllerEpochTakeoverLeaseName(name string) string {
	return controllerEpochTakeoverLeaseName(name)
}

// takeoverObservation is a takeover request version this store has seen,
// and when it first saw it, on its own monotonic clock.
type takeoverObservation struct {
	uid             types.UID
	resourceVersion string
	firstSeen       time.Time
}

func (s *Store) takeoverRequestTTL() time.Duration {
	if s.interlockTakeoverRequestTTL > 0 {
		return s.interlockTakeoverRequestTTL
	}
	return controllerEpochTakeoverRequestTTL
}

func (s *Store) wallNow() time.Time {
	if s.wallClock != nil {
		return s.wallClock()
	}
	return time.Now()
}

// controllerEpochTakeoverWaiting reports whether a successor's request to
// replace epoch is in force, and who made it. A request version is in force
// for the takeover request TTL after this store first saw it, judged on this
// store's own monotonic clock, never on the time the successor wrote into
// it, so neither side's wall-clock skew can make a request last forever or
// not at all. A successor that keeps trying rewrites it, which makes a new
// version; one that stops loses its effect within the TTL (ADR 0058).
func (s *Store) controllerEpochTakeoverWaiting(ctx context.Context, name string, epoch int64) (bool, string, error) {
	request := &coordinationv1.Lease{}
	key := types.NamespacedName{Namespace: s.controlNamespace, Name: controllerEpochTakeoverLeaseName(name)}
	if err := s.readClient().Get(ctx, key, request); apierrors.IsNotFound(err) {
		return false, "", nil
	} else if err != nil {
		return false, "", mapKubernetesError("get controller epoch takeover request", err)
	}
	by := strings.TrimSpace(request.Annotations[annotationEpochTakeoverRequestedBy])
	if by == "" || request.Annotations[annotationEpochTakeoverFromEpoch] != strconv.FormatInt(epoch, 10) {
		return false, "", nil
	}
	s.interlockMu.Lock()
	defer s.interlockMu.Unlock()
	if s.takeoverSeen.uid != request.UID || s.takeoverSeen.resourceVersion != request.ResourceVersion {
		s.takeoverSeen = takeoverObservation{uid: request.UID, resourceVersion: request.ResourceVersion, firstSeen: time.Now()}
	}
	return time.Since(s.takeoverSeen.firstSeen) < s.takeoverRequestTTL(), by, nil
}

// requestControllerEpochTakeover records that holder is waiting to replace
// epoch, so the token holder writes no new token and the epoch Lease is free
// within one of its holds (ADR 0058). The request is rewritten, making a new
// version, whenever this store's own last write of it is
// controllerEpochTakeoverRequestRefresh old on its monotonic clock, or the
// request is not the version it wrote. It is best effort: a request that
// does not commit only leaves the successor retrying as before.
func (s *Store) requestControllerEpochTakeover(ctx context.Context, name string, epoch int64, holder string) {
	key := types.NamespacedName{Namespace: s.controlNamespace, Name: controllerEpochTakeoverLeaseName(name)}
	request := &coordinationv1.Lease{}
	err := s.readClient().Get(ctx, key, request)
	if err != nil && !apierrors.IsNotFound(err) {
		return
	}
	exists := err == nil
	s.interlockMu.Lock()
	written := s.takeoverWritten
	s.interlockMu.Unlock()
	if exists && written.uid == request.UID && written.resourceVersion == request.ResourceVersion &&
		time.Since(written.firstSeen) < controllerEpochTakeoverRequestRefresh {
		return
	}
	if !exists {
		request = &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Labels: controlLabels(name)}}
	}
	if request.Annotations == nil {
		request.Annotations = map[string]string{}
	}
	request.Annotations[annotationLogicalName] = name
	request.Annotations[annotationEpochTakeoverRequestedBy] = holder
	request.Annotations[annotationEpochTakeoverFromEpoch] = strconv.FormatInt(epoch, 10)
	// Diagnostic only: no store judges a request by this time.
	request.Annotations[annotationEpochTakeoverRequestedAt] = formatTime(s.wallNow().UTC())
	sent := time.Now()
	if exists {
		err = s.client.Update(ctx, request)
	} else {
		err = s.client.Create(ctx, request)
	}
	if err == nil {
		s.interlockMu.Lock()
		s.takeoverWritten = takeoverObservation{uid: request.UID, resourceVersion: request.ResourceVersion, firstSeen: sent}
		s.interlockMu.Unlock()
	}
}

// PublishControllerEpochTakeoverIntent records that holder is waiting to
// replace the current epoch of name, learned from a single read of the epoch
// Lease. That read cannot conflict, so the request is published even while
// a busy holder's token changes the Lease between any two reads, as the
// mirror checks of an epoch read or change do (ADR 0058).
//
// A positive expectedEpoch is the epoch the caller's change replaces: the
// request is published only while the Lease still carries it, so a stale
// change that has already lost, or one that committed with an ambiguous
// result, cannot hold off the epoch that now stands. Zero means the current
// epoch, for a caller that holds none yet; it may be a predecessor's that
// shares the caller's holder ID, as after a restart that keeps its identity.
// Nothing is published before the epoch exists. It is best effort.
func (s *Store) PublishControllerEpochTakeoverIntent(ctx context.Context, name, holder string, expectedEpoch int64) {
	if s == nil || s.client == nil {
		return
	}
	holder = strings.TrimSpace(holder)
	normalized, err := store.NormalizeControllerEpochName(name)
	if err != nil || holder == "" {
		return
	}
	lease := &coordinationv1.Lease{}
	key := types.NamespacedName{Namespace: s.controlNamespace, Name: controllerEpochLeaseName(normalized)}
	if err := s.readClient().Get(ctx, key, lease); err != nil {
		return
	}
	epoch, err := parsePositiveInt64("controller epoch", lease.Annotations[annotationControllerEpoch])
	if err != nil || (expectedEpoch > 0 && epoch != expectedEpoch) {
		return
	}
	s.requestControllerEpochTakeover(ctx, normalized, epoch, holder)
}

// clearControllerEpochTakeover deletes holder's request to replace
// replacedEpoch once that change committed. A newer request, against the
// epoch that change created, is left alone even when it shares the holder
// ID, as a restart's does. A request left behind names an older epoch and is
// ignored.
func (s *Store) clearControllerEpochTakeover(ctx context.Context, name, holder string, replacedEpoch int64) {
	key := types.NamespacedName{Namespace: s.controlNamespace, Name: controllerEpochTakeoverLeaseName(name)}
	request := &coordinationv1.Lease{}
	if err := s.readClient().Get(ctx, key, request); err != nil || request.Annotations[annotationEpochTakeoverRequestedBy] != holder ||
		request.Annotations[annotationEpochTakeoverFromEpoch] != strconv.FormatInt(replacedEpoch, 10) {
		return
	}
	_ = s.client.Delete(ctx, request, client.Preconditions{UID: &request.UID, ResourceVersion: &request.ResourceVersion})
}
