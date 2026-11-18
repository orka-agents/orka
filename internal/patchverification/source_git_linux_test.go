//go:build linux

package patchverification

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func sourceFixtureGit(t *testing.T, directory string, input []byte, arguments ...string) []byte {
	t.Helper()
	command := exec.Command("/usr/bin/git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false"}, arguments...)...)
	command.Dir = directory
	command.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + directory, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Source Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid",
		"GIT_COMMITTER_NAME=Source Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z"}
	command.Stdin = bytes.NewReader(input)
	output, err := command.Output()
	require.NoError(t, err)
	return output
}

func sourceFixtureWrite(t *testing.T, filename, content string, mode os.FileMode) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(filename), 0700))
	require.NoError(t, os.WriteFile(filename, []byte(content), mode))
}

// Only synthetic local object tests use this launcher. The sole production
// caller fixes /worker --validation-git, whose native hard limits are tested in
// workers/general and whose isolation is qualified in the Kubernetes lane.
func sourceTestGit(t *testing.T, format string) *sourceGit {
	t.Helper()
	git, err := sourceNewGitLauncher(t.Context(), t.TempDir(), format, []string{"/usr/bin/env"})
	require.NoError(t, err)
	return git
}

func sourceTestRepository(t *testing.T, format string) (string, string) {
	t.Helper()
	repository := t.TempDir()
	sourceFixtureGit(t, repository, nil, "init", "--quiet", "--object-format="+format)
	sourceFixtureWrite(t, filepath.Join(repository, "value.txt"), "original\n", 0600)
	sourceFixtureGit(t, repository, nil, "add", ".")
	sourceFixtureGit(t, repository, nil, "commit", "--quiet", "-m", "synthetic source")
	commit := strings.TrimSpace(string(sourceFixtureGit(t, repository, nil, "rev-parse", "HEAD")))
	return repository, commit
}

func TestSourceIsolatedObjects(t *testing.T) {
	repository, _ := sourceTestRepository(t, sourceSHA1)
	sourceFixtureWrite(t, filepath.Join(repository, ".gitattributes"), "value.txt export-ignore\nsubst.txt export-subst\n", 0600)
	sourceFixtureWrite(t, filepath.Join(repository, "subst.txt"), "$Format:%H$\n", 0600)
	sourceFixtureWrite(t, filepath.Join(repository, "bin/run"), "#!/bin/sh\nexit 0\n", 0700)
	sourceFixtureGit(t, repository, nil, "add", ".")
	sourceFixtureGit(t, repository, nil, "commit", "--quiet", "-m", "synthetic attributes")
	commit := strings.TrimSpace(string(sourceFixtureGit(t, repository, nil, "rev-parse", "HEAD")))
	expected := strings.TrimSpace(string(sourceFixtureGit(t, repository, nil, "rev-parse", "HEAD^{tree}")))
	sourceFixtureWrite(t, filepath.Join(repository, "value.txt"), "dirty bytes\n", 0600)
	git := sourceTestGit(t, sourceSHA1)
	tree, err := git.importCommit(t.Context(), filepath.Join(repository, ".git/objects"), commit)
	require.NoError(t, err)
	require.Equal(t, expected, tree)
	archive, err := git.archive(t.Context(), tree)
	require.NoError(t, err)
	repeated, err := git.archive(t.Context(), tree)
	require.NoError(t, err)
	require.Equal(t, archive, repeated)
	files := make(map[string]string)
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if header.Typeflag == tar.TypeDir {
			continue
		}
		content, err := io.ReadAll(reader)
		require.NoError(t, err)
		files[header.Name] = string(content)
		if header.Name == "bin/run" {
			require.Equal(t, int64(0755), header.Mode)
		}
	}
	require.Equal(t, "original\n", files["value.txt"])
	require.Equal(t, "$Format:%H$\n", files["subst.txt"])
	require.Len(t, files, 4)
	require.NoError(t, os.RemoveAll(filepath.Join(repository, ".git/objects")))
	_, err = git.archive(t.Context(), tree)
	require.NoError(t, err)
}

func TestSourceOutputBoundAndExplicitLauncher(t *testing.T) {
	output := &sourceBuffer{limit: 5}
	_, err := io.Copy(output, struct{ io.Reader }{strings.NewReader("more than five bytes")})
	require.ErrorIs(t, err, errSourceLimit)
	require.LessOrEqual(t, output.Len(), 5)
	git := sourceTestGit(t, sourceSHA1)
	_, err = git.run(t.Context(), nil, 1, "--version")
	require.ErrorIs(t, err, errSourceLimit)
	git.launcher = nil
	_, err = git.run(t.Context(), nil, 8192, "--version")
	require.ErrorIs(t, err, errSourceGit)
	_, err = sourceNewGitLauncher(t.Context(), t.TempDir(), sourceSHA1, nil)
	require.ErrorIs(t, err, errSourceGit)
}

func TestSourcePatchExpansionBounds(t *testing.T) {
	for _, patch := range []string{"GIT binary patch\nliteral 4294967295\n", "GIT binary patch\ndelta 4294967295\n",
		"GIT binary patch\ndelta 10\nA~~~~~\n", "GIT binary patch\nliteral -1\n"} {
		require.Error(t, sourcePatchBounds(t.Context(), []byte(patch), 1024))
	}
	require.ErrorIs(t, sourcePatchBounds(t.Context(), []byte(strings.Repeat("diff --git a/large b/copy\n", 5)), sourceMaxArchiveBytes), errSourceLimit)
	_, _, err := sourceDeltaSize([]byte{0xff, 0xff, 0xff, 0xff, 0x7f})
	require.ErrorIs(t, err, errSourceLimit)
	for _, size := range []int{16, 1 << 20} {
		patch := "diff --git a/added b/added\nnew file mode 100644\n--- /dev/null\n+++ b/added\n@@ -0,0 +1 @@\n+" + strings.Repeat("x", size) + "\n"
		patch += strings.Repeat("diff --git a/added b/added\nold mode 100644\nnew mode 100755\ndiff --git a/added b/added\nold mode 100755\nnew mode 100644\n", 64)
		err := sourcePatchBounds(t.Context(), []byte(patch), 2048)
		if size == 16 {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, errSourceLimit)
		}
	}
	repository, _ := sourceTestRepository(t, sourceSHA1)
	sourceFixtureWrite(t, filepath.Join(repository, "binary.dat"), strings.Repeat("\x00", 1<<20), 0600)
	sourceFixtureGit(t, repository, nil, "add", ".")
	patch := sourceFixtureGit(t, repository, nil, "diff", "--cached", "--binary", "HEAD", "--")
	require.Contains(t, string(patch), "literal 1048576\n")
	require.NoError(t, sourcePatchBounds(t.Context(), patch, 2048))
	patch = append(patch, []byte(strings.Repeat("diff --git a/binary.dat b/binary.dat\nold mode 100644\nnew mode 100755\ndiff --git a/binary.dat b/binary.dat\nold mode 100755\nnew mode 100644\n", 64))...)
	require.ErrorIs(t, sourcePatchBounds(t.Context(), patch, 2048), errSourceLimit)
}

func TestSourceGitBinaryPatchAndSHA256(t *testing.T) {
	for _, format := range []string{sourceSHA1, sourceSHA256} {
		t.Run(format, func(t *testing.T) {
			repository, _ := sourceTestRepository(t, format)
			content := make([]byte, 32768)
			_, err := rand.New(rand.NewSource(1)).Read(content)
			require.NoError(t, err)
			sourceFixtureWrite(t, filepath.Join(repository, "binary.dat"), string(content), 0600)
			sourceFixtureGit(t, repository, nil, "add", ".")
			sourceFixtureGit(t, repository, nil, "commit", "--quiet", "-m", "synthetic original")
			original := strings.TrimSpace(string(sourceFixtureGit(t, repository, nil, "rev-parse", "HEAD")))
			content[1000] ^= 0xff
			sourceFixtureWrite(t, filepath.Join(repository, "binary.dat"), string(content), 0600)
			sourceFixtureWrite(t, filepath.Join(repository, "added.dat"), string([]byte{0, 1, 2, 3}), 0600)
			sourceFixtureGit(t, repository, nil, "add", ".")
			sourceFixtureGit(t, repository, nil, "commit", "--quiet", "-m", "synthetic patched")
			patched := strings.TrimSpace(string(sourceFixtureGit(t, repository, nil, "rev-parse", "HEAD")))
			patch := sourceFixtureGit(t, repository, nil, "diff", "--binary", original, patched, "--")
			require.Contains(t, string(patch), "delta ")
			require.Contains(t, string(patch), "literal ")
			git := sourceTestGit(t, format)
			originalTree, err := git.importCommit(t.Context(), filepath.Join(repository, ".git/objects"), original)
			require.NoError(t, err)
			expectedTree, err := git.importCommit(t.Context(), filepath.Join(repository, ".git/objects"), patched)
			require.NoError(t, err)
			tree, err := git.applyPatch(t.Context(), originalTree, patch, len(content)+4096)
			require.NoError(t, err)
			require.Equal(t, expectedTree, tree)
			require.Len(t, tree, len(original))
		})
	}
}

func TestSourceGitImportOrdersBoundedObjectsBeforeTraversal(t *testing.T) {
	repository, _ := sourceTestRepository(t, sourceSHA1)
	sourceFixtureWrite(t, filepath.Join(repository, "nested/value.txt"), "nested\n", 0600)
	sourceFixtureGit(t, repository, nil, "add", ".")
	sourceFixtureGit(t, repository, nil, "commit", "--quiet", "-m", "synthetic nested tree")
	commit := strings.TrimSpace(string(sourceFixtureGit(t, repository, nil, "rev-parse", "HEAD")))
	git := sourceTestGit(t, sourceSHA1)
	script := filepath.Join(git.root, "git-ordering")
	sourceFixtureWrite(t, script, "#!/bin/sh\nprintf '%s\\n' \"$GIT_OBJECT_DIRECTORY|$*\" >> \"$HOME/operations\"\nexec /usr/bin/git \"$@\"\n", 0700)
	git.binary = script
	objects := filepath.Join(repository, ".git/objects")
	_, err := git.importCommit(t.Context(), objects, commit)
	require.NoError(t, err)
	content, err := os.ReadFile(filepath.Join(git.root, "operations"))
	require.NoError(t, err)
	copiedCommit, lastTree := false, ""
	copiedTrees := make(map[string]bool)
	for line := range strings.SplitSeq(strings.TrimSpace(string(content)), "\n") {
		fromSource := strings.HasPrefix(line, objects+"|")
		require.False(t, fromSource && (strings.Contains(line, "^{") || strings.Contains(line, " rev-parse ") || strings.Contains(line, " ls-tree ")))
		switch {
		case strings.HasSuffix(line, " hash-object -w --no-filters -t commit --stdin"):
			copiedCommit = true
		case strings.Contains(line, " rev-parse "):
			require.True(t, copiedCommit)
			require.NotEmpty(t, copiedTrees)
			require.False(t, fromSource)
		case fromSource && strings.Contains(line, " cat-file tree "):
			fields := strings.Fields(line)
			lastTree = fields[len(fields)-1]
		case strings.HasSuffix(line, " hash-object -w --no-filters -t tree --stdin"):
			copiedTrees[lastTree] = true
		case strings.Contains(line, " ls-tree "):
			fields := strings.Fields(line)
			require.NotContains(t, line, " -r ")
			require.True(t, copiedTrees[fields[len(fields)-1]])
		}
	}
	require.True(t, copiedCommit)
	require.Len(t, copiedTrees, 2)
}

func TestSourceGitChildEnvironmentAndCancellation(t *testing.T) {
	t.Run("environment", func(t *testing.T) {
		git := sourceTestGit(t, sourceSHA1)
		script := filepath.Join(git.root, "git-environment")
		sourceFixtureWrite(t, script, "#!/bin/sh\nexec /usr/bin/env\n", 0700)
		for _, name := range []string{"SOURCE_TEST_SECRET", "GITHUB_TOKEN", "HTTP_PROXY", "HTTPS_PROXY",
			"SSH_AUTH_SOCK", "GIT_ASKPASS", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_ALTERNATE_OBJECT_DIRECTORIES"} {
			t.Setenv(name, "fixture-only-must-not-reach-git")
		}
		git.binary = script
		content, err := git.run(t.Context(), nil, 8192, "status")
		require.NoError(t, err)
		require.NotContains(t, string(content), "fixture-only")
		for _, expected := range []string{"GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "PATH=/usr/bin:/bin", "GIT_ASKPASS=/bin/false"} {
			require.Contains(t, string(content), expected+"\n")
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		git := sourceTestGit(t, sourceSHA1)
		require.NoError(t, syscall.Mkfifo(filepath.Join(git.root, "blocker"), 0600))
		script := filepath.Join(git.root, "blocked-git")
		sourceFixtureWrite(t, script, "#!/bin/sh\nprintf '%s\\n' \"$$\" > \"$HOME/child.pid\"\nread ignored < \"$HOME/blocker\"\n", 0700)
		git.binary = script
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		_, err := git.run(ctx, nil, 0, "status")
		require.ErrorIs(t, err, context.DeadlineExceeded)
		content, err := os.ReadFile(filepath.Join(git.root, "child.pid"))
		require.NoError(t, err)
		pid, err := strconv.Atoi(strings.TrimSpace(string(content)))
		require.NoError(t, err)
		require.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH)
	})
}
