package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const MaxEvictionSuspendWindow = harnessv2.WorkerEvictionSuspendWindow

func EvictionSuspendWindow() (time.Duration, error) {
	return evictionSuspendWindow(harnessv2.SubstrateIdentityDirectory, os.Getenv(EnvDurableWorkspaceDir))
}

func evictionSuspendWindow(identityDirectory, durableWorkspaceDir string) (time.Duration, error) {
	if strings.TrimSpace(durableWorkspaceDir) == "" {
		return 0, nil
	}
	identity, err := harnessv2.ReadSubstrateActorIdentity(identityDirectory)
	if err != nil {
		return 0, fmt.Errorf("substrate identity projection: %w", err)
	}
	if identity == nil {
		return 0, nil
	}
	return MaxEvictionSuspendWindow, nil
}

type EvictionHolder interface {
	BeginEviction(now time.Time) bool
}

func (s *Server) BeginEviction(now time.Time) bool {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	s.mu.Lock()
	if s.drain.Requested && s.drain.Reason == harnessv2.DrainReasonWorkspaceRetirement {
		s.mu.Unlock()
		return false
	}
	var cleanup []drainCleanupCandidate
	if !s.drain.Requested {
		cleanup = s.beginDrainLocked(harnessv2.DrainReasonWorkerEviction, now.UTC())
	}
	if s.drain.WorkerEvictionAt.IsZero() {
		s.drain.WorkerEvictionAt = now.UTC()
	}
	s.mu.Unlock()
	s.startDrainCleanup(cleanup)
	return true
}

func WatchShutdownSignals(
	ctx context.Context,
	signals <-chan os.Signal,
	window time.Duration,
	holder func() EvictionHolder,
	now func() time.Time,
	after func(time.Duration) <-chan time.Time,
	cancel context.CancelFunc,
) {
	defer cancel()
	if now == nil {
		now = time.Now
	}
	if after == nil {
		after = time.After
	}
	var first os.Signal
	select {
	case <-ctx.Done():
		return
	case first = <-signals:
	}
	if first != syscall.SIGTERM || window <= 0 || holder == nil {
		return
	}
	server := holder()
	if server == nil || !server.BeginEviction(now().UTC()) {
		return
	}
	slog.Info("worker eviction signalled; holding for data-only suspension", "window", window.String())
	deadline := after(window)
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			slog.Warn("worker eviction window elapsed without suspension; shutting down")
			return
		case sig := <-signals:
			if sig != syscall.SIGTERM {
				return
			}
		}
	}
}
