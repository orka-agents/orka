package scriptstest

import (
	"bytes"
	"context"
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

//go:embed testdata/workspace-lifecycle/sandbox.sh
var sandboxFixture string

//go:embed testdata/workspace-lifecycle/substrate.sh
var substrateFixture string

func workspaceScript(t *testing.T, name string) string {
	t.Helper()
	root := os.Getenv("WORKSPACE_LIFECYCLE_SOURCE_ROOT")
	if root == "" {
		root = filepath.Join("..", "..")
	}
	data, err := os.ReadFile(filepath.Join(root, "scripts", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func sourceSection(t *testing.T, source, start, end string) string {
	t.Helper()
	begin := strings.Index(source, start)
	if begin < 0 {
		t.Fatalf("missing cleanup start: %s", start)
	}
	finish := strings.Index(source[begin:], end)
	if finish < 0 {
		t.Fatalf("missing cleanup end: %s", end)
	}
	return source[begin : begin+finish]
}

func withoutLastLine(t *testing.T, body string) string {
	t.Helper()
	end := strings.LastIndex(body, "\n")
	if end < 0 {
		t.Fatal("cleanup fragment has no newline")
	}
	return body[:end]
}

type cleanupResult struct {
	calls  []string
	err    error
	stderr string
}

func executeCleanup(t *testing.T, body, scenario, fixture string) cleanupResult {
	t.Helper()
	for _, name := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatalf("workspace cleanup tests require %s", name)
		}
	}
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "clock"), []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "calls"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "bash", "-c", fixture+"\nexercise() {\n"+body+"\n}\nexercise\n")
	command.Env = append(os.Environ(), "TEST_DIR="+folder, "SCENARIO="+scenario)
	command.WaitDelay = time.Second
	var stderr bytes.Buffer
	command.Stderr = &stderr
	err := command.Run()
	if ctx.Err() != nil {
		t.Fatalf("cleanup fixture timed out: %s", stderr.String())
	}
	data, readErr := os.ReadFile(filepath.Join(folder, "calls"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	return cleanupResult{calls: strings.Fields(string(data)), err: err, stderr: stderr.String()}
}

func requireOutcome(t *testing.T, result cleanupResult, success bool) {
	t.Helper()
	if (result.err == nil) != success {
		t.Fatalf("unexpected cleanup outcome: %v, stderr: %s", result.err, result.stderr)
	}
}

func requireBefore(t *testing.T, calls []string, first, second string) {
	t.Helper()
	a, b := slices.Index(calls, first), slices.Index(calls, second)
	if a < 0 || b < 0 || a >= b {
		t.Fatalf("%s must precede %s: %v", first, second, calls)
	}
}

func requireNoCalls(t *testing.T, calls []string, forbidden ...string) {
	t.Helper()
	for _, name := range forbidden {
		if slices.Contains(calls, name) {
			t.Fatalf("unexpected %s: %v", name, calls)
		}
	}
}

func requireNoArchival(t *testing.T, calls []string) {
	t.Helper()
	for _, call := range calls {
		if strings.HasPrefix(call, "archive:") {
			t.Fatalf("premature archival: %v", calls)
		}
	}
}

func TestCancellationEvidencePrecedesArchivalAndFinalizerWait(t *testing.T) {
	source := workspaceScript(t, "live-agent-sandbox-e2e.sh")
	start := "  # Release the observer only"
	if strings.Index(source, "  # No-replay proof:") < strings.Index(source, start) {
		start = "  # No-replay proof:"
	}
	body := sourceSection(t, source, start, `  if [[ -n "${cancel_pool}" ]]; then`)
	body = strings.ReplaceAll(body, "-n orka-system", "-n fixture")
	for _, scenario := range []string{"success", "replay", "no-disconnect", "archive-conflict", "finalizer-retained"} {
		t.Run(scenario, func(t *testing.T) {
			result := executeCleanup(t, body, scenario, sandboxFixture)
			requireOutcome(t, result, scenario == "success")
			if scenario == "success" {
				requireBefore(t, result.calls, "fixture-disconnect", "archive:orka-ws-lc-cancel-session")
				requireBefore(t, result.calls, "archive:orka-ws-lc-cancel-session", "observer-release")
			} else {
				requireNoCalls(t, result.calls, "observer-release", "task-absence")
				if scenario == "replay" || scenario == "no-disconnect" {
					requireNoArchival(t, result.calls)
				}
			}
		})
	}
}

func TestFinalCleanupIncludesUnknownSessionsBeforeTaskAbsence(t *testing.T) {
	source := workspaceScript(t, "live-agent-sandbox-e2e.sh")
	body := sourceSection(t, source,
		"  # Session archival retains", `delete runtimepool "${pool_name}" --ignore-not-found=true`)
	body = strings.ReplaceAll(withoutLastLine(t, body), "-n orka-system", "-n fixture")
	for _, scenario := range []string{"success", "archive-conflict"} {
		t.Run(scenario, func(t *testing.T) {
			result := executeCleanup(t, body, scenario, sandboxFixture)
			requireOutcome(t, result, scenario == "success")
			if scenario == "success" {
				want := []string{"archive:orka-ws-lc-session", "archive:orka-ws-lc-timeout-session",
					"archive:fixture-ambiguous", "archive:fixture-restart", "task-absence"}
				if !slices.Equal(result.calls, want) {
					t.Fatalf("incorrect final cleanup: %v", result.calls)
				}
			} else {
				requireNoCalls(t, result.calls, "task-absence")
			}
		})
	}
}

func TestInterruptedRunCancelsThenArchivesBeforeWaiting(t *testing.T) {
	source := workspaceScript(t, "live-agent-sandbox-e2e.sh")
	body := sourceSection(t, source, "  # Request cancellation first", "delete agent orka-ws-lc-agent")
	body = strings.ReplaceAll(withoutLastLine(t, body), "-n orka-system", "-n fixture")
	for _, scenario := range []string{"success", "archive-conflict"} {
		t.Run(scenario, func(t *testing.T) {
			result := executeCleanup(t, body, scenario, sandboxFixture)
			requireOutcome(t, result, scenario == "success")
			if len(result.calls) == 0 || result.calls[0] != "cancellation-request" {
				t.Fatalf("cancellation was not first: %v", result.calls)
			}
			if scenario == "success" {
				archives := 0
				for _, call := range result.calls {
					if strings.HasPrefix(call, "archive:") {
						archives++
					}
				}
				if archives != 5 || result.calls[len(result.calls)-1] != "task-absence" {
					t.Fatalf("incomplete cleanup: %v", result.calls)
				}
			} else {
				requireNoCalls(t, result.calls, "task-absence")
			}
		})
	}
}

func TestNativeCancellationSettlesAndDisconnectsBeforeArchival(t *testing.T) {
	source := workspaceScript(t, "agent-substrate-e2e.sh")
	body := sourceSection(t, source, "  submit_task native-cancel", "  cleanup_acp_workspaces")
	for _, scenario := range []string{
		"success", "unsettled", "no-disconnect", "not-suspended", "active-actor", "archive-conflict",
	} {
		t.Run(scenario, func(t *testing.T) {
			result := executeCleanup(t, body, scenario, substrateFixture)
			requireOutcome(t, result, scenario == "success")
			if scenario == "success" {
				want := []string{"cancellation-request", "settlement", "provider-disconnect", "workspace-suspended",
					"actors-checked", "archive:cancel-session", "task-absence"}
				if !slices.Equal(result.calls, want) {
					t.Fatalf("incorrect native cancellation: %v", result.calls)
				}
			} else {
				requireNoCalls(t, result.calls, "task-absence")
				if scenario != "archive-conflict" {
					requireNoArchival(t, result.calls)
				}
			}
		})
	}
}

func TestNativeSourceSessionsArchiveBeforeWorkspaceDeletion(t *testing.T) {
	source := workspaceScript(t, "agent-substrate-e2e.sh")
	end := `  wait_absent executionworkspace "${workspace}"`
	for _, session := range []string{"native-session", "native-data-session"} {
		body := sourceSection(t, source, "  delete_native_session "+session+"\n", end) + end + "\n"
		for _, scenario := range []string{"success", "archive-conflict"} {
			t.Run(session+"/"+scenario, func(t *testing.T) {
				result := executeCleanup(t, body, scenario, substrateFixture)
				requireOutcome(t, result, scenario == "success")
				want := []string{"archive:" + session}
				if scenario == "success" {
					want = append(want, "workspace-delete", "executionworkspace-absence")
				}
				if !slices.Equal(result.calls, want) {
					t.Fatalf("incorrect workspace deletion: %v", result.calls)
				}
			})
		}
	}
}
