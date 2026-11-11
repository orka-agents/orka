package patchverification

import (
	"archive/tar"
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSourcePathAndCommitIdentity(t *testing.T) {
	for _, name := range []string{"main.go", "src/main.go", ".gitattributes", "nested/a file", "a-b/c.d"} {
		require.True(t, sourceValidPath(name), name)
	}
	for _, name := range []string{"", ".", "..", "../escape", "a/../escape", "/absolute", "a//b", "a/", ".git",
		"a/.GIT/config", ".git./config", "a\\b", "C:/file", "a\x00b", "a\nb", string([]byte{0xff}),
		strings.Repeat("a", 256), strings.Repeat("a/", sourceMaxPathDepth) + "a"} {
		require.False(t, sourceValidPath(name), name)
	}
	for _, identity := range []string{strings.Repeat("a", 40), strings.Repeat("f", 64)} {
		require.True(t, sourceValidCommit(identity))
	}
	for _, identity := range []string{"HEAD", "main", "refs/heads/main", "HEAD~1", "abc123",
		strings.Repeat("a", 39), strings.Repeat("A", 40), strings.Repeat("g", 64), "--help"} {
		require.False(t, sourceValidCommit(identity))
	}
}

func TestSourceArchiveRejectsAuthorityAndMetadata(t *testing.T) {
	for name, headers := range map[string][]*tar.Header{
		"traversal":        {{Name: "../escape", Mode: 0644, Typeflag: tar.TypeReg}},
		"absolute":         {{Name: "/escape", Mode: 0644, Typeflag: tar.TypeReg}},
		"git":              {{Name: ".git/config", Mode: 0644, Typeflag: tar.TypeReg}},
		"symlink":          {{Name: "link", Mode: 0777, Typeflag: tar.TypeSymlink, Linkname: "../escape"}},
		"hardlink":         {{Name: "link", Mode: 0644, Typeflag: tar.TypeLink, Linkname: "elsewhere"}},
		"fifo":             {{Name: "fifo", Mode: 0644, Typeflag: tar.TypeFifo}},
		"device":           {{Name: "device", Mode: 0644, Typeflag: tar.TypeChar}},
		"setuid":           {{Name: "special", Mode: 04755, Typeflag: tar.TypeReg}},
		"xattrs":           {{Name: "file", Mode: 0644, Typeflag: tar.TypeReg, PAXRecords: map[string]string{"SCHILY.xattr.user.test": "value"}}},
		"unknown metadata": {{Name: "file", Mode: 0644, Typeflag: tar.TypeReg, PAXRecords: map[string]string{"ORKA.test": "value"}}},
		"duplicate":        {{Name: "same", Mode: 0644, Typeflag: tar.TypeReg}, {Name: "same", Mode: 0644, Typeflag: tar.TypeReg}},
		"file parent":      {{Name: "parent", Mode: 0644, Typeflag: tar.TypeReg}, {Name: "parent/child", Mode: 0644, Typeflag: tar.TypeReg}},
	} {
		t.Run(name, func(t *testing.T) {
			var content bytes.Buffer
			writer := tar.NewWriter(&content)
			for _, header := range headers {
				require.NoError(t, writer.WriteHeader(header))
			}
			require.NoError(t, writer.Close())
			require.Error(t, sourceExtractArchive(t.Context(), content.Bytes(), filepath.Join(t.TempDir(), "source")))
		})
	}
	require.Error(t, sourceExtractArchive(t.Context(), make([]byte, 512), filepath.Join(t.TempDir(), "truncated")))
}
