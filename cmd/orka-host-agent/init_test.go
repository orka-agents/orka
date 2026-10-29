/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// initHelperEnv makes the test binary run as "orka-host-agent init" instead of
// running tests, so each test gets an init process of its own.
const initHelperEnv = "ORKA_HOST_AGENT_TEST_INIT"

func TestMain(m *testing.M) {
	if os.Getenv(initHelperEnv) == "1" {
		code, err := runInit(os.Args[1:])
		if err != nil {
			_, _ = os.Stderr.WriteString(err.Error() + "\n")
			os.Exit(125)
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// initProcess returns an init process that runs a shell script.
func initProcess(t *testing.T, script string, args ...string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], append([]string{"--", "/bin/sh", "-c", script, "sh"}, args...)...)
	cmd.Env = append(os.Environ(), initHelperEnv+"=1")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	return cmd, &output
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.ExitCode()
	default:
		t.Fatal(err)
		return -1
	}
}

func TestInitReturnsTheCommandsExitCode(t *testing.T) {
	for script, want := range map[string]int{
		"exit 0":        0,
		"exit 3":        3,
		"kill -KILL $$": 128 + int(syscall.SIGKILL),
	} {
		cmd, output := initProcess(t, script)
		if got := exitCode(t, cmd.Run()); got != want {
			t.Errorf("%q: init exited %d, want %d; output: %s", script, got, want, output)
		}
	}
}

func TestInitForwardsSignalsAndWaitsForTheCommand(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	// The command takes its time to stop and exits with its own status.
	cmd, output := initProcess(t, `trap 'sleep 0.3; exit 7' TERM; : > "$1"; while :; do sleep 0.05; done`, ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the command never started; output: %s", output)
		}
	}
	// A second signal while the command is stopping must not end init early.
	for range 2 {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := exitCode(t, cmd.Wait()); got != 7 {
		t.Fatalf("init exited %d, want the command's 7; output: %s", got, output)
	}
}

func TestInitRefusesAMissingCommand(t *testing.T) {
	for _, args := range [][]string{nil, {"--"}} {
		if _, err := runInit(args); err == nil {
			t.Errorf("runInit(%q) started nothing without an error", args)
		}
	}
}
