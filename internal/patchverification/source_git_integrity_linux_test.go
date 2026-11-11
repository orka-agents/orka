//go:build linux

package patchverification

import (
	"bytes"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSourceGitRejectsMalformedAndPrivilegedTrees(t *testing.T) {
	repository, _ := sourceTestRepository(t, sourceSHA1)
	blob := strings.TrimSpace(string(sourceFixtureGit(t, repository, []byte("synthetic\n"),
		"hash-object", "-w", "--stdin")))
	identity, err := hex.DecodeString(blob)
	require.NoError(t, err)
	entry := func(mode, name string) []byte {
		return append(append([]byte(mode+" "+name), 0), identity...)
	}
	for name, raw := range map[string][]byte{
		"symlink":            entry("120000", "link"),
		"submodule":          entry("160000", "module"),
		"git metadata":       entry("100644", ".git"),
		"unsafe path":        entry("100644", "../outside"),
		"unexpected mode":    entry("100664", "file"),
		"duplicate":          append(entry("100644", "same"), entry("100644", "same")...),
		"unsorted":           append(entry("100644", "z"), entry("100644", "a")...),
		"truncated identity": entry("100644", "file")[:24],
		"oversized object":   bytes.Repeat([]byte("x"), sourceMaxTreeBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			tree := strings.TrimSpace(string(sourceFixtureGit(t, repository, raw,
				"hash-object", "--literally", "-t", "tree", "-w", "--stdin")))
			content := "tree " + tree + "\nauthor Fixture <fixture@example.invalid> 0 +0000\n" +
				"committer Fixture <fixture@example.invalid> 0 +0000\n\nsynthetic malformed tree\n"
			commit := strings.TrimSpace(string(sourceFixtureGit(t, repository, []byte(content),
				"hash-object", "-t", "commit", "-w", "--stdin")))
			git := sourceTestGit(t, sourceSHA1)
			_, err := git.importCommit(t.Context(), filepath.Join(repository, ".git/objects"), commit)
			require.Error(t, err)
		})
	}
}
