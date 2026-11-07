package supervisor

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestEvictionSuspendWindowRequiresSubstrateActorWithDurableWorkspace(t *testing.T) {
	identity := t.TempDir()
	for name, value := range map[string]string{"atespace": "orka", "name": "actor", "uid": "actor-uid"} {
		if err := os.WriteFile(filepath.Join(identity, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	incomplete := t.TempDir()
	if err := os.WriteFile(filepath.Join(incomplete, "uid"), []byte("actor-uid"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "absent")
	for _, test := range []struct {
		name     string
		identity string
		durable  string
		want     time.Duration
		wantErr  bool
	}{
		{name: "substrate durable workspace", identity: identity, durable: "/durable/orka-workspace", want: MaxEvictionSuspendWindow},
		{name: "substrate without durable workspace", identity: identity},
		{name: "durable workspace outside substrate", identity: missing, durable: "/durable/orka-workspace"},
		{name: "invalid identity projection", identity: incomplete, durable: "/durable/orka-workspace", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := evictionSuspendWindow(test.identity, test.durable)
			if (err != nil) != test.wantErr || got != test.want {
				t.Fatalf("evictionSuspendWindow = %s, %v; want %s, error %t", got, err, test.want, test.wantErr)
			}
		})
	}
	if MaxEvictionSuspendWindow != 30*time.Minute {
		t.Fatalf("eviction window %s differs from the Substrate worker shutdown budget", MaxEvictionSuspendWindow)
	}
}

// An evicted supervisor closes admission through the controller's drain and
// rejects every fresh prompt as a retryable, never-accepted request, so the
// controller can requeue it onto the resumed instance without replaying work.
func TestBeginEvictionDrainsAndRejectsFreshPromptsWithoutAcceptance(t *testing.T) {
	server, cfg, profile := newTestServer(t, "immediate")
	create := testCreateSessionRequest(t, cfg, profile)
	if response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg); response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	signalledAt := time.Now().UTC().Add(-time.Second)
	if !server.BeginEviction(signalledAt) {
		t.Fatal("eviction of an undrained supervisor must hold the process for suspension")
	}
	status := server.status()
	if err := status.Validate(); err != nil {
		t.Fatalf("eviction status is not protocol-valid: %v", err)
	}
	if !status.Drain.Requested || status.Drain.AcceptingNewSessions || status.Drain.Reason != harnessv2.DrainReasonWorkerEviction ||
		!status.Drain.RequestedAt.Equal(signalledAt) || status.Lifecycle != harnessv2.SupervisorLifecycleDraining {
		t.Fatalf("eviction did not self-report a worker eviction drain: %+v lifecycle=%s", status.Drain, status.Lifecycle)
	}
	prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, cfg)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("prompt during eviction status=%d body=%s; want a retryable unaccepted rejection", response.Code, response.Body.String())
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if state := server.sessions[create.RuntimeSessionID]; state != nil && state.prompt != nil {
		t.Fatal("evicted supervisor accepted a prompt")
	}
}

// A prompt sent after eviction cleanup already retired its idle session was
// never accepted, so it is rejected as retryable rather than as a missing
// session. A replay of a prompt the session already admitted keeps its
// existing classification and is never offered for retry.
func TestEvictionCleanedSessionRejectsFreshPromptAsRetryable(t *testing.T) {
	server, cfg, profile := newTestServer(t, "immediate")
	create := testCreateSessionRequest(t, cfg, profile)
	if response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg); response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	if !server.BeginEviction(time.Now().UTC()) {
		t.Fatal("eviction of an undrained supervisor must hold the process for suspension")
	}
	awaitDeletedRuntimeSession(t, server, create.RuntimeSessionID)

	fresh := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", fresh, cfg)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("fresh prompt after eviction cleanup status=%d body=%s; want a retryable unaccepted rejection", response.Code, response.Body.String())
	}
	var rejection harnessv2.ErrorResponse
	decodeResponse(t, response, &rejection)
	if rejection.Code != harnessv2.ErrorCodeRateLimited || !rejection.Retryable {
		t.Fatalf("fresh prompt after eviction cleanup rejection = %+v; want retryable rate_limited", rejection)
	}

	// The retired session had admitted this prompt before cleanup.
	server.mu.Lock()
	tombstone := server.tombstones[create.Metadata.Fence.RuntimeSessionUID]
	if !tombstone.drainCleaned {
		server.mu.Unlock()
		t.Fatal("eviction cleanup did not mark its tombstone")
	}
	tombstone.prompt = &retiredPromptSettlement{metadata: fresh.Metadata}
	server.tombstones[create.Metadata.Fence.RuntimeSessionUID] = tombstone
	server.mu.Unlock()
	replay := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", fresh, cfg)
	if replay.Code == http.StatusTooManyRequests {
		t.Fatalf("replay of an admitted prompt was offered for retry: body=%s", replay.Body.String())
	}
}

func TestBeginEvictionReleasesOnlyForControllerStopRetirement(t *testing.T) {
	for _, test := range []struct {
		reason   string
		wantHold bool
	}{
		{reason: harnessv2.DrainReasonWorkspaceRetirement, wantHold: false},
		{reason: harnessv2.DrainReasonWorkspaceSuspend, wantHold: true},
		{reason: harnessv2.DrainReasonSessionIdentityCapacity, wantHold: true},
		{reason: "rollout", wantHold: true},
	} {
		t.Run(test.reason, func(t *testing.T) {
			server, _, _ := newTestServer(t, "immediate")
			server.BeginDrain(test.reason)
			if got := server.BeginEviction(time.Now()); got != test.wantHold {
				t.Fatalf("BeginEviction after %q drain = %t, want %t", test.reason, got, test.wantHold)
			}
			if status := server.status(); status.Drain.Reason != test.reason {
				t.Fatalf("eviction rewrote the controller drain reason to %q", status.Drain.Reason)
			}
		})
	}
}

type fakeEvictionHolder struct {
	hold  bool
	calls int
}

func (f *fakeEvictionHolder) BeginEviction(time.Time) bool {
	f.calls++
	return f.hold
}

func runWatch(t *testing.T, window time.Duration, holder EvictionHolder, timer chan time.Time) (chan<- os.Signal, <-chan struct{}) {
	t.Helper()
	signals := make(chan os.Signal, 4)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	watchCtx, watchCancel := context.WithCancel(ctx)
	go func() {
		<-watchCtx.Done()
		close(done)
	}()
	var holderFn func() EvictionHolder
	if holder != nil {
		holderFn = func() EvictionHolder { return holder }
	}
	go WatchShutdownSignals(ctx, signals, window, holderFn, nil, func(time.Duration) <-chan time.Time { return timer }, watchCancel)
	return signals, done
}

func expectCancelled(t *testing.T, done <-chan struct{}, want bool) {
	t.Helper()
	select {
	case <-done:
		if !want {
			t.Fatal("shutdown started during the eviction hold")
		}
	case <-time.After(100 * time.Millisecond):
		if want {
			t.Fatal("shutdown did not start")
		}
	}
}

func TestWatchShutdownSignals(t *testing.T) {
	t.Run("no window exits on SIGTERM", func(t *testing.T) {
		holder := &fakeEvictionHolder{hold: true}
		signals, done := runWatch(t, 0, holder, make(chan time.Time))
		signals <- syscall.SIGTERM
		expectCancelled(t, done, true)
		if holder.calls != 0 {
			t.Fatal("eviction began without a configured window")
		}
	})
	t.Run("server not yet running exits on SIGTERM", func(t *testing.T) {
		signals, done := runWatch(t, time.Minute, nil, make(chan time.Time))
		signals <- syscall.SIGTERM
		expectCancelled(t, done, true)
	})
	t.Run("released hold exits promptly", func(t *testing.T) {
		signals, done := runWatch(t, time.Minute, &fakeEvictionHolder{hold: false}, make(chan time.Time))
		signals <- syscall.SIGTERM
		expectCancelled(t, done, true)
	})
	t.Run("hold survives repeated SIGTERM until the window ends", func(t *testing.T) {
		timer := make(chan time.Time, 1)
		holder := &fakeEvictionHolder{hold: true}
		signals, done := runWatch(t, time.Minute, holder, timer)
		signals <- syscall.SIGTERM
		signals <- syscall.SIGTERM
		expectCancelled(t, done, false)
		timer <- time.Now()
		expectCancelled(t, done, true)
		if holder.calls != 1 {
			t.Fatalf("eviction began %d times, want once", holder.calls)
		}
	})
	t.Run("SIGINT ends the hold", func(t *testing.T) {
		signals, done := runWatch(t, time.Minute, &fakeEvictionHolder{hold: true}, make(chan time.Time))
		signals <- syscall.SIGTERM
		expectCancelled(t, done, false)
		signals <- syscall.SIGINT
		expectCancelled(t, done, true)
	})
	t.Run("SIGINT never holds", func(t *testing.T) {
		holder := &fakeEvictionHolder{hold: true}
		signals, done := runWatch(t, time.Minute, holder, make(chan time.Time))
		signals <- syscall.SIGINT
		expectCancelled(t, done, true)
		if holder.calls != 0 {
			t.Fatal("SIGINT began an eviction hold")
		}
	})
}
