//go:build linux

package main

import (
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
		return
	}
	allowed := os.Getenv("ORKA_TEST_CONFINEMENT_ALLOWED")
	outside := os.Getenv("ORKA_TEST_CONFINEMENT_OUTSIDE")
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := applyCommandWriteConfinement([]string{allowed}); err != nil {
		t.Fatalf("apply confinement: %v", err)
	}
	if err := os.WriteFile(filepath.Join(allowed, "allowed"), []byte("ok"), 0o600); err != nil {
		t.Fatalf("write allowed root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "blocked"), []byte("bad"), 0o600); err == nil {
		t.Fatal("write outside confined roots succeeded")
	}
}
