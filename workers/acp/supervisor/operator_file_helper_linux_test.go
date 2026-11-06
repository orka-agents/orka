//go:build linux

package supervisor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func runFileHelper(t *testing.T, args []string, stdin io.Reader) (operatorFileStatus, []byte) {
	t.Helper()
	var stdout bytes.Buffer
	RunOperatorFileHelper(args, stdin, &stdout)
	reader := bufio.NewReader(&stdout)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("helper wrote no status line: %q", stdout.String())
	}
	var status operatorFileStatus
	if err := json.Unmarshal(line, &status); err != nil {
		t.Fatal(err)
	}
	rest, _ := io.ReadAll(reader)
	return status, rest
}

func TestOperatorFileHelperReadsRegularFilesBeneathTheWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, content := runFileHelper(t, []string{OperatorFileHelperRead, root, "src/main.go", "1024"}, nil)
	if !status.OK || status.Size != 13 || string(content) != "package main\n" {
		t.Fatalf("read = %+v %q", status, content)
	}
	status, _ = runFileHelper(t, []string{OperatorFileHelperRead, root, "src/main.go", "4"}, nil)
	if status.OK || status.Code != operatorFileCodeTooLarge {
		t.Fatalf("oversized read must be refused: %+v", status)
	}
	status, _ = runFileHelper(t, []string{OperatorFileHelperRead, root, "src", "1024"}, nil)
	if status.OK || status.Code != operatorFileCodeNotRegular {
		t.Fatalf("directory read must be refused: %+v", status)
	}
	status, _ = runFileHelper(t, []string{OperatorFileHelperRead, root, "missing", "1024"}, nil)
	if status.OK || status.Code != operatorFileCodeNotFound {
		t.Fatalf("missing file must be not_found: %+v", status)
	}
}

func TestOperatorFileHelperRefusesTraversalAndSymlinks(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Symlink(secret, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dirlink")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"../secret", "a/../../secret", "/etc/passwd", "link", "dirlink/secret", "./link"} {
		status, content := runFileHelper(t, []string{OperatorFileHelperRead, root, rel, "1024"}, nil)
		if status.OK || strings.Contains(string(content), "secret") {
			t.Fatalf("path %q escaped the workspace: %+v %q", rel, status, content)
		}
	}
	// Writes through a symlinked directory or onto a symlink never touch the target.
	for _, rel := range []string{"dirlink/new", "link"} {
		status, _ := runFileHelper(t, []string{OperatorFileHelperWrite, root, rel, "3"}, strings.NewReader("pwn"))
		if status.OK {
			t.Fatalf("write to %q must be refused", rel)
		}
	}
	if data, _ := os.ReadFile(secret); string(data) != "secret" {
		t.Fatalf("symlink target was modified: %q", data)
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !os.IsNotExist(err) {
		t.Fatalf("write escaped through a symlinked directory: %v", err)
	}
}

func TestOperatorFileHelperWritesExactlyTheDeclaredSize(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(target, []byte("original"), 0o640); err != nil {
		t.Fatal(err)
	}
	// A truncated upload never replaces the target.
	status, _ := runFileHelper(t, []string{OperatorFileHelperWrite, root, "notes.txt", "10"}, strings.NewReader("short"))
	if status.OK {
		t.Fatal("short upload must fail")
	}
	// An oversized upload never replaces the target.
	status, _ = runFileHelper(t, []string{OperatorFileHelperWrite, root, "notes.txt", "3"}, strings.NewReader("too long"))
	if status.OK || status.Code != operatorFileCodeTooLarge {
		t.Fatalf("oversized upload must fail: %+v", status)
	}
	if data, _ := os.ReadFile(target); string(data) != "original" {
		t.Fatalf("failed uploads modified the target: %q", data)
	}
	status, _ = runFileHelper(t, []string{OperatorFileHelperWrite, root, "notes.txt", strconv.Itoa(len("updated"))}, strings.NewReader("updated"))
	if !status.OK || status.Size != 7 {
		t.Fatalf("exact upload failed: %+v", status)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(target); string(data) != "updated" || info.Mode().Perm() != 0o640 {
		t.Fatalf("upload = %q mode %v; want updated with preserved mode", data, info.Mode().Perm())
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatalf("temporary upload files were left behind: %v", entries)
	}
	status, _ = runFileHelper(t, []string{OperatorFileHelperWrite, root, "missing-dir/file", "1"}, strings.NewReader("x"))
	if status.OK || status.Code != operatorFileCodeNotFound {
		t.Fatalf("missing parent directory must be not_found: %+v", status)
	}
}

func TestOperatorFileHelperRejectsMalformedInvocations(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{
		{},
		{"chmod", root, "a", "1"},
		{OperatorFileHelperRead, "relative", "a", "1"},
		{OperatorFileHelperRead, root, "a", "-1"},
		{OperatorFileHelperRead, root, "a", "not-a-number"},
		{OperatorFileHelperRead, root, "/a", "1"},
	} {
		if status, _ := runFileHelper(t, args, nil); status.OK {
			t.Fatalf("invocation %v must be refused", args)
		}
	}
}
