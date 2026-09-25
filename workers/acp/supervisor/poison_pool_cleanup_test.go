package supervisor

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

type cleanupFailurePeers struct {
	server      *Server
	cfg         Config
	create      harnessv2.CreateRuntimeSessionRequest
	failed      *sessionState
	idle        *sessionState
	failedCalls atomic.Int32
	peerCalls   atomic.Int32
	peerStarted chan struct{}
	releasePeer func()
}

func newCleanupFailurePeers(t *testing.T) *cleanupFailurePeers {
	t.Helper()
	const peerUID harnessv2.RuntimeSessionUID = "idle-peer-uid"
	fixture := &cleanupFailurePeers{peerStarted: make(chan struct{})}
	peerMayFail := make(chan struct{})
	fixture.releasePeer = sync.OnceFunc(func() { close(peerMayFail) })
	broker := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner, _ := foundryTestProof(t, r)
		if r.URL.Path != "/internal/v1/retire" {
			t.Errorf("unexpected broker request: %s", r.URL.Path)
		}
		if owner.Owner.RuntimeSessionUID == peerUID {
			if fixture.peerCalls.Add(1) == 1 {
				close(fixture.peerStarted)
			}
			select {
			case <-peerMayFail:
			case <-r.Context().Done():
				return
			}
		} else {
			fixture.failedCalls.Add(1)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	fixture.server, fixture.cfg, fixture.create = newFoundryTestServer(t, broker)
	server, cfg, create := fixture.server, fixture.cfg, fixture.create
	t.Cleanup(fixture.releasePeer)
	if response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg); response.Code != http.StatusCreated {
		t.Fatalf("create original session status = %d", response.Code)
	}
	// Seed a second initialized resident without requiring a non-root
	// test process to assign another OS UID. The two fixture servers
	// have the same harness fence and independent local processes.
	peerServer, peerCfg, peer := newFoundryTestServer(t, broker)
	t.Cleanup(fixture.releasePeer)
	if harnessv2.CompareFence(cfg.Fence, peerCfg.Fence, false) != harnessv2.FenceMatch {
		t.Fatal("peer fixture does not share the original pool fence")
	}
	peer.RuntimeSessionID = "idle-peer"
	peer.Metadata.Fence.RuntimeSessionUID = peerUID
	peer.Metadata.OperationID = "create-idle-peer"
	sealRequest(t, &peer.Metadata.RequestDigest, peer)
	if response := performMutation(t, peerServer.Handler(), http.MethodPut, "/v2/runtime-sessions/idle-peer", peer, peerCfg); response.Code != http.StatusCreated {
		t.Fatalf("create idle peer status = %d", response.Code)
	}
	peerServer.mu.Lock()
	fixture.idle = peerServer.sessions[peer.RuntimeSessionID]
	delete(peerServer.sessions, peer.RuntimeSessionID)
	peerServer.mu.Unlock()
	server.mu.Lock()
	fixture.failed = server.sessions[create.RuntimeSessionID]
	server.sessions[peer.RuntimeSessionID] = fixture.idle
	server.mu.Unlock()
	return fixture
}

func TestSupervisorCleanupFailureDrainsIdlePeerOnce(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		name := "task delete"
		if automatic {
			name = "automatic cleanup"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newCleanupFailurePeers(t)
			server, cfg, create := fixture.server, fixture.cfg, fixture.create
			failed, idle := fixture.failed, fixture.idle
			server.mu.Lock()
			if automatic {
				failed.drainCleanupScheduled = true
			}
			server.mu.Unlock()
			if automatic {
				server.cleanupDrainedSession(failed.id, failed)
			} else {
				deletion := harnessv2.DeleteRuntimeSessionRequest{
					Protocol: harnessv2.ProtocolVersion, Metadata: testMetadata(create.Metadata.Fence, "delete-original", false), Reason: "task cleanup",
				}
				sealRequest(t, &deletion.Metadata.RequestDigest, deletion)
				if response := performMutation(t, server.Handler(), http.MethodDelete, "/v2/runtime-sessions/session-1", deletion, cfg); response.Code != http.StatusInternalServerError {
					t.Fatalf("unproven deletion status = %d", response.Code)
				}
			}

			select {
			case <-fixture.peerStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("poisoned pool left its idle peer without a cleanup attempt")
			}
			server.mu.Lock()
			if failed.descriptor.State != harnessv2.RuntimeSessionStatePoisoned || failed.drainCleanupScheduled ||
				idle.descriptor.State != harnessv2.RuntimeSessionStateDeleting || !idle.drainCleanupScheduled {
				server.mu.Unlock()
				t.Fatal("pool poison retried the failed resident or lost ownership of peer cleanup")
			}
			server.mu.Unlock()
			fixture.releasePeer()
			deadline := time.Now().Add(5 * time.Second)
			for {
				server.mu.Lock()
				returned := idle.descriptor.State == harnessv2.RuntimeSessionStatePoisoned && !idle.drainCleanupScheduled
				server.mu.Unlock()
				if returned {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("failed peer cleanup did not return for an authenticated retry")
				}
				time.Sleep(time.Millisecond)
			}

			// Neither another cleanup failure nor a repeated pool poison may
			// recursively reschedule residents whose cleanup already failed.
			server.poisonPool("later_cleanup_failure")
			server.mu.Lock()
			defer server.mu.Unlock()
			if fixture.failedCalls.Load() != 1 || fixture.peerCalls.Load() != 1 || failed.drainCleanupScheduled || idle.drainCleanupScheduled ||
				failed.descriptor.State != harnessv2.RuntimeSessionStatePoisoned || idle.descriptor.State != harnessv2.RuntimeSessionStatePoisoned {
				t.Fatal("poisoned pool automatically retried unproven cleanup")
			}
			if !server.poisoned || !server.drain.Requested || server.drain.AcceptingNewSessions || server.lifecycle != harnessv2.SupervisorLifecycleTerminating {
				t.Fatal("cleanup failure reopened pool admission")
			}
			if len(server.tombstones) != 0 {
				t.Fatal("unproven cleanup discarded a resident")
			}
		})
	}
}

func TestPoisonPoolPreservesPeerCleanupEligibility(t *testing.T) {
	idle := &sessionState{descriptor: harnessv2.RuntimeSessionDescriptor{State: harnessv2.RuntimeSessionStateIdle}}
	active := &sessionState{descriptor: harnessv2.RuntimeSessionDescriptor{State: harnessv2.RuntimeSessionStatePromptRunning}, prompt: &promptState{}}
	creating := &sessionState{descriptor: harnessv2.RuntimeSessionDescriptor{State: harnessv2.RuntimeSessionStateCreating}, creating: true}
	deleting := &sessionState{descriptor: harnessv2.RuntimeSessionDescriptor{State: harnessv2.RuntimeSessionStateDeleting}, drainCleanupScheduled: true}
	finalizing := &sessionState{descriptor: harnessv2.RuntimeSessionDescriptor{State: harnessv2.RuntimeSessionStateFinalizing}}
	server := &Server{sessions: map[harnessv2.RuntimeSessionID]*sessionState{
		"idle": idle, "active": active, "creating": creating, "deleting": deleting, "finalizing": finalizing,
	}}
	server.mu.Lock()
	defer server.mu.Unlock()
	server.poisonPoolLocked("peer_cleanup_failed")
	if !idle.drainCleanupScheduled {
		t.Fatal("eligible idle peer was not scheduled")
	}
	for name, state := range server.sessions {
		if name == "idle" {
			continue
		}
		if state.drainCleanupScheduled != (state == deleting) {
			t.Fatalf("pool poison changed cleanup ownership for %s", name)
		}
	}
	if active.prompt.settlement != nil || active.descriptor.State != harnessv2.RuntimeSessionStatePromptRunning ||
		creating.descriptor.State != harnessv2.RuntimeSessionStateCreating || deleting.descriptor.State != harnessv2.RuntimeSessionStateDeleting ||
		finalizing.descriptor.State != harnessv2.RuntimeSessionStateFinalizing {
		t.Fatal("pool poison bypassed an existing session transition")
	}
}
