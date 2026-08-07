package cliwrapper

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
)

func TestFinalizeTurnResultNeutralizesExecutableRepositoryConfig(t *testing.T) {
	repo := t.TempDir()
	runResultTestGit(t, repo, "init")
	runResultTestGit(t, repo, "config", "user.email", "test@example.invalid")
	runResultTestGit(t, repo, "config", "user.name", "Test User")
	tracked := filepath.Join(repo, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runResultTestGit(t, repo, "add", "tracked.txt")
	runResultTestGit(t, repo, "commit", "-m", "initial")

	marker := filepath.Join(repo, "filter-ran")
	filterCommand := "sh -c 'touch " + marker + "; cat'"
	runResultTestGit(t, repo, "config", "filter.hostile.clean", filterCommand)
	runResultTestGit(t, repo, "config", "filter.hostile.smudge", filterCommand)
	runResultTestGit(t, repo, "config", "filter.hostile.required", "true")
	runResultTestGit(t, repo, "config", "core.fsmonitor", filterCommand)
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("*.txt filter=hostile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tracked, []byte("after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(workerenv.PushBranch, "")

	raw, err := FinalizeTurnResult(repo, "finished safely")
	if err != nil {
		t.Fatalf("FinalizeTurnResult: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("repository-controlled Git command executed, marker stat error = %v", err)
	}
	result := common.ParseStructuredResult(string(raw))
	if result.Summary != "finished safely" || result.BaseSHA == "" ||
		!strings.Contains(result.Diff, "after") || !slices.Contains(result.Files, "tracked.txt") {
		t.Fatalf("structured result = %+v", result)
	}
	config := runResultTestGitOutput(t, repo, "config", "--local", "--name-only", "--list")
	removedKeys := []string{
		"filter.hostile.clean", "filter.hostile.smudge", "filter.hostile.required", "core.fsmonitor",
	}
	for _, removed := range removedKeys {
		if strings.Contains(config, removed) {
			t.Fatalf("executable repository config %q survived sanitization:\n%s", removed, config)
		}
	}
}

func TestFinalizeTurnResultRejectsPublication(t *testing.T) {
	t.Setenv(workerenv.PushBranch, "agent-branch")
	if _, err := FinalizeTurnResult(t.TempDir(), "result"); err == nil ||
		!strings.Contains(err.Error(), "does not permit branch publication") {
		t.Fatalf("publication error = %v, want refusal", err)
	}
}

func TestParseWrapperDiffNameStatusPathsIncludesRenameEndpoints(t *testing.T) {
	got := parseWrapperDiffNameStatusPaths("M\x00changed.txt\x00R100\x00old.txt\x00new.txt\x00")
	for _, want := range []string{"changed.txt", "old.txt", "new.txt"} {
		if !slices.Contains(got, want) {
			t.Fatalf("parsed paths = %v, missing %q", got, want)
		}
	}
}

func runResultTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func runResultTestGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
