package acp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRuntimeSessionLoadRequiresSupportAndNeverCreatesFallback(t *testing.T) {
	for _, mode := range []string{"load-success", "load-error", "load-unsupported"} {
		t.Run(mode, func(t *testing.T) {
			uid, gid := os.Getuid(), os.Getgid()
			if uid == 0 {
				uid, gid = 65534, 65534
			}
			root, err := os.MkdirTemp("", "orka-acp-load-test-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			if err := os.Chmod(root, 0o755); err != nil {
				t.Fatal(err)
			}
			paths, err := PrepareSessionPaths(filepath.Join(root, "sessions"), "load-session")
			if err != nil {
				t.Fatal(err)
			}
			if err := FinalizeSessionOwnership(paths.Root, uid, gid); err != nil {
				t.Fatal(err)
			}
			env, err := BuildChildEnvironment(paths, EnvironmentConfig{Values: map[string]string{"GO_WANT_ACP_HELPER": "1", "ACP_HELPER_MODE": mode}})
			if err != nil {
				t.Fatal(err)
			}
			const nativeID = "01970b26-25ad-71ef-bd22-9374ec0b741b"
			command, helperCommand := testAdapterCommand(t), testExecHelperCommand(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			session, err := NewRuntimeSession(ctx, RuntimeSessionConfig{
				ID: "load-session", Generation: 1, ProfileDigest: "sha256:profile", LoadSessionID: nativeID,
				MCPServers:  []MCPServer{{Name: "current-mcp", Type: "http", URL: "http://current.example/mcp"}},
				Process:     ProcessConfig{Command: command, Args: []string{"-test.run=TestACPHelperProcess"}, Environment: env, Paths: paths, UID: uid, GID: gid, ExecHelperCommand: helperCommand},
				CancelGrace: 100 * time.Millisecond,
			})
			if mode != "load-success" {
				if err == nil {
					_, _ = session.Delete(ctx)
					t.Fatal("unsupported or failed native load succeeded")
				}
				want := "load ACP provider session"
				if mode == "load-unsupported" {
					want = "did not advertise session/load"
				}
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("native load error = %v, want %s", err, want)
				}
				initialization, ok := errors.AsType[*InitializationError](err)
				if !ok || initialization.RuntimeSession() == nil || session != nil {
					t.Fatalf("failed load lost its runtime cleanup handle: %v", err)
				}
				if mode == "load-error" {
					if rpc, ok := errors.AsType[*RPCError](err); !ok || rpc.Code != -32602 {
						t.Fatalf("failed load lost its RPC cause: %v", err)
					}
				}
				failedSession := initialization.RuntimeSession()
				if _, err := failedSession.StartPrompt(ctx, "after-failed-load", "digest", []ContentBlock{Text("continue")}); err == nil {
					t.Fatal("failed initialization admitted a prompt")
				}
				if initialization.Cleanup.Proven != (runtime.GOOS == "linux") {
					t.Fatalf("initialization cleanup proof = %#v", initialization.Cleanup)
				}
				cleanup, stopErr := failedSession.Delete(ctx)
				if runtime.GOOS == "linux" && (stopErr != nil || !cleanup.Proven) {
					t.Fatalf("retained cleanup retry: %#v %v", cleanup, stopErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _, _ = session.Delete(ctx) }()
			if session.ProviderSessionID() != nativeID {
				t.Fatalf("loaded provider ID = %q", session.ProviderSessionID())
			}
			run, err := session.StartPrompt(ctx, "continue", "digest", []ContentBlock{Text("continue")})
			if err != nil {
				t.Fatal(err)
			}
			for event := range run.Events {
				if event.Update != nil && strings.Contains(string(event.Update.Update), "imported history") {
					t.Fatal("session/load history entered the fresh prompt")
				}
			}
			if result := <-run.Result; result.Outcome != PromptOutcomeCompleted {
				t.Fatalf("continued load outcome = %#v", result)
			}
		})
	}
}
