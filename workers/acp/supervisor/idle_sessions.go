/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package supervisor

import (
	"fmt"
	"strings"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// MinIdleSessionTTL is the shortest idle time after which a resident
// RuntimeSession may be retired. It keeps retirement well clear of the seconds
// between a prompt settling and the controller validating, publishing, and
// finalizing that turn on the same session.
const MinIdleSessionTTL = time.Minute

// maxIdleSweepInterval bounds how long an expired idle session waits for the
// next sweep.
const maxIdleSweepInterval = time.Minute

// parseIdleSessionTTL reads EnvIdleSessionTTL. Empty selects zero, which keeps
// idle sessions resident.
func parseIdleSessionTTL(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	ttl, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a Go duration: %w", EnvIdleSessionTTL, err)
	}
	if ttl < 0 {
		return 0, fmt.Errorf("%s must not be negative", EnvIdleSessionTTL)
	}
	return ttl, nil
}

func idleSweepInterval(ttl time.Duration) time.Duration {
	return min(max(ttl/4, time.Second), maxIdleSweepInterval)
}

// startIdleSessionSweeper retires expired idle sessions until Close. It does
// nothing when idle retirement is disabled.
func (s *Server) startIdleSessionSweeper() {
	if s.cfg.IdleSessionTTL <= 0 {
		return
	}
	s.idleSweepStop = make(chan struct{})
	interval := idleSweepInterval(s.cfg.IdleSessionTTL)
	go func(stop <-chan struct{}) {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				s.retireIdleSessions(time.Now().UTC())
			}
		}
	}(s.idleSweepStop)
}

// stopIdleSessionSweeper stops the sweeper; it is safe to call repeatedly.
func (s *Server) stopIdleSessionSweeper() {
	s.idleSweepOnce.Do(func() {
		if s.idleSweepStop != nil {
			close(s.idleSweepStop)
		}
	})
}

// retireIdleSessions schedules every expired idle session for retirement. A
// scheduled session refuses new prompts as provably unaccepted, exactly like
// a session drain cleanup retires, so the controller requeues the turn and
// recreates the session.
func (s *Server) retireIdleSessions(now time.Time) {
	type candidate struct {
		id    harnessv2.RuntimeSessionID
		state *sessionState
	}
	var candidates []candidate
	s.mu.Lock()
	for id, state := range s.sessions {
		if s.idleRetirementDueLocked(state, now) {
			state.drainCleanupScheduled = true
			state.idleRetirementScheduled = true
			state.idleRetirementActivity = sessionLastActivity(state)
			candidates = append(candidates, candidate{id: id, state: state})
		}
	}
	s.mu.Unlock()
	for _, retired := range candidates {
		go s.retireResidentSession(retired.id, retired.state, true)
	}
}

// idleRetirementDueLocked reports whether a session may be retired for
// idleness now. Only a fully created, idle session qualifies: none with a
// prompt still settling, unproven create cleanup, native capture evidence, an
// attached operator, or a pending drain cleanup, and none on a supervisor that
// is draining or not ready, whose own lifecycle governs its sessions. The
// caller must hold s.mu.
func (s *Server) idleRetirementDueLocked(state *sessionState, now time.Time) bool {
	if state == nil || state.drainCleanupScheduled || !s.idleRetirableLocked(state) {
		return false
	}
	return now.Sub(sessionLastActivity(state)) >= s.cfg.IdleSessionTTL
}

// idleRetirementStillDueLocked re-checks a session the sweep scheduled, just
// before its retirement starts: it must still qualify and have had no
// activity since it was scheduled. The caller must hold s.mu.
func (s *Server) idleRetirementStillDueLocked(state *sessionState) bool {
	return state != nil && s.idleRetirableLocked(state) && !sessionLastActivity(state).After(state.idleRetirementActivity)
}

// idleRetirableLocked reports whether a session's state allows idle
// retirement, apart from how long it has idled. The caller must hold s.mu.
func (s *Server) idleRetirableLocked(state *sessionState) bool {
	if s.cfg.IdleSessionTTL <= 0 || s.drain.Requested || s.lifecycle != harnessv2.SupervisorLifecycleReady {
		return false
	}
	if state.creating || state.runtime == nil || len(state.operators) > 0 {
		return false
	}
	if state.descriptor.State != harnessv2.RuntimeSessionStateIdle || (state.prompt != nil && state.prompt.settlement == nil) {
		return false
	}
	return state.createCleanupUnproven == nil && state.nativeInstallUnresolved == nil && !retainsNativeCaptureEvidence(state)
}

// sessionLastActivity is the latest of the session's last state transition
// and its last recorded controller operation or operator activity.
func sessionLastActivity(state *sessionState) time.Time {
	last := state.descriptor.LastTransitionAt
	if state.lastActivityAt.After(last) {
		last = state.lastActivityAt
	}
	return last
}
