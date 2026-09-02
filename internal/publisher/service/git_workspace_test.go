package service

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/artifactcap"
	"github.com/orka-agents/orka/internal/publisher"
)

type testWriteCloser struct {
	bytes.Buffer
}

func (*testWriteCloser) Close() error { return nil }

func TestGitBlobBatchValidatesObjectIdentitySizeAndFraming(t *testing.T) {
	t.Parallel()
	oid := strings.Repeat("a", 40)
	entry := workspaceEntry{OID: oid, Size: 3}

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		input := &testWriteCloser{}
		batch := syntheticGitBlobBatch(context.Background(), input, oid+" blob 3\nabc\n", 3)
		blob, err := batch.readBlob(entry, 3)
		if err != nil {
			t.Fatalf("readBlob: %v", err)
		}
		if string(blob) != "abc" || input.String() != oid+"\n" {
			t.Fatalf("blob/input = %q/%q", blob, input.String())
		}
	})

	for name, response := range map[string]string{
		"oid mismatch":  strings.Repeat("b", 40) + " blob 3\nabc\n",
		"type mismatch": oid + " tree 3\nabc\n",
		"size mismatch": oid + " blob 4\nabcd\n",
		"short content": oid + " blob 3\nab",
		"bad framing":   oid + " blob 3\nabcx",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			batch := syntheticGitBlobBatch(context.Background(), &testWriteCloser{}, response, 3)
			_, err := batch.readBlob(entry, 3)
			assertOperationCode(t, err, "source_corrupt")
		})
	}
}

func TestGitBlobBatchEnforcesLimitsAndCancellation(t *testing.T) {
	t.Parallel()
	oid := strings.Repeat("a", 40)
	entry := workspaceEntry{OID: oid, Size: 3}

	limited := syntheticGitBlobBatch(context.Background(), &testWriteCloser{}, oid+" blob 3\nabc\n", 2)
	_, err := limited.readBlob(entry, 3)
	assertOperationCode(t, err, "scm_output_limit")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceled := syntheticGitBlobBatch(ctx, &testWriteCloser{}, oid+" blob 3\nabc\n", 3)
	_, err = canceled.readBlob(entry, 3)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled readBlob error = %v", err)
	}
}

func TestWorkspacePrepareBatchesGitObjectAccess(t *testing.T) {
	repository := newRepositoryFixture(t, false)
	const fileCount = 300
	for index := range fileCount {
		writeFile(t, filepath.Join(repository.seed, "many", fmt.Sprintf("%04d.txt", index)), fmt.Sprintf("content-%04d\n", index), 0o644)
	}
	if err := os.Symlink("many/0000.txt", filepath.Join(repository.seed, "latest-many")); err != nil {
		t.Fatal(err)
	}
	runGitEnv(t, repository.seed, fixedGitEnv(), "add", "--all")
	runGitEnv(t, repository.seed, fixedGitEnv(), "commit", "-m", "many workspace entries")
	repository.baselineOID = strings.TrimSpace(runGit(t, repository.seed, "rev-parse", "HEAD"))
	runGit(t, repository.seed, "push", repository.source.URL, "HEAD:refs/heads/main")

	gitBinary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gitBinary, err = filepath.Abs(gitBinary)
	if err != nil {
		t.Fatal(err)
	}
	commandLog := filepath.Join(t.TempDir(), "git-commands.log")
	wrapper := filepath.Join(t.TempDir(), "git-wrapper")
	wrapperScript := fmt.Sprintf(
		"#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %s\nexec %s \"$@\"\n",
		shellQuote(commandLog), shellQuote(gitBinary),
	)
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := newServiceFixtureWithOptions(t, nil, func(config *Config) {
		config.GitBinary = wrapper
	})
	response, err := fixture.client.PrepareWorkspace(context.Background(), WorkspacePrepareRequest{
		Metadata: OperationMetadata{Namespace: testNamespace, OperationID: "workspace-batched-objects", TaskID: "task-batched-objects"},
		Source:   repository.source, SourceRef: testMainRef, BaselineOID: repository.baselineOID,
	})
	if err != nil {
		t.Fatalf("PrepareWorkspace: %v", err)
	}
	if response.EntryCount < fileCount {
		t.Fatalf("entry count = %d, want at least %d", response.EntryCount, fileCount)
	}
	archive := fixture.artifact.download(t, artifactcap.Identity{Namespace: testNamespace, TaskID: "task-batched-objects"}, response.Artifact)
	entries := readWorkspaceTar(t, archive)
	if got := entries["many/0299.txt"]; got != "content-0299\n" {
		t.Fatalf("many/0299.txt = %q", got)
	}
	if got := entries["latest-many"]; got != "symlink:many/0000.txt" {
		t.Fatalf("latest-many = %q", got)
	}

	data, err := os.ReadFile(commandLog)
	if err != nil {
		t.Fatal(err)
	}
	commands := strings.FieldsFunc(strings.TrimSpace(string(data)), func(value rune) bool { return value == '\n' })
	var batchCommands, treeCommands, legacyObjectCommands int
	for _, command := range commands {
		switch {
		case strings.Contains(command, " cat-file --batch"):
			batchCommands++
		case strings.Contains(command, " cat-file -s "), strings.Contains(command, " cat-file blob "):
			legacyObjectCommands++
		}
		if strings.Contains(command, " ls-tree -lrz ") {
			treeCommands++
		}
	}
	if batchCommands != 1 || treeCommands != 1 || legacyObjectCommands != 0 {
		t.Fatalf("Git object commands: batch=%d tree=%d legacy=%d\n%s", batchCommands, treeCommands, legacyObjectCommands, data)
	}
	if len(commands) > 16 {
		t.Fatalf("workspace preparation spawned %d top-level Git processes for %d files, want at most 16\n%s", len(commands), fileCount, data)
	}
}

func TestWorkspacePreparationCancelsBlockedBlobBatch(t *testing.T) {
	repository := newRepositoryFixture(t, false)
	gitBinary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gitBinary, err = filepath.Abs(gitBinary)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "batch-requested")
	wrapper := filepath.Join(t.TempDir(), "git-wrapper")
	wrapperScript := fmt.Sprintf(`#!/bin/sh
case " $*" in
  *" cat-file --batch"*)
    IFS= read -r object_id
    : > %s
    sleep 30
    exit 1
    ;;
esac
exec %s "$@"
`, shellQuote(marker), shellQuote(gitBinary))
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o700); err != nil {
		t.Fatal(err)
	}
	runner, err := newGitRunner(wrapper, t.TempDir(), 4<<20, publisher.ProxyEnvironment{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		archive, buildErr := buildWorkspaceArchive(
			ctx, runner, repository.source.ID, repository.source.URL, testMainRef, repository.baselineOID,
			WorkspaceLimits{MaxEntries: 1000, MaxFileBytes: 4 << 20, MaxExpandedBytes: 16 << 20, MaxArtifactBytes: 32 << 20, MaxPathBytes: 4096},
		)
		if archive.path != "" {
			_ = os.Remove(archive.path)
		}
		result <- buildErr
	}()

	deadline := time.NewTimer(5 * time.Second)
	poll := time.NewTicker(10 * time.Millisecond)
	defer deadline.Stop()
	defer poll.Stop()
	requested := false
	for !requested {
		select {
		case <-poll.C:
			if _, statErr := os.Stat(marker); statErr == nil {
				requested = true
			}
		case <-deadline.C:
			cancel()
			select {
			case <-result:
			case <-time.After(3 * time.Second):
			}
			t.Fatal("blob batch did not receive an object request")
		}
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled workspace preparation error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled blob batch did not terminate promptly")
	}
}

func TestWorkspacePreparationClassifiesPrematureBatchExit(t *testing.T) {
	repository := newRepositoryFixture(t, false)
	gitBinary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gitBinary, err = filepath.Abs(gitBinary)
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "git-wrapper")
	wrapperScript := fmt.Sprintf(`#!/bin/sh
case " $*" in
  *" cat-file --batch"*)
    IFS= read -r object_id
    exit 42
    ;;
esac
exec %s "$@"
`, shellQuote(gitBinary))
	if err := os.WriteFile(wrapper, []byte(wrapperScript), 0o700); err != nil {
		t.Fatal(err)
	}
	runner, err := newGitRunner(wrapper, t.TempDir(), 4<<20, publisher.ProxyEnvironment{})
	if err != nil {
		t.Fatal(err)
	}
	archive, err := buildWorkspaceArchive(
		context.Background(), runner, repository.source.ID, repository.source.URL, testMainRef, repository.baselineOID,
		WorkspaceLimits{MaxEntries: 1000, MaxFileBytes: 4 << 20, MaxExpandedBytes: 16 << 20, MaxArtifactBytes: 32 << 20, MaxPathBytes: 4096},
	)
	if archive.path != "" {
		_ = os.Remove(archive.path)
	}
	var operationErr *operationError
	if !errors.As(err, &operationErr) || operationErr.code != "scm_failure" || !operationErr.retryable {
		t.Fatalf("premature batch exit error = %v, want retryable scm_failure", err)
	}
}

func syntheticGitBlobBatch(
	ctx context.Context,
	input io.WriteCloser,
	response string,
	maxBytes int64,
) *gitBlobBatch {
	return &gitBlobBatch{
		ctx: ctx, stdin: input, stdout: bufio.NewReaderSize(strings.NewReader(response), gitBatchHeaderLimit),
		remainingBytes: maxBytes, remainingObjects: 1,
	}
}

func assertOperationCode(t *testing.T, err error, code string) {
	t.Helper()
	var operationErr *operationError
	if !errors.As(err, &operationErr) || operationErr.code != code {
		t.Fatalf("error = %v, want operation code %q", err, code)
	}
}
