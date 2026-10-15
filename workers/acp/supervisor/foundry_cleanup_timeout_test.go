package supervisor

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestSupervisorFoundryCleanupRetriesAfterRetirementDeadline(t *testing.T) {
	if runtime.GOOS != linuxGOOS {
		t.Skip("successful UID descendant cleanup requires Linux")
	}
	retirementStarted := make(chan struct{})
	allowProof := make(chan struct{})
	release := sync.OnceFunc(func() { close(allowProof) })
	defer release()
	var retirementCalls atomic.Int32
	server, cfg, create := newFoundryTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, proof := foundryTestProof(t, r)
		if r.URL.Path == "/internal/v1/retire" {
			if retirementCalls.Add(1) == 1 {
				close(retirementStarted)
			}
			select {
			case <-allowProof:
			case <-r.Context().Done():
				return
			}
			proof.RemoteSessionCreated = true
		}
		_ = json.NewEncoder(w).Encode(proof)
	}))
	// Keep the real ACP child alive until KILL so the first already-expired
	// process observation cannot race a successful TERM-based exit.
	server.cfg.Provider.Args = []string{"-test.run=^TestSupervisorFoundryCleanupTimeoutProcess$"}
	created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d", created.Code)
	}
	server.mu.Lock()
	state := server.sessions[create.RuntimeSessionID]
	server.mu.Unlock()
	deletion := harnessv2.DeleteRuntimeSessionRequest{
		Protocol: harnessv2.ProtocolVersion,
		Metadata: testMetadata(create.Metadata.Fence, "delete-after-retirement-timeout", false),
		Reason:   "retire",
	}
	sealRequest(t, &deletion.Metadata.RequestDigest, deletion)
	path := "/v2/runtime-sessions/session-1"
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	firstDone := serveMutationAsync(server.Handler(), mutationHTTPRequest(t, http.MethodDelete, path, deletion, cfg).WithContext(ctx))
	awaitSignal(t, retirementStarted, "deletion did not request exact broker retirement")
	select {
	case first := <-firstDone:
		if first.Code != http.StatusInternalServerError || ctx.Err() != context.DeadlineExceeded {
			t.Fatalf("expired retirement status = %d, context error = %v", first.Code, ctx.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expired retirement did not bound deletion")
	}
	server.mu.Lock()
	retained := server.sessions[create.RuntimeSessionID] == state && state.descriptor.State == harnessv2.RuntimeSessionStatePoisoned
	_, tombstoned := server.tombstones[create.Metadata.Fence.RuntimeSessionUID]
	server.mu.Unlock()
	if !retained || tombstoned {
		t.Fatal("unproven retirement discarded the exact session or created a tombstone")
	}
	if _, err := os.Stat(state.paths.Root); err != nil {
		t.Fatal("unproven retirement discarded the session filesystem")
	}
	select {
	case <-state.runtime.Process().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("expired deletion did not stop the exact ACP child")
	}
	release()
	retried := performMutation(t, server.Handler(), http.MethodDelete, path, deletion, cfg)
	if retried.Code != http.StatusOK {
		t.Fatalf("retirement proof could not recover the expired local observation: HTTP %d", retried.Code)
	}
	var response harnessv2.DeleteRuntimeSessionResponse
	decodeResponse(t, retried, &response)
	if err := response.Tombstone.Validate(); err != nil {
		t.Fatal(err)
	}
	if response.Tombstone.RuntimeSessionUID != create.Metadata.Fence.RuntimeSessionUID ||
		response.Tombstone.RuntimeSessionGeneration != create.Metadata.Fence.RuntimeSessionGeneration ||
		response.Tombstone.RuntimeProfileDigest != create.Metadata.Fence.RuntimeProfileDigest {
		t.Fatal("successful cleanup lost its exact session fence")
	}
	if retirementCalls.Load() != 1 {
		t.Fatal("retry repeated the still-running original retirement operation")
	}
	server.mu.Lock()
	_, resident := server.sessions[create.RuntimeSessionID]
	server.mu.Unlock()
	if resident {
		t.Fatal("proven retirement left the session resident")
	}
	if _, err := os.Stat(state.paths.Root); !os.IsNotExist(err) {
		t.Fatal("proven cleanup retained the session filesystem")
	}
	assertFoundryDeletionReplay(t, server, cfg, deletion)
	if retirementCalls.Load() != 1 {
		t.Fatal("successful deletion replay requested retirement again")
	}
}

func TestSupervisorFoundryCleanupTimeoutProcess(t *testing.T) {
	if os.Getenv("GO_WANT_SUPERVISOR_ACP_HELPER") != "1" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	TestSupervisorACPHelper(t)
}
