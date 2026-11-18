//go:build linux

package source

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func fixtureGit(t *testing.T, directory string, input []byte, arguments ...string) []byte {
	t.Helper()
	command := exec.CommandContext(t.Context(), "/usr/bin/git", append([]string{
		"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgSign=false", "-c", "core.attributesFile=/dev/null",
	}, arguments...)...)
	command.Dir = directory
	command.Env = []string{
		"PATH=/usr/bin:/bin", "HOME=/dev/null", "XDG_CONFIG_HOME=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_AUTHOR_NAME=Source Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid",
		"GIT_COMMITTER_NAME=Source Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
	}
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "synthetic fixture Git: %s", output)
	return output
}

func writeFixture(t *testing.T, filename, content string, mode os.FileMode) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(filename), 0700))
	require.NoError(t, os.WriteFile(filename, []byte(content), mode))
}

func TestPacketTreeAndBlobHashesMatchRealGit(t *testing.T) {
	for _, identityLength := range []int{40, 64} {
		t.Run(fmt.Sprint(identityLength), func(t *testing.T) {
			format := "sha1"
			if identityLength == 64 {
				format = "sha256"
			}
			repository := t.TempDir()
			fixtureGit(t, repository, nil, "init", "--quiet", "--object-format="+format, "--initial-branch=main", "--template=")
			writeFixture(t, filepath.Join(repository, "a", "b"), "nested\n", 0600)
			writeFixture(t, filepath.Join(repository, "a.c"), "file sorts before the a/ tree\n", 0600)
			writeFixture(t, filepath.Join(repository, "run"), "#!/bin/sh\nexit 0\n", 0700)
			fixtureGit(t, repository, nil, "add", ".")
			fixtureGit(t, repository, nil, "commit", "--quiet", "-m", "synthetic packet source")
			tree := strings.TrimSpace(string(fixtureGit(t, repository, nil, "rev-parse", "HEAD^{tree}")))
			fixture := newPacketFixture(t, identityLength,
				packetNode{name: "run", mode: "100755", content: []byte("#!/bin/sh\nexit 0\n")},
				packetNode{name: "a", mode: "040000", children: []packetNode{{name: "b", content: []byte("nested\n")}}},
				packetNode{name: "a.c", content: []byte("file sorts before the a/ tree\n")},
			)
			require.Equal(t, tree, fixture.target.Tree, "independent fixture encoding must match Git tree ordering and raw identities")
			api := newAPIFixture(t, fixture.replies)
			packet, err := api.client.Packet(t.Context(), fixture.target, []string{"a/b", "a.c", "run"})
			require.NoError(t, err)
			for _, file := range packet.Files {
				expected := strings.TrimSpace(string(fixtureGit(t, repository, []byte(file.Content), "hash-object", "--stdin")))
				require.Equal(t, expected, file.BlobSHA)
				require.Equal(t, file.Content, string(fixtureGit(t, repository, nil, "cat-file", "blob", file.BlobSHA)))
			}
		})
	}
}
