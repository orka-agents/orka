//go:build linux

package patchverification

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

type unrenderablePreparationError struct{ cause error }

func (e unrenderablePreparationError) Error() string { panic("arbitrary errors must not be rendered") }
func (e unrenderablePreparationError) Unwrap() error { return e.cause }

func decodeFailure(t *testing.T, err error) preparationDiagnostic {
	t.Helper()
	content := PreparationFailureDiagnostic(err)
	require.LessOrEqual(t, len(content), MaxPreparationDiagnosticBytes)
	require.True(t, strings.HasSuffix(string(content), "\n"))
	var result preparationDiagnostic
	require.NoError(t, json.Unmarshal(content, &result))
	require.Equal(t, 1, result.Version)
	return result
}

func TestPreparationDiagnosticTypedMapping(t *testing.T) {
	for _, test := range []struct {
		name     string
		stage    PreparationStage
		err      error
		category string
		errno    int
	}{
		{"input", PreparationInputDecode, ErrEvidence, "invalid", 0},
		{"identity", PreparationIdentity, ErrBinding, "identity", 0},
		{"isolation", PreparationIsolation, errors.New("private policy details"), "isolation", 0},
		{"limit", PreparationOriginalArchive, errSourceLimit, "limit", 0},
		{"integrity", PreparationGitImport, errSourceIntegrity, "integrity", 0},
		{"timeout", PreparationGitFetch, context.DeadlineExceeded, "timeout", 0},
		{"canceled", PreparationGitFetch, context.Canceled, "cancelled", 0},
		{"ENOSPC", PreparationWorkspace, &os.PathError{Op: "private-op", Path: "/private/path", Err: syscall.ENOSPC}, "io", int(syscall.ENOSPC)},
		{"EPIPE", PreparationReportWrite, syscall.EPIPE, "io", int(syscall.EPIPE)},
		{"short write", PreparationReportWrite, io.ErrShortWrite, "io", 0},
		{"unknown errno", PreparationWorkspace, syscall.Errno(4095), "io", 0},
		{"unknown", PreparationReportEncode, unrenderablePreparationError{}, "internal", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := PreparationFailure(test.stage, unrenderablePreparationError{cause: test.err})
			require.Equal(t, "source preparation failed", err.Error())
			got := decodeFailure(t, err)
			require.Equal(t, string(test.stage), got.Stage)
			require.Equal(t, test.category, got.Category)
			require.Equal(t, test.errno, got.Errno)
			require.Zero(t, got.ExitCode)
			require.Zero(t, got.Signal)
		})
	}
}

func TestPreparationDiagnosticCannotRenderUntrustedData(t *testing.T) {
	const canary = "DO-NOT-EMIT-source-report-patch-https://private.invalid/path"
	err := PreparationFailure(PreparationInputOpen, &os.PathError{Op: canary, Path: canary, Err: syscall.EACCES})
	content := PreparationFailureDiagnostic(err)
	require.NotContains(t, string(content), canary)
	require.NotContains(t, string(content), "private.invalid")
	require.NotContains(t, string(content), "/path")
	require.JSONEq(t, `{"version":1,"stage":"input-open","category":"io","errno":13}`, string(content))
	manufactured := &preparationDiagnosticError{diagnostic: preparationDiagnostic{
		Version: 123, Stage: strings.Repeat(canary, 100), Category: strings.Repeat(canary, 100),
		ExitCode: 99999, Signal: 99999, Errno: 99999,
	}, cause: unrenderablePreparationError{}}
	require.JSONEq(t, `{"version":1,"stage":"internal","category":"internal"}`,
		string(PreparationFailureDiagnostic(manufactured)))
	for _, diagnostic := range []preparationDiagnostic{
		{Stage: string(PreparationGitFetch), Category: "exit", ExitCode: -1},
		{Stage: string(PreparationGitFetch), Category: "exit", ExitCode: 256},
		{Stage: string(PreparationGitFetch), Category: "signal", Signal: 65},
		{Stage: string(PreparationGitFetch), Category: "io", Errno: -1},
	} {
		got := decodeFailure(t, &preparationDiagnosticError{diagnostic: diagnostic})
		require.Zero(t, got.ExitCode)
		require.Zero(t, got.Signal)
		require.Zero(t, got.Errno)
	}
}

func TestGitFailurePreservesOnlyNumericProcessInformation(t *testing.T) {
	git := sourceTestGit(t, sourceSHA1)
	script := filepath.Join(git.root, "synthetic-git")
	sourceFixtureWrite(t, script, "#!/bin/sh\nprintf 'DO-NOT-EMIT-Git-stderr' >&2\nexit 128\n", 0700)
	git.binary = script
	_, err := git.run(t.Context(), nil, 4096, "private-command", "https://private.invalid/repo")
	require.ErrorIs(t, err, errSourceGit)
	err = PreparationFailure(PreparationGitFetch, err)
	got := decodeFailure(t, err)
	require.Equal(t, preparationDiagnostic{Version: 1, Stage: "git-fetch", Category: "exit", ExitCode: 128}, got)
	require.NotContains(t, string(PreparationFailureDiagnostic(err)), "DO-NOT-EMIT")
	require.NotContains(t, string(PreparationFailureDiagnostic(err)), "private")
	command := exec.CommandContext(t.Context(), "/bin/sh", "-c", "kill -TERM $$")
	err = preparationCommandError(command.Run())
	got = decodeFailure(t, PreparationFailure(PreparationGitImport, err))
	require.Equal(t, "signal", got.Category)
	require.Equal(t, int(syscall.SIGTERM), got.Signal)
	require.Zero(t, got.ExitCode)
	git.binary = filepath.Join(git.root, "missing-private-executable")
	_, err = git.run(t.Context(), nil, 4096, "private-command")
	got = decodeFailure(t, PreparationFailure(PreparationGitInit, err))
	require.Equal(t, "exit", got.Category)
	require.Equal(t, 127, got.ExitCode)
	require.Zero(t, got.Errno, "an exit code must never be interpreted as an OS errno")
	git.launcher = []string{filepath.Join(git.root, "missing-private-launcher")}
	_, err = git.run(t.Context(), nil, 4096, "private-command")
	got = decodeFailure(t, PreparationFailure(PreparationGitInit, err))
	require.Equal(t, "io", got.Category)
	require.Equal(t, int(syscall.ENOENT), got.Errno)
}

func TestPreparationDiagnosticStageMappingOnActualSourcePaths(t *testing.T) {
	input := workerInputFixture(t, VerifyPatch)
	repository, commit := sourceTestRepository(t, sourceSHA1)
	git := sourceTestGit(t, sourceSHA1)
	tree, err := git.importCommit(t.Context(), filepath.Join(repository, ".git/objects"), commit)
	require.NoError(t, err)
	input.Manifest.Sources.Original.Commit, input.Manifest.Sources.Original.Tree = commit, tree
	input.Patch = []byte("--- a/value.txt\n+++ b/value.txt\n@@ -1 +1 @@\n-not-the-original\n+patched\n")
	_, err = prepareWorkerArchives(t.Context(), git, input, tree)
	require.Error(t, err)
	got := decodeFailure(t, err)
	require.Equal(t, "patch-validate", got.Stage)
	require.Equal(t, "exit", got.Category)
	require.NotZero(t, got.ExitCode)
	input = workerInputFixture(t, ValidateReport)
	git.launcher = []string{filepath.Join(git.root, "missing")}
	_, err = prepareWorkerArchives(t.Context(), git, input, tree)
	got = decodeFailure(t, err)
	require.Equal(t, "original-archive", got.Stage)
	require.Equal(t, int(syscall.ENOENT), got.Errno)
	_, err = git.importCommit(t.Context(), filepath.Join(repository, ".git/objects"), commit)
	got = decodeFailure(t, PreparationFailure(PreparationGitImport, err))
	require.Equal(t, "git-import", got.Stage)
	require.Equal(t, int(syscall.ENOENT), got.Errno, "import must not collapse a process failure into integrity")
}
