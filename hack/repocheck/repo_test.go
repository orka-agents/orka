/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package repocheck

import (
	"bytes"
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
	return files
}
