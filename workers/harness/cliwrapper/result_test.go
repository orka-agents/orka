package cliwrapper

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

	raw, err := FinalizeTurnResult(context.Background(), repo, "finished safely")
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
	if _, err := FinalizeTurnResult(context.Background(), t.TempDir(), "result"); err == nil ||
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

func TestUploadTurnArtifactsCancelsRetryBackoff(t *testing.T) {
	artifactDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(artifactDir, "evidence.txt"), []byte("evidence"), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	firstAttempt := make(chan struct{})
	var attempts atomic.Int32
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			close(firstAttempt)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer controller.Close()
	t.Setenv(workerenv.ControllerURL, controller.URL)

	t.Setenv(EnvChildUID, "")
	t.Setenv(EnvChildGID, "")
	server := &Server{config: Config{AuthValue: strings.Repeat("artifact-fixture-", 3)}}
	request := validWrapperStartTurnRequest()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.uploadTurnArtifacts(ctx, TurnContext{
			Namespace: request.Namespace, TaskName: request.TaskName, TurnID: string(request.TurnID),
			Metadata: request.Metadata,
		}, artifactDir)
	}()

	select {
	case <-firstAttempt:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first artifact upload attempt")
	}
	// The first retry delay is two seconds. Give the response time to reach the
	// uploader, then verify cancellation interrupts the delay rather than waiting
	// for another attempt.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("UploadTurnArtifacts() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("UploadTurnArtifacts() did not stop after cancellation")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("artifact upload attempts = %d, want 1 before cancellation", got)
	}
}
