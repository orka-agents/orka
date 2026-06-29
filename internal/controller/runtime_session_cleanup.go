package controller

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/sozercan/orka/internal/harness"
)

const DefaultRuntimeSessionCleanupInterval = 5 * time.Minute

// RuntimeSessionCleanupLoop periodically removes expired inactive runtime
// sessions from the internal RuntimeSessionStore. It is intentionally provider-
// neutral: provider-specific daemon/workspace teardown should happen before a
// session is marked inactive, while this loop owns the persisted lifecycle
// cleanup boundary.
type runtimeSessionCleanupStore interface {
	CleanupIdleRuntimeSessions(context.Context, time.Time) ([]harness.RuntimeSession, error)
}

type RuntimeSessionCleanupLoop struct {
	Store    runtimeSessionCleanupStore
	Interval time.Duration
	Now      func() time.Time
}

func (l *RuntimeSessionCleanupLoop) Start(ctx context.Context) error {
	interval := l.interval()
	if interval <= 0 || l.Store == nil {
		<-ctx.Done()
		return nil
	}

	// Run once at startup so an operator restart does not wait a full interval
	// before clearing stale inactive runtimes.
	l.RunOnce(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			l.RunOnce(ctx)
		}
	}
}

func (l *RuntimeSessionCleanupLoop) RunOnce(ctx context.Context) {
	if l == nil || l.Store == nil {
		return
	}
	logger := log.FromContext(ctx).WithName("runtime-session-cleanup")
	deleted, err := l.Store.CleanupIdleRuntimeSessions(ctx, l.now())
	if err != nil {
		logger.Error(err, "failed to cleanup expired runtime sessions")
		return
	}
	if len(deleted) > 0 {
		logger.Info("cleaned up expired runtime sessions", "count", len(deleted))
	}
}

func (l *RuntimeSessionCleanupLoop) interval() time.Duration {
	if l == nil {
		return DefaultRuntimeSessionCleanupInterval
	}
	if l.Interval == 0 {
		return DefaultRuntimeSessionCleanupInterval
	}
	return l.Interval
}

func (l *RuntimeSessionCleanupLoop) now() time.Time {
	if l != nil && l.Now != nil {
		return l.Now().UTC()
	}
	return time.Now().UTC()
}
