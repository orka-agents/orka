//go:build linux

package supervisor

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestDurableWorkspaceCleanupReclaimsOwnership(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to exercise distinct supervisor and agent identities")
	}
	if _, err := os.Stat(acp.DefaultExecHelperCommand); err != nil {
		t.Fatalf("install the production ACP exec helper before running this test: %v", err)
	}
	for _, action := range []string{"delete", "drain", "close"} {
		t.Run(action, func(t *testing.T) {
			// The fixture child has a distinct UID and needs traversal through
			// the test's parent directories, as it does in the runtime image.
			testRoot, err := os.MkdirTemp("", "orka-checkpoint-cleanup-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(testRoot) })
			if err := os.Chmod(testRoot, 0o711); err != nil {
				t.Fatal(err)
			}
			cfg, profile := newTestConfigWithUpstream(t, "immediate", "http://127.0.0.1:1", strings.Repeat("p", 32))
			cfg.SessionBaseDir = filepath.Join(testRoot, "sessions")
			cfg.DurableWorkspaceDir, cfg.DurableWorkspaceKey = filepath.Join(testRoot, "durable"), "workspace"
			cfg.Capabilities.Limits.MaxResidentSessions = 1
			cfg.Capabilities.Limits.MaxConcurrentPrompts = 1
			server, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_ = server.Close(ctx)
			})
			create := testCreateSessionRequest(t, cfg, profile)
			created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
			if created.Code != http.StatusCreated {
				t.Fatalf("create status=%d body=%s", created.Code, created.Body)
			}
			server.mu.Lock()
			state := server.sessions[create.RuntimeSessionID]
			paths := state.paths
			uid, gid := state.runtime.ChildIdentity()
			server.mu.Unlock()
			file := filepath.Join(paths.Workspace, "nested", "checkpoint.txt")
			if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
				t.Fatal(err)
			}
			const contents = "saved by the completed agent"
			if err := os.WriteFile(file, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := acp.FinalizeSessionOwnership(paths.Workspace, uid, gid); err != nil {
				t.Fatal(err)
			}

			switch action {
			case "delete":
				request := harnessv2.DeleteRuntimeSessionRequest{Protocol: harnessv2.ProtocolVersion, Metadata: create.Metadata, Reason: "checkpoint"}
				request.Metadata.OperationID = "delete-for-checkpoint"
				sealRequest(t, &request.Metadata.RequestDigest, request)
				response := performMutation(t, server.Handler(), http.MethodDelete, "/v2/runtime-sessions/session-1", request, cfg)
				if response.Code != http.StatusOK {
					t.Fatalf("delete status=%d body=%s", response.Code, response.Body)
				}
			case "drain":
				request := harnessv2.DrainRequest{Protocol: harnessv2.ProtocolVersion, Metadata: harnessv2.MutationMetadata{
					Fence: cfg.Fence, OperationID: "drain-for-checkpoint", RequestDigestSchemaVersion: harnessv2.RequestDigestSchemaVersion,
					ExpiresAt: time.Now().UTC().Add(time.Minute),
				}, Reason: "checkpoint"}
				sealRequest(t, &request.Metadata.RequestDigest, request)
				response := performMutation(t, server.Handler(), http.MethodPut, harnessv2.DrainPath, request, cfg)
				if response.Code != http.StatusOK {
					t.Fatalf("drain status=%d body=%s", response.Code, response.Body)
				}
				deadline := time.Now().Add(5 * time.Second)
				for {
					server.mu.Lock()
					_, resident := server.sessions[create.RuntimeSessionID]
					server.mu.Unlock()
					if !resident {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("drain did not retire the session")
					}
					time.Sleep(10 * time.Millisecond)
				}
			case "close":
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if err := server.Close(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(paths.Root); !os.IsNotExist(err) {
				t.Fatalf("ephemeral session tree survived cleanup: %v", err)
			}
			if got, err := os.ReadFile(file); err != nil || string(got) != contents {
				t.Fatalf("durable data changed during cleanup: %q, %v", got, err)
			}
			for _, path := range []string{paths.Workspace, filepath.Dir(file), file} {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				owner := info.Sys().(*syscall.Stat_t)
				if owner.Uid != uint32(os.Geteuid()) || owner.Gid != uint32(os.Getegid()) {
					t.Fatalf("durable path %s is still agent-owned: uid=%d gid=%d", path, owner.Uid, owner.Gid)
				}
			}
		})
	}
}
