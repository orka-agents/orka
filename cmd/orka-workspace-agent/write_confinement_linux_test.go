//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCommandWriteConfinementAllowsOnlyConfiguredRoots(t *testing.T) {
	if !commandWriteConfinementSupported() {
		t.Skip("Landlock is unavailable")
	}
	allowed := t.TempDir()
	outside := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCommandWriteConfinementHelper$")
	cmd.Env = append(os.Environ(),
		"ORKA_TEST_CONFINEMENT_HELPER=1",
		"ORKA_TEST_CONFINEMENT_ALLOWED="+allowed,
		"ORKA_TEST_CONFINEMENT_OUTSIDE="+outside,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("confinement helper: %v: %s", err, output)
	}
}

func TestCommandWriteConfinementHelper(t *testing.T) {
	if os.Getenv("ORKA_TEST_CONFINEMENT_HELPER") != "1" {
		t.Skip("helper process for TestCommandWriteConfinementAllowsOnlyConfiguredRoots")
	}
	allowed := os.Getenv("ORKA_TEST_CONFINEMENT_ALLOWED")
	outside := os.Getenv("ORKA_TEST_CONFINEMENT_OUTSIDE")
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// Confinement is irreversible and applies to this process, so the test framework's own
	// teardown (coverage flush, temp directory cleanup) would fail once it is applied and a
	// passing helper would still exit non-zero. Report the outcome and exit before returning
	// to the framework.
	if err := checkCommandWriteConfinement(allowed, outside); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func checkCommandWriteConfinement(allowed, outside string) error {
	if err := applyCommandWriteConfinement([]string{allowed}); err != nil {
		return fmt.Errorf("apply confinement: %w", err)
	}
	if err := os.WriteFile(filepath.Join(allowed, "allowed"), []byte("ok"), 0o600); err != nil {
		return fmt.Errorf("write allowed root: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "blocked"), []byte("bad"), 0o600); err == nil {
		return errors.New("write outside confined roots succeeded")
	}
	return nil
}
