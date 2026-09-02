/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"errors"
	"slices"
	"testing"
)

func TestRunValidationNetworkSandboxInstallsBeforeExec(t *testing.T) {
	originalInstall := validationNetworkSandboxInstall
	originalExec := validationNetworkSandboxExec
	t.Cleanup(func() {
		validationNetworkSandboxInstall = originalInstall
		validationNetworkSandboxExec = originalExec
	})

	installed := false
	validationNetworkSandboxInstall = func() error {
		installed = true
		return nil
	}
	wantCommand := []string{"/bin/sh", "-c", "go test ./..."}
	validationNetworkSandboxExec = func(path string, args, _ []string) error {
		if !installed {
			t.Fatal("validation command executed before the network sandbox was installed")
		}
		if path != wantCommand[0] || !slices.Equal(args, wantCommand) {
			t.Fatalf("exec path/args = %q/%#v, want %q/%#v", path, args, wantCommand[0], wantCommand)
		}
		return nil
	}

	if err := runValidationNetworkSandbox(wantCommand); err != nil {
		t.Fatalf("runValidationNetworkSandbox() error = %v", err)
	}
}

func TestRunValidationNetworkSandboxFailsClosed(t *testing.T) {
	originalInstall := validationNetworkSandboxInstall
	originalExec := validationNetworkSandboxExec
	t.Cleanup(func() {
		validationNetworkSandboxInstall = originalInstall
		validationNetworkSandboxExec = originalExec
	})

	installErr := errors.New("seccomp unavailable")
	validationNetworkSandboxInstall = func() error { return installErr }
	validationNetworkSandboxExec = func(string, []string, []string) error {
		t.Fatal("validation command executed after sandbox installation failed")
		return nil
	}

	err := runValidationNetworkSandbox([]string{"/bin/true"})
	if !errors.Is(err, installErr) {
		t.Fatalf("runValidationNetworkSandbox() error = %v, want sandbox installation failure", err)
	}
}
