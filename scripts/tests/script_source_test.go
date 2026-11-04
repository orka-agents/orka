package scriptstest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func workspaceScript(t *testing.T, name string) string {
	t.Helper()
	root := os.Getenv("WORKSPACE_LIFECYCLE_SOURCE_ROOT")
	if root == "" {
		root = filepath.Join("..", "..")
	}
	data, err := os.ReadFile(filepath.Join(root, "scripts", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func sourceSection(t *testing.T, source, start, end string) string {
	t.Helper()
	begin := strings.Index(source, start)
	if begin < 0 {
		t.Fatalf("missing cleanup start: %s", start)
	}
	finish := strings.Index(source[begin:], end)
	if finish < 0 {
		t.Fatalf("missing cleanup end: %s", end)
	}
	return source[begin : begin+finish]
}
