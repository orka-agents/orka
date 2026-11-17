/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package supervisor

import (
	"net/http"
	"strings"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestParseIdleSessionTTL(t *testing.T) {
	for raw, want := range map[string]time.Duration{"": 0, " ": 0, "0": 0, "30m": 30 * time.Minute, "1h": time.Hour} {
		got, err := parseIdleSessionTTL(raw)
		if err != nil || got != want {
			t.Fatalf("parseIdleSessionTTL(%q) = %s, %v; want %s", raw, got, err, want)
		}
	}
	for _, raw := range []string{"soon", "-1m", "30"} {
		if _, err := parseIdleSessionTTL(raw); err == nil {
			t.Fatalf("parseIdleSessionTTL(%q) accepted", raw)
		}
	}
}

func TestConfigRejectsTooShortIdleSessionTTL(t *testing.T) {
	cfg, _ := newTestConfigWithUpstream(t, "immediate", "http://127.0.0.1:1", strings.Repeat("p", 32))
	for ttl, valid := range map[time.Duration]bool{0: true, MinIdleSessionTTL: true, time.Hour: true, time.Second: false, -time.Minute: false} {
		cfg.IdleSessionTTL = ttl
		if err := cfg.Validate(); (err == nil) != valid {
			t.Fatalf("Validate(idle TTL %s) error = %v, want valid=%v", ttl, err, valid)
		}
	}
}

func TestIdleSweepIntervalIsBounded(t *testing.T) {
	for ttl, want := range map[time.Duration]time.Duration{
		time.Minute: 15 * time.Second, 30 * time.Minute: maxIdleSweepInterval, 2 * time.Second: time.Second,
	} {
		if got := idleSweepInterval(ttl); got != want {
			t.Fatalf("idleSweepInterval(%s) = %s, want %s", ttl, got, want)
		}
	}
}

// createIdleTestSession creates one resident session on a test server whose
// idle TTL is a minute; the sweeper itself is not running, so tests drive
// sweeps with an explicit clock.
func createIdleTestSession(t *testing.T) (*Server, Config, harnessv2.CreateRuntimeSessionRequest) {
	t.Helper()
	server, cfg, profile := newTestServer(t, "immediate")
	server.cfg.IdleSessionTTL = time.Minute
	create := testCreateSessionRequest(t, cfg, profile)
	if response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg); response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	return server, cfg, create
}

func residentSession(server *Server, id harnessv2.RuntimeSessionID) *sessionState {
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.sessions[id]
}

// A session idle past the TTL is retired with proven cleanup; a fresh prompt
// for it is then refused as retryable and provably unaccepted, and the
// runtime no longer reports it, so the controller requeues and recreates it.
func TestIdleSessionIsRetiredAndItsPromptIsRetryable(t *testing.T) {
	server, cfg, create := createIdleTestSession(t)

	// Within the TTL nothing happens.
	server.retireIdleSessions(time.Now().UTC().Add(30 * time.Second))
	if state := residentSession(server, create.RuntimeSessionID); state == nil || state.drainCleanupScheduled {
		t.Fatal("a session inside its idle TTL was scheduled for retirement")
	}

	server.retireIdleSessions(time.Now().UTC().Add(2 * time.Minute))
	awaitDeletedRuntimeSession(t, server, create.RuntimeSessionID)
	server.mu.Lock()
	tombstone, ok := server.tombstones[create.Metadata.Fence.RuntimeSessionUID]
	poisoned := server.poisoned
	server.mu.Unlock()
	if !ok || !tombstone.idleRetired || tombstone.drainCleaned {
		t.Fatalf("tombstone = %+v, %v; want an idle-retired tombstone", tombstone, ok)
	}
	if poisoned {
		t.Fatal("a proven idle retirement poisoned the pool")
	}
	for _, session := range server.status().Sessions {
		if session.RuntimeSessionUID == create.Metadata.Fence.RuntimeSessionUID {
			t.Fatalf("retired session is still reported: %+v", session)
		}
	}

	prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, cfg)
	var rejection harnessv2.ErrorResponse
	decodeResponse(t, response, &rejection)
	if response.Code != http.StatusTooManyRequests || rejection.Code != harnessv2.ErrorCodeRateLimited || !rejection.Retryable ||
		!strings.Contains(rejection.Message, "idling") {
		t.Fatalf("prompt after idle retirement status=%d rejection=%+v; want a retryable rejection", response.Code, rejection)
	}

	// The same session under another fence stays a non-retryable refusal.
	stale := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	stale.Metadata.Fence.SupervisorBootID += "-previous"
	stale.Metadata.RequestDigest = ""
	sealRequest(t, &stale.Metadata.RequestDigest, stale)
	response = performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", stale, cfg)
	decodeResponse(t, response, &rejection)
	if response.Code == http.StatusTooManyRequests || rejection.Retryable || rejection.Code != harnessv2.ErrorCodeStaleFence {
		t.Fatalf("stale prompt after idle retirement status=%d rejection=%+v; want stale fence", response.Code, rejection)
	}
}

// Activity resets the idle clock, and sessions with work, an attached
// operator, or on a draining supervisor are never retired for idleness.
func TestIdleRetirementSkipsSessionsInUse(t *testing.T) {
	later := time.Now().UTC().Add(2 * time.Minute)
	for name, prepare := range map[string]func(*Server, *sessionState){
		"recent activity": func(_ *Server, state *sessionState) { state.lastActivityAt = later.Add(-30 * time.Second) },
		"attached operator": func(_ *Server, state *sessionState) {
			state.operators = map[uint64]*operatorRegistration{1: {}}
		},
		"prompt still settling": func(_ *Server, state *sessionState) { state.prompt = &promptState{} },
		"not idle":              func(_ *Server, state *sessionState) { state.descriptor.State = harnessv2.RuntimeSessionStateValidating },
		"draining supervisor":   func(server *Server, _ *sessionState) { server.drain.Requested = true },
		"native capture evidence": func(_ *Server, state *sessionState) {
			state.nativeCapture = &nativeSessionCapture{done: make(chan struct{})}
		},
	} {
		t.Run(name, func(t *testing.T) {
			server, _, create := createIdleTestSession(t)
			server.mu.Lock()
			state := server.sessions[create.RuntimeSessionID]
			prepare(server, state)
			// Keep the transition clock old so only the prepared condition
			// can hold retirement back.
			state.descriptor.LastTransitionAt = later.Add(-time.Hour)
			if name != "recent activity" {
				state.lastActivityAt = later.Add(-time.Hour)
			}
			due := server.idleRetirementDueLocked(state, later)
			server.mu.Unlock()
			if due {
				t.Fatalf("%s: session was due for idle retirement", name)
			}
		})
	}
}

// A prompt arriving after the sweep scheduled a session, but before its
// retirement ran, wins: retirement re-checks and leaves the session resident.
func TestIdleRetirementRechecksBeforeRetiring(t *testing.T) {
	server, _, create := createIdleTestSession(t)
	server.mu.Lock()
	state := server.sessions[create.RuntimeSessionID]
	state.drainCleanupScheduled = true
	state.idleRetirementScheduled = true
	state.idleRetirementActivity = sessionLastActivity(state)
	// The session was used again after the sweep scheduled it.
	state.lastActivityAt = state.idleRetirementActivity.Add(time.Second)
	server.mu.Unlock()
	server.retireResidentSession(create.RuntimeSessionID, state, true)
	if current := residentSession(server, create.RuntimeSessionID); current == nil || current.drainCleanupScheduled || current.idleRetirementScheduled {
		t.Fatal("a session used again after scheduling was retired or left scheduled")
	}
}

// While a session is scheduled for idle retirement, a prompt is refused as
// retryable, exactly as during drain cleanup.
func TestPromptForScheduledIdleRetirementIsRetryable(t *testing.T) {
	server, cfg, create := createIdleTestSession(t)
	server.mu.Lock()
	state := server.sessions[create.RuntimeSessionID]
	state.drainCleanupScheduled = true
	state.idleRetirementScheduled = true
	server.mu.Unlock()
	prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, cfg)
	var rejection harnessv2.ErrorResponse
	decodeResponse(t, response, &rejection)
	if response.Code != http.StatusTooManyRequests || !rejection.Retryable {
		t.Fatalf("prompt during idle retirement status=%d rejection=%+v; want retryable", response.Code, rejection)
	}
}

// The sweeper starts only with a TTL, and Close stops it more than once
// safely.
func TestIdleSessionSweeperLifecycle(t *testing.T) {
	server, _, _ := newTestServer(t, "immediate")
	if server.idleSweepStop != nil {
		t.Fatal("sweeper started without an idle TTL")
	}
	server.cfg.IdleSessionTTL = time.Minute
	server.startIdleSessionSweeper()
	if server.idleSweepStop == nil {
		t.Fatal("sweeper did not start with an idle TTL")
	}
	server.stopIdleSessionSweeper()
	server.stopIdleSessionSweeper()
	select {
	case <-server.idleSweepStop:
	default:
		t.Fatal("sweeper stop channel was not closed")
	}
}
