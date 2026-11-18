package remediation

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompilePatchFromExactEdits(t *testing.T) {
	original := map[string][]byte{"main.go": []byte("package main\n\nfunc allow() bool { return true }\n")}
	patch, err := CompilePatch(PatchProposal{Edits: []SourceEdit{{Path: "main.go", Old: "return true", New: "return false"}}}, original)
	if err != nil || !strings.HasPrefix(patch, "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n") ||
		!strings.Contains(patch, "-func allow() bool { return true }\n+func allow() bool { return false }\n") {
		t.Fatalf("exact edit did not produce a valid diff: %v", err)
	}
	if string(original["main.go"]) != "package main\n\nfunc allow() bool { return true }\n" {
		t.Fatal("diff construction mutated the source")
	}
}

func TestCompilePatchRejectsMissingAndAmbiguousOldText(t *testing.T) {
	for _, old := range []string{"", "not-present", "same"} {
		_, err := CompilePatch(PatchProposal{Edits: []SourceEdit{{Path: "file", Old: old, New: "changed"}}},
			map[string][]byte{"file": []byte("same\nsame\n")})
		if err == nil {
			t.Fatal("unbound source replacement accepted")
		}
	}
}

func TestCompilePatchAppliesAtEndOfFile(t *testing.T) {
	for _, original := range []string{"first\nlast\n", "first\nlast\n\n"} {
		t.Run(strings.TrimSuffix(original, "\n"), func(t *testing.T) {
			patch, err := CompilePatch(PatchProposal{Edits: []SourceEdit{
				{Path: "source.txt", Old: "last\n", New: "changed\nadded\n"},
			}}, map[string][]byte{"source.txt": []byte(original)})
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			name := filepath.Join(dir, "source.txt")
			if err := os.WriteFile(name, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("git", "apply", "-")
			cmd.Dir, cmd.Stdin = dir, strings.NewReader(patch)
			cmd.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+filepath.Dir(dir))
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("generated patch does not apply: %v: %s", err, output)
			}
			got, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if want := strings.Replace(original, "last\n", "changed\nadded\n", 1); string(got) != want {
				t.Fatalf("applied file = %q, want %q", got, want)
			}
		})
	}
}
