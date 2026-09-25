package kube

import (
	"context"
	"sync"

	"golang.org/x/sync/semaphore"
)

type sessionCleanupLockKey struct {
	namespace, name string
}

type sessionCleanupLock struct {
	gate *semaphore.Weighted
	refs int
}

// sessionCleanupLockSet serializes retries of one Session while runtime I/O
// releases the global epoch slot. Entries exist only while held or awaited.
type sessionCleanupLockSet struct {
	mu    sync.Mutex
	locks map[sessionCleanupLockKey]*sessionCleanupLock
}

func (s *sessionCleanupLockSet) acquire(ctx context.Context, namespace, name string) (func(), error) {
	key := sessionCleanupLockKey{namespace: namespace, name: name}
	s.mu.Lock()
	if s.locks == nil {
		s.locks = make(map[sessionCleanupLockKey]*sessionCleanupLock)
	}
	lock := s.locks[key]
	if lock == nil {
		lock = &sessionCleanupLock{gate: semaphore.NewWeighted(1)}
		s.locks[key] = lock
	}
	lock.refs++
	s.mu.Unlock()
	forget := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		lock.refs--
		if lock.refs == 0 {
			delete(s.locks, key)
		}
	}
	if err := lock.gate.Acquire(ctx, 1); err != nil {
		forget()
		return nil, err
	}
	return func() {
		lock.gate.Release(1)
		forget()
	}, nil
}
