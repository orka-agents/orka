/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package repocheck

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// trackedFile is one entry in the git index.
type trackedFile struct {
	mode string
	path string
}

// repoRoot returns the repository root, two levels above this package.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// trackedFiles lists every file in the git index with its mode.
func trackedFiles(t *testing.T, root string) []trackedFile {
	t.Helper()
	cmd := exec.Command("git", "ls-files", "--stage", "-z")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v (these checks need a git checkout)", err)
	}
	var files []trackedFile
	for entry := range bytes.SplitSeq(out, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		// <mode> SP <object> SP <stage> TAB <path>
		meta, path, ok := strings.Cut(string(entry), "\t")
		if !ok {
			t.Fatalf("unexpected git ls-files entry %q", entry)
		}
		mode, _, _ := strings.Cut(meta, " ")
		files = append(files, trackedFile{mode: mode, path: path})
	}
	if len(files) == 0 {
		t.Fatal("git ls-files listed no files")
	}
	// The test cache tracks only files a test opens or stats. Stat the
	// index so staging a file invalidates a cached pass. Go skips inputs
	// outside the module root, such as a linked worktree's index, so the
	// documented command also passes -count=1.
	cmd = exec.Command("git", "rev-parse", "--path-format=absolute", "--git-path", "index")
	cmd.Dir = root
	index, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse --git-path index: %v", err)
	}
	if _, err := os.Stat(strings.TrimSpace(string(index))); err != nil {
		t.Fatalf("stat git index: %v", err)
	}
	return files
}
