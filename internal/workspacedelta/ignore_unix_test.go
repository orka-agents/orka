//go:build darwin || linux

package workspacedelta

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Cargo hard-links build outputs inside target/; an ignored target/ must not
// reach the single-link check.
func TestBuildSkipsHardlinkedIgnoredBuildOutput(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".gitignore", "/target\n", 0o644)
	writeTestFile(t, root, "src/main.rs", "fn main() {}\n", 0o644)
	baseline := captureTestBaseline(t, root, Options{})

	writeTestFile(t, root, "src/main.rs", "fn main() { println!(\"fixed\"); }\n", 0o644)
	writeTestFile(t, root, "target/debug/deps/app-1234", "binary\n", 0o755)
	if err := os.Link(filepath.Join(root, "target/debug/deps/app-1234"), filepath.Join(root, "target/debug/app")); err != nil {
		t.Fatalf("hard link: %v", err)
	}
	if err := os.Symlink("/usr/bin/true", filepath.Join(root, "target/debug/escape")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	result, err := Build(baseline, root, IntentWrite)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	assertChangedPaths(t, result, "src/main.rs")
}

func TestBuildKeepsHardlinkCheckOnPublishedFiles(t *testing.T) {
	t.Run("new file linked into ignored tree", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, root, ".gitignore", "target/\n", 0o644)
		baseline := captureTestBaseline(t, root, Options{})
		writeTestFile(t, root, "target/blob", "data\n", 0o644)
		if err := os.Link(filepath.Join(root, "target/blob"), filepath.Join(root, "published.txt")); err != nil {
			t.Fatalf("hard link: %v", err)
		}
		_, err := Build(baseline, root, IntentWrite)
		if !errors.Is(err, ErrHardlinkAmbiguous) {
			t.Fatalf("Build error = %v, want ErrHardlinkAmbiguous", err)
		}
	})
	t.Run("tracked ignored file", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, root, ".gitignore", "*.lock\n", 0o644)
		writeTestFile(t, root, "deps.lock", "v1\n", 0o644)
		baseline := captureTestBaseline(t, root, Options{})
		if err := os.Link(filepath.Join(root, "deps.lock"), filepath.Join(t.TempDir(), "outside")); err != nil {
			t.Fatalf("hard link: %v", err)
		}
		_, err := Build(baseline, root, IntentWrite)
		if !errors.Is(err, ErrHardlinkAmbiguous) {
			t.Fatalf("Build error = %v, want ErrHardlinkAmbiguous", err)
		}
	})
}
