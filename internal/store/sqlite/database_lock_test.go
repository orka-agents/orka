//go:build darwin || linux

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenLockedStoreExcludesOverlappingProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orka.db")
	first, err := OpenLockedStore(path)
	if err != nil {
		t.Fatalf("OpenLockedStore(first): %v", err)
	}

	if _, err := OpenLockedStore(path); err == nil || !strings.Contains(err.Error(), "process lock") {
		t.Fatalf("OpenLockedStore(second) error = %v, want process lock conflict", err)
	}

	if err := first.close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}
	second, err := OpenLockedStore(path)
	if err != nil {
		t.Fatalf("OpenLockedStore(after release): %v", err)
	}
	if err := second.close(); err != nil {
		t.Fatalf("close second store: %v", err)
	}
}

func TestOpenLockedStoreRejectsNonFilesystemPaths(t *testing.T) {
	for _, path := range []string{"", "  ", ":memory:"} {
		t.Run(path, func(t *testing.T) {
			if _, err := OpenLockedStore(path); err == nil {
				t.Fatalf("OpenLockedStore(%q) error = nil", path)
			}
		})
	}
}

func TestLockedStoreRunnableRequiresLeaderElection(t *testing.T) {
	store, err := OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	if err != nil {
		t.Fatalf("OpenLockedStore: %v", err)
	}
	if !store.NeedLeaderElection() {
		t.Fatal("NeedLeaderElection() = false, want true")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Start(ctx); err != nil {
		t.Fatalf("Start(cancelled): %v", err)
	}
}
