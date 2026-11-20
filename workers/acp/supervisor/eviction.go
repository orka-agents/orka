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

// MaxEvictionSuspendWindow is the Agent Substrate worker shutdown budget. The
// worker kills whatever is still running once it elapses, so a longer hold
// would only mask that kill.
const MaxEvictionSuspendWindow = harnessv2.WorkerEvictionSuspendWindow

// EvictionSuspendWindow returns how long a SIGTERM may hold the process for a
// data-only suspension, or zero for the default exit-on-SIGTERM behavior.
//
// The hold applies only to a supervisor that is an Agent Substrate Actor
// (its read-only SystemInfo identity projection is present) and that hosts a
// data-only suspendable workspace (a durable workspace directory is set). Its
// worker forwards its own SIGTERM and then allows a bounded window in which
// the Actor must be suspended; a process that exits in that window cannot be.
// No template variable selects it: the native backend admits few environment
// entries, and the identity projection cannot be forged from the template.
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

// EvictionHolder is the server-side half of the eviction hold.
type EvictionHolder interface {
	BeginEviction(now time.Time) bool
}

// BeginEviction handles the host worker's termination signal. It closes
// admission through the same drain the controller uses, so new prompts are
// rejected as provably unaccepted and idle sessions are released, and reports
// whether the process must stay alive for a data-only suspension.
//
// The only drain that releases the hold is a controller-requested
// Stop/Delete retirement: that instance's data is being discarded, and its
// provider deletes the worker while it still runs. Every other state, including
// an in-progress controller suspension drain, holds so the Actor remains
// suspendable. Active prompts are never interrupted or replayed here; they
// either settle before the controller authorizes suspension or are lost with
// the instance once the window ends.
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
	// The eviction is reported on its own, whatever drain closed admission
	// first: Core enforces the eviction deadline from this timestamp, and a
	// suspension or capacity drain's reason would otherwise hide it.
	if s.drain.WorkerEvictionAt.IsZero() {
		s.drain.WorkerEvictionAt = now.UTC()
	}
	s.mu.Unlock()
	s.startDrainCleanup(cleanup)
	return true
}

// WatchShutdownSignals converts process signals into cancellation of the
// supervisor's serve context. Without an eviction window, or before the server
// exists, the first SIGTERM or SIGINT cancels immediately. With a window, a
// SIGTERM that the server accepts as an eviction holds the process until the
// window elapses or SIGINT arrives; repeated SIGTERMs do not shorten it.
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
