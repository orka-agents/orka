/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRun_Success(t *testing.T) {
	os.Args = []string{"worker", "echo", "hello"}
	err := run()
	if err != nil {
		t.Errorf("run() returned error: %v", err)
	}
}

func TestRun_NoCommand(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	os.Args = []string{"worker"}
	os.Unsetenv("ORKA_COMMAND") //nolint:errcheck
	err := run()
	if err == nil {
		t.Error("run() should return error when no command specified")
	}
}

func TestRun_CommandFromEnv(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	os.Args = []string{"worker"}
	os.Setenv("ORKA_COMMAND", "echo hello") //nolint:errcheck
	defer os.Unsetenv("ORKA_COMMAND")       //nolint:errcheck

	err := run()
	if err != nil {
		t.Errorf("run() returned error: %v", err)
	}
}

func TestWorkspaceRootUsesSubPath(t *testing.T) {
	t.Setenv("ORKA_WORKSPACE_SUBPATH", "src")
	if got := workspaceRoot(); got != filepath.Join(workspaceDir, "src") {
		t.Fatalf("workspaceRoot() = %q", got)
	}
}

func TestRun_CommandNotFound(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	// run() calls os.Exit for exec failures, so we test the underlying exec
	os.Args = []string{"worker", "nonexistent_command_12345"}
	err := run()
	if err == nil {
		t.Error("run() should return error for nonexistent command")
	}
	if _, ok := err.(*exec.Error); !ok {
		t.Errorf("expected *exec.Error, got %T", err)
	}
}
