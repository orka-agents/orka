package security

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenRepoRegularFileRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := openRepoRegularFile(root, "../secret.txt"); err == nil {
		t.Fatal("openRepoRegularFile() error = nil, want traversal rejection")
	}
}

func TestOpenRepoRegularFileRejectsEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "outside.txt")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	if _, _, err := openRepoRegularFile(root, "link.txt"); err == nil {
		t.Fatal("openRepoRegularFile() error = nil, want escaping symlink rejection")
	}
}

func TestOpenRepoRegularFileRejectsInRepoSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "target.txt"), []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	if _, _, err := openRepoRegularFile(root, "link.txt"); err == nil {
		t.Fatal("openRepoRegularFile() error = nil, want final symlink rejection")
	}
}
