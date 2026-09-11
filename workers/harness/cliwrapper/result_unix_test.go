//go:build !windows

package cliwrapper

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestFinalizeTurnResultHonorsContextWhenGitIndexBlocks(t *testing.T) {
	repo := resultTestRepoWithFIFOIndex(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	_, err := FinalizeTurnResult(ctx, repo, "result")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("FinalizeTurnResult error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("FinalizeTurnResult blocked for %v after context deadline", elapsed)
	}
}

func TestCleanFinalizedWorkDirHonorsContextWhenGitIndexBlocks(t *testing.T) {
	repo := resultTestRepoWithFIFOIndex(t)
	canonicalRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatalf("canonicalize repository path: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	err = CleanFinalizedWorkDir(ctx, canonicalRepo)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CleanFinalizedWorkDir error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("CleanFinalizedWorkDir blocked for %v after context deadline", elapsed)
	}
}

func resultTestRepoWithFIFOIndex(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runResultTestGit(t, repo, "init")
	runResultTestGit(t, repo, "config", "user.email", "test@example.invalid")
	runResultTestGit(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runResultTestGit(t, repo, "add", "tracked.txt")
	runResultTestGit(t, repo, "commit", "-m", "initial")

	indexPath := filepath.Join(repo, ".git", "index")
	if err := os.Remove(indexPath); err != nil {
		t.Fatalf("remove Git index: %v", err)
	}
	if err := unix.Mkfifo(indexPath, 0o600); err != nil {
		t.Fatalf("replace Git index with FIFO: %v", err)
	}
	return repo
}
