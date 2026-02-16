/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package uiembed

import (
	"io/fs"
	"testing"
)

func TestFS(t *testing.T) {
	fsys, err := FS()
	if err != nil {
		t.Fatalf("FS() error = %v", err)
	}
	if fsys == nil {
		t.Fatal("FS() returned nil")
	}
}

func TestFS_ContainsIndexHTML(t *testing.T) {
	fsys, err := FS()
	if err != nil {
		t.Fatalf("FS() error = %v", err)
	}

	// The dist/ directory should contain at least index.html
	f, err := fsys.Open("index.html")
	if err != nil {
		t.Fatalf("Open(index.html) error = %v", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.IsDir() {
		t.Error("index.html should not be a directory")
	}
}

func TestFS_ReadDir(t *testing.T) {
	fsys, err := FS()
	if err != nil {
		t.Fatalf("FS() error = %v", err)
	}

	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatalf("ReadDir(.) error = %v", err)
	}
	if len(entries) == 0 {
		t.Error("expected at least one entry in dist/")
	}

	// Verify index.html is among entries
	found := false
	for _, e := range entries {
		if e.Name() == "index.html" {
			found = true
			break
		}
	}
	if !found {
		t.Error("index.html not found in directory listing")
	}
}
